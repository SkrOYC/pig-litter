package pig_litter

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func assistantEvent(reason, text string) string {
	data, _ := json.Marshal(map[string]any{"type": "message_end", "message": map[string]any{"role": "assistant", "provider": "fixture", "model": "scout", "stopReason": reason, "content": []any{map[string]any{"type": "text", "text": text}}, "usage": map[string]any{"input": 2, "output": 3, "totalTokens": 5, "cost": map[string]any{"total": 0.1}}}})
	return string(data) + "\n"
}

func TestChildOutcomes(t *testing.T) {
	good := assistantEvent("stop", "read complete") + "{\"type\":\"agent_end\"}\n"
	cases := []struct {
		name, output string
		exit         int
		killed       bool
		state        Outcome
		report       string
	}{
		{"completed", good, 0, false, Completed, "read complete"},
		{"missing terminal", assistantEvent("stop", "read complete"), 0, false, Partial, "read complete"},
		{"empty", "", 0, false, Partial, "Child ended without a terminal assistant response."},
		{"corrupt", good + "broken\n", 0, false, Partial, "Child emitted invalid or oversized JSON events."},
		{"provider error", assistantEvent("error", "provider failed") + "{\"type\":\"agent_end\"}\n", 0, false, Failed, "provider failed"},
		{"aborted", assistantEvent("aborted", "stopped") + "{\"type\":\"agent_end\"}\n", 0, false, Stopped, "stopped"},
		{"length", assistantEvent("length", "incomplete") + "{\"type\":\"agent_end\"}\n", 0, false, Partial, "incomplete"},
		{"unknown", assistantEvent("unknown", "incomplete") + "{\"type\":\"agent_end\"}\n", 0, false, Partial, "incomplete"},
		{"process error", good, 1, false, Failed, "read complete"},
		{"timeout", good, -1, true, Stopped, "read complete"},
		{"model mismatch", strings.ReplaceAll(good, "\"scout\"", "\"worker\""), 0, false, Failed, "Child resolved a different model than requested."},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			got := parseChild(test.output, test.exit, test.killed, LaunchRequest{AgentType: "scout"}, "fixture/scout")
			if got.State != test.state || got.Report != test.report {
				t.Fatalf("got %+v, want %s %q", got, test.state, test.report)
			}
		})
	}
}

func TestReportBoundAndAggregateUsage(t *testing.T) {
	output := assistantEvent("toolUse", "private intermediate") + assistantEvent("stop", strings.Repeat("猫", 4000)) + "{\"type\":\"agent_end\"}\n"
	got := parseChild(output, 0, false, LaunchRequest{AgentType: "scout"}, "fixture/scout")
	if got.State != Completed || !got.ReportTruncated || len(got.Report) > maxReportBytes || strings.Contains(got.Report, "private intermediate") || got.Usage.TotalTokens != 10 || got.Usage.Cost != 0.2 {
		t.Fatalf("unexpected bounded handback %+v", got)
	}
}

func TestRequestAndLiteralTask(t *testing.T) {
	for _, params := range []map[string]any{{"type": "unknown", "task": "x"}, {"type": "scout", "task": " "}, {"type": "worker", "task": "x", "mode": "background"}, {"type": "scout", "task": "x", "timeoutMs": float64(300001)}, {"type": "scout", "task": "x", "background": true}, {"type": "scout", "task": strings.Repeat("x", maxTaskBytes+1)}} {
		if _, err := parseRequest(params); err == nil {
			t.Fatalf("accepted invalid request %v", params)
		}
	}
	request, err := parseRequest(map[string]any{"type": "scout", "task": "@secret", "timeoutMs": float64(2000)})
	if err != nil {
		t.Fatal(err)
	}
	args := childArgs(request, "fixture/scout")
	if !slices.Contains(args, "--offline") {
		t.Fatal("child startup must suppress package networking")
	}
	if args[len(args)-1] != "Task:\n@secret" || strings.Contains(strings.Join(args, " "), "--approve") {
		t.Fatalf("unsafe task construction %q", args)
	}
}

func TestInvalidOrderingAndKnownFailures(t *testing.T) {
	for _, test := range []struct {
		output string
		exit   int
		state  Outcome
	}{
		{"{\"type\":\"agent_end\"}\n" + assistantEvent("stop", "late"), 0, Partial},
		{assistantEvent("error", "provider failed") + "broken\n", 0, Failed},
		{strings.Repeat("x", maxOutputBytes+1), 1, Failed},
	} {
		if got := parseChild(test.output, test.exit, false, LaunchRequest{AgentType: "scout"}, "fixture/scout"); got.State != test.state {
			t.Fatalf("got %s, want %s", got.State, test.state)
		}
	}
}

func TestNonAssistantMessageShapes(t *testing.T) {
	output := "{\"type\":\"message_end\",\"message\":{\"role\":\"system\",\"content\":\"system text\"}}\n" + assistantEvent("stop", "complete") + "{\"type\":\"agent_end\"}\n"
	got := parseChild(output, 0, false, LaunchRequest{AgentType: "scout"}, "fixture/scout")
	if got.State != Completed || got.Report != "complete" {
		t.Fatalf("unexpected result %+v", got)
	}
}

func TestAllHandbackPathsBounded(t *testing.T) {
	result := toolResult(Result{State: Rejected, AgentType: strings.Repeat("x", 20000), Model: strings.Repeat("y", 20000), Report: strings.Repeat("猫", 20000)})
	var got Result
	if err := json.Unmarshal([]byte(result.Content), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.AgentType) > 64 || len(got.Model) > 256 || len(got.Report) > maxReportBytes || !got.ReportTruncated {
		t.Fatalf("unbounded handback %+v", got)
	}
	if _, err := parseRequest(map[string]any{strings.Repeat("x", 20000): true}); err == nil || len(err.Error()) > 256 {
		t.Fatalf("unbounded request diagnostic %v", err)
	}
}
