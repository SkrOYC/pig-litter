package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	piglitter "github.com/SkrOYC/pig-litter/extensions/pig-litter"
)

func TestWaitReturnsFixtureFailure(t *testing.T) {
	failure := errors.New("fixture provider request failed")
	f := &fixture{changed: make(chan struct{}), failure: failure}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	if err := f.wait(ctx, func() bool { return false }); !errors.Is(err, failure) {
		t.Fatalf("wait error = %v, want fixture failure", err)
	}
}

func TestRequirePausedOutcomeRejectsCompletedLatestOutcome(t *testing.T) {
	childRef := ref{ID: "paused-child"}
	value := result{
		Child:   child{ref: childRef, State: "paused"},
		Outcome: outcome{Run: childRef, State: "paused", Model: "fixture/lifecycle_child"},
	}
	if err := requirePausedOutcome(value, childRef); err != nil {
		t.Fatalf("paused child outcome was rejected: %v", err)
	}

	value.Outcome.State = "completed"
	if err := requirePausedOutcome(value, childRef); err == nil {
		t.Fatal("accepted a completed outcome for an active-abort pause")
	}
}

func TestServeValidatesConfiguredInstructionLiteralForEachScout(t *testing.T) {
	for _, test := range []struct {
		name, model, prompt, wantPath string
		wantStatus                    int
	}{
		{"captured user-only scout", "cwd_nested", "USER_ONLY_SCOUT_INSTRUCTIONS", "@cwd-output.txt", http.StatusOK},
		{"literal project filename", "missing_file", "input.txt", "missing-file.txt", http.StatusOK},
		{"wrong scout configuration", "cwd_nested", "input.txt", "", http.StatusInternalServerError},
		{"file contents replaced literal", "missing_file", "stock fixture read marker\n", "", http.StatusInternalServerError},
		{"file contents appended to literal", "missing_file", "input.txt\nstock fixture read marker", "", http.StatusInternalServerError},
		{"user-only prompt changed", "cwd_nested", "USER_ONLY_SCOUT_INSTRUCTIONS\nextra", "", http.StatusInternalServerError},
	} {
		t.Run(test.name, func(t *testing.T) {
			var body modelRequest
			captured := `{"model":"cwd_nested","messages":[{"role":"system","content":"USER_ONLY_SCOUT_INSTRUCTIONS"},{"role":"user","content":[{"type":"text","text":"Task:\ncwd-nested"}]}],"tools":[{"function":{"name":"read"}}]}`
			if err := json.Unmarshal([]byte(captured), &body); err != nil {
				t.Fatal(err)
			}
			body.Model = test.model
			body.Messages[0].Content, _ = json.Marshal(test.prompt)
			raw, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			f := &fixture{changed: make(chan struct{})}
			response := httptest.NewRecorder()
			f.serve(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(raw))))
			if response.Code != test.wantStatus {
				t.Fatalf("serve returned %d: %s", response.Code, response.Body.String())
			}
			if test.wantStatus != http.StatusOK {
				if f.failure == nil {
					t.Fatal("invalid instructions did not record the fixture failure")
				}
				return
			}
			line, _, _ := strings.Cut(response.Body.String(), "\n")
			payload, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				t.Fatalf("serve did not return an SSE completion: %s", response.Body.String())
			}
			var chunk struct {
				Choices []struct {
					Delta struct {
						ToolCalls []struct {
							Function struct{ Name, Arguments string }
						} `json:"tool_calls"`
					}
				}
			}
			if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
				t.Fatal(err)
			}
			if len(chunk.Choices) != 1 || len(chunk.Choices[0].Delta.ToolCalls) != 1 {
				t.Fatalf("serve did not dispatch the scout read: %s", payload)
			}
			tool := chunk.Choices[0].Delta.ToolCalls[0].Function
			var args struct{ Path string }
			if err := json.Unmarshal([]byte(tool.Arguments), &args); err != nil || tool.Name != "read" || args.Path != test.wantPath {
				t.Fatalf("scout dispatched %#v: %v", tool, err)
			}
		})
	}
}

