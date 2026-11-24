package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

func withSettlementReviewFixture(t *testing.T, exercise func(*owner, *memberBinding, *coding.SessionManager, *teamStore)) {
	t.Helper()
	store, agentDir, member := planTestMember(t)
	manager := nativeTestManager(t, member.Workspace.Cwd, string(member.Ref.ID))
	tree := testTree(t, defaultConfig())
	tree.root.id = member.Ref.Team.Root
	worker, _ := agentDefaults("worker")
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, model: "fixture/fixed", cwd: member.Workspace.Cwd, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	host := &runtimeHost{}
	withCompletionHost(t, tree, func(o *owner) {
		o.agentDir, o.teamStore = agentDir, store
		binding := bindMemberFixture(t, o, selected.owner, selected, filepath.Dir(manager.Path()), manager.Path(), testMemberControl{})
		exercise(o, binding, manager, store)
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
}

func appendReviewAssistant(t *testing.T, manager *coding.SessionManager, text string) {
	t.Helper()
	_, err := manager.AppendMessage(agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant", Content: []ai.AssistantContentBlock{ai.TextContent{Text: text}}, StopReason: ai.StopReasonStop}})
	if err != nil {
		t.Error(err)
	}
}

func TestReviewNativeToolFailureCannotBecomeCompleted(t *testing.T) {
	withSettlementReviewFixture(t, func(o *owner, binding *memberBinding, manager *coding.SessionManager, _ *teamStore) {
		_, err := manager.AppendMessage(agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant", Content: []ai.AssistantContentBlock{ai.ToolCall{ID: "write-failed", Name: "write", Arguments: ai.JsonObject{}}}, StopReason: ai.StopReasonToolUse}})
		if err != nil {
			t.Error(err)
			return
		}
		_, err = manager.AppendMessage(agent.AgentMessage{ToolResult: &agent.ToolResultMessage{Role: "toolResult", ToolCallID: "write-failed", ToolName: "write", IsError: true, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "disk failed"}}}})
		if err != nil {
			t.Error(err)
			return
		}
		appendReviewAssistant(t, manager, "I am done.")
		evidence, err := o.memberOutcome(context.Background(), binding)
		if err != nil || evidence.outcome.State != Partial || !strings.Contains(evidence.outcome.Error, "disk failed") {
			t.Errorf("failed native tool outcome = %#v, %v", evidence.outcome, err)
			return
		}
		o.tree.mu.Lock()
		binding.run.started = true
		o.tree.mu.Unlock()
		o.tree.teamFinished = func(*record, *run, Outcome) error { return nil }
		decision, err := o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 1, report: "I am done.", completed: true})
		if err != nil || !decision.allow {
			t.Errorf("failed tool settlement decision = %#v, %v", decision, err)
			return
		}
		authority := &memberAuthority{owner: o, binding: binding, gate: &teamAdmissionGate{owner: o, record: binding.record, run: binding.run, committed: true}}
		decision, err = authority.finalizeSettlement(context.Background(), memberFinalizationRequest{Boundary: 1})
		if err != nil || !decision.allow || binding.run.outcome == nil || binding.run.outcome.State != Partial || !strings.Contains(binding.run.outcome.Error, "disk failed") {
			t.Errorf("failed tool root outcome = %#v, %#v, %v", decision, binding.run.outcome, err)
		}
	})
}

