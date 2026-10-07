package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

func appendEvidenceCall(t *testing.T, manager *coding.SessionManager, callID, path, text string, failed bool) {
	t.Helper()
	call := agent.AgentMessage{Assistant: &agent.AssistantMessage{Role: "assistant", Content: []ai.AssistantContentBlock{ai.ToolCall{ID: callID, Name: "read", Arguments: ai.JsonObject{"path": path}}}}}
	result := agent.AgentMessage{ToolResult: &agent.ToolResultMessage{Role: "toolResult", ToolCallID: callID, ToolName: "read", IsError: failed, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: text}}}}
	for _, message := range []agent.AgentMessage{call, result} {
		if _, err := manager.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGenerationMetadataStaysOutsideModelContextAndFitsAdmissionReserve(t *testing.T) {
	manager, err := coding.NewInMemorySessionManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	before := historyBytes(manager)
	if err := appendGeneration(manager, Ref{"0123456789abcdef0123456789abcdef:1", 1}, 1024); err != nil {
		t.Fatal(err)
	}
	if added := historyBytes(manager) - before; added > 512 {
		t.Fatalf("generation metadata exceeds the admission reserve: %d bytes", added)
	}
	if _, err := manager.AppendMessage(agent.AgentMessage{User: &agent.UserMessage{Role: "user", Content: ai.UserText("read notes.txt")}}); err != nil {
		t.Fatal(err)
	}
	messages := manager.BuildContext(nil)
	if len(messages) != 1 || messages[0].User == nil || messages[0].User.Content != ai.UserText("read notes.txt") {
		t.Fatalf("generation metadata entered model context: %#v", messages)
	}
	if len(manager.Entries()) != 2 {
		t.Fatalf("generation metadata was not retained: %#v", manager.Entries())
	}
}

func TestInspectGenerationAndEvidenceKeepPriorFailureSeparateFromRecovery(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "missing-file-check")
	r := tree.records[child.ID]
	manager := r.history
	if err := appendGeneration(manager, child.Ref, tree.config.MaxHistoryBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendMessage(agent.AgentMessage{User: &agent.UserMessage{Role: "user", Content: ai.UserText("read the missing file")}}); err != nil {
		t.Fatal(err)
	}
	appendEvidenceCall(t, manager, "reused-call", "missing.txt", "ENOENT: missing.txt", true)
	tree.finish(r, r.run, Outcome{State: Partial, Text: "No content was read.", Error: "tool read failed: ENOENT: missing.txt"})
	tree.acknowledge(child.Ref)
	resumed, err := tree.message(context.Background(), child.Ref, "resume", "read notes.txt")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendGeneration(manager, resumed.Ref, tree.config.MaxHistoryBytes); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendCustomMessage("litter_completion", "descendant prose is not tool evidence", false, nil); err != nil {
		t.Fatal(err)
	}
	appendEvidenceCall(t, manager, "reused-call", "notes.txt", "Private marker: exact-marker\n", false)
	evidence := projectEvidence(resumed.Ref, historyEntries(manager))
	tree.finish(r, r.run, Outcome{State: Completed, Text: "The child misquoted its marker.", Evidence: &evidence})
	stale := child.Generation
	if _, err := tree.inspect(InspectTarget{ID: child.ID, Generation: &stale}, false, 0, 10); err == nil || err.Error() != "stale child generation 1; current generation is 2; use litter_inspect with id only to refresh" {
		t.Fatalf("stale inspection %v", err)
	}
	current := resumed.Generation
	inspection, err := tree.inspect(InspectTarget{ID: child.ID, Generation: &current}, false, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if inspection.Child.Generation != 2 || inspection.Outcome.State != Completed || inspection.Outcome.ReportProvenance != "child_assistant" || inspection.Outcome.Evidence != nil || inspection.Evidence.CurrentTotal != 1 || inspection.Evidence.CurrentFailed != 0 || inspection.Evidence.PreviousFailureTotal != 1 {
		t.Fatalf("recovery inspection %#v", inspection)
	}
	if len(inspection.Evidence.Current) != 1 || len(inspection.Evidence.PreviousFailures) != 1 {
		t.Fatalf("receipt groups %#v", inspection.Evidence)
	}
	success, failure := inspection.Evidence.Current[0], inspection.Evidence.PreviousFailures[0]
	if success.Generation != 2 || success.Tool != "read" || success.Path != "notes.txt" || success.IsError || success.Text != "Private marker: exact-marker\n" || failure.Generation != 1 || failure.Path != "missing.txt" || !failure.IsError || failure.Text != "ENOENT: missing.txt" {
		t.Fatalf("authoritative receipts %#v %#v", success, failure)
	}
	page, err := tree.inspect(InspectTarget{ID: child.ID}, true, failure.EntryOffset, 1)
	if err != nil || len(page.Entries) != 1 {
		t.Fatalf("raw failure lookup %#v %v", page, err)
	}
	var raw struct {
		ID      string
		Message agent.AgentMessage
	}
	if err := json.Unmarshal(page.Entries[0], &raw); err != nil {
		t.Fatal(err)
	}
	if raw.ID != failure.EntryID || raw.Message.ToolResult == nil || !raw.Message.ToolResult.IsError || raw.Message.ToolResult.Text() != "ENOENT: missing.txt" {
		t.Fatalf("receipt does not locate original failure %#v", raw)
	}
	full, err := tree.inspect(InspectTarget{ID: child.ID}, true, 0, 50)
	if err != nil || !strings.Contains(string(full.Entries[failure.EntryOffset]), "ENOENT: missing.txt") || !strings.Contains(string(full.Entries[success.EntryOffset]), "exact-marker") {
		t.Fatalf("full retained history %#v %v", full, err)
	}
	tree.retireHistory(r)
	frozen, err := tree.inspect(InspectTarget{ID: child.ID}, false, 0, 10)
	if err != nil || frozen.HistoryAvailable || frozen.Evidence.CurrentTotal != 1 || frozen.Evidence.PreviousFailureTotal != 1 || frozen.Evidence.Current[0].Text != "Private marker: exact-marker\n" {
		t.Fatalf("retired history lost frozen evidence %#v %v", frozen, err)
	}
}

func TestInspectReturnsDisplayLabelsAndTranscriptRequestMetadata(t *testing.T) {
	tree := testTree(t, defaultConfig())
	unnamed := testAdmission(t, tree, Ref{}, "scout", "")
	named := testAdmission(t, tree, Ref{}, "scout", "named-child")
	if unnamed.Name != string(unnamed.ID) || unnamed.DisplayLabel != "#1" {
		t.Fatalf("unnamed admission %#v", unnamed)
	}
	if named.Name != "named-child" || named.DisplayLabel != "named-child" {
		t.Fatalf("named admission %#v", named)
	}

	manager := tree.records[unnamed.ID].history
	if err := appendGeneration(manager, unnamed.Ref, tree.config.MaxHistoryBytes); err != nil {
		t.Fatal(err)
	}
	appendEvidenceCall(t, manager, "child-read", "notes.txt", "confirmed tool output", false)

	withoutTranscript, err := tree.inspect(InspectTarget{ID: unnamed.ID}, false, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if withoutTranscript.TranscriptRequested || len(withoutTranscript.Entries) != 0 || withoutTranscript.Child.DisplayLabel != "#1" || withoutTranscript.Evidence.CurrentTotal != 1 || len(withoutTranscript.Evidence.Current) != 1 || withoutTranscript.Evidence.Current[0].Text != "confirmed tool output" {
		t.Fatalf("default inspection %#v", withoutTranscript)
	}

	namedInspection, err := tree.inspect(InspectTarget{ID: named.ID}, false, 0, 10)
	if err != nil || namedInspection.Child.DisplayLabel != "named-child" || namedInspection.TranscriptRequested || len(namedInspection.Entries) != 0 {
		t.Fatalf("named inspection %#v %v", namedInspection, err)
	}

	withTranscript, err := tree.inspect(InspectTarget{ID: unnamed.ID}, true, 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	if !withTranscript.TranscriptRequested || len(withTranscript.Entries) == 0 || withTranscript.NextOffset != len(withTranscript.Entries) || withTranscript.More || withTranscript.Evidence.CurrentTotal != 1 || withTranscript.Evidence.Current[0].Text != "confirmed tool output" {
		t.Fatalf("transcript inspection %#v", withTranscript)
	}
	foundRawResult := false
	for _, entry := range withTranscript.Entries {
		var raw struct {
			Message agent.AgentMessage
		}
		if err := json.Unmarshal(entry, &raw); err != nil {
			t.Fatal(err)
		}
		if raw.Message.ToolResult != nil && raw.Message.ToolResult.Text() == "confirmed tool output" {
			foundRawResult = true
		}
	}
	if !foundRawResult {
		t.Fatalf("transcript omitted the retained tool result %#v", withTranscript.Entries)
	}
}

func TestMailboxPreviewsOmitReceiptsWhileWaitKeepsFrozenEvidence(t *testing.T) {
	tree := testTree(t, defaultConfig())
	parent := testAdmission(t, tree, Ref{}, "scout", "parent")
	child := testAdmission(t, tree, parent.Ref, "scout", "child")
	r := tree.records[child.ID]
	if err := appendGeneration(r.history, child.Ref, tree.config.MaxHistoryBytes); err != nil {
		t.Fatal(err)
	}
	appendEvidenceCall(t, r.history, "child-read", "notes.txt", "confirmed tool output", false)
	evidence := projectEvidence(child.Ref, historyEntries(r.history))
	tree.finish(r, r.run, Outcome{State: Completed, Text: "The child report.", Evidence: &evidence})
	inspection, err := tree.inspect(InspectTarget{ID: parent.ID}, false, 0, 10)
	if err != nil || len(inspection.Mailbox) != 1 || inspection.Mailbox[0].Text != "The child report." || inspection.Mailbox[0].Evidence != nil {
		t.Fatalf("mailbox report preview %#v %v", inspection, err)
	}
	waited, err := tree.wait(context.Background(), child.Ref, 1000)
	if err != nil || waited.State != Completed || waited.Evidence == nil || waited.Evidence.CurrentTotal != 1 || waited.Evidence.Current[0].Text != "confirmed tool output" {
		t.Fatalf("wait lost frozen evidence %#v %v", waited, err)
	}
}

func TestEvidenceBoundsEscapedTextAndKeepsFirstFailureAndLatestResult(t *testing.T) {
	manager, err := coding.NewInMemorySessionManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := Ref{"child:1", 1}
	if err := appendGeneration(manager, ref, 1024*1024); err != nil {
		t.Fatal(err)
	}
	text := strings.Repeat("é<", 600)
	for i := range 20 {
		appendEvidenceCall(t, manager, fmt.Sprintf("read-%d", i), strings.Repeat("<", 512), text, i == 0)
	}
	evidence := projectEvidence(ref, historyEntries(manager))
	encoded, err := json.Marshal(evidence)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) > 4096 || evidence.CurrentTotal != 20 || evidence.CurrentFailed != 1 || !evidence.More || !evidence.Truncated || evidence.FirstFailure == nil || evidence.FirstFailure.CallID != "read-0" {
		t.Fatalf("bounded evidence %d bytes %#v", len(encoded), evidence)
	}
	last := evidence.Current[len(evidence.Current)-1]
	if last.CallID != "read-19" || !last.Truncated || !utf8.ValidString(last.Text) || !strings.HasPrefix(text, last.Text) || last.Text == "" {
		t.Fatalf("latest exact UTF-8 prefix %#v", last)
	}
	if !utf8.ValidString(evidence.FirstFailure.Text) || !strings.HasPrefix(text, evidence.FirstFailure.Text) {
		t.Fatalf("first failure preview changed %#v", evidence.FirstFailure)
	}
}

func TestEvidenceCountsNonTextResultsAndRejectsUnattributedHistory(t *testing.T) {
	manager, err := coding.NewInMemorySessionManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ref := Ref{"child:1", 1}
	if err := appendGeneration(manager, ref, 1024); err != nil {
		t.Fatal(err)
	}
	message := agent.AgentMessage{ToolResult: &agent.ToolResultMessage{Role: "toolResult", ToolName: "read", ToolCallID: "image-read", Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "before"}, ai.ImageContent{Data: "fixture", MimeType: "image/png"}, ai.TextContent{Text: "after"}}}}
	if _, err := manager.AppendMessage(message); err != nil {
		t.Fatal(err)
	}
	evidence := projectEvidence(ref, historyEntries(manager))
	if evidence.CurrentTotal != 1 || len(evidence.Current) != 1 || evidence.Current[0].Text != "beforeafter" || evidence.Current[0].TextBlocks != 2 || evidence.Current[0].NonTextBlocks != 1 || evidence.TextFormat != "concatenated_text_blocks" {
		t.Fatalf("non-text result %#v", evidence)
	}
	unattributed := projectEvidence(ref, []json.RawMessage{json.RawMessage(`{"type":"message","id":"result","message":{"role":"toolResult","toolName":"read","toolCallId":"old","content":[{"type":"text","text":"old output"}],"isError":false}}`)})
	if unattributed.Source != "retained_tool_results" || unattributed.Unavailable != "retained tool results have no generation metadata" {
		t.Fatalf("unattributed history appeared authoritative %#v", unattributed)
	}
}

func TestInspectSchemaAcceptsGenerationAndRejectsUnsafeArguments(t *testing.T) {
	compiler := jsonschema.NewCompiler()
	encoded, err := json.Marshal((&nativeTool{name: "litter_inspect", schema: schemas["litter_inspect"]}).Schema().Parameters)
	if err != nil {
		t.Fatal(err)
	}
	var definition any
	if err := json.Unmarshal(encoded, &definition); err != nil {
		t.Fatal(err)
	}
	if err := compiler.AddResource("inspect.json", definition); err != nil {
		t.Fatal(err)
	}
	schema, err := compiler.Compile("inspect.json")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		args  string
		valid bool
	}{
		{`{"id":"child:1"}`, true},
		{`{"id":"child:1","generation":1,"transcript":true}`, true},
		{`{"id":"child:1","generation":0}`, false},
		{`{"id":"child:1","generation":null}`, false},
		{`{"id":"child:1","generation":"1"}`, false},
		{`{"id":"child:1","generation":1,"ignoreSafety":true}`, false},
	} {
		var args any
		if err := json.Unmarshal([]byte(test.args), &args); err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(args); (err == nil) != test.valid {
			t.Errorf("arguments %s validity=%t, validation %v", test.args, test.valid, err)
		}
	}
}

