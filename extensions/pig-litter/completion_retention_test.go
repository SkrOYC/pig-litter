package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func withCompletionHost(t *testing.T, tree *Tree, exercise func(*owner), hostCall func(string, json.RawMessage) json.RawMessage) {
	t.Helper()
	host, peer := net.Pipe()
	defer host.Close()
	defer peer.Close()
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ext := sdk.New("completion-retention-test")
	ext.OnEvent("exercise", func(ctx sdk.Context, _ map[string]any) (any, error) {
		state := &extensionState{calls: newCallScope()}
		o := &owner{state: state, sessionID: "fixture", context: ctx, tree: tree, config: tree.config, acks: map[*callTicket]*acknowledgement{}, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}}
		state.owner = o
		exercise(o)
		return nil, nil
	})
	done := make(chan error, 1)
	go func() { done <- ext.RunWithConn(peer) }()
	if frame := readCompletionEnvelope(t, host); frame.Type != subprocess.MsgRegister {
		t.Fatal(frame)
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgReady, Ready: &subprocess.ReadyPayload{Cwd: t.TempDir(), Width: 80}})
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "exercise", Request: &subprocess.RequestPayload{Method: "event", Event: "exercise", HandlerID: 1, Args: json.RawMessage(`{}`)}})
	for {
		frame := readCompletionEnvelope(t, host)
		if frame.Type == subprocess.MsgResponse && frame.ID == "exercise" {
			if frame.Response.Error != nil {
				t.Fatal(frame.Response.Error)
			}
			break
		}
		if frame.Type != subprocess.MsgCall {
			continue
		}
		result := hostCall(frame.Call.Method, frame.Call.Args)
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: result}})
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgShutdown, Shutdown: &subprocess.ShutdownPayload{Reason: "complete"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestRootSettlementRecoversDiscardedNoticeBeforeReleasingCapacity(t *testing.T) {
	config := defaultConfig()
	config.MaxMailbox = 1
	tree := testTree(t, config)
	child := testAdmission(t, tree, Ref{}, "scout", "child")
	selected := testRun(t, tree, child.Ref)
	tree.finish(tree.records[child.ID], selected, Outcome{State: Completed, Text: "retained report"})
	entries := []map[string]any{}
	sends := 0
	withCompletionHost(t, tree, func(o *owner) {
		o.sendCompletion(tree.records[child.ID], selected, *selected.outcome)
		if len(tree.pending) != 1 || len(tree.root.receipts) != 1 {
			t.Fatal("discarded root notice lost its storage reservation")
		}
		o.retireReceipts(tree.root)
		if len(tree.pending) != 0 || len(tree.root.receipts) != 0 {
			t.Fatal("root settlement did not recover the discarded notice")
		}
		if _, _, err := tree.admit(admission{name: "next", agent: config.Agents["scout"]}); err != nil {
			t.Fatalf("recovered root mailbox stayed full: %v", err)
		}
		inspection, _ := tree.inspect(child.ID)
		if inspection.Outcome.Text != "retained report" {
			t.Fatalf("recovery lost the child result: %#v", inspection)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		switch method {
		case "sessionRead":
			var query struct{ Method string }
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getEntries" {
				data, _ := json.Marshal(entries)
				return data
			}
			return json.RawMessage(`"fixture"`)
		case "isIdle":
			return json.RawMessage(`{"idle":false}`)
		case "sendMessage":
			sends++
			var args struct {
				Message map[string]any
				Options sdk.SendMessageOptions
			}
			if err := json.Unmarshal(raw, &args); err != nil {
				t.Fatal(err)
			}
			if args.Options.TriggerTurn == nil || *args.Options.TriggerTurn {
				t.Fatal("recovery can start a paused parent")
			}
			if sends == 2 {
				args.Message["id"] = "recovered-entry"
				entries = append(entries, args.Message)
			}
			return json.RawMessage(`{}`)
		default:
			t.Fatalf("unexpected call %s", method)
			return nil
		}
	})
	if sends != 2 || len(entries) != 1 {
		t.Fatalf("recovery sends=%d stored=%d", sends, len(entries))
	}
}

func TestLateWaitSuppressesStoredNoticeAndConsumedOnlyContinuation(t *testing.T) {
	for _, consumed := range []bool{false, true} {
		t.Run(fmt.Sprint(consumed), func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			child := testAdmission(t, tree, Ref{}, "scout", "child")
			selected := testRun(t, tree, child.Ref)
			tree.finish(tree.records[child.ID], selected, Outcome{State: Completed, Text: "stored report"})
			entries := []map[string]any{}
			audits := 0
			withCompletionHost(t, tree, func(o *owner) {
				o.sendCompletion(tree.records[child.ID], selected, *selected.outcome)
				if len(tree.pending) != 0 {
					t.Fatal("actual append did not release delivery capacity")
				}
				if consumed {
					ticket := testCall(t, o, nil, "wait-after-storage", "litter_wait")
					o.beginOutcomeAck(ticket, selected, "litter_wait", nil)
					o.readyOutcomeAck(ticket, true)
					o.finishCall(ticket, nativeResult("litter_wait", map[string]any{"outcome": selected.outcome}, nil), false)
				}
				if got := o.continueForCompletions(tree.root); got == consumed {
					t.Fatalf("automatic continuation=%t consumed=%t", got, consumed)
				}
				if o.continueForCompletions(tree.root) {
					t.Fatal("one stored notice requested a second continuation")
				}
				notice := map[string]any{"role": "custom", "customType": "litter_completion", "content": selected.reservation.content, "details": completionDetails{ChildID: child.ID, Receipt: selected.noticeID}}
				user := map[string]any{"role": "user", "content": "next task"}
				filtered := o.completionContext(tree.root, []any{notice, user})
				want := 2
				if consumed {
					want = 1
				}
				if len(filtered) != want || filtered[len(filtered)-1].(map[string]any)["content"] != "next task" {
					t.Fatalf("filtered context=%#v", filtered)
				}
				o.completionContext(tree.root, []any{user})
				if len(tree.root.notices) != 0 {
					t.Fatal("compacted context retained old notice tracking")
				}
			}, func(method string, raw json.RawMessage) json.RawMessage {
				switch method {
				case "sessionRead":
					var query struct{ Method string }
					_ = json.Unmarshal(raw, &query)
					if query.Method == "getEntries" {
						data, _ := json.Marshal(entries)
						return data
					}
					return json.RawMessage(`"fixture"`)
				case "isIdle":
					return json.RawMessage(`{"idle":false}`)
				case "sendMessage":
					var args struct{ Message map[string]any }
					_ = json.Unmarshal(raw, &args)
					args.Message["id"] = "stored-entry"
					entries = append(entries, args.Message)
					return json.RawMessage(`{}`)
				case "appendEntry":
					audits++
					return json.RawMessage(`{}`)
				default:
					t.Fatalf("unexpected call %s", method)
					return nil
				}
			})
			wantAudits := 0
			if consumed {
				wantAudits = 1
			}
			if audits != wantAudits {
				t.Fatalf("audits=%d want=%d", audits, wantAudits)
			}
		})
	}
}

