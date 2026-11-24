package main

import "testing"

func TestHookNativeEvidenceRequiresBothAttributedToolResults(t *testing.T) {
	allowed := `{"type":"message","message":{"role":"toolResult","toolCallId":"allow-1","toolName":"write","isError":false,"content":[{"type":"text","text":"Successfully wrote the fixture"}]}}` + "\n"
	denied := `{"type":"message","message":{"role":"toolResult","toolCallId":"deny-1","toolName":"write","isError":true,"content":[{"type":"text","text":"P02_HOOK_REFUSED"}]}}` + "\n"
	phases := []collaborationCLIHookPhase{{Event: collaborationCLIHookEvent{CallID: "allow-1"}}, {Event: collaborationCLIHookEvent{CallID: "deny-1"}}}
	if err := collaborationValidateHookNative([]byte(allowed+denied), phases); err != nil {
		t.Fatalf("attributed native results: %v", err)
	}
	if err := collaborationValidateHookNative([]byte(allowed+allowed), phases); err == nil {
		t.Fatal("accepted a missing denial and duplicated allowed result")
	}
}

func TestHookScenarioIsRegisteredForResearch(t *testing.T) {
	catalog := collaborationTestCatalog(t)
	attempt := collaborationAttempt{
		Scenario: "host-tool-veto", ScenarioRevision: 1,
		Purpose: "research", Channel: "research", Platform: "linux",
	}
	if !collaborationP02ScenarioRegistered(catalog, attempt) {
		t.Fatal("the implemented hook driver is not registered for research evidence")
	}
}
