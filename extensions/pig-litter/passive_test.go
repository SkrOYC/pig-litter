package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestFreshPassiveReopenPreservesUnconsumedReceiptsAndRestoredActions(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		runtimeAnswer(w, "RESTORED_NATIVE_RESULT")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: []string{"read", "ls"}}
	tree := testTree(t, defaultConfig())
	tree.root.id = "fixture"
	withRuntimeOwner(t, host, tree, directory, agentDir, func(old *owner) {
		value, ticket, err := runtimeOperation(t, old, "litter_spawn", "spawn", map[string]any{"type": "scout", "task": "retain native history", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		old.finishCall(ticket, nativeResult("litter_spawn", value, nil), false)
		selected := testRun(t, tree, value["child"].(Snapshot).Ref)
		if outcome, err := tree.wait(context.Background(), selected, 3000); err != nil || outcome.Text != "RESTORED_NATIVE_RESULT" {
			t.Fatalf("initial result %#v %v", outcome, err)
		}
		if len(tree.pending) != 1 || tree.pending[selected].outcomeObserved {
			t.Fatal("fixture did not retain an unconsumed completion receipt")
		}
		if result := awaitHuman(t, old.humanAction(context.Background(), selected, humanAction{kind: humanPause})); result.err != nil {
			t.Fatal(result.err)
		}
		old.state.retire()
		host.mu.Lock()
		before, _ := json.Marshal(host.entries)
		host.mu.Unlock()
		state := &extensionState{}
		passive, err := state.acquire(old.context, false)
		if err != nil {
			t.Fatal(err)
		}
		defer state.retire()
		restored := testRun(t, passive.tree, selected.ref)
		panel := newChildPanel(passive, nil, func() int { return 32 })
		panelKey(t, panel, "\r")
		detail := strings.Join(panel.Render(100), "\n")
		panelKey(t, panel, "v")
		report := strings.Join(panel.Render(100), "\n")
		panelKey(t, panel, "\x1b")
		panel.Dispose()
		if !strings.Contains(detail, "State paused") || !strings.Contains(detail, "Elapsed unknown") || !strings.Contains(detail, "Admission ready") || !strings.Contains(report, "RESTORED_NATIVE_RESULT") {
			t.Fatalf("restored zero-timing child was not inspectable: detail=%s report=%s", detail, report)
		}
		passiveAgain, err := state.acquire(old.context, false)
		if err != nil || passiveAgain != passive {
			t.Fatalf("passive reopen changed owners: %p %p %v", passive, passiveAgain, err)
		}
		host.mu.Lock()
		after, _ := json.Marshal(host.entries)
		host.mu.Unlock()
		passive.tree.mu.Lock()
		pending := passive.tree.pending[restored]
		untouched := pending != nil && !pending.outcomeObserved && !pending.sent && passive.tree.root.paused && restored.owner.paused && restored.progress.activated.IsZero() && restored.admission == admissionReady
		passive.tree.mu.Unlock()
		if string(before) != string(after) || !untouched || calls.Load() != 1 {
			t.Fatalf("fresh passive view changed receipts, metadata, pause, or inference: untouched=%t calls=%d before=%s after=%s", untouched, calls.Load(), before, after)
		}
		passive.tree.completion = nil
		result := awaitHuman(t, passive.humanAction(context.Background(), restored, humanAction{kind: humanResume, text: "resume restored zero-timing child"}))
		if result.err != nil || result.selected.ref.ID != selected.ref.ID {
			t.Fatalf("restored paused child was mistaken for provisional admission: %#v", result)
		}
		if _, err := passive.tree.selectOwned(nil, selected.ref.ID); err == nil {
			t.Fatal("human resume opened a restored owner's root model authority")
		}
		if outcome, err := passive.tree.wait(context.Background(), result.selected, 3000); err != nil || outcome.Text != "RESTORED_NATIVE_RESULT" || calls.Load() != 2 {
			t.Fatalf("restored native resume %#v calls=%d error=%v", outcome, calls.Load(), err)
		}
	})
}

func TestFirstPanelRequestCancellationKeepsCompletedSessionStartOwnerContext(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	t.Setenv("PIG_CODING_AGENT_DIR", t.TempDir())
	host, peer := net.Pipe()
	defer host.Close()
	defer peer.Close()
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	state := &extensionState{}
	defer state.retire()
	ext := newExtension(state)
	ext.OnEvent("probe_owner", func(_ sdk.Context, _ map[string]any) (any, error) {
		state.mu.Lock()
		o := state.owner
		state.mu.Unlock()
		if o == nil {
			return map[string]any{"current": false, "root": "missing"}, nil
		}
		return map[string]any{"current": o.current() == nil, "root": o.sessionID, "contextAlive": o.context.Err() == nil}, nil
	})
	done := make(chan error, 1)
	go func() { done <- ext.RunWithConn(peer) }()
	registration := readCompletionEnvelope(t, host)
	if registration.Type != subprocess.MsgRegister {
		t.Fatal(registration)
	}
	handlers := map[string]int{}
	for _, handler := range registration.Register.Handlers {
		handlers[handler.Event] = handler.HandlerID
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgReady, Ready: &subprocess.ReadyPayload{Cwd: t.TempDir(), Mode: "tui", Width: 100, Height: 32, State: &subprocess.StatePayload{HasUI: true}}})
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "session-start", Request: &subprocess.RequestPayload{Method: "event", Event: "session_start", HandlerID: handlers["session_start"], Args: json.RawMessage(`{}`)}})
	for {
		frame := readCompletionEnvelope(t, host)
		if frame.Type == subprocess.MsgResponse && frame.ID == "session-start" {
			if frame.Response.Error != nil {
				t.Fatal(frame.Response.Error)
			}
			break
		}
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "first-panel", Request: &subprocess.RequestPayload{Method: "command", Tool: "pig-litter", Args: json.RawMessage(`""`)}})
	opened, cancelled, probed := false, false, false
	for !probed {
		frame := readCompletionEnvelope(t, host)
		if frame.Type == subprocess.MsgCall {
			var result json.RawMessage
			switch frame.Call.Method {
			case "sessionRead":
				var query struct{ Method string }
				_ = json.Unmarshal(frame.Call.Args, &query)
				if query.Method == "getSessionId" {
					result = json.RawMessage(`"fixture"`)
				} else if query.Method == "getEntries" {
					result = json.RawMessage(`[]`)
				} else {
					t.Fatalf("unexpected passive session read %s", query.Method)
				}
			case "ui.custom":
				opened = true
				writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCancel, ID: "first-panel", Cancel: &subprocess.CancelPayload{RequestID: "first-panel", Reason: "cancel first modal request"}})
				continue
			default:
				t.Fatalf("passive command made an active host call %s", frame.Call.Method)
			}
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: result}})
		} else if frame.Type == subprocess.MsgResponse && frame.ID == "first-panel" {
			cancelled = true
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "probe", Request: &subprocess.RequestPayload{Method: "event", Event: "probe_owner", HandlerID: handlers["probe_owner"], Args: json.RawMessage(`{}`)}})
		} else if frame.Type == subprocess.MsgResponse && frame.ID == "probe" {
			var observed struct {
				Current, ContextAlive bool
				Root                  string
			}
			if err := json.Unmarshal(frame.Response.Result, &observed); err != nil || !observed.Current || !observed.ContextAlive || observed.Root != "fixture" {
				t.Fatalf("first command cancellation retired the owner lifetime: %#v %v", observed, err)
			}
			probed = true
		}
	}
	if !opened || !cancelled {
		t.Fatal("the first panel did not open and cancel through the real SDK")
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgShutdown, Shutdown: &subprocess.ShutdownPayload{Reason: "complete"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestScopedRecoveryNeedsGenuineCurrentActiveChildAndLeavesRootReceipt(t *testing.T) {
	tree := testTree(t, defaultConfig())
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read", "ls"}, CanDelegate: true}
	parent := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	nested := testAdmission(t, tree, parent.Ref, "scout", "nested")
	rootChild := testAdmission(t, tree, Ref{}, "scout", "root child")
	parentRun := testRun(t, tree, parent.Ref)
	nestedRun := testRun(t, tree, nested.Ref)
	rootRun := testRun(t, tree, rootChild.Ref)
	tree.finish(nestedRun.owner, nestedRun, Outcome{State: Completed, Text: "NESTED_RECOVERY_REPORT"})
	tree.finish(rootRun.owner, rootRun, Outcome{State: Completed, Text: "ROOT_MUST_STAY_PASSIVE"})
	tree.finish(parentRun.owner, parentRun, Outcome{State: Completed, Text: "parent report"})
	tree.acknowledge(parentRun)
	withCompletionHost(t, tree, func(o *owner) {
		o.recovered = o.recoveryBatch()
		o.startRecovery(parentRun)
		if len(o.recoveryParents) != 0 {
			t.Fatal("settled parent consumed restored completion recovery")
		}
		_, next, err := tree.resume(nil, parentRun, "genuine current parent")
		if err != nil {
			t.Fatal(err)
		}
		tree.execute = func(_ *record, run *run) Outcome {
			<-run.ctx.Done()
			return Outcome{State: Stopped}
		}
		o.startRecovery(next)
		if len(o.recoveryParents) != 0 {
			t.Fatal("provisional parent consumed restored completion recovery")
		}
		if err := tree.activate(next); err != nil {
			t.Fatal(err)
		}
		o.startRecovery(parentRun)
		if len(o.recoveryParents) != 0 {
			t.Fatal("stale parent's late TurnStart consumed recovery")
		}
		o.startRecovery(next)
		tree.completionWG.Wait()
		if !reflect.DeepEqual(o.recoveryParents, map[ChildID]bool{parent.ID: true}) {
			t.Fatalf("genuine child recovered another parent's mailbox: %#v", o.recoveryParents)
		}
		tree.mu.Lock()
		rootNotice := tree.pending[rootRun]
		rootUntouched := rootNotice != nil && !rootNotice.sent && !rootNotice.outcomeObserved
		tree.mu.Unlock()
		if !rootUntouched {
			t.Fatal("genuine child activity consumed the root receipt")
		}
		entries, _ := json.Marshal(next.owner.activeLog.Entries())
		if strings.Count(string(entries), "NESTED_RECOVERY_REPORT") != 1 || strings.Contains(string(entries), "ROOT_MUST_STAY_PASSIVE") {
			t.Fatalf("genuine child did not recover its native mailbox exactly once: %s", entries)
		}
		o.startRecovery(next)
		tree.completionWG.Wait()
		again, _ := json.Marshal(next.owner.activeLog.Entries())
		if string(entries) != string(again) {
			t.Fatal("the same genuine child turn replayed restored recovery")
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method != "sessionRead" {
			t.Fatalf("unexpected scoped recovery host call %s", method)
		}
		return json.RawMessage(`"fixture"`)
	})
}

func TestStorageInFlightHumanControlsCannotTakeModelAdmission(t *testing.T) {
	for _, resume := range []bool{false, true} {
		t.Run(fmt.Sprint(resume), func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			selected := panelAdmission(t, tree, "model admission")
			tree.execute = func(*record, *run) Outcome { return Outcome{State: Completed, Text: "MODEL_ACTIVATION"} }
			if resume {
				if err := tree.activate(selected); err != nil {
					t.Fatal(err)
				}
				if _, err := tree.wait(context.Background(), selected, 1000); err != nil {
					t.Fatal(err)
				}
				tree.acknowledge(selected)
				_, next, err := tree.resume(nil, selected, "model continuation")
				if err != nil {
					t.Fatal(err)
				}
				selected = next
			}
			entered, release := make(chan struct{}), make(chan struct{})
			tree.persist = func(*record) error { close(entered); <-release; return nil }
			withCompletionHost(t, tree, func(o *owner) {
				done := make(chan error, 1)
				go func() { done <- tree.activate(selected) }()
				<-entered
				for _, action := range []humanAction{{kind: humanPause}, {kind: humanMessage, mode: "steer", text: "not accepted"}, {kind: humanResume, text: "not accepted"}} {
					result := awaitHuman(t, o.humanAction(context.Background(), selected, action))
					if result.err == nil || !strings.Contains(result.err.Error(), "admission is pending") {
						t.Fatalf("storage-in-flight action took admission: %#v", result)
					}
				}
				tree.persist = nil
				close(release)
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				if outcome, err := tree.wait(context.Background(), selected, 1000); err != nil || outcome.Text != "MODEL_ACTIVATION" || outcome.State != Completed {
					t.Fatalf("storage-in-flight model admission failed to activate: %#v %v", outcome, err)
				}
			}, func(method string, _ json.RawMessage) json.RawMessage {
				if method != "sessionRead" {
					t.Fatalf("unexpected host call %s", method)
				}
				return json.RawMessage(`"fixture"`)
			})
		})
	}
}
