package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestTeamResumeRetainsIdentityAndPause(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "paused"}[paused], func(t *testing.T) {
			store, _, root, workspace := storeTestOpen(t)
			member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "sender", workspace)
			if err != nil {
				t.Fatal(err)
			}
			member, err = store.Commit(member.AdmissionID)
			if err != nil {
				t.Fatal(err)
			}
			tree := testTree(t, defaultConfig())
			tree.root.id = root
			worker := AgentDefinition{Role: "worker", Tools: []string{"read", "write"}, CanDelegate: true}
			_, previous, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: worker, member: &member})
			if err != nil {
				t.Fatal(err)
			}
			var exerciseErr error
			withCompletionHost(t, tree, func(o *owner) {
				o.teamStore = store
				tree.teamFinished, tree.teamPaused, tree.teamResuming = o.finishTeamMember, o.pauseTeamMember, o.resumeTeamMember
				tree.finish(previous.owner, previous, Outcome{State: Completed, Text: "before resume"})
				if paused {
					if _, _, err := tree.stop(nil, previous); err != nil {
						exerciseErr = err
						return
					}
				}
				_, before, err := store.Snapshot()
				if err != nil || len(before) != 1 {
					exerciseErr = fmt.Errorf("initial roster: %#v, %v", before, err)
					return
				}
				started, release := make(chan struct{}), make(chan struct{})
				defer close(release)
				tree.execute = func(_ *record, _ *run) Outcome {
					close(started)
					<-release
					return Outcome{State: Completed, Text: "resumed"}
				}
				snapshot, selected, err := tree.resume(nil, previous, "continue")
				if err != nil || snapshot.ID != member.Ref.ID || selected == previous || selected.owner.team.Ref != member.Ref {
					exerciseErr = fmt.Errorf("resume reservation: %#v, %v", snapshot, err)
					return
				}
				_, pending, err := store.Snapshot()
				if err != nil || len(pending) != 1 || pending[0].State != before[0].State {
					exerciseErr = fmt.Errorf("unacknowledged resume changed the roster: %#v, %v", pending, err)
					return
				}
				if err := tree.activate(selected); err != nil {
					exerciseErr = err
					return
				}
				select {
				case <-started:
				case <-time.After(5 * time.Second):
					exerciseErr = fmt.Errorf("resumed teammate did not start")
					return
				}
				_, running, err := store.Snapshot()
				if err != nil || len(running) != 1 || running[0].State != "running" || running[0].Ref != member.Ref {
					exerciseErr = fmt.Errorf("resumed roster: %#v, %v", running, err)
				}
			}, func(method string, _ json.RawMessage) json.RawMessage {
				if method == "sessionRead" {
					return json.RawMessage(`"fixture"`)
				}
				t.Errorf("unexpected host method %s", method)
				return nil
			})
			if exerciseErr != nil {
				t.Fatal(exerciseErr)
			}
		})
	}
}

func TestTeamResumeChecksIdentityAndRetainedState(t *testing.T) {
	store, _, _, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "sender", workspace)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PauseMember(member.Ref); err != nil {
		t.Fatal(err)
	}
	for name, change := range map[string]func(*teamMember){
		"reference": func(m *teamMember) { m.Ref.Generation++ },
		"admission": func(m *teamMember) { m.AdmissionID = string(storeTestUUID(4)) },
		"name":      func(m *teamMember) { m.Name = "other" },
		"workspace": func(m *teamMember) { m.Workspace.Cwd = "/other" },
	} {
		t.Run(name, func(t *testing.T) {
			changed := member
			change(&changed)
			if err := store.ResumeMember(changed, true); err == nil {
				t.Fatal("changed teammate identity was accepted")
			}
		})
	}
	if err := store.ResumeMember(member, false); err == nil {
		t.Fatal("resume ignored the retained pause")
	}
	_, paused, err := store.Snapshot()
	if err != nil || len(paused) != 1 || paused[0].State != "paused" || paused[0].Ref != member.Ref {
		t.Fatalf("rejected resumes changed the roster: %#v, %v", paused, err)
	}
	if err := store.ResumeMember(member, true); err != nil {
		t.Fatal(err)
	}
	_, running, err := store.Snapshot()
	if err != nil || len(running) != 1 || running[0].State != "running" || running[0].Ref != member.Ref {
		t.Fatalf("resumed roster: %#v, %v", running, err)
	}
}

func TestTeamResumeRequiresParentAndSettlement(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "sender", workspace)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	worker := AgentDefinition{Role: "worker", Tools: []string{"read"}, CanDelegate: true}
	_, teammate, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	_, outsider, err := tree.admit(admission{kind: "worker", name: "outsider", cwd: t.TempDir(), task: "other", agent: worker})
	if err != nil {
		t.Fatal(err)
	}
	tree.finish(teammate.owner, teammate, Outcome{State: Completed})
	if _, _, err := tree.resume(outsider, teammate, "resume someone else"); err == nil {
		t.Fatal("another child resumed a teammate")
	}
	if _, _, err := tree.resume(nil, teammate, "resume before settlement"); err == nil {
		t.Fatal("resume passed an unfinished roster settlement")
	}
	teammate.teamFinishErr = errors.New("roster write failed")
	close(teammate.teamFinishDone)
	if _, _, err := tree.resume(nil, teammate, "resume after failed settlement"); err == nil {
		t.Fatal("resume passed a failed roster settlement")
	}
	teammate.teamFinishErr = nil
	_, selected, err := tree.resume(nil, teammate, "continue")
	if err != nil {
		t.Fatal(err)
	}
	tree.cancelPending(selected)
	_, members, err := store.Snapshot()
	if err != nil || len(members) != 1 || members[0].Ref != member.Ref {
		t.Fatalf("cancelled resume removed the teammate: %#v, %v", members, err)
	}
	if _, err := tree.wait(context.Background(), teammate, 1000); err != nil {
		t.Fatal(err)
	}
}

