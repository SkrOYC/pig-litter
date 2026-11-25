package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

type collaborationTUIMessageRequest struct {
	Token     string `json:"token"`
	Operation string `json:"operation"`
	ID        string `json:"id"`
}

type collaborationTUIMessageState struct {
	SessionID    string `json:"session_id"`
	SessionFile  string `json:"session_file"`
	Idle         bool   `json:"idle"`
	MemoryCopies int    `json:"memory_copies"`
	Error        string `json:"error,omitempty"`
}

type collaborationTUIMessageResponse struct {
	Process    observedProcess              `json:"process"`
	Operation  string                       `json:"operation"`
	ID         string                       `json:"id"`
	Before     collaborationTUIMessageState `json:"before"`
	After      collaborationTUIMessageState `json:"after"`
	StartedNS  int64                        `json:"started_ns,omitempty"`
	ReturnedNS int64                        `json:"returned_ns,omitempty"`
	CallError  string                       `json:"call_error,omitempty"`
}

type collaborationTUIMessageBridge struct {
	Name        string          `json:"name"`
	SHA256      string          `json:"sha256"`
	Socket      string          `json:"socket"`
	SessionID   string          `json:"session_id"`
	SessionFile string          `json:"session_file"`
	Process     observedProcess `json:"process"`
}

type collaborationTUIDiskSnapshot struct {
	Exists bool   `json:"exists"`
	Copies int    `json:"copies"`
	Path   string `json:"snapshot,omitempty"`
}

type collaborationTUIMessagePhase struct {
	Name           string                          `json:"name"`
	Response       collaborationTUIMessageResponse `json:"response"`
	Before         collaborationTUIDiskSnapshot    `json:"disk_before"`
	After          collaborationTUIDiskSnapshot    `json:"disk_after"`
	ProviderBefore int                             `json:"provider_before"`
	ProviderAfter  int                             `json:"provider_after"`
	Error          string                          `json:"error,omitempty"`
}

type collaborationTUIMessageReceipt struct {
	Schema         int                            `json:"schema"`
	Mode           string                         `json:"mode"`
	Status         string                         `json:"status"`
	Bridge         collaborationTUIMessageBridge  `json:"bridge"`
	PiG            observedProcess                `json:"pig"`
	HoldObservedNS int64                          `json:"hold_observed_ns,omitempty"`
	ReleaseStartNS int64                          `json:"release_start_ns,omitempty"`
	HoldReleasedNS int64                          `json:"hold_released_ns,omitempty"`
	SettledNS      int64                          `json:"settled_ns,omitempty"`
	Phases         []collaborationTUIMessagePhase `json:"phases"`
	Error          string                         `json:"error,omitempty"`
}

type collaborationTUIMessageProbe struct {
	mode, token, socket, directory, sessions, executable      string
	bridge                                                    collaborationTUIMessageBridge
	phases                                                    []collaborationTUIMessagePhase
	holdObservedNS, releaseStartNS, holdReleasedNS, settledNS int64
}

var collaborationTUIMessageID = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)

const collaborationTUIMessageExtensionName = "collab-message-probe"

func collaborationStageTUIMessageExtension(scratch, source, expectedSHA string) (string, error) {
	path := filepath.Join(scratch, collaborationTUIMessageExtensionName)
	if err := copyExecutable(source, path); err != nil {
		return "", err
	}
	digest, err := fileSHA256(path)
	if err != nil || digest != expectedSHA {
		return "", errors.New("staged message extension differs from the verifier")
	}
	return path, nil
}

func collaborationTUIMessageSnapshot(ctx sdk.Context, id string) collaborationTUIMessageState {
	var state collaborationTUIMessageState
	var errs []error
	var err error
	state.Idle, err = ctx.IsIdle()
	errs = append(errs, err)
	state.SessionID, err = ctx.SessionManager().GetSessionID()
	errs = append(errs, err)
	path, err := ctx.SessionManager().GetSessionFile()
	errs = append(errs, err)
	if path != nil {
		state.SessionFile = *path
	}
	entries, err := ctx.SessionManager().GetEntries()
	errs = append(errs, err)
	for _, entry := range entries {
		details, _ := entry["details"].(map[string]any)
		if entry["type"] == "custom_message" && entry["customType"] == "collab_tui_probe" && details["message_id"] == id {
			state.MemoryCopies++
		}
	}
	if err := errors.Join(errs...); err != nil {
		state.Error = err.Error()
	}
	return state
}

