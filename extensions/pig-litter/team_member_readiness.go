package piglitter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

type memberReadiness struct {
	Window        string            `json:"window"`
	Session       ChildID           `json:"session"`
	Cwd           string            `json:"cwd"`
	Model         string            `json:"model"`
	Tools         []string          `json:"tools"`
	SessionFile   string            `json:"sessionFile"`
	Leaf          *string           `json:"leaf"`
	Entries       []json.RawMessage `json:"entries"`
	HistoryCount  *int              `json:"historyCount,omitempty"`
	HistoryDigest string            `json:"historyDigest,omitempty"`
}

type memberStartingLog struct {
	path    string
	entries []json.RawMessage
}

func (queue *memberControlQueue) acceptReadiness(ctx context.Context, request memberReadiness) error {
	queue.mu.Lock()
	binding, handoff := queue.binding, queue.starting
	installed := handoff != nil && handoff.installed && handoff.window == request.Window
	queue.mu.Unlock()
	if binding == nil || !installed || !queue.bound() {
		return reject("unavailable", "native starting transfer is not installed")
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(encoded)
	binding.readyMu.Lock()
	if binding.readyDone != nil {
		if binding.readyHash != digest {
			binding.readyMu.Unlock()
			return reject("unavailable", "native readiness conflicts with its first reply")
		}
		done := binding.readyDone
		binding.readyMu.Unlock()
		select {
		case <-done:
			binding.readyMu.Lock()
			err := binding.readyErr
			binding.readyMu.Unlock()
			return err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	binding.readyHash, binding.readyDone = digest, make(chan struct{})
	done := binding.readyDone
	binding.readyMu.Unlock()
	err = queue.installReadiness(ctx, binding, request)
	binding.readyMu.Lock()
	binding.readyErr = err
	close(done)
	binding.readyMu.Unlock()
	return err
}

func (queue *memberControlQueue) installReadiness(ctx context.Context, binding *memberBinding, request memberReadiness) error {
	tools := slices.Clone(request.Tools)
	slices.Sort(tools)
	owned := binding.owned
	if request.Session != binding.member.ID || request.Cwd != owned.Cwd || request.Model != owned.Model || !slices.Equal(tools, owned.Tools) || !filepath.IsAbs(request.SessionFile) || filepath.Dir(request.SessionFile) != owned.SessionDirectory || !strings.HasSuffix(filepath.Base(request.SessionFile), "_"+string(binding.member.ID)+".jsonl") {
		return reject("unavailable", "native readiness does not match its owned launch")
	}
	binding.run.mu.Lock()
	resume := binding.run.nativeResume
	binding.run.mu.Unlock()
	var pin memberLogPin
	var fresh *memberStartingLog
	entries := request.Entries
	if binding.run.resume {
		if resume == nil || request.HistoryCount == nil || request.SessionFile != resume.log.path || len(request.Entries) != 0 {
			return reject("unavailable", "native resume readiness has no matching history")
		}
		snapshot, err := readMemberLog(ctx, resume.log)
		sameLeaf := request.Leaf == nil && resume.leaf == nil || request.Leaf != nil && resume.leaf != nil && *request.Leaf == *resume.leaf
		if err != nil || len(snapshot.entries) != resume.log.baselineCount || memberEntriesDigest(snapshot.entries) != resume.log.baselineHash || *request.HistoryCount != resume.count || request.HistoryDigest != hex.EncodeToString(resume.digest[:]) || !sameLeaf {
			return reject("unavailable", "native resume readiness does not match its history")
		}
		pin, entries = resume.log, snapshot.entries
	} else if request.HistoryCount != nil || request.HistoryDigest != "" || len(entries) > 200 {
		return reject("unavailable", "native readiness history exceeds its bound")
	}
	bytesTotal, foundLeaf := 0, request.Leaf == nil
	for _, raw := range entries {
		bytesTotal += len(raw)
		var entry struct {
			ID string `json:"id"`
		}
		if !binding.run.resume && bytesTotal > 128*1024 || json.Unmarshal(raw, &entry) != nil {
			return reject("unavailable", "native readiness history exceeds its bound")
		}
		if request.Leaf != nil && entry.ID != "" && entry.ID == *request.Leaf {
			foundLeaf = true
		}
	}
	if !foundLeaf {
		return reject("unavailable", "native readiness leaf was not observed")
	}
	if !binding.run.resume {
		if _, err := os.Lstat(request.SessionFile); errors.Is(err, os.ErrNotExist) {
			entries := make([]json.RawMessage, len(request.Entries))
			for index, entry := range request.Entries {
				entries[index] = bytes.Clone(entry)
			}
			fresh = &memberStartingLog{path: request.SessionFile, entries: entries}
		} else if err != nil {
			return err
		} else {
			pin, err = pinMemberLog(ctx, owned.SessionDirectory, request.SessionFile, owned.Cwd, binding.member.ID)
			if err != nil {
				return err
			}
			snapshot, err := readMemberLog(ctx, pin)
			if err != nil || len(snapshot.entries) != len(request.Entries) || memberEntriesDigest(snapshot.entries) != memberEntriesDigest(request.Entries) {
				return reject("unavailable", "native readiness does not match its session history")
			}
		}
	}
	o := queue.owner
	o.tree.mu.Lock()
	if !o.memberBindingCurrentLocked(binding) || binding.ready || binding.run.ctx.Err() != nil || binding.record.paused {
		o.tree.mu.Unlock()
		return reject("unavailable", "native execution cannot accept readiness")
	}
	after := ""
	if request.Leaf != nil {
		after = *request.Leaf
	}
	binding.record.pendingInputs = []pendingInput{{Text: binding.run.prompt, After: &after}}
	o.tree.dirty[binding.record.id] = binding.record
	o.tree.mu.Unlock()
	if err := o.tree.notify(); err != nil {
		return err
	}
	if err := queue.valid(); err != nil {
		return err
	}
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	if !o.memberBindingCurrentLocked(binding) || binding.ready || binding.run.ctx.Err() != nil || binding.record.paused {
		return reject("unavailable", "native execution changed during readiness")
	}
	queue.mu.Lock()
	select {
	case <-queue.closed:
		queue.mu.Unlock()
		return reject("unavailable", "native control closed during readiness")
	case <-queue.cancelled:
		queue.mu.Unlock()
		return reject("cancelled", "native control cancelled during readiness")
	default:
	}
	binding.log, binding.startingLog, binding.ready = pin, fresh, true
	queue.mu.Unlock()
	go queue.deliverInputs()
	if binding.run.nativeFrozen {
		binding.run.nativeFrozen = false
		close(binding.run.nativeThaw)
		binding.run.nativeThaw = nil
	}
	select {
	case <-binding.run.ready:
	default:
		close(binding.run.ready)
	}
	return nil
}

func (o *owner) ensureMemberLog(ctx context.Context, binding *memberBinding) error {
	binding.pinMu.Lock()
	defer binding.pinMu.Unlock()
	o.tree.mu.Lock()
	if !o.memberBindingCurrentLocked(binding) || !binding.ready {
		o.tree.mu.Unlock()
		return reject("unavailable", "native member is not ready")
	}
	if binding.log.file != nil {
		o.tree.mu.Unlock()
		return nil
	}
	starting := binding.startingLog
	o.tree.mu.Unlock()
	if starting == nil {
		return reject("unavailable", "native member has no starting history")
	}
	pin, err := pinMemberLog(ctx, binding.owned.SessionDirectory, starting.path, binding.owned.Cwd, binding.member.ID)
	if err != nil {
		return err
	}
	snapshot, err := readMemberLog(ctx, pin)
	if err != nil || len(snapshot.entries) < len(starting.entries) || memberEntriesDigest(snapshot.entries[:len(starting.entries)]) != memberEntriesDigest(starting.entries) {
		return reject("unavailable", "native session does not retain its starting history")
	}
	pin.baselineCount = len(starting.entries)
	pin.baselineHash = memberEntriesDigest(snapshot.entries[:pin.baselineCount])
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	if !o.memberBindingCurrentLocked(binding) || !binding.ready || binding.startingLog != starting || binding.log.file != nil {
		return reject("unavailable", "native execution changed while pinning its history")
	}
	binding.log, binding.startingLog = pin, nil
	return nil
}
