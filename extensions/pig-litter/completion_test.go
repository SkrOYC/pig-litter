package piglitter

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func readCompletionEnvelope(t *testing.T, connection net.Conn) subprocess.Envelope {
	t.Helper()
	var header [4]byte
	if _, err := io.ReadFull(connection, header[:]); err != nil {
		t.Fatal(err)
	}
	size := binary.BigEndian.Uint32(header[:])
	if size > subprocess.MaxFrameSize {
		t.Fatalf("oversized SDK frame: %d", size)
	}
	raw := make([]byte, size)
	if _, err := io.ReadFull(connection, raw); err != nil {
		t.Fatal(err)
	}
	var envelope subprocess.Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func writeCompletionEnvelope(t *testing.T, connection net.Conn, envelope subprocess.Envelope) {
	t.Helper()
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	if _, err := connection.Write(append(header[:], raw...)); err != nil {
		t.Fatal(err)
	}
}

func TestRootCompletionDeliveryUsesLiveStateAndPreservesReservation(t *testing.T) {
	for _, test := range []struct {
		name, state   string
		failed        bool
		observed      bool
		handoff       bool
		handoffReady  bool
		handoffFailed bool
		wrongTool     bool
		wrongRef      bool
		auditFailed   bool
	}{
		{name: "busy", state: `{"idle":false}`},
		{name: "idle", state: `{"idle":true}`},
		{name: "busy after observed outcome", state: `{"idle":false}`, observed: true},
		{name: "audit append failure", state: `{"idle":false}`, observed: true, auditFailed: true},
		{name: "busy with successful handoff", state: `{"idle":false}`, handoff: true, handoffReady: true},
		{name: "busy with failed handoff", state: `{"idle":false}`, handoff: true, handoffReady: true, handoffFailed: true},
		{name: "busy with nonterminal handoff", state: `{"idle":false}`, handoff: true},
		{name: "busy with mismatched tool", state: `{"idle":false}`, handoff: true, handoffReady: true, wrongTool: true},
		{name: "busy with mismatched outcome", state: `{"idle":false}`, handoff: true, handoffReady: true, wrongRef: true},
		{name: "state unavailable", failed: true},
		{name: "missing idle state", state: `{}`, failed: true},
		{name: "invalid idle state", state: `{"idle":"false"}`, failed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			consumed := test.observed || test.handoff && test.handoffReady && !test.handoffFailed && !test.wrongTool && !test.wrongRef
			host, extension := net.Pipe()
			defer host.Close()
			defer extension.Close()
			if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			run := Ref{"child:1", 2}
			tree := &Tree{ctx: context.Background(), pending: map[Ref]*reservation{run: {outcomeObserved: test.observed}}}
			ext := sdk.New("completion-test")
			var o *owner
			ext.OnEvent("session_start", func(ctx sdk.Context, _ map[string]any) (any, error) {
				state := &extensionState{}
				o = &owner{state: state, sessionID: "fixture", context: ctx, tree: tree, outcomeAcks: map[ackKey]*outcomeAcknowledgement{}}
				state.owner = o
				if test.handoff {
					key := ackKey{callID: "wait-result"}
					o.beginOutcomeAck(key, run, "litter_wait", &ctx)
					if test.handoffReady {
						o.readyOutcomeAck(key, true)
					}
				}
				o.sendCompletion(&record{}, Outcome{Run: run, State: Completed, Model: "fixture/child", Text: "child report"})
				return nil, nil
			})
			ext.OnEvent("message_end", func(_ sdk.Context, _ map[string]any) (any, error) {
				ref, tool := run, "litter_wait"
				if test.wrongTool {
					tool = "read"
				}
				if test.wrongRef {
					ref.Generation++
				}
				content, _ := resultContent("litter_wait", map[string]any{"outcome": &Outcome{Run: ref, State: Completed}}, nil)
				o.observeOutcome(Ref{}, tool, "wait-result", content, test.handoffFailed)
				return nil, nil
			})
			done := make(chan error, 1)
			go func() { done <- ext.RunWithConn(extension) }()
			if envelope := readCompletionEnvelope(t, host); envelope.Type != subprocess.MsgRegister {
				t.Fatalf("SDK registration: %#v", envelope)
			}
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgReady, Ready: &subprocess.ReadyPayload{Cwd: t.TempDir(), Width: 80}})
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "complete", Request: &subprocess.RequestPayload{Method: "event", Event: "session_start", HandlerID: 1, Args: json.RawMessage(`{}`)}})
			idleCalls, sends, sessionReads, audits := 0, 0, 0, 0
			for {
				envelope := readCompletionEnvelope(t, host)
				if envelope.Type == subprocess.MsgResponse && envelope.ID == "complete" {
					break
				}
				if envelope.Type != subprocess.MsgCall {
					continue
				}
				response := &subprocess.CallResultPayload{Result: json.RawMessage(`{}`)}
				switch envelope.Call.Method {
				case "sessionRead":
					sessionReads++
					response.Result = json.RawMessage(`"fixture"`)
				case "isIdle":
					idleCalls++
					response.Result = json.RawMessage(test.state)
					if test.state == "" {
						response.Error = &subprocess.ErrorInfo{Message: "state unavailable"}
					}
				case "appendEntry":
					audits++
					var args struct {
						CustomType string
						Data       completionNotice
					}
					if err := json.Unmarshal(envelope.Call.Args, &args); err != nil {
						t.Fatal(err)
					}
					if !consumed || args.CustomType != "litter_completion" || args.Data.ID != run.ID || args.Data.Generation != run.Generation || args.Data.State != Completed || args.Data.Report != "child report" || args.Data.ReportProvenance != "child_assistant" {
						t.Fatalf("consumed completion audit %#v", args)
					}
					if pending := tree.pending[run]; pending == nil || !pending.sent {
						t.Fatal("audit released reservation before append succeeded")
					}
					if test.auditFailed {
						response.Error = &subprocess.ErrorInfo{Message: "audit append failed"}
					}
				case "sendMessage":
					if consumed {
						t.Fatal("consumed outcome sent a model-visible message")
					}
					sends++
					var args struct {
						Message struct {
							CustomType string
							Content    string
							Display    bool
							Details    completionDetails
						}
						Options sdk.SendMessageOptions
					}
					if err := json.Unmarshal(envelope.Call.Args, &args); err != nil {
						t.Fatal(err)
					}
					if args.Message.CustomType != "litter_completion" || args.Message.Display || args.Message.Details != (completionDetails{run.ID, run.Generation}) {
						t.Fatalf("completion message changed: %#v", args.Message)
					}
					if test.state == `{"idle":false}` && !consumed && (args.Options.TriggerTurn != nil || args.Options.DeliverAs != "followUp") {
						t.Fatalf("busy completion options: %#v", args.Options)
					}
					if test.name == "idle" && (args.Options.TriggerTurn == nil || *args.Options.TriggerTurn || args.Options.DeliverAs != "") {
						t.Fatalf("idle completion options: %#v", args.Options)
					}
				default:
					t.Fatalf("unexpected host method %q", envelope.Call.Method)
				}
				writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: envelope.ID, CallResult: response})
				if test.handoff && envelope.Call.Method == "sessionRead" && sessionReads == 1 {
					writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "observe", Request: &subprocess.RequestPayload{Method: "event", Event: "message_end", HandlerID: 2, Args: json.RawMessage(`{}`)}})
				}
			}
			wantSends := 1
			if test.failed || consumed {
				wantSends = 0
			}
			wantIdle, wantAudits := 1, 0
			if consumed {
				wantIdle, wantAudits = 0, 1
			}
			if idleCalls != wantIdle || sends != wantSends || audits != wantAudits {
				t.Fatalf("state queries=%d sends=%d audits=%d", idleCalls, sends, audits)
			}
			pending := tree.pending[run]
			if consumed && !test.auditFailed {
				if pending != nil {
					t.Fatal("successful audit did not release its reservation")
				}
			} else if pending == nil || !pending.sent {
				t.Fatal("failed or unobserved delivery released its reservation")
			}
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgShutdown, Shutdown: &subprocess.ShutdownPayload{Reason: "test complete"}})
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("SDK did not shut down")
			}
		})
	}
}

