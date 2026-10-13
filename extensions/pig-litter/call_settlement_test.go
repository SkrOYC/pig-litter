package piglitter

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
)

func testCallOwner(tree *Tree) *owner {
	return &owner{tree: tree, config: tree.config, state: &extensionState{calls: newCallScope()}, acks: map[*callTicket]*acknowledgement{}, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}}
}

func testCall(t *testing.T, o *owner, caller *run, id, operation string) *callTicket {
	t.Helper()
	if caller != nil && caller.calls == nil {
		caller.calls = newCallScope()
	}
	ticket, err := o.callScope(caller).begin(caller, id, operation, o.config.MaxMailbox)
	if err != nil {
		t.Fatal(err)
	}
	return ticket
}

func TestFinalIndividualSuccessSurvivesHookReplacementAndOuterFailure(t *testing.T) {
	for _, callID := range []string{"direct-spawn", "script/1"} {
		for _, hookFailure := range []bool{false, true} {
			t.Run(callID+map[bool]string{false: "/success", true: "/failure"}[hookFailure], func(t *testing.T) {
				tree := testTree(t, defaultConfig())
				o := testCallOwner(tree)
				tree.execute = func(_ *record, _ *run) Outcome {
					return Outcome{State: Completed, Text: "child completed inside script"}
				}
				var selected *run
				tool := &nativeTool{name: "litter_spawn", schema: object([]string{}, map[string]any{}), execute: func(ctx context.Context, id string, _ json.RawMessage) (agent.AgentToolResult, error) {
					ticket := testCall(t, o, nil, id, "litter_spawn")
					child := testAdmission(t, tree, Ref{}, "scout", "child")
					selected = testRun(t, tree, child.Ref)
					o.deferActivation(ticket, selected, ctx.Done(), func() bool { return ctx.Err() != nil }, "litter_spawn")
					return nativeResult("litter_spawn", map[string]any{"child": child}, nil), nil
				}}
				final, err := agent.RunToolCall(context.Background(), agent.AgentToolCall{ID: callID, Name: "litter_spawn", Arguments: ai.JsonObject{}}, agent.RunToolCallOptions{Tools: []agent.AgentTool{tool}, ToolCallHooks: agent.ToolCallHooks{AfterToolCall: []agent.AfterToolCallHook{func(context.Context, string, string, json.RawMessage, agent.AgentToolResult) agent.AfterToolCallResult {
					return agent.AfterToolCallResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "replacement output"}}, Details: map[string]any{"hook": "replacement metadata"}, IsError: &hookFailure}
				}}}})
				if err != nil {
					t.Fatal(err)
				}
				o.observeCall(nil, "litter_spawn", callID, final.Result, final.IsError)
				o.observeToolEvent(map[string]any{"toolCallId": "script", "toolName": "codemode", "isError": true})
				outcome, err := tree.wait(context.Background(), selected, 1000)
				if err != nil {
					t.Fatal(err)
				}
				if hookFailure {
					if outcome.State != Stopped || tree.live != 0 || len(tree.records) != 0 || len(tree.pending) != 0 {
						t.Fatalf("failed final hook retained an admission: %#v", outcome)
					}
				} else if outcome.State != Completed || outcome.Text != "child completed inside script" {
					t.Fatalf("successful transformed result lost its child: %#v", outcome)
				}
			})
		}
	}
}

func TestCapturedCallbackCannotSettleReusedIDInANewTurn(t *testing.T) {
	tree := testTree(t, defaultConfig())
	o := testCallOwner(tree)
	tree.execute = func(r *record, _ *run) Outcome { return Outcome{State: Completed, Text: r.name} }
	oldChild := testAdmission(t, tree, Ref{}, "scout", "old")
	oldRun := testRun(t, tree, oldChild.Ref)
	oldTicket := testCall(t, o, nil, "same-call-id", "litter_spawn")
	o.deferActivation(oldTicket, oldRun, nil, func() bool { return false }, "litter_spawn")
	captured := oldTicket.scope.capture(oldTicket.callID, "litter_spawn")
	if _, err := oldTicket.scope.begin(nil, oldTicket.callID, "litter_spawn", 16); err == nil {
		t.Fatal("duplicate current-scope ID replaced the live ticket")
	}
	if oldTicket.scope.capture(oldTicket.callID, "litter_spawn") != captured {
		t.Fatal("duplicate changed the captured invocation")
	}
	o.retireScope(oldTicket.scope)
	o.state.calls = newCallScope()
	newChild := testAdmission(t, tree, Ref{}, "scout", "new")
	newRun := testRun(t, tree, newChild.Ref)
	newTicket := testCall(t, o, nil, "same-call-id", "litter_spawn")
	o.deferActivation(newTicket, newRun, nil, func() bool { return false }, "litter_spawn")
	o.finishCall(captured, agent.AgentToolResult{}, false)
	o.finishCall(captured, agent.AgentToolResult{}, true)
	if _, err := tree.wait(context.Background(), newRun, 1); err == nil {
		t.Fatal("old callback settled a replacement invocation")
	}
	o.observeCall(nil, "litter_spawn", newTicket.callID, agent.AgentToolResult{}, false)
	o.finishCall(newTicket, agent.AgentToolResult{}, false)
	outcome, err := tree.wait(context.Background(), newRun, 1000)
	if err != nil || outcome.Text != "new" || outcome.State != Completed {
		t.Fatalf("matching final result failed: %#v %v", outcome, err)
	}
}

