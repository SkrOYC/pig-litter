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

func TestLatestRootPromptIgnoresLaterChildNotifications(t *testing.T) {
	messages := []openAIMessage{
		{Role: "user", Content: rawJSON(t, "START_WIDGET")},
		{Role: "assistant", Content: rawJSON(t, "PARENT_SCROLLBACK_MARKER")},
		{Role: "user", Content: rawJSON(t, "STOP_WIDGET tree:1 tree:2")},
		{Role: "user", Content: rawJSON(t, "A child completed and sent a notification.")},
	}
	got := latestRootPrompt(messages)
	if got.marker != "STOP_WIDGET" || got.text != "STOP_WIDGET tree:1 tree:2" {
		t.Fatalf("latestRootPrompt() = %#v", got)
	}
}

func TestProviderDrivesActualSpawnReadHoldAndStopResults(t *testing.T) {
	state := newFixtureState(caseDirect)
	parentTools := make([]openAITool, 0, len(controlTools)+1)
	for _, name := range append(append([]string(nil), controlTools...), "read") {
		var tool openAITool
		tool.Function.Name = name
		parentTools = append(parentTools, tool)
	}
	start := openAIRequest{Model: "parent", Tools: parentTools, Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "START_WIDGET")}}}
	turn, err := state.decide(start)
	if err != nil {
		t.Fatal(err)
	}
	if turn.kind != "tools" || len(turn.calls) != 2 || turn.calls[0].name != "litter_spawn" || turn.calls[1].name != "litter_spawn" {
		t.Fatalf("initial start turn = %#v", turn)
	}

	childTools := []openAITool{{}}
	childTools[0].Function.Name = "read"
	for index, name := range heldNames {
		letter := "A"
		if index == 1 {
			letter = "B"
		}
		child := openAIRequest{Model: "held", Tools: childTools, Messages: []openAIMessage{{Role: "user", Content: rawJSON(t, "Hold widget child "+letter+" until the parent explicitly stops it.")}}}
		readTurn, err := state.decide(child)
		if err != nil {
			t.Fatal(err)
		}
		if readTurn.kind != "tools" || len(readTurn.calls) != 1 || readTurn.calls[0].name != "read" {
			t.Fatalf("child %s read turn = %#v", name, readTurn)
		}
		child.Messages = append(child.Messages, openAIMessage{Role: "tool", Content: rawJSON(t, "TUI_READ_MARKER")})
		holdTurn, err := state.decide(child)
		if err != nil {
			t.Fatal(err)
		}
		if holdTurn.kind != "hold" || holdTurn.name != name {
			t.Fatalf("child %s hold turn = %#v", name, holdTurn)
		}
		if err := state.beginHold(name); err != nil {
			t.Fatal(err)
		}
	}

	toolMessages := []openAIMessage{
		{Role: "tool", Content: rawJSON(t, `{"operation":"litter_spawn","child":{"id":"tree:1","generation":1,"name":"held-a"}}`)},
		{Role: "tool", Content: rawJSON(t, `{"operation":"litter_spawn","child":{"id":"tree:2","generation":1,"name":"held-b"}}`)},
	}
	parentMessages := []openAIMessage{{Role: "user", Content: rawJSON(t, "START_WIDGET")}}
	parentMessages = append(parentMessages, toolMessages...)
	parentMessages = append(parentMessages, openAIMessage{Role: "user", Content: rawJSON(t, "A child notification arrived.")})
	started, err := state.decide(openAIRequest{Model: "parent", Tools: parentTools, Messages: parentMessages})
	if err != nil {
		t.Fatal(err)
	}
	if started.kind != "text" || started.text != "PARENT_SCROLLBACK_MARKER" {
		t.Fatalf("parent start turn = %#v", started)
	}
	if !state.startReady() {
		t.Fatal("state did not record both read children and actual spawn references")
	}
	selected := state.selectedTools()
	for _, expected := range append(append([]string(nil), controlTools...), "read") {
		if !contains(selected, expected) {
			t.Errorf("parent tool inventory is missing %s", expected)
		}
	}

	for _, name := range heldNames {
		state.cancelHold(name)
	}
	stopResults := []openAIMessage{
		{Role: "tool", Content: rawJSON(t, `{"operation":"litter_stop","state":"stopped"}`)},
		{Role: "tool", Content: rawJSON(t, `{"operation":"litter_stop","state":"stopped"}`)},
	}
	stopMessages := []openAIMessage{{Role: "user", Content: rawJSON(t, "STOP_WIDGET tree:1 tree:2")}}
	stopMessages = append(stopMessages, toolMessages...)
	stopMessages = append(stopMessages, stopResults...)
	stopped, err := state.decide(openAIRequest{Model: "parent", Tools: parentTools, Messages: stopMessages})
	if err != nil {
		t.Fatal(err)
	}
	if stopped.kind != "text" || stopped.text != "STOP_DONE" || !state.stoppedReady() {
		t.Fatalf("parent stop turn = %#v", stopped)
	}
}

