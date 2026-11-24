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
)

type collaborationMessageProcess struct {
	PID        int    `json:"pid"`
	StartTicks string `json:"startTicks"`
	Executable string `json:"executable"`
}

type collaborationMessageDisk struct {
	Exists bool   `json:"exists"`
	Copies int    `json:"copies"`
	Path   string `json:"snapshot"`
}

type collaborationMessageState struct {
	SessionID    string `json:"session_id"`
	SessionFile  string `json:"session_file"`
	Idle         bool   `json:"idle"`
	MemoryCopies int    `json:"memory_copies"`
	Error        string `json:"error"`
}

type collaborationCLIMessagePhase struct {
	Name     string `json:"name"`
	Response struct {
		Process    collaborationMessageProcess `json:"process"`
		Operation  string                      `json:"operation"`
		ID         string                      `json:"id"`
		Before     collaborationMessageState   `json:"before"`
		After      collaborationMessageState   `json:"after"`
		StartedNS  int64                       `json:"started_ns"`
		ReturnedNS int64                       `json:"returned_ns"`
		CallError  string                      `json:"call_error"`
	} `json:"response"`
	Before         collaborationMessageDisk `json:"disk_before"`
	After          collaborationMessageDisk `json:"disk_after"`
	ProviderBefore int                      `json:"provider_before"`
	ProviderAfter  int                      `json:"provider_after"`
	Error          string                   `json:"error"`
}

type collaborationMessageBridge struct {
	Name        string                      `json:"name"`
	SHA256      string                      `json:"sha256"`
	Socket      string                      `json:"socket"`
	SessionID   string                      `json:"session_id"`
	SessionFile string                      `json:"session_file"`
	Process     collaborationMessageProcess `json:"process"`
}

type collaborationMessageReceipt struct {
	Schema         int                            `json:"schema"`
	Mode           string                         `json:"mode"`
	Status         string                         `json:"status"`
	Bridge         collaborationMessageBridge     `json:"bridge"`
	PiG            collaborationMessageProcess    `json:"pig"`
	HoldObservedNS int64                          `json:"hold_observed_ns"`
	ReleaseStartNS int64                          `json:"release_start_ns"`
	HoldReleasedNS int64                          `json:"hold_released_ns"`
	SettledNS      int64                          `json:"settled_ns"`
	Phases         []collaborationCLIMessagePhase `json:"phases"`
	Error          string                         `json:"error"`
}

func (runner *collaborationCLIRunner) messageTerminal(ctx context.Context, target collaborationCLITarget) error {
	if err := runner.terminal(ctx, "message-terminal", target, "direct", runner.options.scenario); err != nil {
		return err
	}
	evidence := runner.drivers[len(runner.drivers)-1].Evidence
	if err := runner.validateTUIMessageEvidence(evidence, runner.options.scenario); err != nil {
		return err
	}
	runner.attempt.Observation = "The SDK call, native transcript snapshots, provider counts, and terminal media belong to the same PiG session. The SDK return is not a durable acknowledgement."
	return nil
}