func TestReviewNativePlanDenialMatchesItsExactResponse(t *testing.T) {
	withSettlementReviewFixture(t, func(o *owner, binding *memberBinding, manager *coding.SessionManager, _ *teamStore) {
		o.tree.mu.Lock()
		binding.ownership.Tools = append(binding.ownership.Tools, "write")
		slices.Sort(binding.ownership.Tools)
		binding.owned = cloneMemberOwnership(binding.ownership)
		o.tree.mu.Unlock()
		authority := &memberAuthority{owner: o, binding: binding, gate: &teamAdmissionGate{owner: o, record: binding.record, run: binding.run, committed: true}}
		if err := authority.startTurn(1); err != nil {
			t.Error(err)
			return
		}
		call := ai.ToolCall{ID: "same-call", Name: "write", Arguments: ai.JsonObject{}}
		response, err := manager.AppendMessage(agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant", Content: []ai.AssistantContentBlock{call}, StopReason: ai.StopReasonToolUse}})
		if err != nil || authority.observeResponse(context.Background(), memberResponseRequest{Turn: 1, EntryID: response}) != nil {
			t.Errorf("observe first response: %v", err)
			return
		}
		decision := authority.authorize(context.Background(), memberToolRequest{Turn: 1, ID: call.ID, Name: call.Name, Arguments: json.RawMessage(`{}`)})
		if !decision.Block || !strings.Contains(decision.Reason, "plan approval") {
			t.Errorf("expected plan denial = %#v", decision)
			return
		}
		appendFailure := func(text string) {
			t.Helper()
			_, err := manager.AppendMessage(agent.AgentMessage{ToolResult: &agent.ToolResultMessage{Role: "toolResult", ToolCallID: call.ID, ToolName: call.Name, IsError: true, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: text}}}})
			if err != nil {
				t.Error(err)
			}
		}
		appendFailure(decision.Reason)
		appendReviewAssistant(t, manager, "Plan needed.")
		evidence, err := o.memberOutcome(context.Background(), binding)
		if err != nil || evidence.outcome.State != Completed {
			t.Errorf("expected plan denial outcome = %#v, %v", evidence.outcome, err)
		}
		if _, err := manager.AppendMessage(agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant", Content: []ai.AssistantContentBlock{call}, StopReason: ai.StopReasonToolUse}}); err != nil {
			t.Error(err)
			return
		}
		appendFailure("different disk failure")
		appendReviewAssistant(t, manager, "I am done.")
		evidence, err = o.memberOutcome(context.Background(), binding)
		if err != nil || evidence.outcome.State != Partial || !strings.Contains(evidence.outcome.Error, "different disk failure") {
			t.Errorf("reused call ID concealed a later failure: %#v, %v", evidence.outcome, err)
		}
		_ = authority.endTurn(1)
	})
}

