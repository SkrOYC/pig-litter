package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

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

func TestCollaborationHostIndependentSelectedProcess(t *testing.T) {
	fixture := collaborationMessageFixtureNew(t, 0)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pig, err := exec.LookPath("pig")
	if err != nil {
		t.Fatalf("resolve the actual PiG executable: %v", err)
	}
	pig, err = filepath.Abs(pig)
	if err != nil {
		t.Fatal(err)
	}
	binary := os.Getenv("P02_PROCESS_EXTENSION")
	if binary == "" {
		binary = filepath.Join(fixture.root, "pig-litter")
		build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", binary, "./cmd/pig-litter")
		build.Dir = filepath.Clean(filepath.Join("..", ".."))
		output, err := build.CombinedOutput()
		if writeErr := os.WriteFile(filepath.Join(fixture.root, "extension-build.log"), output, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		if err != nil {
			t.Fatalf("build selected extension: %v: %s", err, output)
		}
	}
	if !filepath.IsAbs(binary) {
		t.Fatal("P02_PROCESS_EXTENSION must be an absolute path")
	}
	pigHash, err := collaborationProbeFileSHA256(pig)
	if err != nil {
		t.Fatal(err)
	}
	extensionHash, err := collaborationProbeFileSHA256(binary)
	if err != nil {
		t.Fatal(err)
	}
	workspace := filepath.Join(fixture.root, "process-workspace")
	if err := os.MkdirAll(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, "litter.yaml"), []byte("concurrency: 2\nagents:\n  scout:\n    instructions: Inspect the fixture.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	const sessionID = "550e8400-e29b-41d4-a716-446655440001"
	tools := []string{"litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}
	arguments := []string{"--mode", "json", "--print", "--no-extensions", "-e", binary, "--no-mcp", "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--approve", "--offline", "--model", "fixture/probe", "--tools", strings.Join(tools, ","), "--session-id", sessionID, "--session-dir", filepath.Join(fixture.root, "process-sessions"), "Report that the independent fixture is ready."}
	command := exec.CommandContext(ctx, pig, arguments...)
	command.Dir = workspace
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Fatal(err)
	}
	command.Env = []string{
		"PATH=" + filepath.Dir(pig) + string(os.PathListSeparator) + filepath.Dir(bash),
		"HOME=" + filepath.Join(fixture.root, "home"),
		"USERPROFILE=" + filepath.Join(fixture.root, "home"),
		"PIG_HOME=" + fixture.root,
		"PIG_CODING_AGENT_DIR=" + filepath.Join(fixture.root, "agent"),
		"PIG_USE_PI_DIRS=0", "PIG_OFFLINE=1", "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1", "GOENV=off", "GOPROXY=off", "TERM=xterm-256color", "LANG=C.UTF-8",
	}
	stdout, err := os.Create(filepath.Join(fixture.root, "process.stdout.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(fixture.root, "process.stderr"))
	if err != nil {
		t.Fatal(err)
	}
	defer stderr.Close()
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start independent PiG: %v", err)
	}
	processID := command.Process.Pid
	start := time.Now()
	err = command.Wait()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("independent PiG exit: %v (see %s)", err, filepath.Join(fixture.root, "process.stderr"))
	}
	if processID <= 0 || processID == os.Getpid() {
		t.Fatalf("independent process ID = %d, parent = %d", processID, os.Getpid())
	}
	fixture.mu.Lock()
	requests := append([]json.RawMessage(nil), fixture.requests...)
	fixture.mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("independent process made %d provider requests, want one", len(requests))
	}
	var request struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(requests[0], &request); err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range request.Tools {
		got = append(got, tool.Function.Name)
	}
	sort.Strings(got)
	sort.Strings(tools)
	if strings.Join(got, ",") != strings.Join(tools, ",") {
		t.Fatalf("selected extension tools = %v, want %v", got, tools)
	}
	files, err := filepath.Glob(filepath.Join(fixture.root, "process-sessions", "*.jsonl"))
	if err != nil || len(files) != 1 {
		t.Fatalf("native process session files = %v, %v", files, err)
	}
	native, err := os.Open(files[0])
	if err != nil {
		t.Fatal(err)
	}
	scanner := bufio.NewScanner(native)
	if !scanner.Scan() {
		_ = native.Close()
		t.Fatalf("native session header absent: %v", scanner.Err())
	}
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	err = json.Unmarshal(scanner.Bytes(), &header)
	_ = native.Close()
	if err != nil || header.Type != "session" || header.ID != sessionID {
		t.Fatalf("native session header = %#v, %v", header, err)
	}
	metadata := map[string]any{"host_pid": processID, "parent_pid": os.Getpid(), "session_id": header.ID, "session_file": files[0], "pig": pig, "pig_sha256": pigHash, "extension": binary, "extension_sha256": extensionHash, "args": arguments, "provider_tools": got, "provider_requests": len(requests), "duration_ns": elapsed.Nanoseconds(), "exit_code": command.ProcessState.ExitCode()}
	data, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fixture.root, "independent-process.json"), append(data, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("independent PiG pid=%d session=%s selected tools=%v", processID, sessionID, got)
}