func TestTeamResumeChecksOwnerAndIsolatedWorkspace(t *testing.T) {
	store, _, root, _ := storeTestOpen(t)
	repository := t.TempDir()
	git := func(directory string, args ...string) error {
		output, err := exec.Command("git", append([]string{"-C", directory}, args...)...).CombinedOutput()
		if err != nil {
			return fmt.Errorf("git %v: %s: %w", args, output, err)
		}
		return nil
	}
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.name", "Fixture"}, {"config", "user.email", "fixture@example.invalid"}} {
		if err := git(repository, args...); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(repository, "fixture.txt"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "fixture.txt"}, {"commit", "-qm", "fixture"}} {
		if err := git(repository, args...); err != nil {
			t.Fatal(err)
		}
	}
	id := storeTestUUID(2)
	workspace, err := teamWorktreeIntent(context.Background(), store, repository, id)
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.Reserve(id, string(storeTestUUID(3)), "sender", workspace)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareTeamWorktree(context.Background(), workspace); err != nil {
		t.Fatal(err)
	}
	member, err = store.SetWorkspacePhase(member.Ref, "prepared")
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(member.Ref, "idle"); err != nil {
		t.Fatal(err)
	}
	if err := git(workspace.Cwd, "switch", "--detach", "-q"); err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	selected := tree.newRunLocked(Ref{id}, "continue")
	defer selected.cancel()
	r := &record{id: id, team: &member}
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		o.state.owner = nil
		if err := o.resumeTeamMember(r, selected); err == nil {
			exerciseErr = errors.New("retired owner resumed a teammate")
		}
		o.state.owner = o
		if exerciseErr == nil {
			if err := o.resumeTeamMember(r, selected); err == nil {
				exerciseErr = errors.New("detached isolated workspace was accepted")
			}
		}
		_, roster, err := store.Snapshot()
		if exerciseErr == nil && (err != nil || len(roster) != 1 || roster[0].State != "idle") {
			exerciseErr = fmt.Errorf("rejected resume changed the roster: %#v, %v", roster, err)
		}
		if exerciseErr != nil {
			return
		}
		if err := git(workspace.Cwd, "switch", "-q", workspace.Branch); err != nil {
			exerciseErr = err
			return
		}
		if err := o.resumeTeamMember(r, selected); err != nil {
			exerciseErr = err
			return
		}
		_, roster, err = store.Snapshot()
		if err != nil || len(roster) != 1 || roster[0].State != "running" || roster[0].Ref != member.Ref {
			exerciseErr = fmt.Errorf("valid isolated resume: %#v, %v", roster, err)
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			return json.RawMessage(`"fixture"`)
		}
		t.Errorf("unexpected host method %s", method)
		return nil
	})
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func TestTeamResumeAllowsInterruptedAndUnavailableRecovery(t *testing.T) {
	for _, state := range []string{"interrupted", "unavailable"} {
		t.Run(state, func(t *testing.T) {
			store, _, _, workspace := storeTestOpen(t)
			member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "sender", workspace)
			if err != nil {
				t.Fatal(err)
			}
			member, err = store.Commit(member.AdmissionID)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.SetState(member.Ref, state); err != nil {
				t.Fatal(err)
			}
			if err := store.ResumeMember(member, true); err != nil {
				t.Fatal(err)
			}
			_, roster, err := store.Snapshot()
			if err != nil || len(roster) != 1 || roster[0].Ref != member.Ref || roster[0].State != "running" {
				t.Fatalf("recovered roster: %#v, %v", roster, err)
			}
		})
	}
}

func TestTeamResumeSerializesPauseWithActivation(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "sender", workspace)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	worker := AgentDefinition{Role: "worker", Tools: []string{"read"}, CanDelegate: true}
	_, previous, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		tree.teamFinished, tree.teamPaused = o.finishTeamMember, o.pauseTeamMember
		tree.finish(previous.owner, previous, Outcome{State: Completed})
		_, selected, err := tree.resume(nil, previous, "continue")
		if err != nil {
			exerciseErr = err
			return
		}
		entered, release := make(chan struct{}), make(chan struct{})
		tree.teamResuming = func(r *record, selected *run) error {
			close(entered)
			<-release
			return o.resumeTeamMember(r, selected)
		}
		tree.execute = func(_ *record, selected *run) Outcome {
			<-selected.ctx.Done()
			return Outcome{State: Paused}
		}
		activation := make(chan error, 1)
		go func() { activation <- tree.activate(selected) }()
		<-entered
		stopped := make(chan error, 1)
		go func() {
			_, _, err := tree.stop(nil, selected)
			stopped <- err
		}()
		select {
		case err := <-stopped:
			exerciseErr = fmt.Errorf("pause finished before activation: %v", err)
			close(release)
			<-activation
			return
		case <-time.After(50 * time.Millisecond):
		}
		close(release)
		if err := <-activation; err != nil {
			exerciseErr = err
			return
		}
		if err := <-stopped; err != nil {
			exerciseErr = err
			return
		}
		_, roster, err := store.Snapshot()
		if err != nil || len(roster) != 1 || roster[0].Ref != member.Ref || roster[0].State != "paused" {
			exerciseErr = fmt.Errorf("pause after activation: %#v, %v", roster, err)
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			return json.RawMessage(`"fixture"`)
		}
		t.Errorf("unexpected host method %s", method)
		return nil
	})
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}