func collaborationServeTUIMessage(listener net.Listener, token string, ctx sdk.Context, process observedProcess) {
	defer listener.Close()
	for {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		func() {
			defer connection.Close()
			_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
			var request collaborationTUIMessageRequest
			if json.NewDecoder(io.LimitReader(connection, 4096)).Decode(&request) != nil || request.Token != token || (request.Operation != "inspect" && request.Operation != "send") || !collaborationTUIMessageID.MatchString(request.ID) {
				return
			}
			response := collaborationTUIMessageResponse{Process: process, Operation: request.Operation, ID: request.ID, Before: collaborationTUIMessageSnapshot(ctx, request.ID)}
			if request.Operation == "send" && response.Before.Error == "" {
				trigger := false
				response.StartedNS, err = collaborationMonotonicNow()
				if err == nil {
					err = ctx.SendCustomMessage(sdk.CustomMessage{CustomType: "collab_tui_probe", Content: "peer message " + request.ID, Display: false, Details: map[string]any{"message_id": request.ID}}, sdk.SendMessageOptions{TriggerTurn: &trigger})
				}
				response.ReturnedNS, _ = collaborationMonotonicNow()
				if err != nil {
					response.CallError = err.Error()
				}
			}
			response.After = collaborationTUIMessageSnapshot(ctx, request.ID)
			_ = json.NewEncoder(connection).Encode(response)
		}()
	}
}

func runCollaborationTUIMessageExtension() error {
	socket, token, directory := os.Getenv("COLLAB_TUI_MESSAGE_SOCKET"), os.Getenv("COLLAB_TUI_MESSAGE_TOKEN"), os.Getenv("COLLAB_TUI_MESSAGE_DIRECTORY")
	if !filepath.IsAbs(socket) || !filepath.IsAbs(directory) || len(token) < 20 {
		return errors.New("invalid private TUI message fixture configuration")
	}
	extension := sdk.New(collaborationTUIMessageExtensionName)
	var mu sync.Mutex
	var listener net.Listener
	extension.OnEvent(sdk.EventSessionStart, func(ctx sdk.Context, _ map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		if listener != nil {
			return nil, errors.New("message probe session started twice")
		}
		id, err := ctx.SessionManager().GetSessionID()
		if err != nil {
			return nil, err
		}
		file, err := ctx.SessionManager().GetSessionFile()
		if err != nil || file == nil {
			return nil, errors.New("message probe has no native session file")
		}
		process, err := readObservedProcess(os.Getpid())
		if err != nil {
			return nil, err
		}
		self, err := os.Executable()
		if err != nil {
			return nil, err
		}
		digest, err := fileSHA256(self)
		if err != nil {
			return nil, err
		}
		listener, err = net.Listen("unix", socket)
		if err != nil {
			return nil, err
		}
		if err := os.Chmod(socket, 0o600); err != nil {
			_ = listener.Close()
			return nil, err
		}
		bridge := collaborationTUIMessageBridge{Name: collaborationTUIMessageExtensionName, SHA256: digest, Socket: socket, SessionID: id, SessionFile: *file, Process: process}
		if err := writeCaptureJSON(filepath.Join(directory, "bridge.json"), bridge); err != nil {
			_ = listener.Close()
			return nil, err
		}
		go collaborationServeTUIMessage(listener, token, ctx, process)
		return nil, nil
	})
	extension.OnEvent(sdk.EventSessionShutdown, func(_ sdk.Context, _ map[string]any) (any, error) {
		mu.Lock()
		defer mu.Unlock()
		if listener != nil {
			if err := listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
				return nil, err
			}
		}
		return nil, nil
	})
	return extension.Run()
}

func (probe *collaborationTUIMessageProbe) connect(ctx context.Context, process observedProcess, descendants []observedProcess) error {
	if err := waitFor(ctx, "private message probe", stageTimeout, func() (bool, error) {
		data, err := os.ReadFile(filepath.Join(probe.directory, "bridge.json"))
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(data, &probe.bridge); err != nil {
			return false, err
		}
		return true, nil
	}); err != nil {
		return err
	}
	relative, err := filepath.Rel(probe.sessions, probe.bridge.SessionFile)
	if err != nil || !filepath.IsLocal(relative) || relative == "." || probe.bridge.Socket != probe.socket || probe.bridge.Name != collaborationTUIMessageExtensionName || probe.bridge.SessionID == "" || process.PID <= 0 {
		return errors.New("message probe is not bound to the selected native session")
	}
	stagedInfo, err := os.Stat(probe.executable)
	if err != nil {
		return err
	}
	digest, err := fileSHA256(probe.executable)
	if err != nil || digest != probe.bridge.SHA256 {
		return errors.New("message probe digest differs from the staged verifier")
	}
	for _, child := range descendants {
		if child == probe.bridge.Process && child.PID != process.PID && observedAlive(child) {
			info, err := os.Stat(child.Executable)
			if err == nil && os.SameFile(stagedInfo, info) {
				return nil
			}
		}
	}
	return errors.New("message probe extension is not an owned PiG descendant")
}

