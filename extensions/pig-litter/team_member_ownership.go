package piglitter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"time"
	"unicode"
)

func memberExecutableDigest(ctx context.Context, path string) (string, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() {
		return "", errors.New("native member executable is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return "", errors.New("native member executable changed while opening")
	}
	hash := sha256.New()
	if _, err := io.Copy(hash, contextReader{ctx: ctx, r: file}); err != nil {
		return "", err
	}
	after, err := os.Lstat(path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return "", errors.New("native member executable changed during hashing")
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func validMemberOwnership(ownership *memberOwnership, member *teamMember, cwd, model, execution string) bool {
	if ownership == nil || member == nil || !isPaneBackend(ownership.Backend) || ownership.Member != member.Ref || ownership.Admission != member.AdmissionID || ownership.Admission == "" || ownership.Execution != execution || ownership.Execution == "" || len(ownership.Execution) > 128 || ownership.Cwd != cwd || ownership.Model != model || !exactModel.MatchString(model) || !tmuxLaunchNonce.MatchString(ownership.Launch) {
		return false
	}
	if ownership.Tools == nil || !slices.IsSorted(ownership.Tools) {
		return false
	}
	if ownership.LaunchPhase != "" && ownership.LaunchPhase != "reserved" && ownership.LaunchPhase != "armed" || ownership.LaunchPhase == "reserved" && ownership.Pane != nil {
		return false
	}
	if len(ownership.ProcessScope) > 128 || strings.IndexFunc(ownership.ProcessScope, unicode.IsControl) >= 0 {
		return false
	}
	for index, tool := range ownership.Tools {
		if !validToolPattern(tool) || strings.ContainsAny(tool, "*?[") || index > 0 && ownership.Tools[index-1] == tool {
			return false
		}
	}
	if !safeLaunchPath(ownership.Executable) || !safeLaunchPath(ownership.Manifest) || filepath.Base(ownership.Manifest) != "member-"+ownership.Launch+".json" || !filepath.IsAbs(ownership.Cwd) || filepath.Clean(ownership.Cwd) != ownership.Cwd || strings.IndexFunc(ownership.Cwd, unicode.IsControl) >= 0 || !filepath.IsAbs(ownership.SessionDirectory) || filepath.Clean(ownership.SessionDirectory) != ownership.SessionDirectory || strings.IndexFunc(ownership.SessionDirectory, unicode.IsControl) >= 0 {
		return false
	}
	digest, err := hex.DecodeString(ownership.ExecutableSHA256)
	if err != nil || len(digest) != sha256.Size || hex.EncodeToString(digest) != ownership.ExecutableSHA256 {
		return false
	}
	server := ownership.Server
	if !filepath.IsAbs(server.Socket) || strings.IndexFunc(server.Socket, unicode.IsControl) >= 0 || server.PID <= 0 || server.Birth == "" || len(server.Birth) > 128 || strings.IndexFunc(server.Birth, unicode.IsControl) >= 0 || ownership.Backend == backendTmux && !tmuxPaneID.MatchString(server.Target) || ownership.Backend == backendITerm && (!itermSessionID.MatchString(server.Target) || !itermWindowID.MatchString(server.Window) || !itermTTY.MatchString(server.TTY)) {
		return false
	}
	if pane := ownership.Pane; pane != nil {
		command := ownership.Executable + " --pig-litter-member " + ownership.Manifest
		if ownership.Backend == backendTmux && !tmuxPaneID.MatchString(pane.ID) || ownership.Backend == backendITerm && (!itermSessionID.MatchString(pane.ID) || !itermTTY.MatchString(pane.TTY) || pane.TTY == server.TTY) || pane.ID == server.Target || pane.PID <= 0 || pane.Birth == "" || len(pane.Birth) > 128 || strings.IndexFunc(pane.Birth, unicode.IsControl) >= 0 || pane.Command != command {
			return false
		}
	}
	return true
}

func verifyMemberPane(ctx context.Context, server tmuxServer, pane memberPaneIdentity) error {
	if server.iterm != nil {
		return server.iterm.verifyPane(ctx, pane)
	}
	if err := server.verify(ctx); err != nil {
		return err
	}
	value, err := server.call(ctx, "display-message", "-p", "-t", pane.ID, tmuxPaneFormat)
	if err != nil {
		return err
	}
	state, err := parseTmuxPane(value)
	if err != nil || state.dead || state.id != pane.ID || state.id == server.target || state.pid != pane.PID || state.command != pane.Command {
		return reject("unavailable", "native pane identity changed")
	}
	birth, err := processBirth(pane.PID)
	if err != nil || birth != pane.Birth {
		return reject("unavailable", "native pane process identity changed")
	}
	return nil
}

func cloneMemberOwnership(ownership *memberOwnership) *memberOwnership {
	if ownership == nil {
		return nil
	}
	copy := *ownership
	copy.Tools = slices.Clone(ownership.Tools)
	if ownership.Pane != nil {
		pane := *ownership.Pane
		copy.Pane = &pane
	}
	return &copy
}

func cloneMemberSession(session *memberSession) *memberSession {
	if session == nil {
		return nil
	}
	copy := *session
	copy.Tools = slices.Clone(session.Tools)
	return &copy
}

func validMemberSession(session *memberSession, id ChildID, cwd, model string) bool {
	if session == nil || !childUUID.MatchString(string(id)) || !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd || strings.IndexFunc(cwd, unicode.IsControl) >= 0 || session.Model != model || !exactModel.MatchString(model) || !tmuxLaunchNonce.MatchString(session.Launch) || !filepath.IsAbs(session.File) || filepath.Clean(session.File) != session.File || strings.IndexFunc(session.File, unicode.IsControl) >= 0 || !strings.HasSuffix(filepath.Base(session.File), "_"+string(id)+".jsonl") || session.Tools == nil || !slices.IsSorted(session.Tools) {
		return false
	}
	for index, tool := range session.Tools {
		if !validToolPattern(tool) || strings.ContainsAny(tool, "*?[") || index > 0 && session.Tools[index-1] == tool {
			return false
		}
	}
	return true
}

func sameMemberOwnership(left, right *memberOwnership) bool {
	if left == nil || right == nil || (left.Pane == nil) != (right.Pane == nil) {
		return false
	}
	return reflect.DeepEqual(left, right)
}

func (o *owner) reserveMemberLaunch(ctx context.Context, r *record, selected *run, server tmuxServer, launch tmuxLaunch, sessionDirectory string) (*memberOwnership, error) {
	if err := o.current(); err != nil {
		return nil, err
	}
	if err := launch.validate(); err != nil {
		return nil, err
	}
	if err := server.verify(ctx); err != nil {
		return nil, err
	}
	processScope, err := memberProcessScope(os.Getpid())
	if err != nil {
		return nil, err
	}
	serverScope, err := memberProcessScope(server.pid)
	if err != nil || serverScope != processScope {
		return nil, reject("unavailable", "tmux server process scope is unavailable")
	}
	directory, err := canonicalTeamDirectory(sessionDirectory)
	if err != nil || directory != sessionDirectory {
		return nil, reject("unavailable", "native session directory is not canonical")
	}
	info, err := os.Stat(directory)
	if err != nil || info.Mode().Perm()&0o077 != 0 {
		return nil, reject("unavailable", "native session directory must be private")
	}
	digest, err := memberExecutableDigest(ctx, launch.executable)
	if err != nil {
		return nil, err
	}
	available, err := o.rootToolNames()
	if err != nil {
		return nil, err
	}
	o.tree.lifecycleMu.Lock()
	o.tree.mu.Lock()
	current, err := o.tree.currentLocked(selected)
	o.state.mu.Lock()
	active := o.state.owner == o && o.state.epoch == o.epoch
	o.state.mu.Unlock()
	selected.mu.Lock()
	hasRuntime := selected.session != nil || selected.member != nil
	selected.mu.Unlock()
	if !active || err != nil || current != r || r.team == nil || r.paused || selected.ctx.Err() != nil || r.activeLog != nil || hasRuntime || r.nativeOwnership != nil || r.nativeReleasing || launch.cwd != r.cwd || !exactModel.MatchString(r.model) {
		o.tree.mu.Unlock()
		o.tree.lifecycleMu.Unlock()
		return nil, reject("unavailable", "native member launch is no longer current")
	}
	tools := slices.Sorted(maps.Keys(childAllowedTools(r, o.tree.root, available)))
	if tools == nil {
		tools = []string{}
	}
	ownership := &memberOwnership{
		Backend: server.backend(), Member: r.team.Ref, Admission: r.team.AdmissionID, Execution: selected.noticeID, OwnerEpoch: o.epoch, ProcessScope: processScope,
		Launch: launch.nonce, Cwd: r.cwd, Model: r.model, Tools: tools, SessionDirectory: sessionDirectory, Executable: launch.executable, ExecutableSHA256: digest, Manifest: launch.manifest,
		Server: server.identity(),
	}
	if selected.resume {
		ownership.LaunchPhase = "reserved"
	}
	r.nativeOwnership = ownership
	o.tree.mu.Unlock()
	o.tree.lifecycleMu.Unlock()
	if err := o.saveChild(r); err != nil {
		return ownership, fmt.Errorf("persist native launch intent: %w", err)
	}
	return ownership, nil
}

func (o *owner) armMemberLaunch(r *record, selected *run, ownership *memberOwnership) error {
	o.tree.lifecycleMu.Lock()
	o.tree.mu.Lock()
	current, err := o.tree.currentLocked(selected)
	if err != nil || current != r || r.nativeOwnership != ownership || ownership.LaunchPhase != "reserved" || ownership.Pane != nil || r.paused || selected.ctx.Err() != nil {
		o.tree.mu.Unlock()
		o.tree.lifecycleMu.Unlock()
		return reject("unavailable", "native launch intent changed before allocation")
	}
	ownership.LaunchPhase = "armed"
	o.tree.mu.Unlock()
	o.tree.lifecycleMu.Unlock()
	if err := o.saveChild(r); err != nil {
		return fmt.Errorf("persist native allocation intent: %w", err)
	}
	return nil
}

func (o *owner) recordMemberPane(ctx context.Context, r *record, selected *run, ownership *memberOwnership, pane tmuxPane) error {
	identity := memberPaneIdentity{ID: pane.id, PID: pane.pid, Birth: pane.birth, Command: pane.command, TTY: pane.tty}
	if err := verifyMemberPane(ctx, pane.server, identity); err != nil {
		return err
	}
	processScope, err := memberProcessScope(pane.pid)
	if err != nil || processScope != ownership.ProcessScope {
		return reject("unavailable", "native member process scope is unavailable")
	}
	o.tree.lifecycleMu.Lock()
	o.tree.mu.Lock()
	current, err := o.tree.currentLocked(selected)
	o.state.mu.Lock()
	active := o.state.owner == o && o.state.epoch == o.epoch
	o.state.mu.Unlock()
	server := ownership.Server
	command := ownership.Executable + " --pig-litter-member " + ownership.Manifest
	if !active || err != nil || current != r || selected.ctx.Err() != nil || r.nativeOwnership != ownership || ownership.LaunchPhase == "reserved" || ownership.Pane != nil || pane.server.identity() != server || pane.command != command || pane.pid <= 0 || pane.birth == "" {
		o.tree.mu.Unlock()
		o.tree.lifecycleMu.Unlock()
		return reject("unavailable", "native pane does not match its launch intent")
	}
	ownership.Pane = &identity
	o.tree.mu.Unlock()
	o.tree.lifecycleMu.Unlock()
	if err := o.saveChild(r); err != nil {
		return fmt.Errorf("persist native pane identity: %w", err)
	}
	return nil
}

func (o *owner) releaseMemberLaunch(ctx context.Context, r *record, selected *run, ownership *memberOwnership, pane tmuxPane) error {
	o.tree.mu.Lock()
	current, err := o.tree.currentLocked(selected)
	snapshot := cloneMemberOwnership(ownership)
	if err != nil || current != r || r.nativeOwnership != ownership || snapshot == nil || snapshot.Pane == nil || snapshot.Server != pane.server.identity() || snapshot.Pane.ID != pane.id || snapshot.Pane.PID != pane.pid || snapshot.Pane.Birth != pane.birth || snapshot.Pane.Command != pane.command || snapshot.Pane.TTY != pane.tty {
		o.tree.mu.Unlock()
		return reject("unavailable", "native pane ownership changed before release")
	}
	o.tree.mu.Unlock()
	for {
		birth, err := processBirth(pane.pid)
		if errors.Is(err, os.ErrNotExist) || err == nil && birth != pane.birth {
			break
		}
		if err != nil {
			return fmt.Errorf("verify native member exit: %w", err)
		}
		select {
		case <-ctx.Done():
			return reject("unavailable", "native member has not exited")
		case <-time.After(20 * time.Millisecond):
		}
	}
	if err := pane.close(ctx); err != nil {
		return err
	}
	o.tree.mu.Lock()
	var session *memberSession
	if binding := r.native; binding != nil && binding.log.file != nil {
		session = &memberSession{File: binding.log.path, Model: snapshot.Model, Tools: slices.Clone(snapshot.Tools), Launch: snapshot.Launch}
	} else if selected.resume && r.nativeSession != nil {
		session = cloneMemberSession(r.nativeSession)
		session.Launch = snapshot.Launch
	}
	o.tree.mu.Unlock()
	return o.clearMemberOwnership(ctx, r, selected, ownership, snapshot, session)
}

func (o *owner) clearMemberOwnership(ctx context.Context, r *record, selected *run, ownership, snapshot *memberOwnership, session *memberSession) error {
	// Other persistence paths take lifecycleMu before persistMu, so never wait for the former while holding the latter.
	for {
		o.persistMu.Lock()
		if o.tree.lifecycleMu.TryLock() {
			break
		}
		o.persistMu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Millisecond):
		}
	}
	defer o.persistMu.Unlock()
	o.tree.mu.Lock()
	current, err := o.tree.currentLocked(selected)
	if err != nil || current != r || r.nativeOwnership != ownership || r.nativeReleasing || !sameMemberOwnership(ownership, snapshot) || session != nil && (!validMemberSession(session, r.id, r.cwd, r.model) || filepath.Dir(session.File) != snapshot.SessionDirectory || session.Launch != snapshot.Launch || !slices.Equal(session.Tools, snapshot.Tools)) {
		o.tree.mu.Unlock()
		o.tree.lifecycleMu.Unlock()
		return reject("unavailable", "native pane ownership changed during release")
	}
	binding := r.native
	cleanupError := r.nativeCleanupError
	reservationConflict := r.nativeReservationConflict
	previousSession := r.nativeSession
	selected.mu.Lock()
	member := selected.member
	selected.member = nil
	selected.mu.Unlock()
	r.nativeReleasing = true
	r.native, r.nativeOwnership = nil, nil
	r.nativeCleanupError = ""
	r.nativeReservationConflict = false
	if session != nil {
		r.nativeSession = cloneMemberSession(session)
	}
	o.tree.mu.Unlock()
	o.tree.lifecycleMu.Unlock()
	err = o.saveChildStateLocked(r, nil)
	o.tree.mu.Lock()
	if o.tree.records[r.id] == r && r.run == selected && r.nativeReleasing && r.nativeOwnership == nil {
		if err != nil {
			r.native, r.nativeOwnership = binding, ownership
			r.nativeCleanupError = cleanupError
			r.nativeReservationConflict = reservationConflict
			r.nativeSession = previousSession
			selected.mu.Lock()
			if selected.member == nil {
				selected.member = member
			}
			selected.mu.Unlock()
		}
		r.nativeReleasing = false
	} else {
		err = errors.Join(err, reject("unavailable", "native ownership changed during persistence"))
	}
	o.tree.mu.Unlock()
	if err != nil {
		return fmt.Errorf("persist native ownership release: %w", err)
	}
	return nil
}
