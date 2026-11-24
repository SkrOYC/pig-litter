package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
)

func TestRetainedOwnerAcceptsProviderConfiguredAfterInitialization(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	if err := os.Mkdir(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		if strings.Contains(string(body), "HUMAN_AFTER_CONFIGURATION") {
			runtimeAnswer(w, "human resumed after configuration")
			return
		}
		if strings.Contains(string(body), "CONTINUE_AFTER_CONFIGURATION") {
			runtimeAnswer(w, "resumed after configuration")
			return
		}
		runtimeAnswer(w, "started after configuration")
	}))
	defer server.Close()
	models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"models":[{"id":"child","name":"child","contextWindow":32000,"maxTokens":1024}]}}}`, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	config := defaultConfig()
	config.Concurrency = 1
	tree := testTree(t, config)
	host := &runtimeHost{tools: append([]string{"read", "ls"}, controlNames...)}
	var parentConfigured atomic.Bool
	parentConfigured.Store(true)
	withCompletionReplies(t, tree, "print", func(o *owner) {
		trusted := true
		services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: directory, AgentDir: agentDir, ProjectTrusted: &trusted})
		if err != nil {
			t.Fatal(err)
		}
		o.cwd, o.agentDir, o.services = directory, agentDir, services
		tree.execute, tree.persist = o.executeRun, o.saveChild
		defer o.state.retire()
		runtime := services.ModelRuntime()
		if runtime.HasConfiguredAuth("fixture") {
			t.Fatal("fixture unexpectedly started with provider configuration")
		}
		if _, err := o.modelFor(context.Background(), "fixture/child", runtime); err == nil || !strings.Contains(err.Error(), "exact model is unavailable") {
			t.Fatalf("missing provider configuration error = %v", err)
		}
		parentConfigured.Store(false)
		if err := os.WriteFile(filepath.Join(agentDir, "auth.json"), []byte(`{"fixture":{"type":"api_key","key":"fixture-only"}}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := o.modelFor(context.Background(), "fixture/child", runtime); err == nil || !strings.Contains(err.Error(), "exact model is unavailable") {
			t.Fatalf("parent authority exclusion error = %v", err)
		}
		parentConfigured.Store(true)
		if runtime.HasConfiguredAuth("fixture") {
			t.Fatal("retained runtime snapshot changed before admission")
		}
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "configured-spawn", map[string]any{"type": "scout", "task": "start after configuration", "model": "fixture/child"})
		if err != nil {
			t.Errorf("spawn after provider configuration: %v", err)
			return
		}
		child := value["child"].(Snapshot)
		if child.Model != "fixture/child" || child.State != Starting {
			t.Fatalf("spawn selected %#v", child)
		}
		_, _, err = runtimeOperation(t, o, "litter_spawn", "missing-at-capacity", map[string]any{"type": "scout", "task": "missing", "model": "fixture/missing"})
		if failure, ok := err.(*treeError); !ok || failure.code != "unavailable" {
			t.Fatalf("missing model at capacity error = %v, want unavailable", err)
		}
		_, _, err = runtimeOperation(t, o, "litter_spawn", "valid-at-capacity", map[string]any{"type": "scout", "task": "valid", "model": "fixture/child"})
		if failure, ok := err.(*treeError); !ok || failure.code != "rejected" || !strings.Contains(err.Error(), "live child capacity") {
			t.Fatalf("available model at capacity error = %v, want capacity rejection", err)
		}
		o.settleAck(ticket, false)
		selected := testRun(t, tree, child.Ref)
		outcome, err := tree.wait(context.Background(), selected, 3000)
		if err != nil || outcome.State != Completed || outcome.Text != "started after configuration" {
			t.Fatalf("configured child outcome = %#v, error = %v", outcome, err)
		}
		tree.acknowledge(selected)
		if runtime.HasConfiguredAuth("fixture") {
			t.Fatal("retained runtime snapshot changed before resume")
		}
		value, ticket, err = runtimeOperation(t, o, "litter_message", "configured-resume", map[string]any{"id": child.ID, "resume": true, "text": "CONTINUE_AFTER_CONFIGURATION"})
		if err != nil || value["child"].(Snapshot).ID != child.ID || value["delivery"] != "resumed" {
			t.Fatalf("retained child resume = %#v, error = %v", value, err)
		}
		o.settleAck(ticket, false)
		selected = testRun(t, tree, child.Ref)
		outcome, err = tree.wait(context.Background(), selected, 3000)
		if err != nil || outcome.State != Completed || outcome.Text != "resumed after configuration" {
			t.Fatalf("resumed child outcome = %#v, error = %v", outcome, err)
		}
		tree.acknowledge(selected)
		if runtime.HasConfiguredAuth("fixture") {
			t.Fatal("retained runtime snapshot changed before human resume")
		}
		human := o.performHuman(context.Background(), selected, humanAction{kind: humanResume, text: "HUMAN_AFTER_CONFIGURATION"})
		if human.err != nil {
			t.Fatalf("human resume after provider configuration: %v", human.err)
		}
		outcome, err = tree.wait(context.Background(), human.selected, 3000)
		if err != nil || outcome.State != Completed || outcome.Text != "human resumed after configuration" {
			t.Fatalf("human resumed child outcome = %#v, error = %v", outcome, err)
		}
		tree.acknowledge(human.selected)
	}, func(method string, raw json.RawMessage) *subprocess.CallResultPayload {
		if method != "getModelRegistryState" {
			return &subprocess.CallResultPayload{Result: host.call(t, method, raw)}
		}
		value := map[string]any{"models": []map[string]any{{"provider": "fixture", "id": "child"}}, "providers": map[string]any{"fixture": map[string]any{"configured": parentConfigured.Load()}}, "registered": []any{}}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return &subprocess.CallResultPayload{Result: data}
	})
}

