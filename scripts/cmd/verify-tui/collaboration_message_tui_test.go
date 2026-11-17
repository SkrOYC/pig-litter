package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestCollaborationTUIMessageSnapshotsCountOnlySelectedID(t *testing.T) {
	root := t.TempDir()
	probe := collaborationTUIMessageProbe{directory: root, bridge: collaborationTUIMessageBridge{SessionFile: filepath.Join(root, "session.jsonl")}}
	if snapshot, err := probe.disk("missing", "P02_IDLE"); err != nil || snapshot.Exists || snapshot.Copies != 0 || snapshot.Path != "" {
		t.Fatalf("missing native session = %#v, %v", snapshot, err)
	}
	data := "{\"type\":\"session\",\"id\":\"root\"}\n{\"type\":\"custom_message\",\"customType\":\"p02_tui_probe\",\"details\":{\"message_id\":\"P02_IDLE\"}}\n{\"type\":\"custom_message\",\"customType\":\"p02_tui_probe\",\"details\":{\"message_id\":\"OTHER\"}}\n"
	if err := os.WriteFile(probe.bridge.SessionFile, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot, err := probe.disk("after", "P02_IDLE")
	if err != nil || !snapshot.Exists || snapshot.Copies != 1 || snapshot.Path != "after.jsonl" {
		t.Fatalf("native session snapshot = %#v, %v", snapshot, err)
	}
	retained, err := os.ReadFile(filepath.Join(root, snapshot.Path))
	if err != nil || string(retained) != data {
		t.Fatalf("retained native snapshot = %q, %v", retained, err)
	}
}

func TestCollaborationTUIMessageIncompleteReceiptFailsClosed(t *testing.T) {
	root := t.TempDir()
	probe := collaborationTUIMessageProbe{mode: "host-idle-message", directory: root}
	if err := probe.save(observedProcess{}, nil); err == nil {
		t.Fatal("incomplete message probe was accepted")
	}
	data, err := os.ReadFile(filepath.Join(root, "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt collaborationTUIMessageReceipt
	if err := json.Unmarshal(data, &receipt); err != nil || receipt.Status != "inconclusive" || receipt.Error == "" {
		t.Fatalf("retained message receipt = %#v, %v", receipt, err)
	}
}

func TestCollaborationTUIMessageExtensionIdentityDoesNotDependOnAttemptName(t *testing.T) {
	if collaborationTUIMessageExtensionName != "p02-message-probe" {
		t.Fatalf("unexpected private extension identity %q", collaborationTUIMessageExtensionName)
	}
	source := filepath.Join(t.TempDir(), "arbitrary-driver-name")
	if err := os.WriteFile(source, []byte("private fixture executable"), 0o700); err != nil {
		t.Fatal(err)
	}
	digest, err := fileSHA256(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, label := range []string{"first-task", "different-attempt"} {
		t.Run(label, func(t *testing.T) {
			path, err := collaborationStageTUIMessageExtension(t.TempDir(), source, digest)
			if err != nil || filepath.Base(path) != collaborationTUIMessageExtensionName {
				t.Fatalf("staged extension = %q, %v", path, err)
			}
			actual, err := fileSHA256(path)
			if err != nil || actual != digest {
				t.Fatalf("staged extension digest = %q, %v", actual, err)
			}
		})
	}
	if _, err := collaborationStageTUIMessageExtension(t.TempDir(), source, "wrong digest"); err == nil {
		t.Fatal("accepted a staged extension with the wrong digest")
	}
}