func TestLateAuditRetiresOversizedHistoryAndReportsDeliveryFailure(t *testing.T) {
	config := defaultConfig()
	config.MaxHistoryBytes, config.MaxResultBytes = 2048, 1800
	tree := testTree(t, config)
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read"}, CanDelegate: true}
	parentRef := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	childRef := testAdmission(t, tree, parentRef.Ref, "scout", "child")
	parent, child := tree.records[parentRef.ID], tree.records[childRef.ID]
	tree.finish(child, child.run, Outcome{State: Completed, Text: strings.Repeat("x", 1800)})
	tree.finish(parent, parent.run, Outcome{State: Completed, Text: "parent settled"})
	tree.pending[child.run].outcomeObserved = true
	withCompletionHost(t, tree, func(o *owner) {
		o.sendCompletion(child, child.run, *child.run.outcome)
		inspection, err := tree.inspect(parent.id)
		if err != nil || !parent.historyLost || parent.history != nil || inspection.Outcome.State != Partial || !strings.Contains(inspection.Outcome.Error, "history cannot fit") {
			t.Fatalf("late audit remained unbounded or invisible: %#v %v", inspection, err)
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method != "sessionRead" {
			t.Fatalf("unexpected host call %s", method)
		}
		return json.RawMessage(`"fixture"`)
	})
}

func TestActiveOwnerPublishesLateDeliveryFailureWhenItFinishes(t *testing.T) {
	tree := testTree(t, defaultConfig())
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read"}, CanDelegate: true}
	parentRef := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	childRef := testAdmission(t, tree, parentRef.Ref, "scout", "child")
	parent, child := tree.records[parentRef.ID], tree.records[childRef.ID]
	tree.finish(child, child.run, Outcome{State: Completed, Text: "child report"})
	tree.deliveryFailed(child.run, fmt.Errorf("parent history unavailable"))
	tree.finish(parent, parent.run, Outcome{State: Completed, Text: "parent report"})
	inspection, err := tree.inspect(parent.id)
	if err != nil || inspection.Outcome.State != Partial || inspection.Outcome.Text != "parent report" || inspection.Outcome.Error != "child completion delivery failed: parent history unavailable" {
		t.Fatalf("active owner lost the delivery failure: %#v %v", inspection, err)
	}
}

func TestLateDeliveryFailureUpdatesCurrentObservationWithoutChangingCapturedResult(t *testing.T) {
	tree := testTree(t, defaultConfig())
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read"}, CanDelegate: true}
	parentRef := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	childRef := testAdmission(t, tree, parentRef.Ref, "scout", "child")
	parent, child := tree.records[parentRef.ID], tree.records[childRef.ID]
	tree.finish(parent, parent.run, Outcome{State: Completed, Text: "parent report", Usage: Usage{TotalTokens: 23}})
	captured := parent.run.outcome
	before, err := tree.wait(context.Background(), parent.run, 1)
	if err != nil {
		t.Fatal(err)
	}
	tree.deliveryFailed(child.run, fmt.Errorf("parent history unavailable"))
	inspection, err := tree.inspect(parent.id)
	if err != nil || inspection.Child.State != Partial || inspection.Outcome.State != Partial || inspection.Outcome.Error != "child completion delivery failed: parent history unavailable" {
		t.Fatalf("late failure projection is inconsistent: %#v %v", inspection, err)
	}
	after, err := tree.wait(context.Background(), parent.run, 1)
	if err != nil || after.State != Partial || after.Error != inspection.Outcome.Error || after.Text != "parent report" || after.Usage.TotalTokens != 23 {
		t.Fatalf("subsequent wait disagrees with inspection: %#v %v", after, err)
	}
	if captured.State != Completed || captured.Error != "" || before.State != Completed || before.Error != "" {
		t.Fatalf("late failure rewrote a captured execution result: captured=%#v before=%#v", captured, before)
	}
	stopped, _, err := tree.stop(nil, parent.run)
	inspection, _ = tree.inspect(parent.id)
	if err != nil || stopped.State != Partial || stopped.Error != after.Error || inspection.Child.State != Paused || inspection.Outcome.State != Partial {
		t.Fatalf("pause lost the retained delivery failure: stopped=%#v inspection=%#v error=%v", stopped, inspection, err)
	}
}
