//go:build linux

package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestReservedNativeLaunchConflictCanRecoverWithoutTouchingPanes(t *testing.T) {
	ctx := context.Background()
	dir, err := os.MkdirTemp("", "p8-conflict-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "tmux.sock")
	tmux := func(args ...string) string {
		t.Helper()
		out, err := exec.CommandContext(ctx, "tmux", append([]string{"-S", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %v: %s: %v", args, out, err)
		}
		return strings.TrimSpace(string(out))
	}
	tmux("new-session", "-d", "-s", "recovery", "sleep", "600")
	defer exec.Command("tmux", "-S", socket, "kill-server").Run()
	parts := strings.Split(tmux("display-message", "-p", "-t", "recovery:0.0", "#{pid}\t#{pane_id}"), "\t")
	pid, err := strconv.Atoi(parts[0])
	if err != nil || len(parts) != 2 {
		t.Fatalf("tmux identity = %q", parts)
	}
	server, err := tmuxFromEnvironment(ctx, fmt.Sprintf("%s,%d,0", socket, pid), parts[1])
	if err != nil {
		t.Fatal(err)
	}
	unrelated := tmux("split-window", "-d", "-P", "-F", "#{pane_id}", "-t", server.target, "sleep", "600")
	scope, err := memberProcessScope(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	store, agentDir, rootID, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "member", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	oldLaunch, newLaunch := "0123456789abcdef0123456789abcdef", "fedcba9876543210fedcba9876543210"
	if err := store.markNativeLaunch(member, oldLaunch); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(agentDir, "history_"+string(member.Ref.ID)+".jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": member.Ref.ID, "timestamp": "2026-11-24T00:00:00Z", "cwd": workspace.Cwd})
	entry, _ := json.Marshal(map[string]any{"type": "message", "id": "prior", "parentId": nil, "timestamp": "2026-11-24T00:00:01Z", "message": map[string]any{"role": "user", "content": "original task", "timestamp": 1}})
	if err := os.WriteFile(path, append(append(append(header, '\n'), entry...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	session := &memberSession{File: path, Model: "fixture/fixed", Tools: []string{"read"}, Launch: oldLaunch}
	executable := filepath.Join(dir, "member")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexec sleep 600\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	ownership := &memberOwnership{Backend: backendTmux, Member: member.Ref, Admission: member.AdmissionID, Execution: "execution", ProcessScope: scope, Launch: newLaunch, LaunchPhase: "reserved", Cwd: workspace.Cwd, Model: session.Model, Tools: []string{"read"}, SessionDirectory: agentDir, Executable: executable, ExecutableSHA256: strings.Repeat("a", 64), Manifest: "/tmp/member-" + newLaunch + ".json", Server: memberServerIdentity{Socket: server.socket, PID: server.pid, Birth: server.birth, Target: server.target}}
	matching := tmux("split-window", "-d", "-P", "-F", "#{pane_id}", "-t", server.target, "--", executable, "--pig-litter-member", ownership.Manifest)
	matchingPID, err := strconv.Atoi(tmux("display-message", "-p", "-t", matching, "#{pane_pid}"))
	if err != nil {
		t.Fatal(err)
	}
	matchingBirth, err := processBirth(matchingPID)
	if err != nil {
		t.Fatal(err)
	}
	if err := confirmReservedMemberUnstarted(ctx, ownership); err == nil {
		t.Fatal("accepted an existing matching pane as unstarted")
	}
	if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, matching) || !strings.Contains(panes, unrelated) {
		t.Fatalf("reconciliation changed live panes: %s", panes)
	}
	tmux("kill-pane", "-t", matching)
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		if _, err := processBirth(matchingPID); os.IsNotExist(err) {
			break
		}
		select {
		case <-time.After(10 * time.Millisecond):
		case <-deadline.C:
			t.Fatal("fixture member process did not exit")
		}
	}
	for _, change := range []func(*memberOwnership){
		func(value *memberOwnership) { value.LaunchPhase = "" },
		func(value *memberOwnership) { value.ProcessScope = "" },
		func(value *memberOwnership) { value.ProcessScope = "changed" },
		func(value *memberOwnership) { value.Server.Birth = "changed" },
	} {
		candidate := cloneMemberOwnership(ownership)
		change(candidate)
		if err := confirmReservedMemberUnstarted(ctx, candidate); err == nil {
			t.Fatalf("accepted unknown or changed ownership: %#v", candidate)
		}
	}
	saved := savedChild{RootID: rootID, ID: member.Ref.ID, ParentID: rootID, Name: member.Name, Kind: "worker", Model: session.Model, Cwd: workspace.Cwd, Tools: []string{"read"}, Admitted: true, State: Unavailable, NoticeID: "execution", Team: &member, Backend: backendTmux, Native: ownership, NativeSession: session}
	host := &runtimeHost{tools: []string{"read"}, entries: []map[string]any{{"customType": childEntryType, "data": saved}}}
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
			t.Fatal(err)
		}
		tree.mu.Lock()
		restored := tree.records[member.Ref.ID]
		tree.mu.Unlock()
		if restored == nil || !restored.nativeReservationConflict || !restored.paused {
			t.Fatalf("interrupted member = %#v", restored)
		}
		tree.mu.Lock()
		restored.nativeSession.Launch = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		tree.mu.Unlock()
		if _, err := o.prepareNativeResume(ctx, restored, restored.run); err == nil {
			t.Fatal("accepted a changed reservation identity")
		}
		tree.mu.Lock()
		restored.nativeSession.Launch = oldLaunch
		tree.mu.Unlock()
		pin, err := o.prepareNativeResume(ctx, restored, restored.run)
		if err != nil || pin == nil || pin.log.path != path {
			t.Fatalf("reconciled history = %#v, %v", pin, err)
		}
		if restored.nativeOwnership != nil || restored.nativeReservationConflict || restored.nativeSession.Launch != oldLaunch {
			t.Fatalf("reconciled identity = %#v", restored)
		}
		if _, _, err := tree.resume(nil, restored.run, "continue"); err != nil {
			t.Fatalf("explicit resume remained fenced: %v", err)
		}
		_, reservations, err := store.recoverySnapshot()
		if err != nil || len(reservations) != 1 || reservations[0].NativeLaunch != oldLaunch {
			t.Fatalf("reservation after reconciliation = %#v, %v", reservations, err)
		}
		panes, err := server.panes(ctx)
		if err != nil || !slices.ContainsFunc(panes, func(pane tmuxPaneState) bool { return pane.id == unrelated }) {
			t.Fatalf("unrelated pane changed: %#v, %v", panes, err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })

	second, err := store.Reserve(storeTestUUID(4), string(storeTestUUID(5)), "second", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.markNativeLaunch(second, oldLaunch); err != nil {
		t.Fatal(err)
	}
	secondPath := filepath.Join(agentDir, "history_"+string(second.Ref.ID)+".jsonl")
	secondHeader, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": second.Ref.ID, "timestamp": "2026-11-24T00:00:00Z", "cwd": workspace.Cwd})
	if err := os.WriteFile(secondPath, append(append(append(secondHeader, '\n'), entry...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	secondSession := &memberSession{File: secondPath, Model: session.Model, Tools: []string{"read"}, Launch: oldLaunch}
	secondOwnership := cloneMemberOwnership(ownership)
	secondOwnership.Member, secondOwnership.Admission, secondOwnership.LaunchPhase = second.Ref, second.AdmissionID, ""
	secondOwnership.Pane = &memberPaneIdentity{ID: matching, PID: matchingPID, Birth: matchingBirth, Command: executable + " --pig-litter-member " + ownership.Manifest}
	secondSaved := savedChild{RootID: rootID, ID: second.Ref.ID, ParentID: rootID, Name: second.Name, Kind: "worker", Model: session.Model, Cwd: workspace.Cwd, Tools: []string{"read"}, Admitted: true, State: Unavailable, NoticeID: "execution", Team: &second, Backend: backendTmux, Native: secondOwnership, NativeSession: secondSession}
	secondHost := &runtimeHost{tools: []string{"read"}, entries: []map[string]any{{"customType": childEntryType, "data": secondSaved}}}
	secondTree := testTree(t, defaultConfig())
	secondTree.root.id = rootID
	withCompletionHost(t, secondTree, func(o *owner) {
		o.teamStore, o.agentDir, o.cwd = store, agentDir, workspace.Cwd
		if err := o.restoreChildren(); err != nil {
			t.Fatal(err)
		}
		restored := secondTree.records[second.Ref.ID]
		if restored == nil || !restored.nativeReservationConflict {
			t.Fatalf("dispatched conflict was not retained: %#v", restored)
		}
		if _, err := o.prepareNativeResume(ctx, restored, restored.run); err != nil {
			t.Fatalf("dead dispatched member was not reconciled: %v", err)
		}
		_, reservations, err := store.recoverySnapshot()
		if err != nil || !slices.ContainsFunc(reservations, func(value teamReservation) bool {
			return value.Member.Ref == second.Ref && value.NativeLaunch == newLaunch
		}) || restored.nativeOwnership != nil || restored.nativeSession.Launch != newLaunch {
			t.Fatalf("dispatched reconciliation = %#v, %#v, %v", restored, reservations, err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage { return secondHost.call(t, method, raw) })

	for _, test := range []struct {
		name, marker, phase, wantLaunch string
		committed, wantResume, live     bool
	}{
		{name: "provisional matching reserved", marker: newLaunch, phase: "reserved", wantResume: true, wantLaunch: newLaunch},
		{name: "provisional matching live pane", marker: newLaunch, phase: "reserved", live: true},
		{name: "provisional mismatched reserved", marker: oldLaunch, phase: "reserved", wantResume: true, wantLaunch: oldLaunch},
		{name: "committed reserved", committed: true, phase: "reserved", wantResume: true, wantLaunch: oldLaunch},
		{name: "provisional unrelated marker", marker: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", phase: "reserved"},
		{name: "provisional matching armed", marker: newLaunch, phase: "armed"},
		{name: "provisional mismatched armed", marker: oldLaunch, phase: "armed"},
		{name: "committed armed", committed: true, phase: "armed"},
		{name: "provisional matching legacy", marker: newLaunch},
		{name: "provisional mismatched legacy", marker: oldLaunch},
		{name: "committed legacy", committed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			matrixStore, matrixAgent, matrixRoot, matrixWorkspace := storeTestOpen(t)
			matrixMember, err := matrixStore.Reserve(storeTestUUID(20), string(storeTestUUID(21)), "matrix", matrixWorkspace, false)
			if err != nil {
				t.Fatal(err)
			}
			if test.committed {
				matrixMember, err = matrixStore.Commit(matrixMember.AdmissionID)
			} else {
				err = matrixStore.markNativeLaunch(matrixMember, test.marker)
			}
			if err != nil {
				t.Fatal(err)
			}
			matrixPath := filepath.Join(matrixAgent, "history_"+string(matrixMember.Ref.ID)+".jsonl")
			matrixHeader, _ := json.Marshal(map[string]any{"type": "session", "version": 3, "id": matrixMember.Ref.ID, "timestamp": "2026-11-24T00:00:00Z", "cwd": matrixWorkspace.Cwd})
			if err := os.WriteFile(matrixPath, append(append(append(matrixHeader, '\n'), entry...), '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			matrixSession := &memberSession{File: matrixPath, Model: "fixture/fixed", Tools: []string{"read"}, Launch: oldLaunch}
			matrixOwnership := cloneMemberOwnership(ownership)
			matrixOwnership.Member, matrixOwnership.Admission = matrixMember.Ref, matrixMember.AdmissionID
			matrixOwnership.Cwd, matrixOwnership.SessionDirectory, matrixOwnership.LaunchPhase = matrixWorkspace.Cwd, matrixAgent, test.phase
			if test.live {
				pane := tmux("split-window", "-d", "-P", "-F", "#{pane_id}", "-t", server.target, "--", matrixOwnership.Executable, "--pig-litter-member", matrixOwnership.Manifest)
				defer tmux("kill-pane", "-t", pane)
				defer func() {
					if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, pane) || !strings.Contains(panes, unrelated) {
						t.Errorf("reconciliation changed live panes: %s", panes)
					}
				}()
			}
			matrixSaved := savedChild{RootID: matrixRoot, ID: matrixMember.Ref.ID, ParentID: matrixRoot, Name: matrixMember.Name, Kind: "worker", Model: matrixSession.Model, Cwd: matrixWorkspace.Cwd, Tools: []string{"read"}, Admitted: true, State: Unavailable, NoticeID: "execution", Team: &matrixMember, Backend: backendTmux, Native: matrixOwnership, NativeSession: matrixSession}
			matrixHost := &runtimeHost{tools: []string{"read"}, entries: []map[string]any{{"customType": childEntryType, "data": matrixSaved}}}
			matrixTree := testTree(t, defaultConfig())
			matrixTree.root.id = matrixRoot
			withCompletionHost(t, matrixTree, func(o *owner) {
				o.teamStore, o.agentDir, o.cwd = matrixStore, matrixAgent, matrixWorkspace.Cwd
				if err := o.restoreChildren(); err != nil {
					t.Errorf("restore failed: %v", err)
					return
				}
				matrixTree.mu.Lock()
				restored := matrixTree.records[matrixMember.Ref.ID]
				matrixTree.mu.Unlock()
				if restored == nil || !restored.paused {
					t.Errorf("restored member = %#v", restored)
					return
				}
				pin, err := o.prepareNativeResume(ctx, restored, restored.run)
				if (err == nil) != test.wantResume {
					t.Errorf("reconcile = %#v, %v; want resume %v", pin, err, test.wantResume)
					return
				}
				if test.wantResume {
					if pin == nil || pin.log.path != matrixPath || restored.nativeOwnership != nil || restored.nativeSession.Launch != test.wantLaunch {
						t.Errorf("reconciled identity = %#v, %#v", restored, pin)
						return
					}
					_, resumed, err := matrixTree.resume(nil, restored.run, "continue")
					if err != nil {
						t.Errorf("explicit resume: %v", err)
						return
					}
					resumed.mu.Lock()
					resumed.nativeResume = pin
					resumed.mu.Unlock()
					if err := o.resumeTeamMember(restored, resumed); err != nil {
						t.Errorf("resume admission lost its session association: %v", err)
					}
				} else if restored.nativeOwnership == nil || restored.nativeSession.Launch != oldLaunch {
					t.Errorf("unproved ownership was released: %#v", restored)
				}
				if _, err := os.Stat(matrixWorkspace.Cwd); err != nil {
					t.Errorf("workspace was removed: %v", err)
				}
			}, func(method string, raw json.RawMessage) json.RawMessage { return matrixHost.call(t, method, raw) })
		})
	}
}