func TestUnboundedChildUsesOriginalWritePathReceipt(t *testing.T) {
	for _, test := range []struct {
		name, receipt string
		wantErr       bool
	}{
		{"original path success", "Successfully wrote to unbounded-marker.txt", false},
		{"contents are not a receipt", "unbounded marker\n", true},
		{"different path", "Successfully wrote to other.txt", true},
		{"rewritten absolute path", "Successfully wrote to /fixture/workspace/unbounded-marker.txt", true},
		{"denied write", "Error: permission denied", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.receipt)
			if err != nil {
				t.Fatal(err)
			}
			f := &fixture{}
			text, calls, err := f.unboundedChildRequest(context.Background(), modelRequest{Messages: []message{{Role: "tool", Content: raw}}})
			if test.wantErr {
				if err == nil {
					t.Fatalf("accepted %q as a successful native write", test.receipt)
				}
				return
			}
			if err != nil || text != "unbounded child marker recorded" || len(calls) != 0 {
				t.Fatalf("native write returned %q, %#v, %v", text, calls, err)
			}
		})
	}
}

func TestCoreWaitsForResumeBeforeCheckingSettledMessageRejection(t *testing.T) {
	f := &fixture{stage: 5, runs: []ref{{ID: "same-child"}}}
	body := modelRequest{}
	for range 8 {
		body.Messages = append(body.Messages, message{Role: "tool", Content: json.RawMessage(`"{}"`)})
	}
	appendResult := func(value string) {
		t.Helper()
		raw, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		body.Messages = append(body.Messages, message{Role: "tool", Content: raw})
	}
	appendResult(`{"state":"starting","child":{"id":"same-child"}}`)
	_, calls, err := f.parent(context.Background(), body)
	want := []call{{"litter_wait", map[string]any{"id": "same-child", "timeoutMs": 10000}}}
	if err != nil || !reflect.DeepEqual(calls, want) || f.stage != 6 {
		t.Fatalf("resume acknowledgment dispatched %#v at stage %d: %v", calls, f.stage, err)
	}
	appendResult(`{"state":"completed","outcome":{"run":{"id":"same-child"},"usage":{"totalTokens":15},"reportProvenance":"child_assistant"}}`)
	_, calls, err = f.parent(context.Background(), body)
	want = []call{{"litter_message", map[string]any{"id": "same-child", "text": "settled follow-up marker", "mode": "follow_up"}}}
	if err != nil || !reflect.DeepEqual(calls, want) || f.stage != 7 {
		t.Fatalf("completed resume dispatched %#v at stage %d: %v", calls, f.stage, err)
	}
	appendResult(`{"state":"rejected","report":"child is not live"}`)
	_, calls, err = f.parent(context.Background(), body)
	if err != nil || len(calls) != 1 || calls[0].name != "litter_spawn" || f.stage != 8 {
		t.Fatalf("settled rejection dispatched %#v at stage %d: %v", calls, f.stage, err)
	}
}

func TestLifecycleEnvironmentUsesOnlyResolvedExecutableDirectories(t *testing.T) {
	pig := filepath.Join("/released", "pig-bin", "pig")
	bash := filepath.Join("/nix", "store", "bash-bin", "bash")
	directory := filepath.Join("/scratch", "run")
	agentDir := filepath.Join(directory, "agent")
	values := make(map[string]string)
	for _, entry := range fixtureEnvironment(pig, bash, directory, agentDir) {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid environment entry %q", entry)
		}
		values[key] = value
	}
	wantPath := filepath.Join("/released", "pig-bin") + string(os.PathListSeparator) + filepath.Join("/nix", "store", "bash-bin")
	if values["PATH"] != wantPath {
		t.Fatalf("child PATH = %q, want %q", values["PATH"], wantPath)
	}
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "NODE_OPTIONS", "NODE_PATH", "CODEX_HOME"} {
		if _, exists := values[key]; exists {
			t.Errorf("child environment included %s", key)
		}
	}
	if values["PIG_HOME"] != directory || values["PIG_CODING_AGENT_DIR"] != agentDir {
		t.Fatalf("explicit fixture roots changed: %#v", values)
	}
	if values["HOME"] != filepath.Join(directory, "home") || values["USERPROFILE"] != values["HOME"] {
		t.Fatalf("fixture user homes are not isolated together: %#v", values)
	}
	defaults := make(map[string]string)
	for _, entry := range fixtureEnvironment(pig, bash, directory, "") {
		key, value, _ := strings.Cut(entry, "=")
		defaults[key] = value
	}
	if defaults["HOME"] != filepath.Join(directory, "home") || defaults["USERPROFILE"] != defaults["HOME"] || defaults["PIG_USE_PI_DIRS"] != "0" || defaults["PATH"] != wantPath {
		t.Fatalf("default-directory fixture environment changed: %#v", defaults)
	}
	for _, key := range []string{"PIG_HOME", "PIG_CODING_AGENT_DIR", "XDG_CONFIG_HOME", "PI_CODING_AGENT_DIR"} {
		if _, exists := defaults[key]; exists {
			t.Errorf("default-directory fixture set explicit root %s", key)
		}
	}
}

