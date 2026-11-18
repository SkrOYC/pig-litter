package piglitter

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
)

func TestModelAuthorityUsesFreshProviderStateAndPreservesQuotaOrder(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { runtimeAnswer(w, "fixture") }))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	config := defaultConfig()
	config.Concurrency = 1
	tree := testTree(t, config)
	host := &runtimeHost{tools: append([]string{"read", "ls"}, controlNames...)}
	var state, reads atomic.Int32
	withCompletionReplies(t, tree, "print", func(o *owner) {
		trusted := true
		services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: directory, AgentDir: agentDir, ProjectTrusted: &trusted})
		if err != nil {
			t.Fatal(err)
		}
		defer services.Close()
		o.cwd, o.agentDir, o.services = directory, agentDir, services
		if _, err := o.modelFor(context.Background(), "fixture/child", services.ModelRuntime()); err == nil || !strings.Contains(err.Error(), "extension-registered parent provider") {
			t.Fatalf("registered provider error = %v", err)
		}
		if got := reads.Load(); got != 1 {
			t.Fatalf("registered provider made %d registry reads, want 1", got)
		}
		state.Store(1)
		if model, err := o.modelFor(context.Background(), "fixture/child", services.ModelRuntime()); err != nil || model == nil {
			t.Fatalf("unregistered provider model = %v, error = %v", model, err)
		}
		state.Store(2)
		if _, err := o.modelFor(context.Background(), "fixture/child", services.ModelRuntime()); err == nil || !strings.Contains(err.Error(), "exact model is unavailable") {
			t.Fatalf("removed availability error = %v", err)
		}
		if got := reads.Load(); got != 3 {
			t.Fatalf("three operations made %d registry reads, want 3", got)
		}
		state.Store(1)
		_, _, err = tree.admit(admission{kind: "scout", name: "occupied", model: "fixture/child", cwd: directory, task: "hold", agent: config.Agents["scout"]})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = runtimeOperation(t, o, "litter_spawn", "invalid-at-capacity", map[string]any{"type": "scout", "task": "missing", "model": "fixture/missing"})
		if failure, ok := err.(*treeError); !ok || failure.code != "unavailable" {
			t.Fatalf("missing model at capacity error = %v, want unavailable", err)
		}
	}, func(method string, raw json.RawMessage) *subprocess.CallResultPayload {
		if method != "getModelRegistryState" {
			return &subprocess.CallResultPayload{Result: host.call(t, method, raw)}
		}
		count := reads.Add(1)
		if state.Load() == 0 && count > 1 {
			return &subprocess.CallResultPayload{Error: &subprocess.ErrorInfo{Message: "availability failed"}}
		}
		registered := []map[string]any{}
		if state.Load() == 0 {
			registered = append(registered, map[string]any{"name": "fixture"})
		}
		value := map[string]any{"models": []map[string]any{{"provider": "fixture", "id": "child"}}, "providers": map[string]any{"fixture": map[string]any{"configured": state.Load() != 2}}, "registered": registered}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return &subprocess.CallResultPayload{Result: data}
	})
}
