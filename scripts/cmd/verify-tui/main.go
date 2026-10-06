package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
)

const (
	selectedExtensionName = "pig-litter"
	fixtureAPIKey         = "local-fixture-only"
	escapeSettle          = 150 * time.Millisecond
	stageTimeout          = 35 * time.Second
	runTimeout            = 2 * time.Minute
	maxEvidenceRequests   = 128
	maxProviderErrors     = 32
)

var (
	controlTools = []string{
		"litter_spawn",
		"litter_list",
		"litter_inspect",
		"litter_message",
		"litter_stop",
		"litter_wait",
	}
	heldNames = []string{"held-a", "held-b"}
)

type runCase string

const (
	caseDirect     runCase = "direct"
	casePiglet     runCase = "piglet"
	caseUnselected runCase = "unselected"
)

type options struct {
	caseName     runCase
	pigPath      string
	extension    string
	piglet       string
	evidenceRoot string
}

type childRef struct {
	ID         string `json:"id"`
	Generation int    `json:"generation"`
	Name       string `json:"name"`
}

type openAIMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type openAITool struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}

type openAIRequest struct {
	Model    string          `json:"model"`
	Messages []openAIMessage `json:"messages"`
	Tools    []openAITool    `json:"tools"`
}

type requestEvidence struct {
	Model            string         `json:"model"`
	Tools            []string       `json:"tools"`
	RootMarker       string         `json:"rootMarker,omitempty"`
	ToolResultCounts map[string]int `json:"toolResultCounts,omitempty"`
}

type failedSpawn struct {
	State  string `json:"state,omitempty"`
	Code   string `json:"code,omitempty"`
	Report string `json:"report,omitempty"`
	Name   string `json:"name,omitempty"`
	ID     string `json:"id,omitempty"`
}

type fixtureState struct {
	mu                    sync.Mutex
	caseName              runCase
	requests              []requestEvidence
	requestsOmitted       int
	held                  map[string]struct{}
	aborted               map[string]struct{}
	childReads            map[string]struct{}
	refs                  []childRef
	parentMarker          bool
	stopDone              bool
	providerError         []string
	providerErrorsOmitted int
	failedSpawns          []failedSpawn
	callSequence          int
}

type providerTurn struct {
	kind  string
	text  string
	name  string
	calls []toolCall
}

type toolCall struct {
	name string
	args any
}

type fixtureProvider struct {
	state  *fixtureState
	server *http.Server
	port   int
}

type paneGeometry struct {
	width  int
	height int
}

type framePair struct {
	visible    string
	scrollback string
}

type tmuxFailure struct {
	Operation string `json:"operation"`
	Stderr    string `json:"stderr"`
}

type cleanupEvidence struct {
	PaneJoined       bool   `json:"paneJoined"`
	TmuxSocketClosed bool   `json:"tmuxSocketClosed"`
	TmuxServerAlive  bool   `json:"tmuxServerAlive"`
	TmuxFailureCount int    `json:"tmuxFailureCount"`
	ScratchRetained  string `json:"scratchRetainedAt,omitempty"`
}

type evidenceReport struct {
	Case              runCase           `json:"case"`
	Pig               string            `json:"pig"`
	Extension         string            `json:"extension"`
	PigletSource      string            `json:"pigletSource,omitempty"`
	PigletFixture     string            `json:"pigletFixture,omitempty"`
	PigletResource    string            `json:"pigletResource,omitempty"`
	SelectedToolNames []string          `json:"selectedToolNames,omitempty"`
	ChildRefs         []childRef        `json:"childRefs,omitempty"`
	AbortedStreams    []string          `json:"abortedStreams,omitempty"`
	ChildReads        []string          `json:"childReads,omitempty"`
	FailedSpawns      []failedSpawn     `json:"failedSpawns,omitempty"`
	ProviderRequests  []requestEvidence `json:"providerRequests,omitempty"`
	RequestsOmitted   int               `json:"providerRequestsOmitted,omitempty"`
	ProviderErrors    []string          `json:"providerErrors,omitempty"`
	ErrorsOmitted     int               `json:"providerErrorsOmitted,omitempty"`
	FailureFrame      map[string]any    `json:"failureFrame,omitempty"`
	Cleanup           cleanupEvidence   `json:"cleanup"`
	OK                bool              `json:"ok"`
	Error             string            `json:"error,omitempty"`
}

type runSummary struct {
	OK       bool    `json:"ok"`
	Case     runCase `json:"case"`
	Evidence string  `json:"evidence"`
}

func main() {
	options, err := parseOptions(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	result, runErr := run(ctx, options)
	_ = json.NewEncoder(os.Stdout).Encode(runSummary{OK: runErr == nil, Case: options.caseName, Evidence: result})
	if runErr != nil {
		fmt.Fprintln(os.Stderr, runErr)
		os.Exit(1)
	}
}

func parseOptions(args []string) (options, error) {
	var result options
	flags := flag.NewFlagSet("go-tui-proof", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	caseValue := flags.String("case", "", "direct, piglet, or unselected")
	flags.StringVar(&result.pigPath, "pig", "", "absolute path to the released PiG executable")
	flags.StringVar(&result.extension, "extension", "", "absolute path to the prebuilt pig-litter executable")
	flags.StringVar(&result.piglet, "piglet", "", "absolute path to the current pig-litter manifest")
	flags.StringVar(&result.evidenceRoot, "evidence-root", "", "absolute directory for private evidence")
	if err := flags.Parse(args); err != nil {
		return options{}, fmt.Errorf("parse flags: %w", err)
	}
	if flags.NArg() != 0 {
		return options{}, errors.New("positional arguments are not supported")
	}
	result.caseName = runCase(*caseValue)
	if result.caseName != caseDirect && result.caseName != casePiglet && result.caseName != caseUnselected {
		return options{}, errors.New("use --case direct|piglet|unselected")
	}
	for label, path := range map[string]string{
		"--pig": result.pigPath, "--extension": result.extension, "--evidence-root": result.evidenceRoot,
	} {
		if path == "" || !filepath.IsAbs(path) {
			return options{}, fmt.Errorf("%s must be an absolute path", label)
		}
	}
	if result.caseName == casePiglet && (result.piglet == "" || !filepath.IsAbs(result.piglet)) {
		return options{}, errors.New("--piglet must be an absolute path for the piglet case")
	}
	if result.piglet != "" && !filepath.IsAbs(result.piglet) {
		return options{}, errors.New("--piglet must be an absolute path")
	}
	if err := requireRegularFile(result.pigPath, "PiG executable"); err != nil {
		return options{}, err
	}
	if err := requireExecutable(result.extension, "prebuilt pig-litter executable"); err != nil {
		return options{}, err
	}
	if filepath.Base(result.extension) != selectedExtensionName {
		return options{}, fmt.Errorf("prebuilt extension basename must be %q", selectedExtensionName)
	}
	if result.caseName == casePiglet {
		if strings.HasSuffix(strings.ToLower(result.piglet), ".mjs") || strings.HasSuffix(strings.ToLower(result.piglet), ".js") || strings.HasSuffix(strings.ToLower(result.piglet), ".ts") {
			return options{}, errors.New("--piglet must name the current Go Piglet manifest, not a JavaScript resource")
		}
		if err := requireRegularFile(result.piglet, "Piglet manifest"); err != nil {
			return options{}, err
		}
	}
	return result, nil
}

func requireRegularFile(path, label string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", label)
	}
	return nil
}

func requireExecutable(path, label string) error {
	if err := requireRegularFile(path, label); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s is not executable", label)
	}
	return nil
}

