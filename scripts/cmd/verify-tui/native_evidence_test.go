package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeSessionProjectionKeepsLatestMetadataAndReceipts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "root.jsonl")
	fixture := `{"type":"session","id":"root-real","cwd":"/private/work"}
{"type":"custom","customType":"litter_child_v1","data":{"rootId":"root-real","id":"child-real","parentId":"root-real","name":"worker","state":"running","paused":false}}
{"type":"message","message":{"role":"assistant","content":[{"type":"text","text":"native-private-token"}]}}
{"type":"custom","customType":"litter_child_v1","data":{"rootId":"root-real","id":"child-real","parentId":"root-real","name":"worker","state":"completed","paused":true,"notices":[{"nonce":"receipt-real","consumed":false}],"latest":{"state":"completed","text":"report","run":{"id":"child-real"}}}}
`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readNativeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	child := got.Children["child-real"]
	if got.ID != "root-real" || got.Cwd != "/private/work" || got.Entries != 4 || got.LitterEntries != 2 || len(got.Messages) != 1 || !child.Paused || child.State != "completed" || len(child.Notices) != 1 || child.Latest.Run.ID != "child-real" {
		t.Fatalf("projection = %#v child=%#v", got, child)
	}
	if string(got.Raw) != fixture {
		t.Fatal("native evidence changed its source records")
	}
}

func TestNativeSessionProjectionRejectsMalformedRecords(t *testing.T) {
	for _, raw := range []string{"{", `{"type":"message","message":{}}`} {
		path := filepath.Join(t.TempDir(), "session.jsonl")
		if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := readNativeSession(path); err == nil {
			t.Fatalf("accepted malformed native session %q", raw)
		}
	}
}

func TestNativeCompletionRecordProjectionPreservesReceipt(t *testing.T) {
	const raw = `{"type":"session","id":"root-real","cwd":"/private"}
{"type":"custom_message","id":"native-entry","timestamp":"2026-10-18T22:00:00.000Z","customType":"litter_completion","details":{"childId":"child-real","receipt":"receipt-real"},"content":"report","display":false}
`
	path := filepath.Join(t.TempDir(), "native.jsonl")
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readNativeSession(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Completions) != 1 || got.Completions[0].EntryID != "native-entry" || got.Completions[0].ChildID != "child-real" || got.Completions[0].Receipt != "receipt-real" || got.Completions[0].Timestamp != "2026-10-18T22:00:00.000Z" || got.Completions[0].Content != "report" || got.Completions[0].Display || len(got.Completions[0].Raw) == 0 || string(got.Raw) != raw {
		t.Fatalf("native completion projection=%#v", got)
	}
}
