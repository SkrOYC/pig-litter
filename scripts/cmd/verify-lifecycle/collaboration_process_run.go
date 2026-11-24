package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type collaborationProcessEvidence struct {
	Schema            int               `json:"schema"`
	Purpose           string            `json:"purpose"`
	Status            string            `json:"status"`
	StartedUTC        string            `json:"started_utc"`
	ElapsedNS         int64             `json:"elapsed_ns"`
	ProcessDurationNS int64             `json:"process_duration_ns"`
	HostPID           int               `json:"host_pid,omitempty"`
	ParentPID         int               `json:"parent_pid"`
	ExitCode          *int              `json:"exit_code,omitempty"`
	SessionID         string            `json:"session_id,omitempty"`
	SessionFile       string            `json:"session_file,omitempty"`
	Pig               string            `json:"pig"`
	PigSHA256         string            `json:"pig_sha256,omitempty"`
	Extension         string            `json:"extension"`
	ExtensionSHA256   string            `json:"extension_sha256,omitempty"`
	Command           []string          `json:"command,omitempty"`
	Environment       []string          `json:"environment,omitempty"`
	ProviderTools     []string          `json:"provider_tools,omitempty"`
	ProviderRequests  []json.RawMessage `json:"provider_requests,omitempty"`
	Error             string            `json:"error,omitempty"`
}

func collaborationProbeFileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func collaborationRunHostIndependentProcessProbe(ctx context.Context, root, pig, binary string) (evidence collaborationProcessEvidence, resultErr error) {
	started := time.Now()
	evidence = collaborationProcessEvidence{Schema: 1, Purpose: "provisional host research; scripted provider and product parity remain unverified", Status: "inconclusive", StartedUTC: started.UTC().Format(time.RFC3339Nano), ParentPID: os.Getpid(), Pig: pig, Extension: binary}
	if err := collaborationProbePrepare(root); err != nil {
		return evidence, err
	}
	defer func() {
		evidence.ElapsedNS = time.Since(started).Nanoseconds()
		if resultErr != nil {
			evidence.Status = "inconclusive"
			evidence.Error = resultErr.Error()
		}
		resultErr = errors.Join(resultErr, collaborationProbeWriteJSON(filepath.Join(root, "result.json"), evidence))
	}()
	if !filepath.IsAbs(pig) || !filepath.IsAbs(binary) {
		return evidence, errors.New("selected PiG and extension paths must be absolute")
	}
	var err error
	evidence.PigSHA256, err = collaborationProbeFileSHA256(pig)
	if err != nil {
		return evidence, fmt.Errorf("hash selected PiG: %w", err)
	}
	evidence.ExtensionSHA256, err = collaborationProbeFileSHA256(binary)
	if err != nil {
		return evidence, fmt.Errorf("hash selected extension: %w", err)
	}
	fixture, err := collaborationMessageFixtureNew(root, 0)
	if err != nil {
		return evidence, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, fixture.close())
		fixture.mu.Lock()
		evidence.ProviderRequests = append([]json.RawMessage(nil), fixture.requests...)
		fixture.mu.Unlock()
	}()
	workspace := filepath.Join(root, "process-workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		return evidence, err
	}
	if err := os.WriteFile(filepath.Join(workspace, "litter.yaml"), []byte("concurrency: 2\nagents:\n  scout:\n    instructions: Inspect the fixture.\n"), 0o600); err != nil {
		return evidence, err
	}
	const sessionID = "550e8400-e29b-41d4-a716-446655440001"
	tools := []string{"litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}
	arguments := []string{"--mode", "json", "--print", "--no-extensions", "-e", binary, "--no-mcp", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--approve", "--offline", "--model", "fixture/probe", "--tools", strings.Join(tools, ","), "--session-id", sessionID, "--session-dir", filepath.Join(root, "process-sessions"), "Report that the independent fixture is ready."}
	evidence.Command = append([]string{pig}, arguments...)
	command := exec.CommandContext(ctx, pig, arguments...)
	prepareProcess(command)
	command.Cancel = func() error {
		stopProcess(command)
		return nil
	}
	command.WaitDelay = 5 * time.Second
	command.Dir = workspace
	bash, err := exec.LookPath("bash")
	if err != nil {
		return evidence, err
	}
	command.Env = []string{
		"PATH=" + filepath.Dir(pig) + string(os.PathListSeparator) + filepath.Dir(bash),
		"HOME=" + filepath.Join(root, "home"),
		"USERPROFILE=" + filepath.Join(root, "home"),
		"PIG_HOME=" + root,
		"PIG_CODING_AGENT_DIR=" + filepath.Join(root, "agent"),
		"PIG_USE_PI_DIRS=0", "PIG_OFFLINE=1", "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1", "GOENV=off", "GOPROXY=off", "TERM=xterm-256color", "LANG=C.UTF-8",
	}
	evidence.Environment = append([]string(nil), command.Env...)
	stdout, err := os.OpenFile(filepath.Join(root, "process.stdout.jsonl"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return evidence, err
	}
	defer func() { resultErr = errors.Join(resultErr, stdout.Close()) }()
	stderr, err := os.OpenFile(filepath.Join(root, "process.stderr"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return evidence, err
	}
	defer func() { resultErr = errors.Join(resultErr, stderr.Close()) }()
	command.Stdout, command.Stderr = stdout, stderr
	processStart := time.Now()
	if err := command.Start(); err != nil {
		return evidence, fmt.Errorf("start independent PiG: %w", err)
	}
	evidence.HostPID = command.Process.Pid
	waitErr := command.Wait()
	evidence.ProcessDurationNS = time.Since(processStart).Nanoseconds()
	if command.ProcessState != nil {
		code := command.ProcessState.ExitCode()
		evidence.ExitCode = &code
	}
	if waitErr != nil {
		return evidence, fmt.Errorf("independent PiG exit: %w", waitErr)
	}
	pigAfter, pigErr := collaborationProbeFileSHA256(pig)
	extensionAfter, extensionErr := collaborationProbeFileSHA256(binary)
	if pigErr != nil || extensionErr != nil || pigAfter != evidence.PigSHA256 || extensionAfter != evidence.ExtensionSHA256 {
		return evidence, fmt.Errorf("selected binary changed during the process: PiG=%v extension=%v", pigErr, extensionErr)
	}
	if evidence.HostPID <= 0 || evidence.HostPID == os.Getpid() {
		return evidence, fmt.Errorf("independent process ID = %d, parent = %d", evidence.HostPID, os.Getpid())
	}
	fixture.mu.Lock()
	requests := append([]json.RawMessage(nil), fixture.requests...)
	fixture.mu.Unlock()
	if len(requests) != 1 {
		return evidence, fmt.Errorf("independent process made %d provider requests, want one", len(requests))
	}
	var request struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(requests[0], &request); err != nil {
		return evidence, err
	}
	for _, tool := range request.Tools {
		evidence.ProviderTools = append(evidence.ProviderTools, tool.Function.Name)
	}
	sort.Strings(evidence.ProviderTools)
	sort.Strings(tools)
	if strings.Join(evidence.ProviderTools, ",") != strings.Join(tools, ",") {
		return evidence, fmt.Errorf("selected extension tools = %v, want %v", evidence.ProviderTools, tools)
	}
	files, err := filepath.Glob(filepath.Join(root, "process-sessions", "*.jsonl"))
	if err != nil || len(files) != 1 {
		return evidence, fmt.Errorf("native process session files = %v, %v", files, err)
	}
	evidence.SessionFile = files[0]
	native, err := os.Open(files[0])
	if err != nil {
		return evidence, err
	}
	scanner := bufio.NewScanner(native)
	if !scanner.Scan() {
		_ = native.Close()
		return evidence, fmt.Errorf("native session header absent: %v", scanner.Err())
	}
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	err = json.Unmarshal(scanner.Bytes(), &header)
	closeErr := native.Close()
	evidence.SessionID = header.ID
	if err != nil || closeErr != nil || header.Type != "session" || header.ID != sessionID {
		return evidence, fmt.Errorf("native session header = %#v, decode=%v close=%v", header, err, closeErr)
	}
	evidence.Status = "observed"
	return evidence, nil
}
