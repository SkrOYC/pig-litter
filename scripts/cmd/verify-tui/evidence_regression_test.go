package main

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestQuickResumeRequiresPrivatePriorAssistantHistory(t *testing.T) {
	for _, role := range []string{"user", "tool", "assistant"} {
		state := newFixtureState(caseDirect)
		request := openAIRequest{Model: "quick", Tools: providerTools("read"), Messages: []openAIMessage{
			{Role: "user", Content: rawJSON(t, "Return QUICK_REPORT immediately. LIVE_QUICK_RUN")},
			{Role: role, Content: rawJSON(t, state.quickHistoryMarker)},
		}}
		turn, err := state.decide(request)
		if role == "assistant" {
			if err != nil || turn.name != "quick-live" {
				t.Fatalf("retained assistant history: %#v %v", turn, err)
			}
		} else if err == nil {
			t.Fatalf("accepted private marker from %s instead of assistant", role)
		}
	}
}

func TestInferenceTotalsContinueAfterEvidenceCapAndOmissionFails(t *testing.T) {
	state := newFixtureState(caseUnselected)
	for index := 0; index < maxEvidenceRequests+3; index++ {
		if got := state.captureRequest([]byte(`{"model":"parent"}`)); got != index {
			t.Fatalf("raw index=%d want%d", got, index)
		}
		_, err := state.decide(openAIRequest{Model: "parent", Tools: providerTools(fileTools...), Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "UNSELECTED_WIDGET")}}})
		if err != nil {
			t.Fatal(err)
		}
	}
	if state.totalRequests() != 131 || state.modelRequests("parent") != 131 || len(state.requestEvidence()) != 128 || state.providerFailure() == nil {
		t.Fatalf("bounded evidence blinded totals or did not fail omissions")
	}
	copy := state.requestTotalSnapshot()
	copy["parent"] = 0
	if state.modelRequests("parent") != 131 {
		t.Fatal("counter snapshot aliases live counters")
	}
}

func TestCompletionEvidenceRejectsWrongContentDisplayAndHeldReservation(t *testing.T) {
	child := nativeChild{ID: "child", NoticeID: "receipt", Latest: &nativeOutcome{State: "completed", Model: "fixture/quick", Text: "private exact report", ReportProvenance: "child_assistant", Usage: json.RawMessage(`{}`)}}
	child.Latest.Run.ID = "child"
	payload := map[string]any{"origin": "pig-litter", "kind": "litter_completion", "reportProvenance": "child_assistant", "id": "child", "state": "completed", "model": "fixture/quick", "report": "private exact report", "error": "", "truncated": false}
	completion := nativeCompletion{ChildID: "child", Receipt: "receipt", Content: string(rawJSON(t, payload))}
	if err := completionMatches(completion, child, "receipt"); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"origin", "kind", "reportProvenance", "id", "state", "model", "report", "error"} {
		old := payload[field]
		payload[field] = "wrong"
		bad := completion
		bad.Content = string(rawJSON(t, payload))
		if completionMatches(bad, child, "receipt") == nil {
			t.Fatalf("accepted wrong %s", field)
		}
		payload[field] = old
	}
	bad := completion
	bad.Display = true
	if completionMatches(bad, child, "receipt") == nil {
		t.Fatal("accepted displayed automatic notice")
	}
	payload["truncated"] = true
	bad = completion
	bad.Content = string(rawJSON(t, payload))
	if completionMatches(bad, child, "receipt") == nil {
		t.Fatal("accepted wrong truncation state")
	}
	child.Notices = []json.RawMessage{rawJSON(t, nativeNotice{Nonce: "receipt", Outcome: child.Latest})}
	if completionMatches(completion, child, "receipt") == nil {
		t.Fatal("accepted unconsumed reservation after promotion")
	}
	child.Notices = []json.RawMessage{rawJSON(t, nativeNotice{Nonce: "receipt", Consumed: true, Outcome: child.Latest})}
	if err := completionMatches(completion, child, "receipt"); err != nil {
		t.Fatal(err)
	}
}

func TestNativeOutcomeProjectionRetainsCompleteOriginalObject(t *testing.T) {
	const original = `{"run":{"id":"child"},"state":"completed","model":"fixture/quick","text":"report","truncated":false,"usage":{"input":1},"reportProvenance":"child_assistant","futureField":{"kept":true}}`
	var outcome nativeOutcome
	if err := json.Unmarshal([]byte(original), &outcome); err != nil {
		t.Fatal(err)
	}
	if string(outcome.Raw) != original || outcome.ReportProvenance != "child_assistant" || string(outcome.Usage) != `{"input":1}` {
		t.Fatalf("lost complete outcome: %#v", outcome)
	}
}