func TestOutcomeHandoffCancellationAndAcknowledgmentKeepReservation(t *testing.T) {
	for _, operation := range []string{"litter_wait", "litter_stop"} {
		for _, received := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s received=%t", operation, received), func(t *testing.T) {
				tree := testTree(t, defaultConfig())
				child := testAdmission(t, tree, Ref{}, "scout", operation)
				o := &owner{tree: tree, outcomeAcks: map[ackKey]*outcomeAcknowledgement{}}
				key := ackKey{callID: operation}
				o.beginOutcomeAck(key, child.Ref, operation, nil)
				o.readyOutcomeAck(key, true)
				settled := make(chan bool, 1)
				go func() { settled <- o.awaitOutcomeAck(child.Ref) }()
				content, _ := resultContent(operation, map[string]any{"outcome": &Outcome{Run: child.Ref, State: Completed}}, nil)
				o.observeOutcome(Ref{}, operation, key.callID, content, !received)
				select {
				case observed := <-settled:
					if observed != received {
						t.Fatalf("handoff observation=%t, want %t", observed, received)
					}
				case <-time.After(time.Second):
					t.Fatal("handoff settlement did not release completion")
				}
				if pending := tree.pending[child.Ref]; pending == nil || pending.outcomeObserved != received {
					t.Fatalf("outcome handoff changed reservation %#v", pending)
				}
				o.ackMu.Lock()
				remaining := len(o.outcomeAcks)
				o.ackMu.Unlock()
				if remaining != 0 {
					t.Fatalf("settled handoff retained %d requests", remaining)
				}
			})
		}
	}
}