func run(parent context.Context, options options) (evidencePath string, runErr error) {
	ctx, cancel := context.WithTimeout(parent, runTimeout)
	defer cancel()
	evidencePath = filepath.Join(options.evidenceRoot, fmt.Sprintf("%s-%s-%d", time.Now().UTC().Format("2006-01-02T15-04-05.000Z"), options.caseName, os.Getpid()))
	if err := os.MkdirAll(evidencePath, 0o700); err != nil {
		return evidencePath, fmt.Errorf("create evidence directory: %w", err)
	}
	if err := os.Chmod(evidencePath, 0o700); err != nil {
		return evidencePath, fmt.Errorf("secure evidence directory: %w", err)
	}
	report := evidenceReport{Case: options.caseName, Pig: options.pigPath, Extension: options.extension}
	scratch, err := os.MkdirTemp("", "litter-tui-proof-")
	if err != nil {
		return evidencePath, fmt.Errorf("create fixture directory: %w", err)
	}
	if err := os.Chmod(scratch, 0o700); err != nil {
		_ = os.RemoveAll(scratch)
		return evidencePath, fmt.Errorf("secure fixture directory: %w", err)
	}
	workspace := filepath.Join(scratch, "workspace")
	home := filepath.Join(scratch, "home")
	pigHome := filepath.Join(scratch, "pig-home")
	agentDir := filepath.Join(scratch, "agent")
	socket := filepath.Join(scratch, "tmux.sock")
	const session = "litter-proof"
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		_ = os.RemoveAll(scratch)
		return evidencePath, errors.New("tmux must be available on the host PATH")
	}
	childEnv := fixtureEnvironment(options.pigPath, tmuxPath, home, pigHome, agentDir, scratch)
	tmuxErrors := make([]tmuxFailure, 0)
	state := newFixtureState(options.caseName)
	provider, err := startFixtureProvider(state)
	if err != nil {
		_ = os.RemoveAll(scratch)
		return evidencePath, err
	}
	socketStarted := false
	panePID := 0
	paneExited := false
	socketClosed := false
	tmuxServerAlive := false
	tmuxSessionExists := false
	var manifestDestination string
	var resourceDestination string
	var command string
	tmuxCall := func(args ...string) (string, error) {
		value, err := runTmux(ctx, tmuxPath, socket, childEnv, args...)
		if err != nil {
			failure := tmuxFailure{Operation: "unknown", Stderr: ""}
			if len(args) > 0 {
				failure.Operation = args[0]
			}
			failure.Stderr = evidenceText(err.Error())
			tmuxErrors = append(tmuxErrors, failure)
			return "", fmt.Errorf("private tmux %s failed: %s", failure.Operation, failure.Stderr)
		}
		return value, nil
	}
	readVisible := func() (string, error) { return tmuxCall("capture-pane", "-p", "-t", session) }
	readScrollback := func() (string, error) {
		return tmuxCall("capture-pane", "-p", "-S", "-1000", "-t", session)
	}
	paneSize := func() (paneGeometry, error) {
		value, err := tmuxCall("display-message", "-p", "-t", session, "#{pane_width} #{pane_height}")
		if err != nil {
			return paneGeometry{}, err
		}
		return parsePaneGeometry(value)
	}
	frame := func(name string) (framePair, error) {
		visible, err := readVisible()
		if err != nil {
			return framePair{}, err
		}
		scrollback, err := readScrollback()
		if err != nil {
			return framePair{}, err
		}
		pair := framePair{visible: visible, scrollback: scrollback}
		if err := saveFrame(evidencePath, name, pair); err != nil {
			return framePair{}, err
		}
		return pair, nil
	}
	sendText := func(text string) error {
		_, err := tmuxCall("send-keys", "-l", "-t", session, text)
		return err
	}
	sendKey := func(key string) error {
		_, err := tmuxCall("send-keys", "-t", session, key)
		return err
	}
	captureFailure := func() map[string]any {
		visible, err := readVisible()
		if err != nil {
			return map[string]any{"captureError": evidenceText(err.Error())}
		}
		_ = writePrivate(filepath.Join(evidencePath, "failure-current-visible.txt"), []byte(visible))
		result := map[string]any{"visibleFile": "failure-current-visible.txt"}
		if scrollback, err := readScrollback(); err == nil {
			_ = writePrivate(filepath.Join(evidencePath, "failure-current-scrollback.txt"), []byte(scrollback))
			result["scrollbackSaved"] = true
		} else {
			result["scrollbackSaved"] = false
		}
		if geometry, err := paneSize(); err == nil {
			result["geometry"] = fmt.Sprintf("%dx%d", geometry.width, geometry.height)
		}
		if dead, err := tmuxCall("display-message", "-p", "-t", session, "#{pane_dead}"); err == nil {
			result["paneDead"] = dead == "1"
		}
		if current, err := tmuxCall("display-message", "-p", "-t", session, "#{pane_current_command}"); err == nil {
			result["paneCommand"] = current
		}
		return result
	}
	defer func() {
		if runErr != nil && socketStarted {
			report.FailureFrame = captureFailure()
		}
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cleanupCancel()
		if tmuxSessionExists {
			if _, err := runTmux(cleanupCtx, tmuxPath, socket, childEnv, "kill-session", "-t", session); err == nil {
				tmuxSessionExists = false
			} else if _, alive, statusErr := tmuxSocketStatus(socket); statusErr != nil || alive {
				tmuxErrors = append(tmuxErrors, tmuxFailure{Operation: "kill-session", Stderr: evidenceText(err.Error())})
			} else {
				tmuxSessionExists = false
			}
		}
		if panePID > 0 {
			paneExited = waitForProcessExit(cleanupCtx, panePID)
		} else {
			paneExited = true
		}
		provider.close()
		if socketStarted {
			socketClosed, tmuxServerAlive = waitForOwnedTmuxExit(cleanupCtx, socket)
		} else {
			socketClosed = true
		}
		if !paneExited && runErr == nil {
			runErr = errors.New("private TUI process did not exit during cleanup")
		}
		if !socketClosed && runErr == nil {
			runErr = errors.New("private tmux socket did not close during cleanup")
		}
		if len(tmuxErrors) > 0 {
			encoded, _ := json.MarshalIndent(tmuxErrors, "", "  ")
			_ = writePrivate(filepath.Join(evidencePath, "tmux-errors.json"), encoded)
		}
		cleanup := cleanupEvidence{PaneJoined: paneExited, TmuxSocketClosed: socketClosed, TmuxServerAlive: tmuxServerAlive, TmuxFailureCount: len(tmuxErrors)}
		if !paneExited || !socketClosed {
			cleanup.ScratchRetained = scratch
		} else {
			_ = os.RemoveAll(scratch)
		}
		report.Case = options.caseName
		report.Pig = options.pigPath
		report.Extension = options.extension
		report.PigletSource = options.piglet
		report.PigletFixture = manifestDestination
		report.PigletResource = resourceDestination
		report.SelectedToolNames = state.selectedTools()
		report.ChildRefs = state.childReferences()
		report.AbortedStreams = state.names(state.aborted)
		report.ChildReads = state.names(state.childReads)
		report.FailedSpawns = state.spawnFailures()
		report.ProviderRequests = state.requestEvidence()
		report.RequestsOmitted = state.requestsOmittedCount()
		report.ProviderErrors = state.errors()
		report.ErrorsOmitted = state.providerErrorsOmittedCount()
		report.Cleanup = cleanup
		report.OK = runErr == nil
		if runErr != nil {
			report.Error = evidenceText(runErr.Error())
		}
		encoded, _ := json.MarshalIndent(report, "", "  ")
		if err := writePrivate(filepath.Join(evidencePath, "result.json"), encoded); err != nil && runErr == nil {
			runErr = fmt.Errorf("write result evidence: %w", err)
		}
	}()

	if err := makeFixtureFiles(workspace, home, pigHome, agentDir, provider.port); err != nil {
		return evidencePath, err
	}
	if options.caseName == casePiglet {
		manifestDestination, resourceDestination, err = copyPigletFixture(options.piglet, workspace, options.extension)
		if err != nil {
			return evidencePath, err
		}
	}
	args := []string{
		"--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-session", "--approve",
		"--model", "fixture/parent",
	}
	switch options.caseName {
	case caseDirect:
		args = append([]string{"--no-extensions", "-e", options.extension}, args...)
	case casePiglet:
		args = append([]string{"--piglet", manifestDestination}, args...)
	case caseUnselected:
		args = append([]string{"--no-extensions"}, args...)
	}
	command = joinShellCommand(options.pigPath, args)

	socketStarted = true
	if _, err := tmuxCall("new-session", "-d", "-s", session, "-x", "100", "-y", "32", "-c", workspace, "/bin/sh"); err != nil {
		return evidencePath, err
	}
	tmuxSessionExists = true
	if _, err := tmuxCall("set-window-option", "-t", session, "remain-on-exit", "on"); err != nil {
		return evidencePath, err
	}
	if _, err := tmuxCall("respawn-pane", "-k", "-t", session, command); err != nil {
		return evidencePath, err
	}
	pidText, err := tmuxCall("display-message", "-p", "-t", session, "#{pane_pid}")
	if err != nil {
		return evidencePath, err
	}
	panePID, err = strconv.Atoi(pidText)
	if err != nil || panePID <= 0 {
		return evidencePath, errors.New("private TUI process ID was unavailable")
	}
	if err := waitFor(ctx, "private PiG TUI", stageTimeout, func() (bool, error) {
		dead, err := tmuxCall("display-message", "-p", "-t", session, "#{pane_dead}")
		if err != nil {
			return false, nil
		}
		if dead == "1" {
			return false, errors.New("PiG exited before TUI startup")
		}
		screen, err := readVisible()
		if err != nil {
			return false, nil
		}
		currentCommand, err := tmuxCall("display-message", "-p", "-t", session, "#{pane_current_command}")
		if err != nil {
			return false, nil
		}
		geometry, err := paneSize()
		if err != nil {
			return false, nil
		}
		return startupFrameReady(screen) && currentCommand != "sh" && geometry.width == 100 && geometry.height == 32 && frameHasGeometry(screen, 100, 32), nil
	}); err != nil {
		return evidencePath, err
	}
	if _, err := frame("startup-100"); err != nil {
		return evidencePath, err
	}

	if options.caseName == caseUnselected {
		if err := sendText("UNSELECTED_WIDGET"); err != nil {
			return evidencePath, err
		}
		if err := sendKey("Enter"); err != nil {
			return evidencePath, err
		}
		if err := waitFor(ctx, "unselected parent marker", stageTimeout, func() (bool, error) {
			if err := state.providerFailure(); err != nil {
				return false, err
			}
			return state.parentMarkerReady() && visibleContains(readVisible, "UNSELECTED_PARENT_MARKER"), nil
		}); err != nil {
			return evidencePath, err
		}
		captured, err := frame("unselected-100")
		if err != nil {
			return evidencePath, err
		}
		if state.hasLitterTools() {
			return evidencePath, errors.New("unselected model inventory contains litter tools")
		}
		if strings.Contains(captured.visible, "Litter") || strings.Contains(captured.visible, "litter_spawn") {
			return evidencePath, errors.New("unselected TUI exposed the Resource widget or tools")
		}
	} else {
		if err := sendText("START_WIDGET"); err != nil {
			return evidencePath, err
		}
		if err := sendKey("Enter"); err != nil {
			return evidencePath, err
		}
		if err := waitFor(ctx, "two default reads, held streams, and parent marker", stageTimeout, func() (bool, error) {
			if err := state.providerFailure(); err != nil {
				return false, err
			}
			return state.startReady() && visibleContains(readVisible, "PARENT_SCROLLBACK_MARKER"), nil
		}); err != nil {
			return evidencePath, err
		}
		initial, err := waitForLayout(ctx, paneSize, readVisible, readScrollback, 100, 32, func(visible string) bool {
			return widgetShows(visible, "running") && strings.Contains(visible, "PARENT_SCROLLBACK_MARKER")
		}, "the live two-child widget at 100 columns")
		if err != nil {
			return evidencePath, err
		}
		if err := saveFrame(evidencePath, "live-100-before-resize", initial); err != nil {
			return evidencePath, err
		}
		if !widgetShows(initial.visible, "running") {
			return evidencePath, errors.New("initial live frame did not show both children")
		}
		if _, err := tmuxCall("resize-window", "-t", session, "-x", "32", "-y", "32"); err != nil {
			return evidencePath, err
		}
		narrow, err := waitForLayout(ctx, paneSize, readVisible, readScrollback, 32, 32, func(visible string) bool {
			return widgetShows(visible, "running") && strings.Contains(visible, "PARENT_SCROLLBACK_MARKER")
		}, "the rendered 32-column live widget")
		if err != nil {
			return evidencePath, err
		}
		if err := saveFrame(evidencePath, "live-32", narrow); err != nil {
			return evidencePath, err
		}
		if _, err := tmuxCall("resize-window", "-t", session, "-x", "100", "-y", "32"); err != nil {
			return evidencePath, err
		}
		wide, err := waitForLayout(ctx, paneSize, readVisible, readScrollback, 100, 32, func(visible string) bool {
			return widgetShows(visible, "running") && strings.Contains(visible, "PARENT_SCROLLBACK_MARKER")
		}, "the restored 100-column live widget")
		if err != nil {
			return evidencePath, err
		}
		if err := saveFrame(evidencePath, "live-100-after-resize", wide); err != nil {
			return evidencePath, err
		}
		if !widgetShows(wide.visible, "running") || !strings.Contains(wide.visible, "PARENT_SCROLLBACK_MARKER") {
			return evidencePath, errors.New("wide resize did not restore the live child rows and parent marker")
		}
		if err := sendText("/pig-litter help"); err != nil {
			return evidencePath, err
		}
		if err := sendKey("Enter"); err != nil {
			return evidencePath, err
		}
		if err := waitFor(ctx, "Pig Litter help command", stageTimeout, func() (bool, error) {
			visible, err := readVisible()
			if err != nil {
				return false, nil
			}
			scrollback, err := readScrollback()
			if err != nil {
				return false, nil
			}
			return strings.Contains(visible+scrollback, "Use litter_list"), nil
		}); err != nil {
			return evidencePath, err
		}
		if _, err := frame("pig-litter-help"); err != nil {
			return evidencePath, err
		}
		probe := "EDITOR_PROBE"
		if err := sendText(probe); err != nil {
			return evidencePath, err
		}
		if err := waitFor(ctx, "editor probe", stageTimeout, func() (bool, error) {
			visible, err := readVisible()
			if err != nil {
				return false, nil
			}
			text, ok := editorInputText(visible)
			return ok && text == probe, nil
		}); err != nil {
			return evidencePath, err
		}
		if _, err := frame("editor-probe"); err != nil {
			return evidencePath, err
		}
		if err := sendKey("Escape"); err != nil {
			return evidencePath, err
		}
		timer := time.NewTimer(escapeSettle)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return evidencePath, ctx.Err()
		}
		escapeFrame, err := frame("escape-safe")
		if err != nil {
			return evidencePath, err
		}
		if text, ok := editorInputText(escapeFrame.visible); !ok || text != probe {
			return evidencePath, errors.New("Escape changed the editor probe")
		}
		if !strings.Contains(escapeFrame.visible, "PARENT_SCROLLBACK_MARKER") {
			return evidencePath, errors.New("Escape removed the parent marker from the visible transcript")
		}
		remaining := probe
		for len(remaining) > 0 {
			next := remaining[:len(remaining)-1]
			if err := sendKey("BSpace"); err != nil {
				return evidencePath, err
			}
			if err := waitFor(ctx, "Backspace editor update", stageTimeout, func() (bool, error) {
				visible, err := readVisible()
				if err != nil {
					return false, nil
				}
				text, ok := editorInputText(visible)
				return ok && text == next, nil
			}); err != nil {
				return evidencePath, err
			}
			remaining = next
		}
		cleared, err := frame("editor-cleared")
		if err != nil {
			return evidencePath, err
		}
		if text, ok := editorInputText(cleared.visible); !ok || text != "" {
			return evidencePath, errors.New("Backspace did not empty the editor")
		}
		refs := state.childReferences()
		if len(refs) != 2 {
			return evidencePath, errors.New("stop cannot proceed without two actual spawn results")
		}
		ids := []string{refs[0].ID, refs[1].ID}
		if err := sendText("STOP_WIDGET " + strings.Join(ids, " ")); err != nil {
			return evidencePath, err
		}
		if err := sendKey("Enter"); err != nil {
			return evidencePath, err
		}
		if err := waitFor(ctx, "both stopped results and STOP_DONE", stageTimeout, func() (bool, error) {
			if err := state.providerFailure(); err != nil {
				return false, err
			}
			return state.stoppedReady() && visibleContains(readVisible, "STOP_DONE"), nil
		}); err != nil {
			return evidencePath, err
		}
		stopped, err := waitForLayout(ctx, paneSize, readVisible, readScrollback, 100, 32, func(visible string) bool {
			return widgetShows(visible, "stopped") && strings.Contains(visible, "STOP_DONE")
		}, "the retained stopped widget")
		if err != nil {
			return evidencePath, err
		}
		if err := saveFrame(evidencePath, "stopped-100", stopped); err != nil {
			return evidencePath, err
		}
		if !widgetShows(stopped.visible, "stopped") {
			return evidencePath, errors.New("stopped frame did not retain both stopped children")
		}
		if err := pageUpUntilVisible(ctx, readVisible, sendKey, "PARENT_SCROLLBACK_MARKER"); err != nil {
			return evidencePath, err
		}
		history, err := frame("parent-history-marker")
		if err != nil {
			return evidencePath, err
		}
		if !strings.Contains(history.visible, "PARENT_SCROLLBACK_MARKER") {
			return evidencePath, errors.New("parent marker is absent from the PiG transcript viewport")
		}
		if err := sendKey("C-End"); err != nil {
			return evidencePath, err
		}
		if err := waitFor(ctx, "latest PiG transcript view", stageTimeout, func() (bool, error) {
			return visibleContains(readVisible, "STOP_DONE"), nil
		}); err != nil {
			return evidencePath, err
		}
		latest, err := frame("latest-after-parent-history")
		if err != nil {
			return evidencePath, err
		}
		if !strings.Contains(latest.visible, "STOP_DONE") {
			return evidencePath, errors.New("Ctrl+End did not restore the latest transcript view")
		}
		if text, ok := editorInputText(latest.visible); !ok || text != "" {
			return evidencePath, errors.New("editor must be empty before Ctrl+D")
		}
	}
	if err := sendKey("C-d"); err != nil {
		return evidencePath, err
	}
	if err := waitFor(ctx, "PiG to exit after Ctrl+D", 10*time.Second, func() (bool, error) {
		dead, err := tmuxCall("display-message", "-p", "-t", session, "#{pane_dead}")
		if err != nil {
			return !processExists(panePID), nil
		}
		return dead == "1" && !processExists(panePID), nil
	}); err != nil {
		return evidencePath, err
	}
	paneExited = true
	return evidencePath, nil
}

