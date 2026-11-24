package piglitter

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
)

func TestNativeMemberOutcomeUsesOwnedHistoryAndSessionAccounting(t *testing.T) {
	directory := t.TempDir()
	rootID, memberID := storeTestUUID(1), storeTestUUID(2)
	manager := nativeTestManager(t, directory, string(memberID))
	usage := func(input, output, cacheRead, cacheWrite int, cost float64) *ai.Usage {
		return &ai.Usage{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite, TotalTokens: input + output + cacheRead + cacheWrite, Cost: ai.UsageCost{Total: cost}}
	}
	if _, err := manager.AppendMessage(agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant", Content: []ai.AssistantContentBlock{ai.TextContent{Text: "old"}}, StopReason: ai.StopReasonStop, Usage: usage(100, 20, 30, 40, 1)}}); err != nil {
		t.Fatal(err)
	}
	before := manager.Accounting()
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	member := teamMember{Ref: MemberRef{Team: TeamID{Root: rootID, Epoch: 1}, ID: memberID, Generation: 1}, AdmissionID: "admission", Workspace: WorkspacePolicy{Kind: "shared_checkout", Cwd: directory}}
	worker, _ := agentDefaults("worker")
	_, selected, err := tree.admit(admission{kind: "worker", name: "member", model: "fixture/fixed", cwd: directory, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	host := &runtimeHost{}
	withCompletionHost(t, tree, func(o *owner) {
		o.agentDir = directory
		binding := bindMemberFixture(t, o, selected.owner, selected, filepath.Dir(manager.Path()), manager.Path(), testMemberControl{})
		if _, err := manager.AppendUsage("cache_warm", "fixture", "fixed", *usage(3, 4, 5, 6, 0.25), ""); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.AppendMessage(agent.AgentMessage{ToolResult: &agent.ToolResultMessage{Role: "toolResult", ToolCallID: "call", ToolName: "read", Usage: usage(7, 8, 9, 10, 0.5)}}); err != nil {
			t.Fatal(err)
		}
		if _, err := manager.AppendMessage(agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant", Content: []ai.AssistantContentBlock{ai.TextContent{Text: "current outcome"}}, StopReason: ai.StopReasonStop, Usage: usage(11, 12, 13, 14, 0.75)}}); err != nil {
			t.Fatal(err)
		}
		after := manager.Accounting()
		evidence, err := o.memberOutcome(context.Background(), binding)
		if err != nil {
			t.Fatal(err)
		}
		outcome := evidence.outcome
		want := Usage{Input: after.Tokens.Input - before.Tokens.Input, Output: after.Tokens.Output - before.Tokens.Output, CacheRead: after.Tokens.CacheRead - before.Tokens.CacheRead, CacheWrite: after.Tokens.CacheWrite - before.Tokens.CacheWrite, TotalTokens: after.Tokens.Total - before.Tokens.Total, Cost: after.Tokens.Cost - before.Tokens.Cost}
		if outcome.State != Completed || outcome.Text != "current outcome" || outcome.Usage != want {
			t.Fatalf("native outcome = %#v, want completed report and %#v", outcome, want)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage { return host.call(t, method, raw) })
}
