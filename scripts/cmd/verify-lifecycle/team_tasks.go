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
	ID          fixtureTaskID     `json:"id"`
	Subject     string            `json:"subject"`
	Description string            `json:"description"`
	State       string            `json:"state"`
	Owner       *fixtureMemberRef `json:"owner"`
	BlockedBy   []fixtureTaskID   `json:"blockedBy"`
	Metadata    map[string]string `json:"metadata"`
	Blocked     bool              `json:"blocked"`
}

type fixtureTaskResult struct {
	Operation string        `json:"operation"`
	State     string        `json:"state"`
	Report    string        `json:"report"`
	Task      fixtureTask   `json:"task"`
	Tasks     []fixtureTask `json:"tasks"`
}

type teamTaskFixture struct {
	leadMu          sync.Mutex
	stage           int
	done            bool
	tasks           map[string]fixtureTask
	refs            map[string]fixtureMemberRef
	saw             map[string]bool
	got             map[string]bool
	edited          map[string]bool
	blocked         map[string]bool
	claims          map[string]fixtureTaskResult
	assigned        fixtureTask
	completed       bool
	unblockedClaim  fixtureTaskResult
	completion      fixtureTaskResult
	completionRetry fixtureTaskResult
	rejectedCycle   bool
	rejectedForeign bool
	deleted         fixtureTask
	afterReopen     fixtureTask
	creationRetry   fixtureTaskResult
}