func newFixtureState(caseName runCase) *fixtureState {
	return &fixtureState{
		caseName:   caseName,
		held:       make(map[string]struct{}),
		aborted:    make(map[string]struct{}),
		childReads: make(map[string]struct{}),
	}
}

func (state *fixtureState) decide(request openAIRequest) (providerTurn, error) {
	marker := latestRootPrompt(request.Messages)
	tools := requestToolNames(request.Tools)
	toolResults := toolResultCounts(request.Messages)
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.requests) < maxEvidenceRequests {
		state.requests = append(state.requests, requestEvidence{
			Model: request.Model, Tools: tools, RootMarker: marker.marker, ToolResultCounts: toolResults,
		})
	} else {
		state.requestsOmitted++
	}
	if request.Model == "held" {
		if state.caseName == caseUnselected {
			return providerTurn{}, errors.New("unselected PiG made a child request")
		}
		name := childName(request.Messages)
		if name == "" {
			return providerTurn{}, errors.New("held child request had no known task")
		}
		if !contains(tools, "read") {
			return providerTurn{}, errors.New("child did not receive the default read tool")
		}
		toolMessages := roleMessages(request.Messages, "tool")
		if len(toolMessages) == 0 {
			return providerTurn{kind: "tools", calls: []toolCall{{name: "read", args: map[string]string{"path": "fixture-read.txt"}}}}, nil
		}
		readComplete := false
		for _, message := range toolMessages {
			if strings.Contains(contentText(message.Content), "TUI_READ_MARKER") {
				readComplete = true
			}
		}
		if !readComplete {
			return providerTurn{}, errors.New("child read did not return the fixture file")
		}
		state.childReads[name] = struct{}{}
		return providerTurn{kind: "hold", name: name}, nil
	}
	if request.Model != "parent" {
		return providerTurn{}, fmt.Errorf("unexpected fixture model %q", request.Model)
	}
	if state.caseName == caseUnselected {
		for _, name := range tools {
			if strings.HasPrefix(name, "litter_") {
				return providerTurn{}, errors.New("unselected PiG exposed a litter tool")
			}
		}
		if marker.marker != "UNSELECTED_WIDGET" {
			return providerTurn{}, errors.New("unselected case got an unexpected prompt")
		}
		state.parentMarker = true
		return providerTurn{kind: "text", text: "UNSELECTED_PARENT_MARKER"}, nil
	}
	for _, name := range controlTools {
		if !contains(tools, name) {
			return providerTurn{}, fmt.Errorf("selected parent is missing %s", name)
		}
	}
	if !contains(tools, "read") {
		return providerTurn{}, errors.New("selected parent is missing its default read tool")
	}
	switch marker.marker {
	case "START_WIDGET":
		results, err := spawnToolResults(request.Messages)
		if err != nil {
			return providerTurn{}, err
		}
		if len(results) == 0 {
			calls := make([]toolCall, 0, len(heldNames))
			for index, name := range heldNames {
				letter := "A"
				if index == 1 {
					letter = "B"
				}
				calls = append(calls, toolCall{name: "litter_spawn", args: map[string]string{
					"type": "scout", "name": name, "model": "fixture/held",
					"task": fmt.Sprintf("Hold widget child %s until the parent explicitly stops it.", letter),
				}})
			}
			return providerTurn{kind: "tools", calls: calls}, nil
		}
		refs := make([]childRef, 0, len(results))
		var failures []failedSpawn
		for _, result := range results {
			if result.Child.ID == "" {
				failure := failedSpawn{
					State: result.State, Code: result.Code, Report: evidenceText(result.Report),
					Name: result.Child.Name, ID: result.Child.ID,
				}
				failures = append(failures, failure)
				continue
			}
			refs = append(refs, childRef{ID: result.Child.ID, Generation: result.Child.Generation, Name: result.Child.Name})
		}
		if len(failures) > 0 {
			state.refs = refs
			state.failedSpawns = append(state.failedSpawns, failures...)
			failure := failures[0]
			return providerTurn{}, fmt.Errorf("litter_spawn failed without a child ID: state=%q code=%q report=%s", failure.State, failure.Code, failure.Report)
		}
		state.refs = refs
		if len(refs) != 2 || refs[0].ID == refs[1].ID {
			return providerTurn{}, errors.New("spawn results did not contain two distinct child IDs")
		}
		for _, name := range heldNames {
			found := false
			for _, ref := range refs {
				if ref.Name == name && ref.Generation == 1 {
					found = true
				}
			}
			if !found {
				return providerTurn{}, fmt.Errorf("spawn results did not include %s at generation 1", name)
			}
		}
		state.parentMarker = true
		return providerTurn{kind: "text", text: "PARENT_SCROLLBACK_MARKER"}, nil
	case "STOP_WIDGET":
		ids := strings.Fields(marker.text)
		if len(ids) != 3 || len(state.refs) != 2 {
			return providerTurn{}, errors.New("stop arrived before actual spawn results or lacks two IDs")
		}
		seen := map[string]bool{}
		for _, id := range ids[1:] {
			if id != state.refs[0].ID && id != state.refs[1].ID {
				return providerTurn{}, errors.New("stop prompt did not use the actual returned IDs")
			}
			seen[id] = true
		}
		if len(seen) != 2 {
			return providerTurn{}, errors.New("stop prompt did not include both distinct child IDs")
		}
		stops, err := stopToolResults(request.Messages)
		if err != nil {
			return providerTurn{}, err
		}
		if len(stops) == 0 {
			calls := make([]toolCall, 0, len(state.refs))
			for _, ref := range state.refs {
				calls = append(calls, toolCall{name: "litter_stop", args: map[string]any{"id": ref.ID, "generation": ref.Generation}})
			}
			return providerTurn{kind: "tools", calls: calls}, nil
		}
		if len(stops) != 2 {
			return providerTurn{}, errors.New("both stop calls did not complete")
		}
		for _, stop := range stops {
			if stop.State != "stopped" {
				return providerTurn{}, errors.New("both stop calls did not return stopped")
			}
		}
		if len(state.held) != 0 || len(state.aborted) != len(heldNames) {
			return providerTurn{}, errors.New("both held provider streams were not cancelled by child stop")
		}
		for _, name := range heldNames {
			if _, ok := state.aborted[name]; !ok {
				return providerTurn{}, fmt.Errorf("held provider stream %s was not cancelled", name)
			}
		}
		state.stopDone = true
		return providerTurn{kind: "text", text: "STOP_DONE"}, nil
	default:
		return providerTurn{}, errors.New("parent request did not contain a recognized fixture marker")
	}
}

