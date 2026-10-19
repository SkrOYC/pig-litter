package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestTUIRootNextTurnAcceptanceDoesNotAcknowledgeOrResubmit(t *testing.T) {
	tree := testTree(t, defaultConfig())
	selected := panelAdmission(t, tree, "deferred root result")
	tree.finish(selected.owner, selected, Outcome{State: Completed, Text: "DEFERRED_ROOT_REPORT"})
	sends, appends := 0, 0
	var observed sdk.SendMessageOptions
	withCompletionMode(t, tree, "tui", func(o *owner) {
		o.sendCompletion(selected.owner, selected, *selected.outcome)
		o.retireReceipts(tree.root)
		o.retireReceipts(tree.root)
		continuation := o.continueForCompletions(tree.root, nil)
		tree.mu.Lock()
		notice := tree.pending[selected]
		pending := notice != nil && !notice.stored && !notice.outcomeObserved
		tree.mu.Unlock()
		if observed.DeliverAs != "nextTurn" || observed.TriggerTurn == nil || *observed.TriggerTurn || sends != 1 || appends != 0 || !pending || continuation {
			t.Errorf("TUI native acceptance was not deferred and unacknowledged: options=%#v sends=%d appends=%d pending=%t continuation=%t", observed, sends, appends, pending, continuation)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		switch method {
		case "sessionRead":
			var query struct{ Method string }
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getEntries" {
				return json.RawMessage(`[]`)
			}
			if query.Method == "getSessionFile" {
				return json.RawMessage(`null`)
			}
			return json.RawMessage(`"fixture"`)
		case "isIdle":
			return json.RawMessage(`{"idle":false}`)
		case "sendMessage":
			sends++
			var args struct{ Options sdk.SendMessageOptions }
			_ = json.Unmarshal(raw, &args)
			observed = args.Options
			return json.RawMessage(`{}`)
		case "appendEntry":
			appends++
			return json.RawMessage(`{}`)
		default:
			t.Errorf("unexpected root delivery host action %s", method)
			return json.RawMessage(`{}`)
		}
	})
}

func TestTUIRootRejectedSendRetriesOnlyFromGenuineReconciliation(t *testing.T) {
	tree := testTree(t, defaultConfig())
	selected := panelAdmission(t, tree, "retry deferred result")
	tree.execute = func(*record, *run) Outcome { return Outcome{State: Completed, Text: "RETRIED_ROOT_REPORT"} }
	if err := tree.activate(selected); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.wait(context.Background(), selected, 1000); err != nil {
		t.Fatal(err)
	}
	entries := []map[string]any{}
	sends := 0
	var last map[string]any
	withCompletionReplies(t, tree, "tui", func(o *owner) {
		o.sendCompletion(selected.owner, selected, *selected.outcome)
		o.retireReceipts(tree.root)
		if sends != 1 || len(tree.pending) != 1 {
			t.Error("settlement retried a rejected terminal notice")
		}
		for range 3 {
			if _, err := o.reconcileRootContext(nil); err != nil {
				t.Error(err)
			}
			tree.completionWG.Wait()
		}
		if sends != 2 || len(tree.pending) != 1 {
			t.Errorf("reconciliation did not retry rejection once: sends=%d pending=%d", sends, len(tree.pending))
		}
		last["id"] = "promoted-next-turn"
		entries = append(entries, last)
		if _, err := o.reconcileRootContext(nil); err != nil {
			t.Error(err)
		}
		if sends != 2 || len(tree.pending) != 0 || len(tree.root.receipts) != 0 || selected.deliveryReceipt != nil {
			t.Errorf("native promotion failed to retire accepted receipt: sends=%d pending=%d receipts=%d", sends, len(tree.pending), len(tree.root.receipts))
		}
		inspection, _ := tree.inspect(selected.ref.ID)
		if inspection.Outcome.Text != "RETRIED_ROOT_REPORT" {
			t.Errorf("retry lost retained result: %#v", inspection.Outcome)
		}
	}, func(method string, raw json.RawMessage) *subprocess.CallResultPayload {
		reply := &subprocess.CallResultPayload{Result: json.RawMessage(`{}`)}
		switch method {
		case "sessionRead":
			var query struct{ Method string }
			_ = json.Unmarshal(raw, &query)
			reply.Result = json.RawMessage(`"fixture"`)
			if query.Method == "getEntries" {
				reply.Result, _ = json.Marshal(entries)
			} else if query.Method == "getSessionFile" {
				reply.Result = json.RawMessage(`null`)
			}
		case "isIdle":
			reply.Result = json.RawMessage(`{"idle":false}`)
		case "sendMessage":
			sends++
			var args struct{ Message map[string]any }
			_ = json.Unmarshal(raw, &args)
			last = args.Message
			if sends == 1 {
				reply.Error = &subprocess.ErrorInfo{Message: "native queue temporarily unavailable"}
			}
		default:
			t.Errorf("unexpected retry host action %s", method)
		}
		return reply
	})
}