func TestFixtureToolProfilesMatchEveryProviderActor(t *testing.T) {
	scout := []string{"read"}
	coordinator := []string{"read", "litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}
	worker := []string{"read", "write", "edit", "bash", "litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}
	for _, test := range []struct {
		name, model string
		got         []string
		wantErr     bool
	}{
		{"scout leaf", "held", scout, false},
		{"scout leaf with ls", "held", append(append([]string(nil), scout...), "ls"), false},
		{"scout control escalation", "held", append(append([]string(nil), scout...), "litter_spawn"), true},
		{"scout writer escalation", "held", append(append([]string(nil), scout...), "write"), true},
		{"scout missing read", "held", []string{"ls"}, true},
		{"read-only coordinator", "nester", coordinator, false},
		{"coordinator writer escalation", "nester", append(append([]string(nil), coordinator...), "write"), true},
		{"coordinator missing stop", "nester", removeTool(coordinator, "litter_stop"), true},
		{"full worker", "worker", worker, false},
		{"full worker with ls", "worker", append(append([]string(nil), worker...), "ls"), false},
		{"worker missing bash", "worker", removeTool(worker, "bash"), true},
		{"worker missing controls", "worker", removeTool(worker, "litter_spawn"), true},
		{"root full worker", "parent", worker, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := requireFixtureModelTools(modelRequestWithTools(test.model, test.got...))
			if test.wantErr && err == nil {
				t.Fatal("accepted a forbidden or incomplete actor inventory")
			}
			if !test.wantErr && err != nil {
				t.Fatalf("rejected the expected actor inventory: %v", err)
			}
		})
	}
	for _, model := range fixtureModelIDs {
		if _, exists := fixtureModelInventories[model]; !exists {
			t.Errorf("fixture provider model %q has no effective-role inventory", model)
		}
	}
	for model, profile := range fixtureModelInventories {
		if _, exists := toolInventories[profile]; !exists {
			t.Errorf("fixture model %q references unknown tool profile %q", model, profile)
		}
	}
}

func modelRequestWithTools(model string, names ...string) modelRequest {
	request := modelRequest{Model: model}
	for _, name := range names {
		var tool modelTool
		tool.Function.Name = name
		request.Tools = append(request.Tools, tool)
	}
	return request
}

func removeTool(names []string, removed string) []string {
	result := make([]string, 0, len(names)-1)
	for _, name := range names {
		if name != removed {
			result = append(result, name)
		}
	}
	return result
}

func TestFixtureConfigurationsExerciseAgentAndProjectLayers(t *testing.T) {
	root := t.TempDir()
	workspace := filepath.Join(root, "workspace")
	agentDir := filepath.Join(root, "agent")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range fixtureConfigFiles(workspace, agentDir, "core") {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PIG_USE_PI_DIRS", "0")
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	core, err := piglitter.LoadConfig(workspace)
	if err != nil {
		t.Fatal(err)
	}
	userScout, exists := core.Agents["zz_user_only"]
	if core.Concurrency != 2 || !exists || userScout.Role != "scout" || len(userScout.Tools) != 1 || userScout.Tools[0] != "read" || userScout.CanDelegate {
		t.Fatalf("project overlay and user-only scout = concurrency %d, agent %#v", core.Concurrency, userScout)
	}

	userOnlyRoot := t.TempDir()
	workspace = filepath.Join(userOnlyRoot, "workspace")
	fixtureHome := filepath.Join(userOnlyRoot, "home")
	configDir := filepath.Join(fixtureHome, ".config", "pig", "agent")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range fixtureConfigFiles(workspace, configDir, "cwd") {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "litter.yaml")); !os.IsNotExist(err) {
		t.Fatalf("user-only cwd fixture created a project config: %v", err)
	}
	t.Setenv("HOME", fixtureHome)
	for _, key := range []string{"PIG_HOME", "PIG_CODING_AGENT_DIR", "XDG_CONFIG_HOME", "PI_CODING_AGENT_DIR"} {
		t.Setenv(key, "")
	}
	cwd, err := piglitter.LoadConfig(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if cwd.Agents["scout"].Instructions != "USER_ONLY_SCOUT_INSTRUCTIONS" || cwd.Agents["worker"].Instructions != "USER_ONLY_WORKER_INSTRUCTIONS" {
		t.Fatalf("user-only instructions = scout %q, worker %q", cwd.Agents["scout"].Instructions, cwd.Agents["worker"].Instructions)
	}
}
