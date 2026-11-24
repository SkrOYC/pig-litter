package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeSettlementCorrelatesBoundaryRevisionAndWake(t *testing.T) {
	store, _, member := planTestMember(t)
	tree := testTree(t, defaultConfig())
	tree.root.id = member.Ref.Team.Root
	worker, _ := agentDefaults("worker")
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, model: "fixture/fixed", cwd: member.Workspace.Cwd, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "session_"+string(member.Ref.ID)+".jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "id": member.Ref.ID, "cwd": member.Workspace.Cwd})
	if err := os.WriteFile(path, append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		binding := bindMemberFixture(t, o, selected.owner, selected, directory, path, testMemberControl{})
		o.policy.TeammateIdle = []string{"/bin/sh", "-c", `printf '{"permit":false,"feedback":"Finish the task."}'`}
		decision, err := o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 1, report: "report", completed: true})
		if err != nil || decision.allow || !decision.continueRun || decision.feedback != "Finish the task." || !strings.Contains(memberSettlementFeedback(decision), "Finish the task.") {
			t.Fatalf("policy refusal = %#v, %v", decision, err)
		}
		commits := 0
		commit := func() error {
			commits++
			if tree.lifecycleMu.TryLock() {
				tree.lifecycleMu.Unlock()
				return errors.New("settlement commit ran without the lifecycle lock")
			}
			return nil
		}
		if _, err := o.finalizeMemberSettlement(binding, 1, false, commit); err == nil || commits != 0 {
			t.Fatalf("a refused boundary committed: commits=%d err=%v", commits, err)
		}
		o.policy.TeammateIdle = nil
		decision, err = o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 2, report: "report", completed: true})
		if err != nil || !decision.allow {
			t.Fatalf("policy approval = %#v, %v", decision, err)
		}
		if _, err := o.finalizeMemberSettlement(binding, 1, false, commit); err == nil {
			t.Fatal("a stale boundary finalized the current decision")
		}
		if _, err := store.createTask("New work", "", nil, nil); err != nil {
			t.Fatal(err)
		}
		decision, err = o.finalizeMemberSettlement(binding, 2, false, commit)
		if err != nil || decision.allow || !strings.Contains(decision.feedback, "changed") || commits != 0 {
			t.Fatalf("changed task revision = %#v, commits=%d, %v", decision, commits, err)
		}
		decision, err = o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 3, report: "report", completed: true})
		if err != nil || !decision.allow {
			t.Fatalf("new policy approval = %#v, %v", decision, err)
		}
		tree.mu.Lock()
		selected.owner.peerWakeVersion++
		tree.mu.Unlock()
		decision, err = o.finalizeMemberSettlement(binding, 3, false, commit)
		if err != nil || decision.allow || commits != 0 {
			t.Fatalf("changed wake = %#v, commits=%d, %v", decision, commits, err)
		}
		decision, err = o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 4, report: "report", completed: true})
		if err != nil || !decision.allow {
			t.Fatalf("final policy approval = %#v, %v", decision, err)
		}
		decision, err = o.finalizeMemberSettlement(binding, 4, false, commit)
		if err != nil || !decision.allow || commits != 1 {
			t.Fatalf("final settlement = %#v, commits=%d, %v", decision, commits, err)
		}
		if _, err := o.finalizeMemberSettlement(binding, 4, false, commit); err == nil || commits != 1 {
			t.Fatalf("replayed settlement committed: commits=%d err=%v", commits, err)
		}
		if _, err := o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 5, report: "failed"}); err == nil {
			t.Fatal("accepted an incomplete native outcome")
		}
		if _, err := o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 5, report: "continuing", completed: true, continuing: true}); err == nil {
			t.Fatal("accepted a continuing native boundary")
		}
		decision, err = o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 5, report: "report", completed: true})
		if err != nil || !decision.allow {
			t.Fatalf("pre-abort approval = %#v, %v", decision, err)
		}
		decision, err = o.finalizeMemberSettlement(binding, 5, true, commit)
		if err != nil || decision.allow || decision.feedback != "" || commits != 1 {
			t.Fatalf("aborted settlement = %#v, commits=%d, %v", decision, commits, err)
		}
		tree.mu.Lock()
		selected.progress.turns = tree.config.MaxTurns
		tree.mu.Unlock()
		o.policy.TeammateIdle = []string{"/bin/sh", "-c", `printf '{"permit":false,"feedback":"More work remains."}'`}
		decision, err = o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 6, report: "report", completed: true})
		if err != nil || decision.allow || decision.continueRun || decision.feedback != "More work remains." {
			t.Fatalf("turn limit refusal = %#v, %v", decision, err)
		}
	}, func(string, json.RawMessage) json.RawMessage { return json.RawMessage(`"fixture"`) })
}
