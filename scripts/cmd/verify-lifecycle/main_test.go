package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	piglitter "github.com/SkrOYC/pig-litter/extensions/pig-litter"
)

func TestUnboundedChildUsesCanonicalWriteReceipt(t *testing.T) {
	for _, test := range []struct {
		name, receipt string
		wantErr       bool
	}{
		{"canonical success", "Successfully wrote to /fixture/workspace/unbounded-marker.txt", false},
		{"contents are not a receipt", "unbounded marker\n", true},
		{"different target", "Successfully wrote to /fixture/workspace/other.txt", true},
		{"denied write", "Error: permission denied", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, err := json.Marshal(test.receipt)
			if err != nil {
				t.Fatal(err)
			}
			f := &fixture{expectedCWD: "/fixture/workspace"}
			text, calls, err := f.unboundedChildRequest(context.Background(), modelRequest{Messages: []message{{Role: "tool", Content: raw}}})
			if test.wantErr {
				if err == nil {
					t.Fatalf("accepted %q as a successful canonical write", test.receipt)
				}
				return
			}
			if err != nil || text != "unbounded child marker recorded" || len(calls) != 0 {
				t.Fatalf("canonical write returned %q, %#v, %v", text, calls, err)
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
	agentDir = filepath.Join(userOnlyRoot, "agent")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, content := range fixtureConfigFiles(workspace, agentDir, "cwd") {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(filepath.Join(workspace, "litter.yaml")); !os.IsNotExist(err) {
		t.Fatalf("user-only cwd fixture created a project config: %v", err)
	}
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	cwd, err := piglitter.LoadConfig(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if cwd.Agents["scout"].Instructions != "USER_ONLY_SCOUT_INSTRUCTIONS" || cwd.Agents["worker"].Instructions != "USER_ONLY_WORKER_INSTRUCTIONS" {
		t.Fatalf("user-only instructions = scout %q, worker %q", cwd.Agents["scout"].Instructions, cwd.Agents["worker"].Instructions)
	}
}
