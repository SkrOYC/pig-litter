package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

type boundaryGateConnection struct {
	conn net.Conn
	err  error
}

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
		if f.stage != 1 || len(tools) != 2 || len(tools[0].Children) != 0 || tools[1].State != "unavailable" {
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
	if strings.Contains(user, "RESULT_HOOK_FAILURE_MUST_NOT_RUN") {
		return "", nil, errors.New("rolled-back spawn reached the child provider")
	}
	if strings.Contains(user, "RESULT_HOOK_SUCCESS") {
		return "RESULT_HOOK_SUCCESS", nil, nil
	}
	if strings.Contains(user, "ROOT_PAUSE_RESUME_A") {
		if !strings.Contains(user, "root-pause-A") {
			return "", nil, errors.New("root resumed a child other than root-pause-A")
		}
		return "ROOT_PAUSE_A_RESUMED_RESULT", nil, nil
	}
	for _, task := range []string{"replace-A", "replace-B", "exit-held"} {
		if strings.Contains(user, task) {
			return f.hold(ctx, task), nil, nil
		}
	}
	for _, task := range []string{"root-pause-A", "root-pause-B"} {
		if strings.Contains(user, task) {
			return f.hold(ctx, task), nil, nil
		}
	}
	return "", nil, errors.New("unexpected lifecycle child task")
}

func latestUserText(body modelRequest) string {
	for index := len(body.Messages) - 1; index >= 0; index-- {
		if body.Messages[index].Role == "user" {
			return content(body.Messages[index].Content)
		}
	}
	return ""
}

func trailingToolResults(body modelRequest, count int) ([]result, error) {
	messages := toolMessages(body)
	if len(messages) < count {
		return nil, fmt.Errorf("provider request has %d tool results, want at least %d", len(messages), count)
	}
	results := make([]result, count)
	for index, message := range messages[len(messages)-count:] {
		if err := json.Unmarshal([]byte(content(message.Content)), &results[index]); err != nil {
			return nil, fmt.Errorf("tool result %d is not JSON: %w", index, err)
		}
	}
	return results, nil
}

