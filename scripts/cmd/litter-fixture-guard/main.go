package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func main() {
	directory := os.Getenv("LITTER_FIXTURE_EVIDENCE")
	if directory == "" {
		fmt.Fprintln(os.Stderr, "LITTER_FIXTURE_EVIDENCE is required")
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
		if name != "read" && name != "write" || path != "denied-read.txt" && path != "denied-write.txt" {
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
	if err := ext.Run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