func TestReviewNativeNestedFailuresRequireCompleteEvidence(t *testing.T) {
	assistant, _ := json.Marshal(map[string]any{"type": "message", "id": "response", "message": agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant", Content: []ai.AssistantContentBlock{ai.ToolCall{ID: "parent", Name: "codemode", Arguments: ai.JsonObject{}}}, StopReason: ai.StopReasonToolUse}}})
	for _, test := range []struct {
		name, callID, text string
		complete           bool
		wantFailure        bool
	}{
		{name: "expected denial", callID: "parent/1", text: "plan approval is required", complete: true},
		{name: "different failure for denied call", callID: "parent/1", text: "disk failed", complete: true, wantFailure: true},
		{name: "other failure", callID: "parent/2", text: "disk failed", complete: true, wantFailure: true},
		{name: "incomplete trace", complete: false, wantFailure: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			calls := []ai.NestedToolCallRecord{}
			if test.callID != "" {
				calls = append(calls, ai.NestedToolCallRecord{ID: test.callID, Name: "write", Status: ai.NestedToolCallError, Error: test.text})
			}
			message := agent.AgentMessage{ToolResult: &agent.ToolResultMessage{Role: "toolResult", ToolCallID: "parent", ToolName: "codemode", NestedCalls: &ai.NestedToolCalls{Calls: calls, Complete: test.complete}}}
			result, _ := json.Marshal(map[string]any{"type": "message", "id": "result", "message": message})
			snapshot := memberLogSnapshot{entries: []json.RawMessage{assistant, result}}
			denials := map[memberPlanDenial]string{{response: "response", call: planDenial{ID: "parent/1", Name: "write"}}: "plan approval is required"}
			failure, err := memberHistoryFailure(snapshot, 0, denials)
			if err != nil || (failure != "") != test.wantFailure {
				t.Fatalf("nested outcome = %q, %v", failure, err)
			}
		})
	}
}

func TestReviewNativePlanDenialDoesNotHideDifferentError(t *testing.T) {
	assistant, _ := json.Marshal(map[string]any{"type": "message", "id": "response", "message": agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant", Content: []ai.AssistantContentBlock{ai.ToolCall{ID: "call", Name: "write", Arguments: ai.JsonObject{}}}, StopReason: ai.StopReasonToolUse}}})
	message := agent.AgentMessage{ToolResult: &agent.ToolResultMessage{Role: "toolResult", ToolCallID: "call", ToolName: "write", IsError: true, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "disk failed"}}}}
	result, _ := json.Marshal(map[string]any{"type": "message", "id": "result", "message": message})
	denials := map[memberPlanDenial]string{{response: "response", call: planDenial{ID: "call", Name: "write"}}: "plan approval is required"}
	failure, err := memberHistoryFailure(memberLogSnapshot{entries: []json.RawMessage{assistant, result}}, 0, denials)
	if err != nil || !strings.Contains(failure, "disk failed") {
		t.Fatalf("different tool failure = %q, %v", failure, err)
	}
}

func TestReviewNativeSettlementDoesNotApproveFailedTeamPersistence(t *testing.T) {
	withSettlementReviewFixture(t, func(o *owner, binding *memberBinding, manager *coding.SessionManager, _ *teamStore) {
		appendReviewAssistant(t, manager, "Complete.")
		o.tree.mu.Lock()
		binding.run.started = true
		o.tree.mu.Unlock()
		failure := errors.New("team notice storage failed")
		o.tree.teamFinished = func(*record, *run, Outcome) error { return failure }
		candidate := memberSettlementCandidate{boundary: 1, report: "Complete.", completed: true}
		decision, err := o.decideMemberSettlement(context.Background(), binding, candidate)
		if err != nil || !decision.allow {
			t.Errorf("pre-settlement decision = %#v, %v", decision, err)
			return
		}
		authority := &memberAuthority{owner: o, binding: binding, gate: &teamAdmissionGate{owner: o, record: binding.record, run: binding.run, committed: true}}
		decision, err = authority.finalizeSettlement(context.Background(), memberFinalizationRequest{Boundary: 1})
		if err == nil || decision.allow || binding.run.outcome == nil || binding.run.outcome.State != Completed || !errors.Is(binding.run.teamFinishErr, failure) {
			t.Errorf("failed team persistence = %#v, outcome=%#v, teamErr=%v, err=%v", decision, binding.run.outcome, binding.run.teamFinishErr, err)
		}
	})
}

func TestReviewNativeSettlementReportsTeamStoreFailure(t *testing.T) {
	withSettlementReviewFixture(t, func(o *owner, binding *memberBinding, manager *coding.SessionManager, store *teamStore) {
		if _, err := store.listTasks(); err != nil {
			t.Error(err)
			return
		}
		appendReviewAssistant(t, manager, "Complete.")
		o.tree.mu.Lock()
		binding.run.started = true
		o.tree.mu.Unlock()
		o.tree.teamFinished = o.finishTeamMember
		decision, err := o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 1, report: "Complete.", completed: true})
		if err != nil || !decision.allow {
			t.Errorf("pre-settlement decision = %#v, %v", decision, err)
			return
		}
		originalSync := store.syncDir
		store.syncDir = func(string) error { return errors.New("fixture team storage failure") }
		defer func() { store.syncDir = originalSync }()
		authority := &memberAuthority{owner: o, binding: binding, gate: &teamAdmissionGate{owner: o, record: binding.record, run: binding.run, committed: true}}
		decision, err = authority.finalizeSettlement(context.Background(), memberFinalizationRequest{Boundary: 1})
		if err == nil || decision.allow || binding.run.outcome == nil || binding.run.outcome.State != Completed || binding.run.teamFinishErr == nil {
			t.Errorf("team store failure = %#v, outcome=%#v, teamErr=%v, err=%v", decision, binding.run.outcome, binding.run.teamFinishErr, err)
		}
	})
}

