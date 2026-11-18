package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
)

func planTestMember(t *testing.T) (*teamStore, string, teamMember) {
	t.Helper()
	store, dir, _, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "planner", workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	return store, dir, member
}

func TestPlanSubmissionAndDecisionAuthority(t *testing.T) {
	store, _, member := planTestMember(t)
	request, err := store.submitPlan(member.Ref, "run-1", "proposal-1", "Write the marker.", 60000)
	if err != nil || request.State != "pending" || request.ID.Member != member.Ref || request.ID.Sequence != 1 || !validPlanNonce(request.ID.Nonce) {
		t.Fatalf("submission = %#v, %v", request, err)
	}
	retry, err := store.submitPlan(member.Ref, "run-1", "proposal-1", "Write the marker.", 60000)
	if err != nil || retry != request {
		t.Fatalf("identical retry = %#v, %v", retry, err)
	}
	if _, err := store.submitPlan(member.Ref, "run-1", "proposal-1", "Change the marker.", 60000); err == nil {
		t.Fatal("changed submission reused an existing ID")
	}
	if _, err := store.decidePlan(request.ID, "another-run", "approve", ""); err == nil {
		t.Fatal("a stale execution received approval")
	}
	foreign := request.ID
	foreign.Member.Generation++
	if _, err := store.decidePlan(foreign, "run-1", "approve", ""); err == nil {
		t.Fatal("a different member generation received approval")
	}
	if _, err := store.decidePlan(request.ID, "run-1", "approve", "unexpected"); err == nil {
		t.Fatal("approval accepted a rejection reason")
	}
	if granted, err := store.planGranted(member.Ref); err != nil || granted {
		t.Fatalf("gate opened before approval: %v, %v", granted, err)
	}
	approved, err := store.decidePlan(request.ID, "run-1", "approve", "")
	if err != nil || approved.State != "approved" {
		t.Fatalf("approval = %#v, %v", approved, err)
	}
	if granted, err := store.planGranted(member.Ref); err != nil || !granted {
		t.Fatalf("approved gate = %v, %v", granted, err)
	}
	if _, err := store.decidePlan(request.ID, "run-1", "approve", ""); err == nil {
		t.Fatal("a replayed decision was accepted")
	}
	if _, err := store.submitPlan(member.Ref, "run-1", "proposal-2", "Replace the approved plan.", 60000); err == nil {
		t.Fatal("a new submission replaced a live grant")
	}
}