func TestToolFailurePreservesCanonicalErrorContext(t *testing.T) {
	result := agent.AgentToolResult{IsError: true, Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "ENOENT: no such file, access '/work/missing.txt'"}}}
	failure := toolFailure("read", result)
	if failure != "tool read failed: ENOENT: no such file, access '/work/missing.txt'" {
		t.Fatalf("canonical error context %q", failure)
	}
	terminal := &agent.AssistantMessage{StopReason: "stop", Content: []ai.AssistantContentBlock{ai.TextContent{Text: "The later write succeeded."}}}
	outcome := outcomeFrom(terminal, coding.SessionStats{}, coding.SessionStats{}, failure, false, false)
	if outcome.State != Partial || outcome.Error != "tool read failed: ENOENT: no such file, access '/work/missing.txt'" || outcome.Text != "The later write succeeded." {
		t.Fatalf("later success hid the earlier failure %#v", outcome)
	}
}

func TestCompletionProvenanceSurvivesSDKModelConversion(t *testing.T) {
	outcome := Outcome{Run: Ref{"child:1", 2}, State: Partial, Model: "fixture/child", Text: "An unverified child quote.", Error: "tool read failed: ENOENT"}
	content, err := completionContent(outcome)
	if err != nil {
		t.Fatal(err)
	}
	messages := agent.ConvertToLLM([]agent.AgentMessage{{Custom: map[string]any{"role": "custom", "customType": "litter_completion", "content": content, "display": false, "details": completionDetails{outcome.Run.ID, outcome.Run.Generation}}}}, nil)
	if len(messages) != 1 {
		t.Fatalf("completion model messages %#v", messages)
	}
	message, ok := messages[0].(ai.UserMessage)
	if !ok {
		t.Fatalf("custom-message conversion changed %#v", messages[0])
	}
	blocks, ok := message.Content.(ai.UserContentBlocks)
	if !ok || len(blocks) != 1 {
		t.Fatalf("completion content %#v", message.Content)
	}
	block, ok := blocks[0].(ai.TextContent)
	if !ok {
		t.Fatalf("completion text block %#v", blocks[0])
	}
	var notice completionNotice
	if err := json.Unmarshal([]byte(block.Text), &notice); err != nil {
		t.Fatal(err)
	}
	if notice.Origin != "pig-litter" || notice.Kind != "litter_completion" || notice.ReportProvenance != "child_assistant" || notice.ID != "child:1" || notice.Generation != 2 || notice.State != Partial || notice.Model != "fixture/child" || notice.Report != "An unverified child quote." || notice.Error != "tool read failed: ENOENT" || !strings.Contains(notice.Notice, "not a user message") || !strings.Contains(notice.Notice, "already reported") {
		t.Fatalf("model-visible completion lost provenance or compatibility %#v", notice)
	}
}
