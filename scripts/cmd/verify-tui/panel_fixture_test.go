package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestFixtureGateCompletesARealHTTPStreamAndPreservesRequest(t *testing.T) {
	state := newFixtureState(caseDirect)
	provider, err := startFixtureProvider(state)
	if err != nil {
		t.Fatal(err)
	}
	defer provider.close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const body = `{"model":"held","temperature":0.25,"messages":[{"role":"user","content":"Hold widget child A. HUMAN_RESUME_MARKER"},{"role":"tool","content":"TUI_READ_MARKER"}],"tools":[{"function":{"name":"read"}}]}`
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://127.0.0.1:"+strconv.Itoa(provider.port)+"/chat/completions", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || !state.holding("held-a") {
		t.Fatalf("hold response=%d requests=%#v", response.StatusCode, state.requestEvidence())
	}
	if err := state.releaseHold("held-a"); err != nil {
		t.Fatal(err)
	}
	stream, err := io.ReadAll(response.Body)
	if err != nil || !strings.Contains(string(stream), "CHILD_PRIVATE_RESUME_BOUNDARY") || state.holding("held-a") {
		t.Fatalf("released stream=%s error=%v", stream, err)
	}
	directory := t.TempDir()
	if err := state.saveRequests(directory); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(directory, "request-000.json"))
	if err != nil || string(raw) != body {
		t.Fatalf("raw request=%s error=%v", raw, err)
	}
}

func TestFixtureRequiresNativeHistoryBeforeHumanResume(t *testing.T) {
	state := newFixtureState(caseDirect)
	_, err := state.decide(openAIRequest{Model: "held", Tools: providerTools("read"), Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "Hold widget child A HUMAN_RESUME_MARKER")}}})
	if err == nil || !strings.Contains(err.Error(), "lost native child history") {
		t.Fatalf("missing-history resume error=%v", err)
	}
}

func TestChainWorkerInventoryFollowsIsolatedSelection(t *testing.T) {
	direct := []string{"read", "bash", "edit", "write", "ls", "litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}
	piglet := []string{"read", "bash", "edit", "write", "ls", "litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait", "codemode", "tool_search"}
	for _, positive := range []struct {
		caseName runCase
		tools    []string
	}{{caseDirect, direct}, {casePiglet, piglet}} {
		state := newFixtureState(positive.caseName)
		turn, err := state.decide(openAIRequest{Model: "chain_worker", Tools: providerTools(positive.tools...), Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "Task:\nWORKER_ORIGINAL_TASK")}}})
		if err != nil || turn.kind != "tools" || len(turn.calls) != 1 || turn.calls[0].name != "litter_spawn" {
			t.Fatalf("%s worker spawn=%#v, error=%v", positive.caseName, turn, err)
		}
	}
	for name, negative := range map[string]struct {
		caseName runCase
		tools    []string
	}{
		"direct codemode":              {caseDirect, append(append([]string(nil), direct...), "codemode")},
		"direct search":                {caseDirect, append(append([]string(nil), direct...), "tool_search")},
		"piglet missing both builtins": {casePiglet, direct},
		"piglet missing search":        {casePiglet, append(append([]string(nil), direct...), "codemode")},
		"piglet missing codemode":      {casePiglet, append(append([]string(nil), direct...), "tool_search")},
		"piglet arbitrary extra":       {casePiglet, append(append([]string(nil), piglet...), "fabricated_extra")},
		"direct arbitrary extra":       {caseDirect, append(append([]string(nil), direct...), "fabricated_extra")},
		"missing native read":          {casePiglet, piglet[1:]},
	} {
		t.Run(name, func(t *testing.T) {
			state := newFixtureState(negative.caseName)
			turn, err := state.decide(openAIRequest{Model: "chain_worker", Tools: providerTools(negative.tools...), Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "Task:\nWORKER_ORIGINAL_TASK")}}})
			if err == nil || turn.kind != "" || !strings.Contains(err.Error(), "tool inventory") {
				t.Fatalf("accepted invalid inventory turn=%#v, error=%v", turn, err)
			}
		})
	}
}