func (probe *collaborationTUIMessageProbe) call(ctx context.Context, operation, id string) (collaborationTUIMessageResponse, error) {
	var response collaborationTUIMessageResponse
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", probe.socket)
	if err != nil {
		return response, err
	}
	defer connection.Close()
	_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	if err := json.NewEncoder(connection).Encode(collaborationTUIMessageRequest{Token: probe.token, Operation: operation, ID: id}); err != nil {
		return response, err
	}
	if err := json.NewDecoder(io.LimitReader(connection, 1<<20)).Decode(&response); err != nil {
		return response, err
	}
	if response.Process != probe.bridge.Process || response.ID != id || response.Operation != operation || response.Before.SessionID != probe.bridge.SessionID || response.After.SessionID != probe.bridge.SessionID || response.Before.SessionFile != probe.bridge.SessionFile || response.After.SessionFile != probe.bridge.SessionFile || response.Before.Error != "" || response.After.Error != "" || response.CallError != "" || (operation == "send" && (response.StartedNS <= 0 || response.ReturnedNS < response.StartedNS)) {
		return response, errors.New("message probe response lacks attributable session or call timing")
	}
	return response, nil
}

func (probe *collaborationTUIMessageProbe) disk(name, id string) (collaborationTUIDiskSnapshot, error) {
	snapshot := collaborationTUIDiskSnapshot{}
	before, err := os.Lstat(probe.bridge.SessionFile)
	if os.IsNotExist(err) {
		return snapshot, nil
	}
	if err != nil {
		return snapshot, err
	}
	if !before.Mode().IsRegular() || before.Mode()&os.ModeSymlink != 0 {
		return snapshot, errors.New("native session is not a regular file")
	}
	file, err := os.Open(probe.bridge.SessionFile)
	if err != nil {
		return snapshot, err
	}
	opened, statErr := file.Stat()
	data, readErr := io.ReadAll(io.LimitReader(file, (8<<20)+1))
	closeErr := file.Close()
	if err := errors.Join(statErr, readErr, closeErr); err != nil {
		return snapshot, err
	}
	if !os.SameFile(before, opened) {
		return snapshot, errors.New("native session changed while opening")
	}
	if len(data) > 8<<20 {
		return snapshot, errors.New("native session snapshot exceeds limit")
	}
	snapshot.Exists = true
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	for scanner.Scan() {
		var entry struct {
			Type       string `json:"type"`
			CustomType string `json:"customType"`
			Details    struct {
				MessageID string `json:"message_id"`
			} `json:"details"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return snapshot, err
		}
		if entry.Type == "custom_message" && entry.CustomType == "collab_tui_probe" && entry.Details.MessageID == id {
			snapshot.Copies++
		}
	}
	if err := scanner.Err(); err != nil {
		return snapshot, err
	}
	snapshot.Path = name + ".jsonl"
	if err := writePrivate(filepath.Join(probe.directory, snapshot.Path), data); err != nil {
		return snapshot, err
	}
	return snapshot, nil
}

func (probe *collaborationTUIMessageProbe) observe(ctx context.Context, name, operation, id string, provider func() int) (phase collaborationTUIMessagePhase, resultErr error) {
	phase = collaborationTUIMessagePhase{Name: name, ProviderBefore: provider()}
	defer func() {
		if resultErr != nil {
			phase.Error = resultErr.Error()
		}
		probe.phases = append(probe.phases, phase)
	}()
	var err error
	phase.Before, err = probe.disk(name+"-before", id)
	if err != nil {
		return phase, err
	}
	phase.Response, err = probe.call(ctx, operation, id)
	if err != nil {
		return phase, err
	}
	phase.After, err = probe.disk(name+"-after", id)
	phase.ProviderAfter = provider()
	return phase, err
}

func (probe *collaborationTUIMessageProbe) save(process observedProcess, runErr error) error {
	var validationErr error
	if runErr == nil && (process.PID <= 0 || probe.bridge.Process.PID <= 0 || probe.bridge.SessionID == "" || (probe.mode == "host-idle-message" && len(probe.phases) != 3) || (probe.mode == "host-busy-message" && len(probe.phases) != 2)) {
		validationErr = errors.New("message probe observation is incomplete")
		runErr = validationErr
	}
	receipt := collaborationTUIMessageReceipt{Schema: 1, Mode: probe.mode, Status: "observed", Bridge: probe.bridge, PiG: process, HoldObservedNS: probe.holdObservedNS, ReleaseStartNS: probe.releaseStartNS, HoldReleasedNS: probe.holdReleasedNS, SettledNS: probe.settledNS, Phases: probe.phases}
	if runErr != nil {
		receipt.Status, receipt.Error = "inconclusive", runErr.Error()
	}
	return errors.Join(validationErr, writeCaptureJSON(filepath.Join(probe.directory, "receipt.json"), receipt))
}

func runCollaborationTUIMessageScenario(ctx context.Context, probe *collaborationTUIMessageProbe, panel *panelProof, state *fixtureState, sendText func(string) error, sendKey func(string) error, visible func() (string, error), frame func(string) (framePair, error), media *collaborationMedia) error {
	if panel == nil || media == nil || panel.rootID != probe.bridge.SessionID || panel.rootPath != probe.bridge.SessionFile {
		return errors.New("message probe does not match the terminal's native root session")
	}
	if probe.mode == "host-idle-message" {
		flushed, err := probe.observe(ctx, "fresh-flushed", "inspect", "COLLAB_FRESH", state.totalRequests)
		if err != nil || !flushed.Response.After.Idle || flushed.After.Copies != 1 || flushed.Response.After.MemoryCopies != 1 || flushed.ProviderBefore != 1 || flushed.ProviderAfter != 1 {
			return fmt.Errorf("fresh message flush observation: %#v, %v", flushed, err)
		}
		idle, err := probe.observe(ctx, "established-idle", "send", "COLLAB_IDLE", state.totalRequests)
		if err != nil || !idle.Response.Before.Idle || !idle.Response.After.Idle || idle.Response.Before.MemoryCopies != 0 || idle.Response.After.MemoryCopies != 1 || idle.Before.Copies != 0 || idle.After.Copies != 1 || idle.ProviderBefore != 1 || idle.ProviderAfter != 1 {
			return fmt.Errorf("established idle observation: %#v, %v", idle, err)
		}
		if _, err := frame("message-idle"); err != nil {
			return err
		}
		if err := media.markObservation(); err != nil {
			return err
		}
		return collaborationTUIHoldFrame(ctx)
	}
	if probe.mode != "host-busy-message" {
		return errors.New("unknown TUI message scenario")
	}
	if err := sendText("COLLAB_BUSY_HOLD"); err != nil {
		return err
	}
	if err := sendKey("Enter"); err != nil {
		return err
	}
	if err := waitFor(ctx, "busy message provider barrier", stageTimeout, func() (bool, error) { return state.holding("collab-busy"), state.providerFailure() }); err != nil {
		return err
	}
	var err error
	probe.holdObservedNS, err = collaborationMonotonicNow()
	if err != nil {
		return err
	}
	defer func() {
		if state.holding("collab-busy") {
			_ = state.releaseHold("collab-busy")
		}
	}()
	if _, err := frame("message-busy"); err != nil {
		return err
	}
	if err := media.markObservation(); err != nil {
		return err
	}
	busy, err := probe.observe(ctx, "busy-return", "send", "COLLAB_BUSY", state.totalRequests)
	if err != nil || busy.Response.Before.Idle || busy.Response.After.Idle || busy.Response.Before.MemoryCopies != 0 || busy.Before.Copies != 0 || busy.ProviderBefore != 2 || busy.ProviderAfter != 2 || !state.holding("collab-busy") {
		return fmt.Errorf("held busy message observation: %#v, %v", busy, err)
	}
	if err := collaborationTUIHoldFrame(ctx); err != nil {
		return err
	}
	probe.releaseStartNS, err = collaborationMonotonicNow()
	if err != nil {
		return err
	}
	if err := state.releaseHold("collab-busy"); err != nil {
		return err
	}
	probe.holdReleasedNS, err = collaborationMonotonicNow()
	if err != nil {
		return err
	}
	if err := waitFor(ctx, "busy message turn settlement", stageTimeout, func() (bool, error) {
		if err := state.providerFailure(); err != nil {
			return false, err
		}
		if state.holding("collab-busy") || !visibleContains(visible, "COLLAB_BUSY_HOLD_DONE") {
			return false, nil
		}
		response, err := probe.call(ctx, "inspect", "COLLAB_BUSY")
		return err == nil && response.After.Idle && response.After.MemoryCopies == 1, err
	}); err != nil {
		return err
	}
	settled, err := probe.observe(ctx, "busy-settled", "inspect", "COLLAB_BUSY", state.totalRequests)
	if err != nil || !settled.Response.After.Idle || settled.Response.After.MemoryCopies != 1 || settled.After.Copies != 1 || settled.ProviderBefore != 2 || settled.ProviderAfter != 2 {
		return fmt.Errorf("settled busy message observation: %#v, %v", settled, err)
	}
	probe.settledNS, err = collaborationMonotonicNow()
	if err != nil || probe.holdObservedNS <= 0 || probe.holdObservedNS > busy.Response.StartedNS || busy.Response.ReturnedNS > probe.releaseStartNS || probe.releaseStartNS > probe.holdReleasedNS || probe.holdReleasedNS > probe.settledNS {
		return errors.New("busy message timing boundaries are inconsistent")
	}
	return nil
}

func collaborationTUIHoldFrame(ctx context.Context) error {
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