func (f *fixture) parentPause(ctx context.Context, body modelRequest) (string, []call, error) {
	latest := latestUserText(body)
	runs := f.lifecycleRunRefs()
	switch {
	case f.stage == 0 && latest == "SPAWN_ROOT_PAUSE":
		f.stage = 1
		return "", []call{
			{"litter_spawn", map[string]any{"type": "scout", "task": "root-pause-A", "name": "root-pause-A", "model": "fixture/lifecycle_child"}},
			{"litter_spawn", map[string]any{"type": "scout", "task": "root-pause-B", "name": "root-pause-B", "model": "fixture/lifecycle_child"}},
		}, nil
	case f.stage == 2 && latest == "WAIT_DURING_ROOT_PAUSE":
		if len(runs) != 2 {
			return "", nil, errors.New("root pause fixture did not retain both child IDs")
		}
		f.stage = 3
		return "", []call{{"litter_wait", with(runs[0], "timeoutMs", 30000)}}, nil
	case f.stage == 3 && latest == "AFTER_ROOT_ABORT":
		f.stage = 4
		return "", []call{
			{"litter_inspect", with(runs[0])},
			{"litter_inspect", with(runs[1])},
			{"litter_list", map[string]any{}},
		}, nil
	case f.stage == 5 && latest == "RESUME_ROOT_PAUSE_A":
		f.stage = 6
		return "", []call{{"litter_message", with(runs[0], "text", "ROOT_PAUSE_RESUME_A", "resume", true)}}, nil
	}

	switch f.stage {
	case 1:
		children, err := trailingToolResults(body, 2)
		if err != nil || children[0].Child.ID == "" || children[1].Child.ID == "" || children[0].Child.ID == children[1].Child.ID || children[0].Child.Name != "root-pause-A" || children[1].Child.Name != "root-pause-B" {
			return "", nil, fmt.Errorf("root pause children were not admitted with distinct IDs: %#v, %v", children, err)
		}
		f.mu.Lock()
		f.runs = []ref{children[0].Child.ref, children[1].Child.ref}
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return f.held["root-pause-A"] != nil && f.held["root-pause-B"] != nil }); err != nil {
			return "", nil, fmt.Errorf("root pause children did not enter their provider requests: %w", err)
		}
		f.stage = 2
		return "ROOT_PAUSE_READY", nil, nil
	case 4:
		results, err := trailingToolResults(body, 3)
		if err != nil {
			return "", nil, err
		}
		if err := requirePausedOutcome(results[0], runs[0]); err != nil {
			return "", nil, fmt.Errorf("root abort did not retain the selected child: %w", err)
		}
		if err := requirePausedOutcome(results[1], runs[1]); err != nil {
			return "", nil, fmt.Errorf("root abort did not retain the sibling child: %w", err)
		}
		if len(results[2].Children) != 0 {
			return "", nil, errors.New("paused children remained in the active root list")
		}
		if f.countRequests("lifecycle_child", "root-pause-A") != 1 || f.countRequests("lifecycle_child", "root-pause-B") != 1 {
			return "", nil, errors.New("root continuation resumed a child before an explicit selection")
		}
		f.markCheck("rootAbortPausedChildren")
		f.stage = 5
		return "ROOT_PAUSE_INSPECTED", nil, nil
	case 6:
		resumed, err := lastToolResult(body)
		if err != nil || resumed.Child.ID != runs[0].ID || resumed.Delivery != "resumed" {
			return "", nil, fmt.Errorf("explicit resume changed the selected child identity: %#v, %v", resumed, err)
		}
		f.stage = 7
		return "", []call{{"litter_wait", with(runs[0], "timeoutMs", 10000)}}, nil
	case 7:
		waited, err := lastToolResult(body)
		if err != nil || waited.State != "completed" || waited.Outcome.Run != runs[0] || waited.Outcome.Text != "ROOT_PAUSE_A_RESUMED_RESULT" {
			return "", nil, fmt.Errorf("selected child did not complete under its original ID: %#v, %v", waited, err)
		}
		f.stage = 8
		return "", []call{
			{"litter_inspect", with(runs[0])},
			{"litter_inspect", with(runs[1])},
			{"litter_list", map[string]any{}},
		}, nil
	case 8:
		results, err := trailingToolResults(body, 3)
		if err != nil {
			return "", nil, err
		}
		if results[0].Child.ID != runs[0].ID || results[0].Child.State != "completed" || results[0].Outcome.Run != runs[0] || results[0].Outcome.Text != "ROOT_PAUSE_A_RESUMED_RESULT" {
			return "", nil, fmt.Errorf("resumed child inspection lost its completed outcome: %#v", results[0])
		}
		if err := requirePausedOutcome(results[1], runs[1]); err != nil {
			return "", nil, fmt.Errorf("unselected sibling did not remain paused: %w", err)
		}
		if len(results[2].Children) != 0 {
			return "", nil, errors.New("completed and paused children remained in the active root list")
		}
		if f.countRequests("lifecycle_child", "root-pause-A") != 2 || f.countRequests("lifecycle_child", "root-pause-B") != 1 {
			return "", nil, errors.New("explicit resume started an unexpected child provider request")
		}
		f.markCheck("rootAbortSelectedResume")
		f.mu.Lock()
		f.done = true
		f.mu.Unlock()
		return "ROOT_PAUSE_RESUME_DONE", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected root pause stage %d for prompt %q", f.stage, latest)
}

func (f *fixture) lifecycleRunRefs() []ref {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ref(nil), f.runs...)
}

