package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

type fixtureTaskID struct {
	Team   fixtureTeamID `json:"team"`
	Number uint64        `json:"number"`
}

type fixtureTask struct {
	ID        fixtureTaskID     `json:"id"`
	Subject   string            `json:"subject"`
	State     string            `json:"state"`
	Owner     *fixtureMemberRef `json:"owner"`
	BlockedBy []fixtureTaskID   `json:"blockedBy"`
	Blocked   bool              `json:"blocked"`
}

type fixtureTaskResult struct {
	Operation string        `json:"operation"`
	State     string        `json:"state"`
	Task      fixtureTask   `json:"task"`
	Tasks     []fixtureTask `json:"tasks"`
}

type teamTaskFixture struct {
	leadMu   sync.Mutex
	stage    int
	done     bool
	tasks    map[string]fixtureTask
	refs     map[string]fixtureMemberRef
	saw      map[string]bool
	blocked  map[string]bool
	claims   map[string]fixtureTaskResult
	assigned fixtureTask
}

var fixtureTaskNames = []string{"alpha", "beta"}

func parseTaskResult(message message) (fixtureTaskResult, error) {
	var result fixtureTaskResult
	err := json.Unmarshal([]byte(content(message.Content)), &result)
	return result, err
}

func (f *fixture) parentTeamTasks(ctx context.Context, body modelRequest) (string, []call, error) {
	g := f.taskGraph
	g.leadMu.Lock()
	defer g.leadMu.Unlock()
	tools := toolMessages(body)
	switch g.stage {
	case 0:
		g.stage = 1
		return "", []call{{"team_task_create", map[string]any{"subject": "prerequisite"}}, {"team_task_create", map[string]any{"subject": "available"}}}, nil
	case 1:
		if len(tools) < 2 {
			return "", nil, errors.New("lead did not receive two tasks")
		}
		for _, message := range tools[len(tools)-2:] {
			result, err := parseTaskResult(message)
			if err != nil || result.State != "created" || result.Task.ID.Number == 0 || (result.Task.Subject != "prerequisite" && result.Task.Subject != "available") {
				return "", nil, fmt.Errorf("invalid task creation: %#v, %v", result, err)
			}
			g.tasks[result.Task.Subject] = result.Task
		}
		if len(g.tasks) != 2 || g.tasks["prerequisite"].ID == g.tasks["available"].ID {
			return "", nil, errors.New("created tasks do not have distinct identities")
		}
		g.stage = 2
		return "", []call{{"team_task_create", map[string]any{"subject": "blocked", "blockedBy": []fixtureTaskID{g.tasks["prerequisite"].ID}}}}, nil
	case 2:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "created" || result.Task.Subject != "blocked" || !result.Task.Blocked || len(result.Task.BlockedBy) != 1 || result.Task.BlockedBy[0] != g.tasks["prerequisite"].ID {
			return "", nil, fmt.Errorf("invalid blocked task: %#v, %v", result, err)
		}
		g.tasks["blocked"] = result.Task
		g.stage = 3
		calls := make([]call, 0, len(fixtureTaskNames))
		for _, name := range fixtureTaskNames {
			calls = append(calls, call{"team_spawn", map[string]any{"type": "worker", "name": name, "task": "Inspect and claim the shared task.", "model": "fixture/task_" + name, "workspace": map[string]string{"kind": "shared_checkout"}}})
		}
		return "", calls, nil
	case 3:
		if len(tools) < 2 {
			return "", nil, errors.New("lead did not receive teammate admissions")
		}
		f.mu.Lock()
		for _, message := range tools[len(tools)-2:] {
			result, err := parseTeamResult(message)
			if err != nil || result.State != "starting" || !contains(fixtureTaskNames, result.Name) || result.Member.ID == "" {
				f.mu.Unlock()
				return "", nil, fmt.Errorf("invalid teammate admission: %#v, %v", result, err)
			}
			g.refs[result.Name] = result.Member
		}
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return len(g.claims) == 2 }); err != nil {
			return "", nil, fmt.Errorf("teammates did not finish competing claims: %w", err)
		}
		f.mu.Lock()
		wins := 0
		var owner *fixtureMemberRef
		for _, result := range g.claims {
			if result.State == "claimed" {
				wins++
				owner = result.Task.Owner
			}
		}
		valid := wins == 1 && owner != nil
		for _, result := range g.claims {
			valid = valid && (result.State == "claimed" || result.State == "already_claimed") && result.Task.Owner != nil && owner != nil && *result.Task.Owner == *owner
		}
		f.mu.Unlock()
		if !valid {
			return "", nil, fmt.Errorf("competing claims did not agree on one owner: %#v", g.claims)
		}
		g.stage = 4
		return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["blocked"].ID, "action": "assign", "assignee": "alpha"}}}, nil
	case 4:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "assigned" || !result.Task.Blocked || result.Task.Owner == nil || *result.Task.Owner != g.refs["alpha"] || len(result.Task.BlockedBy) != 1 {
			return "", nil, fmt.Errorf("blocked lead assignment failed: %#v, %v", result, err)
		}
		g.assigned = result.Task
		g.stage = 5
		return "", []call{{"team_task_list", map[string]any{}}}, nil
	case 5:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "listed" || len(result.Tasks) != 3 {
			return "", nil, fmt.Errorf("lead final task list: %#v, %v", result, err)
		}
		found := false
		for _, task := range result.Tasks {
			if task.ID == g.assigned.ID && task.Blocked && task.Owner != nil && *task.Owner == g.refs["alpha"] {
				found = true
			}
		}
		if !found {
			return "", nil, errors.New("lead assignment was not visible in the final list")
		}
		g.done, g.stage = true, 6
		return "TEAM_TASKS_PREVIEW_DONE", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected team tasks lead stage %d", g.stage)
}

