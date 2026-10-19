package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestFirstTUIContextRestoresWithoutEarlierToolOrPanelAndReconcilesAfterPromotion(t *testing.T) {
	for _, retainedQueue := range []bool{false, true} {
		t.Run(fmt.Sprint(retainedQueue), func(t *testing.T) {
			t.Setenv("PIG_OFFLINE", "1")
			t.Setenv("PIG_CODING_AGENT_DIR", t.TempDir())
			directory := t.TempDir()
			id := ChildID("00000000-0000-7000-8000-000000000001")
			outcome := Outcome{Run: Ref{id}, Model: "fixture/child", State: Completed, Text: "COLD_CONTEXT_REPORT"}
			content, _ := completionContent(outcome)
			details := completionDetails{ChildID: id, Receipt: "cold-context-receipt"}
			saved := savedChild{RootID: "fixture", ParentID: "fixture", ID: id, Cwd: directory, Name: "cold restored child", Kind: "scout", Model: "fixture/child", Admitted: true, State: Completed, Latest: &outcome, NoticeID: details.Receipt, Notices: []savedNotice{{Nonce: details.Receipt, Outcome: outcome}}}
			entries := []map[string]any{{"id": "retained-child", "customType": childEntryType, "data": saved}}
			sends := 0
			var queued map[string]any
			if retainedQueue {
				entries = append(entries, map[string]any{"id": "native-promotion-before-context", "customType": "litter_completion", "content": content, "details": details})
			}
			for prompt := range 3 {
				state := &extensionState{}
				ext := newExtension(state)
				ext.OnEvent("receipt_drain", func(ctx sdk.Context, _ map[string]any) (any, error) {
					return state.observe(ctx, func(o *owner) (any, error) { o.tree.completionWG.Wait(); return nil, nil })
				})
				host, handlers, finish := startExtensionFixture(t, ext, &subprocess.ReadyPayload{Cwd: directory, Mode: "tui", Width: 100, Height: 32, State: &subprocess.StatePayload{HasUI: true}})
				answer := func(frame subprocess.Envelope) {
					reply := json.RawMessage(`{}`)
					switch frame.Call.Method {
					case "sessionRead":
						var query struct{ Method string }
						_ = json.Unmarshal(frame.Call.Args, &query)
						reply = json.RawMessage(`"fixture"`)
						if query.Method == "getEntries" {
							reply, _ = json.Marshal(entries)
						} else if query.Method == "getSessionFile" {
							reply = json.RawMessage(`null`)
						}
					case "appendEntry":
						var entry map[string]any
						_ = json.Unmarshal(frame.Call.Args, &entry)
						entry["id"] = fmt.Sprintf("saved-%d", len(entries))
						entries = append(entries, entry)
					case "isIdle":
						reply = json.RawMessage(`{"idle":false}`)
					case "sendMessage":
						sends++
						var args struct {
							Message map[string]any
							Options sdk.SendMessageOptions
						}
						_ = json.Unmarshal(frame.Call.Args, &args)
						queued = args.Message
						if args.Options.DeliverAs != "nextTurn" || args.Options.TriggerTurn == nil || *args.Options.TriggerTurn || queued["content"] != content {
							t.Errorf("cold recovery changed transport or content: %#v", args)
						}
					default:
						t.Errorf("context-only owner attempted %s", frame.Call.Method)
					}
					writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: reply}})
				}
				request := func(event string, args json.RawMessage) json.RawMessage {
					writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: event, Request: &subprocess.RequestPayload{Method: "event", Event: event, HandlerID: handlers[event], Args: args}})
					for {
						frame := readCompletionEnvelope(t, host)
						if frame.Type == subprocess.MsgCall {
							answer(frame)
						} else if frame.Type == subprocess.MsgResponse && frame.ID == event {
							if frame.Response.Error != nil {
								t.Fatal(frame.Response.Error)
							}
							return frame.Response.Result
						}
					}
				}
				writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgNotify, Notify: &subprocess.NotifyPayload{Method: "run_signal", Args: json.RawMessage(`{"run":1,"active":true}`)}})
				request("agent_start", json.RawMessage(`{}`))
				if state.owner != nil {
					t.Fatal("early agent_start created or replayed an owner")
				}
				request("context", json.RawMessage(`{"messages":[{"role":"user","content":"genuine explicit prompt"}]}`))
				request("receipt_drain", json.RawMessage(`{}`))
				o := state.owner
				if o == nil || len(o.tree.retained()) != 1 || o.tree.root.paused {
					t.Fatal("first genuine context failed to restore and authorize its owner")
				}
				wantSends, wantPending := 0, 0
				if !retainedQueue {
					wantSends = 1
					if prompt == 0 {
						wantPending = 1
					}
				}
				if sends != wantSends || len(o.tree.pending) != wantPending {
					t.Errorf("context %d after retainedQueue=%t replayed or lost recovery: sends=%d pending=%d", prompt, retainedQueue, sends, len(o.tree.pending))
				}
				finish()
				state.retire()
				if queued != nil {
					queued["id"] = "native-promotion-on-next-explicit-prompt"
					entries = append(entries, queued)
					queued = nil
				}
			}
		})
	}
}

