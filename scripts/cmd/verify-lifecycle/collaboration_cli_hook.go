package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type collaborationCLIHookBridge struct {
	Name        string                      `json:"name"`
	SHA256      string                      `json:"sha256"`
	SessionID   string                      `json:"session_id"`
	SessionFile string                      `json:"session_file"`
	Process     collaborationMessageProcess `json:"process"`
}

type collaborationCLIHookEvent struct {
	Tool      string                      `json:"tool"`
	CallID    string                      `json:"call_id"`
	Path      string                      `json:"path"`
	Refused   bool                        `json:"refused"`
	SessionID string                      `json:"session_id"`
	Process   collaborationMessageProcess `json:"process"`
}

type collaborationCLIHookPhase struct {
	Name       string                    `json:"name"`
	Event      collaborationCLIHookEvent `json:"event"`
	FileExists bool                      `json:"file_exists"`
	Content    string                    `json:"content,omitempty"`
}

type collaborationCLIHookReceipt struct {
	Schema int                         `json:"schema"`
	Status string                      `json:"status"`
	Error  string                      `json:"error"`
	PiG    collaborationMessageProcess `json:"pig"`
	Bridge collaborationCLIHookBridge  `json:"bridge"`
	Phases []collaborationCLIHookPhase `json:"phases"`
}

func (runner *collaborationCLIRunner) hookTerminal(ctx context.Context, target collaborationCLITarget, probeRoot string) error {
	data, err := os.ReadFile(filepath.Join(probeRoot, "result.json"))
	var probe collaborationHookEvidence
	if err != nil || json.Unmarshal(data, &probe) != nil || probe.Status != "observed" || len(probe.Phases) != 8 {
		return errors.New("in-process hook probe is incomplete")
	}
	for index, name := range []string{"direct-write-allow", "direct-write-deny", "codemode-write-allow", "codemode-write-deny", "direct-mcp-allow", "direct-mcp-deny", "codemode-mcp-allow", "codemode-mcp-deny"} {
		phase := probe.Phases[index]
		collaborationAssessHookPhase(&phase)
		if phase.Name != name || phase.Verdict != "observed" {
			return fmt.Errorf("in-process hook phase %s is incomplete", name)
		}
	}
	if err := runner.terminal(ctx, "hook-terminal", target, "direct", "host-tool-veto"); err != nil {
		return err
	}
	evidence := runner.drivers[len(runner.drivers)-1].Evidence
	if err := runner.validateTUIHookEvidence(evidence); err != nil {
		return err
	}
	runner.attempt.Observation = "The in-process direct and nested hook probes and a separate captured PiG session observed allowed controls and denied writes. The terminal session exercises the subprocess SDK hook."
	return nil
}

