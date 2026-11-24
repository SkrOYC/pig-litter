package piglitter

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func postAuthority(t *testing.T, client *http.Client, token, path string, value any) (int, []byte) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	response, err := postMemberControl(context.Background(), client, path, token, data)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, body
}

func TestMemberAuthorityUsesRootAdmissionAndPlanGate(t *testing.T) {
	store, _, rootID, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "member", workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	worker, _ := agentDefaults("worker")
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, model: "fixture/fixed", cwd: workspace.Cwd, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	path := filepath.Join(directory, "session_"+string(member.Ref.ID)+".jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "id": member.Ref.ID, "cwd": workspace.Cwd})
	if err := os.WriteFile(path, append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	appendNativeFixture(t, path, map[string]any{"type": "message", "id": "old-response", "message": map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{map[string]any{"type": "toolCall", "id": "first", "name": "write"}}}})
	host := &runtimeHost{}
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore, o.cwd = store, workspace.Cwd
		binding := bindMemberFixture(t, o, selected.owner, selected, directory, path, testMemberControl{})
		tree.mu.Lock()
		binding.ownership.Tools = append(binding.ownership.Tools, "codemode", "read", "write")
		slices.Sort(binding.ownership.Tools)
		binding.owned = cloneMemberOwnership(binding.ownership)
		tree.mu.Unlock()
		queue, err := o.newMemberControlQueue(binding.record, binding.run, binding.ownership)
		if err != nil {
			t.Fatal(err)
		}
		defer queue.Close()
		tree.mu.Lock()
		binding.control = queue
		if err := queue.bind(binding); err != nil {
			tree.mu.Unlock()
			t.Fatal(err)
		}
		selected.mu.Lock()
		selected.member = queue
		selected.mu.Unlock()
		tree.mu.Unlock()
		if err := queue.attachAuthority(binding); err != nil {
			t.Fatal(err)
		}
		socketDir := t.TempDir()
		if err := os.Chmod(socketDir, 0o700); err != nil {
			t.Fatal(err)
		}
		server, err := startMemberControlServer(context.Background(), socketDir, queue)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = server.Close() }()
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", server.path)
		}}
		defer transport.CloseIdleConnections()
		client := &http.Client{Transport: transport, Timeout: 5 * time.Second}
		token := queue.token()
		if status, _ := postAuthority(t, client, token, "/member/turn/start", memberTurnRequest{ID: 1}); status != http.StatusOK {
			t.Fatalf("start native turn status = %d", status)
		}
		marker := filepath.Join(workspace.Cwd, "marker")
		args, _ := json.Marshal(map[string]any{"path": marker, "content": "allowed"})
		request := memberToolRequest{ID: "first", Name: "write", Arguments: args}
		tool := func(request memberToolRequest) memberToolDecision {
			t.Helper()
			if request.Turn == 0 {
				request.Turn = 1
			}
			status, body := postAuthority(t, client, token, "/member/tool", request)
			var decision memberToolDecision
			if status != http.StatusOK || json.Unmarshal(body, &decision) != nil {
				t.Fatalf("native tool decision = %d %s", status, body)
			}
			return decision
		}
		observe := func(turn uint64, entry string, requests ...memberToolRequest) {
			t.Helper()
			calls := make([]any, 0, len(requests))
			for _, request := range requests {
				calls = append(calls, map[string]any{"type": "toolCall", "id": request.ID, "name": request.Name})
			}
			appendNativeFixture(t, path, map[string]any{"type": "message", "id": entry, "message": map[string]any{"role": "assistant", "stopReason": "toolUse", "content": calls}})
			if status, body := postAuthority(t, client, token, "/member/response", memberResponseRequest{Turn: turn, EntryID: entry}); status != http.StatusOK {
				t.Fatalf("native response observation = %d %s", status, body)
			}
		}
		beforeResponse := request
		beforeResponse.ID = "before-response"
		if decision := tool(beforeResponse); !decision.Block {
			t.Fatal("tool ran before the first provider response")
		}
		if status, _ := postAuthority(t, client, token, "/member/response", memberResponseRequest{Turn: 1, EntryID: "old-response"}); status == http.StatusOK {
			t.Fatal("response from before the execution baseline was accepted")
		}
		appendNativeFixture(t, path, map[string]any{"type": "message", "id": "response", "message": map[string]any{"role": "assistant", "stopReason": "toolUse", "content": []any{map[string]any{"type": "toolCall", "id": "first", "name": "write"}}}})
		if status, body := postAuthority(t, client, token, "/member/response", memberResponseRequest{Turn: 1, EntryID: "response"}); status != http.StatusOK {
			t.Fatalf("native response observation = %d %s", status, body)
		}
		if decision := tool(request); !decision.Block || !strings.Contains(decision.Reason, "plan approval") {
			t.Fatalf("unapproved write = %#v", decision)
		}
		deniedEnd := memberToolResult{Turn: 1, CallID: "first", Name: "write", IsError: true, Content: []memberResultBlock{{Type: "text", Text: "plan approval is required"}}}
		if status, body := postAuthority(t, client, token, "/member/tool/end", deniedEnd); status != http.StatusOK {
			t.Fatalf("plan denial observation = %d %s", status, body)
		}
		tree.mu.Lock()
		failure := selected.nativeFailure
		tree.mu.Unlock()
		if failure != "" {
			t.Fatalf("plan denial became a runtime failure: %s", failure)
		}
		_, roster, pending, err := store.SnapshotWithReservations()
		if err != nil || len(roster) != 1 || len(pending) != 0 || roster[0].Ref != member.Ref {
			t.Fatalf("native admission = %#v %#v %v", roster, pending, err)
		}
		if decision := tool(memberToolRequest{ID: "not-observed", Name: "read", Arguments: json.RawMessage(`{}`)}); !decision.Block {
			t.Fatal("tool without an observed assistant call was authorized")
		}
		observe(1, "name-response", memberToolRequest{ID: "observed-name", Name: "read"})
		if decision := tool(memberToolRequest{ID: "observed-name", Name: "write", Arguments: args}); !decision.Block {
			t.Fatal("tool was authorized with a different observed name")
		}
		plan, err := store.submitPlan(member.Ref, selected.noticeID, "plan", "Write the marker.", 60000)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.decidePlan(plan.ID, selected.noticeID, "approve", ""); err != nil {
			t.Fatal(err)
		}
		if decision := tool(request); !decision.Block {
			t.Fatal("replayed denied call became authorized")
		}
		request.ID = "second"
		observe(1, "response-second", request)
		if decision := tool(request); decision.Block {
			t.Fatalf("approved write was blocked: %#v", decision)
		}
		if decision := tool(request); !decision.Block {
			t.Fatal("an issued tool authorization was replayed")
		}
		failedEnd := memberToolResult{Turn: 1, CallID: "second", Name: "write", IsError: true, Content: []memberResultBlock{{Type: "text", Text: "disk failed"}}}
		if status, body := postAuthority(t, client, token, "/member/tool/end", failedEnd); status != http.StatusOK {
			t.Fatalf("failed tool observation = %d %s", status, body)
		}
		status, _ := postAuthority(t, client, token, "/member/tool/end", failedEnd)
		tree.mu.Lock()
		failure = selected.nativeFailure
		tree.mu.Unlock()
		if status == http.StatusOK || !strings.Contains(failure, "disk failed") {
			t.Fatalf("tool failure observation was replayed or lost: %s", failure)
		}
		if err := os.WriteFile(marker, []byte("allowed"), 0o600); err != nil {
			t.Fatal(err)
		}
		if data, err := os.ReadFile(marker); err != nil || string(data) != "allowed" {
			t.Fatalf("approved write marker = %q, %v", data, err)
		}
		if decision := tool(memberToolRequest{ID: "foreign", Name: "bash", Arguments: json.RawMessage(`{}`)}); !decision.Block {
			t.Fatal("tool outside the bound ceiling was allowed")
		}
		if decision := tool(memberToolRequest{ID: "script/1", Parent: "missing", Name: "write", Arguments: args}); !decision.Block {
			t.Fatal("nested call without its parent was allowed")
		}
		observe(1, "response-script", memberToolRequest{ID: "script", Name: "codemode"})
		if decision := tool(memberToolRequest{ID: "script", Name: "codemode", Arguments: json.RawMessage(`{"code":"run"}`)}); decision.Block {
			t.Fatalf("authorized parent tool = %#v", decision)
		}
		if decision := tool(memberToolRequest{ID: "script/1", Parent: "script", Name: "write", Arguments: args}); decision.Block {
			t.Fatalf("authorized nested tool = %#v", decision)
		}
		list := memberOperationRequest{Turn: 1, CallID: "list", Args: json.RawMessage(`{}`)}
		if status, _ := postAuthority(t, client, token, "/member/child/list", list); status == http.StatusOK {
			t.Fatal("native operation ran without tool authorization")
		}
		observe(1, "response-list", memberToolRequest{ID: "list", Name: "litter_list"})
		if decision := tool(memberToolRequest{ID: "list", Name: "litter_list", Arguments: json.RawMessage(`{}`)}); decision.Block {
			t.Fatalf("native child list authorization = %#v", decision)
		}
		status, body := postAuthority(t, client, token, "/member/child/list", list)
		var result memberOperationReply
		if status != http.StatusOK || json.Unmarshal(body, &result) != nil || result.IsError || !strings.Contains(result.Content, `"operation":"litter_list"`) {
			t.Fatalf("native child list = %d %s", status, body)
		}
		if status, _ := postAuthority(t, client, token, "/member/child/list", list); status == http.StatusOK {
			t.Fatal("native operation replayed the same call")
		}
		finished := memberToolResult{Turn: 1, CallID: "list", Name: "litter_list", Content: []memberResultBlock{{Type: "text", Text: result.Content}}}
		if status, _ := postAuthority(t, client, token, "/member/result", finished); status != http.StatusOK {
			t.Fatalf("native result status = %d", status)
		}
		if status, _ := postAuthority(t, client, token, "/member/result", finished); status == http.StatusOK {
			t.Fatal("native tool result was accepted twice")
		}
		spawnArgs := json.RawMessage(`{"type":"scout","task":"inspect","name":"another","workspace":{"kind":"shared_checkout"}}`)
		observe(1, "response-team-spawn", memberToolRequest{ID: "team-spawn", Name: "team_spawn"})
		if decision := tool(memberToolRequest{ID: "team-spawn", Name: "team_spawn", Arguments: spawnArgs}); decision.Block {
			t.Fatalf("native team spawn authorization = %#v", decision)
		}
		status, body = postAuthority(t, client, token, "/member/team/spawn", memberOperationRequest{Turn: 1, CallID: "team-spawn", Args: spawnArgs})
		if status != http.StatusOK || json.Unmarshal(body, &result) != nil || !result.IsError || !strings.Contains(result.Content, "lead_required") {
			t.Fatalf("native teammate could start another teammate: %d %s", status, body)
		}
		missing := memberMissingTool{Turn: 1, CallID: "missing", Name: "mcp__probe__write", EntryID: "response-missing"}
		if status, _ := postAuthority(t, client, token, "/member/tool/missing", missing); status == http.StatusOK {
			t.Fatal("unobserved missing tool was accepted")
		}
		observe(1, "response-missing", memberToolRequest{ID: "missing", Name: "mcp__probe__write"})
		wrong := missing
		wrong.Name = "write"
		if status, _ := postAuthority(t, client, token, "/member/tool/missing", wrong); status == http.StatusOK {
			t.Fatal("missing tool changed its observed name")
		}
		if status, body := postAuthority(t, client, token, "/member/tool/missing", missing); status != http.StatusOK {
			t.Fatalf("missing tool observation = %d %s", status, body)
		}
		if status, _ := postAuthority(t, client, token, "/member/tool/missing", missing); status == http.StatusOK {
			t.Fatal("missing tool observation was replayed")
		}
		if decision := tool(memberToolRequest{ID: "missing", Name: missing.Name, Arguments: json.RawMessage(`{}`)}); !decision.Block {
			t.Fatal("missing tool was later authorized")
		}
		if status, _ := postAuthority(t, client, token, "/member/tool/end", memberToolResult{Turn: 1, CallID: "missing", Name: missing.Name, Content: []memberResultBlock{{Type: "text", Text: "executed"}}}); status == http.StatusOK {
			t.Fatal("missing tool accepted an uncorrelated executed result")
		}
		nestedMissing := memberMissingTool{Turn: 1, CallID: "script/2", Name: "mcp__probe__write", EntryID: "response-script"}
		if status, _ := postAuthority(t, client, token, "/member/tool/missing", nestedMissing); status == http.StatusOK {
			t.Fatal("nested call was accepted as an observed top-level missing tool")
		}
		if status, _ := postAuthority(t, client, token, "/member/turn/end", memberTurnRequest{ID: 1}); status != http.StatusOK {
			t.Fatalf("end native turn status = %d", status)
		}
		if decision := tool(memberToolRequest{ID: "late", Name: "read", Arguments: json.RawMessage(`{}`)}); !decision.Block {
			t.Fatal("tool was authorized after the native turn ended")
		}
		if status, _ := postAuthority(t, client, token, "/member/turn/start", memberTurnRequest{ID: 1}); status == http.StatusOK {
			t.Fatal("native turn ID was reused")
		}
		if status, _ := postAuthority(t, client, token, "/member/turn/start", memberTurnRequest{ID: 2}); status != http.StatusOK {
			t.Fatal("next native turn was not started")
		}
		if decision := tool(memberToolRequest{Turn: 1, ID: "list", Name: "litter_list", Arguments: json.RawMessage(`{}`)}); !decision.Block {
			t.Fatal("stale turn authorized a tool in the next turn")
		}
		if status, _ := postAuthority(t, client, token, "/member/response", memberResponseRequest{Turn: 2, EntryID: "response-list"}); status == http.StatusOK {
			t.Fatal("previous turn's response was accepted in the next turn")
		}
		observe(2, "response-list-turn-two", memberToolRequest{ID: "list", Name: "litter_list"})
		if decision := tool(memberToolRequest{Turn: 2, ID: "list", Name: "litter_list", Arguments: json.RawMessage(`{}`)}); decision.Block {
			t.Fatalf("next turn authorization = %#v", decision)
		}
		if status, _ := postAuthority(t, client, token, "/member/child/list", list); status == http.StatusOK {
			t.Fatal("stale turn used the next turn's authorization")
		}
		list.Turn = 2
		status, body = postAuthority(t, client, token, "/member/child/list", list)
		if status != http.StatusOK || json.Unmarshal(body, &result) != nil || result.IsError {
			t.Fatalf("next turn child list = %d %s", status, body)
		}
		if status, _ := postAuthority(t, client, token, "/member/result", finished); status == http.StatusOK {
			t.Fatal("stale turn completed the next turn's call")
		}
		finished.Turn, finished.Content[0].Text = 2, result.Content
		if status, _ := postAuthority(t, client, token, "/member/result", finished); status != http.StatusOK {
			t.Fatalf("next turn result status = %d", status)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
}