func TestContextOnlyInitializationSanitizesHumanConfigurationErrors(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "litter.yaml"), []byte("\"\\e[2Jbad\\u202e\": 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &extensionState{}
	host, handlers, finish := startExtensionFixture(t, newExtension(state), &subprocess.ReadyPayload{Cwd: directory, Mode: "tui", State: &subprocess.StatePayload{HasUI: true}})
	defer finish()
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgNotify, Notify: &subprocess.NotifyPayload{Method: "run_signal", Args: json.RawMessage(`{"run":1,"active":true}`)}})
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "context", Request: &subprocess.RequestPayload{Method: "event", Event: "context", HandlerID: handlers["context"], Args: json.RawMessage(`{"messages":[]}`)}})
	for {
		frame := readCompletionEnvelope(t, host)
		if frame.Type == subprocess.MsgCall {
			if frame.Call.Method != "sessionRead" {
				t.Fatalf("hostile context continued through %s", frame.Call.Method)
			}
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: json.RawMessage(`"fixture"`)}})
		} else if frame.Type == subprocess.MsgResponse && frame.ID == "context" {
			if frame.Response.Error == nil || !strings.Contains(frame.Response.Error.Message, "unknown setting bad") || strings.ContainsAny(frame.Response.Error.Message, "\x1b\u202e\n\r") || state.owner != nil {
				t.Fatalf("context configuration error crossed the human boundary: %#v", frame.Response)
			}
			break
		}
	}
}

