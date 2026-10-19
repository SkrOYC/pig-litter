package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func startExtensionFixture(t *testing.T, ext *sdk.Extension, ready *subprocess.ReadyPayload) (net.Conn, map[string]int, func()) {
	t.Helper()
	host, peer := net.Pipe()
	if err := host.SetDeadline(time.Now().Add(10 * time.Second)); err != nil {
		t.Fatal(err)
	}
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
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgReady, Ready: ready})
	if handler := handlers["session_start"]; handler != 0 {
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "fixture-start", Request: &subprocess.RequestPayload{Method: "event", Event: "session_start", HandlerID: handler, Args: json.RawMessage(`{}`)}})
		for {
			frame := readCompletionEnvelope(t, host)
			if frame.Type == subprocess.MsgResponse && frame.ID == "fixture-start" {
				if frame.Response.Error != nil {
					t.Fatal(frame.Response.Error)
				}
				break
			}
		}
	}
	return host, handlers, func() {
		defer host.Close()
		defer peer.Close()
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgShutdown, Shutdown: &subprocess.ShutdownPayload{Reason: "complete"}})
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
}

func TestPanelCommandSanitizesAcquisitionAndCustomErrorsOnlyAtHumanBoundary(t *testing.T) {
	for _, source := range []string{"config", "custom"} {
		t.Run(source, func(t *testing.T) {
			t.Setenv("PIG_OFFLINE", "1")
			t.Setenv("PIG_CODING_AGENT_DIR", t.TempDir())
			directory := t.TempDir()
			if source == "config" {
				if err := os.WriteFile(filepath.Join(directory, "litter.yaml"), []byte("\"\\e[2Jbad\\u202e\": 1\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			state := &extensionState{}
			defer state.retire()
			host, _, finish := startExtensionFixture(t, newExtension(state), &subprocess.ReadyPayload{Cwd: directory, Mode: "tui", Width: 100, Height: 32, State: &subprocess.StatePayload{HasUI: true}})
			defer finish()
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "open", Request: &subprocess.RequestPayload{Method: "command", Tool: "pig-litter", Args: json.RawMessage(`""`)}})
			for {
				frame := readCompletionEnvelope(t, host)
				if frame.Type == subprocess.MsgResponse && frame.ID == "open" {
					if frame.Response.Error == nil {
						t.Fatal("hostile command error was reported as success")
					}
					message := frame.Response.Error.Message
					if strings.ContainsAny(message, "\x1b\x00\u202e\r\n") || strings.Contains(message, "LEAK") || !utf8.ValidString(message) {
						t.Fatalf("unsafe native command error: %q", message)
					}
					if source == "config" && !strings.Contains(message, "unknown setting bad") || source == "custom" && !strings.Contains(message, "Custom failed") {
						t.Fatalf("sanitization erased the human failure: %q", message)
					}
					break
				}
				if frame.Type != subprocess.MsgCall {
					continue
				}
				response := &subprocess.CallResultPayload{}
				switch frame.Call.Method {
				case "sessionRead":
					var query struct{ Method string }
					_ = json.Unmarshal(frame.Call.Args, &query)
					response.Result = json.RawMessage(`"fixture"`)
					if query.Method == "getEntries" {
						response.Result = json.RawMessage(`[]`)
					}
				case "ui.custom":
					response.Error = &subprocess.ErrorInfo{Message: "Custom \x1b]52;c;LEAK\a\x1b[2Jfailed\u202e\xff\nnext"}
				default:
					t.Fatalf("unexpected panel command host action %s", frame.Call.Method)
				}
				writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: response})
			}
			machine := errors.New("native\x1b[2Jerror\u202e")
			encoded, _ := resultContent("litter_list", nil, machine)
			var result struct{ Report string }
			if err := json.Unmarshal([]byte(encoded), &result); err != nil || result.Report != machine.Error() {
				t.Fatalf("human sanitization changed machine errors: %#v %v", result, err)
			}
		})
	}
}