func requirePausedOutcome(value result, expected ref) error {
	if value.Child.ID != expected.ID || value.Child.State != "paused" || value.Outcome.Run != expected || value.Outcome.State != "paused" || value.Outcome.Model != "fixture/lifecycle_child" {
		return fmt.Errorf("child/outcome = %#v, want paused ID %q with its retained lifecycle outcome", value, expected.ID)
	}
	return nil
}

func (f *fixture) countRequests(model, marker string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, request := range f.requests {
		if request.Model == model && strings.Contains(userText(request), marker) {
			count++
		}
	}
	return count
}

func fixtureEnvironment(pig, bash, directory, agentDir string) []string {
	searchPath := filepath.Dir(pig)
	bashDir := filepath.Dir(bash)
	if bashDir != filepath.Dir(pig) {
		searchPath += string(os.PathListSeparator) + bashDir
	}
	home := filepath.Join(directory, "home")
	environment := []string{"PATH=" + searchPath, "HOME=" + home, "USERPROFILE=" + home, "PIG_USE_PI_DIRS=0", "PIG_OFFLINE=1", "PI_OFFLINE=1", "PI_SKIP_VERSION_CHECK=1", "GOPROXY=off", "GOENV=off", "TERM=xterm-256color", "LANG=C.UTF-8", "LITTER_FIXTURE_EVIDENCE=" + directory}
	if agentDir != "" {
		environment = append(environment, "PIG_HOME="+directory, "PIG_CODING_AGENT_DIR="+agentDir)
	}
	return environment
}