func TestSpawnFailureWithoutIDStopsTheFixtureAndKeepsDiagnostic(t *testing.T) {
	state := newFixtureState(caseDirect)
	tools := make([]openAITool, 0, len(controlTools)+1)
	for _, name := range append(append([]string(nil), controlTools...), "read") {
		var tool openAITool
		tool.Function.Name = name
		tools = append(tools, tool)
	}
	failedResult := `{"operation":"litter_spawn","state":"unavailable","code":"unavailable","report":"exact model is unavailable to the root and stock SDK"}`
	request := openAIRequest{Model: "parent", Tools: tools, Messages: []openAIMessage{
		{Role: "user", Content: rawJSON(t, "START_WIDGET")},
		{Role: "tool", Content: rawJSON(t, failedResult)},
	}}
	turn, err := state.decide(request)
	if err == nil || turn.kind != "" || !strings.Contains(err.Error(), "exact model is unavailable") {
		t.Fatalf("failed spawn returned turn=%#v error=%v", turn, err)
	}
	failures := state.spawnFailures()
	if len(failures) != 1 || failures[0].State != "unavailable" || failures[0].Code != "unavailable" || failures[0].Report != "exact model is unavailable to the root and stock SDK" {
		t.Fatalf("failed spawn evidence = %#v", failures)
	}
	if got := state.requestEvidence(); len(got) != 1 || got[0].ToolResultCounts["litter_spawn"] != 1 {
		t.Fatalf("failed request evidence = %#v", got)
	}
	state.recordError(err)
	if failure := state.providerFailure(); failure == nil || !strings.Contains(failure.Error(), "unavailable") {
		t.Fatalf("provider failure was not available for fail-fast waiting: %v", failure)
	}
}

