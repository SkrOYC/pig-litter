package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCanonicalReceiptRejectsMissingWrongAndStrayReservations(t *testing.T) {
	latest := &nativeOutcome{State: "completed", Model: "fixture/quick", Text: "latest", ReportProvenance: "child_assistant", Usage: json.RawMessage(`{}`)}
	latest.Run.ID = "child"
	child := nativeChild{ID: "child", NoticeID: "A", Latest: latest}
	canonical := nativeNotice{Nonce: "A", Outcome: latest}
	wrongOutcome := *latest
	wrongOutcome.Text = "different report for the same UUID"
	child.Notices = []json.RawMessage{rawJSON(t, canonical)}
	if receipt, err := pendingReceipt(child); err != nil || receipt != "A" {
		t.Fatalf("canonical pending=%s %v", receipt, err)
	}
	for _, test := range []struct {
		name    string
		notices []nativeNotice
	}{
		{"missing", nil},
		{"wrong nonce", []nativeNotice{{Nonce: "B", Outcome: latest}}},
		{"stray nonce", []nativeNotice{canonical, {Nonce: "B", Outcome: latest}}},
		{"wrong canonical outcome", []nativeNotice{{Nonce: "A", Outcome: &wrongOutcome}}},
	} {
		child.Notices = nil
		for _, notice := range test.notices {
			child.Notices = append(child.Notices, rawJSON(t, notice))
		}
		if _, err := pendingReceipt(child); err == nil {
			t.Fatalf("accepted %s before promotion", test.name)
		}
		if test.name == "wrong nonce" || test.name == "stray nonce" {
			if checkReservation(child, "A", false) == nil {
				t.Fatalf("accepted %s after promotion", test.name)
			}
		}
	}
	older := *latest
	older.Text = "unrelated older outcome"
	canonical.Consumed = true
	child.Notices = []json.RawMessage{rawJSON(t, canonical), rawJSON(t, nativeNotice{Nonce: "old", Outcome: &older})}
	if err := checkReservation(child, "A", false); err != nil {
		t.Fatalf("rejected consumed canonical plus unrelated pending: %v", err)
	}
	child.Notices = child.Notices[1:]
	if err := checkReservation(child, "A", false); err != nil {
		t.Fatalf("rejected absent canonical plus unrelated pending: %v", err)
	}
	canonical.Consumed = false
	child.Notices = append(child.Notices, rawJSON(t, canonical))
	if err := checkReservation(child, "different", true); err == nil {
		t.Fatal("accepted receipt different from NoticeID")
	}
	child.Notices = []json.RawMessage{rawJSON(t, map[string]any{"nonce": "A", "outcome": latest})}
	if _, err := pendingReceipt(child); err == nil {
		t.Fatal("accepted absent consumption field as an explicit unconsumed receipt")
	}
	child.NoticeID = ""
	child.Notices = []json.RawMessage{rawJSON(t, canonical)}
	if _, err := pendingReceipt(child); err == nil {
		t.Fatal("accepted missing NoticeID")
	}
}

func TestCanonicalUsageReorderingCannotHideStrayNonce(t *testing.T) {
	var latest nativeOutcome
	const original = `{"run":{"id":"child"},"state":"completed","model":"fixture/quick","text":"report","truncated":false,"reportProvenance":"child_assistant","usage":{"input":1,"output":2}}`
	if err := json.Unmarshal([]byte(original), &latest); err != nil {
		t.Fatal(err)
	}
	child := nativeChild{ID: "child", NoticeID: "A", Latest: &latest, Notices: []json.RawMessage{
		rawJSON(t, map[string]any{"nonce": "A", "consumed": true, "outcome": json.RawMessage(original)}),
		json.RawMessage(`{"nonce":"B","consumed":false,"outcome":{"run":{"id":"child"},"state":"completed","model":"fixture/quick","text":"report","truncated":false,"reportProvenance":"child_assistant","usage":{"output":2,"input":1}}}`),
	}}
	if checkReservation(child, "A", false) == nil {
		t.Fatal("reordered same-outcome stray reservation escaped correlation")
	}
}