func TestNestedHandbackConsumptionUsesCapturedTicketAndFinalContent(t *testing.T) {
	tree := testTree(t, defaultConfig())
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read"}, CanDelegate: true}
	parent := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	child := testAdmission(t, tree, parent.Ref, "scout", "child")
	caller, selected := testRun(t, tree, parent.Ref), testRun(t, tree, child.Ref)
	o := testCallOwner(tree)
	ticket := testCall(t, o, caller, "script/2", "litter_wait")
	o.beginOutcomeAck(ticket, selected, "litter_wait", nil)
	tree.finish(tree.records[child.ID], selected, Outcome{State: Completed, Text: "nested result"})
	o.readyOutcomeAck(ticket, true)
	result := nativeResult("litter_wait", map[string]any{"outcome": selected.outcome}, nil)
	result.Details = map[string]any{"hook": "new details"}
	o.observeCall(caller, "litter_wait", ticket.callID, result, false)
	if !o.awaitOutcomeAck(selected) || tree.pending[selected] == nil {
		t.Fatal("nested result did not consume its handback while preserving storage capacity")
	}
	tree.acknowledge(selected)
	_, resumed, err := tree.resume(caller, selected, "continue")
	if err != nil {
		t.Fatal(err)
	}
	o.finishCall(ticket, result, false)
	if tree.pending[resumed].outcomeObserved {
		t.Fatal("old handback consumed a later turn")
	}
	missing := testCall(t, o, caller, "script/3", "litter_stop")
	o.beginOutcomeAck(missing, resumed, "litter_stop", nil)
	o.readyOutcomeAck(missing, true)
	o.retireScope(missing.scope)
	settled := make(chan bool, 1)
	go func() { settled <- o.awaitOutcomeAck(resumed) }()
	select {
	case consumed := <-settled:
		if consumed {
			t.Fatal("missing final event consumed the result")
		}
	case <-time.After(time.Second):
		t.Fatal("turn retirement stranded a handback")
	}
}

func TestRootRunSettlementAndAbortHaveDifferentEffects(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal settlement", true: "explicit abort"}[abort], func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			child := testAdmission(t, tree, Ref{}, "scout", "child")
			selected := testRun(t, tree, child.Ref)
			o := testCallOwner(tree)
			signal, cancel := context.WithCancel(context.Background())
			defer cancel()
			o.watchRoot(signal)
			if abort {
				cancel()
			}
			o.settleRoot()
			inspection, err := tree.inspect(child.ID)
			if err != nil {
				t.Fatal(err)
			}
			if abort {
				if inspection.Child.State != Paused || selected.ctx.Err() == nil {
					t.Fatalf("abort did not pause subtree: %#v", inspection)
				}
			} else if inspection.Child.State != Starting || selected.ctx.Err() != nil {
				t.Fatalf("normal settlement paused a child: %#v", inspection)
			}
		})
	}
}

func TestFailedAdmissionReleasesNameRecordAndMailbox(t *testing.T) {
	config := defaultConfig()
	config.Concurrency, config.MaxRecords, config.MaxMailbox = 1, 1, 1
	tree := testTree(t, config)
	o := testCallOwner(tree)
	for range 3 {
		child := testAdmission(t, tree, Ref{}, "scout", "same name")
		selected := testRun(t, tree, child.Ref)
		o.state.calls = newCallScope()
		ticket := testCall(t, o, nil, "spawn", "litter_spawn")
		o.deferActivation(ticket, selected, nil, func() bool { return false }, "litter_spawn")
		o.observeCall(nil, "litter_spawn", ticket.callID, agent.AgentToolResult{}, true)
		if outcome, err := tree.wait(context.Background(), selected, 1000); err != nil || outcome.State != Stopped {
			t.Fatalf("failed final hook outcome: %#v %v", outcome, err)
		}
		if len(tree.records) != 0 || len(tree.names) != 0 || len(tree.pending) != 0 || tree.live != 0 {
			t.Fatal("failed invocation retained capacity")
		}
	}
}
