package piglitter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/MichaelKinsy/PiG/coding"
)

func (o *owner) prepareNativeResume(ctx context.Context, r *record, selected *run) (*memberResumePin, error) {
	if err := o.current(); err != nil {
		return nil, err
	}
	o.tree.mu.Lock()
	if o.tree.records[r.id] != r || r.run != selected || r.backend != backendTmux || r.nativeReleasing || r.native != nil || !selected.state.terminal() || selected.finishing {
		o.tree.mu.Unlock()
		return nil, reject("unavailable", "native teammate is not ready for reconciliation")
	}
	if selected.executorDone != nil {
		select {
		case <-selected.executorDone:
		default:
			o.tree.mu.Unlock()
			return nil, reject("unavailable", "native teammate cleanup is still running")
		}
	}
	ownership := r.nativeOwnership
	prior := cloneMemberOwnership(ownership)
	session := cloneMemberSession(r.nativeSession)
	conflict := r.nativeReservationConflict
	var member *teamMember
	if r.team != nil {
		copy := *r.team
		member = &copy
	}
	o.tree.mu.Unlock()
	var store *teamStore
	unstarted := false
	previousLaunch := ""
	if conflict {
		if prior == nil || member == nil || !validMemberSession(session, r.id, r.cwd, r.model) || filepath.Dir(session.File) != prior.SessionDirectory || !slices.Equal(session.Tools, prior.Tools) {
			return nil, reject("unavailable", "native launch reservation requires reconciliation")
		}
		previousLaunch = session.Launch
		var err error
		store, err = o.teamStoreForRoot()
		if err != nil {
			return nil, err
		}
		_, reservations, err := store.recoverySnapshot()
		if err != nil {
			return nil, err
		}
		matched := slices.ContainsFunc(reservations, func(reservation teamReservation) bool {
			return reservation.Member.Ref == member.Ref && reservation.Member.AdmissionID == member.AdmissionID && reservation.Member.Workspace == member.Workspace && reservation.NativeLaunch == session.Launch
		})
		if !matched {
			return nil, reject("unavailable", "native launch reservation changed during reconciliation")
		}
		unstarted = prior.LaunchPhase == "reserved" && prior.Pane == nil
	}
	if prior != nil {
		var err error
		if unstarted {
			err = confirmReservedMemberUnstarted(ctx, prior)
		} else {
			err = confirmOldMemberGone(ctx, prior)
		}
		if err != nil {
			return nil, err
		}
		if !unstarted {
			path, err := uniqueNativeSession(prior.SessionDirectory, r.id)
			if err != nil {
				return nil, err
			}
			session = &memberSession{File: path, Model: prior.Model, Tools: slices.Clone(prior.Tools), Launch: prior.Launch}
		}
	}
	if !validMemberSession(session, r.id, r.cwd, r.model) {
		return nil, reject("unavailable", "native teammate has no retained session to resume")
	}
	available, err := o.rootToolNames()
	if err != nil {
		return nil, err
	}
	tools := slices.Sorted(maps.Keys(childAllowedTools(r, o.tree.root, available)))
	if !slices.Equal(tools, session.Tools) {
		return nil, reject("unavailable", "native teammate tool ceiling changed before resume")
	}
	pin, err := pinMemberLog(ctx, filepath.Dir(session.File), session.File, r.cwd, r.id)
	if err != nil {
		return nil, err
	}
	resume, err := o.projectNativeResume(ctx, pin, r.cwd, r.id)
	if err != nil {
		return nil, err
	}
	if prior != nil {
		if err := o.current(); err != nil {
			return nil, err
		}
		if conflict && !unstarted {
			if err := store.markNativeLaunch(*member, prior.Launch, previousLaunch); err != nil {
				return nil, err
			}
		}
		clearedSession := session
		if unstarted {
			clearedSession = nil
		}
		if err := o.clearMemberOwnership(ctx, r, selected, ownership, prior, clearedSession); err != nil {
			return nil, err
		}
	}
	return resume, nil
}

func confirmReservedMemberUnstarted(ctx context.Context, ownership *memberOwnership) error {
	if ownership == nil || ownership.LaunchPhase != "reserved" || ownership.Pane != nil {
		return reject("unavailable", "native allocation intent is uncertain")
	}
	scope, err := memberProcessScope(os.Getpid())
	if err != nil || ownership.ProcessScope == "" || scope != ownership.ProcessScope {
		return reject("unavailable", "native process scope is unavailable or changed")
	}
	command, err := exec.LookPath("tmux")
	if err != nil {
		return reject("unavailable", "tmux is unavailable for native reconciliation")
	}
	identity := ownership.Server
	server := tmuxServer{command: command, socket: identity.Socket, pid: identity.PID, birth: identity.Birth, target: identity.Target}
	if err := server.verify(ctx); err != nil {
		return reject("unavailable", "native tmux server is unavailable or changed")
	}
	serverScope, err := memberProcessScope(identity.PID)
	if err != nil || serverScope != scope {
		return reject("unavailable", "native tmux server scope is unavailable or changed")
	}
	panes, err := server.panes(ctx)
	if err != nil {
		return reject("unavailable", "native tmux panes cannot be verified")
	}
	launchCommand := ownership.Executable + " --pig-litter-member " + ownership.Manifest
	target := false
	for _, pane := range panes {
		target = target || pane.id == identity.Target
		if pane.command == launchCommand {
			return reject("unavailable", "native allocation may have started")
		}
	}
	if !target || server.verify(ctx) != nil {
		return reject("unavailable", "native tmux server or target changed")
	}
	return nil
}