func TestProviderRequestEvidenceIsBoundedAndCountsResults(t *testing.T) {
	state := newFixtureState(caseUnselected)
	request := openAIRequest{Model: "parent", Messages: []openAIMessage{
		{Role: "user", Content: rawJSON(t, "UNSELECTED_WIDGET")},
		{Role: "tool", Content: rawJSON(t, `{"operation":"fixture_action"}`)},
		{Role: "tool", Content: rawJSON(t, "plain result")},
	}}
	for index := 0; index < maxEvidenceRequests+3; index++ {
		if _, err := state.decide(request); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(state.requestEvidence()); got != maxEvidenceRequests {
		t.Fatalf("recorded %d requests, want cap %d", got, maxEvidenceRequests)
	}
	if got := state.requestsOmittedCount(); got != 3 {
		t.Fatalf("omitted %d requests, want 3", got)
	}
	counts := state.requestEvidence()[0].ToolResultCounts
	if counts["fixture_action"] != 1 || counts["unparsed"] != 1 {
		t.Fatalf("tool result counts = %#v", counts)
	}
}

func TestTmuxSocketCleanupOnlyRemovesStaleOwnedSocket(t *testing.T) {
	socket := filepath.Join(t.TempDir(), "tmux.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	listener.(*net.UnixListener).SetUnlinkOnClose(false)
	t.Cleanup(func() {
		_ = listener.Close()
		_ = os.Remove(socket)
	})
	if exists, alive, err := tmuxSocketStatus(socket); err != nil || !exists || !alive {
		t.Fatalf("live socket status = exists %v, alive %v, error %v", exists, alive, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if closed, alive := waitForOwnedTmuxExit(ctx, socket); closed || !alive {
		t.Fatalf("active server cleanup = closed %v, alive %v", closed, alive)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if closed, alive := waitForOwnedTmuxExit(context.Background(), socket); !closed || alive {
		t.Fatalf("stale socket cleanup = closed %v, alive %v", closed, alive)
	}
	if _, err := os.Lstat(socket); !os.IsNotExist(err) {
		t.Fatalf("stale socket remains: %v", err)
	}
}

func TestPigletCopiesPrebuiltResourceAtUnchangedGoOrigin(t *testing.T) {
	root := t.TempDir()
	manifestPath := filepath.Join(root, "piglet.yaml")
	manifest := "name: pig-litter\nextensions:\n  - name: pig-litter\n    origins: [local:./extensions/pig-litter]\n"
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	prebuilt := filepath.Join(root, "dist", "pig-litter")
	if err := os.MkdirAll(filepath.Dir(prebuilt), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prebuilt, []byte("compiled Go extension"), 0o700); err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(root, "fixture")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	manifestCopy, resourceCopy, err := copyPigletFixture(manifestPath, workspace, prebuilt)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(manifestCopy); err != nil || string(got) != manifest {
		t.Fatalf("copied manifest changed: %q, %v", got, err)
	}
	if filepath.Clean(resourceCopy) != filepath.Join(workspace, "extensions", "pig-litter") {
		t.Fatalf("resource copied to %q", resourceCopy)
	}
	if got, err := os.ReadFile(resourceCopy); err != nil || string(got) != "compiled Go extension" {
		t.Fatalf("copied resource = %q, %v", got, err)
	}
	info, err := os.Stat(resourceCopy)
	if err != nil || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("copied resource lost execute permission: %v", err)
	}
}

func TestPigletOriginRejectsJavaScriptAndNonLocalResources(t *testing.T) {
	for _, origin := range []string{"local:./extensions/pig-litter/dist/pig-litter.mjs", "github:owner/repo@v1"} {
		manifest := "name: pig-litter\nextensions:\n  - name: pig-litter\n    origins: [" + origin + "]\n"
		if _, err := pigletExtensionOrigin(manifest, selectedExtensionName); err == nil {
			t.Errorf("pigletExtensionOrigin accepted %q", origin)
		}
	}
}

func TestWidgetGeometryStartupAndEditorPredicates(t *testing.T) {
	live := strings.Join([]string{"Litter 2 live 0 kept", "held-a running", "held-b running"}, "\n")
	stopped := strings.Join([]string{"Litter 0 live 2 kept", "held-a stopped", "held-b stopped"}, "\n")
	if !widgetShows(live, "running") || !widgetShows(stopped, "stopped") || widgetShows(live, "stopped") {
		t.Fatal("widget state predicate did not distinguish live and retained children")
	}
	if got, err := parsePaneGeometry("32 32\n"); err != nil || got != (paneGeometry{width: 32, height: 32}) {
		t.Fatalf("parsePaneGeometry() = %#v, %v", got, err)
	}
	if _, err := parsePaneGeometry("100x32"); err == nil {
		t.Fatal("parsePaneGeometry accepted malformed dimensions")
	}
	lines := make([]string, 32)
	lines[1] = "v0.4.1+1.0.3"
	lines[27] = strings.Repeat("─", 100)
	lines[29] = strings.Repeat("─", 100)
	lines[31] = strings.Repeat(" ", 84) + "parent"
	startup := strings.Join(lines, "\n")
	if !startupFrameReady(startup) || !frameHasGeometry(startup, 100, 32) {
		t.Fatal("startup frame predicates rejected a ready 100x32 screen")
	}
	lines[28] = "EDITOR_PROBE"
	probe := strings.Join(lines, "\n")
	if text, ok := editorInputText(probe); !ok || text != "EDITOR_PROBE" {
		t.Fatalf("editor probe = %q, %v", text, ok)
	}
	lines[28] = ""
	if text, ok := editorInputText(strings.Join(lines, "\n")); !ok || text != "" {
		t.Fatalf("empty editor = %q, %v", text, ok)
	}
}

func TestFixtureEnvironmentDoesNotInheritRuntimeOrProviderSecrets(t *testing.T) {
	env := fixtureEnvironment("/fixture/bin/pig", "/fixture/tools/tmux", "/private/home", "/private/pig", "/private/agent", "/private")
	values := make(map[string]string)
	for _, entry := range env {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			t.Fatalf("invalid environment entry %q", entry)
		}
		values[key] = value
	}
	for _, key := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "NODE_OPTIONS", "NODE_PATH", "CODEX_HOME"} {
		if _, exists := values[key]; exists {
			t.Errorf("fixture environment inherited %s", key)
		}
	}
	if values["HOME"] != "/private/home" || values["PIG_HOME"] != "/private/pig" || values["PIG_CODING_AGENT_DIR"] != "/private/agent" {
		t.Fatalf("fixture roots = %#v", values)
	}
	if got := values["PATH"]; got != strings.Join([]string{"/fixture/bin", "/fixture/tools", "/run/current-system/sw/bin", "/usr/bin", "/bin"}, string(os.PathListSeparator)) {
		t.Fatalf("fixture PATH = %q", got)
	}
}

func TestShellQuoteKeepsMetacharactersLiteral(t *testing.T) {
	if got := shellQuote("with space's $HOME"); got != "'with space'\\''s $HOME'" {
		t.Fatalf("shellQuote() = %q", got)
	}
	if got := joinShellCommand("/bin/pig", []string{"--model", "fixture/parent"}); got != "'/bin/pig' '--model' 'fixture/parent'" {
		t.Fatalf("joinShellCommand() = %q", got)
	}
}

func TestParseOptionsRequiresAbsolutePrebuiltPaths(t *testing.T) {
	directory := t.TempDir()
	pig := filepath.Join(directory, "pig")
	extension := filepath.Join(directory, "pig-litter")
	if err := os.WriteFile(pig, []byte("pig"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(extension, []byte("extension"), 0o700); err != nil {
		t.Fatal(err)
	}
	got, err := parseOptions([]string{"--case", "direct", "--pig", pig, "--extension", extension, "--evidence-root", filepath.Join(directory, "evidence")})
	if err != nil {
		t.Fatal(err)
	}
	if got.caseName != caseDirect || got.pigPath != pig || got.extension != extension {
		t.Fatalf("parseOptions() = %#v", got)
	}
	if _, err := parseOptions([]string{"--case", "direct", "--pig", "pig", "--extension", extension, "--evidence-root", directory}); err == nil {
		t.Fatal("parseOptions accepted a non-absolute PiG path")
	}
}

func rawJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
