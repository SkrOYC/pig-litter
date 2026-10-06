package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (f *fixture) parentLifecycle(ctx context.Context, body modelRequest) (string, []call, error) {
	tools, err := toolResults(body)
	if err != nil {
		return "", nil, err
	}
	user := ""
	for _, message := range body.Messages {
		if message.Role == "user" {
			user += content(message.Content) + "\n"
		}
	}
	if strings.Contains(user, "SPAWN_EXIT") {
		if f.stage == 2 {
			f.stage = 3
			return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "exit-held", "name": "exit-held", "model": "fixture/lifecycle_child"}}}, nil
		}
		if f.stage != 3 || len(tools) != 3 || tools[2].Child.ID == "" {
			return "", nil, errors.New("replacement owner did not admit its exit child")
		}
		for _, old := range f.runs {
			if tools[2].Child.ID == old.ID {
				return "", nil, errors.New("new owner reused a previous child ID")
			}
		}
		if err := f.wait(ctx, func() bool { return f.held["exit-held"] != nil }); err != nil {
			return "", nil, err
		}
		return "EXIT_READY", nil, nil
	}
	if strings.Contains(user, "AFTER_REPLACEMENT") {
		if f.stage == 0 {
			f.stage = 1
			return "", []call{{"litter_list", map[string]any{}}, {"litter_inspect", map[string]any{"id": f.runs[0].ID}}}, nil
		}
		if f.stage != 1 || len(tools) != 2 || len(tools[0].Children) != 0 || tools[1].State != "rejected" {
			return "", nil, errors.New("replacement retained old children or retargeted an old ID")
		}
		f.stage = 2
		f.mu.Lock()
		f.checks["replacementCleared"] = true
		f.mu.Unlock()
		return "REPLACEMENT_CLEARED", nil, nil
	}
	if len(tools) == 0 {
		return "", []call{
			{"litter_spawn", map[string]any{"type": "scout", "task": "replace-A", "name": "replace-A", "model": "fixture/lifecycle_child"}},
			{"litter_spawn", map[string]any{"type": "scout", "task": "replace-B", "name": "replace-B", "model": "fixture/lifecycle_child"}},
		}, nil
	}
	if len(tools) != 2 || tools[0].Child.ID == "" || tools[1].Child.ID == "" {
		return "", nil, errors.New("replacement fixture did not admit two native children")
	}
	f.runs = []ref{tools[0].Child.ref, tools[1].Child.ref}
	if err := f.wait(ctx, func() bool { return f.held["replace-A"] != nil && f.held["replace-B"] != nil }); err != nil {
		return "", nil, err
	}
	return "REPLACEMENT_READY", nil, nil
}

func (f *fixture) lifecycleChild(ctx context.Context, body modelRequest) (string, []call, error) {
	user := ""
	for _, message := range body.Messages {
		if message.Role == "user" {
			user += content(message.Content)
		}
	}
	for _, task := range []string{"replace-A", "replace-B", "exit-held"} {
		if strings.Contains(user, task) {
			return f.hold(ctx, task), nil, nil
		}
	}
	return "", nil, errors.New("unexpected lifecycle child task")
}

func fixtureEnvironment(pig, directory, agentDir string) []string {
	return []string{"PATH=" + filepath.Dir(pig), "HOME=" + filepath.Join(directory, "home"), "PIG_HOME=" + directory, "PIG_CODING_AGENT_DIR=" + agentDir, "PIG_USE_PI_DIRS=0", "PIG_OFFLINE=1", "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1", "GOPROXY=off", "GOENV=off", "TERM=xterm-256color", "LANG=C.UTF-8", "LITTER_FIXTURE_EVIDENCE=" + directory}
}

