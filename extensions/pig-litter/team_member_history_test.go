package piglitter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/coding"
)

type testMemberControl struct {
	snapshot memberSnapshot
	cancel   func()
}

func (testMemberControl) Queue(context.Context, memberInput) (memberInputDisposition, error) {
	return memberInputQueued, nil
}
func (control testMemberControl) Snapshot(context.Context, memberHistoryBound) (memberSnapshot, error) {
	return control.snapshot, nil
}
func (testMemberControl) RecordCompletion(context.Context, completionAudit) error { return nil }
func (control testMemberControl) Cancel() {
	if control.cancel != nil {
		control.cancel()
	}
}
func (testMemberControl) Focus(context.Context) error { return nil }

func bindMemberFixture(t *testing.T, o *owner, r *record, selected *run, directory, path string, control memberControl) *memberBinding {
	t.Helper()
	pin, err := pinMemberLog(context.Background(), directory, path, r.cwd, r.id)
	if err != nil {
		t.Fatal(err)
	}
	launch := "0123456789abcdef0123456789abcdef"
	tools := append(slices.Clone(controlNames), teamToolNames...)
	slices.Sort(tools)
	ownership := &memberOwnership{Backend: backendTmux, Member: r.team.Ref, Admission: r.team.AdmissionID, Execution: selected.noticeID, OwnerEpoch: o.epoch, Launch: launch, Cwd: r.cwd, Model: r.model, Tools: tools, SessionDirectory: directory, Executable: "/fixture", ExecutableSHA256: strings.Repeat("0", 64), Manifest: "/tmp/member-" + launch + ".json", Server: memberServerIdentity{Socket: "/tmp/tmux", PID: 1, Birth: "1", Target: "%0"}, Pane: &memberPaneIdentity{ID: "%1", PID: 2, Birth: "2", Command: "/fixture --pig-litter-member /tmp/member-" + launch + ".json"}}
	binding := &memberBinding{record: r, run: selected, member: r.team.Ref, admission: r.team.AdmissionID, execution: selected.noticeID, ownerEpoch: o.epoch, launch: launch, log: pin, ownership: ownership, owned: cloneMemberOwnership(ownership), control: control, ready: true}
	o.tree.mu.Lock()
	r.nativeOwnership, r.native = ownership, binding
	selected.mu.Lock()
	selected.member = control
	selected.mu.Unlock()
	o.tree.mu.Unlock()
	return binding
}

func TestNativeMemberHistoryStaysWithItsOwner(t *testing.T) {
	rootID := ChildID("018f0000-0000-7000-8000-000000000001")
	memberID := ChildID("018f0000-0000-7000-8000-000000000002")
	cwd, directory := t.TempDir(), t.TempDir()
	path := filepath.Join(directory, "2026-11-24_"+string(memberID)+".jsonl")
	header, err := json.Marshal(map[string]any{"type": "session", "id": memberID, "cwd": cwd})
	if err != nil {
		t.Fatal(err)
	}
	first := `{"type":"message","id":"one","message":{"role":"user","content":"hello"}}`
	if err := os.WriteFile(path, []byte(string(header)+"\n"+first+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	member := teamMember{Ref: MemberRef{Team: TeamID{Root: rootID, Epoch: 1}, ID: memberID, Generation: 1}, AdmissionID: "admission", Workspace: WorkspacePolicy{Kind: "shared_checkout", Cwd: cwd}}
	worker, _ := agentDefaults("worker")
	_, selected, err := tree.admit(admission{kind: "worker", name: "member", model: "fixture/fixed", cwd: cwd, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	withCompletionHost(t, tree, func(o *owner) {
		binding := bindMemberFixture(t, o, selected.owner, selected, directory, path, testMemberControl{})
		for _, read := range []func(ChildID) ([]json.RawMessage, error){o.completionEntries, o.persistedCompletionEntries} {
			entries, err := read(memberID)
			if err != nil || len(entries) != 1 || string(entries[0]) != first {
				t.Fatalf("native history = %s, %v", entries, err)
			}
		}
		opened := false
		if err := o.withChildLogLocked(selected.owner, func(*coding.SessionManager) error { opened = true; return nil }); err == nil || opened {
			t.Fatalf("opened a second session writer: %v", err)
		}
		file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		_, err = file.WriteString(`{"type":"message","id":"two","message":{"role":"assistant","content":"done"}}` + "\n")
		if closeErr := file.Close(); err != nil || closeErr != nil {
			t.Fatal(err, closeErr)
		}
		entries, err := o.memberLogEntries(context.Background(), binding)
		if err != nil || len(entries) != 2 || !strings.Contains(string(entries[1]), `"done"`) {
			t.Fatalf("appended native history = %s, %v", entries, err)
		}
		changed := strings.Replace(first, "hello", "other", 1)
		if err := os.WriteFile(path, []byte(string(header)+"\n"+changed+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := o.memberLogEntries(context.Background(), binding); err == nil {
			t.Fatal("native history accepted a changed execution baseline")
		}
		if err := os.WriteFile(path, []byte(string(header)+"\n"+first+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := o.memberCallTicket(binding, o.callScope(binding.run), "foreign-call", "arbitrary_tool", ""); err == nil {
			t.Fatal("native member invoked an unregistered operation")
		}
		o.state.mu.Lock()
		o.state.epoch++
		o.state.mu.Unlock()
		if _, err := o.memberLogEntries(context.Background(), binding); err == nil {
			t.Fatal("retired owner read native history")
		}
		o.state.mu.Lock()
		o.state.epoch--
		o.state.mu.Unlock()
		if err := os.Rename(path, path+".old"); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(string(header)+"\n"+first+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := o.completionEntries(memberID); err == nil {
			t.Fatal("accepted a replacement native session file")
		}
	}, func(string, json.RawMessage) json.RawMessage { return json.RawMessage(`"fixture"`) })
}