func (state *fixtureState) beginHold(name string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, exists := state.held[name]; exists {
		return fmt.Errorf("child %s opened more than one hold stream", name)
	}
	state.held[name] = struct{}{}
	return nil
}

func (state *fixtureState) cancelHold(name string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if _, exists := state.held[name]; exists {
		delete(state.held, name)
		state.aborted[name] = struct{}{}
	}
}

func (state *fixtureState) recordError(err error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.providerError) < maxProviderErrors {
		state.providerError = append(state.providerError, evidenceText(err.Error()))
	} else {
		state.providerErrorsOmitted++
	}
}

func (state *fixtureState) nextCallSequence() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	state.callSequence++
	return state.callSequence
}

func (state *fixtureState) startReady() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.parentMarker && len(state.refs) == 2 && len(state.childReads) == 2 && len(state.held) == 2
}

func (state *fixtureState) stoppedReady() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.stopDone && len(state.held) == 0 && len(state.aborted) == 2
}

func (state *fixtureState) parentMarkerReady() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.parentMarker
}

func (state *fixtureState) hasLitterTools() bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, request := range state.requests {
		for _, name := range request.Tools {
			if strings.HasPrefix(name, "litter_") {
				return true
			}
		}
	}
	return false
}

func (state *fixtureState) selectedTools() []string {
	state.mu.Lock()
	defer state.mu.Unlock()
	set := make(map[string]struct{})
	for _, request := range state.requests {
		if request.Model == "parent" {
			for _, name := range request.Tools {
				set[name] = struct{}{}
			}
		}
	}
	result := make([]string, 0, len(set))
	for name := range set {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func (state *fixtureState) childReferences() []childRef {
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]childRef(nil), state.refs...)
}