func TestNestedFixtureResumesOnlyThroughWorkerTool(t *testing.T) {
	state := newFixtureState(caseDirect)
	tools := providerTools(append(append([]string(nil), fileTools...), controlTools...)...)
	worker := openAIRequest{Model: "chain_worker", Tools: tools, Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "WORKER_ORIGINAL_TASK")}}}
	turn, err := state.decide(worker)
	if err != nil || turn.kind != "tools" || len(turn.calls) != 1 || turn.calls[0].name != "litter_spawn" {
		t.Fatalf("worker spawn=%#v %v", turn, err)
	}
	worker.Messages = append(worker.Messages, openAIMessage{Role: "tool", Content: rawJSON(t, `{"operation":"litter_spawn","state":"starting","child":{"id":"grand-real","name":"chain-grandchild"}}`)})
	turn, err = state.decide(worker)
	if err != nil || turn.kind != "hold" || turn.name != "chain-worker" {
		t.Fatalf("worker hold=%#v %v", turn, err)
	}
	worker.Messages = append(worker.Messages, openAIMessage{Role: "user", Content: rawJSON(t, "HUMAN_CHAIN_RESUME")})
	turn, err = state.decide(worker)
	if err != nil || turn.kind != "hold" || turn.name != "chain-worker-resumed" || len(turn.calls) != 1 || turn.calls[0].name != "litter_message" {
		t.Fatalf("resumed worker=%#v %v", turn, err)
	}
	args, ok := turn.calls[0].args.(map[string]any)
	if !ok || args["id"] != "grand-real" || args["resume"] != true || args["text"] != "NESTED_RESUME_MARKER" {
		t.Fatalf("nested resume args=%#v", args)
	}
	grand := openAIRequest{Model: "chain_grand", Tools: providerTools("read"), Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "GRAND_ORIGINAL_TASK NESTED_RESUME_MARKER")}, {Role: "tool", Content: rawJSON(t, "TUI_READ_MARKER")}}}
	turn, err = state.decide(grand)
	if err != nil || turn.kind != "text" || turn.text != "GRAND_RESUMED_REPORT" {
		t.Fatalf("grandchild resume=%#v %v", turn, err)
	}
}

func TestNestedFixtureRejectsMissingRetainedHistoryDespiteKnownIdentity(t *testing.T) {
	state := newFixtureState(caseDirect)
	state.chainGrandchild = childRef{ID: "remembered-id", Name: "chain-grandchild"}
	tools := providerTools(append(append([]string(nil), fileTools...), controlTools...)...)
	_, err := state.decide(openAIRequest{Model: "chain_worker", Tools: tools, Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "WORKER_ORIGINAL_TASK HUMAN_CHAIN_RESUME")}}})
	if err == nil || !strings.Contains(err.Error(), "lost native grandchild identity") {
		t.Fatalf("missing-history resume error=%v", err)
	}
}

func TestExplicitRootPromptRequiresExactlyOneDeferredNotice(t *testing.T) {
	for _, count := range []int{0, 1, 2} {
		state := newFixtureState(caseDirect)
		messages := []openAIMessage{{Role: "user", Content: rawJSON(t, "ROOT_NOTICE_FIRST")}}
		for range count {
			messages = append(messages, openAIMessage{Role: "user", Content: rawJSON(t, `{"origin":"pig-litter","kind":"litter_completion","report":"LIVE_REPORT_DONE"}`)})
		}
		turn, err := state.decide(openAIRequest{Model: "parent", Messages: messages})
		if count == 1 {
			if err != nil || turn.text != "ROOT_NOTICE_FIRST_DONE" {
				t.Fatalf("single notice turn=%#v error=%v", turn, err)
			}
		} else if err == nil {
			t.Fatalf("accepted %d notice entries", count)
		}
	}
}

func messageExerciseRequest(t *testing.T, history ...[2]string) openAIRequest {
	t.Helper()
	request := openAIRequest{Model: "held", Tools: providerTools("read"), Messages: []openAIMessage{
		{Role: "user", Content: rawJSON(t, "Task:\nHold widget child A until the parent explicitly stops it.")},
		{Role: "tool", Content: rawJSON(t, "TUI_READ_MARKER")},
		{Role: "user", Content: rawJSON(t, "HUMAN_MESSAGE_RUN")},
	}}
	for _, event := range history {
		request.Messages = append(request.Messages, openAIMessage{Role: event[0], Content: rawJSON(t, event[1])})
	}
	return request
}

func messageExerciseTurns() []struct {
	history [][2]string
	want    string
} {
	return []struct {
		history [][2]string
		want    string
	}{
		{[][2]string{{"user", "KITTY_INPUT_MARKER"}}, "KITTY_HANDLED"},
		{[][2]string{{"user", "KITTY_INPUT_MARKER"}, {"assistant", "KITTY_HANDLED"}, {"user", "STEER_MARKER"}}, "STEER_HANDLED"},
		{[][2]string{{"user", "KITTY_INPUT_MARKER"}, {"assistant", "KITTY_HANDLED"}, {"user", "STEER_MARKER"}, {"assistant", "STEER_HANDLED"}, {"user", "FOLLOWUP_MARKER"}}, fixtureReport("MESSAGES_HANDLED")},
	}
}