func (f *fixture) teamTaskMember(ctx context.Context, body modelRequest) (string, []call, error) {
	g := f.taskGraph
	name := strings.TrimPrefix(body.Model, "task_")
	tools := toolMessages(body)
	switch len(tools) {
	case 0:
		return "", []call{{"team_task_list", map[string]any{}}}, nil
	case 1:
		result, err := parseTaskResult(tools[0])
		if err != nil || result.State != "listed" || len(result.Tasks) != 3 {
			return "", nil, fmt.Errorf("teammate %s task list: %#v, %v", name, result, err)
		}
		seen := map[fixtureTaskID]bool{}
		for _, task := range result.Tasks {
			seen[task.ID] = true
		}
		for _, task := range g.tasks {
			if !seen[task.ID] {
				return "", nil, fmt.Errorf("teammate %s missed task %s", name, task.Subject)
			}
		}
		f.mu.Lock()
		g.saw[name] = true
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return len(g.saw) == 2 && len(g.refs) == 2 }); err != nil {
			return "", nil, err
		}
		return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["blocked"].ID, "action": "claim"}}}, nil
	case 2:
		result, err := parseTaskResult(tools[1])
		if err != nil || result.State != "blocked" || !result.Task.Blocked || result.Task.Owner != nil {
			return "", nil, fmt.Errorf("teammate %s blocked claim: %#v, %v", name, result, err)
		}
		f.mu.Lock()
		g.blocked[name] = true
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return len(g.blocked) == 2 }); err != nil {
			return "", nil, err
		}
		return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["available"].ID, "action": "claim"}}}, nil
	case 3:
		result, err := parseTaskResult(tools[2])
		if err != nil || result.Task.ID != g.tasks["available"].ID {
			return "", nil, fmt.Errorf("teammate %s claim: %#v, %v", name, result, err)
		}
		f.mu.Lock()
		g.claims[name] = result
		f.signalLocked()
		f.mu.Unlock()
		return "TEAM_TASK_" + strings.ToUpper(name) + "_DONE", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected teammate %s task turn %d", name, len(tools))
}

func (f *fixture) runTeamTasks(ctx context.Context, directory, workspace, agentDir, pig, extension string, stdout, stderr *os.File) error {
	f.expectedCWD = workspace
	f.taskGraph = &teamTaskFixture{tasks: map[string]fixtureTask{}, refs: map[string]fixtureMemberRef{}, saw: map[string]bool{}, blocked: map[string]bool{}, claims: map[string]fixtureTaskResult{}}
	if err := os.WriteFile(filepath.Join(workspace, "litter.yaml"), []byte("concurrency: 3\ndepth: 2\n"), 0o600); err != nil {
		return err
	}
	arguments := []string{"--no-extensions", "-e", extension, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--session-dir", filepath.Join(directory, "parent-sessions"), "--approve", "--offline", "--model", "fixture/parent_team_tasks", "--mode", "json", "--print", "Run the shared task fixture."}
	command := exec.CommandContext(ctx, pig, arguments...)
	prepareProcess(command)
	defer stopProcess(command)
	command.Dir, command.Env = workspace, fixtureEnvironment(pig, f.bashPath, directory, agentDir)
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("team tasks fixture host exit: %w", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failure != nil {
		return f.failure
	}
	if !f.taskGraph.done {
		return fmt.Errorf("team tasks fixture stopped at stage %d", f.taskGraph.stage)
	}
	return writeJSON(filepath.Join(directory, "team-tasks.json"), map[string]any{"argv": append([]string{pig}, arguments...), "tasks": f.taskGraph.tasks, "members": f.taskGraph.refs, "saw": f.taskGraph.saw, "blocked": f.taskGraph.blocked, "claims": f.taskGraph.claims, "assigned": f.taskGraph.assigned})
}