func (f *fixture) runLifecycle(ctx context.Context, directory, workspace, agentDir, pig, extension string, stdout, stderr *os.File) error {
	model := "fixture/parent_lifecycle"
	if f.scenario == "pause" {
		model = "fixture/parent_pause"
	}
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
	var boundaryGatePath, boundaryGateDirectory string
	var boundaryGateListener net.Listener
	var boundaryGateReady chan boundaryGateConnection
	if f.scenario == "completion" {
		var err error
		boundaryGateDirectory, err = os.MkdirTemp("", "litter-boundary-gate-")
		if err != nil {
			return err
		}
		boundaryGatePath = filepath.Join(boundaryGateDirectory, "gate.sock")
		boundaryGateListener, err = net.Listen("unix", boundaryGatePath)
		if err != nil {
			_ = os.RemoveAll(boundaryGateDirectory)
			return err
		}
		defer func() {
			_ = boundaryGateListener.Close()
			_ = os.RemoveAll(boundaryGateDirectory)
		}()
		boundaryGateReady = make(chan boundaryGateConnection, 2)
		go func() {
			for range 2 {
				conn, err := boundaryGateListener.Accept()
				if err != nil {
					boundaryGateReady <- boundaryGateConnection{err: err}
					return
				}
				entered, err := bufio.NewReader(conn).ReadString('\n')
				if err != nil || entered != "entered\n" {
					_ = conn.Close()
					if err == nil {
						err = fmt.Errorf("unexpected boundary gate signal %q", entered)
					}
					boundaryGateReady <- boundaryGateConnection{err: err}
					return
				}
				boundaryGateReady <- boundaryGateConnection{conn: conn}
			}
		}()
	}
	arguments := []string{"--no-extensions", "-e", selectedExtension}
	if f.scenario == "completion" {
		arguments = append(arguments, "-e", filepath.Join(filepath.Dir(extension), "litter-fixture-guard"))
	}
	arguments = append(arguments, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-session", "--approve", "--offline", "--model", model, "--mode", "rpc")
	command := exec.CommandContext(ctx, pig, arguments...)
	prepareProcess(command)
	command.Dir, command.Env, command.Stderr = workspace, fixtureEnvironment(pig, f.bashPath, directory, agentDir), stderr
	if f.scenario == "completion" {
		command.Env = append(command.Env,
			"LITTER_FIXTURE_PROXY_TARGET="+extension,
			"LITTER_FIXTURE_BOUNDARY_GATE_ARM="+filepath.Join(directory, "boundary-gate-arm"),
			"LITTER_FIXTURE_BOUNDARY_GATE_SOCKET="+boundaryGatePath,
			"LITTER_FIXTURE_BOUNDARY_GATE_EVIDENCE="+filepath.Join(directory, "boundary-gate-entry.json"),
		)
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
	waitToolExecution := func(toolName, id string) error {
		for {
			select {
			case event, open := <-events:
				if !open {
					return fmt.Errorf("host exited before tool %s for child %s started", toolName, id)
				}
				if event["type"] == "response" && event["success"] == false {
					return fmt.Errorf("host rejected command: %v", event)
				}
				args, _ := event["args"].(map[string]any)
				if event["type"] == "tool_execution_start" && event["toolName"] == toolName && args["id"] == id {
					return nil
				}
				if event["type"] == "agent_end" {
					return fmt.Errorf("root turn ended before tool %s for child %s started", toolName, id)
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
	if f.scenario == "completion" {
		if err := f.runCompletion(ctx, directory, send, wait, waitToolExecution, events, boundaryGateReady); err != nil {
			return err
		}
	} else if f.scenario == "pause" {
		if err := f.runRootPause(ctx, send, wait, waitToolExecution); err != nil {
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
	done := f.done
	runs := append([]ref(nil), f.runs...)
	f.mu.Unlock()
	if failure != nil {
		return failure
	}
	if f.scenario == "pause" && !done {
		return errors.New("root pause fixture did not complete")
	}
	return writeJSON(filepath.Join(directory, "result.json"), map[string]any{"passed": true, "scenario": f.scenario, "checks": checks, "retiredRuns": runs, "exitCode": command.ProcessState.ExitCode(), "isolatedEnvironment": true})
}

func (f *fixture) runRootPause(ctx context.Context, send func(any) error, wait func(string, string) error, waitToolExecution func(string, string) error) error {
	if err := send(map[string]any{"id": "root-pause-start", "type": "prompt", "message": "SPAWN_ROOT_PAUSE"}); err != nil {
		return err
	}
	if err := wait("ROOT_PAUSE_READY", ""); err != nil {
		return err
	}
	runs := f.lifecycleRunRefs()
	if len(runs) != 2 {
		return errors.New("root pause fixture did not retain both spawned child IDs")
	}
	if err := send(map[string]any{"id": "root-pause-wait", "type": "prompt", "message": "WAIT_DURING_ROOT_PAUSE"}); err != nil {
		return err
	}
	if err := waitToolExecution("litter_wait", runs[0].ID); err != nil {
		return err
	}
	f.markCheck("rootWaitActiveForAbort")
	if err := send(map[string]any{"id": "root-abort", "type": "abort"}); err != nil {
		return err
	}
	if err := wait("", "root-abort"); err != nil {
		return err
	}
	if err := f.wait(ctx, func() bool {
		return f.aborted["root-pause-A"] && f.aborted["root-pause-B"] && f.held["root-pause-A"] == nil && f.held["root-pause-B"] == nil
	}); err != nil {
		return fmt.Errorf("root abort did not cancel both child provider requests: %w", err)
	}
	f.markCheck("rootAbortCancelledProviderStreams")
	if f.countRequests("lifecycle_child", "root-pause-A") != 1 || f.countRequests("lifecycle_child", "root-pause-B") != 1 {
		return errors.New("root abort caused an unexpected child provider retry")
	}
	if err := send(map[string]any{"id": "root-pause-inspect", "type": "prompt", "message": "AFTER_ROOT_ABORT"}); err != nil {
		return err
	}
	if err := wait("ROOT_PAUSE_INSPECTED", ""); err != nil {
		return err
	}
	if err := send(map[string]any{"id": "root-pause-resume", "type": "prompt", "message": "RESUME_ROOT_PAUSE_A"}); err != nil {
		return err
	}
	if err := wait("ROOT_PAUSE_RESUME_DONE", ""); err != nil {
		return err
	}
	return nil
}
