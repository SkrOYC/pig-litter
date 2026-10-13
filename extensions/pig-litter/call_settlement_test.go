package piglitter

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestFinalIndividualToolSuccessStartsChildBeforeOuterCompletion(t *testing.T) {
	for _, callID := range []string{"direct-spawn", "script/1"} {
		t.Run(callID, func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			child := testAdmission(t, tree, Ref{}, "scout", "child")
			selected := testRun(t, tree, child.Ref)
			o := &owner{tree: tree, acks: map[ackKey]*acknowledgement{}, outcomeAcks: map[ackKey]*outcomeAcknowledgement{}}
			tree.execute = func(_ *record, _ *run) Outcome {
				return Outcome{State: Completed, Text: "child completed inside script"}
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			key := ackKey{callID: callID, nonce: "individual-invocation"}
			o.deferActivation(key, selected, ctx.Done(), func() bool { return ctx.Err() != nil }, "litter_spawn")
			result := sdkResult("litter_spawn", map[string]any{"state": Starting, "child": child, "_receipt": key.nonce}, nil)
			o.observeToolEvent(map[string]any{"toolCallId": callID, "toolName": "litter_spawn", "parentToolCallId": "script", "result": map[string]any{"content": []any{map[string]any{"type": "text", "text": result.Content}}, "details": result.Details}, "isError": false})
			o.observeToolEvent(map[string]any{"toolCallId": "script", "toolName": "codemode", "isError": true})
			cancel()
			outcome, err := tree.wait(context.Background(), selected, 1000)
			if err != nil || outcome.State != Completed || outcome.Text != "child completed inside script" {
				t.Fatalf("outer completion controlled a successful child: %#v %v", outcome, err)
			}
			var content map[string]any
			if err := json.Unmarshal([]byte(result.Content), &content); err != nil {
				t.Fatal(err)
			}
			if _, exists := content["_receipt"]; exists {
				t.Fatalf("private invocation receipt leaked into model content: %s", result.Content)
			}
		})
	}
}

func TestReusedCallIDCannotSettleAnotherInvocation(t *testing.T) {
	tree := testTree(t, defaultConfig())
	o := &owner{tree: tree, acks: map[ackKey]*acknowledgement{}, outcomeAcks: map[ackKey]*outcomeAcknowledgement{}}
	tree.execute = func(r *record, _ *run) Outcome { return Outcome{State: Completed, Text: r.name} }
	oldChild := testAdmission(t, tree, Ref{}, "scout", "old")
	newChild := testAdmission(t, tree, Ref{}, "scout", "new")
	oldRun, newRun := testRun(t, tree, oldChild.Ref), testRun(t, tree, newChild.Ref)
	oldKey := ackKey{callID: "same-call-id", nonce: "old-invocation"}
	newKey := ackKey{callID: "same-call-id", nonce: "new-invocation"}
	o.deferActivation(oldKey, oldRun, nil, func() bool { return false }, "litter_spawn")
	o.deferActivation(newKey, newRun, nil, func() bool { return false }, "litter_spawn")
	oldResult := nativeResult("litter_spawn", map[string]any{"child": oldChild, "_receipt": oldKey.nonce}, nil)
	newResult := nativeResult("litter_spawn", map[string]any{"child": newChild, "_receipt": newKey.nonce}, nil)
	o.observeCall(nil, "litter_spawn", oldKey.callID, oldResult, true)
	o.observeCall(nil, "litter_spawn", oldKey.callID, oldResult, false)
	if _, err := tree.wait(context.Background(), newRun, 1); err == nil {
		t.Fatal("a delayed result settled the replacement invocation")
	}
	o.observeCall(nil, "litter_spawn", newKey.callID, newResult, false)
	o.observeCall(nil, "litter_spawn", newKey.callID, newResult, false)
	outcome, err := tree.wait(context.Background(), newRun, 1000)
	if err != nil || outcome.Text != "new" || outcome.State != Completed {
		t.Fatalf("matching invocation failed: %#v %v", outcome, err)
	}
	outcome, err = tree.wait(context.Background(), oldRun, 1000)
	if err != nil || outcome.State != Stopped {
		t.Fatalf("failed final hook admitted its child: %#v %v", outcome, err)
	}
}

