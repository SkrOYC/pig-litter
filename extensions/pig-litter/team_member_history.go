package piglitter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

func pinMemberLog(ctx context.Context, directory, path, cwd string, session ChildID) (memberLogPin, error) {
	var pin memberLogPin
	if !filepath.IsAbs(directory) || !filepath.IsAbs(path) || filepath.Dir(path) != directory || !childUUID.MatchString(string(session)) || !strings.HasSuffix(filepath.Base(path), "_"+string(session)+".jsonl") {
		return pin, reject("unavailable", "native session log location is invalid")
	}
	actual, err := canonicalTeamDirectory(directory)
	if err != nil || actual != directory {
		return pin, reject("unavailable", "native session directory is not canonical")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return pin, reject("unavailable", "native session log is not a regular file")
	}
	pin = memberLogPin{path: path, cwd: cwd, session: session, file: info}
	snapshot, err := readMemberLog(ctx, pin)
	if err != nil {
		return memberLogPin{}, err
	}
	pin.header = bytes.Clone(snapshot.header)
	pin.baselineCount, pin.baselineHash, pin.baselineSet = len(snapshot.entries), memberEntriesDigest(snapshot.entries), true
	return pin, nil
}

func memberEntriesDigest(entries []json.RawMessage) [32]byte {
	hash := sha256.New()
	var size [8]byte
	for _, entry := range entries {
		binary.BigEndian.PutUint64(size[:], uint64(len(entry)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write(entry)
	}
	var digest [32]byte
	copy(digest[:], hash.Sum(nil))
	return digest
}

func readMemberLog(ctx context.Context, pin memberLogPin) (memberLogSnapshot, error) {
	var snapshot memberLogSnapshot
	current, err := os.Lstat(pin.path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, pin.file) {
		return snapshot, reject("unavailable", "native session log identity changed")
	}
	file, err := os.Open(pin.path)
	if err != nil {
		return snapshot, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(opened, pin.file) {
		return snapshot, reject("unavailable", "native session log identity changed")
	}
	size := opened.Size()
	hash := sha256.New()
	reader := contextReader{ctx: ctx, r: io.TeeReader(io.NewSectionReader(file, 0, size), hash)}
	decoder := json.NewDecoder(bufio.NewReader(reader))
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Cwd  string `json:"cwd"`
	}
	if err := decoder.Decode(&snapshot.header); err != nil || json.Unmarshal(snapshot.header, &header) != nil || header.Type != "session" || header.ID != string(pin.session) || header.Cwd != pin.cwd || pin.header != nil && !bytes.Equal(pin.header, snapshot.header) {
		return snapshot, reject("unavailable", "native session log header does not match its owner")
	}
	for {
		var entry json.RawMessage
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return snapshot, fmt.Errorf("native session log is incomplete: %w", err)
		}
		snapshot.entries = append(snapshot.entries, entry)
	}
	check := sha256.New()
	if _, err := io.Copy(check, contextReader{ctx: ctx, r: io.NewSectionReader(file, 0, size)}); err != nil {
		return snapshot, err
	}
	after, err := os.Lstat(pin.path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(after, pin.file) || after.Size() < size || !bytes.Equal(hash.Sum(nil), check.Sum(nil)) {
		return snapshot, reject("unavailable", "native session log changed during inspection")
	}
	if pin.baselineSet && (len(snapshot.entries) < pin.baselineCount || memberEntriesDigest(snapshot.entries[:pin.baselineCount]) != pin.baselineHash) {
		return snapshot, reject("unavailable", "native session history changed before its execution baseline")
	}
	return snapshot, nil
}

type memberInstall struct {
	ownership *memberOwnership
	owned     *memberOwnership
	server    tmuxServer
	cwd       string
	model     string
}

func (o *owner) memberLaunchForInstall(ctx context.Context, r *record, selected *run, directory, launch string, control memberControl) (memberInstall, error) {
	var install memberInstall
	if control == nil || !tmuxLaunchNonce.MatchString(launch) {
		return install, reject("unavailable", "native member handle is invalid")
	}
	if err := o.current(); err != nil {
		return install, err
	}
	o.tree.mu.Lock()
	ownership := r.nativeOwnership
	snapshot := cloneMemberOwnership(ownership)
	var member *teamMember
	if r.team != nil {
		copy := *r.team
		member = &copy
	}
	cwd, model := r.cwd, r.model
	o.tree.mu.Unlock()
	if snapshot == nil || snapshot.Pane == nil || !validMemberOwnership(snapshot, member, cwd, model, selected.noticeID) || snapshot.OwnerEpoch != o.epoch || snapshot.Launch != launch || snapshot.SessionDirectory != directory {
		return install, reject("unavailable", "native member has no matching owned pane")
	}
	server, err := tmuxFromEnvironment(ctx, fmt.Sprintf("%s,%d,0", snapshot.Server.Socket, snapshot.Server.PID), snapshot.Server.Target)
	if err != nil || server.birth != snapshot.Server.Birth {
		return install, reject("unavailable", "native member tmux server identity changed")
	}
	if err := verifyMemberPane(ctx, server, *snapshot.Pane); err != nil {
		return install, err
	}
	digest, err := memberProcessDigest(ctx, snapshot.Pane.PID, snapshot.Pane.Birth)
	if err != nil || digest != snapshot.ExecutableSHA256 {
		return install, reject("unavailable", "native member executable identity does not match its launch")
	}
	return memberInstall{ownership: ownership, owned: snapshot, server: server, cwd: cwd, model: model}, nil
}

func (o *owner) installProvisionalMemberHandle(ctx context.Context, r *record, selected *run, directory, launch string, control memberControl) (*memberBinding, error) {
	install, err := o.memberLaunchForInstall(ctx, r, selected, directory, launch, control)
	if err != nil {
		return nil, err
	}
	return o.bindMemberHandle(r, selected, launch, control, install, memberLogPin{}, false)
}

func (o *owner) installMemberHandle(ctx context.Context, r *record, selected *run, directory, path, launch string, control memberControl) (*memberBinding, error) {
	install, err := o.memberLaunchForInstall(ctx, r, selected, directory, launch, control)
	if err != nil {
		return nil, err
	}
	native, err := control.Snapshot(ctx, memberHistoryBound{})
	if err != nil {
		return nil, reject("unavailable", "native member readiness is unavailable")
	}
	tools := slices.Clone(native.tools)
	slices.Sort(tools)
	if native.session != r.id || native.cwd != install.cwd || native.model != install.model || !slices.Equal(tools, install.owned.Tools) || native.pending != 0 || native.phase != memberStarting && native.phase != memberIdle {
		return nil, reject("unavailable", "native member readiness does not match its launch")
	}
	pin, err := pinMemberLog(ctx, directory, path, install.cwd, r.id)
	if err != nil {
		return nil, err
	}
	if native.leaf != nil {
		snapshot, err := readMemberLog(ctx, pin)
		if err != nil {
			return nil, err
		}
		found := false
		for _, raw := range snapshot.entries {
			var entry struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &entry) == nil && entry.ID != "" && entry.ID == *native.leaf {
				found = true
				break
			}
		}
		if !found {
			return nil, reject("unavailable", "native readiness leaf is not in the owned session log")
		}
	}
	if err := verifyMemberPane(ctx, install.server, *install.owned.Pane); err != nil {
		return nil, err
	}
	return o.bindMemberHandle(r, selected, launch, control, install, pin, true)
}

func (o *owner) bindMemberHandle(r *record, selected *run, launch string, control memberControl, install memberInstall, pin memberLogPin, ready bool) (*memberBinding, error) {
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	o.state.mu.Lock()
	active := o.state.owner == o && o.state.epoch == o.epoch
	o.state.mu.Unlock()
	current, err := o.tree.currentLocked(selected)
	if !active || err != nil || current != r || r.team == nil || r.paused || selected.ctx.Err() != nil || r.native != nil || r.activeLog != nil || r.nativeOwnership != install.ownership || !sameMemberOwnership(r.nativeOwnership, install.owned) {
		return nil, reject("unavailable", "native member execution is no longer current")
	}
	selected.mu.Lock()
	defer selected.mu.Unlock()
	if selected.session != nil || selected.member != nil {
		return nil, reject("unavailable", "native member execution already has a runtime")
	}
	binding := &memberBinding{record: r, run: selected, member: r.team.Ref, admission: r.team.AdmissionID, execution: selected.noticeID, ownerEpoch: o.epoch, launch: launch, log: pin, ownership: install.ownership, owned: install.owned, control: control, ready: ready}
	if queue, ok := control.(*memberControlQueue); ok {
		if err := queue.bind(binding); err != nil {
			return nil, err
		}
	}
	r.native = binding
	selected.member = control
	return binding, nil
}

func (o *owner) memberLogEntries(ctx context.Context, binding *memberBinding) ([]json.RawMessage, error) {
	snapshot, err := o.memberLogSnapshot(ctx, binding)
	return snapshot.entries, err
}

func (o *owner) memberLogSnapshot(ctx context.Context, binding *memberBinding) (memberLogSnapshot, error) {
	var empty memberLogSnapshot
	if err := o.current(); err != nil {
		return empty, err
	}
	if err := o.ensureMemberLog(ctx, binding); err != nil {
		return empty, err
	}
	o.tree.mu.Lock()
	if !o.memberBindingCurrentLocked(binding) || !binding.ready || binding.log.file == nil {
		o.tree.mu.Unlock()
		return empty, reject("unavailable", "native member history is not ready")
	}
	pin := binding.log
	o.tree.mu.Unlock()
	snapshot, err := readMemberLog(ctx, pin)
	if err != nil {
		return empty, err
	}
	if err := o.current(); err != nil {
		return empty, err
	}
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	if !o.memberBindingCurrentLocked(binding) {
		return empty, reject("unavailable", "native member execution changed during inspection")
	}
	return snapshot, nil
}

func (o *owner) memberBindingCurrentLocked(binding *memberBinding) bool {
	o.state.mu.Lock()
	active := o.state.owner == o && o.state.epoch == o.epoch
	o.state.mu.Unlock()
	r := binding.record
	return active && !o.tree.closed && o.tree.records[r.id] == r && r.run == binding.run && r.native == binding && r.nativeOwnership == binding.ownership && sameMemberOwnership(r.nativeOwnership, binding.owned) && r.team != nil && r.team.Ref == binding.member && r.team.AdmissionID == binding.admission && binding.run.noticeID == binding.execution && o.epoch == binding.ownerEpoch
}

func (o *owner) memberCallTicket(binding *memberBinding, scope *callScope, callID, operation, leaf string) (*callTicket, error) {
	if err := o.current(); err != nil {
		return nil, err
	}
	var captured *string
	if operation == "litter_wait" || operation == "litter_stop" {
		entries, err := o.memberLogEntries(binding.run.ctx, binding)
		if err != nil {
			return nil, err
		}
		found := leaf == ""
		for _, raw := range entries {
			var entry struct {
				ID string `json:"id"`
			}
			if json.Unmarshal(raw, &entry) == nil && entry.ID == leaf {
				found = true
				break
			}
		}
		if !found {
			return nil, reject("unavailable", "native tool call leaf is not in the owned session log")
		}
		captured = &leaf
	}
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	if !o.memberBindingCurrentLocked(binding) || binding.run.ctx.Err() != nil || binding.record.paused {
		return nil, reject("unavailable", "native member execution is no longer current")
	}
	if !slices.Contains(binding.owned.Tools, operation) || !slices.Contains(teamToolNames, operation) && (!binding.record.canDelegate || !slices.Contains(controlNames, operation)) {
		return nil, reject("permission_denied", "native member cannot invoke this operation")
	}
	if scope == nil || scope != o.callScope(binding.run) {
		return nil, reject("unavailable", "native member has no active turn")
	}
	return scope.beginWithLeaf(binding.run, callID, operation, captured)
}
