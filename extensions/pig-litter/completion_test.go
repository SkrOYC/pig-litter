package piglitter

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/coding"
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
			selected := &run{ref: Ref{"child:1"}}
			tree := &Tree{ctx: context.Background(), pending: map[*run]*reservation{selected: {outcomeObserved: test.observed}}}
			ext := sdk.New("completion-test")
			var o *owner
			ext.OnEvent("session_start", func(ctx sdk.Context, _ map[string]any) (any, error) {
				state := &extensionState{}
				o = &owner{state: state, sessionID: "fixture", context: ctx, tree: tree, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}}
				state.owner = o
				if test.handoff {
					key := &callTicket{callID: "wait-result"}
					o.beginOutcomeAck(key, selected, "litter_wait", &ctx)
					if test.handoffReady {
						o.readyOutcomeAck(key, true)
					}
				}
				o.sendCompletion(&record{}, selected, Outcome{Run: selected.ref, State: Completed, Model: "fixture/child", Text: "child report"})
				return nil, nil
			})
			ext.OnEvent("message_end", func(_ sdk.Context, _ map[string]any) (any, error) {
				ref, tool := selected.ref, "litter_wait"
				if test.wrongTool {
					tool = "read"
				}
				if test.wrongRef {
					ref.ID = "other-child"
				}
				content, _ := resultContent("litter_wait", map[string]any{"outcome": &Outcome{Run: ref, State: Completed}}, nil)
				testObserveOutcome(o, nil, tool, "wait-result", content, test.handoffFailed)
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
					var query struct{ Method string }
					if err := json.Unmarshal(envelope.Call.Args, &query); err != nil {
						t.Fatal(err)
					}
					if query.Method == "getEntries" {
						response.Result = json.RawMessage(`[]`)
					}
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
					if !consumed || args.CustomType != "litter_completion" || args.Data.ID != selected.ref.ID || args.Data.State != Completed || args.Data.Report != "child report" || args.Data.ReportProvenance != "child_assistant" {
						t.Fatalf("consumed completion audit %#v", args)
					}
					if pending := tree.pending[selected]; pending == nil || !pending.sent {
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
					if args.Message.CustomType != "litter_completion" || args.Message.Display || args.Message.Details != (completionDetails{ChildID: selected.ref.ID}) {
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
			pending := tree.pending[selected]
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
				o := &owner{tree: tree, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}}
				key := &callTicket{callID: operation}
				o.beginOutcomeAck(key, testRun(t, tree, child.Ref), operation, nil)
				o.readyOutcomeAck(key, true)
				settled := make(chan bool, 1)
				go func() { settled <- o.awaitOutcomeAck(testRun(t, tree, child.Ref)) }()
				content, _ := resultContent(operation, map[string]any{"outcome": &Outcome{Run: child.Ref, State: Completed}}, nil)
				testObserveOutcome(o, nil, operation, key.callID, content, !received)
				select {
				case observed := <-settled:
					if observed != received {
						t.Fatalf("handoff observation=%t, want %t", observed, received)
					}
				case <-time.After(time.Second):
					t.Fatal("handoff settlement did not release completion")
				}
				if pending := tree.pending[testRun(t, tree, child.Ref)]; pending == nil || pending.outcomeObserved != received {
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
	o := &owner{tree: tree, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}}
	key := &callTicket{callID: "cancelled-wait"}
	o.beginOutcomeAck(key, testRun(t, tree, child.Ref), "litter_wait", nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	outcome, err := tree.wait(ctx, testRun(t, tree, child.Ref), 1000)
	o.readyOutcomeAck(key, err == nil && outcome.State.terminal())
	if err == nil || err.Error() != "wait cancelled" || o.awaitOutcomeAck(testRun(t, tree, child.Ref)) {
		t.Fatalf("cancelled wait claimed completion %#v %v", outcome, err)
	}
	if pending := tree.pending[testRun(t, tree, child.Ref)]; pending == nil || pending.outcomeObserved {
		t.Fatalf("cancelled wait lost the completion reservation %#v", pending)
	}
	stopped, _, err := tree.stop(nil, testRun(t, tree, child.Ref))
	if err != nil || stopped == nil || stopped.State != Paused {
		t.Fatalf("cancelled wait changed the child %#v %v", stopped, err)
	}
}

func TestUncorrelatedWaitCannotHoldCompletionDelivery(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "uncorrelated")
	o := &owner{tree: tree, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}}
	key := &callTicket{}
	o.beginOutcomeAck(key, testRun(t, tree, child.Ref), "litter_wait", nil)
	o.readyOutcomeAck(key, true)
	settled := make(chan bool, 1)
	go func() { settled <- o.awaitOutcomeAck(testRun(t, tree, child.Ref)) }()
	select {
	case observed := <-settled:
		if observed {
			t.Fatal("an empty tool-call ID claimed an observed outcome")
		}
	case <-time.After(time.Second):
		t.Fatal("an empty tool-call ID held the completion")
	}
	if pending := tree.pending[testRun(t, tree, child.Ref)]; pending == nil || pending.outcomeObserved {
		t.Fatalf("uncorrelated wait changed completion reservation %#v", pending)
	}
	key.callID = "correlated"
	o.beginOutcomeAck(key, testRun(t, tree, child.Ref), "litter_wait", nil)
	o.readyOutcomeAck(key, true)
	content, _ := resultContent("litter_wait", map[string]any{"outcome": &Outcome{Run: child.Ref, State: Completed}}, nil)
	testObserveOutcome(o, nil, "litter_wait", key.callID, content, false)
	if !o.awaitOutcomeAck(testRun(t, tree, child.Ref)) {
		t.Fatal("the correlated terminal outcome was not observed")
	}
}

func TestOutcomeRegistrationPrecedesCompletionDecision(t *testing.T) {
	for _, operation := range []string{"litter_wait", "litter_stop"} {
		t.Run(operation, func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			child := testAdmission(t, tree, Ref{}, "scout", operation)
			selected := testRun(t, tree, child.Ref)
			reserved, release, registered := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var releaseOnce sync.Once
			releaseRegistration := func() { releaseOnce.Do(func() { close(release) }) }
			defer releaseRegistration()
			o := &owner{tree: tree, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}, afterOutcomeReservation: func() {
				close(reserved)
				<-release
			}}
			key := &callTicket{callID: "outcome"}
			go func() {
				o.beginOutcomeAck(key, selected, operation, nil)
				close(registered)
			}()
			<-reserved
			decision := make(chan bool, 1)
			started := make(chan struct{})
			go func() {
				close(started)
				decision <- o.awaitOutcomeAck(selected)
			}()
			<-started
			select {
			case observed := <-decision:
				t.Fatalf("completion chose delivery during %s registration: observed=%t", operation, observed)
			case <-time.After(50 * time.Millisecond):
			}
			releaseRegistration()
			<-registered
			o.readyOutcomeAck(key, true)
			content, _ := resultContent(operation, map[string]any{"outcome": &Outcome{Run: child.Ref, State: Completed}}, nil)
			testObserveOutcome(o, nil, operation, key.callID, content, false)
			select {
			case observed := <-decision:
				if !observed {
					t.Fatal("registered outcome selected an automatic duplicate completion")
				}
			case <-time.After(time.Second):
				t.Fatal("observed outcome did not release completion delivery")
			}
		})
	}
}