func TestPassiveDigestsDetectOrdinaryRootAndChildLogMutation(t *testing.T) {
	dir := t.TempDir()
	rootDir := filepath.Join(dir, "roots")
	childDir := filepath.Join(dir, "agent", "sessions")
	for _, directory := range []string{rootDir, childDir} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	paths := []string{filepath.Join(rootDir, "root.jsonl"), filepath.Join(childDir, "child.jsonl")}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("native original\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	p := panelProof{rootSessions: rootDir, agentDir: filepath.Join(dir, "agent")}
	before, err := nativeDigests(rootDir, childDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range paths {
		if err := os.WriteFile(path, []byte("native original\nunrelated ordinary message\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if p.logsUnchanged(before) == nil {
			t.Fatalf("ignored passive log mutation %s", path)
		}
		if err := os.WriteFile(path, []byte("native original\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestMalformedNativeEvidenceStillCopiesExactRawLog(t *testing.T) {
	source := t.TempDir()
	destination := t.TempDir()
	path := filepath.Join(source, "malformed.jsonl")
	const raw = "{malformed native record\n"
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	if saveNativeEvidence(destination, source) == nil {
		t.Fatal("malformed evidence reported success")
	}
	saved, err := os.ReadFile(filepath.Join(destination, "malformed.jsonl"))
	if err != nil || string(saved) != raw {
		t.Fatalf("lost malformed raw evidence: %q %v", saved, err)
	}
}

func TestRawPTYAllowsHostANSIAndRejectsExecutedAttackBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.raw")
	if err := os.WriteFile(path+".stderr", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		raw    string
		unsafe bool
	}{
		{"\x1b[2J\x1b[1mHOSTILE_CONFIG_BEGIN safe\x1b[0m", false},
		{"safe prefix\x1b]0;CONFIG_OSC_SHOULD_NOT_PAINT\a\x1b[2J", true},
		{"safe prefix\x1b]0;OSC_PAYLOAD_SHOULD_NOT_PAINT\a", true},
	} {
		if err := os.WriteFile(path, []byte(test.raw), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := checkRawPTY(path); (got != nil) != test.unsafe {
			t.Fatalf("transport=%q error=%v", test.raw, got)
		}
	}
	if err := os.WriteFile(path, []byte("safe recorded prefix"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".stderr", []byte("head: write error"), 0o600); err != nil {
		t.Fatal(err)
	}
	if checkRawPTY(path) == nil {
		t.Fatal("accepted incomplete raw recording after collector write error")
	}
}

func TestClosedSocketCannotProveRecordedServerExit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := readObservedProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	closed, _ := waitForTmuxSocketClose(context.Background(), path)
	if !closed || !observedAlive(identity) {
		t.Fatal("socket closure lost live server identity")
	}
	identity.StartTicks = "different-start"
	if observedAlive(identity) {
		t.Fatal("PID reuse matched wrong start ticks")
	}
}

func TestEmptyInputBarrierRejectsSubmissionEvenWithVisibleMarker(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	p := panelProof{ctx: ctx, state: newFixtureState(caseDirect), sendKey: func(string) error { return nil }, sendText: func(string) error { return nil }, visible: func() (string, error) { return "Pig Litter | list\nEMPTY_INPUT_BARRIER\n", nil }}
	if err := p.emptyInput("empty-enter", "message"); err == nil {
		t.Fatal("empty submission passed an already-visible marker")
	}
}

func TestSpawnIdentityCorrelatesNativeHeaderAndParent(t *testing.T) {
	dir := t.TempDir()
	childDir := filepath.Join(dir, "agent", "sessions")
	if err := os.MkdirAll(childDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(childDir, "child.jsonl"), []byte(`{"type":"session","id":"child","cwd":"/fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	state := newFixtureState(caseDirect)
	state.fixtureRoot, state.fixtureWorkspace = "root", "/fixture"
	state.admissions["child"] = admissionChild{ID: "child", Name: "worker", Parent: "root", Type: "scout", Model: "fixture/held", Cwd: "/fixture"}
	state.admissionCalls["child"] = "call-worker"
	state.expectedSpawns["call-worker"] = spawnExpectation{CallID: "call-worker", Name: "worker", ParentName: "$root", Kind: "scout", Model: "fixture/held", Cwd: "/fixture", Task: "independent task"}
	state.childRequests = []childRequest{{Name: "worker", Model: "held"}}
	p := panelProof{state: state, rootID: "root", workspace: "/fixture", agentDir: filepath.Join(dir, "agent")}
	child := nativeChild{ID: "child", RootID: "root", ParentID: "root", Name: "worker", Kind: "scout", Model: "fixture/held", Cwd: "/fixture"}
	root := nativeSession{ID: "root", Cwd: "/fixture", Children: map[string]nativeChild{"child": child}}
	if err := p.verifyIdentities(root); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"root", "parent", "model", "cwd"} {
		bad := child
		switch field {
		case "root":
			bad.RootID = "other-root"
		case "parent":
			bad.ParentID = "other-parent"
		case "model":
			bad.Model = "other-model"
		case "cwd":
			bad.Cwd = "/other"
		}
		root.Children["child"] = bad
		if err := p.verifyIdentities(root); err == nil {
			t.Fatalf("accepted wrong %s identity", field)
		}
	}
	root.Children["child"] = child
	if err := os.WriteFile(filepath.Join(childDir, "child.jsonl"), []byte(`{"type":"session","id":"different-native-session","cwd":"/fixture"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.verifyIdentities(root); err == nil || !strings.Contains(err.Error(), "absent") {
		t.Fatalf("wrong child header accepted: %v", err)
	}
}