func TestStoredReceiptWinsOverLateSubmissionAndRetirement(t *testing.T) {
	for _, late := range []error{nil, errors.New("late native rejection")} {
		for _, consumed := range []bool{false, true} {
			t.Run(fmt.Sprintf("late=%v/consumed=%t", late, consumed), func(t *testing.T) {
				tree := testTree(t, defaultConfig())
				selected := panelAdmission(t, tree, "late result")
				tree.finish(selected.owner, selected, Outcome{State: Completed, Text: "TERMINAL_NATIVE_REPORT"})
				content, _ := completionContent(*selected.outcome)
				details := completionDetails{ChildID: selected.ref.ID, Receipt: selected.noticeID}
				entries := []map[string]any{}
				if consumed {
					after := "before-native-wait"
					result, _ := resultContent("litter_wait", map[string]any{"outcome": selected.outcome}, nil)
					entries = append(entries,
						map[string]any{"id": after},
						map[string]any{"id": "native-wait", "message": map[string]any{"role": "toolResult", "toolCallId": "genuine-wait", "toolName": "litter_wait", "content": []map[string]any{{"type": "text", "text": result}}}},
						map[string]any{"id": "native-audit", "customType": "litter_completion", "data": completionAudit{Content: content, Details: details, Handback: &outcomeHandback{CallID: "genuine-wait", Operation: "litter_wait", After: &after}}})
				} else {
					entries = append(entries, map[string]any{"id": "native-message", "customType": "litter_completion", "details": details, "content": content})
				}
				raw, _ := rawEntries(entries)
				submitted, fallback := 0, 0
				receipt := &completionReceipt{deferred: true,
					observed: func() bool { return completionStored(raw, details, content) },
					appended: func() { tree.acknowledge(selected) },
					rejected: func(err error) { tree.deliveryFailed(selected, err) },
					fallback: func() error { fallback++; return nil },
					submit:   func() error { submitted++; return nil },
				}
				receipt.complete()
				receipt.submitted(late)
				receipt.retire()
				if receipt.retry() {
					t.Error("durably confirmed receipt became retryable")
				}
				inspection, _ := tree.inspect(selected.ref.ID)
				if receipt.state != receiptConfirmed || len(tree.pending) != 0 || inspection.Outcome.State != Completed || inspection.Outcome.Error != "" || inspection.Outcome.Text != "TERMINAL_NATIVE_REPORT" || fallback != 0 || submitted != 0 {
					t.Errorf("late submission changed native confirmation: receipt=%v pending=%d outcome=%#v fallback=%d sends=%d", receipt.state, len(tree.pending), inspection.Outcome, fallback, submitted)
				}
			})
		}
	}
}

