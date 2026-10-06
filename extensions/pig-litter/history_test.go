package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/coding"
)

func TestNativeSessionConstructionHistoryFitsTheDefaultBudget(t *testing.T) {
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
	trusted := true
	settings, err := memorySettings()
	if err != nil {
		t.Fatal(err)
	}
	services, err := coding.NewServices(coding.ServicesOptions{CWD: dir, AgentDir: dir, ProjectTrusted: &trusted, SettingsManager: settings})
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
	tools := []agent.AgentTool{}
	for _, name := range controlNames {
		tools = append(tools, &nativeTool{name: name, schema: schemas[name], execute: func(context.Context, string, json.RawMessage) (agent.AgentToolResult, error) {
			return agent.AgentToolResult{}, nil
		}})
	}
	session, err := runtime.New(coding.SessionStartOptions{SessionManager: manager, Model: services.ModelRuntime().GetModel("fixture", "child"), SystemPrompt: "input.txt", ResourceLoader: coding.NoResources, SkipBuiltinTools: true, ExtraTools: tools})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	go func() {
		for range session.Events() {
		}
	}()
	var mu sync.Mutex
	unsubscribe := session.Subscribe(func(event agent.AgentEvent) {
		if _, ok := event.(agent.MessageEndEvent); ok {
			mu.Lock()
			t.Logf("message_end retained history is %d bytes", historyBytes(manager))
			mu.Unlock()
		}
	})
	defer unsubscribe()
	size := historyBytes(manager)
	t.Logf("native initial history is %d bytes before child inference", size)
	if size == 0 || size > defaultConfig().MaxHistoryBytes {
		t.Fatalf("native construction history exceeded the default budget: %d bytes", size)
	}
	if _, err := session.Send(context.Background(), "short task"); err != nil {
		t.Fatal(err)
	}
	if size := historyBytes(manager); size > defaultConfig().MaxHistoryBytes {
		t.Fatalf("short native turn exceeded the default budget: %d", size)
	}
}