func TestPlanRejectionCancellationExpiryAndRecovery(t *testing.T) {
	store, dir, member := planTestMember(t)
	first, err := store.submitPlan(member.Ref, "run-1", "one", "First plan.", 60000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.decidePlan(first.ID, "run-1", "reject", ""); err == nil {
		t.Fatal("rejection accepted an empty reason")
	}
	rejected, err := store.decidePlan(first.ID, "run-1", "reject", "Revise the plan.")
	if err != nil || rejected.State != "rejected" || rejected.Reason != "Revise the plan." {
		t.Fatalf("rejection = %#v, %v", rejected, err)
	}
	second, err := store.submitPlan(member.Ref, "run-1", "two", "Second plan.", 60000)
	if err != nil || second.ID.Sequence != 2 {
		t.Fatalf("revision = %#v, %v", second, err)
	}
	cancelled, err := store.cancelPlan(second.ID)
	if err != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancellation = %#v, %v", cancelled, err)
	}
	third, err := store.submitPlan(member.Ref, "run-1", "three", "Third plan.", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.planTransaction(member.Ref, func(ledger *planLedger) (bool, *PlanRequestID, error) {
		request := &ledger.Requests[third.ID.Sequence-1]
		request.CreatedAt = time.Now().UTC().Add(-2 * time.Second)
		request.Deadline = request.CreatedAt.Add(time.Second)
		return true, nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.decidePlan(third.ID, "run-1", "approve", ""); err == nil {
		t.Fatal("an expired request received approval")
	}
	requests, err := store.readPlans(member.Ref)
	if err != nil || requests[2].State != "expired" {
		t.Fatalf("expired request = %#v, %v", requests, err)
	}
	fourth, err := store.submitPlan(member.Ref, "run-1", "four", "Fourth plan.", 60000)
	if err != nil || fourth.ID.Sequence != 4 {
		t.Fatalf("pending request = %#v, %v", fourth, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTeamStore(dir, member.Ref.Team.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.recoverPlans(member.Ref); err != nil {
		t.Fatal(err)
	}
	requests, err = reopened.readPlans(member.Ref)
	if err != nil || len(requests) != 4 || requests[3].State != "cancelled" || requests[0].State != "rejected" {
		t.Fatalf("recovered plan history = %#v, %v", requests, err)
	}
	if granted, err := reopened.planGranted(member.Ref); err != nil || granted {
		t.Fatalf("recovery restored authority: %v, %v", granted, err)
	}
}

func TestPlanRecoveryKeepsApprovalHistoryWithoutGrant(t *testing.T) {
	store, dir, member := planTestMember(t)
	request, err := store.submitPlan(member.Ref, "run-1", "approved", "Approved plan.", 60000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.decidePlan(request.ID, "run-1", "approve", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTeamStore(dir, member.Ref.Team.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := reopened.recoverPlans(member.Ref); err != nil {
		t.Fatal(err)
	}
	history, err := reopened.readPlans(member.Ref)
	if err != nil || len(history) != 1 || history[0].State != "approved" {
		t.Fatalf("approval history = %#v, %v", history, err)
	}
	if granted, err := reopened.planGranted(member.Ref); err != nil || granted {
		t.Fatalf("historical approval reopened the gate: %v, %v", granted, err)
	}
	next, err := reopened.submitPlan(member.Ref, "run-2", "fresh", "New plan.", 60000)
	if err != nil || next.ID.Sequence != 2 || next.ID.Member != member.Ref {
		t.Fatalf("fresh approval request = %#v, %v", next, err)
	}
}

func TestPlanDeadlineExpiresWithoutAnotherToolCall(t *testing.T) {
	store, _, member := planTestMember(t)
	request, err := store.submitPlan(member.Ref, "run-1", "deadline", "Plan.", 1000)
	if err != nil {
		t.Fatal(err)
	}
	_, path := planPaths(store.dir, member.Ref)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(path)
		var ledger planLedger
		if err == nil && json.Unmarshal(data, &ledger) == nil && len(ledger.Requests) == 1 && ledger.Requests[0].State == "expired" {
			if _, err := store.decidePlan(request.ID, "run-1", "approve", ""); err == nil {
				t.Fatal("expired plan received a grant")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the pending plan did not expire in the durable record")
}

func TestPlanUncertainApprovalDoesNotGrant(t *testing.T) {
	store, _, member := planTestMember(t)
	request, err := store.submitPlan(member.Ref, "run-1", "uncertain", "Plan.", 60000)
	if err != nil {
		t.Fatal(err)
	}
	var fail atomic.Bool
	fail.Store(true)
	store.syncDir = func(path string) error {
		if fail.Load() {
			return errors.New("injected directory sync failure")
		}
		return syncDir(path)
	}
	if _, err := store.decidePlan(request.ID, "run-1", "approve", ""); err == nil {
		t.Fatal("approval succeeded with an uncertain directory write")
	}
	if granted, err := store.planGranted(member.Ref); err == nil || granted {
		t.Fatalf("uncertain approval opened the gate: %v, %v", granted, err)
	}
	fail.Store(false)
	if granted, err := store.planGranted(member.Ref); err != nil || granted {
		t.Fatalf("reconciliation manufactured a grant: %v, %v", granted, err)
	}
	history, err := store.readPlans(member.Ref)
	if err != nil || len(history) != 1 || history[0].State != "approved" {
		t.Fatalf("reconciled decision history = %#v, %v", history, err)
	}
	if _, err := store.decidePlan(request.ID, "run-1", "approve", ""); err == nil {
		t.Fatal("a reconciled decision was replayed as a fresh grant")
	}
}

func TestPlanGrantClosesWhenStoredHistoryChanges(t *testing.T) {
	store, _, member := planTestMember(t)
	request, err := store.submitPlan(member.Ref, "run-1", "intact", "Plan.", 60000)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.decidePlan(request.ID, "run-1", "approve", ""); err != nil {
		t.Fatal(err)
	}
	_, path := planPaths(store.dir, member.Ref)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(append([]byte(nil), original...), ' '), 0o600); err != nil {
		t.Fatal(err)
	}
	if granted, err := store.planGranted(member.Ref); err == nil || granted {
		t.Fatalf("changed plan history left the gate open: %v, %v", granted, err)
	}
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if granted, err := store.planGranted(member.Ref); err != nil || !granted {
		t.Fatalf("restored plan history lost its owner-local grant: %v, %v", granted, err)
	}
}

func TestPlanCompetingDecisionsCommitOnce(t *testing.T) {
	store, _, member := planTestMember(t)
	request, err := store.submitPlan(member.Ref, "run-1", "race", "Plan.", 60000)
	if err != nil {
		t.Fatal(err)
	}
	var group sync.WaitGroup
	results := make(chan string, 2)
	for _, decision := range []string{"approve", "reject"} {
		group.Add(1)
		go func(decision string) {
			defer group.Done()
			reason := ""
			if decision == "reject" {
				reason = "Revise."
			}
			if _, err := store.decidePlan(request.ID, "run-1", decision, reason); err == nil {
				results <- decision
			}
		}(decision)
	}
	group.Wait()
	close(results)
	winners := []string{}
	for decision := range results {
		winners = append(winners, decision)
	}
	if len(winners) != 1 {
		t.Fatalf("decision winners = %#v", winners)
	}
	granted, err := store.planGranted(member.Ref)
	if err != nil || granted != (winners[0] == "approve") {
		t.Fatalf("gate after competing decisions = %v, %v; winner %s", granted, err, winners[0])
	}
}

func TestPlanCommandRejectsAmbiguousActions(t *testing.T) {
	id := `{"member":{"team":{"root":"018f0000-0000-7000-8000-000000000001","epoch":1},"id":"018f0000-0000-7000-8000-000000000002","generation":1},"sequence":1}`
	for _, raw := range []string{
		`{"action":"submit","submissionId":"one","text":"Plan","timeoutMs":1000}`,
		`{"action":"list"}`,
		`{"action":"decide","requestId":` + id + `,"decision":"approve"}`,
		`{"action":"decide","requestId":` + id + `,"decision":"reject","reason":"Revise."}`,
	} {
		if _, err := parsePlanCommand(json.RawMessage(raw)); err != nil {
			t.Fatalf("rejected valid command %s: %v", raw, err)
		}
	}
	for _, raw := range []string{
		`{"action":"list","action":"submit"}`,
		`{"action":"list","sender":"lead"}`,
		`{"action":"decide","requestId":` + id + `,"decision":"approve","reason":"no"}`,
		`{"action":"decide","requestId":` + id + `,"decision":"reject"}`,
		`{"action":"submit","submissionId":"one","text":"Plan","timeoutMs":null}`,
		strings.Replace(id, `"epoch":1`, `"epoch":1,"epoch":2`, 1),
	} {
		if _, err := parsePlanCommand(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted ambiguous command %s", raw)
		}
	}
}

func TestTeamAdmissionModeRequiresMatchingLeadCallAndResult(t *testing.T) {
	_, _, member := planTestMember(t)
	call := `{"message":{"role":"assistant","content":[{"type":"toolCall","id":"spawn","name":"team_spawn","arguments":{"type":"worker","task":"Plan","name":"planner","workspace":{"kind":"shared_checkout"},"planRequired":true}}]}}`
	result, err := json.Marshal(map[string]any{"operation": "team_spawn", "state": "starting", "admission": member.AdmissionID, "member": member.Ref, "planRequired": true})
	if err != nil {
		t.Fatal(err)
	}
	entry, err := json.Marshal(map[string]any{"message": map[string]any{"role": "toolResult", "toolCallId": "spawn", "toolName": "team_spawn", "content": []any{map[string]any{"type": "text", "text": string(result)}}}})
	if err != nil {
		t.Fatal(err)
	}
	mode, known, err := teamAdmissionMode([]json.RawMessage{json.RawMessage(call), entry}, member)
	if err != nil || !known || !mode {
		t.Fatalf("verified admission = %v, %v, %v", mode, known, err)
	}
	if _, _, err := teamAdmissionMode([]json.RawMessage{entry}, member); err == nil {
		t.Fatal("accepted a result without the bound lead call")
	}
	conflicting := strings.Replace(call, `"planRequired":true`, `"planRequired":false`, 1)
	if _, _, err := teamAdmissionMode([]json.RawMessage{json.RawMessage(conflicting), entry}, member); err == nil {
		t.Fatal("accepted conflicting admission mode")
	}
}

func TestPlanHookProtectsOrdinaryDescendant(t *testing.T) {
	store, _, member := planTestMember(t)
	tree := testTree(t, defaultConfig())
	tree.root.id = member.Ref.Team.Root
	worker := AgentDefinition{Role: "worker", Tools: []string{"read", "write"}, CanDelegate: true}
	_, parent, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: member.Workspace.Cwd, task: "plan", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	child, selected, err := tree.admit(admission{parentRun: parent, kind: "scout", name: "descendant", cwd: t.TempDir(), task: "write", agent: AgentDefinition{Role: "scout", Tools: []string{"read"}}})
	if err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "marker")
	tool := &nativeTool{name: "write", schema: object([]string{}, map[string]any{}), execute: func(context.Context, string, json.RawMessage) (agent.AgentToolResult, error) {
		return agent.AgentToolResult{}, os.WriteFile(marker, []byte("approved"), 0o600)
	}}
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		for _, name := range []string{"edit", "bash", "codemode", "mcp__fixture__write", "litter_spawn", "litter_wait", "team_send", "team_task_create", "team_task_update"} {
			if got := o.beforePlanTool(selected.owner, selected, "pending-"+name, name, json.RawMessage(`{}`)); !got.Block {
				t.Fatalf("pending descendant was allowed to call %s", name)
			}
		}
		invoke := func(id string) agent.AgentToolCallOutcome {
			result, err := agent.RunToolCall(context.Background(), agent.AgentToolCall{ID: id, Name: "write", Arguments: ai.JsonObject{}}, agent.RunToolCallOptions{Tools: []agent.AgentTool{tool}, ToolCallHooks: agent.ToolCallHooks{BeforeToolCallHooks: []agent.BeforeToolCallHook{o.planHook(selected.owner, selected)}}})
			if err != nil {
				t.Fatal(err)
			}
			return result
		}
		blocked := invoke("blocked")
		if !blocked.IsError || !consumePlanDenial(selected, "blocked", "write") || consumePlanDenial(selected, "other", "write") {
			t.Fatalf("descendant denial = %#v", blocked)
		}
		if _, err := os.Stat(marker); !os.IsNotExist(err) {
			t.Fatalf("unapproved descendant changed the marker: %v", err)
		}
		request, err := store.submitPlan(member.Ref, parent.noticeID, "descendant-plan", "Permit the work.", 60000)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.decidePlan(request.ID, parent.noticeID, "approve", ""); err != nil {
			t.Fatal(err)
		}
		allowed := invoke("allowed")
		data, err := os.ReadFile(marker)
		if err != nil || allowed.IsError || string(data) != "approved" {
			t.Fatalf("approved descendant %s = %q, %#v, %v", child.ID, data, allowed, err)
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			return json.RawMessage(`"fixture"`)
		}
		t.Fatalf("unexpected host method %s", method)
		return nil
	})
}

func TestPlanOperationBindsLeadAndCurrentRun(t *testing.T) {
	store, _, member := planTestMember(t)
	tree := testTree(t, defaultConfig())
	tree.root.id = member.Ref.Team.Root
	worker := AgentDefinition{Role: "worker", Tools: []string{"read", "write"}, CanDelegate: true}
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: member.Workspace.Cwd, task: "plan", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		submit := json.RawMessage(`{"action":"submit","submissionId":"bound","text":"Plan the work.","timeoutMs":60000}`)
		if _, err := o.planOperation(submit, nil, nil); err == nil {
			t.Fatal("lead submitted a teammate plan")
		}
		value, err := o.planOperation(submit, selected, selected.owner)
		if err != nil || value["state"] != "pending" {
			t.Fatalf("member submission = %#v, %v", value, err)
		}
		request := value["request"].(planRequest)
		decision, _ := json.Marshal(map[string]any{"action": "decide", "requestId": request.ID, "decision": "approve"})
		if _, err := o.planOperation(decision, selected, selected.owner); err == nil {
			t.Fatal("teammate approved its own plan")
		}
		wrong := request.ID
		wrong.Member.Generation++
		wrongDecision, _ := json.Marshal(map[string]any{"action": "decide", "requestId": wrong, "decision": "approve"})
		if _, err := o.planOperation(wrongDecision, nil, nil); err == nil {
			t.Fatal("lead approved a different member generation")
		}
		tree.mu.Lock()
		replacement := tree.newRunLocked(Ref{member.Ref.ID}, "")
		replacement.owner = selected.owner
		selected.owner.run = replacement
		tree.mu.Unlock()
		if _, err := o.planOperation(decision, nil, nil); err == nil {
			t.Fatal("lead approved a request from an old execution")
		}
		tree.mu.Lock()
		selected.owner.run = selected
		tree.mu.Unlock()
		replacement.cancel()
		value, err = o.planOperation(decision, nil, nil)
		if err != nil || value["state"] != "approved" || value["gate"] != "granted" {
			t.Fatalf("lead approval = %#v, %v", value, err)
		}
		if _, err := o.planOperation(decision, nil, nil); err == nil {
			t.Fatal("lead replayed an approval")
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			return json.RawMessage(`"fixture"`)
		}
		t.Fatalf("unexpected host method %s", method)
		return nil
	})
}