func (f *fixture) runLifecycle(ctx context.Context, directory, workspace, agentDir, pig, extension string, stdout, stderr *os.File) error {
	model := "fixture/parent_lifecycle"
	selectedExtension := extension
	if f.scenario == "completion" {
		model = "fixture/parent_completion"
		self, err := os.Executable()
		if err != nil {
			return err
		}
		source, err := os.Open(self)
		if err != nil {
			return err
		}
		defer source.Close()
		proxyDir := filepath.Join(directory, "proxy")
		if err := os.MkdirAll(proxyDir, 0o700); err != nil {
			return err
		}
		selectedExtension = filepath.Join(proxyDir, "pig-litter")
		copy, err := os.OpenFile(selectedExtension, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
		if err != nil {
			return err
		}
		_, copyErr := io.Copy(copy, source)
		if err := errors.Join(copyErr, copy.Close()); err != nil {
			return err
		}
	}
	command := exec.CommandContext(ctx, pig, "--no-extensions", "-e", selectedExtension, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-session", "--approve", "--offline", "--model", model, "--mode", "rpc")
	prepareProcess(command)
	command.Dir, command.Env, command.Stderr = workspace, fixtureEnvironment(pig, directory, agentDir), stderr
	if f.scenario == "completion" {
		command.Env = append(command.Env, "LITTER_FIXTURE_PROXY_TARGET="+extension)
	}
	stdin, err := command.StdinPipe()
	if err != nil {
		return err
	}
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	if err := command.Start(); err != nil {
		return err
	}
	events := make(chan map[string]any, 256)
	readDone := make(chan error, 1)
	go func() {
		defer close(events)
		scanner := bufio.NewScanner(output)
		scanner.Buffer(make([]byte, 65536), 8<<20)
		for scanner.Scan() {
			line := append([]byte(nil), scanner.Bytes()...)
			if _, err := stdout.Write(append(line, '\n')); err != nil {
				readDone <- err
				return
			}
			var event map[string]any
			if err := json.Unmarshal(line, &event); err != nil {
				readDone <- err
				return
			}
			select {
			case events <- event:
			case <-ctx.Done():
				readDone <- ctx.Err()
				return
			}
		}
		readDone <- scanner.Err()
	}()
	defer func() {
		_ = stdin.Close()
		if command.ProcessState == nil {
			stopProcess(command)
			_ = command.Wait()
		}
	}()
	send := func(value any) error { return json.NewEncoder(stdin).Encode(value) }
	wait := func(marker, responseID string) error {
		seen := false
		for {
			select {
			case event, open := <-events:
				if !open {
					return fmt.Errorf("host exited before %s%s", marker, responseID)
				}
				if event["type"] == "response" && event["success"] == false {
					return fmt.Errorf("host rejected command: %v", event)
				}
				if responseID != "" && event["type"] == "response" && event["id"] == responseID {
					return nil
				}
				if event["type"] == "message_end" {
					raw, _ := json.Marshal(event["message"])
					seen = seen || strings.Contains(string(raw), marker)
				}
				if marker != "" && seen && event["type"] == "agent_end" {
					return nil
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if f.scenario == "completion" {
		if err := f.runCompletion(ctx, directory, send, wait, events); err != nil {
			return err
		}
	} else {
		if err := send(map[string]any{"id": "spawn", "type": "prompt", "message": "SPAWN_REPLACEMENT. PARENT_PRIVATE_MARKER"}); err != nil {
			return err
		}
		if err := wait("REPLACEMENT_READY", ""); err != nil {
			return err
		}
		if err := send(map[string]any{"id": "replace", "type": "new_session"}); err != nil {
			return err
		}
		if err := wait("", "replace"); err != nil {
			return err
		}
		if err := f.wait(ctx, func() bool { return f.aborted["replace-A"] && f.aborted["replace-B"] }); err != nil {
			return fmt.Errorf("replacement did not cancel both actual provider streams: %w", err)
		}
		f.mu.Lock()
		f.checks["replacementCancelledStreams"] = true
		f.mu.Unlock()
		if err := send(map[string]any{"id": "after", "type": "prompt", "message": "AFTER_REPLACEMENT"}); err != nil {
			return err
		}
		if err := wait("REPLACEMENT_CLEARED", ""); err != nil {
			return err
		}
		if err := send(map[string]any{"id": "exit", "type": "prompt", "message": "SPAWN_EXIT"}); err != nil {
			return err
		}
		if err := wait("EXIT_READY", ""); err != nil {
			return err
		}
	}
	if err := stdin.Close(); err != nil {
		return err
	}
	select {
	case err := <-readDone:
		if err != nil {
			return err
		}
	case <-ctx.Done():
		return ctx.Err()
	}
	if err := command.Wait(); err != nil {
		return err
	}
	if f.scenario == "lifecycle" {
		if err := f.wait(ctx, func() bool { return f.aborted["exit-held"] && len(f.held) == 0 }); err != nil {
			return fmt.Errorf("owner exit did not drain its live native child: %w", err)
		}
		f.mu.Lock()
		f.checks["ownerExitCancelledStream"] = true
		f.mu.Unlock()
	}
	f.mu.Lock()
	checks := f.checks
	failure := f.failure
	f.mu.Unlock()
	if failure != nil {
		return failure
	}
	return writeJSON(filepath.Join(directory, "result.json"), map[string]any{"passed": true, "scenario": f.scenario, "checks": checks, "retiredRuns": f.runs, "exitCode": command.ProcessState.ExitCode(), "isolatedEnvironment": true})
}
