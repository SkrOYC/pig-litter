package piglitter

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestParallelAcquisitionDoesNotBlockEarlierHostCall(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "litter.yaml"), []byte("unknown_key: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &extensionState{}
	ext := sdk.New("acquisition-order")
	firstMayProceed := make(chan struct{})
	ext.RegisterTool(sdk.ToolDefinition{
		Name: "acquire_probe", Parameters: object(nil, map[string]any{}), ExecutionMode: "parallel",
		Execute: func(ctx sdk.Context, _ map[string]any) (any, error) {
			if ctx.ToolCallID() == "first" {
				<-firstMayProceed
			}
			_, err := state.ensure(ctx)
			if err == nil {
				return map[string]string{"error": "unexpected owner"}, nil
			}
			return map[string]string{"error": err.Error()}, nil
		},
	})
	host, _, finish := startExtensionFixture(t, ext, &subprocess.ReadyPayload{Cwd: directory, Mode: "json", State: &subprocess.StatePayload{}})
	released := false
	t.Cleanup(func() {
		if !released {
			close(firstMayProceed)
		}
		if t.Failed() {
			_ = host.Close()
			return
		}
		finish()
	})
	for _, id := range []string{"first", "second"} {
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: id, Request: &subprocess.RequestPayload{Method: "tool_call", Tool: "acquire_probe", ToolCallID: id, Batch: "parallel", Args: json.RawMessage(`{}`)}})
	}
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	results := map[string]string{}
	for len(results) != 2 {
		frame := readCompletionEnvelope(t, host)
		if frame.Type == subprocess.MsgRequestState && frame.RequestState.RequestID == "second" && frame.RequestState.State == "blocked" && !released {
			close(firstMayProceed)
			released = true
		}
		if frame.Type == subprocess.MsgCall {
			if frame.Call.Method != "sessionRead" {
				t.Fatalf("unexpected host call %q", frame.Call.Method)
			}
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: json.RawMessage(`"fixture"`)}})
		}
		if frame.Type == subprocess.MsgResponse {
			if frame.Response.Error != nil {
				t.Fatal(frame.Response.Error)
			}
			var result map[string]string
			if err := json.Unmarshal(frame.Response.Result, &result); err != nil {
				t.Fatal(err)
			}
			results[frame.ID] = result["error"]
		}
	}
	for _, id := range []string{"first", "second"} {
		if got := results[id]; !strings.HasSuffix(got, "litter.yaml has unknown setting unknown_key") {
			t.Fatalf("%s acquisition returned %q", id, got)
		}
	}
}

func TestAcquisitionRechecksSessionAfterInitializationWait(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "litter.yaml"), []byte("unknown_key: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	state := &extensionState{}
	state.initialization.Lock()
	locked := true
	t.Cleanup(func() {
		if locked {
			state.initialization.Unlock()
		}
	})
	ext := sdk.New("acquisition-session-change")
	ext.RegisterTool(sdk.ToolDefinition{
		Name: "acquire_probe", Parameters: object(nil, map[string]any{}),
		Execute: func(ctx sdk.Context, _ map[string]any) (any, error) {
			state.mu.Lock()
			state.sessionContext = &ctx
			state.mu.Unlock()
			_, err := state.acquire(ctx, false)
			if err == nil {
				return map[string]string{"error": "unexpected owner"}, nil
			}
			return map[string]string{"error": err.Error()}, nil
		},
	})
	host, _, finish := startExtensionFixture(t, ext, &subprocess.ReadyPayload{Cwd: directory, Mode: "json", State: &subprocess.StatePayload{}})
	t.Cleanup(func() {
		if t.Failed() {
			_ = host.Close()
			return
		}
		finish()
	})
	writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: "probe", Request: &subprocess.RequestPayload{Method: "tool_call", Tool: "acquire_probe", ToolCallID: "probe", Args: json.RawMessage(`{}`)}})
	if err := host.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	reads := 0
	for {
		frame := readCompletionEnvelope(t, host)
		if frame.Type == subprocess.MsgCall {
			if frame.Call.Method != "sessionRead" {
				t.Fatalf("unexpected host call %q", frame.Call.Method)
			}
			reads++
			id := json.RawMessage(`"new"`)
			if reads == 1 {
				id = json.RawMessage(`"old"`)
			}
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: id}})
			if locked {
				state.initialization.Unlock()
				locked = false
			}
		}
		if frame.Type == subprocess.MsgResponse {
			if frame.Response.Error != nil {
				t.Fatal(frame.Response.Error)
			}
			var result map[string]string
			if err := json.Unmarshal(frame.Response.Result, &result); err != nil {
				t.Fatal(err)
			}
			if got := result["error"]; !strings.HasSuffix(got, "litter.yaml has unknown setting unknown_key") {
				t.Fatalf("acquisition after session change returned %q", got)
			}
			break
		}
	}
}
