package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

const collaborationTUIHookName = "collab-hook-probe"
const collaborationTUIHookRefusal = "COLLAB_HOOK_REFUSED"
const collaborationTUIHookContent = "COLLAB_HOOK_FIXTURE"

type collaborationTUIHookBridge struct {
	Name        string          `json:"name"`
	SHA256      string          `json:"sha256"`
	SessionID   string          `json:"session_id"`
	SessionFile string          `json:"session_file"`
	Process     observedProcess `json:"process"`
}

type collaborationTUIHookEvent struct {
	Tool      string          `json:"tool"`
	CallID    string          `json:"call_id"`
	Path      string          `json:"path"`
	Refused   bool            `json:"refused"`
	SessionID string          `json:"session_id"`
	Process   observedProcess `json:"process"`
}

type collaborationTUIHookPhase struct {
	Name       string                    `json:"name"`
	Event      collaborationTUIHookEvent `json:"event"`
	FileExists bool                      `json:"file_exists"`
	Content    string                    `json:"content,omitempty"`
}

type collaborationTUIHookReceipt struct {
	Schema int                         `json:"schema"`
	Status string                      `json:"status"`
	Error  string                      `json:"error,omitempty"`
	PiG    observedProcess             `json:"pig"`
	Bridge collaborationTUIHookBridge  `json:"bridge"`
	Phases []collaborationTUIHookPhase `json:"phases"`
}

type collaborationTUIHookProbe struct {
	directory  string
	workspace  string
	executable string
	bridge     collaborationTUIHookBridge
	phases     []collaborationTUIHookPhase
}