func (state *fixtureState) requestEvidence() []requestEvidence {
	state.mu.Lock()
	defer state.mu.Unlock()
	result := append([]requestEvidence(nil), state.requests...)
	for index := range result {
		result[index].Tools = append([]string(nil), result[index].Tools...)
		if result[index].ToolResultCounts != nil {
			counts := make(map[string]int, len(result[index].ToolResultCounts))
			for name, count := range result[index].ToolResultCounts {
				counts[name] = count
			}
			result[index].ToolResultCounts = counts
		}
	}
	return result
}

func (state *fixtureState) requestsOmittedCount() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.requestsOmitted
}

func (state *fixtureState) spawnFailures() []failedSpawn {
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]failedSpawn(nil), state.failedSpawns...)
}

func (state *fixtureState) providerErrorsOmittedCount() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.providerErrorsOmitted
}

func (state *fixtureState) errors() []string {
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]string(nil), state.providerError...)
}

func (state *fixtureState) providerFailure() error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if len(state.providerError) == 0 {
		return nil
	}
	return errors.New(state.providerError[0])
}

func (state *fixtureState) names(source map[string]struct{}) []string {
	state.mu.Lock()
	defer state.mu.Unlock()
	result := make([]string, 0, len(source))
	for name := range source {
		result = append(result, name)
	}
	sort.Strings(result)
	return result
}

func startFixtureProvider(state *fixtureState) (*fixtureProvider, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("start loopback provider: %w", err)
	}
	provider := &fixtureProvider{state: state, port: listener.Addr().(*net.TCPAddr).Port}
	provider.server = &http.Server{
		Handler:           http.HandlerFunc(provider.serveHTTP),
		ReadHeaderTimeout: 5 * time.Second,
		ErrorLog:          log.New(io.Discard, "", 0),
	}
	go func() { _ = provider.server.Serve(listener) }()
	return provider, nil
}

func (provider *fixtureProvider) close() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = provider.server.Shutdown(ctx)
	_ = provider.server.Close()
}