func (runner *collaborationCLIRunner) validateTUIMessageEvidence(evidence, mode string) error {
	data, err := runner.messageReadSelected(filepath.Join(evidence, "message-probe", "receipt.json"), false)
	var receipt collaborationMessageReceipt
	if err != nil || json.Unmarshal(data, &receipt) != nil || receipt.Schema != 1 || receipt.Mode != mode || receipt.Status != "observed" || receipt.Error != "" {
		return errors.New("TUI message receipt is incomplete")
	}
	var result struct {
		OK             bool   `json:"ok"`
		VerifierSHA256 string `json:"verifierSha256"`
		Startup        struct {
			Process collaborationMessageProcess `json:"process"`
		} `json:"startup"`
		Fixture struct {
			RootSessions []struct {
				ID string `json:"id"`
			} `json:"root_sessions"`
		} `json:"fixture"`
		Cleanup struct {
			ObservedProcesses []collaborationMessageProcess `json:"observedProcesses"`
		} `json:"cleanup"`
	}
	resultData, err := runner.messageReadSelected(filepath.Join(evidence, "result.json"), true)
	if err != nil || json.Unmarshal(resultData, &result) != nil || !result.OK || result.Startup.Process != receipt.PiG || receipt.PiG.PID <= 0 {
		return errors.New("TUI message receipt does not identify the captured PiG")
	}
	if receipt.Bridge.Process.PID <= 0 || receipt.Bridge.Process.PID == receipt.PiG.PID || receipt.Bridge.Name == "" || filepath.Base(receipt.Bridge.Process.Executable) != receipt.Bridge.Name || receipt.Bridge.SHA256 != result.VerifierSHA256 || receipt.Bridge.SessionID == "" || len(result.Fixture.RootSessions) != 1 || result.Fixture.RootSessions[0].ID != receipt.Bridge.SessionID {
		return errors.New("TUI message receipt does not identify the captured PiG and native session")
	}
	bridgeData, err := runner.messageReadSelected(filepath.Join(evidence, "message-probe", "bridge.json"), false)
	var bridge collaborationMessageBridge
	if err != nil || json.Unmarshal(bridgeData, &bridge) != nil || bridge != receipt.Bridge {
		return errors.New("TUI message bridge differs from its retained receipt")
	}
	owned := false
	for _, process := range result.Cleanup.ObservedProcesses {
		owned = owned || process == receipt.Bridge.Process
	}
	if !owned {
		return errors.New("TUI message extension was not recorded as an owned process")
	}
	for _, phase := range receipt.Phases {
		response := phase.Response
		if phase.Error != "" || response.Process != receipt.Bridge.Process || response.Before.SessionID != receipt.Bridge.SessionID || response.After.SessionID != receipt.Bridge.SessionID || response.Before.SessionFile != receipt.Bridge.SessionFile || response.After.SessionFile != receipt.Bridge.SessionFile {
			return errors.New("TUI message phase does not match the captured session")
		}
		if response.Before.Error != "" || response.After.Error != "" || response.CallError != "" || (response.Operation == "send" && (response.StartedNS <= 0 || response.ReturnedNS < response.StartedNS)) {
			return errors.New("TUI message phase has an error or invalid call timing")
		}
		for _, snapshot := range []collaborationMessageDisk{phase.Before, phase.After} {
			if err := runner.messageSnapshot(evidence, phase.Response.ID, snapshot); err != nil {
				return err
			}
		}
	}
	if err := collaborationValidateTUIMessagePhases(receipt); err != nil {
		return err
	}
	if err := runner.messageProviderRequests(evidence, receipt.Mode); err != nil {
		return err
	}
	var fixture collaborationCLIFixtureResult
	if err := json.Unmarshal(resultData, &fixture); err != nil {
		return err
	}
	if err := runner.validateFixtureSessions(evidence, fixture); err != nil {
		return err
	}
	return nil
}

func (runner *collaborationCLIRunner) messageReadSelected(path string, selected bool) ([]byte, error) {
	var artifact collaborationArtifact
	if selected {
		relative, err := filepath.Rel(runner.directory, path)
		if err != nil || !filepath.IsLocal(relative) {
			return nil, errors.New("selected message artifact escapes its attempt")
		}
		for _, candidate := range runner.artifacts {
			if candidate.Path == relative {
				if artifact.Path != "" {
					return nil, errors.New("message artifact is selected more than once")
				}
				artifact = candidate
			}
		}
		if artifact.Path == "" {
			return nil, errors.New("message artifact was not selected")
		}
	} else {
		var err error
		artifact, err = runner.artifact(path, "log")
		if err != nil {
			return nil, err
		}
	}
	root, err := os.OpenRoot(runner.directory)
	if err != nil {
		return nil, err
	}
	file, err := collaborationOpenFile(root, artifact.Path)
	if err != nil {
		_ = root.Close()
		return nil, err
	}
	data, readErr := collaborationReadArtifact(file, collaborationMaxJSONBytes, artifact.SHA256)
	closeErr := errors.Join(file.Close(), root.Close())
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, err
	}
	if !selected {
		runner.artifacts = append(runner.artifacts, artifact)
	}
	return data, nil
}

func (runner *collaborationCLIRunner) messageProviderRequests(evidence, mode string) error {
	paths, err := filepath.Glob(filepath.Join(evidence, "provider-requests", "request-*.json"))
	want := 1
	if mode == "host-busy-message" {
		want = 2
	}
	if err != nil || len(paths) != want {
		return fmt.Errorf("retained provider requests = %d, want %d: %v", len(paths), want, err)
	}
	for index, path := range paths {
		data, err := runner.messageReadSelected(path, false)
		if err != nil {
			return err
		}
		if !json.Valid(data) {
			return errors.New("provider request trace is not JSON")
		}
		if mode == "host-idle-message" && (!bytes.Contains(data, []byte("peer message P02_FRESH")) || bytes.Contains(data, []byte("peer message P02_IDLE"))) {
			return errors.New("idle provider request does not match the observed message boundary")
		}
		if mode == "host-busy-message" && (bytes.Contains(data, []byte("peer message P02_BUSY")) || (index == 1 && !bytes.Contains(data, []byte("P02_BUSY_HOLD")))) {
			return errors.New("provider request trace does not match the observed message boundary")
		}
	}
	return nil
}

