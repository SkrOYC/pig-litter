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
)

func TestNativeMCPToolsKeepRootCeilingAcrossCodemodeSearchAndResume(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	t.Setenv("PIG_USE_PI_DIRS", "0")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	childDir := filepath.Join(directory, "child")
	skillDir := filepath.Join(childDir, ".pig", "skills", "probe")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte("---\nname: probe\ndescription: CHILD_SKILL_AVAILABLE\n---\nFULL_SKILL_INSTRUCTIONS\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var callsMu sync.Mutex
	calls := map[string]int{}
	var closed atomic.Int32
	mcpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method == http.MethodDelete {
			closed.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if request.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
				Name            string `json:"name"`
			} `json:"params"`
		}
		if err := json.NewDecoder(request.Body).Decode(&rpc); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if len(rpc.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch rpc.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "native-fixture")
			result = map[string]any{"protocolVersion": rpc.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}
		case "tools/list":
			tools := []map[string]any{}
			for _, name := range []string{"search", "blocked", "late"} {
				tools = append(tools, map[string]any{"name": name, "description": "Fixture " + name, "inputSchema": map[string]any{"type": "object", "properties": map[string]any{}}})
			}
			result = map[string]any{"tools": tools}
		case "tools/call":
			callsMu.Lock()
			calls[rpc.Params.Name]++
			callsMu.Unlock()
			result = map[string]any{"content": []map[string]any{{"type": "text", "text": "MCP_" + strings.ToUpper(rpc.Params.Name)}}}
		default:
			t.Errorf("unexpected MCP method %s", rpc.Method)
			result = map[string]any{}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
	defer mcpServer.Close()
	rootConfigDir := filepath.Join(directory, ".pig")
	if err := os.Mkdir(rootConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := fmt.Sprintf(`{"mcpServers":{"docs":{"url":%q,"exposure":"deferred"}}}`, mcpServer.URL)
	if err := os.WriteFile(filepath.Join(rootConfigDir, "mcp.json"), []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childDir, ".pig", "mcp.json"), []byte(`{"mcpServers":{"docs":{"url":"http://127.0.0.1:1/child-only","exposure":"direct"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	var skillSeen atomic.Bool
	var providerFailure atomic.Pointer[string]
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		text := runtimeRequestText(body)
		number := requests.Add(1)
		if strings.Contains(text, "CHILD_SKILL_AVAILABLE") {
			skillSeen.Store(true)
		}
		var declared struct {
			Tools []struct {
				Function struct{ Name string } `json:"function"`
			} `json:"tools"`
			Messages []struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		_ = json.Unmarshal(body, &declared)
		searchDeclared := false
		for _, tool := range declared.Tools {
			searchDeclared = searchDeclared || tool.Function.Name == "mcp__docs__search"
			if tool.Function.Name == "mcp__docs__blocked" || tool.Function.Name == "mcp__docs__late" {
				failure := "root-excluded MCP tool was declared before deferred discovery"
				providerFailure.Store(&failure)
			}
		}
		if number <= 4 && searchDeclared || number == 5 && !searchDeclared {
			failure := "deferred MCP declaration did not follow tool_search discovery"
			providerFailure.Store(&failure)
		}
		switch number {
		case 1:
			runtimeToolAnswer(w, "skill", "read", map[string]any{"path": skillPath})
		case 2:
			if !strings.Contains(text, "FULL_SKILL_INSTRUCTIONS") {
				failure := "worker could not read its native skill"
				providerFailure.Store(&failure)
			}
			runtimeToolAnswer(w, "script", "codemode", map[string]any{"code": `text({result: await tools.mcp__docs__search({}), blocked: "mcp__docs__blocked" in tools, late: "mcp__docs__late" in tools});`})
		case 3:
			if !strings.Contains(text, "MCP_SEARCH") || !strings.Contains(text, `"blocked":false`) || !strings.Contains(text, `"late":false`) {
				failure := "codemode lost its allowed tool or exposed a tool outside the root ceiling"
				providerFailure.Store(&failure)
			}
			runtimeToolAnswer(w, "denied-search", "tool_search", map[string]any{"query": "blocked"})
		case 4:
			runtimeToolAnswer(w, "allowed-search", "tool_search", map[string]any{"query": "search"})
		case 5:
			var result string
			last := declared.Messages[len(declared.Messages)-1]
			if last.Role != "tool" || json.Unmarshal(last.Content, &result) != nil || !strings.Contains(result, "mcp__docs__search") || strings.Contains(result, "mcp__docs__blocked") {
				failure := "tool_search did not return the allowed deferred MCP tool"
				providerFailure.Store(&failure)
			}
			runtimeToolAnswer(w, "direct", "mcp__docs__search", map[string]any{})
		case 6:
			runtimeAnswer(w, "native MCP/codemode/search ceilings verified")
		case 7:
			runtimeToolAnswer(w, "late-script", "codemode", map[string]any{"code": `text(await tools.mcp__docs__late({}));`})
		case 8:
			if !strings.Contains(text, "MCP_LATE") {
				failure := "late root MCP tool was not available after UUID resume"
				providerFailure.Store(&failure)
			}
			runtimeAnswer(w, "late MCP reachable")
		default:
			failure := "unexpected provider continuation"
			providerFailure.Store(&failure)
			runtimeAnswer(w, failure)
		}
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: append([]string{"read", "ls", "write", "edit", "bash", "codemode", "tool_search", "mcp__docs__search"}, controlNames...)}
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "mcp-spawn", map[string]any{"type": "worker", "task": "use the native skill and root MCP tools", "model": "fixture/child", "cwd": childDir})
		if err != nil {
			t.Fatal(err)
		}
		id := value["child"].(Snapshot).ID
		o.settleAck(ticket, false)
		selected := testRun(t, tree, Ref{id})
		outcome, err := tree.wait(context.Background(), selected, 15000)
		if err != nil || outcome.State != Completed || outcome.Text != "native MCP/codemode/search ceilings verified" {
			t.Fatalf("native MCP run failed: %#v %v", outcome, err)
		}
		tree.acknowledge(selected)
		host.mu.Lock()
		host.tools = append(host.tools, "mcp__docs__late")
		host.mu.Unlock()
		_, ticket, err = runtimeOperation(t, o, "litter_message", "mcp-resume", map[string]any{"id": id, "resume": true, "text": "LATE_MCP"})
		if err != nil {
			t.Fatal(err)
		}
		o.settleAck(ticket, false)
		selected = testRun(t, tree, Ref{id})
		outcome, err = tree.wait(context.Background(), selected, 15000)
		if err != nil || outcome.State != Completed || outcome.Text != "late MCP reachable" {
			t.Fatalf("late MCP run failed: %#v %v", outcome, err)
		}
		tree.acknowledge(selected)
	})
	if failure := providerFailure.Load(); failure != nil {
		t.Fatal(*failure)
	}
	if !skillSeen.Load() || calls["search"] != 2 || calls["late"] != 1 || calls["blocked"] != 0 || closed.Load() != 2 {
		t.Fatalf("native skill/MCP behavior changed: skill=%t calls=%v closed=%d", skillSeen.Load(), calls, closed.Load())
	}
}