func (provider *fixtureProvider) serveHTTP(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost || !strings.HasSuffix(request.URL.Path, "/chat/completions") {
		http.NotFound(writer, request)
		return
	}
	var body openAIRequest
	if err := json.NewDecoder(io.LimitReader(request.Body, 8<<20)).Decode(&body); err != nil {
		provider.fail(writer, fmt.Errorf("decode provider request: %w", err))
		return
	}
	turn, err := provider.state.decide(body)
	if err != nil {
		provider.fail(writer, err)
		return
	}
	if turn.kind == "hold" {
		if err := provider.state.beginHold(turn.name); err != nil {
			provider.fail(writer, err)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Cache-Control", "no-cache")
		writer.WriteHeader(http.StatusOK)
		if flusher, ok := writer.(http.Flusher); ok {
			flusher.Flush()
		}
		<-request.Context().Done()
		provider.state.cancelHold(turn.name)
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	if turn.kind == "text" {
		provider.writeText(writer, body.Model, turn.text)
		return
	}
	if turn.kind == "tools" {
		provider.writeTools(writer, body.Model, turn.calls)
		return
	}
	provider.fail(writer, fmt.Errorf("unknown fixture response kind %q", turn.kind))
}

func (provider *fixtureProvider) fail(writer http.ResponseWriter, err error) {
	provider.state.recordError(err)
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(http.StatusInternalServerError)
	_ = json.NewEncoder(writer).Encode(map[string]any{"error": map[string]string{"message": evidenceText(err.Error()), "type": "fixture_error"}})
}

func (provider *fixtureProvider) writeText(writer http.ResponseWriter, model, text string) {
	flusher, _ := writer.(http.Flusher)
	_ = writeSSE(writer, flusher, completionChunk{Model: model, Delta: map[string]any{"role": "assistant", "content": text}})
	_ = writeSSE(writer, flusher, completionChunk{Model: model, Delta: map[string]any{}, FinishReason: "stop", Usage: &completionUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}})
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

func (provider *fixtureProvider) writeTools(writer http.ResponseWriter, model string, calls []toolCall) {
	sequence := provider.state.nextCallSequence()
	toolCalls := make([]map[string]any, 0, len(calls))
	for index, call := range calls {
		arguments, _ := json.Marshal(call.args)
		toolCalls = append(toolCalls, map[string]any{
			"index": index,
			"id":    fmt.Sprintf("litter-tui-%d-%d", sequence, index),
			"type":  "function",
			"function": map[string]string{
				"name": call.name, "arguments": string(arguments),
			},
		})
	}
	flusher, _ := writer.(http.Flusher)
	_ = writeSSE(writer, flusher, completionChunk{Model: model, Delta: map[string]any{"role": "assistant", "tool_calls": toolCalls}, FinishReason: "tool_calls", Usage: &completionUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2}})
	_, _ = io.WriteString(writer, "data: [DONE]\n\n")
	if flusher != nil {
		flusher.Flush()
	}
}

type completionUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

type completionChunk struct {
	Model        string
	Delta        map[string]any
	FinishReason string
	Usage        *completionUsage
}

type completionChoice struct {
	Index        int            `json:"index"`
	Delta        map[string]any `json:"delta"`
	FinishReason any            `json:"finish_reason"`
}

func writeSSE(writer io.Writer, flusher http.Flusher, chunk completionChunk) error {
	finishReason := any(nil)
	if chunk.FinishReason != "" {
		finishReason = chunk.FinishReason
	}
	encoded, err := json.Marshal(struct {
		ID      string             `json:"id"`
		Object  string             `json:"object"`
		Created int                `json:"created"`
		Model   string             `json:"model"`
		Choices []completionChoice `json:"choices"`
		Usage   *completionUsage   `json:"usage,omitempty"`
	}{
		ID: "litter-tui-fixture", Object: "chat.completion.chunk", Created: 1, Model: chunk.Model,
		Choices: []completionChoice{{Index: 0, Delta: chunk.Delta, FinishReason: finishReason}}, Usage: chunk.Usage,
	})
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "data: %s\n\n", encoded); err != nil {
		return err
	}
	if flusher != nil {
		flusher.Flush()
	}
	return nil
}

func makeFixtureFiles(workspace, home, pigHome, agentDir string, port int) error {
	for _, dir := range []string{workspace, home, pigHome, agentDir, filepath.Join(workspace, ".pig"), filepath.Join(pigHome, "agent")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create private fixture path: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return fmt.Errorf("secure private fixture path: %w", err)
		}
	}
	files := map[string][]byte{
		filepath.Join(workspace, "litter.yaml"):           []byte("concurrency: 4\ndepth: 2\n"),
		filepath.Join(workspace, "fixture-read.txt"):      []byte("TUI_READ_MARKER\n"),
		filepath.Join(agentDir, "models.json"):            []byte(fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":"http://127.0.0.1:%d/v1","apiKey":%q,"models":[{"id":"parent","name":"parent","contextWindow":32000,"maxTokens":2048},{"id":"held","name":"held","contextWindow":32000,"maxTokens":2048}]}}}`, port, fixtureAPIKey)),
		filepath.Join(agentDir, "auth.json"):              []byte(fmt.Sprintf(`{"fixture":{"type":"api_key","key":%q}}`, fixtureAPIKey)),
		filepath.Join(agentDir, "trust.json"):             []byte(`{}`),
		filepath.Join(agentDir, "settings.json"):          []byte(`{}`),
		filepath.Join(workspace, ".pig", "settings.json"): []byte(`{}`),
		filepath.Join(workspace, ".pig", "trust.json"):    []byte(`{}`),
	}
	for path, content := range files {
		if err := writePrivate(path, content); err != nil {
			return err
		}
	}
	return nil
}

func fixtureEnvironment(pigPath, tmuxPath, home, pigHome, agentDir, socketDir string) []string {
	pathDirs := uniqueNonEmpty([]string{filepath.Dir(pigPath), filepath.Dir(tmuxPath), "/run/current-system/sw/bin", "/usr/bin", "/bin"})
	return []string{
		"PATH=" + strings.Join(pathDirs, string(os.PathListSeparator)),
		"HOME=" + home,
		"SHELL=/bin/sh",
		"LANG=C.UTF-8",
		"TERM=xterm-256color",
		"TMUX_TMPDIR=" + socketDir,
		"PIG_HOME=" + pigHome,
		"PIG_CODING_AGENT_DIR=" + agentDir,
		"PIG_USE_PI_DIRS=0",
		"PIG_OFFLINE=1",
		"PI_OFFLINE=1",
		"PI_TELEMETRY=0",
		"PI_SKIP_VERSION_CHECK=1",
	}
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func copyPigletFixture(manifestPath, workspace, extensionPath string) (string, string, error) {
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		return "", "", fmt.Errorf("read requested Piglet manifest: %w", err)
	}
	origin, err := pigletExtensionOrigin(string(manifest), selectedExtensionName)
	if err != nil {
		return "", "", err
	}
	if filepath.IsAbs(origin) || filepath.Clean(origin) == "." || strings.HasPrefix(filepath.Clean(origin), ".."+string(filepath.Separator)) || filepath.Clean(origin) == ".." {
		return "", "", errors.New("Piglet local extension origin must stay inside the fixture workspace")
	}
	if filepath.Base(origin) != selectedExtensionName {
		return "", "", fmt.Errorf("Piglet origin must name the %s resource", selectedExtensionName)
	}
	resourceDestination := filepath.Join(workspace, filepath.FromSlash(origin))
	if err := os.MkdirAll(filepath.Dir(resourceDestination), 0o700); err != nil {
		return "", "", fmt.Errorf("create Piglet resource directory: %w", err)
	}
	if err := copyExecutable(extensionPath, resourceDestination); err != nil {
		return "", "", err
	}
	manifestDestination := filepath.Join(workspace, filepath.Base(manifestPath))
	if err := writePrivate(manifestDestination, manifest); err != nil {
		return "", "", fmt.Errorf("copy requested Piglet manifest: %w", err)
	}
	return manifestDestination, resourceDestination, nil
}

