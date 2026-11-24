package main

import (
	"path/filepath"
	"testing"
)

func TestHookFixtureRequestsWriteAndChecksTheCallerResult(t *testing.T) {
	for _, tc := range []struct {
		prompt string
		file   string
		result string
		done   string
	}{
		{"P02_HOOK_ALLOW", "p02-hook-allow.txt", "Successfully wrote the fixture", "P02_HOOK_ALLOW_DONE"},
		{"P02_HOOK_DENY", "p02-hook-deny.txt", "P02_HOOK_REFUSED", "P02_HOOK_DENY_DONE"},
	} {
		t.Run(tc.prompt, func(t *testing.T) {
			state := newFixtureState(caseDirect)
			state.fixtureWorkspace = t.TempDir()
			request := openAIRequest{
				Model:    "parent",
				Tools:    providerTools(append(append([]string(nil), fileTools...), controlTools...)...),
				Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, tc.prompt)}},
			}
			turn, err := state.decide(request)
			if err != nil {
				t.Fatal(err)
			}
			if turn.kind != "tools" || len(turn.calls) != 1 || turn.calls[0].name != "write" {
				t.Fatalf("first turn = %#v", turn)
			}
			args, ok := turn.calls[0].args.(map[string]string)
			if !ok || args["path"] != filepath.Join(state.fixtureWorkspace, tc.file) || args["content"] != "P02_HOOK_FIXTURE" {
				t.Fatalf("write arguments = %#v", turn.calls[0].args)
			}
			request.Messages = append(request.Messages,
				openAIMessage{Role: "tool", ToolCallID: "call-1", Content: rawJSON(t, tc.result)},
				openAIMessage{Role: "user", Content: rawJSON(t, "A child notification arrived.")},
			)
			turn, err = state.decide(request)
			if err != nil || turn.kind != "text" || turn.text != tc.done {
				t.Fatalf("caller result turn = %#v, %v", turn, err)
			}
			request.Messages[len(request.Messages)-2].Content = rawJSON(t, "unexpected result")
			if _, err := state.decide(request); err == nil {
				t.Fatal("accepted a caller result that does not match the requested phase")
			}
		})
	}
}