func (runner *collaborationCLIRunner) messageSnapshot(evidence, id string, snapshot collaborationMessageDisk) error {
	if !snapshot.Exists {
		if snapshot.Copies != 0 || snapshot.Path != "" {
			return errors.New("absent native snapshot has content")
		}
		return nil
	}
	if !filepath.IsLocal(snapshot.Path) || filepath.Base(snapshot.Path) != snapshot.Path {
		return errors.New("native snapshot path is invalid")
	}
	path := filepath.Join(evidence, "message-probe", snapshot.Path)
	data, err := runner.messageReadSelected(path, false)
	if err != nil {
		return err
	}
	count := 0
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), collaborationMaxJSONBytes)
	for scanner.Scan() {
		var entry struct {
			Type       string `json:"type"`
			CustomType string `json:"customType"`
			Details    struct {
				ID string `json:"message_id"`
			} `json:"details"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return err
		}
		if entry.Type == "custom_message" && entry.CustomType == "p02_tui_probe" && entry.Details.ID == id {
			count++
		}
	}
	if err := scanner.Err(); err != nil || count != snapshot.Copies {
		return fmt.Errorf("native message snapshot count = %d, want %d: %v", count, snapshot.Copies, err)
	}
	return nil
}

func collaborationValidateTUIMessagePhases(receipt collaborationMessageReceipt) error {
	if receipt.Mode == "host-idle-message" && len(receipt.Phases) == 3 {
		fresh, flushed, idle := receipt.Phases[0], receipt.Phases[1], receipt.Phases[2]
		if !collaborationMessagePhaseIs(fresh, "fresh-idle", "send", "P02_FRESH", 0) || !fresh.Response.Before.Idle || !fresh.Response.After.Idle || fresh.Response.Before.MemoryCopies != 0 || fresh.Response.After.MemoryCopies != 1 || fresh.Before.Copies != 0 {
			return errors.New("fresh idle message observation is incomplete")
		}
		if !collaborationMessagePhaseIs(flushed, "fresh-flushed", "inspect", "P02_FRESH", 1) || !flushed.Response.After.Idle || flushed.Response.After.MemoryCopies != 1 || flushed.After.Copies != 1 {
			return errors.New("fresh message flush observation is incomplete")
		}
		if !collaborationMessagePhaseIs(idle, "established-idle", "send", "P02_IDLE", 1) || !idle.Response.Before.Idle || !idle.Response.After.Idle || idle.Response.Before.MemoryCopies != 0 || idle.Response.After.MemoryCopies != 1 || idle.Before.Copies != 0 || idle.After.Copies != 1 {
			return errors.New("established idle message observation is incomplete")
		}
		return nil
	}
	if receipt.Mode == "host-busy-message" && len(receipt.Phases) == 2 {
		busy, settled := receipt.Phases[0], receipt.Phases[1]
		if receipt.HoldObservedNS <= 0 || receipt.HoldObservedNS > busy.Response.StartedNS || busy.Response.ReturnedNS > receipt.ReleaseStartNS || receipt.ReleaseStartNS > receipt.HoldReleasedNS || receipt.HoldReleasedNS > receipt.SettledNS {
			return errors.New("busy message timing boundaries are inconsistent")
		}
		if !collaborationMessagePhaseIs(busy, "busy-return", "send", "P02_BUSY", 2) || busy.Response.Before.Idle || busy.Response.After.Idle || busy.Response.Before.MemoryCopies != 0 || busy.Before.Copies != 0 || busy.Response.After.MemoryCopies < 0 || busy.Response.After.MemoryCopies > 1 || busy.After.Copies < 0 || busy.After.Copies > 1 {
			return errors.New("held busy message observation is incomplete")
		}
		if !collaborationMessagePhaseIs(settled, "busy-settled", "inspect", "P02_BUSY", 2) || !settled.Response.After.Idle || settled.Response.After.MemoryCopies != 1 || settled.After.Copies != 1 {
			return errors.New("settled busy message observation is incomplete")
		}
		return nil
	}
	return errors.New("TUI message observations do not establish the requested delivery boundary")
}

func collaborationMessagePhaseIs(phase collaborationCLIMessagePhase, name, operation, id string, requests int) bool {
	return phase.Name == name && phase.Response.Operation == operation && phase.Response.ID == id && phase.ProviderBefore == requests && phase.ProviderAfter == requests
}