func TestReviewNativeSettlementFencesTaskMutations(t *testing.T) {
	for _, action := range []string{"create", "assign", "delete"} {
		t.Run(action, func(t *testing.T) {
			withSettlementReviewFixture(t, func(o *owner, binding *memberBinding, _ *coding.SessionManager, store *teamStore) {
				task, err := store.createTask("Pending work", "", nil, nil)
				if err != nil {
					t.Error(err)
					return
				}
				decision, err := o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 1, report: "Complete.", completed: true})
				if err != nil || !decision.allow {
					t.Errorf("pre-settlement decision = %#v, %v", decision, err)
					return
				}
				operation := "team_task_update"
				input := map[string]any{"taskId": task.ID, "action": action}
				if action == "create" {
					operation = "team_task_create"
					input = map[string]any{"subject": "New work"}
				} else if action == "assign" {
					input["assignee"] = binding.record.name
				}
				args, _ := json.Marshal(input)
				entered, started := make(chan struct{}), make(chan struct{})
				mutation := make(chan error, 1)
				go func() {
					<-entered
					close(started)
					_, err := o.operation(context.Background(), args, &callTicket{operation: operation}, nil)
					mutation <- err
				}()
				mutatedBeforeCommit := false
				var mutationErr error
				decision, err = o.finalizeMemberSettlement(binding, 1, false, func() error {
					close(entered)
					<-started
					select {
					case mutationErr = <-mutation:
						mutatedBeforeCommit = true
					case <-time.After(750 * time.Millisecond):
					}
					return nil
				})
				if !mutatedBeforeCommit {
					mutationErr = <-mutation
				}
				if err != nil || !decision.allow || mutationErr != nil || mutatedBeforeCommit {
					t.Errorf("task %s crossed finalization: decision=%#v, mutation before commit=%t, mutation error=%v, err=%v", action, decision, mutatedBeforeCommit, mutationErr, err)
				}
			})
		})
	}
}

func TestReviewNativeSettlementReleasesLifecycleBeforePersistence(t *testing.T) {
	withSettlementReviewFixture(t, func(o *owner, binding *memberBinding, manager *coding.SessionManager, store *teamStore) {
		appendReviewAssistant(t, manager, "Complete.")
		task, err := store.createTask("Pending work", "", nil, nil)
		if err != nil {
			t.Error(err)
			return
		}
		decision, err := o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 1, report: "Complete.", completed: true})
		if err != nil || !decision.allow {
			t.Errorf("pre-settlement decision = %#v, %v", decision, err)
			return
		}
		o.tree.mu.Lock()
		binding.run.started = true
		o.tree.mu.Unlock()
		o.tree.teamFinished = func(*record, *run, Outcome) error { return nil }
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		o.tree.persist = func(*record) error {
			once.Do(func() { close(entered); <-release })
			return nil
		}
		authority := &memberAuthority{owner: o, binding: binding, gate: &teamAdmissionGate{owner: o, record: binding.record, run: binding.run, committed: true}}
		settled := make(chan error, 1)
		go func() {
			decision, err := authority.finalizeSettlement(context.Background(), memberFinalizationRequest{Boundary: 1})
			if err == nil && !decision.allow {
				err = errors.New("settlement was not allowed")
			}
			settled <- err
		}()
		<-entered
		args, _ := json.Marshal(map[string]any{"taskId": task.ID, "action": "assign", "assignee": binding.record.name})
		mutation := make(chan error, 1)
		go func() {
			_, err := o.operation(context.Background(), args, &callTicket{operation: "team_task_update"}, nil)
			mutation <- err
		}()
		select {
		case err := <-mutation:
			if err != nil {
				t.Errorf("assignment after in-memory commit: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Error("task assignment was blocked by in-flight persistence")
		}
		close(release)
		if err := <-settled; err != nil {
			t.Errorf("root settlement = %v", err)
		}
		o.tree.persist = nil
	})
}