func TestOutcomeJSONSemanticsKeepUnknownFieldsAndExactNumbers(t *testing.T) {
	for _, test := range []struct {
		left, right string
		equal       bool
	}{
		{`{"future":{"a":1,"b":[2,3]},"usage":{"input":1,"output":2}}`, `{"usage":{"output":2.0,"input":1e0},"future":{"b":[2,3],"a":1}}`, true},
		{`{"future":9007199254740992}`, `{"future":9007199254740993}`, false},
		{`{"future":9007199254740993}`, `{"future":9007199254740993.0}`, true},
		{`{"future":[1,2]}`, `{"future":[2,1]}`, false},
		{`{"future":null}`, `{}`, false},
		{`{"future":1}`, `{"future":1} {}`, false},
	} {
		if got := equalJSON([]byte(test.left), []byte(test.right)); got != test.equal {
			t.Fatalf("JSON semantics %s / %s equal=%v", test.left, test.right, got)
		}
	}
	var left, right nativeOutcome
	if err := json.Unmarshal([]byte(`{"run":{"id":"child"},"state":"completed","usage":{"input":1},"future":9007199254740992}`), &left); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(`{"run":{"id":"child"},"state":"completed","usage":{"input":1},"future":9007199254740993}`), &right); err != nil {
		t.Fatal(err)
	}
	if sameOutcome(&left, &right) {
		t.Fatal("meaningful unknown outcome number was rounded away")
	}
}

func TestEmittedSpawnExpectationsUseCallIdentityAcrossParallelOrder(t *testing.T) {
	state := newFixtureState(caseDirect)
	state.fixtureRoot = "native-root"
	state.fixtureWorkspace = "/known/workspace"
	provider := fixtureProvider{state: state}
	writer := httptest.NewRecorder()
	provider.writeTools(writer, openAIRequest{Model: "parent"}, []toolCall{
		{name: "litter_spawn", args: map[string]string{"name": "a", "type": "scout", "model": "fixture/held", "task": "task A"}},
		{name: "litter_spawn", args: map[string]string{"name": "b", "type": "worker", "model": "fixture/quick", "cwd": "subdir", "task": "task B"}},
	})
	if !strings.Contains(writer.Body.String(), `"id":"litter-tui-1-0"`) {
		t.Fatalf("actual emitted call missing: %s", writer.Body.String())
	}
	if state.expectedSpawns["litter-tui-1-0"].Cwd != "/known/workspace" || state.expectedSpawns["litter-tui-1-1"].Cwd != "/known/workspace/subdir" {
		t.Fatal("expected CWD did not use emitted request/known fixture directory")
	}
	state.mu.Lock()
	userName := state.requestChildNameLocked(openAIRequest{Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "Task:\ntask A")}}})
	assistantName := state.requestChildNameLocked(openAIRequest{Messages: []openAIMessage{{Role: "assistant", Content: rawJSON(t, "Task:\ntask A")}}})
	state.mu.Unlock()
	if userName != "a" || assistantName != "" {
		t.Fatalf("original task attribution user=%q assistant=%q", userName, assistantName)
	}
	admissions := map[string]admissionChild{
		"uuid-b": {ID: "uuid-b", Name: "b", Parent: "native-root", Type: "worker", Model: "fixture/quick", Cwd: "/known/workspace/subdir"},
		"uuid-a": {ID: "uuid-a", Name: "a", Parent: "native-root", Type: "scout", Model: "fixture/held", Cwd: "/known/workspace"},
	}
	calls := map[string]string{"uuid-b": "litter-tui-1-1", "uuid-a": "litter-tui-1-0"}
	ids, err := expectedIdentityIDs(admissions, calls, state.expectedSpawns)
	if err != nil || ids["a"] != "uuid-a" || ids["b"] != "uuid-b" {
		t.Fatalf("independent call identities=%v %v", ids, err)
	}
	calls["uuid-a"], calls["uuid-b"] = calls["uuid-b"], calls["uuid-a"]
	if _, err := expectedIdentityIDs(admissions, calls, state.expectedSpawns); err == nil {
		t.Fatal("accepted swapped genuine call IDs")
	}
}

