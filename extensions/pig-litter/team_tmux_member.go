package piglitter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
)

type tmuxMemberEnvironment struct {
	server     tmuxServer
	executable string
	pigHome    string
	usePiDirs  bool
}

type tmuxMemberScope struct {
	owner     *owner
	record    *record
	run       *run
	directory string
	dirInfo   os.FileInfo
	manifest  string
	fileInfo  os.FileInfo
	ownership *memberOwnership
	pane      *tmuxPane
	process   *memberProcessHandle
	queue     *memberControlQueue
	control   *memberControlServer
	attempted bool
	marked    bool
}

func currentTmuxMemberEnvironment(ctx context.Context) (tmuxMemberEnvironment, error) {
	server, err := tmuxFromEnvironment(ctx, os.Getenv("TMUX"), os.Getenv("TMUX_PANE"))
	if err != nil {
		return tmuxMemberEnvironment{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return tmuxMemberEnvironment{}, err
	}
	return tmuxMemberEnvironment{server: server, executable: executable, usePiDirs: os.Getenv("PIG_USE_PI_DIRS") == "1"}, nil
}

func (o *owner) executeTmuxMember(r *record, selected *run, env tmuxMemberEnvironment) (outcome Outcome) {
	outcome.State = Unavailable
	if err := o.current(); err != nil {
		outcome.Error = err.Error()
		return
	}
	trusted, err := o.context.IsProjectTrusted()
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	agentDir, err := canonicalTeamDirectory(o.agentDir)
	if err != nil {
		outcome.Error = "native member agent directory is unavailable: " + err.Error()
		return
	}
	var sessionDir string
	var resumeSession *memberSession
	if selected.resume {
		selected.mu.Lock()
		pin := selected.nativeResume
		selected.mu.Unlock()
		o.tree.mu.Lock()
		resumeSession = cloneMemberSession(r.nativeSession)
		o.tree.mu.Unlock()
		if pin == nil || !validMemberSession(resumeSession, r.id, r.cwd, r.model) || pin.log.path != resumeSession.File {
			outcome.Error = "native resume history is unavailable"
			return
		}
		snapshot, readErr := readMemberLog(selected.ctx, pin.log)
		if readErr != nil || len(snapshot.entries) != pin.log.baselineCount || memberEntriesDigest(snapshot.entries) != pin.log.baselineHash {
			outcome.Error = "native resume history changed before launch"
			return
		}
		sessionDir = filepath.Dir(pin.log.path)
	} else {
		sessionDir, err = nativeSessionDirectory(r.cwd, agentDir, r.id, trusted)
		if err != nil {
			outcome.Error = err.Error()
			return
		}
	}
	directory, err := newNativeMemberDirectory(agentDir)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	scope := &tmuxMemberScope{owner: o, record: r, run: selected, directory: directory}
	scope.dirInfo, err = os.Lstat(directory)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	defer func() {
		if err := scope.close(); err != nil {
			o.noteNativeCleanupFailure(r, selected, err)
			outcome.State = Unavailable
			outcome.Error = strings.TrimSpace(outcome.Error + " " + err.Error())
		}
	}()
	var nonce [16]byte
	var capability [32]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		outcome.Error = err.Error()
		return
	}
	if _, err := rand.Read(capability[:]); err != nil {
		outcome.Error = err.Error()
		return
	}
	launchID := hex.EncodeToString(nonce[:])
	scope.manifest = filepath.Join(directory, "member-"+launchID+".json")
	launch := tmuxLaunch{executable: env.executable, manifest: scope.manifest, cwd: r.cwd, nonce: launchID}
	manifest := nativeMemberManifest{Version: 1, Launch: launchID, Session: r.id, Cwd: r.cwd, Model: r.model, Tools: []string{}, SessionDirectory: sessionDir, AgentDirectory: agentDir, PigHome: env.pigHome, UsePiDirs: env.usePiDirs, Socket: filepath.Join(directory, "member-control.sock"), Capability: hex.EncodeToString(capability[:]), Prompt: selected.prompt, Instructions: r.instructions, MailboxLimit: o.config.MaxMailbox, ProjectTrusted: trusted}
	if resumeSession != nil {
		manifest.ResumeFile = resumeSession.File
	}
	manifest.PeerWake = selected.peerWake != nil
	if err := manifest.validate(directory, filepath.Base(scope.manifest)); err != nil {
		outcome.Error = err.Error()
		return
	}
	o.tree.mu.Lock()
	var member *teamMember
	if r.team != nil {
		copy := *r.team
		member = &copy
	}
	o.tree.mu.Unlock()
	if member == nil {
		outcome.Error = "native member has no teammate reservation"
		return
	}
	store, err := o.teamStoreForRoot()
	if err == nil && !selected.resume {
		err = store.markNativeLaunch(*member, launchID)
		scope.marked = err == nil
	}
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	scope.ownership, err = o.reserveMemberLaunch(selected.ctx, r, selected, env.server, launch, sessionDir)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	if resumeSession != nil {
		if !slices.Equal(scope.ownership.Tools, resumeSession.Tools) {
			outcome.Error = "native teammate tool ceiling changed before launch"
			return
		}
		if err := store.markNativeLaunch(*member, launchID, resumeSession.Launch); err != nil {
			outcome.Error = err.Error()
			return
		}
		scope.marked = true
	}
	manifest.Tools = scope.ownership.Tools
	if err := manifest.validate(directory, filepath.Base(scope.manifest)); err != nil {
		outcome.Error = err.Error()
		return
	}
	data, err := json.Marshal(manifest)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	file, err := os.OpenFile(scope.manifest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	scope.fileInfo, err = file.Stat()
	if err == nil {
		_, err = file.Write(data)
	}
	if err == nil {
		err = file.Sync()
	}
	err = errors.Join(err, file.Close())
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	scope.attempted = true
	pane, err := env.server.launch(selected.ctx, launch)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	scope.pane = &pane
	scope.process, err = openMemberProcess(pane.pid, pane.birth)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	if err := o.recordMemberPane(selected.ctx, r, selected, scope.ownership, pane); err != nil {
		outcome.Error = err.Error()
		return
	}
	scope.queue, err = o.newMemberControlQueueWithCapability(r, selected, scope.ownership, capability)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	binding, err := o.installProvisionalMemberHandle(selected.ctx, r, selected, sessionDir, launchID, scope.queue)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	if err := scope.queue.attachAuthority(binding); err != nil {
		outcome.Error = err.Error()
		return
	}
	scope.control, err = startMemberControlServer(context.Background(), directory, scope.queue)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	if err := scope.wait(); err != nil {
		outcome.Error = err.Error()
		return
	}
	o.tree.mu.Lock()
	if selected.outcome != nil {
		outcome = *selected.outcome
	} else {
		outcome.Error = "native member exited before approved settlement"
	}
	o.tree.mu.Unlock()
	return
}

func nativeSessionDirectory(cwd, agentDir string, id ChildID, trusted bool) (string, error) {
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: cwd, AgentDir: agentDir, ProjectTrusted: &trusted})
	if err != nil {
		return "", err
	}
	defer services.Close()
	manager, err := coding.SessionManagerFor(services, coding.SessionStartOptions{SessionID: string(id)})
	if err != nil {
		return "", err
	}
	directory := manager.GetSessionDir()
	canonical, err := canonicalTeamDirectory(directory)
	if err != nil || canonical != directory {
		return "", errors.New("native session directory is not canonical")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), "_"+string(id)+".jsonl") {
			return "", errors.New("native session history already exists and needs reconciliation")
		}
	}
	return directory, nil
}

