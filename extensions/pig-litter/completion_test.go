package piglitter

import (
	"encoding/binary"
	"encoding/json"
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
		name, state string
		failed      bool
	}{
		{"busy", `{"idle":false}`, false},
		{"idle", `{"idle":true}`, false},
		{"state unavailable", "", true},
		{"missing idle state", `{}`, true},
		{"invalid idle state", `{"idle":"false"}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			host, extension := net.Pipe()
			defer host.Close()
			defer extension.Close()
			if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			run := Ref{"child:1", 2}
			tree := &Tree{pending: map[Ref]*reservation{run: {}}}
			ext := sdk.New("completion-test")
			ext.OnEvent("session_start", func(ctx sdk.Context, _ map[string]any) (any, error) {
				state := &extensionState{}
				o := &owner{state: state, sessionID: "fixture", context: ctx, tree: tree}
				state.owner = o
				o.sendCompletion(&record{}, Outcome{Run: run, State: Completed, Model: "fixture/child", Text: "child report"})
				return nil, nil
			})
			done := make(chan error, 1)
			go func() { done <- ext.RunWithConn(extension) }()
			if envelope := readCompletionEnvelope(t, host); envelope.Type != subprocess.MsgRegister {
				t.Fatalf("SDK registration: %#v", envelope)
			}
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgReady, Ready: &subprocess.ReadyPayload{Cwd: t.TempDir(), Width: 80}})
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "complete", Request: &subprocess.RequestPayload{Method: "event", Event: "session_start", HandlerID: 1, Args: json.RawMessage(`{}`)}})
			idleCalls, sends := 0, 0
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
					idleCalls++
					response.Result = json.RawMessage(test.state)
					if test.state == "" {
						response.Error = &subprocess.ErrorInfo{Message: "state unavailable"}
					}
				case "sendMessage":
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
					if test.name == "busy" && (args.Options.TriggerTurn != nil || args.Options.DeliverAs != "followUp") {
						t.Fatalf("busy completion options: %#v", args.Options)
					}
					if test.name == "idle" && (args.Options.TriggerTurn == nil || *args.Options.TriggerTurn || args.Options.DeliverAs != "") {
						t.Fatalf("idle completion options: %#v", args.Options)
					}
				default:
					t.Fatalf("unexpected host method %q", envelope.Call.Method)
				}
				writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: envelope.ID, CallResult: response})
			}
			wantSends := 1
			if test.failed {
				wantSends = 0
			}
			if idleCalls != 1 || sends != wantSends {
				t.Fatalf("state queries=%d sends=%d", idleCalls, sends)
			}
			if pending := tree.pending[run]; pending == nil || !pending.sent {
				t.Fatal("completion delivery released its reservation before observation")
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