func TestRootCallbackWritersHoldTheFullRetirementBarrier(t *testing.T) {
	for _, blockedAt := range []string{"read", "ack", "send"} {
		t.Run(blockedAt, func(t *testing.T) {
			t.Setenv("PIG_OFFLINE", "1")
			t.Setenv("PIG_CODING_AGENT_DIR", t.TempDir())
			directory := t.TempDir()
			id := ChildID("00000000-0000-7000-8000-000000000001")
			outcome := Outcome{Run: Ref{id}, State: Completed, Model: "fixture/child", Text: "JOINED_ROOT_WRITER"}
			content, _ := completionContent(outcome)
			details := completionDetails{ChildID: id, Receipt: "writer-receipt"}
			saved := savedChild{RootID: "fixture", ParentID: "fixture", ID: id, Cwd: directory, Name: "writer child", Kind: "scout", Model: "fixture/child", Admitted: true, State: Completed, Latest: &outcome, NoticeID: details.Receipt, Notices: []savedNotice{{Nonce: details.Receipt, Outcome: outcome}}}
			entries := []map[string]any{{"id": "child", "customType": childEntryType, "data": saved}}
			if blockedAt == "ack" {
				entries = append(entries, map[string]any{"id": "promoted-receipt", "customType": "litter_completion", "details": details, "content": content})
			}
			state := &extensionState{}
			ext := newExtension(state)
			captured := make(chan *owner, 1)
			ext.OnEvent("writer_join", func(sdk.Context, map[string]any) (any, error) {
				previous := <-captured
				previous.tree.completionWG.Wait()
				return nil, nil
			})
			host, handlers, finish := startExtensionFixture(t, ext, &subprocess.ReadyPayload{Cwd: directory, Mode: "tui", Width: 100, Height: 32, State: &subprocess.StatePayload{HasUI: true}})
			defer finish()
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgNotify, Notify: &subprocess.NotifyPayload{Method: "run_signal", Args: json.RawMessage(`{"run":1,"active":true}`)}})
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "context", Request: &subprocess.RequestPayload{Method: "event", Event: "context", HandlerID: handlers["context"], Args: json.RawMessage(`{"messages":[]}`)}})
			retired := make(chan struct{})
			var previous *owner
			var barrier <-chan struct{}
			reads, oldWrites := 0, 0
			var serviceDone <-chan struct{}
			contextReturned := false
			answer := func(frame subprocess.Envelope) {
				reply := json.RawMessage(`{}`)
				switch frame.Call.Method {
				case "sessionRead":
					var query struct{ Method string }
					_ = json.Unmarshal(frame.Call.Args, &query)
					reply = json.RawMessage(`"fixture"`)
					if query.Method == "getEntries" {
						reply, _ = json.Marshal(entries)
					} else if query.Method == "getSessionFile" {
						reply = json.RawMessage(`null`)
					}
				case "appendEntry":
					oldWrites++
				case "isIdle":
					reply = json.RawMessage(`{"idle":false}`)
				case "sendMessage":
					oldWrites++
				default:
					t.Errorf("unexpected writer host action %s", frame.Call.Method)
				}
				if barrier != nil {
					select {
					case <-barrier:
						t.Fatal("old native writer crossed the full retirement barrier")
					default:
					}
				}
				writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: reply}})
			}
			for {
				frame := readCompletionEnvelope(t, host)
				if frame.Type == subprocess.MsgResponse && frame.ID == "context" {
					contextReturned = true
					if frame.Response.Error != nil {
						t.Fatal(frame.Response.Error)
					}
				}
				if frame.Type != subprocess.MsgCall {
					continue
				}
				var query struct{ Method string }
				_ = json.Unmarshal(frame.Call.Args, &query)
				if frame.Call.Method == "sessionRead" && query.Method == "getEntries" {
					reads++
				}
				block := blockedAt == "read" && reads == 2 && query.Method == "getEntries" || blockedAt == "ack" && frame.Call.Method == "appendEntry" || blockedAt == "send" && frame.Call.Method == "sendMessage"
				if !block {
					answer(frame)
					continue
				}
				state.mu.Lock()
				previous = state.owner
				state.mu.Unlock()
				if previous == nil {
					t.Fatal("writer was not admitted to an owner")
				}
				serviceDone = previous.services.Registry().ModelTaskContext(context.Background()).Done()
				go func() { state.retire(); close(retired) }()
				for {
					pushed := readCompletionEnvelope(t, host)
					if pushed.Type == subprocess.MsgWidgetPush && len(pushed.WidgetPush.Lines) == 0 {
						break
					}
				}
				state.mu.Lock()
				barrier = state.retiring
				state.mu.Unlock()
				if barrier == nil {
					t.Fatal("retirement never published its full barrier")
				}
				select {
				case <-barrier:
					t.Fatal("retirement skipped the blocked native writer")
				case <-serviceDone:
					t.Fatal("services closed beneath a blocked native writer")
				default:
				}
				answer(frame)
				break
			}
			joined := make(chan struct{})
			go func() { state.retire(); close(joined) }()
			captured <- previous
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "writer_join", Request: &subprocess.RequestPayload{Method: "event", Event: "writer_join", HandlerID: handlers["writer_join"], Args: json.RawMessage(`{}`)}})
			writerReturned := false
			for !contextReturned || !writerReturned {
				frame := readCompletionEnvelope(t, host)
				if frame.Type == subprocess.MsgCall {
					answer(frame)
				} else if frame.Type == subprocess.MsgResponse && frame.ID == "context" {
					contextReturned = true
					if frame.Response.Error != nil {
						t.Fatal(frame.Response.Error)
					}
				} else if frame.Type == subprocess.MsgResponse && frame.ID == "writer_join" {
					writerReturned = true
				}
			}
			<-retired
			<-joined
			<-serviceDone
			if blockedAt == "ack" && oldWrites != 1 || blockedAt == "send" && oldWrites != 1 || state.owner != nil {
				t.Fatalf("retired root writer retained effects or ownership: writes=%d owner=%p", oldWrites, state.owner)
			}
		})
	}
}

