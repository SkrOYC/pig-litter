package piglitter

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestNativeReadinessBindsBeforeFirstSessionWrite(t *testing.T) {
	withMemberControlServer(t, false, func(queue *memberControlQueue, binding *memberBinding, client *http.Client) {
		tree, selected := queue.owner.tree, binding.run
		tree.mu.Lock()
		binding.ready, binding.log = false, memberLogPin{}
		tree.mu.Unlock()
		stores := 0
		tree.persist = func(*record) error { stores++; return nil }
		token := queue.token()
		if status, _ := postAuthority(t, client, token, "/input/start", memberStartingWindow{Window: "window"}); status != http.StatusOK {
			t.Fatalf("starting window status = %d", status)
		}
		if status, _ := postAuthority(t, client, token, "/input/installed", memberStartingInstallation{Window: "window"}); status != http.StatusOK {
			t.Fatalf("starting installation status = %d", status)
		}
		leaf := "metadata"
		path := filepath.Join(binding.owned.SessionDirectory, "fresh_"+string(binding.member.ID)+".jsonl")
		baseline := json.RawMessage(`{"type":"model_change","id":"metadata","provider":"fixture","modelId":"fixed"}`)
		request := memberReadiness{Window: "window", Session: binding.member.ID, Cwd: binding.owned.Cwd, Model: binding.owned.Model, Tools: binding.owned.Tools, SessionFile: path, Leaf: &leaf, Entries: []json.RawMessage{baseline}}
		for range 2 {
			if status, body := postAuthority(t, client, token, "/input/ready", request); status != http.StatusOK {
				t.Fatalf("readiness or its replay = %d %s", status, body)
			}
		}
		if stores != 1 || !queue.ready() || binding.log.file != nil || len(binding.record.pendingInputs) != 1 || binding.record.pendingInputs[0].Text != selected.prompt || *binding.record.pendingInputs[0].After != leaf {
			t.Fatalf("held readiness = stores %d, binding %#v, inputs %#v", stores, binding, binding.record.pendingInputs)
		}
		changed := request
		changed.Model = "fixture/other"
		if status, _ := postAuthority(t, client, token, "/input/ready", changed); status == http.StatusOK {
			t.Fatal("conflicting readiness replay was accepted")
		}
		header, _ := json.Marshal(map[string]any{"type": "session", "id": binding.member.ID, "cwd": binding.owned.Cwd})
		contents := append(append(append(header, '\n'), baseline...), '\n')
		contents = append(contents, []byte(`{"type":"message","id":"seed","message":{"role":"user","content":"work"}}`+"\n")...)
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		entries, err := queue.owner.memberLogEntries(context.Background(), binding)
		if err != nil || len(entries) != 2 || binding.log.baselineCount != 1 {
			t.Fatalf("first native history = %s, baseline=%d, %v", entries, binding.log.baselineCount, err)
		}
	})
}

func TestNativeReadinessDoesNotActivateAfterOwnerLoss(t *testing.T) {
	withMemberControlServer(t, false, func(queue *memberControlQueue, binding *memberBinding, client *http.Client) {
		tree := queue.owner.tree
		tree.mu.Lock()
		binding.ready, binding.log = false, memberLogPin{}
		tree.mu.Unlock()
		entered, release := make(chan struct{}), make(chan struct{})
		tree.persist = func(*record) error { close(entered); <-release; return nil }
		token := queue.token()
		if status, _ := postAuthority(t, client, token, "/input/start", memberStartingWindow{Window: "window"}); status != http.StatusOK {
			t.Fatalf("starting window status = %d", status)
		}
		if status, _ := postAuthority(t, client, token, "/input/installed", memberStartingInstallation{Window: "window"}); status != http.StatusOK {
			t.Fatalf("starting installation status = %d", status)
		}
		path := filepath.Join(binding.owned.SessionDirectory, "fresh_"+string(binding.member.ID)+".jsonl")
		request := memberReadiness{Window: "window", Session: binding.member.ID, Cwd: binding.owned.Cwd, Model: binding.owned.Model, Tools: binding.owned.Tools, SessionFile: path}
		finished := make(chan int, 1)
		go func() {
			status, _ := postAuthority(t, client, token, "/input/ready", request)
			finished <- status
		}()
		<-entered
		queue.Close()
		close(release)
		if status := <-finished; status == http.StatusOK {
			t.Fatal("readiness succeeded after the owner closed")
		}
		tree.mu.Lock()
		ready := binding.ready
		tree.mu.Unlock()
		if ready {
			t.Fatal("owner loss activated the provisional handle")
		}
		tree.persist = nil
	})
}

func TestNativeReadinessKeepsPendingToolsWithinTheCeiling(t *testing.T) {
	for _, test := range []struct {
		name, want string
		ready      []string
		pending    []string
	}{
		{name: "allowed MCP pending", ready: []string{"read"}, pending: []string{"mcp__probe__write"}, want: "ok"},
		{name: "non-MCP pending", pending: []string{"mcp__probe__write", "read"}},
		{name: "unknown pending", ready: []string{"read"}, pending: []string{"mcp__probe__other"}},
		{name: "missing allowed tool", ready: []string{"read"}},
		{name: "duplicate tool", ready: []string{"mcp__probe__write", "read"}, pending: []string{"mcp__probe__write"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			withMemberControlServer(t, false, func(queue *memberControlQueue, binding *memberBinding, client *http.Client) {
				queue.owner.tree.mu.Lock()
				binding.ready, binding.log = false, memberLogPin{}
				binding.ownership.Tools = []string{"mcp__probe__write", "read"}
				binding.owned = cloneMemberOwnership(binding.ownership)
				queue.owned = cloneMemberOwnership(binding.ownership)
				queue.owner.tree.mu.Unlock()
				token := queue.token()
				if status, _ := postAuthority(t, client, token, "/input/start", memberStartingWindow{Window: "window"}); status != http.StatusOK {
					t.Fatalf("starting window status = %d", status)
				}
				if status, _ := postAuthority(t, client, token, "/input/installed", memberStartingInstallation{Window: "window"}); status != http.StatusOK {
					t.Fatalf("starting installation status = %d", status)
				}
				path := filepath.Join(binding.owned.SessionDirectory, "pending_"+string(binding.member.ID)+".jsonl")
				request := memberReadiness{Window: "window", Session: binding.member.ID, Cwd: binding.owned.Cwd, Model: binding.owned.Model, Tools: test.ready, PendingTools: test.pending, SessionFile: path}
				status, body := postAuthority(t, client, token, "/input/ready", request)
				if (status == http.StatusOK) != (test.want == "ok") {
					t.Fatalf("readiness = %d %s", status, body)
				}
			})
		})
	}
}