func TestAcceptedMessagesDrainSeparatelyWithPriorAssistantAcknowledgements(t *testing.T) {
	state := newFixtureState(caseDirect)
	initial, err := state.decide(messageExerciseRequest(t))
	if err != nil || initial.kind != "hold" || state.messageDrain != drainKitty {
		t.Fatalf("initial held request=%#v error=%v", initial, err)
	}
	for _, test := range messageExerciseTurns() {
		turn, err := state.decide(messageExerciseRequest(t, test.history...))
		if err != nil || turn.kind != "text" || turn.text != test.want {
			t.Fatalf("literal separate drain=%#v error=%v", turn, err)
		}
	}
	if state.messageDrain != drainComplete {
		t.Fatal("message exercise did not complete all three provider stages")
	}
}

func TestMessageDrainsRejectSkippedBatchedAndOutOfOrderInputs(t *testing.T) {
	for _, test := range []struct {
		name    string
		prepare int
		history [][2]string
	}{
		{"skipped Kitty", 0, [][2]string{{"user", "STEER_MARKER"}}},
		{"all markers in separate messages", 0, [][2]string{{"user", "KITTY_INPUT_MARKER"}, {"user", "STEER_MARKER"}, {"user", "FOLLOWUP_MARKER"}}},
		{"all markers in one message", 0, [][2]string{{"user", "KITTY_INPUT_MARKER STEER_MARKER FOLLOWUP_MARKER"}}},
		{"reversed first inputs", 0, [][2]string{{"user", "STEER_MARKER"}, {"user", "KITTY_INPUT_MARKER"}}},
		{"missing Kitty acknowledgement", 1, [][2]string{{"user", "KITTY_INPUT_MARKER"}, {"user", "STEER_MARKER"}}},
		{"Kitty acknowledgement in user role", 1, [][2]string{{"user", "KITTY_INPUT_MARKER"}, {"user", "KITTY_HANDLED"}, {"user", "STEER_MARKER"}}},
		{"followup skips steer", 1, [][2]string{{"user", "KITTY_INPUT_MARKER"}, {"assistant", "KITTY_HANDLED"}, {"user", "FOLLOWUP_MARKER"}}},
		{"steer and followup batched", 1, [][2]string{{"user", "KITTY_INPUT_MARKER"}, {"assistant", "KITTY_HANDLED"}, {"user", "STEER_MARKER"}, {"user", "FOLLOWUP_MARKER"}}},
		{"late Kitty acknowledgement", 1, [][2]string{{"user", "KITTY_INPUT_MARKER"}, {"user", "STEER_MARKER"}, {"assistant", "KITTY_HANDLED"}}},
		{"duplicate Kitty", 1, [][2]string{{"user", "KITTY_INPUT_MARKER"}, {"assistant", "KITTY_HANDLED"}, {"user", "KITTY_INPUT_MARKER"}, {"user", "STEER_MARKER"}}},
		{"missing steer acknowledgement", 2, [][2]string{{"user", "KITTY_INPUT_MARKER"}, {"assistant", "KITTY_HANDLED"}, {"user", "STEER_MARKER"}, {"user", "FOLLOWUP_MARKER"}}},
		{"steer acknowledgement in user role", 2, [][2]string{{"user", "KITTY_INPUT_MARKER"}, {"assistant", "KITTY_HANDLED"}, {"user", "STEER_MARKER"}, {"user", "STEER_HANDLED"}, {"user", "FOLLOWUP_MARKER"}}},
		{"repeated final drain", 3, [][2]string{{"user", "KITTY_INPUT_MARKER"}, {"assistant", "KITTY_HANDLED"}, {"user", "STEER_MARKER"}, {"assistant", "STEER_HANDLED"}, {"user", "FOLLOWUP_MARKER"}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := newFixtureState(caseDirect)
			for _, prior := range messageExerciseTurns()[:test.prepare] {
				turn, err := state.decide(messageExerciseRequest(t, prior.history...))
				if err != nil || turn.text != prior.want {
					t.Fatalf("precondition drain=%#v error=%v", turn, err)
				}
			}
			before := state.messageDrain
			if _, err := state.decide(messageExerciseRequest(t, test.history...)); err == nil {
				t.Fatal("invalid distinct-drain history was accepted")
			}
			if state.messageDrain != before {
				t.Fatal("rejected history advanced the provider stage")
			}
		})
	}
}

