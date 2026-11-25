package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollaborationTUIMessagePhasesRequireNativeDeliveryAndNoExtraInference(t *testing.T) {
	var receipt collaborationMessageReceipt
	receipt.Mode = "host-busy-message"
	busy := collaborationCLIMessagePhase{Name: "busy-return", ProviderBefore: 2, ProviderAfter: 2}
	busy.Response.Operation, busy.Response.ID = "send", "COLLAB_BUSY"
	busy.Response.StartedNS, busy.Response.ReturnedNS = 2, 3
	settled := collaborationCLIMessagePhase{Name: "busy-settled", ProviderBefore: 2, ProviderAfter: 2}
	settled.Response.Operation, settled.Response.ID = "inspect", "COLLAB_BUSY"
	settled.Response.After.Idle = true
	settled.Response.After.MemoryCopies = 1
	settled.After.Copies = 1
	receipt.Phases = []collaborationCLIMessagePhase{busy, settled}
	receipt.HoldObservedNS, receipt.ReleaseStartNS, receipt.HoldReleasedNS, receipt.SettledNS = 1, 4, 5, 6
	if err := collaborationValidateTUIMessagePhases(receipt); err != nil {
		t.Fatalf("observed busy delivery = %v", err)
	}
	receipt.Phases[1].After.Copies = 0
	if err := collaborationValidateTUIMessagePhases(receipt); err == nil {
		t.Fatal("accepted a missing native append")
	}
	receipt.Phases[1].After.Copies = 1
	receipt.Phases[1].ProviderAfter = 3
	if err := collaborationValidateTUIMessagePhases(receipt); err == nil {
		t.Fatal("accepted unexpected inference")
	}
}

func TestCollaborationTUIMessageRejectsChangedSelectedArtifact(t *testing.T) {
	runner := collaborationCLIRunner{directory: t.TempDir()}
	path := filepath.Join(runner.directory, "result.json")
	if err := os.WriteFile(path, []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	artifact, err := runner.artifact(path, "log")
	if err != nil {
		t.Fatal(err)
	}
	runner.artifacts = []collaborationArtifact{artifact}
	if err := os.WriteFile(path, []byte(`{"ok":false}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := runner.messageReadSelected(path, true); err == nil {
		t.Fatal("accepted a changed selected message artifact")
	}
}
