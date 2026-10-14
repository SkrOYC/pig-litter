package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (f *fixture) parentPersistence(body modelRequest) (string, []call, error) {
	switch f.stage {
	case 0:
		f.stage = 1
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "PERSISTED_CHILD_ORIGINAL_TASK", "name": "persistent-child", "model": "fixture/persistence_child", "cwd": f.expectedCWD}}}, nil
	case 1:
		value, err := lastToolResult(body)
		if err != nil || value.Child.ID == "" {
			return "", nil, fmt.Errorf("persistent child admission failed: %#v %v", value, err)
		}
		f.worker = value.Child.ref
		f.stage = 2
		return "", []call{{"litter_wait", with(f.worker, "timeoutMs", 10000)}}, nil
	case 2:
		value, err := lastToolResult(body)
		if err != nil || value.State != "completed" || value.Outcome.Text != "CHILD_NATIVE_PRIVATE_TOKEN" || value.Outcome.Run != f.worker {
			return "", nil, fmt.Errorf("persistent child's first outcome failed: %#v %v", value, err)
		}
		f.stage = 3
		f.markCheck("nativeChildPersisted")
		return "PERSISTENCE_FIRST_DONE", nil, nil
	case 3:
		if !strings.Contains(userText(body), "ROOT_RESTART") {
			return "", nil, errors.New("root persistence fixture continued before restart")
		}
		f.stage = 4
		return "", []call{
			{"litter_inspect", with(f.worker)},
			{"litter_spawn", map[string]any{"type": "scout", "task": "duplicate name after root restart", "name": "persistent-child", "model": "fixture/persistence_child"}},
		}, nil
	case 4:
		values, err := trailingToolResults(body, 2)
		if err != nil || values[0].Child.ID != f.worker.ID || values[0].Child.CWD != f.expectedCWD || values[0].Outcome.Text != "CHILD_NATIVE_PRIVATE_TOKEN" || values[1].State != "rejected" || !strings.Contains(values[1].Report, "name already") {
			return "", nil, fmt.Errorf("restarted owner lost child metadata or names: %#v %v", values, err)
		}
		f.stage = 5
		f.markCheck("nativeOwnerRecovered")
		return "", []call{{"litter_message", with(f.worker, "resume", true, "text", "CONTINUE_PERSISTED_CHILD")}}, nil
	case 5:
		value, err := lastToolResult(body)
		if err != nil || value.Child.ID != f.worker.ID || value.Delivery != "resumed" {
			return "", nil, fmt.Errorf("restarted owner did not resume the same UUID: %#v %v", value, err)
		}
		f.stage = 6
		return "", []call{{"litter_wait", with(f.worker, "timeoutMs", 10000)}}, nil
	case 6:
		value, err := lastToolResult(body)
		if err != nil || value.State != "completed" || value.Outcome.Run != f.worker || value.Outcome.Text != "CHILD_PERSISTENCE_RESUMED" {
			return "", nil, fmt.Errorf("restarted owner lost native child history: %#v %v", value, err)
		}
		f.stage = 7
		f.markCheck("nativeUUIDResumedAfterRestart")
		return "", []call{{"litter_stop", with(f.worker)}}, nil
	case 7:
		value, err := lastToolResult(body)
		if err != nil || value.State != "completed" || value.Outcome.State != "completed" || value.Outcome.Run != f.worker || value.StoppedCount != 0 {
			return "", nil, fmt.Errorf("terminal stop changed its real outcome: %#v %v", value, err)
		}
		f.markCheck("completedStopOutcome")
		f.done = true
		return "PERSISTENCE_RESTART_DONE", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected persistence stage %d", f.stage)
}

func (f *fixture) persistenceChild(body modelRequest) (string, []call, error) {
	if strings.Contains(messageText(body), "PARENT_PRIVATE_MARKER") {
		return "", nil, errors.New("native child received the parent's private conversation")
	}
	if strings.Contains(userText(body), "CONTINUE_PERSISTED_CHILD") {
		if !strings.Contains(userText(body), "PERSISTED_CHILD_ORIGINAL_TASK") || !strings.Contains(messageText(body), "CHILD_NATIVE_PRIVATE_TOKEN") {
			return "", nil, errors.New("UUID resume did not retain the child's native conversation")
		}
		return "CHILD_PERSISTENCE_RESUMED", nil, nil
	}
	if len(toolMessages(body)) == 0 {
		return "", []call{{"read", map[string]any{"path": "input.txt"}}}, nil
	}
	if !strings.Contains(messageText(body), "persisted child directory marker") {
		return "", nil, errors.New("native child did not use its own persisted working directory")
	}
	return "CHILD_NATIVE_PRIVATE_TOKEN", nil, nil
}

func (f *fixture) runPersistence(ctx context.Context, directory, workspace, agentDir, pig, extension string, stdout, stderr *os.File) error {
	f.expectedCWD = filepath.Join(workspace, "persisted-child")
	if err := os.Mkdir(f.expectedCWD, 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(f.expectedCWD, "input.txt"), []byte("persisted child directory marker\n"), 0o600); err != nil {
		return err
	}
	arguments := []string{"--no-extensions", "-e", extension, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--session-dir", filepath.Join(directory, "parent-sessions"), "--approve", "--offline", "--model", "fixture/parent_persistence", "--mode", "json", "--print"}
	run := func(args []string) error {
		command := exec.CommandContext(ctx, pig, args...)
		prepareProcess(command)
		defer stopProcess(command)
		command.Dir = workspace
		command.Env = fixtureEnvironment(pig, f.bashPath, directory, agentDir)
		command.Stdout, command.Stderr = stdout, stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("released persistence host exit: %w", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.failure
	}
	if err := run(append(append([]string(nil), arguments...), "PERSISTENCE_FIRST PARENT_PRIVATE_MARKER")); err != nil {
		return err
	}
	if f.stage != 3 {
		return fmt.Errorf("first persisted root did not finish, stage %d", f.stage)
	}
	file, err := os.Open(stdout.Name())
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(file)
	var rootID string
	for scanner.Scan() {
		var event struct{ Type, ID string }
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "session" {
			rootID = event.ID
			break
		}
	}
	_ = file.Close()
	if rootID == "" {
		return errors.New("released host did not report its persisted root UUID")
	}
	resume := append(append([]string(nil), arguments...), "--session", rootID, "ROOT_RESTART")
	if err := run(resume); err != nil {
		return err
	}
	if !f.done {
		return errors.New("restarted persisted root did not finish")
	}
	return writeJSON(filepath.Join(directory, "persistence.json"), map[string]any{"rootId": rootID, "childId": f.worker.ID, "childCwd": f.expectedCWD, "checks": f.checks})
}