func TestTUIConsumedFilteringSurvivesCapacityReleaseWithoutTrustingAuditAlone(t *testing.T) {
	for _, fault := range []string{"valid", "missing-handback", "wrong-child", "wrong-content", "wrong-receipt", "wrong-call", "before-boundary", "wrong-operation", "wrong-root", "unadmitted", "grandchild", "wrong-message"} {
		t.Run(fault, func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			id := ChildID("00000000-0000-7000-8000-000000000001")
			outcome := Outcome{Run: Ref{id}, State: Completed, Model: "fixture/child", Text: "CONSUMED_NATIVE_REPORT"}
			content, _ := completionContent(outcome)
			details := completionDetails{ChildID: id, Receipt: "consumed-receipt"}
			saved := savedChild{RootID: tree.root.id, ParentID: tree.root.id, ID: id, Admitted: true, Notices: []savedNotice{{Nonce: details.Receipt, Outcome: outcome}}}
			after := "before-native-wait"
			handback := &outcomeHandback{CallID: "native-wait", Operation: "litter_wait", After: &after}
			audit := completionAudit{Content: content, Details: details, Handback: handback}
			returned := outcome
			switch fault {
			case "wrong-child":
				returned.Run.ID = "00000000-0000-7000-8000-000000000002"
			case "wrong-content":
				returned.Text = "another native outcome"
			case "wrong-receipt":
				audit.Details.Receipt = "forged-receipt"
			case "wrong-call":
				handback.CallID = "another-call"
			case "before-boundary":
				*handback.After = "after-native-wait"
			case "wrong-operation":
				handback.Operation = "litter_inspect"
			case "wrong-root":
				saved.RootID = "another-root"
			case "unadmitted":
				saved.Admitted = false
			case "grandchild":
				saved.ParentID = "another-parent"
			}
			result, _ := resultContent("litter_wait", map[string]any{"outcome": &returned}, nil)
			entries := []map[string]any{
				{"id": "before-native-wait"},
				{"id": "old-authoritative-child-metadata", "customType": childEntryType, "data": saved},
				{"id": "native-wait-result", "message": map[string]any{"role": "toolResult", "toolCallId": "native-wait", "toolName": "litter_wait", "content": []map[string]any{{"type": "text", "text": result}}}},
				{"id": "after-native-wait"},
				{"id": "existing-completion-audit", "customType": "litter_completion", "data": audit},
				{"id": "latest-metadata-after-capacity-release", "customType": childEntryType, "data": savedChild{RootID: tree.root.id, ParentID: tree.root.id, ID: id, Admitted: true}},
			}
			if fault == "missing-handback" {
				entries = append(entries[:2], entries[3:]...)
			}
			message := map[string]any{"role": "custom", "customType": "litter_completion", "content": content, "details": details}
			if fault == "wrong-message" {
				message["content"] = "unrelated message using the same details"
			}
			withCompletionMode(t, tree, "tui", func(o *owner) {
				for range 2 {
					returned, err := o.reconcileRootContext([]any{message, map[string]any{"role": "user", "content": "explicit next task"}})
					if err != nil {
						t.Error(err)
						return
					}
					messages := returned.(map[string]any)["messages"].([]any)
					want := 2
					if fault == "valid" {
						want = 1
					}
					if len(messages) != want || messages[len(messages)-1].(map[string]any)["content"] != "explicit next task" {
						t.Errorf("released capacity lost receipt authority or trusted %s: %#v", fault, messages)
					}
				}
			}, func(method string, raw json.RawMessage) json.RawMessage {
				if method == "sessionRead" {
					var query struct{ Method string }
					_ = json.Unmarshal(raw, &query)
					if query.Method == "getEntries" {
						data, _ := json.Marshal(entries)
						return data
					}
					if query.Method == "getSessionFile" {
						return json.RawMessage(`null`)
					}
				}
				t.Errorf("consumed filtering attempted a write or send through %s", method)
				return json.RawMessage(`{}`)
			})
		})
	}
}

func TestTUIUnpersistedHandbackCanRecoverThroughNativeDelivery(t *testing.T) {
	tree := testTree(t, defaultConfig())
	selected := panelAdmission(t, tree, "unpersisted handback")
	tree.execute = func(*record, *run) Outcome { return Outcome{State: Completed, Text: "MISSING_NATIVE_HANDBACK"} }
	if err := tree.activate(selected); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.wait(context.Background(), selected, 1000); err != nil {
		t.Fatal(err)
	}
	tree.pending[selected].outcomeObserved = true
	entries := []map[string]any{}
	sends := 0
	withCompletionMode(t, tree, "tui", func(o *owner) {
		o.sendCompletion(selected.owner, selected, *selected.outcome)
		if len(tree.pending) != 1 || selected.deliveryReceipt == nil || sends != 0 {
			t.Error("unpersisted handback was acknowledged or delivered before reconciliation")
		}
		if _, err := o.reconcileRootContext(nil); err != nil {
			t.Error(err)
		}
		tree.completionWG.Wait()
		if sends != 1 || len(tree.pending) != 0 || selected.deliveryReceipt != nil {
			t.Errorf("unpersisted handback stranded its receipt: sends=%d pending=%d", sends, len(tree.pending))
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
			if query.Method == "getSessionFile" {
				return json.RawMessage(`null`)
			}
			return json.RawMessage(`"fixture"`)
		case "isIdle":
			return json.RawMessage(`{"idle":false}`)
		case "appendEntry", "sendMessage":
			var entry map[string]any
			_ = json.Unmarshal(raw, &entry)
			if method == "sendMessage" {
				sends++
				entry = entry["message"].(map[string]any)
				if !strings.Contains(entry["content"].(string), "MISSING_NATIVE_HANDBACK") {
					t.Error("recovery omitted the real retained report")
				}
			}
			entry["id"] = fmt.Sprintf("native-%d", len(entries))
			entries = append(entries, entry)
			return json.RawMessage(`{}`)
		default:
			t.Errorf("unexpected handback recovery host action %s", method)
			return json.RawMessage(`{}`)
		}
	})
}