func TestRPCPanelCommandReturnsGuidanceAndKeepsHelpWithoutOpeningCustom(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	state := &extensionState{}
	host, _, finish := startExtensionFixture(t, newExtension(state), &subprocess.ReadyPayload{Cwd: t.TempDir(), Mode: "rpc", Width: 80, State: &subprocess.StatePayload{HasUI: true}})
	defer finish()
	for _, args := range []string{"", "help"} {
		raw, _ := json.Marshal(args)
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "command", Request: &subprocess.RequestPayload{Method: "command", Tool: "pig-litter", Args: raw}})
		guidance := ""
		for {
			frame := readCompletionEnvelope(t, host)
			if frame.Type == subprocess.MsgResponse && frame.ID == "command" {
				if frame.Response.Error != nil {
					t.Fatal(frame.Response.Error)
				}
				break
			}
			if frame.Type != subprocess.MsgCall {
				continue
			}
			if frame.Call.Method != "ui.notify" {
				t.Fatalf("RPC command tried to open or infer through %s", frame.Call.Method)
			}
			var notification struct{ Message string }
			_ = json.Unmarshal(frame.Call.Args, &notification)
			guidance = notification.Message
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: json.RawMessage(`{}`)}})
		}
		if args == "" && !strings.Contains(guidance, "terminal UI") || args == "help" && !strings.Contains(guidance, "Use litter_list") {
			t.Fatalf("RPC guidance for %q was lost: %q", args, guidance)
		}
	}
	if state.owner != nil {
		t.Fatal("RPC guidance acquired a product owner")
	}
}

func TestOwnerGateDrainsLateTicketsAndRejectsWorkAfterEnsureReturns(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, &runtimeHost{}, tree, directory, agentDir, func(o *owner) {
		ensured, err := o.state.ensure(o.context)
		if err != nil || ensured != o {
			t.Fatal(err)
		}
		action, err := o.beginAction(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		selected := panelAdmission(t, tree, "late genuine admission")
		lifetime := o.services.Registry().ModelTaskContext(context.Background())
		retired := make(chan struct{})
		go func() { o.state.retire(); close(retired) }()
		<-action.ctx.Done()
		ticket, err := action.scope.begin(nil, "late-ticket", "litter_spawn")
		if err != nil {
			t.Fatal(err)
		}
		o.deferActivation(ticket, selected, nil, func() bool { return false }, "litter_spawn")
		if lifetime.Err() != nil {
			t.Fatal("services closed before an admitted tool handler finished")
		}
		select {
		case <-retired:
			t.Fatal("retirement passed an admitted native handler")
		default:
		}
		action.finish()
		<-retired
		outcome, err := tree.wait(context.Background(), selected, 1000)
		if err == nil || outcome.State != "" {
			t.Fatalf("closed Tree remained externally waitable: %#v %v", outcome, err)
		}
		if selected.outcome == nil || selected.outcome.State != Stopped || lifetime.Err() == nil || len(o.acks) != 0 || len(tree.retained()) != 0 {
			t.Fatalf("retirement left a late ticket, services, or provisional child: outcome=%#v service=%v acks=%d records=%d", selected.outcome, lifetime.Err(), len(o.acks), len(tree.retained()))
		}
		if _, err := ensured.beginAction(context.Background()); err == nil {
			t.Fatal("a handler that returned from ensure before retirement reentered the owner")
		}
		if _, err := o.callScope(nil).begin(nil, "after-retire", "litter_spawn"); err == nil || o.state.calls != nil {
			t.Fatal("a stale handler recreated the global call scope")
		}
	})
}