func TestCompletionAuditAfterSettlementBetweenObservationAndAckRead(t *testing.T) {
	host, extension := net.Pipe()
	defer host.Close()
	defer extension.Close()
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ref := Ref{"child:1"}
	selected := &run{ref: ref}
	tree := &Tree{ctx: context.Background(), pending: map[*run]*reservation{selected: {}}}
	snapshot, release := make(chan struct{}), make(chan struct{})
	var gate sync.Once
	var o *owner
	ext := sdk.New("interleaving-test")
	ext.OnEvent("session_start", func(ctx sdk.Context, _ map[string]any) (any, error) {
		state := &extensionState{}
		o = &owner{state: state, sessionID: "fixture", context: ctx, tree: tree, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}, afterOutcomeSnapshot: func() {
			gate.Do(func() { close(snapshot); <-release })
		}}
		state.owner = o
		key := &callTicket{callID: "wait-result"}
		o.beginOutcomeAck(key, selected, "litter_wait", &ctx)
		o.readyOutcomeAck(key, true)
		o.sendCompletion(&record{}, selected, Outcome{Run: ref, State: Completed, Model: "fixture/child", Text: "verified child report"})
		return nil, nil
	})
	ext.OnEvent("message_end", func(_ sdk.Context, data map[string]any) (any, error) {
		message := data["message"].(map[string]any)
		testObserveOutcome(o, nil, message["toolName"].(string), message["toolCallId"].(string), message["content"].(string), message["isError"].(bool))
		return nil, nil
	})
	done := make(chan error, 1)
	go func() { done <- ext.RunWithConn(extension) }()
	if envelope := readCompletionEnvelope(t, host); envelope.Type != subprocess.MsgRegister {
		t.Fatalf("SDK registration %#v", envelope)
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgReady, Ready: &subprocess.ReadyPayload{Cwd: t.TempDir(), Width: 80}})
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "complete", Request: &subprocess.RequestPayload{Method: "event", Event: "session_start", HandlerID: 1, Args: json.RawMessage(`{}`)}})
	first := readCompletionEnvelope(t, host)
	for first.Type != subprocess.MsgCall {
		first = readCompletionEnvelope(t, host)
	}
	if first.Type != subprocess.MsgCall || first.Call.Method != "sessionRead" {
		t.Fatalf("completion startup %#v", first)
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: first.ID, CallResult: &subprocess.CallResultPayload{Result: json.RawMessage(`"fixture"`)}})
	if envelope := readCompletionEnvelope(t, host); envelope.Type != subprocess.MsgRequestState {
		t.Fatalf("SDK call state %#v", envelope)
	}
	select {
	case <-snapshot:
	case <-time.After(time.Second):
		t.Fatal("completion did not reach the observation gate")
	}
	content, _ := resultContent("litter_wait", map[string]any{"outcome": &Outcome{Run: ref, State: Completed}}, nil)
	message, err := json.Marshal(map[string]any{"message": map[string]any{"role": "toolResult", "toolName": "litter_wait", "toolCallId": "wait-result", "content": content, "isError": false}})
	if err != nil {
		t.Fatal(err)
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "observe", Request: &subprocess.RequestPayload{Method: "event", Event: "message_end", HandlerID: 2, Args: message}})
	for {
		envelope := readCompletionEnvelope(t, host)
		if envelope.Type == subprocess.MsgResponse && envelope.ID == "observe" {
			break
		}
		if envelope.Type == subprocess.MsgCall {
			t.Fatalf("SDK call before settlement %#v", envelope)
		}
	}
	o.ackMu.Lock()
	remaining := len(o.outcomeAcks)
	o.ackMu.Unlock()
	tree.mu.Lock()
	observed := tree.pending[selected].outcomeObserved
	tree.mu.Unlock()
	if !observed || remaining != 0 {
		t.Fatalf("handoff did not settle before releasing gate: observed=%t remaining=%d", observed, remaining)
	}
	close(release)
	audits, sends := 0, 0
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
			response.Result = json.RawMessage(`"fixture"`)
		case "isIdle":
			response.Result = json.RawMessage(`{"idle":false}`)
		case "sendMessage":
			sends++
		case "appendEntry":
			audits++
			var args struct {
				CustomType string
				Data       completionNotice
			}
			if err := json.Unmarshal(envelope.Call.Args, &args); err != nil {
				t.Fatal(err)
			}
			if args.CustomType != "litter_completion" || args.Data.ID != "child:1" || args.Data.State != Completed || args.Data.Report != "verified child report" {
				t.Fatalf("audit payload %#v", args)
			}
			tree.mu.Lock()
			pending := tree.pending[selected]
			tree.mu.Unlock()
			if pending == nil || !pending.sent {
				t.Fatal("reservation released before successful audit append")
			}
		default:
			t.Fatalf("unexpected SDK call %q", envelope.Call.Method)
		}
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: envelope.ID, CallResult: response})
	}
	tree.mu.Lock()
	pending := tree.pending[selected]
	tree.mu.Unlock()
	if audits != 1 || sends != 0 || pending != nil {
		t.Fatalf("interleaved completion audits=%d sends=%d reservation=%#v", audits, sends, pending)
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgShutdown, Shutdown: &subprocess.ShutdownPayload{Reason: "test complete"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestIdenticalCompletionsRequireNewSDKEntries(t *testing.T) {
	host, extension := net.Pipe()
	defer host.Close()
	defer extension.Close()
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	first := &run{ref: Ref{"child:1"}, noticeID: "first-receipt"}
	second := &run{ref: first.ref, noticeID: "second-receipt"}
	child := &record{id: first.ref.ID, run: first}
	tree := &Tree{ctx: context.Background(), records: map[ChildID]*record{child.id: child}, pending: map[*run]*reservation{first: {nonce: first.noticeID}, second: {nonce: second.noticeID}}}
	var o *owner
	ext := sdk.New("repeated-completion-test")
	ext.OnEvent("exercise", func(ctx sdk.Context, data map[string]any) (any, error) {
		if o == nil {
			state := &extensionState{}
			o = &owner{state: state, sessionID: "fixture", context: ctx, tree: tree, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}}
			state.owner = o
		}
		switch data["action"] {
		case "first":
			o.sendCompletion(child, first, Outcome{Run: first.ref, State: Completed, Text: "same report"})
		case "second":
			tree.mu.Lock()
			child.run = second
			tree.mu.Unlock()
			o.sendCompletion(child, second, Outcome{Run: second.ref, State: Completed, Text: "same report"})
		case "observe", "observeSecond":
			nonce := first.noticeID
			if data["action"] == "observeSecond" {
				nonce = second.noticeID
			}
			o.observeCompletion(nil, map[string]any{"customType": "litter_completion", "details": completionDetails{ChildID: child.id, Receipt: nonce}})
		}
		return nil, nil
	})
	done := make(chan error, 1)
	go func() { done <- ext.RunWithConn(extension) }()
	if envelope := readCompletionEnvelope(t, host); envelope.Type != subprocess.MsgRegister {
		t.Fatalf("registration %#v", envelope)
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgReady, Ready: &subprocess.ReadyPayload{Cwd: t.TempDir(), Width: 80}})
	entries := []map[string]any{}
	var sent string
	var sentDetails completionDetails
	exercise := func(action string) {
		t.Helper()
		args, _ := json.Marshal(map[string]string{"action": action})
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: action, Request: &subprocess.RequestPayload{Method: "event", Event: "exercise", HandlerID: 1, Args: args}})
		for {
			envelope := readCompletionEnvelope(t, host)
			if envelope.Type == subprocess.MsgResponse && envelope.ID == action {
				return
			}
			if envelope.Type != subprocess.MsgCall {
				continue
			}
			response := &subprocess.CallResultPayload{Result: json.RawMessage(`{}`)}
			switch envelope.Call.Method {
			case "sessionRead":
				var query struct{ Method string }
				if err := json.Unmarshal(envelope.Call.Args, &query); err != nil {
					t.Fatal(err)
				}
				response.Result = json.RawMessage(`"fixture"`)
				if query.Method == "getEntries" {
					response.Result, _ = json.Marshal(entries)
				}
			case "isIdle":
				response.Result = json.RawMessage(`{"idle":true}`)
			case "sendMessage":
				var args struct {
					Message struct {
						Content string
						Details completionDetails
					}
				}
				if err := json.Unmarshal(envelope.Call.Args, &args); err != nil {
					t.Fatal(err)
				}
				sent, sentDetails = args.Message.Content, args.Message.Details
			default:
				t.Fatalf("unexpected host method %q", envelope.Call.Method)
			}
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: envelope.ID, CallResult: response})
		}
	}
	appendNotice := func(id string) {
		entries = append(entries, map[string]any{"id": id, "type": "custom_message", "customType": "litter_completion", "content": sent, "details": sentDetails})
	}
	exercise("first")
	exercise("observe")
	if tree.pending[first] == nil {
		t.Fatal("enqueue or callback acknowledged an unappended completion")
	}
	appendNotice("sdk-first")
	exercise("observe")
	if tree.pending[first] != nil || tree.pending[second] == nil {
		t.Fatal("first persisted entry did not acknowledge exactly its receipt")
	}
	exercise("second")
	exercise("observe")
	if tree.pending[second] == nil {
		t.Fatal("a duplicate old callback acknowledged the new completion")
	}
	appendNotice("sdk-second")
	exercise("observe")
	if tree.pending[second] == nil {
		t.Fatal("old callback acknowledged a new stored receipt")
	}
	exercise("observeSecond")
	if tree.pending[second] != nil {
		t.Fatal("new SDK entry did not acknowledge the identical resumed report")
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgShutdown, Shutdown: &subprocess.ShutdownPayload{Reason: "complete"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestNestedCompletionFollowsParentLifecycleAndReleasesReceipt(t *testing.T) {
	for _, parentState := range []string{"running", "starting", "construction failed", "disposed", "owner cancelled", "consumed"} {
		t.Run(parentState, func(t *testing.T) { testNestedCompletion(t, parentState) })
	}
}

func testNestedCompletion(t *testing.T, parentState string) {
	t.Helper()
	t.Setenv("PIG_OFFLINE", "1")
	dir := t.TempDir()
	models := `{"providers":{"fixture":{"api":"openai-completions","baseUrl":"http://127.0.0.1:1/v1","apiKey":"fixture-only","models":[{"id":"child","name":"child","contextWindow":32000,"maxTokens":1024}]}}}`
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	services, err := coding.NewServices(coding.ServicesOptions{CWD: dir, AgentDir: dir})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, AbortContext: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	manager, err := coding.NewInMemorySessionManager(dir)
	if err != nil {
		t.Fatal(err)
	}
	session, err := runtime.New(coding.SessionStartOptions{SessionManager: manager, Model: services.ModelRuntime().GetModel("fixture", "child"), SystemPrompt: "fixture", ResourceLoader: coding.NoResources, SkipBuiltinTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	go func() {
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	unrelated := &run{ref: Ref{"other-child"}}
	unrelatedReceipt := &completionReceipt{}
	parentRun := &run{ref: Ref{"parent"}, ready: make(chan struct{}), done: make(chan struct{})}
	if parentState == "running" {
		parentRun.session = session
		close(parentRun.ready)
	}
	if parentState == "disposed" {
		parentRun.disposing = true
		if err := session.Close(); err != nil {
			t.Fatal(err)
		}
		close(parentRun.done)
	}
	liveDelivery := parentState == "running"
	treeContext, cancelTree := context.WithCancel(context.Background())
	defer cancelTree()
	selected := &run{ref: Ref{"child"}}
	parent := &record{id: "parent", run: parentRun, history: manager, receipts: map[*run]*completionReceipt{unrelated: unrelatedReceipt}}
	parentRun.owner = parent
	child := &record{id: "child", parent: parent, run: selected}
	selected.owner = child
	tree := &Tree{ctx: treeContext, records: map[ChildID]*record{"child": child, "parent": parent}, pending: map[*run]*reservation{selected: {parent: "parent", outcomeObserved: parentState == "consumed"}}}
	entered, release := make(chan struct{}), make(chan struct{})
	var o *owner
	unsubscribe := session.Subscribe(func(event agent.AgentEvent) {
		if event, ok := event.(agent.MessageEndEvent); ok && event.Message.Custom != nil {
			close(entered)
			<-release
			o.observeCompletion(parentRun, event.Message.Custom)
		}
	})
	defer unsubscribe()
	host, extension := net.Pipe()
	defer host.Close()
	defer extension.Close()
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	ext := sdk.New("nested-completion-test")
	ext.OnEvent("exercise", func(ctx sdk.Context, _ map[string]any) (any, error) {
		state := &extensionState{}
		o = &owner{state: state, sessionID: "fixture", context: ctx, tree: tree, config: defaultConfig()}
		state.owner = o
		if parentState == "owner cancelled" {
			cancelTree()
		}
		o.sendCompletion(child, selected, Outcome{Run: selected.ref, State: Completed, Text: "nested report"})
		return nil, nil
	})
	done := make(chan error, 1)
	go func() { done <- ext.RunWithConn(extension) }()
	if envelope := readCompletionEnvelope(t, host); envelope.Type != subprocess.MsgRegister {
		t.Fatalf("registration %#v", envelope)
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgReady, Ready: &subprocess.ReadyPayload{Cwd: dir, Width: 80}})
	checked := make(chan bool, 1)
	if liveDelivery {
		go func() {
			<-entered
			tree.mu.Lock()
			acquired := make(chan struct{})
			go func() {
				parentRun.mu.Lock()
				parentRun.mu.Unlock()
				close(acquired)
			}()
			free := false
			select {
			case <-acquired:
				free = true
			case <-time.After(time.Second):
			}
			tree.mu.Unlock()
			close(release)
			<-acquired
			checked <- free
		}()
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "exercise", Request: &subprocess.RequestPayload{Method: "event", Event: "exercise", HandlerID: 1, Args: json.RawMessage(`{}`)}})
	for {
		envelope := readCompletionEnvelope(t, host)
		if envelope.Type == subprocess.MsgResponse && envelope.ID == "exercise" {
			break
		}
		if envelope.Type != subprocess.MsgCall {
			continue
		}
		if envelope.Call.Method != "sessionRead" {
			t.Fatalf("unexpected host method %q", envelope.Call.Method)
		}
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: envelope.ID, CallResult: &subprocess.CallResultPayload{Result: json.RawMessage(`"fixture"`)}})
	}
	if liveDelivery && !<-checked {
		t.Fatal("nested completion held the parent state lock across its synchronous callback")
	}
	if parentState == "owner cancelled" {
		if tree.pending[selected] == nil {
			t.Fatal("owner cancellation acknowledged an undelivered completion")
		}
	} else {
		if tree.pending[selected] != nil {
			t.Fatal("nested completion did not acknowledge its actual retained entry")
		}
		if selected.deliveryReceipt != nil {
			t.Fatal("completed child retained its delivery receipt")
		}
	}
	if len(parent.receipts) != 1 || parent.receipts[unrelated] != unrelatedReceipt {
		t.Fatalf("nested receipt retirement changed unrelated receipts: %#v", parent.receipts)
	}
	if liveDelivery {
		select {
		case <-parentRun.done:
			t.Fatal("live parent ended before receiving the child completion")
		default:
		}
	}
	entries, err := rawEntries(manager.Entries())
	if err != nil {
		t.Fatal(err)
	}
	notice, err := completionContent(Outcome{Run: selected.ref, State: Completed, Text: "nested report"})
	if err != nil {
		t.Fatal(err)
	}
	if got := completionStored(entries, completionDetails{ChildID: child.id}, notice); got != (parentState != "owner cancelled" && parentState != "consumed") {
		t.Fatalf("persisted nested completion=%t for parent %s", got, parentState)
	}
	if parentState == "consumed" {
		found := false
		for _, raw := range entries {
			var entry struct {
				Type, CustomType string
				Data             completionNotice
			}
			if json.Unmarshal(raw, &entry) == nil && entry.Type == "custom" && entry.CustomType == "litter_completion" && entry.Data.Report == "nested report" {
				found = true
			}
		}
		if !found {
			t.Fatal("consumed nested result was not stored as an audit entry")
		}
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgShutdown, Shutdown: &subprocess.ShutdownPayload{Reason: "complete"}})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func testObserveOutcome(o *owner, caller *run, operation, callID, content string, failed bool) {
	o.ackMu.Lock()
	var ticket *callTicket
	for key := range o.outcomeAcks {
		if key.caller == caller && key.callID == callID {
			ticket = key
			break
		}
	}
	o.ackMu.Unlock()
	if ticket != nil {
		o.observeOutcome(ticket, operation, content, failed)
	}
}