func (o *owner) projectNativeResume(ctx context.Context, pin memberLogPin, cwd string, id ChildID) (*memberResumePin, error) {
	snapshot, err := readMemberLog(ctx, pin)
	if err != nil {
		return nil, err
	}
	directory, err := os.MkdirTemp("", "pig-resume-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(directory)
	path := filepath.Join(directory, "history_"+string(id)+".jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	writer := bufio.NewWriter(file)
	for _, entry := range append([]json.RawMessage{snapshot.header}, snapshot.entries...) {
		if err == nil {
			err = ctx.Err()
		}
		if err == nil {
			_, err = writer.Write(entry)
		}
		if err == nil {
			err = writer.WriteByte('\n')
		}
	}
	if err == nil {
		err = writer.Flush()
	}
	if err = errors.Join(err, file.Close()); err != nil {
		return nil, err
	}
	trusted := true
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: cwd, AgentDir: o.agentDir, ProjectTrusted: &trusted})
	if err != nil {
		return nil, err
	}
	defer services.Close()
	manager, err := coding.SessionManagerFor(services, coding.SessionStartOptions{ResumePath: path, SessionDir: directory})
	if err != nil || manager.ID() != string(id) || manager.GetCwd() != cwd {
		return nil, reject("unavailable", "native resume history cannot be opened safely")
	}
	projected, err := rawEntries(manager.GetEntries())
	if err != nil {
		return nil, err
	}
	check, err := pinMemberLog(ctx, directory, path, cwd, id)
	if err != nil {
		return nil, err
	}
	copySnapshot, err := readMemberLog(ctx, check)
	if err != nil || !bytes.Equal(copySnapshot.header, snapshot.header) || len(copySnapshot.entries) != len(snapshot.entries) || memberEntriesDigest(copySnapshot.entries) != memberEntriesDigest(snapshot.entries) {
		return nil, reject("unavailable", "native resume history requires migration")
	}
	current, err := readMemberLog(ctx, pin)
	if err != nil || len(current.entries) != pin.baselineCount || memberEntriesDigest(current.entries) != pin.baselineHash {
		return nil, reject("unavailable", "native resume history changed during verification")
	}
	return &memberResumePin{log: pin, count: len(projected), digest: memberEntriesDigest(projected), leaf: manager.GetLeafID()}, nil
}

func uniqueNativeSession(directory string, id ChildID) (string, error) {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return "", err
	}
	var path string
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), "_"+string(id)+".jsonl") {
			continue
		}
		if path != "" {
			return "", reject("unavailable", "native teammate has multiple session logs")
		}
		path = filepath.Join(directory, entry.Name())
	}
	if path == "" {
		return "", reject("unavailable", "native teammate session log is unavailable")
	}
	return path, nil
}

func confirmOldMemberGone(ctx context.Context, ownership *memberOwnership) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if ownership == nil || ownership.Pane == nil {
		return reject("unavailable", "native pane identity is not known")
	}
	scope, err := memberProcessScope(os.Getpid())
	if err != nil || ownership.ProcessScope == "" || scope != ownership.ProcessScope {
		return reject("unavailable", "previous native process scope is unavailable or changed")
	}
	if _, err := processBirth(ownership.Pane.PID); !errors.Is(err, os.ErrNotExist) {
		return reject("unavailable", "previous native process is live or its identity is uncertain")
	}
	server := ownership.Server
	birth, err := processBirth(server.PID)
	if errors.Is(err, os.ErrNotExist) {
		if _, socketErr := os.Lstat(server.Socket); errors.Is(socketErr, os.ErrNotExist) {
			return nil
		}
		return reject("unavailable", "previous tmux socket is still present or uncertain")
	}
	if err != nil || birth != server.Birth {
		return reject("unavailable", "previous tmux server identity is uncertain")
	}
	command, err := exec.LookPath("tmux")
	if err != nil {
		return reject("unavailable", "tmux is unavailable for native reconciliation")
	}
	current := tmuxServer{command: command, socket: server.Socket, pid: server.PID, birth: server.Birth, target: server.Target}
	if err := current.verify(ctx); err != nil {
		return reject("unavailable", "previous tmux server cannot be verified")
	}
	panes, err := current.panes(ctx)
	if err != nil {
		return reject("unavailable", "previous tmux panes cannot be verified")
	}
	for _, pane := range panes {
		if pane.id == ownership.Pane.ID {
			return reject("unavailable", "previous native pane still exists or was changed")
		}
	}
	if err := current.verify(ctx); err != nil {
		return reject("unavailable", "previous tmux server changed during reconciliation")
	}
	return nil
}
