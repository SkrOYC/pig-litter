package piglitter

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/coding"
)

func TestRestoredNativeOwnershipFencesResumeAndSessionWriter(t *testing.T) {
	for _, allocated := range []bool{false, true} {
		name := "intent"
		if allocated {
			name = "pane"
		}
		t.Run(name, func(t *testing.T) {
			store, agentDir, rootID, workspace := storeTestOpen(t)
			member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "member", workspace, false)
			if err != nil {
				t.Fatal(err)
			}
			member, err = store.Commit(member.AdmissionID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetState(member.Ref, "running"); err != nil {
				t.Fatal(err)
			}
			launch := "0123456789abcdef0123456789abcdef"
			ownership := &memberOwnership{Backend: backendTmux, Member: member.Ref, Admission: member.AdmissionID, Execution: "execution", Launch: launch, Cwd: workspace.Cwd, Model: "fixture/fixed", Tools: []string{"read"}, SessionDirectory: agentDir, Executable: "/fixture", ExecutableSHA256: strings.Repeat("a", 64), Manifest: "/tmp/member-" + launch + ".json", Server: memberServerIdentity{Socket: "/tmp/tmux", PID: 1, Birth: "1", Target: "%0"}}
			if allocated {
				ownership.Pane = &memberPaneIdentity{ID: "%1", PID: 2, Birth: "2", Command: "/fixture --pig-litter-member /tmp/member-" + launch + ".json"}
			}
			saved := savedChild{RootID: rootID, ID: member.Ref.ID, ParentID: rootID, Name: member.Name, Kind: "worker", Model: "fixture/fixed", Cwd: workspace.Cwd, Instructions: "work", Tools: []string{"read"}, Admitted: true, State: Running, NoticeID: "execution", Team: &member, Native: ownership}
			host := &runtimeHost{entries: []map[string]any{{"customType": childEntryType, "data": saved}}}
			tree := testTree(t, defaultConfig())
			tree.root.id = rootID
			withCompletionHost(t, tree, func(o *owner) {
				o.teamStore, o.agentDir, o.cwd = store, agentDir, workspace.Cwd
				if err := o.restoreChildren(); err != nil {
					t.Fatal(err)
				}
				tree.mu.Lock()
				restored := tree.records[member.Ref.ID]
				tree.mu.Unlock()
				if restored == nil || restored.nativeOwnership == nil || restored.native != nil || !restored.paused || restored.latest == nil || restored.latest.State != Unavailable {
					t.Fatalf("restored native member = %#v", restored)
				}
				inspection, err := tree.inspect(member.Ref.ID)
				if err != nil || inspection.Backend == nil || inspection.Backend.Kind != "tmux" || inspection.Backend.Ownership != "cleanup_failed" || !strings.Contains(inspection.Backend.Error, "requires reconciliation") {
					t.Fatalf("restored native cleanup status = %#v, %v", inspection.Backend, err)
				}
				if _, _, err := tree.resume(nil, restored.run, "continue"); err == nil || !strings.Contains(err.Error(), "requires reconciliation") {
					t.Fatalf("native member resumed before reconciliation: %v", err)
				}
				opened := false
				if err := o.withChildLogLocked(restored, func(*coding.SessionManager) error { opened = true; return nil }); err == nil || opened {
					t.Fatalf("opened the native log before reconciliation: %v", err)
				}
				if _, err := o.persistedCompletionEntries(member.Ref.ID); err == nil {
					t.Fatal("read through an unbound native session")
				}
				_, members, err := store.Snapshot()
				if err != nil || len(members) != 1 || members[0].State != "unavailable" {
					t.Fatalf("native roster state = %#v, %v", members, err)
				}
			}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
		})
	}
}

func TestProvisionalNativeOwnershipRejectsDifferentLaunch(t *testing.T) {
	store, agentDir, rootID, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "member", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.markNativeLaunch(member, "0123456789abcdef0123456789abcdef"); err != nil {
		t.Fatal(err)
	}
	launch := "fedcba9876543210fedcba9876543210"
	ownership := &memberOwnership{Backend: backendTmux, Member: member.Ref, Admission: member.AdmissionID, Execution: "execution", Launch: launch, Cwd: workspace.Cwd, Model: "fixture/fixed", Tools: []string{"read"}, SessionDirectory: agentDir, Executable: "/fixture", ExecutableSHA256: strings.Repeat("a", 64), Manifest: "/tmp/member-" + launch + ".json", Server: memberServerIdentity{Socket: "/tmp/tmux", PID: 1, Birth: "1", Target: "%0"}}
	saved := savedChild{RootID: rootID, ID: member.Ref.ID, ParentID: rootID, Name: member.Name, Kind: "worker", Model: "fixture/fixed", Cwd: workspace.Cwd, Admitted: true, State: Unavailable, NoticeID: "execution", Team: &member, Native: ownership}
	host := &runtimeHost{entries: []map[string]any{{"customType": childEntryType, "data": saved}}}
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore, o.agentDir, o.cwd = store, agentDir, workspace.Cwd
		if err := o.restoreChildren(); err == nil || !strings.Contains(err.Error(), "does not match its reservation") {
			t.Fatalf("mismatched native launch recovery = %v", err)
		}
		pending, err := store.Reservations()
		if err != nil || len(pending) != 1 || pending[0] != member {
			t.Fatalf("mismatched recovery changed the reservation: %#v, %v", pending, err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
}