func pigletExtensionOrigin(manifest, extensionName string) (string, error) {
	lines := strings.Split(manifest, "\n")
	inExtensions := false
	currentName := ""
	foundName := false
	var origin string
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		indent := len(rawLine) - len(strings.TrimLeft(rawLine, " \t"))
		if line == "extensions:" {
			inExtensions = true
			currentName = ""
			continue
		}
		if inExtensions && indent == 0 && line != "" {
			inExtensions = false
		}
		if !inExtensions || line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "- name:") {
			currentName = yamlScalar(strings.TrimSpace(strings.TrimPrefix(line, "- name:")))
			if currentName == extensionName {
				foundName = true
			}
			continue
		}
		if currentName != extensionName || !strings.HasPrefix(line, "origins:") {
			continue
		}
		value := strings.TrimSpace(strings.TrimPrefix(line, "origins:"))
		if !strings.HasPrefix(value, "[") || !strings.HasSuffix(value, "]") {
			return "", errors.New("Piglet extension origins must be an inline list")
		}
		items := strings.Split(strings.TrimSpace(value[1:len(value)-1]), ",")
		if len(items) != 1 {
			return "", errors.New("Piglet fixture requires one selected pig-litter origin")
		}
		item := yamlScalar(strings.TrimSpace(items[0]))
		if !strings.HasPrefix(item, "local:") {
			return "", errors.New("Piglet pig-litter origin must be a local resource")
		}
		origin = strings.TrimPrefix(item, "local:")
		origin = filepath.ToSlash(strings.TrimSpace(origin))
		if origin == "" || strings.HasSuffix(strings.ToLower(origin), ".mjs") || strings.HasSuffix(strings.ToLower(origin), ".js") || strings.HasSuffix(strings.ToLower(origin), ".ts") {
			return "", errors.New("Piglet origin must select a prebuilt Go resource, not JavaScript")
		}
	}
	if !foundName {
		return "", fmt.Errorf("Piglet manifest has no extension named %q", extensionName)
	}
	if origin == "" {
		return "", fmt.Errorf("Piglet extension %q has no local origin", extensionName)
	}
	return origin, nil
}

func yamlScalar(value string) string {
	value = strings.TrimSpace(strings.TrimSuffix(value, ","))
	if len(value) >= 2 && ((value[0] == '\'' && value[len(value)-1] == '\'') || (value[0] == '"' && value[len(value)-1] == '"')) {
		return value[1 : len(value)-1]
	}
	return value
}

func copyExecutable(source, destination string) error {
	if err := requireExecutable(source, "prebuilt Go extension"); err != nil {
		return err
	}
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open prebuilt Go extension: %w", err)
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		return fmt.Errorf("create fixture Go extension: %w", err)
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return fmt.Errorf("copy fixture Go extension: %w", err)
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("close fixture Go extension: %w", err)
	}
	if err := os.Chmod(destination, 0o700); err != nil {
		return fmt.Errorf("make fixture Go extension executable: %w", err)
	}
	return nil
}

func latestRootPrompt(messages []openAIMessage) (marker struct{ marker, text string }) {
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role != "user" {
			continue
		}
		lines := strings.Split(strings.ReplaceAll(contentText(messages[index].Content), "\r\n", "\n"), "\n")
		for lineIndex := len(lines) - 1; lineIndex >= 0; lineIndex-- {
			line := strings.TrimSpace(lines[lineIndex])
			if line == "START_WIDGET" || line == "UNSELECTED_WIDGET" {
				return struct{ marker, text string }{marker: line, text: line}
			}
			if strings.HasPrefix(line, "STOP_WIDGET ") {
				return struct{ marker, text string }{marker: "STOP_WIDGET", text: line}
			}
		}
	}
	return struct{ marker, text string }{}
}