func TestColdContextRootProducesFiniteTextWithoutToolCalls(t *testing.T) {
	state := newFixtureState(caseDirect)
	turn, err := state.decide(openAIRequest{Model: "parent", Tools: providerTools(append(append([]string(nil), fileTools...), controlTools...)...), Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "COLD_ROOT_HEADER")}}})
	if err != nil || turn.kind != "text" || turn.text != "COLD_ROOT_HEADER_DONE" || len(turn.calls) != 0 || state.modelRequests("parent") != 1 || len(state.requestEvidence()[0].ToolResultCounts) != 0 {
		t.Fatalf("cold context=%#v error=%v", turn, err)
	}
}

func TestChainFixtureUsesNewestMatchingWaitBeforeOlderResumeAck(t *testing.T) {
	const grand = "01a1519f-6e21-75ed-ae69-745a6ebabe4c"
	const spawn = `{"child":{"id":"01a1519f-6e21-75ed-ae69-745a6ebabe4c","parent":"01a1519f-6c4b-75ed-ae69-7457c8cab4aa","depth":2,"name":"chain-grandchild","displayLabel":"chain-grandchild","type":"scout","model":"fixture/chain_grand","cwd":"/tmp/litter-tui-proof-4075976733/workspace","state":"starting"},"handbackTruncated":false,"operation":"litter_spawn","state":"starting"}`
	const resumed = `{"child":{"id":"01a1519f-6e21-75ed-ae69-745a6ebabe4c","parent":"01a1519f-6c4b-75ed-ae69-7457c8cab4aa","depth":2,"name":"chain-grandchild","displayLabel":"chain-grandchild","type":"scout","model":"fixture/chain_grand","cwd":"/tmp/litter-tui-proof-4075976733/workspace","state":"starting"},"delivery":"resumed","handbackTruncated":false,"operation":"litter_message","state":"starting"}`
	const completed = `{"handbackTruncated":false,"operation":"litter_wait","outcome":{"run":{"id":"01a1519f-6e21-75ed-ae69-745a6ebabe4c"},"state":"completed","model":"fixture/chain_grand","text":"GRAND_RESUMED_REPORT","truncated":false,"usage":{"input":1,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":2,"cost":0},"reportProvenance":"child_assistant"},"state":"completed"}`
	for _, test := range []struct {
		name    string
		results []string
		done    bool
		failed  bool
	}{
		{"actual request011 order", []string{resumed, completed}, true, false},
		{"repeated completed wait", []string{resumed, completed, completed}, true, false},
		{"new resume supersedes older wait", []string{completed, resumed}, false, false},
		{"other grandchild wait", []string{resumed, strings.ReplaceAll(completed, grand, "01a1519f-6e21-75ed-ae69-745a6ebabe4d")}, false, false},
		{"wrong latest report", []string{resumed, strings.ReplaceAll(completed, "GRAND_RESUMED_REPORT", "WRONG_REPORT")}, false, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			state := newFixtureState(caseDirect)
			request := openAIRequest{Model: "chain_worker", Tools: providerTools(append(append([]string(nil), fileTools...), controlTools...)...), Messages: []openAIMessage{
				{Role: "user", Content: rawJSON(t, "Task:\nWORKER_ORIGINAL_TASK")},
				{Role: "tool", ToolCallID: "litter-tui-3-0", Content: rawJSON(t, spawn)},
				{Role: "user", Content: rawJSON(t, "HUMAN_CHAIN_RESUME")},
			}}
			for _, result := range test.results {
				request.Messages = append(request.Messages, openAIMessage{Role: "tool", Content: rawJSON(t, result)})
			}
			turn, err := state.decide(request)
			if test.failed {
				if err == nil {
					t.Fatal("wrong completed grandchild result accepted")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if test.done {
				if turn.kind != "text" || turn.text != "WORKER_CHAIN_DONE" || len(turn.calls) != 0 {
					t.Fatalf("completed native wait did not terminate finitely: %#v", turn)
				}
				return
			}
			if turn.kind != "tools" || len(turn.calls) != 1 || turn.calls[0].name != "litter_wait" {
				t.Fatalf("current resume did not wait: %#v", turn)
			}
			arguments, ok := turn.calls[0].args.(map[string]any)
			if !ok || arguments["id"] != grand {
				t.Fatalf("wait changed grandchild identity: %#v", arguments)
			}
		})
	}
}
