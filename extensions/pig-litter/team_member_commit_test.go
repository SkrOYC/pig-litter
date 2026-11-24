package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
)

func TestNativeSettlementCommitsRootOutcomeAfterRevalidation(t *testing.T) {
	for _, failStore := range []bool{false, true} {
		name := "complete"
		if failStore {
			name = "storage failure"
		}
		t.Run(name, func(t *testing.T) {
			store, agentDir, member := planTestMember(t)
			manager := nativeTestManager(t, member.Workspace.Cwd, string(member.Ref.ID))
			tree := testTree(t, defaultConfig())
			tree.root.id = member.Ref.Team.Root
			worker, _ := agentDefaults("worker")
			_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, model: "fixture/fixed", cwd: member.Workspace.Cwd, task: "work", agent: worker, member: &member})
			if err != nil {
				t.Fatal(err)
			}
			selected.started = true
			var published []Outcome
			tree.teamFinished = func(_ *record, _ *run, outcome Outcome) error {
				published = append(published, outcome)
				return nil
			}
			if failStore {
				tree.persist = func(*record) error { return errors.New("fixture storage failure") }
			}
			host := &runtimeHost{}
			withCompletionHost(t, tree, func(o *owner) {
				o.agentDir, o.teamStore = agentDir, store
				binding := bindMemberFixture(t, o, selected.owner, selected, filepath.Dir(manager.Path()), manager.Path(), testMemberControl{})
				if _, err := manager.AppendMessage(agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant", Content: []ai.AssistantContentBlock{ai.TextContent{Text: "Completed review."}}, StopReason: ai.StopReasonStop, Usage: &ai.Usage{Input: 10, Output: 4, TotalTokens: 14}}}); err != nil {
					t.Fatal(err)
				}
				authority := &memberAuthority{owner: o, binding: binding, gate: &teamAdmissionGate{owner: o, record: binding.record, run: selected, committed: true}}
				candidate := memberSettlementCandidate{boundary: 1, report: "Completed review.", completed: true}
				decision, err := o.decideMemberSettlement(context.Background(), binding, candidate)
				if err != nil || !decision.allow {
					t.Fatalf("settlement decision = %#v, %v", decision, err)
				}
				if _, err := store.createTask("New work", "", nil, nil); err != nil {
					t.Fatal(err)
				}
				decision, err = authority.finalizeSettlement(context.Background(), memberFinalizationRequest{Boundary: 1})
				if err != nil || decision.allow || !decision.continueRun || len(published) != 0 {
					t.Fatalf("stale settlement = %#v, published=%#v, %v", decision, published, err)
				}
				candidate.boundary = 2
				decision, err = o.decideMemberSettlement(context.Background(), binding, candidate)
				if err != nil || !decision.allow {
					t.Fatalf("renewed settlement decision = %#v, %v", decision, err)
				}
				decision, err = authority.finalizeSettlement(context.Background(), memberFinalizationRequest{Boundary: 2})
				if failStore {
					if err == nil || decision.allow || len(published) != 1 || published[0].State != Partial || !strings.Contains(published[0].Error, "storage failure") {
						t.Fatalf("failed persistence = %#v, published=%#v, %v", decision, published, err)
					}
				} else if err != nil || !decision.allow || len(published) != 1 || published[0].State != Completed || published[0].Text != "Completed review." || published[0].Usage.TotalTokens != 14 {
					t.Fatalf("root outcome = %#v, published=%#v, %v", decision, published, err)
				}
				select {
				case <-selected.done:
				default:
					t.Fatal("committed outcome did not settle the root run")
				}
				if _, err := authority.finalizeSettlement(context.Background(), memberFinalizationRequest{Boundary: 2}); err == nil || len(published) != 1 {
					t.Fatalf("settlement replay published another outcome: %#v, %v", published, err)
				}
			}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
		})
	}
}