var fixtureTaskNames = []string{"alpha", "beta", "gamma", "delta"}

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
		return "", []call{{"team_task_create", map[string]any{"subject": "prerequisite"}}, {"team_task_create", map[string]any{"subject": "available", "description": "before", "metadata": map[string]any{"keep": "yes", "remove": "old"}}}}, nil
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
		g.stage = 20
		return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["available"].ID, "action": "edit", "subject": "available-edited", "description": "shared update", "metadata": map[string]any{"add": "new", "remove": nil}, "addBlocks": []fixtureTaskID{result.Task.ID}}}}, nil
	case 20:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "updated" || result.Task.ID != g.tasks["available"].ID || result.Task.Subject != "available-edited" || result.Task.Description != "shared update" || result.Task.Metadata["keep"] != "yes" || result.Task.Metadata["add"] != "new" {
			return "", nil, fmt.Errorf("task value edit failed: %#v, %v", result, err)
		}
		if _, present := result.Task.Metadata["remove"]; present {
			return "", nil, errors.New("task metadata removal failed")
		}
		g.tasks["available"] = result.Task
		foreign := g.tasks["blocked"].ID
		foreign.Team.Root = "00000000-0000-7000-8000-000000000099"
		g.stage = 21
		return "", []call{
			{"team_task_update", map[string]any{"taskId": result.Task.ID, "action": "edit", "subject": "must-not-save", "addBlockedBy": []fixtureTaskID{g.tasks["blocked"].ID}}},
			{"team_task_update", map[string]any{"taskId": result.Task.ID, "action": "edit", "subject": "must-not-save", "addBlockedBy": []fixtureTaskID{foreign}}},
		}, nil
	case 21:
		if len(tools) < 2 {
			return "", nil, errors.New("missing invalid task edit results")
		}
		cycle, cycleErr := parseTaskResult(tools[len(tools)-2])
		foreign, foreignErr := parseTaskResult(tools[len(tools)-1])
		if cycleErr != nil || cycle.State != "rejected" || !strings.Contains(cycle.Report, "cycle") || foreignErr != nil || foreign.State != "rejected" || !strings.Contains(foreign.Report, "another team") {
			return "", nil, fmt.Errorf("invalid dependency results: %#v, %#v, %v, %v", cycle, foreign, cycleErr, foreignErr)
		}
		g.rejectedCycle, g.rejectedForeign = true, true
		g.stage = 22
		return "", []call{{"team_task_get", map[string]any{"taskId": g.tasks["available"].ID}}}, nil
	case 22:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "found" || result.Task.Subject != "available-edited" || len(result.Task.BlockedBy) != 0 {
			return "", nil, fmt.Errorf("rejected edit changed the task: %#v, %v", result, err)
		}
		g.stage = 3
		calls := make([]call, 0, len(fixtureTaskNames))
		for _, name := range fixtureTaskNames {
			calls = append(calls, call{"team_spawn", map[string]any{"type": "worker", "name": name, "task": "Inspect and claim the shared task.", "model": "fixture/task_" + name, "workspace": map[string]string{"kind": "shared_checkout"}}})
		}
		return "", calls, nil
	case 3:
		if len(tools) < len(fixtureTaskNames) {
			return "", nil, errors.New("lead did not receive teammate admissions")
		}
		f.mu.Lock()
		for _, message := range tools[len(tools)-len(fixtureTaskNames):] {
			result, err := parseTeamResult(message)
			if err != nil || result.State != "starting" || !contains(fixtureTaskNames, result.Name) || result.Member.ID == "" {
				f.mu.Unlock()
				return "", nil, fmt.Errorf("invalid teammate admission: %#v, %v", result, err)
			}
			g.refs[result.Name] = result.Member
		}
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return len(g.claims) == len(fixtureTaskNames) }); err != nil {
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
		if err != nil || result.State != "assigned" || !result.Task.Blocked || result.Task.Owner == nil || *result.Task.Owner != g.refs["alpha"] || len(result.Task.BlockedBy) != 2 {
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
		g.stage = 6
		return "", []call{{"team_task_create", map[string]any{"subject": "unblockable", "blockedBy": []fixtureTaskID{g.tasks["prerequisite"].ID}}}}, nil
	case 6:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "created" || result.Task.Subject != "unblockable" || !result.Task.Blocked {
			return "", nil, fmt.Errorf("invalid task awaiting completion: %#v, %v", result, err)
		}
		g.tasks["unblockable"] = result.Task
		g.stage = 7
		return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["prerequisite"].ID, "action": "complete"}}}, nil
	case 7:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "completed" || result.Task.State != "completed" || result.Task.ID != g.tasks["prerequisite"].ID {
			return "", nil, fmt.Errorf("prerequisite completion failed: %#v, %v", result, err)
		}
		f.mu.Lock()
		g.completed = true
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return g.completionRetry.State != "" }); err != nil {
			return "", nil, fmt.Errorf("teammate did not complete the unblocked task twice: %w", err)
		}
		f.mu.Lock()
		claim := g.unblockedClaim
		completion := g.completion
		retry := g.completionRetry
		f.mu.Unlock()
		if claim.State != "claimed" || claim.Task.Blocked || claim.Task.Owner == nil || *claim.Task.Owner != g.refs["alpha"] || claim.Task.ID != g.tasks["unblockable"].ID {
			return "", nil, fmt.Errorf("unblocked claim failed: %#v", claim)
		}
		if completion.State != "completed" || retry.State != "already_completed" || completion.Task.ID != claim.Task.ID || retry.Task.ID != claim.Task.ID || completion.Task.State != "completed" || retry.Task.State != "completed" {
			return "", nil, fmt.Errorf("owner completion or retry failed: %#v, %#v", completion, retry)
		}
		g.stage = 8
		return "", []call{{"team_task_create", map[string]any{"subject": "delete-highest"}}}, nil
	case 8:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "created" || result.Task.Subject != "delete-highest" || result.Task.ID.Number <= g.tasks["unblockable"].ID.Number {
			return "", nil, fmt.Errorf("invalid highest task: %#v, %v", result, err)
		}
		g.deleted = result.Task
		g.stage = 9
		return "", []call{{"team_task_update", map[string]any{"taskId": result.Task.ID, "action": "delete"}}}, nil
	case 9:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "deleted" || result.Task.ID != g.deleted.ID {
			return "", nil, fmt.Errorf("highest task deletion failed: %#v, %v", result, err)
		}
		g.stage = 10
		return "TEAM_TASKS_FIRST_DONE", nil, nil
	case 10:
		if !strings.Contains(userText(body), "ROOT_TEAM_TASKS_REOPEN") {
			return "", nil, errors.New("task fixture continued before reopening")
		}
		g.stage = 11
		return "", []call{{"team_task_create", map[string]any{"subject": "after-reopen", "submissionId": "post-reopen-1"}}}, nil
	case 11:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "created" || result.Task.Subject != "after-reopen" || result.Task.ID.Team != g.deleted.ID.Team || result.Task.ID.Number <= g.deleted.ID.Number {
			return "", nil, fmt.Errorf("task ID after reopening did not advance: %#v, %v", result, err)
		}
		g.afterReopen = result.Task
		g.stage = 12
		return "", []call{{"team_task_create", map[string]any{"subject": "after-reopen", "submissionId": "post-reopen-1"}}}, nil
	case 12:
		result, err := parseTaskResult(tools[len(tools)-1])
		if err != nil || result.State != "already_created" || result.Task.ID != g.afterReopen.ID {
			return "", nil, fmt.Errorf("task creation retry failed: %#v, %v", result, err)
		}
		g.creationRetry = result
		g.done, g.stage = true, 13
		return "TEAM_TASKS_REOPENED_DONE", nil, nil
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
			if task.ID == g.tasks["available"].ID && (task.Subject != "available-edited" || task.Description != "shared update" || task.Metadata["keep"] != "yes" || task.Metadata["add"] != "new") {
				return "", nil, fmt.Errorf("teammate %s saw different task values: %#v", name, task)
			}
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
		if err := f.wait(ctx, func() bool { return len(g.saw) == len(fixtureTaskNames) && len(g.refs) == len(fixtureTaskNames) }); err != nil {
			return "", nil, err
		}
		return "", []call{{"team_task_get", map[string]any{"taskId": g.tasks["available"].ID}}}, nil
	case 2:
		result, err := parseTaskResult(tools[1])
		if err != nil || result.State != "found" || result.Task.ID != g.tasks["available"].ID || result.Task.Subject != "available-edited" || result.Task.Description != "shared update" || result.Task.Metadata["add"] != "new" || result.Task.State != "pending" || result.Task.Owner != nil || result.Task.Blocked {
			return "", nil, fmt.Errorf("teammate %s task lookup: %#v, %v", name, result, err)
		}
		f.mu.Lock()
		g.got[name] = true
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return len(g.got) == len(fixtureTaskNames) }); err != nil {
			return "", nil, err
		}
		return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["available"].ID, "action": "edit", "metadata": map[string]any{name: "confirmed"}}}}, nil
	case 3:
		result, err := parseTaskResult(tools[2])
		if err != nil || result.State != "updated" || result.Task.Metadata[name] != "confirmed" {
			return "", nil, fmt.Errorf("teammate %s task edit: %#v, %v", name, result, err)
		}
		f.mu.Lock()
		g.edited[name] = true
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return len(g.edited) == len(fixtureTaskNames) }); err != nil {
			return "", nil, err
		}
		return "", []call{{"team_task_list", map[string]any{}}}, nil
	case 4:
		result, err := parseTaskResult(tools[3])
		if err != nil || result.State != "listed" || len(result.Tasks) != 3 {
			return "", nil, fmt.Errorf("teammate %s updated task list: %#v, %v", name, result, err)
		}
		found := false
		for _, task := range result.Tasks {
			if task.ID != g.tasks["available"].ID {
				continue
			}
			found = true
			for _, member := range fixtureTaskNames {
				if task.Metadata[member] != "confirmed" {
					return "", nil, fmt.Errorf("teammate %s missed %s's update: %#v", name, member, task)
				}
			}
		}
		if !found {
			return "", nil, fmt.Errorf("teammate %s missed the updated task", name)
		}
		return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["blocked"].ID, "action": "claim"}}}, nil
	case 5:
		result, err := parseTaskResult(tools[4])
		if err != nil || result.State != "blocked" || !result.Task.Blocked || result.Task.Owner != nil {
			return "", nil, fmt.Errorf("teammate %s blocked claim: %#v, %v", name, result, err)
		}
		f.mu.Lock()
		g.blocked[name] = true
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return len(g.blocked) == len(fixtureTaskNames) }); err != nil {
			return "", nil, err
		}
		return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["available"].ID, "action": "claim"}}}, nil
	case 6:
		result, err := parseTaskResult(tools[5])
		if err != nil || result.Task.ID != g.tasks["available"].ID {
			return "", nil, fmt.Errorf("teammate %s claim: %#v, %v", name, result, err)
		}
		f.mu.Lock()
		g.claims[name] = result
		f.signalLocked()
		f.mu.Unlock()
		if name == "alpha" {
			if err := f.wait(ctx, func() bool { return g.completed }); err != nil {
				return "", nil, fmt.Errorf("teammate did not observe prerequisite completion: %w", err)
			}
			return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["unblockable"].ID, "action": "claim"}}}, nil
		}
		return "TEAM_TASK_" + strings.ToUpper(name) + "_DONE", nil, nil
	case 7:
		result, err := parseTaskResult(tools[6])
		if name != "alpha" || err != nil {
			return "", nil, fmt.Errorf("invalid unblocked teammate result: %#v, %v", result, err)
		}
		f.mu.Lock()
		g.unblockedClaim = result
		f.signalLocked()
		f.mu.Unlock()
		return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["unblockable"].ID, "action": "complete"}}}, nil
	case 8:
		result, err := parseTaskResult(tools[7])
		if name != "alpha" || err != nil || result.State != "completed" || result.Task.ID != g.tasks["unblockable"].ID || result.Task.State != "completed" {
			return "", nil, fmt.Errorf("teammate completion failed: %#v, %v", result, err)
		}
		f.mu.Lock()
		g.completion = result
		f.mu.Unlock()
		return "", []call{{"team_task_update", map[string]any{"taskId": g.tasks["unblockable"].ID, "action": "complete"}}}, nil
	case 9:
		result, err := parseTaskResult(tools[8])
		if name != "alpha" || err != nil || result.State != "already_completed" || result.Task.ID != g.tasks["unblockable"].ID || result.Task.State != "completed" {
			return "", nil, fmt.Errorf("teammate completion retry failed: %#v, %v", result, err)
		}
		f.mu.Lock()
		g.completionRetry = result
		f.signalLocked()
		f.mu.Unlock()
		return "TEAM_TASK_ALPHA_DONE", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected teammate %s task turn %d", name, len(tools))
}