func TestIndependentIdentityRejectsConsistentlyWrongProductFields(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, "agent", "sessions")
	if err := os.MkdirAll(sessions, 0o700); err != nil {
		t.Fatal(err)
	}
	state := newFixtureState(caseDirect)
	state.fixtureRoot = "root"
	state.fixtureWorkspace = "/expected"
	state.expectedSpawns["call"] = spawnExpectation{CallID: "call", Name: "worker", ParentName: "$root", Kind: "scout", Model: "fixture/held", Cwd: "/expected", Task: "independent task"}
	state.admissionCalls["child"] = "call"
	p := panelProof{state: state, rootID: "root", workspace: "/expected", agentDir: filepath.Join(dir, "agent")}
	for _, test := range []string{"valid", "missing parent", "missing model", "missing cwd", "wrong model everywhere", "wrong cwd everywhere", "wrong provider model"} {
		admitted := admissionChild{ID: "child", Name: "worker", Parent: "root", Type: "scout", Model: "fixture/held", Cwd: "/expected"}
		saved := nativeChild{ID: "child", RootID: "root", ParentID: "root", Name: "worker", Kind: "scout", Model: "fixture/held", Cwd: "/expected"}
		providerModel := "held"
		nativeCwd := "/expected"
		switch test {
		case "missing parent":
			admitted.Parent = ""
		case "missing model":
			admitted.Model = ""
		case "missing cwd":
			admitted.Cwd = ""
		case "wrong model everywhere":
			admitted.Model = "fixture/quick"
			saved.Model = "fixture/quick"
		case "wrong cwd everywhere":
			admitted.Cwd = "/wrong"
			saved.Cwd = "/wrong"
			nativeCwd = "/wrong"
		case "wrong provider model":
			providerModel = "quick"
		}
		state.admissions["child"] = admitted
		state.childRequests = []childRequest{{Name: "worker", Model: providerModel}}
		root := nativeSession{ID: "root", Cwd: "/expected", Children: map[string]nativeChild{"child": saved}}
		header := map[string]string{"type": "session", "id": "child", "cwd": nativeCwd}
		if err := os.WriteFile(filepath.Join(sessions, "child.jsonl"), rawJSON(t, header), 0o600); err != nil {
			t.Fatal(err)
		}
		err := p.verifyIdentities(root)
		if (err == nil) != (test == "valid") {
			t.Fatalf("%s identity error=%v", test, err)
		}
	}
}

func TestPaintedIdentityRejectsPrefixesAndDuplicateFields(t *testing.T) {
	child := nativeChild{ID: "01a15124-fc3d-7253-b68b-58f8112c9288", Name: "worker", Kind: "scout", Model: "fixture/held", Cwd: "/expected", State: "running"}
	const row = "> running worker #112c9288"
	const detail = "Pig Litter | detail\nUUID 01a15124-fc3d-7253-b68b-58f8112c9288\nName worker\nType scout\nModel fixture/held\nCwd /expected\nState running\nAdmission ready\n"
	if err := paintedIdentityMatches(row, detail, child); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{strings.Replace(detail, "Model fixture/held", "Model fixture/held-extra", 1), strings.Replace(detail, "Cwd /expected", "Cwd /expected-extra", 1), detail + "Model fixture/held\n"} {
		if paintedIdentityMatches(row, bad, child) == nil {
			t.Fatal("accepted wrong or ambiguous exact identity field")
		}
	}
	if paintedIdentityMatches("> running other-worker #112c9288", detail, child) == nil {
		t.Fatal("accepted row label containing expected name only as substring")
	}
}