func (runner *collaborationCLIRunner) validateTUIHookEvidence(evidence string) error {
	read := func(name string, selected bool) ([]byte, error) {
		return runner.messageReadSelected(filepath.Join(evidence, name), selected)
	}
	data, err := read("hook-probe/receipt.json", false)
	var receipt collaborationCLIHookReceipt
	if err != nil || json.Unmarshal(data, &receipt) != nil || receipt.Schema != 1 || receipt.Status != "observed" || receipt.Error != "" || len(receipt.Phases) != 2 {
		return errors.New("captured hook receipt is incomplete")
	}
	data, err = read("result.json", true)
	resultData := data
	var result struct {
		OK             bool   `json:"ok"`
		VerifierSHA256 string `json:"verifierSha256"`
		Startup        struct {
			Process collaborationMessageProcess `json:"process"`
		} `json:"startup"`
		Fixture struct {
			Workspace struct {
				Path string `json:"path"`
			} `json:"workspace"`
			RootSessions []struct {
				ID   string `json:"id"`
				File struct {
					Path string `json:"path"`
				} `json:"file"`
			} `json:"root_sessions"`
		} `json:"fixture"`
		Cleanup struct {
			ObservedProcesses []collaborationMessageProcess `json:"observedProcesses"`
		} `json:"cleanup"`
	}
	if err != nil || json.Unmarshal(data, &result) != nil || !result.OK || result.Startup.Process != receipt.PiG || receipt.PiG.PID <= 0 {
		return errors.New("hook receipt does not identify the captured PiG")
	}
	bridge := receipt.Bridge
	if bridge.Name != "p02-hook-probe" || bridge.SHA256 != result.VerifierSHA256 || bridge.Process.PID <= 0 || bridge.Process.PID == receipt.PiG.PID || filepath.Base(bridge.Process.Executable) != bridge.Name || len(result.Fixture.RootSessions) != 1 || bridge.SessionID != result.Fixture.RootSessions[0].ID || bridge.SessionFile != result.Fixture.RootSessions[0].File.Path {
		return errors.New("hook extension does not identify the captured native session")
	}
	owned := false
	for _, process := range result.Cleanup.ObservedProcesses {
		owned = owned || process == bridge.Process
	}
	if !owned {
		return errors.New("hook extension was not recorded as an owned process")
	}
	data, err = read("hook-probe/bridge.json", false)
	var retainedBridge collaborationCLIHookBridge
	if err != nil || json.Unmarshal(data, &retainedBridge) != nil || retainedBridge != bridge {
		return errors.New("hook bridge differs from its retained receipt")
	}
	for index, name := range []string{"allow", "deny"} {
		phase := receipt.Phases[index]
		event := phase.Event
		if phase.Name != name || event.Tool != "write" || event.CallID == "" || event.Path != filepath.Join(result.Fixture.Workspace.Path, "p02-hook-"+name+".txt") || event.Refused != (name == "deny") || event.SessionID != bridge.SessionID || event.Process != bridge.Process || phase.FileExists != (name == "allow") {
			return fmt.Errorf("captured hook %s phase is incomplete", name)
		}
		if (name == "allow" && phase.Content != collaborationHookMarker) || (name == "deny" && phase.Content != "") {
			return fmt.Errorf("captured hook %s file observation differs", name)
		}
		data, err := read("hook-probe/"+name+".json", false)
		var retained collaborationCLIHookEvent
		if err != nil || json.Unmarshal(data, &retained) != nil || retained != event {
			return fmt.Errorf("captured hook %s event differs from its receipt", name)
		}
	}
	if receipt.Phases[0].Event.CallID == receipt.Phases[1].Event.CallID {
		return errors.New("captured hook calls reused an identity")
	}
	data, err = read("hook-probe/allowed-file.txt", false)
	if err != nil || string(data) != collaborationHookMarker {
		return errors.New("captured allowed file differs from its receipt")
	}
	data, err = read("hook-denied-ansi.txt", false)
	if err != nil || !bytes.Contains(data, []byte("P02_HOOK_DENY_DONE")) {
		return errors.New("captured hook frame does not show the denied call's completed turn")
	}
	if err := runner.validateHookRequests(evidence, receipt.Phases); err != nil {
		return err
	}
	var fixture collaborationCLIFixtureResult
	if err := json.Unmarshal(resultData, &fixture); err != nil {
		return err
	}
	if err := runner.validateFixtureSessions(evidence, fixture); err != nil {
		return err
	}
	data, err = read(filepath.Join("native-final", bridge.SessionID+".jsonl"), true)
	if err != nil {
		return err
	}
	return collaborationValidateHookNative(data, receipt.Phases)
}

func (runner *collaborationCLIRunner) validateHookRequests(evidence string, phases []collaborationCLIHookPhase) error {
	paths, err := filepath.Glob(filepath.Join(evidence, "provider-requests", "request-*.json"))
	if err != nil || len(paths) == 0 {
		return errors.New("captured hook provider requests are absent")
	}
	found := [2]bool{}
	for _, path := range paths {
		data, err := runner.messageReadSelected(path, false)
		var request struct {
			Messages []struct {
				Role       string          `json:"role"`
				ToolCallID string          `json:"tool_call_id"`
				Content    json.RawMessage `json:"content"`
			} `json:"messages"`
		}
		if err != nil || json.Unmarshal(data, &request) != nil {
			return errors.New("captured hook provider request is invalid")
		}
		for _, message := range request.Messages {
			for index, phase := range phases {
				if message.Role != "tool" || message.ToolCallID != phase.Event.CallID {
					continue
				}
				want := "Successfully wrote"
				if index == 1 {
					want = collaborationHookRefusal
				}
				if !bytes.Contains(message.Content, []byte(want)) {
					return fmt.Errorf("captured hook %s caller result differs", phase.Name)
				}
				found[index] = true
			}
		}
	}
	if found != [2]bool{true, true} {
		return errors.New("captured hook caller results are missing")
	}
	return nil
}

func collaborationValidateHookNative(data []byte, phases []collaborationCLIHookPhase) error {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), 8<<20)
	found := [2]int{}
	for scanner.Scan() {
		var entry struct {
			Type    string `json:"type"`
			Message struct {
				Role       string          `json:"role"`
				ToolCallID string          `json:"toolCallId"`
				ToolName   string          `json:"toolName"`
				IsError    bool            `json:"isError"`
				Content    json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return err
		}
		for index, phase := range phases {
			message := entry.Message
			if entry.Type != "message" || message.Role != "toolResult" || message.ToolCallID != phase.Event.CallID {
				continue
			}
			want := "Successfully wrote"
			if index == 1 {
				want = collaborationHookRefusal
			}
			if message.ToolName != "write" || message.IsError != (index == 1) || !strings.Contains(string(message.Content), want) {
				return fmt.Errorf("native hook %s result differs", phase.Name)
			}
			found[index]++
		}
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	if found != [2]int{1, 1} {
		return errors.New("native hook call results are absent or duplicated")
	}
	return nil
}