func contentText(content json.RawMessage) string {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(content, &blocks) == nil {
		parts := make([]string, 0, len(blocks))
		for _, block := range blocks {
			if block.Type == "text" {
				parts = append(parts, block.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func roleMessages(messages []openAIMessage, role string) []openAIMessage {
	var result []openAIMessage
	for _, message := range messages {
		if message.Role == role {
			result = append(result, message)
		}
	}
	return result
}

func childName(messages []openAIMessage) string {
	serialized, _ := json.Marshal(messages)
	text := string(serialized)
	if strings.Contains(text, "Hold widget child A") {
		return "held-a"
	}
	if strings.Contains(text, "Hold widget child B") {
		return "held-b"
	}
	return ""
}

func requestToolNames(tools []openAITool) []string {
	result := make([]string, 0, len(tools))
	for _, tool := range tools {
		if tool.Function.Name != "" {
			result = append(result, tool.Function.Name)
		}
	}
	return result
}

type toolResult struct {
	Operation string `json:"operation"`
	State     string `json:"state"`
	Code      string `json:"code"`
	Report    string `json:"report"`
	Child     struct {
		ID         string `json:"id"`
		Generation int    `json:"generation"`
		Name       string `json:"name"`
	} `json:"child"`
}

func toolResultValues(messages []openAIMessage) ([]toolResult, error) {
	results := roleMessages(messages, "tool")
	values := make([]toolResult, 0, len(results))
	for _, message := range results {
		text := contentText(message.Content)
		var value toolResult
		if err := json.Unmarshal([]byte(text), &value); err != nil {
			return nil, fmt.Errorf("decode tool result: %w", err)
		}
		if value.Operation == "" {
			return nil, errors.New("tool result has no operation")
		}
		values = append(values, value)
	}
	return values, nil
}

func toolResultCounts(messages []openAIMessage) map[string]int {
	toolMessages := roleMessages(messages, "tool")
	result := make(map[string]int)
	for _, message := range toolMessages {
		var value toolResult
		if err := json.Unmarshal([]byte(contentText(message.Content)), &value); err != nil || value.Operation == "" {
			result["unparsed"]++
			continue
		}
		result[value.Operation]++
	}
	return result
}

func spawnToolResults(messages []openAIMessage) ([]toolResult, error) {
	values, err := toolResultValues(messages)
	if err != nil {
		return nil, err
	}
	var result []toolResult
	for _, value := range values {
		if value.Operation == "litter_spawn" {
			result = append(result, value)
		}
	}
	return result, nil
}

func stopToolResults(messages []openAIMessage) ([]toolResult, error) {
	values, err := toolResultValues(messages)
	if err != nil {
		return nil, err
	}
	var result []toolResult
	for _, value := range values {
		if value.Operation == "litter_stop" {
			result = append(result, value)
		}
	}
	return result, nil
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func latestWidgetLines(frame string) []string {
	lines := strings.Split(frame, "\n")
	heading := -1
	for index, line := range lines {
		if strings.Contains(line, "Litter") {
			heading = index
		}
	}
	if heading < 0 {
		return nil
	}
	end := heading + 3
	if end > len(lines) {
		end = len(lines)
	}
	result := make([]string, 0, end-heading)
	for _, line := range lines[heading:end] {
		result = append(result, strings.TrimSpace(line))
	}
	return result
}

func widgetShows(frame, state string) bool {
	lines := latestWidgetLines(frame)
	heading := "Litter 2 live 0 kept"
	if state == "stopped" {
		heading = "Litter 0 live 2 kept"
	}
	if len(lines) != 3 || lines[0] != heading {
		return false
	}
	return contains(lines, "held-a "+state) && contains(lines, "held-b "+state)
}

func parsePaneGeometry(value string) (paneGeometry, error) {
	parts := strings.Fields(value)
	if len(parts) != 2 {
		return paneGeometry{}, errors.New("invalid tmux pane geometry")
	}
	width, widthErr := strconv.Atoi(parts[0])
	height, heightErr := strconv.Atoi(parts[1])
	if widthErr != nil || heightErr != nil || width <= 0 || height <= 0 {
		return paneGeometry{}, errors.New("invalid tmux pane geometry")
	}
	return paneGeometry{width: width, height: height}, nil
}

func frameHasGeometry(frame string, width, height int) bool {
	lines := strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
	if len(lines) != height {
		return false
	}
	for _, line := range lines {
		if utf8.RuneCountInString(line) == width {
			return true
		}
	}
	return false
}

func startupFrameReady(frame string) bool {
	if !strings.Contains(frame, "v0.4.1+1.0.3") {
		return false
	}
	parentFooter := false
	borders := 0
	for _, line := range strings.Split(frame, "\n") {
		if fields := strings.Fields(line); len(fields) > 0 && fields[len(fields)-1] == "parent" {
			parentFooter = true
		}
		if horizontalBorder(line) {
			borders++
		}
	}
	return parentFooter && borders >= 2
}

func horizontalBorder(line string) bool {
	line = strings.TrimSpace(line)
	runes := []rune(line)
	if len(runes) < 29 {
		return false
	}
	for _, character := range runes {
		if character != '─' {
			return false
		}
	}
	return true
}

func editorInputText(frame string) (string, bool) {
	lines := strings.Split(frame, "\n")
	var borders []int
	for index, line := range lines {
		if horizontalBorder(line) {
			borders = append(borders, index)
		}
	}
	if len(borders) < 2 {
		return "", false
	}
	start, end := borders[len(borders)-2]+1, borders[len(borders)-1]
	if end < start {
		return "", false
	}
	return strings.TrimSpace(strings.Join(lines[start:end], "\n")), true
}

func sanitizeText(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, fixtureAPIKey, "[redacted]"))
}

func evidenceText(value string) string {
	value = sanitizeText(value)
	runes := []rune(value)
	if len(runes) > 512 {
		return string(runes[:512]) + "…"
	}
	return value
}

func joinShellCommand(program string, args []string) string {
	parts := make([]string, 0, len(args)+1)
	parts = append(parts, shellQuote(program))
	for _, arg := range args {
		parts = append(parts, shellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\\''") + "'"
}

func runTmux(ctx context.Context, binary, socket string, env []string, args ...string) (string, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	allArgs := []string{"-S", socket, "-f", "/dev/null"}
	allArgs = append(allArgs, args...)
	command := exec.CommandContext(commandCtx, binary, allArgs...)
	command.Env = env
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		if stderr.Len() > 0 {
			return "", fmt.Errorf("%w: %s", err, evidenceText(stderr.String()))
		}
		return "", err
	}
	return strings.TrimRight(stdout.String(), "\r\n"), nil
}

func waitFor(ctx context.Context, label string, timeout time.Duration, check func() (bool, error)) error {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(40 * time.Millisecond)
	defer ticker.Stop()
	for {
		ready, err := check()
		if err != nil {
			return fmt.Errorf("%s: %w", label, err)
		}
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("timed out waiting for %s: %w", label, ctx.Err())
		case <-deadline.C:
			return fmt.Errorf("timed out waiting for %s", label)
		case <-ticker.C:
		}
	}
}

func waitForLayout(ctx context.Context, geometry func() (paneGeometry, error), visible, scrollback func() (string, error), width, height int, ready func(string) bool, label string) (framePair, error) {
	var captured framePair
	err := waitFor(ctx, label, stageTimeout, func() (bool, error) {
		pane, err := geometry()
		if err != nil {
			return false, nil
		}
		current, err := visible()
		if err != nil {
			return false, nil
		}
		old, err := scrollback()
		if err != nil {
			return false, nil
		}
		captured = framePair{visible: current, scrollback: old}
		return pane.width == width && pane.height == height && frameHasGeometry(current, width, height) && ready(current), nil
	})
	return captured, err
}

func visibleContains(readVisible func() (string, error), needle string) bool {
	visible, err := readVisible()
	return err == nil && strings.Contains(visible, needle)
}

func pageUpUntilVisible(ctx context.Context, readVisible func() (string, error), sendKey func(string) error, needle string) error {
	for page := 0; page < 24; page++ {
		visible, err := readVisible()
		if err != nil {
			return err
		}
		if strings.Contains(visible, needle) {
			return nil
		}
		if err := sendKey("PageUp"); err != nil {
			return err
		}
		if err := waitFor(ctx, "PiG transcript viewport to move", 1500*time.Millisecond, func() (bool, error) {
			next, err := readVisible()
			return err == nil && (strings.Contains(next, needle) || next != visible), nil
		}); err != nil {
			return err
		}
	}
	return errors.New("parent marker did not appear in the PiG transcript viewport")
}

func saveFrame(directory, name string, frame framePair) error {
	for filename, content := range map[string]string{
		name + "-visible.txt":    frame.visible,
		name + "-scrollback.txt": frame.scrollback,
	} {
		if err := writePrivate(filepath.Join(directory, filename), []byte(content)); err != nil {
			return err
		}
	}
	return nil
}

func writePrivate(path string, content []byte) error {
	if err := os.WriteFile(path, content, 0o600); err != nil {
		return fmt.Errorf("write private evidence %s: %w", filepath.Base(path), err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("secure private evidence %s: %w", filepath.Base(path), err)
	}
	return nil
}

func processExists(pid int) bool {
	if pid <= 0 {
		return false
	}
	_, err := os.Stat(filepath.Join("/proc", strconv.Itoa(pid), "status"))
	return err == nil
}

func waitForProcessExit(ctx context.Context, pid int) bool {
	if pid <= 0 {
		return true
	}
	return waitUntilDone(ctx, func() bool { return !processExists(pid) })
}

func tmuxSocketStatus(path string) (exists, alive bool, err error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, false, nil
	}
	if err != nil {
		return false, false, err
	}
	if info.Mode()&os.ModeSocket == 0 {
		return true, false, fmt.Errorf("private tmux socket path is not a socket")
	}
	connection, err := net.DialTimeout("unix", path, 100*time.Millisecond)
	if err == nil {
		_ = connection.Close()
		return true, true, nil
	}
	if errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.ENOENT) || errors.Is(err, os.ErrNotExist) {
		return true, false, nil
	}
	return true, false, err
}

func waitForOwnedTmuxExit(ctx context.Context, path string) (closed, alive bool) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		exists, listening, err := tmuxSocketStatus(path)
		if err == nil {
			if !exists {
				return true, false
			}
			if !listening {
				if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
					return false, false
				}
				return true, false
			}
		}
		select {
		case <-ctx.Done():
			return false, listening
		case <-ticker.C:
		}
	}
}

func waitUntilDone(ctx context.Context, check func() bool) bool {
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		if check() {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
	}
}