func newNativeMemberDirectory(agentDir string) (string, error) {
	base := filepath.Join(agentDir, "p08")
	if err := os.Mkdir(base, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return "", err
	}
	info, err := os.Lstat(base)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
		return "", errors.New("native member directory must be private")
	}
	return os.MkdirTemp(base, "m-")
}

func (scope *tmuxMemberScope) wait() error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	var deadline <-chan time.Time
	var timer *time.Timer
	cancelled := scope.run.ctx.Done()
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		birth, err := processBirth(scope.pane.pid)
		if errors.Is(err, os.ErrNotExist) || err == nil && birth != scope.pane.birth {
			return nil
		}
		if err != nil {
			return fmt.Errorf("verify native member process: %w", err)
		}
		select {
		case <-cancelled:
			cancelled = nil
			timer = time.NewTimer(2 * time.Second)
			deadline = timer.C
		case <-deadline:
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := errors.Join(scope.pane.close(ctx), scope.process.kill())
			cancel()
			if err != nil {
				return err
			}
			return nil
		case <-ticker.C:
		}
	}
}

func (scope *tmuxMemberScope) close() error {
	if scope.queue != nil {
		scope.queue.Close()
	}
	var failure error
	if scope.control != nil {
		failure = scope.control.Close()
	}
	cleared := scope.pane == nil && !scope.attempted
	if scope.pane != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		paneErr := scope.pane.close(ctx)
		failure = errors.Join(failure, paneErr)
		if scope.process != nil {
			failure = errors.Join(failure, scope.process.kill())
			defer scope.process.close()
		}
		exitErr := waitOwnedMemberExit(ctx, *scope.pane)
		failure = errors.Join(failure, exitErr)
		cleared = paneErr == nil && exitErr == nil
		if cleared && scope.ownership != nil && scope.ownership.Pane != nil {
			failure = errors.Join(failure, scope.owner.releaseMemberLaunch(ctx, scope.record, scope.run, scope.ownership, *scope.pane))
		}
		cancel()
	}
	if cleared && scope.ownership != nil && scope.ownership.Pane == nil {
		snapshot := cloneMemberOwnership(scope.ownership)
		var session *memberSession
		if scope.run.resume && scope.marked {
			scope.owner.tree.mu.Lock()
			session = cloneMemberSession(scope.record.nativeSession)
			scope.owner.tree.mu.Unlock()
			if session != nil {
				session.Launch = snapshot.Launch
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		failure = errors.Join(failure, scope.owner.clearMemberOwnership(ctx, scope.record, scope.run, scope.ownership, snapshot, session))
		cancel()
	}
	scope.owner.tree.mu.Lock()
	retained := scope.record.nativeOwnership != nil
	scope.owner.tree.mu.Unlock()
	if retained || scope.attempted && scope.pane == nil {
		if failure == nil {
			failure = errors.New("native pane ownership remains fenced for reconciliation")
		}
		return failure
	}
	if scope.fileInfo != nil {
		current, err := os.Lstat(scope.manifest)
		if err != nil || !os.SameFile(current, scope.fileInfo) {
			return errors.Join(failure, errors.New("native member manifest identity changed; refusing cleanup"))
		}
		failure = errors.Join(failure, os.Remove(scope.manifest))
	}
	current, err := os.Lstat(scope.directory)
	if err != nil || !os.SameFile(current, scope.dirInfo) {
		return errors.Join(failure, errors.New("native member directory identity changed; refusing cleanup"))
	}
	return errors.Join(failure, os.Remove(scope.directory))
}

func (o *owner) noteNativeCleanupFailure(r *record, selected *run, failure error) {
	description, _ := bounded("native cleanup failed: "+failure.Error(), 1024)
	o.tree.mu.Lock()
	current := o.tree.records[r.id] == r && r.run == selected
	if current {
		r.nativeCleanupError = description
	}
	o.tree.mu.Unlock()
	if current && o.current() == nil {
		_ = o.saveChild(r)
	}
}

func waitOwnedMemberExit(ctx context.Context, pane tmuxPane) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		birth, err := processBirth(pane.pid)
		if errors.Is(err, os.ErrNotExist) || err == nil && birth != pane.birth {
			return nil
		}
		if err != nil {
			return fmt.Errorf("verify native member exit: %w", err)
		}
		select {
		case <-ctx.Done():
			return reject("unavailable", "native member did not exit after pane shutdown")
		case <-ticker.C:
		}
	}
}