func TestNestedTerminalHandbackConsumesOnlyItsCapturedTurn(t *testing.T) {
	tree := testTree(t, defaultConfig())
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read"}, CanDelegate: true}
	parent := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	child := testAdmission(t, tree, parent.Ref, "scout", "child")
	caller, selected := testRun(t, tree, parent.Ref), testRun(t, tree, child.Ref)
	o := &owner{tree: tree, outcomeAcks: map[ackKey]*outcomeAcknowledgement{}}
	key := ackKey{caller: caller, callID: "nested-script/2", nonce: "wait-invocation"}
	o.beginOutcomeAck(key, selected, "litter_wait", nil)
	tree.finish(tree.records[child.ID], selected, Outcome{State: Completed, Text: "nested result"})
	o.readyOutcomeAck(key, true)
	result := nativeResult("litter_wait", map[string]any{"outcome": selected.outcome, "_receipt": key.nonce}, nil)
	o.observeCall(caller, "litter_wait", key.callID, result, false)
	if !o.awaitOutcomeAck(selected) {
		t.Fatal("nested final handback did not suppress the automatic duplicate")
	}
	if tree.pending[selected] == nil {
		t.Fatal("consumption released capacity before an audit append")
	}
	tree.acknowledge(selected)
	_, resumed, err := tree.resume(testCaller(selected), selected, "continue")
	if err != nil {
		t.Fatal(err)
	}
	o.observeCall(caller, "litter_wait", key.callID, result, false)
	if tree.pending[resumed].outcomeObserved {
		t.Fatal("old handback consumed the replacement turn")
	}
	missing := ackKey{caller: caller, callID: "missing/1", nonce: "missing-event"}
	o.beginOutcomeAck(missing, resumed, "litter_stop", nil)
	o.readyOutcomeAck(missing, true)
	o.retireAcks(caller)
	settled := make(chan bool, 1)
	go func() { settled <- o.awaitOutcomeAck(resumed) }()
	select {
	case consumed := <-settled:
		if consumed {
			t.Fatal("missing final event consumed a result")
		}
	case <-time.After(time.Second):
		t.Fatal("caller retirement stranded a terminal handback")
	}
}

func TestRootRunSettlementAndAbortHaveDifferentEffects(t *testing.T) {
	for _, abort := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal settlement", true: "explicit abort"}[abort], func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			child := testAdmission(t, tree, Ref{}, "scout", "child")
			selected := testRun(t, tree, child.Ref)
			o := &owner{tree: tree}
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

func TestFailedAdmissionReleasesItsNameRecordAndMailbox(t *testing.T) {
	config := defaultConfig()
	config.Concurrency, config.MaxRecords, config.MaxMailbox = 1, 1, 1
	tree := testTree(t, config)
	o := &owner{tree: tree, acks: map[ackKey]*acknowledgement{}}
	for _, nonce := range []string{"first", "retry", "retry-again"} {
		child := testAdmission(t, tree, Ref{}, "scout", "same name")
		selected := testRun(t, tree, child.Ref)
		key := ackKey{callID: "spawn", nonce: nonce}
		o.deferActivation(key, selected, nil, func() bool { return false }, "litter_spawn")
		result := nativeResult("litter_spawn", map[string]any{"child": child, "_receipt": nonce}, nil)
		o.observeCall(nil, "litter_spawn", key.callID, result, true)
		if outcome, err := tree.wait(context.Background(), selected, 1000); err != nil || outcome.State != Stopped {
			t.Fatalf("failed final hook outcome: %#v %v", outcome, err)
		}
		if len(tree.records) != 0 || len(tree.names) != 0 || len(tree.pending) != 0 || tree.live != 0 {
			t.Fatalf("failed invocation retained capacity: records=%d names=%d notices=%d live=%d", len(tree.records), len(tree.names), len(tree.pending), tree.live)
		}
	}
}

func TestPauseBeforeFinalSuccessStillRollsBackTheProvisionalAdmission(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "provisional")
	selected := testRun(t, tree, child.Ref)
	o := &owner{tree: tree, acks: map[ackKey]*acknowledgement{}}
	key := ackKey{callID: "spawn", nonce: "paused-before-final-success"}
	o.deferActivation(key, selected, nil, func() bool { return false }, "litter_spawn")
	tree.pauseRoot()
	o.retireAcks(nil)
	if len(tree.records) != 0 || len(tree.pending) != 0 || tree.live != 0 {
		t.Fatalf("pause retained an uncommitted child: nodes=%d pending=%d live=%d", len(tree.records), len(tree.pending), tree.live)
	}
	result := nativeResult("litter_spawn", map[string]any{"child": child, "_receipt": key.nonce}, nil)
	o.observeCall(nil, "litter_spawn", key.callID, result, false)
	if len(tree.records) != 0 {
		t.Fatal("late success resurrected an abandoned admission")
	}
}