func runCollaborationTUIHookExtension() error {
	directory, workspace := os.Getenv("COLLAB_TUI_HOOK_DIRECTORY"), os.Getenv("COLLAB_TUI_HOOK_WORKSPACE")
	if !filepath.IsAbs(directory) || !filepath.IsAbs(workspace) {
		return errors.New("invalid private TUI hook fixture configuration")
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	digest, err := fileSHA256(self)
	if err != nil {
		return err
	}
	process, err := readObservedProcess(os.Getpid())
	if err != nil {
		return err
	}
	extension := sdk.New(collaborationTUIHookName)
	extension.OnEvent(sdk.EventSessionStart, func(ctx sdk.Context, _ map[string]any) (any, error) {
		id, err := ctx.SessionManager().GetSessionID()
		if err != nil {
			return nil, err
		}
		file, err := ctx.SessionManager().GetSessionFile()
		if err != nil || file == nil {
			return nil, errors.New("hook probe has no native session file")
		}
		bridge := collaborationTUIHookBridge{Name: collaborationTUIHookName, SHA256: digest, SessionID: id, SessionFile: *file, Process: process}
		return nil, writeCaptureJSON(filepath.Join(directory, "bridge.json"), bridge)
	})
	extension.OnEvent(sdk.EventToolCall, func(ctx sdk.Context, event map[string]any) (any, error) {
		if event["toolName"] != "write" {
			return nil, nil
		}
		input, _ := event["input"].(map[string]any)
		path, _ := input["path"].(string)
		name := ""
		switch path {
		case filepath.Join(workspace, "collab-hook-allow.txt"):
			name = "allow"
		case filepath.Join(workspace, "collab-hook-deny.txt"):
			name = "deny"
		default:
			return nil, nil
		}
		callID, _ := event["toolCallId"].(string)
		id, err := ctx.SessionManager().GetSessionID()
		if err != nil || callID == "" {
			return nil, errors.New("hook event lacks a session or call identity")
		}
		observation := collaborationTUIHookEvent{Tool: "write", CallID: callID, Path: path, Refused: name == "deny", SessionID: id, Process: process}
		file, err := os.OpenFile(filepath.Join(directory, name+".json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return nil, err
		}
		writeErr := json.NewEncoder(file).Encode(observation)
		if err := errors.Join(writeErr, file.Sync(), file.Close()); err != nil {
			return nil, err
		}
		if observation.Refused {
			return map[string]any{"block": true, "reason": collaborationTUIHookRefusal}, nil
		}
		return nil, nil
	})
	return extension.Run()
}

func (state *fixtureState) hookTurnLocked(request openAIRequest, marker string, tools []string) (providerTurn, error) {
	if state.caseName != caseDirect || request.Model != "parent" {
		return providerTurn{}, errors.New("hook fixture requires a selected direct root")
	}
	if err := requireToolInventory(tools, append(append([]string(nil), fileTools...), controlTools...), []string{"ls"}, "hook root"); err != nil {
		return providerTurn{}, err
	}
	name, expected := "allow", "Successfully wrote"
	if marker == "COLLAB_HOOK_DENY" {
		name, expected = "deny", collaborationTUIHookRefusal
	}
	lastUser := -1
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "user" && strings.TrimSpace(contentText(request.Messages[index].Content)) == marker {
			lastUser = index
			break
		}
	}
	if lastUser < 0 {
		return providerTurn{}, errors.New("hook fixture prompt is absent")
	}
	var results []openAIMessage
	for _, message := range request.Messages[lastUser+1:] {
		if message.Role == "tool" {
			results = append(results, message)
		}
	}
	if len(results) == 0 {
		path := filepath.Join(state.fixtureWorkspace, "collab-hook-"+name+".txt")
		return providerTurn{kind: "tools", calls: []toolCall{{name: "write", args: map[string]string{"path": path, "content": collaborationTUIHookContent}}}}, nil
	}
	if len(results) != 1 || results[0].ToolCallID == "" || !strings.Contains(contentText(results[0].Content), expected) {
		return providerTurn{}, fmt.Errorf("hook %s caller result did not match its expected outcome", name)
	}
	return providerTurn{kind: "text", text: marker + "_DONE"}, nil
}

func (probe *collaborationTUIHookProbe) connect(ctx context.Context, panel *panelProof, verifierSHA string) error {
	if panel == nil {
		return errors.New("hook probe has no selected root session")
	}
	if err := waitFor(ctx, "private hook probe", stageTimeout, func() (bool, error) {
		data, err := os.ReadFile(filepath.Join(probe.directory, "bridge.json"))
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		return true, json.Unmarshal(data, &probe.bridge)
	}); err != nil {
		return err
	}
	if probe.bridge.Name != collaborationTUIHookName || probe.bridge.SHA256 != verifierSHA || probe.bridge.SessionID != panel.rootID || probe.bridge.SessionFile != panel.rootPath || probe.bridge.Process.PID <= 0 {
		return errors.New("hook probe is not bound to the captured native session")
	}
	staged, err := os.Stat(probe.executable)
	if err != nil {
		return err
	}
	processes, err := panel.processes()
	if err != nil {
		return err
	}
	for _, process := range processes {
		if process == probe.bridge.Process && observedAlive(process) {
			current, err := os.Stat(process.Executable)
			if err == nil && os.SameFile(staged, current) {
				return nil
			}
		}
	}
	return errors.New("hook extension is not an observed child of the captured PiG")
}

func (probe *collaborationTUIHookProbe) run(ctx context.Context, panel *panelProof, state *fixtureState, sendText, sendKey func(string) error, visible func() (string, error), frame func(string) (framePair, error), media *collaborationMedia) error {
	if media == nil {
		return errors.New("hook fixture requires live terminal media")
	}
	for _, name := range []string{"allow", "deny"} {
		path := filepath.Join(probe.workspace, "collab-hook-"+name+".txt")
		if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("hook fixture target was not absent: %s", name)
		}
	}
	for _, name := range []string{"allow", "deny"} {
		prompt := "COLLAB_HOOK_" + strings.ToUpper(name)
		if err := panel.awaitRootIdle(prompt); err != nil {
			return err
		}
		if err := sendText(prompt); err != nil {
			return err
		}
		if err := sendKey("Enter"); err != nil {
			return err
		}
		if err := waitFor(ctx, "hook "+name+" caller result", stageTimeout, func() (bool, error) {
			return visibleContains(visible, prompt+"_DONE"), state.providerFailure()
		}); err != nil {
			return err
		}
		if err := panel.awaitRootIdle(prompt + " result"); err != nil {
			return err
		}
		data, err := os.ReadFile(filepath.Join(probe.directory, name+".json"))
		var event collaborationTUIHookEvent
		if err != nil || json.Unmarshal(data, &event) != nil || event.Tool != "write" || event.CallID == "" || event.Path != filepath.Join(probe.workspace, "collab-hook-"+name+".txt") || event.Refused != (name == "deny") || event.SessionID != probe.bridge.SessionID || event.Process != probe.bridge.Process {
			return fmt.Errorf("hook %s event does not match the captured session", name)
		}
		phase := collaborationTUIHookPhase{Name: name, Event: event}
		content, readErr := os.ReadFile(event.Path)
		if readErr == nil {
			phase.FileExists, phase.Content = true, string(content)
		} else if !errors.Is(readErr, os.ErrNotExist) {
			return readErr
		}
		probe.phases = append(probe.phases, phase)
		if name == "allow" {
			if !phase.FileExists || phase.Content != collaborationTUIHookContent {
				return errors.New("allowed hook control did not create its fixture file")
			}
			if err := writePrivate(filepath.Join(probe.directory, "allowed-file.txt"), content); err != nil {
				return err
			}
		} else if phase.FileExists {
			return errors.New("rejecting hook allowed the denied fixture write")
		}
	}
	if probe.phases[0].Event.CallID == probe.phases[1].Event.CallID {
		return errors.New("allowed and denied hook calls reused an identity")
	}
	if _, err := frame("hook-denied"); err != nil {
		return err
	}
	if err := media.markObservation(); err != nil {
		return err
	}
	return collaborationTUIHoldFrame(ctx)
}

func (probe *collaborationTUIHookProbe) save(process observedProcess, runErr error) error {
	var validationErr error
	if runErr == nil && (process.PID <= 0 || probe.bridge.Process.PID <= 0 || len(probe.phases) != 2) {
		validationErr = errors.New("hook probe observation is incomplete")
		runErr = validationErr
	}
	receipt := collaborationTUIHookReceipt{Schema: 1, Status: "observed", PiG: process, Bridge: probe.bridge, Phases: probe.phases}
	if runErr != nil {
		receipt.Status, receipt.Error = "inconclusive", runErr.Error()
	}
	return errors.Join(validationErr, writeCaptureJSON(filepath.Join(probe.directory, "receipt.json"), receipt))
}
