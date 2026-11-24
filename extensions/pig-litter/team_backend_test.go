package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestTeamBackendSelection(t *testing.T) {
	for _, example := range []struct {
		request  backendRequest
		kind     backendRequest
		fallback string
		err      string
		probeErr error
	}{
		{request: "", kind: "embedded"},
		{request: "embedded", kind: "embedded"},
		{request: "auto", kind: "tmux"},
		{request: "tmux", kind: "tmux"},
		{request: "auto", kind: "embedded", fallback: "tmux unavailable: no tmux", probeErr: errors.New("no tmux")},
		{request: "tmux", err: "tmux teammate backend is unavailable: no tmux; choose embedded or auto", probeErr: errors.New("no tmux")},
		{request: "iterm2", kind: "iterm2"},
		{request: "iterm2", err: "iterm2 teammate backend is unavailable: no iTerm; choose embedded or auto", probeErr: errors.New("no iTerm")},
		{request: "other", err: "backend must be embedded, tmux, iterm2, or auto"},
	} {
		calls := 0
		choice, err := chooseTeamBackend(example.request, func() error { calls++; return example.probeErr })
		if (example.request == backendAuto || isPaneBackend(example.request)) != (calls == 1) {
			t.Fatalf("%q capability probes = %d", example.request, calls)
		}
		if example.err != "" {
			if err == nil || err.Error() != example.err {
				t.Fatalf("%q error = %v, want %q", example.request, err, example.err)
			}
			continue
		}
		if err != nil || choice.kind != example.kind || choice.fallback != example.fallback {
			t.Fatalf("%q choice = %#v, %v", example.request, choice, err)
		}
	}
}

func TestUnavailableBackendFailsBeforeTeamAdmission(t *testing.T) {
	t.Setenv("TMUX", "")
	t.Setenv("TMUX_PANE", "")
	for _, backend := range []string{"tmux", "iterm2"} {
		t.Run(backend, func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			withCompletionHost(t, tree, func(owner *owner) {
				ticket, err := owner.callScope(nil).begin(nil, "call", "team_spawn")
				if err != nil {
					t.Fatal(err)
				}
				args, err := json.Marshal(map[string]any{"type": "worker", "task": "work", "name": "member", "workspace": map[string]any{"kind": "shared_checkout"}, "backend": backend})
				if err != nil {
					t.Fatal(err)
				}
				_, err = owner.operation(context.Background(), args, ticket, nil)
				if err == nil || !strings.Contains(err.Error(), backend+" teammate backend is unavailable") {
					t.Fatalf("team spawn error = %v", err)
				}
				tree.mu.Lock()
				defer tree.mu.Unlock()
				if len(tree.records) != 0 || tree.live != 0 || owner.teamStore != nil {
					t.Fatalf("unavailable backend admitted a member: records=%d live=%d", len(tree.records), tree.live)
				}
			}, func(string, json.RawMessage) json.RawMessage { return json.RawMessage(`"fixture"`) })
		})
	}
}