func TestNativeStorageDuringAnInflightSendWinsOverItsLateReply(t *testing.T) {
	for _, rejected := range []bool{false, true} {
		for _, consumed := range []bool{false, true} {
			t.Run(fmt.Sprintf("rejected=%t/consumed=%t", rejected, consumed), func(t *testing.T) {
				t.Setenv("PIG_OFFLINE", "1")
				t.Setenv("PIG_CODING_AGENT_DIR", t.TempDir())
				directory := t.TempDir()
				id := ChildID("00000000-0000-7000-8000-000000000001")
				outcome := Outcome{Run: Ref{id}, State: Completed, Model: "fixture/child", Text: "NATIVE_BEFORE_SEND_REPLY"}
				details := completionDetails{ChildID: id, Receipt: "inflight-receipt"}
				saved := savedChild{RootID: "fixture", ParentID: "fixture", ID: id, Cwd: directory, Name: "late send child", Kind: "scout", Model: "fixture/child", Admitted: true, State: Completed, Latest: &outcome, NoticeID: details.Receipt, Notices: []savedNotice{{Nonce: details.Receipt, Outcome: outcome}}}
				entries := []map[string]any{{"id": "retained-child", "customType": childEntryType, "data": saved}}
				state := &extensionState{}
				ext := newExtension(state)
				ext.OnEvent("receipt_drain", func(ctx sdk.Context, _ map[string]any) (any, error) {
					return state.observe(ctx, func(o *owner) (any, error) { o.tree.completionWG.Wait(); return nil, nil })
				})
				host, handlers, finish := startExtensionFixture(t, ext, &subprocess.ReadyPayload{Cwd: directory, Mode: "tui", Width: 100, Height: 32, State: &subprocess.StatePayload{HasUI: true}})
				defer state.retire()
				defer finish()
				answer := func(frame subprocess.Envelope) {
					reply := json.RawMessage(`{}`)
					switch frame.Call.Method {
					case "sessionRead":
						var query struct{ Method string }
						_ = json.Unmarshal(frame.Call.Args, &query)
						reply = json.RawMessage(`"fixture"`)
						if query.Method == "getEntries" {
							reply, _ = json.Marshal(entries)
						} else if query.Method == "getSessionFile" {
							reply = json.RawMessage(`null`)
						} else if query.Method == "getLeafId" {
							reply, _ = json.Marshal(entries[len(entries)-1]["id"])
						}
					case "appendEntry":
						var entry map[string]any
						_ = json.Unmarshal(frame.Call.Args, &entry)
						entry["id"] = fmt.Sprintf("saved-%d", len(entries))
						entries = append(entries, entry)
					case "isIdle":
						reply = json.RawMessage(`{"idle":false}`)
					default:
						t.Errorf("unexpected inflight host action %s", frame.Call.Method)
					}
					writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: reply}})
				}
				writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgNotify, Notify: &subprocess.NotifyPayload{Method: "run_signal", Args: json.RawMessage(`{"run":1,"active":true}`)}})
				writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "context", Request: &subprocess.RequestPayload{Method: "event", Event: "context", HandlerID: handlers["context"], Args: json.RawMessage(`{"messages":[]}`)}})
				var inflight subprocess.Envelope
				for {
					frame := readCompletionEnvelope(t, host)
					if frame.Type == subprocess.MsgCall {
						if frame.Call.Method == "sendMessage" {
							inflight = frame
							break
						}
						answer(frame)
					}
				}
				if consumed {
					params, _ := json.Marshal(map[string]any{"id": id})
					writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "native-wait", Request: &subprocess.RequestPayload{Method: "tool_call", Tool: "litter_wait", ToolCallID: "genuine-native-wait", Args: params}})
					for {
						frame := readCompletionEnvelope(t, host)
						if frame.Type == subprocess.MsgCall {
							answer(frame)
						} else if frame.Type == subprocess.MsgResponse && frame.ID == "native-wait" {
							var result struct {
								Content string
								IsError bool `json:"is_error"`
							}
							if err := json.Unmarshal(frame.Response.Result, &result); err != nil || result.IsError || !strings.Contains(result.Content, "NATIVE_BEFORE_SEND_REPLY") {
								t.Fatalf("real wait handler did not return its native outcome: %#v %v", result, err)
							}
							entries = append(entries, map[string]any{"id": "persisted-native-wait", "message": map[string]any{"role": "toolResult", "toolCallId": "genuine-native-wait", "toolName": "litter_wait", "content": []map[string]any{{"type": "text", "text": result.Content}}}})
							break
						}
					}
					writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "observed-native", Request: &subprocess.RequestPayload{Method: "event", Event: "context", HandlerID: handlers["context"], Args: json.RawMessage(`{"messages":[]}`)}})
				} else {
					var args struct{ Message map[string]any }
					_ = json.Unmarshal(inflight.Call.Args, &args)
					args.Message["id"], args.Message["role"] = "promoted-while-send-awaits-reply", "custom"
					entries = append(entries, args.Message)
					event, _ := json.Marshal(map[string]any{"message": args.Message})
					writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "observed-native", Request: &subprocess.RequestPayload{Method: "event", Event: "message_end", HandlerID: handlers["message_end"], Args: event}})
				}
				for {
					frame := readCompletionEnvelope(t, host)
					if frame.Type == subprocess.MsgCall {
						answer(frame)
					} else if frame.Type == subprocess.MsgResponse && frame.ID == "observed-native" {
						if frame.Response.Error != nil {
							t.Fatal(frame.Response.Error)
						}
						break
					}
				}
				o := state.owner
				selected := o.tree.records[id].run
				if len(o.tree.pending) != 0 || selected.deliveryReceipt != nil || len(o.tree.root.receipts) != 0 {
					t.Fatal("native storage waited for the send response before retiring its receipt")
				}
				reply := &subprocess.CallResultPayload{Result: json.RawMessage(`{}`)}
				if rejected {
					reply.Error = &subprocess.ErrorInfo{Message: "late rejected queue reply"}
				}
				writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: inflight.ID, CallResult: reply})
				writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "receipt_drain", Request: &subprocess.RequestPayload{Method: "event", Event: "receipt_drain", HandlerID: handlers["receipt_drain"], Args: json.RawMessage(`{}`)}})
				for {
					frame := readCompletionEnvelope(t, host)
					if frame.Type == subprocess.MsgCall {
						answer(frame)
					} else if frame.Type == subprocess.MsgResponse && frame.ID == "receipt_drain" {
						break
					}
				}
				inspection, _ := o.tree.inspect(id)
				if inspection.Outcome.State != Completed || inspection.Outcome.Error != "" || inspection.Outcome.Text != "NATIVE_BEFORE_SEND_REPLY" || len(o.tree.pending) != 0 {
					t.Fatalf("late reply downgraded native storage: %#v", inspection.Outcome)
				}
			})
		}
	}
}