func (f *fixture) runTeamTasks(ctx context.Context, directory, workspace, agentDir, pig, extension string, stdout, stderr *os.File) error {
	f.expectedCWD = workspace
	f.taskGraph = &teamTaskFixture{tasks: map[string]fixtureTask{}, refs: map[string]fixtureMemberRef{}, saw: map[string]bool{}, got: map[string]bool{}, edited: map[string]bool{}, blocked: map[string]bool{}, claims: map[string]fixtureTaskResult{}}
	if err := os.WriteFile(filepath.Join(workspace, "litter.yaml"), []byte("concurrency: 4\ndepth: 2\n"), 0o600); err != nil {
		return err
	}
	arguments := []string{"--no-extensions", "-e", extension, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--session-dir", filepath.Join(directory, "parent-sessions"), "--approve", "--offline", "--model", "fixture/parent_team_tasks", "--mode", "json", "--print"}
	run := func(args []string) error {
		command := exec.CommandContext(ctx, pig, args...)
		prepareProcess(command)
		defer stopProcess(command)
		command.Dir, command.Env = workspace, fixtureEnvironment(pig, f.bashPath, directory, agentDir)
		command.Stdout, command.Stderr = stdout, stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("team tasks fixture host exit: %w", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.failure
	}
	firstArgs := append(append([]string(nil), arguments...), "Run the shared task fixture.")
	if err := run(firstArgs); err != nil {
		return err
	}
	f.taskGraph.leadMu.Lock()
	stage := f.taskGraph.stage
	root := f.taskGraph.tasks["prerequisite"].ID.Team.Root
	f.taskGraph.leadMu.Unlock()
	if stage != 10 {
		return fmt.Errorf("first task fixture stopped at stage %d", stage)
	}
	reopenArgs := append(append([]string(nil), arguments...), "--session", root, "ROOT_TEAM_TASKS_REOPEN")
	if err := run(reopenArgs); err != nil {
		return err
	}
	f.taskGraph.leadMu.Lock()
	defer f.taskGraph.leadMu.Unlock()
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.taskGraph.done {
		return fmt.Errorf("team tasks fixture stopped at stage %d", f.taskGraph.stage)
	}
	return writeJSON(filepath.Join(directory, "team-tasks.json"), map[string]any{"firstArgv": append([]string{pig}, firstArgs...), "reopenArgv": append([]string{pig}, reopenArgs...), "tasks": f.taskGraph.tasks, "members": f.taskGraph.refs, "saw": f.taskGraph.saw, "got": f.taskGraph.got, "edited": f.taskGraph.edited, "blocked": f.taskGraph.blocked, "claims": f.taskGraph.claims, "assigned": f.taskGraph.assigned, "unblockedClaim": f.taskGraph.unblockedClaim, "completion": f.taskGraph.completion, "completionRetry": f.taskGraph.completionRetry, "rejectedCycle": f.taskGraph.rejectedCycle, "rejectedForeign": f.taskGraph.rejectedForeign, "deleted": f.taskGraph.deleted, "afterReopen": f.taskGraph.afterReopen, "creationRetry": f.taskGraph.creationRetry})
}
