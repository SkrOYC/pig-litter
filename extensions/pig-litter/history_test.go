package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/coding"
)

func TestNativeSessionResourcesAndTranscriptSurviveResume(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIG_OFFLINE", "1")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"history-test\",\"object\":\"chat.completion.chunk\",\"model\":\"child\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"short response\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"history-test\",\"object\":\"chat.completion.chunk\",\"model\":\"child\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"fixture-only","models":[{"id":"child","name":"child","contextWindow":32000,"maxTokens":1024}]}}}`, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(dir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("CHILD_DIRECTORY_CONTEXT"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "input.txt"), []byte("DO_NOT_USE_THIS_FILE_AS_INSTRUCTIONS"), 0o600); err != nil {
		t.Fatal(err)
	}
	trusted := true
	services, err := coding.NewServices(coding.ServicesOptions{CWD: dir, AgentDir: dir, ProjectTrusted: &trusted})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, AbortContext: context.Background()})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	tools := []agent.AgentTool{}
	for _, name := range controlNames {
		tools = append(tools, &nativeTool{name: name, schema: schemas[name], execute: func(context.Context, string, json.RawMessage) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{}, nil
		}})
	}
	loader := coding.NewDefaultResourceLoader(coding.DefaultResourceLoaderOptions{CWD: dir, AgentDir: dir, SettingsManager: services.SettingsManager(), AppendSystemPromptOverride: func(found []string) []string { return append(found, "input.txt") }})
	if err := loader.Reload(); err != nil {
		t.Fatal(err)
	}
	options := coding.SessionStartOptions{SessionDir: filepath.Join(dir, "sessions"), Model: services.ModelRuntime().GetModel("fixture", "child"), ResourceLoader: loader, ExtraTools: tools, AllowedTools: map[string]struct{}{"read": {}}}
	session, err := runtime.New(options)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	go func() {
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	prompt := session.SystemPrompt()
	if !strings.Contains(prompt, "CHILD_DIRECTORY_CONTEXT") || !strings.Contains(prompt, "input.txt") || strings.Contains(prompt, "DO_NOT_USE_THIS_FILE_AS_INSTRUCTIONS") {
		t.Fatalf("native prompt lost directory context or literal role instructions: %s", prompt)
	}
	if _, err := session.Send(context.Background(), "short task"); err != nil {
		t.Fatal(err)
	}
	if report := session.LastAssistantText(); report == nil || *report != "short response" {
		t.Fatalf("native Send did not settle: %v", report)
	}
	id := session.ID()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	resumed, err := runtime.Resume(id, options)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	entries, err := json.Marshal(resumed.SessionManager().Entries())
	if err != nil {
		t.Fatal(err)
	}
	if resumed.ID() != id || !strings.Contains(string(entries), "short task") || !strings.Contains(string(entries), "short response") {
		t.Fatalf("native UUID resume lost its transcript: id=%q entries=%s", resumed.ID(), entries)
	}
	if prompt := resumed.SystemPrompt(); !strings.Contains(prompt, "CHILD_DIRECTORY_CONTEXT") || !strings.Contains(prompt, "input.txt") || strings.Contains(prompt, "DO_NOT_USE_THIS_FILE_AS_INSTRUCTIONS") {
		t.Fatalf("native resume lost directory context or literal role instructions: %s", prompt)
	}
}