func TestCancelledWaitReleasesHandoffWithoutClaimingAnOutcome(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "waiting")
	o := &owner{tree: tree, outcomeAcks: map[ackKey]*outcomeAcknowledgement{}}
	key := ackKey{callID: "cancelled-wait"}
	o.beginOutcomeAck(key, child.Ref, "litter_wait", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := tree.wait(ctx, child.Ref, 1000)
	o.readyOutcomeAck(key, err == nil && outcome.State.terminal())
	if err == nil || err.Error() != "wait cancelled" || o.awaitOutcomeAck(child.Ref) {
		t.Fatalf("cancelled wait claimed completion %#v %v", outcome, err)
	}
	if pending := tree.pending[child.Ref]; pending == nil || pending.outcomeObserved {
		t.Fatalf("cancelled wait lost the completion reservation %#v", pending)
	}
	stopped, _, err := tree.stop(child.Ref)
	if err != nil || stopped == nil || stopped.State != Stopped {
		t.Fatalf("cancelled wait changed the child %#v %v", stopped, err)
	}
}

func TestUncorrelatedWaitCannotHoldCompletionDelivery(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "uncorrelated")
	o := &owner{tree: tree, outcomeAcks: map[ackKey]*outcomeAcknowledgement{}}
	key := ackKey{}
	o.beginOutcomeAck(key, child.Ref, "litter_wait", nil)
	o.readyOutcomeAck(key, true)
	settled := make(chan bool, 1)
	go func() { settled <- o.awaitOutcomeAck(child.Ref) }()
	select {
	case observed := <-settled:
		if observed {
			t.Fatal("an empty tool-call ID claimed an observed outcome")
		}
	case <-time.After(time.Second):
		t.Fatal("an empty tool-call ID held the completion")
	}
	if pending := tree.pending[child.Ref]; pending == nil || pending.outcomeObserved {
		t.Fatalf("uncorrelated wait changed completion reservation %#v", pending)
	}
	key.callID = "correlated"
	o.beginOutcomeAck(key, child.Ref, "litter_wait", nil)
	o.readyOutcomeAck(key, true)
	content, _ := resultContent("litter_wait", map[string]any{"outcome": &Outcome{Run: child.Ref, State: Completed}}, nil)
	o.observeOutcome(Ref{}, "litter_wait", key.callID, content, false)
	if !o.awaitOutcomeAck(child.Ref) {
		t.Fatal("the correlated terminal outcome was not observed")
	}
}
