package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

type recordingMemberControl struct {
	session ChildID
	queue   func(memberInput) (memberInputDisposition, error)
	audit   func(completionAudit) error
}

func (c *recordingMemberControl) Queue(_ context.Context, input memberInput) (memberInputDisposition, error) {
	return c.queue(input)
}
func (c *recordingMemberControl) Snapshot(context.Context, memberHistoryBound) (memberSnapshot, error) {
	return memberSnapshot{session: c.session, phase: memberStreaming}, nil
}
func (c *recordingMemberControl) RecordCompletion(_ context.Context, audit completionAudit) error {
	return c.audit(audit)
}
func (*recordingMemberControl) Cancel()                     {}
func (*recordingMemberControl) Focus(context.Context) error { return nil }

func appendNativeFixture(t *testing.T, path string, entry any) {
	t.Helper()
	data, err := json.Marshal(entry)
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.Write(append(data, '\n'))
	if closeErr := file.Close(); err != nil || closeErr != nil {
		t.Fatal(err, closeErr)
	}
}

func nativeCompletionFixture(t *testing.T, exercise func(*owner, *memberBinding, *run, string, *runtimeHost)) {
	t.Helper()
	rootID := ChildID("018f0000-0000-7000-8000-000000000001")
	memberID := ChildID("018f0000-0000-7000-8000-000000000002")
	cwd, directory := t.TempDir(), t.TempDir()
	path := filepath.Join(directory, "2026-11-24_"+string(memberID)+".jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "id": memberID, "cwd": cwd})
	if err := os.WriteFile(path, append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	appendNativeFixture(t, path, map[string]any{"type": "message", "id": "leaf-before-wait", "message": map[string]any{"role": "user", "content": "start"}})
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	member := teamMember{Ref: MemberRef{Team: TeamID{Root: rootID, Epoch: 1}, ID: memberID, Generation: 1}, AdmissionID: "admission", Workspace: WorkspacePolicy{Kind: "shared_checkout", Cwd: cwd}}
	worker, _ := agentDefaults("worker")
	_, parent, err := tree.admit(admission{kind: "worker", name: "member", model: "fixture/fixed", cwd: cwd, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	parent.calls = newCallScope()
	scout, _ := agentDefaults("scout")
	_, child, err := tree.admit(admission{parentRun: parent, kind: "scout", name: "child", cwd: cwd, task: "inspect", agent: scout})
	if err != nil {
		t.Fatal(err)
	}
	tree.finish(child.owner, child, Outcome{State: Completed, Text: "child report"})
	outcome, err := tree.wait(context.Background(), child, 1000)
	if err != nil || outcome.State != Completed || outcome.Text != "child report" {
		t.Fatalf("child wait = %#v, %v", outcome, err)
	}
	content, err := completionContent(outcome)
	if err != nil || !tree.startCompletion(child, content) {
		t.Fatal("could not reserve child completion", err)
	}
	host := &runtimeHost{}
	withCompletionHost(t, tree, func(o *owner) {
		control := &recordingMemberControl{session: memberID}
		binding := bindMemberFixture(t, o, parent.owner, parent, directory, path, control)
		exercise(o, binding, child, path, host)
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
}

func persistedNativeAttempt(host *runtimeHost, audit bool) bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	for _, entry := range host.entries {
		data, _ := json.Marshal(entry["data"])
		var child savedChild
		if json.Unmarshal(data, &child) != nil {
			continue
		}
		for _, notice := range child.Notices {
			if audit && notice.AuditAttempted || !audit && notice.NativeAttempted {
				return true
			}
		}
	}
	return false
}

func nativeWaitHandback(t *testing.T, o *owner, binding *memberBinding, child *run, path string) {
	t.Helper()
	ticket, err := o.memberCallTicket(binding, "wait-call", "litter_wait", "leaf-before-wait")
	if err != nil {
		t.Fatal(err)
	}
	o.beginOutcomeAck(ticket, child, "litter_wait", nil)
	o.readyOutcomeAck(ticket, true)
	outcome := *child.outcome
	result := map[string]any{"state": outcome.State, "outcome": &outcome}
	content, _ := resultContent("litter_wait", result, nil)
	o.finishCall(ticket, nativeResult("litter_wait", result, nil), false)
	appendNativeFixture(t, path, map[string]any{"type": "message", "id": "wait-result", "message": map[string]any{"role": "toolResult", "toolCallId": "wait-call", "toolName": "litter_wait", "content": []any{map[string]any{"type": "text", "text": content}}}})
}

func TestNativeChildWaitAuditUsesCapturedLeafAndOneWriter(t *testing.T) {
	nativeCompletionFixture(t, func(o *owner, binding *memberBinding, child *run, path string, host *runtimeHost) {
		nativeWaitHandback(t, o, binding, child, path)
		writes := 0
		control := binding.control.(*recordingMemberControl)
		control.audit = func(audit completionAudit) error {
			if !persistedNativeAttempt(host, true) {
				t.Fatal("audit ran before its attempt was stored")
			}
			writes++
			appendNativeFixture(t, path, map[string]any{"type": "custom", "id": "audit", "customType": "litter_completion", "data": audit})
			return errors.New("reply lost after append")
		}
		if err := o.auditCompletion(child); err != nil {
			t.Fatalf("observed audit after lost reply: %v", err)
		}
		if writes != 1 {
			t.Fatalf("native audit writes = %d", writes)
		}
		entries, err := o.memberLogEntries(context.Background(), binding)
		if err != nil || !completionStored(entries, completionDetails{ChildID: child.ref.ID, Receipt: child.noticeID}, child.reservation.content) {
			t.Fatalf("native audit was not correlated with the wait result: %v", err)
		}
		if got := child.reservation.handback; got == nil || got.After == nil || *got.After != "leaf-before-wait" {
			t.Fatalf("wait handback leaf = %#v", got)
		}
	})
}

func TestNativeAuditDoesNotRetryAnUncertainAppend(t *testing.T) {
	nativeCompletionFixture(t, func(o *owner, binding *memberBinding, child *run, path string, host *runtimeHost) {
		nativeWaitHandback(t, o, binding, child, path)
		control := binding.control.(*recordingMemberControl)
		writes := 0
		var attempted completionAudit
		control.audit = func(audit completionAudit) error {
			if !persistedNativeAttempt(host, true) {
				t.Fatal("audit ran before its attempt was stored")
			}
			writes++
			attempted = audit
			return errors.New("reply lost")
		}
		for range 2 {
			if err := o.auditCompletion(child); err == nil {
				t.Fatal("uncertain native audit reported success")
			}
		}
		if writes != 1 {
			t.Fatalf("uncertain audit was repeated %d times", writes)
		}
		appendNativeFixture(t, path, map[string]any{"type": "custom", "id": "delayed-audit", "customType": "litter_completion", "data": attempted})
		if err := o.auditCompletion(child); err != nil || writes != 1 {
			t.Fatalf("observed native audit was not reconciled: writes=%d err=%v", writes, err)
		}
	})
}

func TestNativeCompletionDoesNotRetryAnUncertainSend(t *testing.T) {
	nativeCompletionFixture(t, func(o *owner, binding *memberBinding, child *run, path string, host *runtimeHost) {
		control := binding.control.(*recordingMemberControl)
		writes := 0
		control.queue = func(memberInput) (memberInputDisposition, error) {
			if !persistedNativeAttempt(host, false) {
				t.Fatal("completion ran before its attempt was stored")
			}
			writes++
			return memberInputUncertain, errors.New("reply lost")
		}
		details := completionDetails{ChildID: child.ref.ID, Receipt: child.noticeID}
		content := child.reservation.content
		for range 2 {
			if err := o.deliverRetained(binding.record, details, content); err == nil {
				t.Fatal("uncertain native send reported success")
			}
		}
		if writes != 1 {
			t.Fatalf("uncertain send was repeated %d times", writes)
		}
		appendNativeFixture(t, path, map[string]any{"type": "message", "id": "delivered", "message": map[string]any{"role": "custom", "customType": "litter_completion", "content": content, "details": details}})
		if err := o.deliverRetained(binding.record, details, content); err != nil || writes != 1 {
			t.Fatalf("observed native send was not reconciled: writes=%d err=%v", writes, err)
		}
	})
}
