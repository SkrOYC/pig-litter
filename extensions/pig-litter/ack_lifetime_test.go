package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestRegisteredModelAdmissionWaitsForItsGenuineAcknowledgementAfterExecuteReturns(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		for _, settlement := range []string{"success", "failure", "cancel"} {
			t.Run(fmt.Sprintf("resume=%t/%s", resumed, settlement), func(t *testing.T) {
				t.Setenv("PIG_OFFLINE", "1")
				directory := t.TempDir()
				agentDir := filepath.Join(directory, "agent")
				t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
				var providerCalls atomic.Int32
				requests := make(chan string, 4)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
					body, _ := io.ReadAll(request.Body)
					requests <- string(body)
					providerCalls.Add(1)
					runtimeAnswer(w, "ACK_OWNED_NATIVE_RESULT")
				}))
				defer server.Close()
				writeRuntimeModels(t, agentDir, server.URL)
				state := &extensionState{}
				ext := newExtension(state)
				target := make(chan *run, 1)
				ext.OnEvent("wait_execution", func(sdk.Context, map[string]any) (any, error) {
					selected := <-target
					state.mu.Lock()
					o := state.owner
					state.mu.Unlock()
					outcome, err := o.tree.wait(context.Background(), selected, 3000)
					return map[string]any{"outcome": outcome}, err
				})
				host, handlers, finish := startExtensionFixture(t, ext, &subprocess.ReadyPayload{Cwd: directory, Mode: "rpc", Width: 80, State: &subprocess.StatePayload{HasUI: true}})
				defer state.retire()
				defer finish()
				runtime := &runtimeHost{tools: []string{"read", "ls"}}
				until := func(id string) json.RawMessage {
					for {
						frame := readCompletionEnvelope(t, host)
						if frame.Type == subprocess.MsgCall {
							result := json.RawMessage(`{}`)
							if frame.Call.Method != "sendMessage" && frame.Call.Method != "isIdle" {
								result = runtime.call(t, frame.Call.Method, frame.Call.Args)
							} else if frame.Call.Method == "isIdle" {
								result = json.RawMessage(`{"idle":false}`)
							}
							writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: result}})
						} else if frame.Type == subprocess.MsgResponse && frame.ID == id {
							if frame.Response.Error != nil {
								t.Fatal(frame.Response.Error)
							}
							return frame.Response.Result
						}
					}
				}
				execute := func(tool string, args map[string]any, callID, final string) *run {
					params, _ := json.Marshal(args)
					writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: callID, Request: &subprocess.RequestPayload{Method: "tool_call", Tool: tool, ToolCallID: callID, Args: params}})
					var returned struct {
						Content string
						IsError bool `json:"is_error"`
					}
					if err := json.Unmarshal(until(callID), &returned); err != nil || returned.IsError {
						t.Fatalf("registered Execute failed: %#v %v", returned, err)
					}
					var result struct{ Child Snapshot }
					if err := json.Unmarshal([]byte(returned.Content), &result); err != nil || result.Child.State != Starting {
						t.Fatalf("registered admission lost raw starting contract: %#v %v", result, err)
					}
					o := state.owner
					o.tree.mu.Lock()
					record := o.tree.records[result.Child.ID]
					if record == nil {
						o.tree.mu.Unlock()
						t.Fatal("normal Execute return cancelled provisional admission")
					}
					selected := record.run
					o.tree.mu.Unlock()
					select {
					case <-selected.done:
						t.Fatal("normal Execute return settled admission before native acknowledgement")
					case <-time.After(20 * time.Millisecond):
					}
					event := "tool_execution_end"
					payload := map[string]any{"toolCallId": callID, "toolName": tool, "isError": final == "failure", "result": map[string]any{"content": []map[string]any{{"type": "text", "text": returned.Content}}}}
					if final == "cancel" {
						event = "turn_end"
						payload = map[string]any{}
					}
					raw, _ := json.Marshal(payload)
					writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "native-final", Request: &subprocess.RequestPayload{Method: "event", Event: event, HandlerID: handlers[event], Args: raw}})
					until("native-final")
					target <- selected
					writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "wait_execution", Request: &subprocess.RequestPayload{Method: "event", Event: "wait_execution", HandlerID: handlers["wait_execution"], Args: json.RawMessage(`{}`)}})
					var settled struct{ Outcome Outcome }
					if err := json.Unmarshal(until("wait_execution"), &settled); err != nil {
						t.Fatal(err)
					}
					if final == "success" {
						if settled.Outcome.State != Completed || settled.Outcome.Text != "ACK_OWNED_NATIVE_RESULT" {
							t.Fatalf("genuine success did not activate native child: %#v", settled.Outcome)
						}
					} else if settled.Outcome.State != Stopped {
						t.Fatalf("genuine failure or cancellation did not reject admission: %#v", settled.Outcome)
					}
					return selected
				}
				args := map[string]any{"type": "scout", "task": "ORIGINAL_ACK_TASK", "model": "fixture/child"}
				tool, before := "litter_spawn", int32(0)
				if resumed {
					initial := execute(tool, args, "initial", "success")
					tool, before = "litter_message", 1
					args = map[string]any{"id": initial.ref.ID, "resume": true, "text": "GENUINE_RESUME_TEXT"}
					<-requests
				}
				execute(tool, args, "subject", settlement)
				want := before
				if settlement == "success" {
					want++
					body := <-requests
					if strings.Count(body, "ORIGINAL_ACK_TASK") != 1 || resumed && strings.Count(body, "GENUINE_RESUME_TEXT") != 1 {
						t.Fatalf("native acknowledgement changed accepted history: %s", body)
					}
				}
				if providerCalls.Load() != want {
					t.Fatalf("provisional or rejected admission inferred: calls=%d want=%d", providerCalls.Load(), want)
				}
			})
		}
	}
}

func TestRetirementJoinsAnAlreadyEnteredAcknowledgementCancellationWatcher(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, &runtimeHost{}, tree, directory, t.TempDir(), func(o *owner) {
		action, err := o.beginAction(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		selected := panelAdmission(t, tree, "joined cancellation watcher")
		ticket, err := action.scope.begin(nil, "joined-watcher", "litter_spawn")
		if err != nil {
			t.Fatal(err)
		}
		signal, entered, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var released sync.Once
		defer released.Do(func() { close(release) })
		o.deferActivation(ticket, selected, signal, func() bool { close(entered); <-release; return true }, "litter_spawn")
		close(signal)
		<-entered
		action.finish()
		services := o.services.Registry().ModelTaskContext(context.Background())
		retired := make(chan struct{})
		go func() { o.state.retire(); close(retired) }()
		<-o.actionContext.Done()
		select {
		case <-retired:
			t.Error("full retirement barrier passed an entered acknowledgement watcher")
		case <-services.Done():
			t.Error("services closed while an acknowledgement watcher still owned work")
		case <-time.After(20 * time.Millisecond):
		}
		released.Do(func() { close(release) })
		<-retired
		o.ackMu.Lock()
		remaining := len(o.acks)
		o.ackMu.Unlock()
		if services.Err() == nil || selected.outcome == nil || selected.outcome.State != Stopped || remaining != 0 {
			t.Fatalf("joined cancellation retained admission or services: outcome=%#v services=%v acks=%d", selected.outcome, services.Err(), remaining)
		}
	})
}