func TestModelAuthorityPreservesChecksAndProviderErrors(t *testing.T) {
	const (
		parentAvailable int32 = iota
		parentRegistered
		parentScopedOut
		parentErrored
	)
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	config := defaultConfig()
	config.Concurrency = 1
	tree := testTree(t, config)
	host := &runtimeHost{tools: append([]string{"read", "ls"}, controlNames...)}
	var parentState, checks, failAt atomic.Int32
	providerFailure := errors.New("synthetic provider check failed")
	withCompletionReplies(t, tree, "print", func(o *owner) {
		runtime, err := coding.CreateModelRuntime(context.Background(), coding.CreateModelRuntimeOptions{
			Credentials: ai.NewInMemoryCredentialStore(), ModelsPath: new((*string)(nil)), ModelsStore: ai.NewInMemoryModelsStore(), RefreshOnCreate: new(false),
		})
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		model := func(id string) *ai.Model {
			return &ai.Model{ID: id, DisplayName: id, Input: []string{"text"}, ProviderMeta: ai.ProviderMetadata{ProviderID: "fixture", API: ai.APIOpenAICompletions, BaseURL: "http://127.0.0.1:1/v1"}, Capabilities: ai.ModelCapabilities{ContextWindow: 32000, MaxOutputTokens: 1024}}
		}
		catalog := []*ai.Model{model("visible"), model("hidden")}
		provider := &ai.ModelsProvider{
			ID: "fixture", GetModels: func() ([]*ai.Model, error) { return catalog, nil },
			Auth: ai.ProviderAuth{APIKey: &ai.APIKeyAuth{Check: func(context.Context, ai.APIKeyAuthInput) (*ai.AuthCheck, error) {
				count := checks.Add(1)
				if failAt.Load() != 0 && count >= failAt.Load() {
					return nil, providerFailure
				}
				return &ai.AuthCheck{Type: ai.CredentialAPIKey}, nil
			}}},
			FilterModels: func(models []*ai.Model, _ *ai.Credential) []*ai.Model { return models[:1] },
		}
		if err := runtime.RegisterNativeProvider(provider); err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.GetAvailable(context.Background()); err != nil {
			t.Fatal(err)
		}
		services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: directory, AgentDir: directory, ModelRuntime: runtime})
		if err != nil {
			t.Fatal(err)
		}
		o.cwd, o.agentDir, o.services = directory, directory, services
		defer o.state.retire()
		if got, err := o.modelFor(context.Background(), "fixture/visible", runtime); err != nil || got == nil || got.ID != "visible" {
			t.Fatalf("available exact model = %v, error = %v", got, err)
		}
		if _, err := o.modelFor(context.Background(), "fixture/hidden", runtime); err == nil || !strings.Contains(err.Error(), "exact model is unavailable to the stock SDK") {
			t.Fatalf("filtered exact model error = %v", err)
		}
		checks.Store(0)
		failAt.Store(1)
		parentState.Store(parentRegistered)
		if _, err := o.modelFor(context.Background(), "fixture/visible", runtime); err == nil || !strings.Contains(err.Error(), "extension-registered parent provider") {
			t.Fatalf("registered provider error = %v", err)
		}
		parentState.Store(parentScopedOut)
		if _, err := o.modelFor(context.Background(), "fixture/visible", runtime); err == nil || !strings.Contains(err.Error(), "exact model is unavailable") {
			t.Fatalf("scope exclusion error = %v", err)
		}
		parentState.Store(parentAvailable)
		if _, err := o.modelFor(context.Background(), "fixture/missing", runtime); err == nil || !strings.Contains(err.Error(), "exact model is unavailable") {
			t.Fatalf("missing catalog model error = %v", err)
		}
		parentState.Store(parentErrored)
		if _, err := o.modelFor(context.Background(), "fixture/visible", runtime); err == nil || !strings.Contains(err.Error(), "parent provider failed") {
			t.Fatalf("parent provider error = %v", err)
		}
		parentState.Store(parentAvailable)
		cancelled, cancel := context.WithCancelCause(context.Background())
		cancel(errors.New("synthetic abort cause"))
		if _, err := o.modelFor(cancelled, "fixture/visible", runtime); !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled model check error = %v", err)
		}
		_, _, err = tree.admit(admission{kind: "scout", name: "occupied", model: "fixture/visible", cwd: directory, task: "hold", agent: config.Agents["scout"]})
		if err != nil {
			t.Fatal(err)
		}
		_, _, err = runtimeOperation(t, o, "litter_spawn", "provider-error-at-capacity", map[string]any{"type": "scout", "task": "valid", "model": "fixture/visible"})
		if !errors.Is(err, providerFailure) {
			t.Fatalf("provider error at capacity = %v", err)
		}
		checks.Store(0)
		failAt.Store(2)
		if _, err := o.modelFor(context.Background(), "fixture/visible", runtime); !errors.Is(err, providerFailure) {
			t.Fatalf("independent availability error = %v", err)
		}
	}, func(method string, raw json.RawMessage) *subprocess.CallResultPayload {
		if method == "getScopedModels" && parentState.Load() == parentScopedOut {
			return &subprocess.CallResultPayload{Result: json.RawMessage(`[{"model":{"provider":"fixture","id":"hidden"}}]`)}
		}
		if method != "getModelRegistryState" {
			return &subprocess.CallResultPayload{Result: host.call(t, method, raw)}
		}
		if parentState.Load() == parentErrored {
			return &subprocess.CallResultPayload{Error: &subprocess.ErrorInfo{Message: "parent provider failed"}}
		}
		registered := []map[string]any{}
		if parentState.Load() == parentRegistered {
			registered = append(registered, map[string]any{"name": "fixture"})
		}
		value := map[string]any{"models": []map[string]any{{"provider": "fixture", "id": "visible"}, {"provider": "fixture", "id": "hidden"}, {"provider": "fixture", "id": "missing"}}, "providers": map[string]any{"fixture": map[string]any{"configured": true}}, "registered": registered}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return &subprocess.CallResultPayload{Result: data}
	})
}

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
