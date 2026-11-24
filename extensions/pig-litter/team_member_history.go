package piglitter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
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
	if _, err := readMemberLog(ctx, pin); err != nil {
		return memberLogPin{}, err
	}
	return pin, nil
}

func readMemberLog(ctx context.Context, pin memberLogPin) ([]json.RawMessage, error) {
	current, err := os.Lstat(pin.path)
	if err != nil || !current.Mode().IsRegular() || !os.SameFile(current, pin.file) {
		return nil, reject("unavailable", "native session log identity changed")
	}
	file, err := os.Open(pin.path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(opened, pin.file) {
		return nil, reject("unavailable", "native session log identity changed")
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
	if err := decoder.Decode(&header); err != nil || header.Type != "session" || header.ID != string(pin.session) || header.Cwd != pin.cwd {
		return nil, reject("unavailable", "native session log header does not match its owner")
	}
	var entries []json.RawMessage
	for {
		var entry json.RawMessage
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("native session log is incomplete: %w", err)
		}
		entries = append(entries, entry)
	}
	check := sha256.New()
	if _, err := io.Copy(check, contextReader{ctx: ctx, r: io.NewSectionReader(file, 0, size)}); err != nil {
		return nil, err
	}
	after, err := os.Lstat(pin.path)
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(after, pin.file) || after.Size() < size || !bytes.Equal(hash.Sum(nil), check.Sum(nil)) {
		return nil, reject("unavailable", "native session log changed during inspection")
	}
	return entries, nil
}

func (o *owner) installMemberHandle(ctx context.Context, r *record, selected *run, directory, path, launch string, control memberControl) (*memberBinding, error) {
	if control == nil || !tmuxLaunchNonce.MatchString(launch) {
		return nil, reject("unavailable", "native member handle is invalid")
	}
	if err := o.current(); err != nil {
		return nil, err
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
		return nil, reject("unavailable", "native member has no matching owned pane")
	}
	server, err := tmuxFromEnvironment(ctx, fmt.Sprintf("%s,%d,0", snapshot.Server.Socket, snapshot.Server.PID), snapshot.Server.Target)
	if err != nil || server.birth != snapshot.Server.Birth {
		return nil, reject("unavailable", "native member tmux server identity changed")
	}
	if err := verifyMemberPane(ctx, server, *snapshot.Pane); err != nil {
		return nil, err
	}
	native, err := control.Snapshot(ctx, memberHistoryBound{})
	if err != nil {
		return nil, reject("unavailable", "native member readiness is unavailable")
	}
	tools := slices.Clone(native.tools)
	slices.Sort(tools)
	if native.session != r.id || native.cwd != cwd || native.model != model || !slices.Equal(tools, snapshot.Tools) || native.pending != 0 || native.phase != memberStarting && native.phase != memberIdle {
		return nil, reject("unavailable", "native member readiness does not match its launch")
	}
	digest, err := memberProcessDigest(ctx, snapshot.Pane.PID, snapshot.Pane.Birth)
	if err != nil || digest != snapshot.ExecutableSHA256 {
		return nil, reject("unavailable", "native member executable identity does not match its launch")
	}
	pin, err := pinMemberLog(ctx, directory, path, cwd, r.id)
	if err != nil {
		return nil, err
	}
	if err := verifyMemberPane(ctx, server, *snapshot.Pane); err != nil {
		return nil, err
	}
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	o.state.mu.Lock()
	active := o.state.owner == o && o.state.epoch == o.epoch
	o.state.mu.Unlock()
	current, err := o.tree.currentLocked(selected)
	if !active || err != nil || current != r || r.team == nil || r.paused || selected.ctx.Err() != nil || r.native != nil || r.activeLog != nil || r.nativeOwnership != ownership || !sameMemberOwnership(r.nativeOwnership, snapshot) {
		return nil, reject("unavailable", "native member execution is no longer current")
	}
	selected.mu.Lock()
	defer selected.mu.Unlock()
	if selected.session != nil || selected.member != nil {
		return nil, reject("unavailable", "native member execution already has a runtime")
	}
	binding := &memberBinding{record: r, run: selected, member: r.team.Ref, admission: r.team.AdmissionID, execution: selected.noticeID, ownerEpoch: o.epoch, launch: launch, log: pin, ownership: ownership, owned: snapshot, control: control}
	r.native = binding
	selected.member = control
	return binding, nil
}

func (o *owner) memberLogEntries(ctx context.Context, binding *memberBinding) ([]json.RawMessage, error) {
	if err := o.current(); err != nil {
		return nil, err
	}
	entries, err := readMemberLog(ctx, binding.log)
	if err != nil {
		return nil, err
	}
	if err := o.current(); err != nil {
		return nil, err
	}
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	if !o.memberBindingCurrentLocked(binding) {
		return nil, reject("unavailable", "native member execution changed during inspection")
	}
	return entries, nil
}

func (o *owner) memberBindingCurrentLocked(binding *memberBinding) bool {
	o.state.mu.Lock()
	active := o.state.owner == o && o.state.epoch == o.epoch
	o.state.mu.Unlock()
	r := binding.record
	return active && !o.tree.closed && o.tree.records[r.id] == r && r.run == binding.run && r.native == binding && r.nativeOwnership == binding.ownership && sameMemberOwnership(r.nativeOwnership, binding.owned) && r.team != nil && r.team.Ref == binding.member && r.team.AdmissionID == binding.admission && binding.run.noticeID == binding.execution && o.epoch == binding.ownerEpoch
}

func (o *owner) memberCallTicket(binding *memberBinding, callID, operation, leaf string) (*callTicket, error) {
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
	scope := o.callScope(binding.run)
	if scope == nil {
		return nil, reject("unavailable", "native member has no active turn")
	}
	return scope.beginWithLeaf(binding.run, callID, operation, captured)
}