func TestReviewNativeCloseWaitsForInFlightSettlement(t *testing.T) {
	withSettlementReviewFixture(t, func(o *owner, binding *memberBinding, manager *coding.SessionManager, _ *teamStore) {
		appendReviewAssistant(t, manager, "Complete.")
		decision, err := o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 1, report: "Complete.", completed: true})
		if err != nil || !decision.allow {
			t.Errorf("pre-settlement decision = %#v, %v", decision, err)
			return
		}
		o.tree.mu.Lock()
		binding.run.started = true
		o.tree.mu.Unlock()
		o.tree.teamFinished = func(*record, *run, Outcome) error { return nil }
		entered, release := make(chan struct{}), make(chan struct{})
		var once sync.Once
		o.tree.persist = func(*record) error {
			once.Do(func() { close(entered); <-release })
			return nil
		}
		authority := &memberAuthority{owner: o, binding: binding, gate: &teamAdmissionGate{owner: o, record: binding.record, run: binding.run, committed: true}}
		settled, closed := make(chan error, 1), make(chan struct{})
		go func() {
			_, err := authority.finalizeSettlement(context.Background(), memberFinalizationRequest{Boundary: 1})
			settled <- err
		}()
		<-entered
		go func() { o.tree.close(); close(closed) }()
		select {
		case <-closed:
			t.Error("tree closed before the committed outcome finished persisting")
		case <-time.After(100 * time.Millisecond):
		}
		close(release)
		if err := <-settled; err == nil {
			t.Error("retired owner reported settlement approval")
		}
		select {
		case <-closed:
		case <-time.After(2 * time.Second):
			t.Error("tree did not close after settlement finished")
		}
		o.tree.persist = nil
	})
}

func TestReviewNativeSettlementMakesHostCallsOutsideLifecycleLock(t *testing.T) {
	store, agentDir, rootID, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "member", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	manager := nativeTestManager(t, workspace.Cwd, string(member.Ref.ID))
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	worker, _ := agentDefaults("worker")
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, model: "fixture/fixed", cwd: workspace.Cwd, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	selected.started = true
	tree.teamFinished = func(*record, *run, Outcome) error { return nil }
	host := &runtimeHost{}
	var held atomic.Int64
	withCompletionHost(t, tree, func(o *owner) {
		o.agentDir, o.teamStore = agentDir, store
		tree.persist = o.saveChild
		task, err := store.createTask("Task to complete", "", nil, nil)
		if err != nil {
			t.Error(err)
			return
		}
		binding := bindMemberFixture(t, o, selected.owner, selected, filepath.Dir(manager.Path()), manager.Path(), testMemberControl{})
		appendReviewAssistant(t, manager, "Complete.")
		decision, err := o.decideMemberSettlement(context.Background(), binding, memberSettlementCandidate{boundary: 1, report: "Complete.", completed: true})
		if err != nil || !decision.allow {
			t.Errorf("pre-settlement decision = %#v, %v", decision, err)
			return
		}
		authority := &memberAuthority{owner: o, binding: binding, gate: &teamAdmissionGate{owner: o, record: binding.record, run: selected}}
		decision, err = authority.finalizeSettlement(context.Background(), memberFinalizationRequest{Boundary: 1})
		if _, _, completeErr := o.applyTaskCompletion(controlAuthority{}, task.ID); completeErr != nil {
			t.Errorf("complete task: %v", completeErr)
		}
		if err != nil || !decision.allow || held.Load() != 0 {
			t.Errorf("settlement = %#v, host calls under lifecycle lock=%d, %v", decision, held.Load(), err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method == "sessionRead" || method == "appendEntry" {
			if tree.lifecycleMu.TryLock() {
				tree.lifecycleMu.Unlock()
			} else {
				held.Add(1)
			}
		}
		return host.call(t, method, raw)
	})
}
