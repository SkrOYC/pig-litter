package piglitter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/coding"
)

func TestRestoredNativeOwnershipFencesResumeAndSessionWriter(t *testing.T) {
	for _, backend := range []backendRequest{backendTmux, backendITerm} {
		for _, allocated := range []bool{false, true} {
			name := "intent"
			if allocated {
				name = "pane"
			}
			t.Run(string(backend)+"/"+name, func(t *testing.T) {
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
				if backend == backendITerm {
					ownership.Backend = backendITerm
					ownership.Server = memberServerIdentity{Socket: "/Applications/iTerm.app/Contents/MacOS/iTerm2", PID: 1, Birth: "1", Target: "1740D83C-9978-425A-9FBF-C81703D90C94", Window: "12", TTY: "/dev/ttys001"}
					if ownership.Pane != nil {
						ownership.Pane.ID = "327BCBA7-FAD2-41C5-BFAB-E374A0931175"
						ownership.Pane.TTY = "/dev/ttys002"
					}
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
					if err != nil || inspection.Backend == nil || inspection.Backend.Kind != string(backend) || inspection.Backend.Ownership != "cleanup_failed" || !strings.Contains(inspection.Backend.Error, "requires reconciliation") {
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
}

func TestReconciledProvisionalNativeSessionSurvivesReopen(t *testing.T) {
	store, agentDir, rootID, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "member", workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	launch := "0123456789abcdef0123456789abcdef"
	if err := store.markNativeLaunch(member, launch); err != nil {
		t.Fatal(err)
	}
	session := &memberSession{File: filepath.Join(agentDir, "session_"+string(member.Ref.ID)+".jsonl"), Model: "fixture/fixed", Tools: []string{"read"}, Launch: launch}
	saved := savedChild{RootID: rootID, ID: member.Ref.ID, ParentID: rootID, Name: member.Name, Kind: "worker", Model: session.Model, Cwd: workspace.Cwd, Admitted: true, State: Unavailable, NoticeID: "execution", Team: &member, Backend: backendTmux, NativeSession: session}
	host := &runtimeHost{entries: []map[string]any{{"customType": childEntryType, "data": saved}}}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTeamStore(agentDir, rootID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore, o.agentDir, o.cwd = reopened, agentDir, workspace.Cwd
		if err := o.restoreChildren(); err != nil {
			t.Fatal(err)
		}
		tree.mu.Lock()
		restored := tree.records[member.Ref.ID]
		tree.mu.Unlock()
		if restored == nil || restored.team == nil || !restored.paused || restored.nativeOwnership != nil || !reflect.DeepEqual(restored.nativeSession, session) || !restored.team.PlanRequired {
			t.Fatalf("reconciled provisional session = %#v", restored)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
}

func TestProvisionalNativeOwnershipFencesDifferentLaunch(t *testing.T) {
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
	other := savedChild{RootID: rootID, ID: storeTestUUID(4), ParentID: rootID, Name: "other", Kind: "worker", Model: "fixture/fixed", Cwd: workspace.Cwd, Admitted: true, State: Completed, NoticeID: "other-execution"}
	host := &runtimeHost{entries: []map[string]any{{"customType": childEntryType, "data": saved}, {"customType": childEntryType, "data": other}}}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openTeamStore(agentDir, rootID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore, o.agentDir, o.cwd = store, agentDir, workspace.Cwd
		if err := o.restoreChildren(); err != nil {
			t.Fatalf("mismatched native launch blocked recovery: %v", err)
		}
		tree.mu.Lock()
		restored, unaffected := tree.records[member.Ref.ID], tree.records[other.ID]
		tree.mu.Unlock()
		if restored == nil || unaffected == nil || !restored.paused || restored.nativeOwnership == nil || restored.nativeOwnership.Launch != launch || restored.latest.State != Unavailable {
			t.Fatalf("mismatched recovery lost a child or ownership: %#v, %#v", restored, unaffected)
		}
		if _, err := o.prepareNativeResume(context.Background(), restored, restored.run); err == nil || !strings.Contains(err.Error(), "reservation") {
			t.Fatalf("mismatched native launch resumed: %v", err)
		}
		if _, _, err := tree.resume(nil, unaffected.run, "continue"); err != nil {
			t.Fatalf("unaffected child cannot resume: %v", err)
		}
		if _, err := os.Stat(workspace.Cwd); err != nil {
			t.Fatalf("mismatched recovery removed its workspace: %v", err)
		}
		pending, err := store.Reservations()
		if err != nil || len(pending) != 1 || pending[0] != member {
			t.Fatalf("mismatched recovery changed the reservation: %#v, %v", pending, err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
}

func TestProvisionalNativeRecoveryUsesLatestSnapshot(t *testing.T) {
	store, agentDir, rootID, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "member", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	oldLaunch, newLaunch := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	if err := store.markNativeLaunch(member, oldLaunch); err != nil {
		t.Fatal(err)
	}
	if err := store.markNativeLaunch(member, newLaunch, oldLaunch); err != nil {
		t.Fatal(err)
	}
	session := &memberSession{File: filepath.Join(agentDir, "session_"+string(member.Ref.ID)+".jsonl"), Model: "fixture/fixed", Tools: []string{"read"}, Launch: oldLaunch}
	saved := savedChild{RootID: rootID, ID: member.Ref.ID, ParentID: rootID, Name: member.Name, Kind: "worker", Model: session.Model, Cwd: workspace.Cwd, Admitted: true, State: Unavailable, NoticeID: "execution", Team: &member, Backend: backendTmux, NativeSession: session}
	newSaved := saved
	newSaved.NativeSession = cloneMemberSession(session)
	newSaved.NativeSession.Launch = newLaunch
	host := &runtimeHost{entries: []map[string]any{{"customType": childEntryType, "data": saved}, {"customType": childEntryType, "data": newSaved}}}
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore, o.agentDir, o.cwd = store, agentDir, workspace.Cwd
		if err := o.restoreChildren(); err != nil {
			t.Fatalf("historical launch blocked latest snapshot: %v", err)
		}
		tree.mu.Lock()
		restored := tree.records[member.Ref.ID]
		tree.mu.Unlock()
		if restored == nil || restored.nativeSession == nil || restored.nativeSession.Launch != newLaunch {
			t.Fatalf("restored historical session: %#v", restored)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
}