func TestFullRootToolHandlerKeepsServicesAliveUntilItReturns(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	writeRuntimeModels(t, agentDir, "http://127.0.0.1:1")
	state := &extensionState{}
	defer state.retire()
	ext := newExtension(state)
	prepared := make(chan *owner, 1)
	ext.OnEvent("prepare", func(ctx sdk.Context, _ map[string]any) (any, error) {
		o, err := state.acquire(ctx, false)
		prepared <- o
		return nil, err
	})
	host, handlers, finish := startExtensionFixture(t, ext, &subprocess.ReadyPayload{Cwd: directory, Mode: "rpc", Width: 80, State: &subprocess.StatePayload{HasUI: true}})
	defer finish()
	runtime := &runtimeHost{tools: []string{"read", "ls"}}
	answer := func(frame subprocess.Envelope) {
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: runtime.call(t, frame.Call.Method, frame.Call.Args)}})
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "prepare", Request: &subprocess.RequestPayload{Method: "event", Event: "prepare", HandlerID: handlers["prepare"], Args: json.RawMessage(`{}`)}})
	for {
		frame := readCompletionEnvelope(t, host)
		if frame.Type == subprocess.MsgCall {
			answer(frame)
		} else if frame.Type == subprocess.MsgResponse && frame.ID == "prepare" {
			if frame.Response.Error != nil {
				t.Fatal(frame.Response.Error)
			}
			break
		}
	}
	o := <-prepared
	if !o.tree.root.paused {
		t.Fatal("passive RPC acquisition opened model authority")
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "list", Request: &subprocess.RequestPayload{Method: "tool_call", Tool: "litter_list", ToolCallID: "sdk-list", Args: json.RawMessage(`{}`)}})
	for {
		frame := readCompletionEnvelope(t, host)
		if frame.Type == subprocess.MsgCall {
			answer(frame)
		} else if frame.Type == subprocess.MsgResponse && frame.ID == "list" {
			var tool struct{ Content string }
			var result struct{ State, Operation string }
			if err := json.Unmarshal(frame.Response.Result, &tool); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tool.Content), &result); err != nil || result.State != "listed" || result.Operation != "litter_list" || o.tree.root.paused {
				t.Fatalf("real root Execute did not authorize the nil-signal RPC path: %#v %v", result, err)
			}
			break
		}
	}
	serviceClosing, releaseService := make(chan struct{}), make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(releaseService) })
	if !o.services.Registry().StartModelTask(context.Background(), func(ctx context.Context) { <-ctx.Done(); close(serviceClosing); <-releaseService }) {
		t.Fatal("live services rejected the native lifetime witness")
	}
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "spawn", Request: &subprocess.RequestPayload{Method: "tool_call", Tool: "litter_spawn", ToolCallID: "sdk-spawn", Args: json.RawMessage(`{"type":"scout","task":"must await genuine final success","model":"fixture/child"}`)}})
	retired := make(chan struct{})
	blocked := false
	for {
		frame := readCompletionEnvelope(t, host)
		if frame.Type == subprocess.MsgCall {
			if frame.Call.Method == "getModelRegistryState" && !blocked {
				blocked = true
				go func() { state.retire(); close(retired) }()
				for {
					notification := readCompletionEnvelope(t, host)
					if notification.Type == subprocess.MsgWidgetPush && len(notification.WidgetPush.Lines) == 0 {
						break
					}
				}
				select {
				case <-serviceClosing:
					t.Fatal("services closed while the full native Execute handler was blocked")
				case <-retired:
					t.Fatal("retirement passed a blocked native Execute handler")
				case <-time.After(20 * time.Millisecond):
				}
			}
			answer(frame)
		} else if frame.Type == subprocess.MsgResponse && frame.ID == "spawn" {
			var tool struct {
				Content string
				IsError bool `json:"is_error"`
			}
			var result struct{ State, Operation, Report string }
			if err := json.Unmarshal(frame.Response.Result, &tool); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal([]byte(tool.Content), &result); err != nil || !tool.IsError || result.State != "failed" || result.Operation != "litter_spawn" || result.Report != "context canceled" {
				t.Fatalf("retired native handler did not cancel truthfully: %#v %v", result, err)
			}
			break
		}
	}
	if !blocked {
		t.Fatal("native Execute never reached the services overlap barrier")
	}
	<-serviceClosing
	select {
	case <-retired:
		t.Fatal("full retirement barrier skipped services cleanup")
	default:
	}
	release.Do(func() { close(releaseService) })
	<-retired
	if len(o.acks) != 0 || len(o.tree.retained()) != 0 || state.calls != nil {
		t.Fatal("retired native Execute left tickets, children, or a global scope")
	}
}
