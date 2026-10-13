package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

const (
	failedSpawnName  = "result-hook-retry"
	failedSpawnTask  = "RESULT_HOOK_FAILURE_MUST_NOT_RUN"
	spawnFailureText = "FIXTURE_FINAL_SPAWN_FAILURE\n"
)

type boundaryGateAction string

const (
	boundaryGateHold     boundaryGateAction = "hold"
	boundaryGateSDKAbort boundaryGateAction = "sdk_abort"
)

func main() {
	directory := os.Getenv("LITTER_FIXTURE_EVIDENCE")
	if directory == "" {
		fmt.Fprintln(os.Stderr, "LITTER_FIXTURE_EVIDENCE is required")
		os.Exit(1)
	}
	workspace, err := os.Getwd()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	var mu sync.Mutex
	ext := sdk.New("litter-fixture-guard")
	if baseURL := os.Getenv("LITTER_FIXTURE_SHADOW_BASE"); baseURL != "" {
		ext.RegisterProvider("fixture", sdk.ProviderConfig{"api": "openai-completions", "baseUrl": baseURL, "apiKey": "local-fixture-only", "models": []map[string]any{
			{"id": "parent_shadow", "name": "parent_shadow", "contextWindow": 100000, "maxTokens": 20000},
			{"id": "held", "name": "held", "contextWindow": 100000, "maxTokens": 20000},
		}})
	}
	ext.OnEvent("tool_call", func(_ sdk.Context, data map[string]any) (any, error) {
		name, _ := data["toolName"].(string)
		input, _ := data["input"].(map[string]any)
		path, _ := input["path"].(string)
		if !deniedPath(workspace, name, path) {
			return nil, nil
		}
		mu.Lock()
		defer mu.Unlock()
		file, err := os.OpenFile(filepath.Join(directory, "permission.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			return nil, err
		}
		defer file.Close()
		if err := json.NewEncoder(file).Encode(data); err != nil {
			return nil, err
		}
		return map[string]any{"block": true, "reason": "PARENT_PERMISSION_DENIED_" + name}, nil
	})
	if os.Getenv("LITTER_FIXTURE_BOUNDARY_GATE_ARM") != "" {
		ext.OnEvent("agent_before_settle", func(ctx sdk.Context, data map[string]any) (any, error) {
			return holdBoundaryForFixture(ctx, data)
		})
	}
	ext.OnToolResult(func(_ sdk.Context, data map[string]any) (any, error) {
		return finalSpawnResultFailure(data)
	})
	if err := ext.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func holdBoundaryForFixture(ctx sdk.Context, data map[string]any) (any, error) {
	armPath := os.Getenv("LITTER_FIXTURE_BOUNDARY_GATE_ARM")
	if armPath == "" {
		return nil, nil
	}
	actionBytes, err := os.ReadFile(armPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if err := os.Remove(armPath); err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	action := boundaryGateAction(strings.TrimSpace(string(actionBytes)))
	if action != boundaryGateHold && action != boundaryGateSDKAbort {
		return nil, fmt.Errorf("unexpected boundary gate action %q", action)
	}
	entry, err := json.Marshal(map[string]any{"event": data["type"], "outcome": data["outcome"], "action": action})
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(os.Getenv("LITTER_FIXTURE_BOUNDARY_GATE_EVIDENCE"), entry, 0o600); err != nil {
		return nil, err
	}
	conn, err := net.DialTimeout("unix", os.Getenv("LITTER_FIXTURE_BOUNDARY_GATE_SOCKET"), 5*time.Second)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	if _, err := fmt.Fprintln(conn, "entered"); err != nil {
		return nil, err
	}
	reader := bufio.NewReader(conn)
	if action == boundaryGateSDKAbort {
		command, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		if command != "abort\n" {
			return nil, fmt.Errorf("unexpected boundary gate command %q", command)
		}
		ctx.Abort()
		if _, err := fmt.Fprintln(conn, "abort-returned"); err != nil {
			return nil, err
		}
	}
	release, err := reader.ReadString('\n')
	if err != nil {
		return nil, err
	}
	if release != "release\n" {
		return nil, fmt.Errorf("unexpected boundary gate command %q", release)
	}
	return nil, nil
}

func finalSpawnResultFailure(data map[string]any) (any, error) {
	toolName, _ := data["toolName"].(string)
	input, _ := data["input"].(map[string]any)
	name, _ := input["name"].(string)
	task, _ := input["task"].(string)
	if toolName == "litter_spawn" && name == failedSpawnName && task == failedSpawnTask {
		original := textContent(data["content"])
		if original == "" {
			return nil, fmt.Errorf("successful spawn %q had no text result to replace", name)
		}
		return map[string]any{
			"content": []any{map[string]any{"type": "text", "text": spawnFailureText + original}},
			"details": map[string]any{"fixture": "replacement result details"},
			"isError": true,
		}, nil
	}
	if os.Getenv("LITTER_FIXTURE_REPLACE_DETAILS") == "1" && strings.HasPrefix(toolName, "litter_") {
		return map[string]any{"details": map[string]any{"fixture": "replacement result details"}}, nil
	}
	return nil, nil
}

func textContent(value any) string {
	blocks, ok := value.([]any)
	if !ok {
		return ""
	}
	for _, block := range blocks {
		fields, ok := block.(map[string]any)
		if !ok || fields["type"] != "text" {
			continue
		}
		if text, ok := fields["text"].(string); ok {
			return text
		}
	}
	return ""
}

func deniedPath(workspace, name, path string) bool {
	if name != "read" && name != "write" {
		return false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(workspace, path)
	}
	path = filepath.Clean(path)
	return slices.Contains([]string{
		filepath.Join(workspace, "denied-read.txt"),
		filepath.Join(workspace, "denied-write.txt"),
	}, path)
}
