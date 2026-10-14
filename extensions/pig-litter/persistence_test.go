package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/MichaelKinsy/PiG/coding"
)

type runtimeHost struct {
	mu        sync.Mutex
	entries   []map[string]any
	tools     []string
	untrusted bool
}

func (h *runtimeHost) call(t *testing.T, method string, raw json.RawMessage) json.RawMessage {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var result any
	switch method {
	case "sessionRead":
		var request struct{ Method string }
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		if request.Method == "getSessionId" {
			result = "fixture"
		} else if request.Method == "getEntries" {
			result = h.entries
		} else {
			t.Fatalf("unexpected session read %s", request.Method)
		}
	case "appendEntry":
		var entry map[string]any
		if err := json.Unmarshal(raw, &entry); err != nil {
			t.Fatal(err)
		}
		entry["type"], entry["id"] = "custom", fmt.Sprintf("entry-%d", len(h.entries)+1)
		h.entries = append(h.entries, entry)
		result = map[string]any{}
	case "isProjectTrusted":
		result = map[string]any{"trusted": !h.untrusted}
	case "getModelRegistryState":
		result = map[string]any{"models": []map[string]any{{"provider": "fixture", "id": "child"}}, "providers": map[string]any{"fixture": map[string]any{"configured": true}}, "registered": []any{}}
	case "getScopedModels":
		result = []any{}
	case "getAllTools":
		tools := []map[string]any{}
		for _, name := range h.tools {
			tools = append(tools, map[string]any{"name": name, "parameters": map[string]any{"type": "object"}})
		}
		result = map[string]any{"tools": tools}
	default:
		t.Fatalf("unexpected native-runtime host call %s", method)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func withRuntimeOwner(t *testing.T, host *runtimeHost, tree *Tree, cwd, agentDir string, exercise func(*owner)) {
	t.Helper()
	withCompletionHost(t, tree, func(o *owner) {
		trusted := true
		services, err := coding.NewServices(coding.ServicesOptions{CWD: cwd, AgentDir: agentDir, ProjectTrusted: &trusted})
		if err != nil {
			t.Fatal(err)
		}
		o.cwd, o.agentDir, o.services = cwd, agentDir, services
		tree.execute, tree.persist = o.executeRun, o.persistChild
		defer o.state.retire()
		exercise(o)
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
}

func runtimeOperation(t *testing.T, o *owner, operation, callID string, args map[string]any) (map[string]any, *callTicket, error) {
	t.Helper()
	ticket, err := o.callScope(nil).begin(nil, callID, operation)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	value, err := o.operation(context.Background(), raw, ticket, nil)
	return value, ticket, err
}

func writeRuntimeModels(t *testing.T, agentDir, endpoint string) {
	t.Helper()
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"fixture-only","models":[{"id":"child","name":"child","contextWindow":32000,"maxTokens":1024}]}}}`, endpoint+"/v1")
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
}

func runtimeAnswer(w http.ResponseWriter, report string) {
	w.Header().Set("Content-Type", "text/event-stream")
	content, _ := json.Marshal(report)
	fmt.Fprintf(w, "data: {\"id\":\"native\",\"object\":\"chat.completion.chunk\",\"model\":\"child\",\"choices\":[{\"index\":0,\"delta\":{\"content\":%s},\"finish_reason\":null}]}\n\ndata: {\"id\":\"native\",\"object\":\"chat.completion.chunk\",\"model\":\"child\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":3,\"total_tokens\":10}}\n\ndata: [DONE]\n\n", content)
}

func runtimeToolAnswer(w http.ResponseWriter, id, name string, args any) {
	w.Header().Set("Content-Type", "text/event-stream")
	encoded, _ := json.Marshal(args)
	arguments, _ := json.Marshal(string(encoded))
	fmt.Fprintf(w, "data: {\"id\":\"native\",\"object\":\"chat.completion.chunk\",\"model\":\"child\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":%s}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"native\",\"object\":\"chat.completion.chunk\",\"model\":\"child\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", id, name, arguments)
}

func runtimeRequestText(body []byte) string {
	var request struct {
		Messages []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	_ = json.Unmarshal(body, &request)
	var text strings.Builder
	for _, message := range request.Messages {
		var value string
		if json.Unmarshal(message.Content, &value) == nil {
			text.WriteString(value)
			continue
		}
		var blocks []struct{ Text string }
		if json.Unmarshal(message.Content, &blocks) == nil {
			for _, block := range blocks {
				text.WriteString(block.Text)
			}
		}
	}
	return text.String()
}

func TestSoftTurnNoticeProducesPartialOutputAndAResumableFreshAllowance(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	if err := os.WriteFile(filepath.Join(directory, "input.txt"), []byte("native input"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var notices atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		text := runtimeRequestText(body)
		number := requests.Add(1)
		wanted := 1
		if strings.Contains(text, "SECOND_ALLOWANCE") {
			wanted = 2
		}
		if count := strings.Count(text, "<system-notice>"); count >= wanted {
			notices.Add(1)
			runtimeAnswer(w, "partial progress with a request to continue")
		} else {
			runtimeToolAnswer(w, fmt.Sprintf("read-%d", number), "read", map[string]any{"path": "input.txt"})
		}
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	config := defaultConfig()
	config.MaxTurns = 2
	tree := testTree(t, config)
	host := &runtimeHost{tools: append([]string{"read", "ls"}, controlNames...)}
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "soft-spawn", map[string]any{"type": "scout", "task": "continue reading until the allowance notice", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		id := value["child"].(Snapshot).ID
		o.settleAck(ticket, false)
		for runNumber := 1; runNumber <= 2; runNumber++ {
			selected := testRun(t, tree, Ref{id})
			outcome, err := tree.wait(context.Background(), selected, 3000)
			if err != nil || outcome.State != Partial || outcome.Text != "partial progress with a request to continue" || !strings.Contains(outcome.Error, "soft turn allowance") {
				t.Fatalf("allowance %d did not settle with a resumable partial report: %#v %v", runNumber, outcome, err)
			}
			tree.acknowledge(selected)
			if runNumber == 1 {
				_, ticket, err := runtimeOperation(t, o, "litter_message", "soft-resume", map[string]any{"id": id, "resume": true, "text": "SECOND_ALLOWANCE"})
				if err != nil {
					t.Fatal(err)
				}
				o.settleAck(ticket, false)
			}
		}
		if notices.Load() != 2 || requests.Load() != 6 {
			t.Fatalf("soft budgets did not nudge once after two turns per allowance: notices=%d requests=%d", notices.Load(), requests.Load())
		}
	})
}

func TestNativeChildResumesByUUIDWithAFreshOwner(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	root := t.TempDir()
	childDir := filepath.Join(root, "child")
	if err := os.Mkdir(childDir, 0o700); err != nil {
		t.Fatal(err)
	}
	agentDir := filepath.Join(root, "agent")
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requests <- string(body)
		if strings.Contains(string(body), "CONTINUE_NATIVE") {
			runtimeAnswer(w, "resumed native report")
		} else {
			runtimeAnswer(w, "initial native report")
		}
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: append([]string{"read", "ls"}, controlNames...)}
	var id ChildID
	first := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, first, root, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "initial", map[string]any{"type": "scout", "task": "ORIGINAL_NATIVE_TASK", "name": "persisted", "model": "fixture/child", "cwd": childDir})
		if err != nil {
			t.Fatal(err)
		}
		child := value["child"].(Snapshot)
		id = child.ID
		if !childUUID.MatchString(string(id)) || child.State != Starting {
			t.Fatalf("admission did not return a native UUID immediately: %#v", child)
		}
		services, err := coding.NewServices(coding.ServicesOptions{CWD: childDir, AgentDir: agentDir})
		if err != nil {
			t.Fatal(err)
		}
		runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := runtime.Resume(string(id), coding.SessionStartOptions{ResourceLoader: coding.NoResources}); err == nil {
			t.Fatal("spawn materialized a session before admission acknowledgment")
		}
		_ = runtime.Close()
		services.Close()
		o.settleAck(ticket, false)
		selected := testRun(t, first, child.Ref)
		outcome, err := first.wait(context.Background(), selected, 3000)
		if err != nil || outcome.State != Completed || outcome.Text != "initial native report" {
			t.Fatalf("native child did not finish: %#v %v", outcome, err)
		}
		if selected.owner.activeLog != nil {
			t.Fatal("settled child retained its native transcript manager")
		}
		first.acknowledge(selected)
	})
	<-requests
	second := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, second, root, agentDir, func(o *owner) {
		if err := o.restoreChildren(); err != nil {
			t.Fatal(err)
		}
		value, _, err := runtimeOperation(t, o, "litter_inspect", "inspect", map[string]any{"id": id})
		if err != nil || value["outcome"].(*Outcome).Text != "initial native report" || value["child"].(Snapshot).Cwd != childDir {
			t.Fatalf("fresh owner lost child metadata or output: %#v %v", value, err)
		}
		if _, _, err := runtimeOperation(t, o, "litter_spawn", "duplicate", map[string]any{"type": "scout", "task": "duplicate", "name": "persisted", "model": "fixture/child"}); err == nil || !strings.Contains(err.Error(), "name already") {
			t.Fatalf("fresh owner lost sibling name uniqueness: %v", err)
		}
		value, ticket, err := runtimeOperation(t, o, "litter_message", "resume", map[string]any{"id": id, "resume": true, "text": "CONTINUE_NATIVE"})
		if err != nil || value["child"].(Snapshot).ID != id {
			t.Fatalf("UUID resume failed: %#v %v", value, err)
		}
		o.settleAck(ticket, false)
		selected := testRun(t, second, Ref{id})
		outcome, err := second.wait(context.Background(), selected, 3000)
		if err != nil || outcome.State != Completed || outcome.Text != "resumed native report" {
			t.Fatalf("fresh owner did not resume native history: %#v %v", outcome, err)
		}
		second.acknowledge(selected)
		if _, _, err := runtimeOperation(t, o, "litter_message", "unknown", map[string]any{"id": "00000000-0000-7000-8000-000000000001", "resume": true, "text": "continue"}); err == nil || err.(*treeError).code != "unavailable" {
			t.Fatalf("unknown UUID did not return unavailable: %v", err)
		}
	})
	resumed := <-requests
	if !strings.Contains(resumed, "ORIGINAL_NATIVE_TASK") || !strings.Contains(resumed, "initial native report") || !strings.Contains(resumed, "CONTINUE_NATIVE") {
		t.Fatalf("native UUID resume lost prior conversation: %s", resumed)
	}
}

func TestEmptyRootToolInventoryLeavesTheChildWithoutTools(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	var count atomic.Int32
	count.Store(-1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		var body struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		count.Store(int32(len(body.Tools)))
		runtimeAnswer(w, "EMPTY_CEILING_VERIFIED")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{}
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "empty-spawn", map[string]any{"type": "worker", "task": "report the empty tool ceiling", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		id := value["child"].(Snapshot).ID
		o.settleAck(ticket, false)
		selected := testRun(t, tree, Ref{id})
		outcome, err := tree.wait(context.Background(), selected, 3000)
		if err != nil || outcome.State != Completed || outcome.Text != "EMPTY_CEILING_VERIFIED" || count.Load() != 0 {
			t.Fatalf("empty root ceiling admitted child tools: %#v tools=%d %v", outcome, count.Load(), err)
		}
		tree.acknowledge(selected)
	})
}

func TestAllowedChildToolsRunWhenHostTrustIsFalse(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1)%2 == 1 {
			runtimeToolAnswer(w, "allowed-write", "write", map[string]any{"path": "allowed.txt", "content": "allowed by the tool list"})
		} else {
			runtimeAnswer(w, "ALLOWED_WITH_HOST_TRUST_FALSE")
		}
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: []string{"read", "ls", "write", "edit", "bash"}, untrusted: true}
	tree := testTree(t, defaultConfig())
	withCompletionHost(t, tree, func(previous *owner) {
		state := &extensionState{}
		o, err := state.ensure(previous.context)
		if err != nil {
			t.Fatal(err)
		}
		defer state.retire()
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "allowed-spawn", map[string]any{"type": "worker", "task": "write the allowed file", "model": "fixture/child", "cwd": directory})
		if err != nil {
			t.Fatal(err)
		}
		id := value["child"].(Snapshot).ID
		o.settleAck(ticket, false)
		selected := testRun(t, o.tree, Ref{id})
		outcome, err := o.tree.wait(context.Background(), selected, 3000)
		if err != nil || outcome.State != Completed || outcome.Text != "ALLOWED_WITH_HOST_TRUST_FALSE" {
			t.Fatalf("host trust added a child permission gate: %#v %v", outcome, err)
		}
		o.tree.acknowledge(selected)
		_, ticket, err = runtimeOperation(t, o, "litter_message", "allowed-resume", map[string]any{"id": id, "resume": true, "text": "write again under the same allowed tool set"})
		if err != nil {
			t.Fatal(err)
		}
		o.settleAck(ticket, false)
		selected = testRun(t, o.tree, Ref{id})
		outcome, err = o.tree.wait(context.Background(), selected, 3000)
		if err != nil || outcome.State != Completed || outcome.Text != "ALLOWED_WITH_HOST_TRUST_FALSE" {
			t.Fatalf("host trust added a resume permission gate: %#v %v", outcome, err)
		}
		o.tree.acknowledge(selected)
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
	contents, err := os.ReadFile(filepath.Join(directory, "allowed.txt"))
	if err != nil || string(contents) != "allowed by the tool list" {
		t.Fatalf("allowed native tool did not execute with host trust false: %q %v", contents, err)
	}
}

func TestOwnerRetirementDuringRecoveryCannotPublishAStaleTree(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	t.Setenv("PIG_CODING_AGENT_DIR", t.TempDir())
	tree := testTree(t, defaultConfig())
	entered, release := make(chan struct{}), make(chan struct{})
	withCompletionHost(t, tree, func(previous *owner) {
		state := &extensionState{}
		done := make(chan error, 1)
		go func() { _, err := state.ensure(previous.context); done <- err }()
		<-entered
		state.mu.Lock()
		published := state.owner != nil
		state.mu.Unlock()
		state.retire()
		close(release)
		err := <-done
		if published || err == nil || err.(*treeError).code != "unavailable" {
			t.Fatalf("recovery exposed or published a retired tree: published=%t error=%v", published, err)
		}
		state.mu.Lock()
		defer state.mu.Unlock()
		if state.owner != nil {
			t.Fatal("retired initialization left an owner installed")
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method != "sessionRead" {
			t.Fatalf("unexpected recovery host call %s", method)
		}
		var request struct{ Method string }
		if err := json.Unmarshal(raw, &request); err != nil {
			t.Fatal(err)
		}
		if request.Method == "getSessionId" {
			return json.RawMessage(`"fixture"`)
		}
		if request.Method == "getEntries" {
			close(entered)
			<-release
			return json.RawMessage(`[]`)
		}
		t.Fatalf("unexpected recovery read %s", request.Method)
		return nil
	})
}
