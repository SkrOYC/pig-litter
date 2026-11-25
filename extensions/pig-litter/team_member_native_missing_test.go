//go:build teammate_native

package piglitter

import (
	"testing"

	"github.com/MichaelKinsy/PiG/coding/extension"
)

func TestNativeMissingToolRequiresAnUnexecutedTopLevelHostError(t *testing.T) {
	event := extension.ToolExecutionEndEvent{ToolCallID: "one", ToolName: "mcp__probe__write", IsError: true}
	content := []memberResultBlock{{Type: "text", Text: "Tool mcp__probe__write not found"}}
	if !nativeMissingTool(event, content) {
		t.Fatal("the host's missing tool error was not recognized")
	}
	executed := event
	executed.DurationMs = new(int64)
	nested := event
	nested.ParentToolCallID = "parent"
	success := event
	success.IsError = false
	for _, candidate := range []extension.ToolExecutionEndEvent{executed, nested, success} {
		if nativeMissingTool(candidate, content) {
			t.Fatalf("accepted an executed, nested, or successful result: %#v", candidate)
		}
	}
	for _, blocks := range [][]memberResultBlock{{{Type: "text", Text: "different"}}, {{Type: "image", Text: content[0].Text}}, append(content, memberResultBlock{Type: "text", Text: "extra"})} {
		if nativeMissingTool(event, blocks) {
			t.Fatalf("accepted an unrelated tool result: %#v", blocks)
		}
	}
}
