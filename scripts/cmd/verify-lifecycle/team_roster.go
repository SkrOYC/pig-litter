package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type fixtureTeamID struct {
	Root  string `json:"root"`
	Epoch uint64 `json:"epoch"`
}

type fixtureMemberRef struct {
	Team       fixtureTeamID `json:"team"`
	ID         string        `json:"id"`
	Generation uint64        `json:"generation"`
}

type fixtureTeamResult struct {
	Operation string           `json:"operation"`
	State     string           `json:"state"`
	Name      string           `json:"name"`
	Member    fixtureMemberRef `json:"member"`
	Team      fixtureTeamID    `json:"team"`
	Child     child            `json:"child"`
	Members   []struct {
		Ref       fixtureMemberRef `json:"ref"`
		Name      string           `json:"name"`
		Role      string           `json:"role"`
		State     string           `json:"state"`
		Cwd       string           `json:"cwd"`
		Workspace struct {
			Kind string `json:"kind"`
			Cwd  string `json:"cwd"`
		} `json:"workspace"`
	} `json:"members"`
	Children []child `json:"children"`
}

type teamRosterFixture struct {
	leadMu      sync.Mutex
	stage       int
	done        bool
	refs        map[string]fixtureMemberRef
	started     map[string]bool
	wrote       map[string]bool
	saw         map[string]bool
	polls       int
	pauseBefore string
	pauseAfter  string
}

var fixtureTeamNames = []string{"alpha", "beta", "gamma"}

func parseTeamResult(message message) (fixtureTeamResult, error) {
	var result fixtureTeamResult
	err := json.Unmarshal([]byte(content(message.Content)), &result)
	return result, err
}

func (f *fixture) checkTeamRoster(result fixtureTeamResult, states map[string]string) error {
	if result.Operation != "team_list" || result.State != "listed" || len(result.Members) != 4 {
		return fmt.Errorf("unexpected team roster: %#v", result)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.team.refs) != 3 {
		return errors.New("missing teammate admission references")
	}
	seen := map[string]bool{}
	lead := false
	for _, member := range result.Members {
		if member.Role == "lead" {
			if lead || member.Name != "team-lead" || member.Ref.ID != result.Team.Root || member.Ref.Team != result.Team {
				return errors.New("invalid lead roster entry")
			}
			lead = true
			continue
		}
		ref, ok := f.team.refs[member.Name]
		if !ok || seen[member.Name] || member.Role != "teammate" || member.Ref != ref || member.Ref.Team != result.Team || member.Cwd != f.expectedCWD || member.Workspace.Kind != "shared_checkout" || member.Workspace.Cwd != f.expectedCWD {
			return fmt.Errorf("invalid teammate roster entry: %#v", member)
		}
		if expected := states[member.Name]; expected != "" && member.State != expected {
			return fmt.Errorf("teammate %s is %s", member.Name, member.State)
		}
		seen[member.Name] = true
	}
	if !lead || len(seen) != 3 {
		return errors.New("team roster is not flat and complete")
	}
	return nil
}

func (f *fixture) parentTeamRoster(ctx context.Context, body modelRequest) (string, []call, error) {
	f.team.leadMu.Lock()
	defer f.team.leadMu.Unlock()
	tools := toolMessages(body)
	switch f.team.stage {
	case 0:
		f.team.stage = 1
		calls := make([]call, 0, len(fixtureTeamNames))
		for _, name := range fixtureTeamNames {
			calls = append(calls, call{"team_spawn", map[string]any{"type": "worker", "name": name, "task": "Write the controlled team fixture file for " + name, "model": "fixture/team_" + name, "workspace": map[string]string{"kind": "shared_checkout"}}})
		}
		return "", calls, nil
	case 1:
		if len(tools) < 3 {
			return "", nil, errors.New("lead did not receive three teammate admissions")
		}
		refs := map[string]fixtureMemberRef{}
		ids := map[string]bool{}
		for _, message := range tools[len(tools)-3:] {
			value, err := parseTeamResult(message)
			if err != nil || value.Operation != "team_spawn" || value.State != "starting" || value.Member.ID == "" || value.Member.Generation == 0 || ids[value.Member.ID] || !contains(fixtureTeamNames, value.Name) {
				return "", nil, fmt.Errorf("invalid teammate admission: %#v, %v", value, err)
			}
			refs[value.Name], ids[value.Member.ID] = value.Member, true
		}
		if len(refs) != 3 {
			return "", nil, errors.New("teammate names were not distinct")
		}
		f.mu.Lock()
		f.team.refs = refs
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return len(f.team.saw) == 3 }); err != nil {
			return "", nil, fmt.Errorf("three teammates did not see the same roster: %w", err)
		}
		f.team.stage = 2
		return "", []call{{"team_list", map[string]any{}}}, nil
	case 2:
		value, err := parseTeamResult(tools[len(tools)-1])
		if err != nil {
			return "", nil, err
		}
		if err := f.checkTeamRoster(value, map[string]string{"alpha": "idle", "beta": "idle", "gamma": "idle"}); err != nil {
			f.team.polls++
			if f.team.polls > 50 || !strings.Contains(err.Error(), " is running") {
				return "", nil, err
			}
			time.Sleep(25 * time.Millisecond)
			return "", []call{{"team_list", map[string]any{}}}, nil
		}
		f.team.stage = 3
		return "", []call{{"litter_list", map[string]any{}}}, nil
	case 3:
		value, err := parseTeamResult(tools[len(tools)-1])
		if err != nil || value.Operation != "litter_list" || len(value.Children) != 0 {
			return "", nil, fmt.Errorf("team members entered ordinary child list: %#v, %v", value, err)
		}
		f.mu.Lock()
		alpha := f.team.refs["alpha"]
		f.mu.Unlock()
		f.team.stage = 4
		return "", []call{{"litter_stop", map[string]any{"id": alpha.ID}}}, nil
	case 4:
		value, err := parseTeamResult(tools[len(tools)-1])
		if err != nil || value.Operation != "litter_stop" || value.State != "paused" {
			return "", nil, fmt.Errorf("idle teammate pause failed: %#v, %v", value, err)
		}
		f.team.stage = 5
		return "", []call{{"team_list", map[string]any{}}}, nil
	case 5:
		value, err := parseTeamResult(tools[len(tools)-1])
		if err != nil {
			return "", nil, err
		}
		if err := f.checkTeamRoster(value, map[string]string{"alpha": "paused", "beta": "idle", "gamma": "idle"}); err != nil {
			return "", nil, fmt.Errorf("paused team roster: %w", err)
		}
		for _, member := range value.Members {
			if member.Name == "alpha" {
				f.team.pauseBefore = member.State
			}
		}
		f.team.stage = 6
		return "TEAM_ROSTER_FIRST_DONE", nil, nil
	case 6:
		if !strings.Contains(userText(body), "ROOT_TEAM_RESTART") {
			return "", nil, errors.New("root team fixture continued before restart")
		}
		f.team.stage = 7
		return "", []call{{"team_list", map[string]any{}}}, nil
	case 7:
		value, err := parseTeamResult(tools[len(tools)-1])
		if err != nil {
			return "", nil, err
		}
		if err := f.checkTeamRoster(value, map[string]string{"alpha": "paused", "beta": "idle", "gamma": "idle"}); err != nil {
			return "", nil, fmt.Errorf("reopened team roster: %w", err)
		}
		for _, member := range value.Members {
			if member.Name == "alpha" {
				f.team.pauseAfter = member.State
			}
		}
		f.mu.Lock()
		alpha := f.team.refs["alpha"]
		f.mu.Unlock()
		f.team.stage = 8
		return "", []call{{"litter_inspect", map[string]any{"id": alpha.ID}}}, nil
	case 8:
		value, err := parseTeamResult(tools[len(tools)-1])
		f.mu.Lock()
		alpha := f.team.refs["alpha"]
		f.mu.Unlock()
		if err != nil || value.Operation != "litter_inspect" || value.Child.ID != alpha.ID || value.Child.State != "paused" {
			return "", nil, fmt.Errorf("reopened teammate pause was not enforced: %#v, %v", value, err)
		}
		f.team.stage, f.team.done = 9, true
		return "TEAM_ROSTER_REOPENED", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected team roster lead stage %d", f.team.stage)
}

func (f *fixture) teamRosterMember(ctx context.Context, body modelRequest) (string, []call, error) {
	name := strings.TrimPrefix(body.Model, "team_")
	if !contains(fixtureTeamNames, name) {
		return "", nil, errors.New("unexpected teammate model")
	}
	tools := toolMessages(body)
	switch len(tools) {
	case 0:
		f.mu.Lock()
		f.team.started[name] = true
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return len(f.team.started) == 3 }); err != nil {
			return "", nil, fmt.Errorf("three shared writers did not start: %w", err)
		}
		return "", []call{{"write", map[string]any{"path": "team-" + name + ".txt", "content": "team-" + name + "-sentinel\n"}}}, nil
	case 1:
		path := filepath.Join(f.expectedCWD, "team-"+name+".txt")
		data, err := os.ReadFile(path)
		if err != nil || string(data) != "team-"+name+"-sentinel\n" {
			return "", nil, fmt.Errorf("teammate write %s = %q, %v", name, data, err)
		}
		f.mu.Lock()
		f.team.wrote[name] = true
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return len(f.team.wrote) == 3 && len(f.team.refs) == 3 }); err != nil {
			return "", nil, fmt.Errorf("three teammates did not write: %w", err)
		}
		return "", []call{{"team_list", map[string]any{}}}, nil
	case 2:
		value, err := parseTeamResult(tools[1])
		if err != nil {
			return "", nil, err
		}
		if err := f.checkTeamRoster(value, nil); err != nil {
			return "", nil, err
		}
		f.mu.Lock()
		f.team.saw[name] = true
		f.signalLocked()
		f.mu.Unlock()
		return "TEAM_" + strings.ToUpper(name) + "_DONE", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected teammate %s turn %d", name, len(tools))
}

func (f *fixture) runTeamRoster(ctx context.Context, directory, workspace, agentDir, pig, extension string, stdout, stderr *os.File) error {
	f.expectedCWD = workspace
	f.team = &teamRosterFixture{refs: map[string]fixtureMemberRef{}, started: map[string]bool{}, wrote: map[string]bool{}, saw: map[string]bool{}}
	if err := os.WriteFile(filepath.Join(workspace, "litter.yaml"), []byte("concurrency: 4\ndepth: 2\n"), 0o600); err != nil {
		return err
	}
	arguments := []string{"--no-extensions", "-e", extension, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--session-dir", filepath.Join(directory, "parent-sessions"), "--approve", "--offline", "--model", "fixture/parent_team_roster", "--mode", "json", "--print"}
	run := func(args []string) error {
		command := exec.CommandContext(ctx, pig, args...)
		prepareProcess(command)
		defer stopProcess(command)
		command.Dir, command.Env = workspace, fixtureEnvironment(pig, f.bashPath, directory, agentDir)
		command.Stdout, command.Stderr = stdout, stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("team fixture host exit: %w", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.failure
	}
	firstArgs := append(append([]string(nil), arguments...), "TEAM_ROSTER_FIRST PARENT_PRIVATE_MARKER")
	if err := run(firstArgs); err != nil {
		return err
	}
	f.team.leadMu.Lock()
	stage := f.team.stage
	f.team.leadMu.Unlock()
	if stage != 6 {
		return fmt.Errorf("first team fixture stopped at stage %d", stage)
	}
	file, err := os.Open(stdout.Name())
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	root := ""
	for scanner.Scan() {
		var event struct{ Type, ID string }
		if json.Unmarshal(scanner.Bytes(), &event) == nil && event.Type == "session" {
			root = event.ID
			break
		}
	}
	_ = file.Close()
	if root == "" {
		return errors.New("team fixture did not record the root UUID")
	}
	before := f.teamRequests()
	reopenArgs := append(append([]string(nil), arguments...), "--session", root, "ROOT_TEAM_RESTART")
	if err := run(reopenArgs); err != nil {
		return err
	}
	f.team.leadMu.Lock()
	done, pauseBefore, pauseAfter := f.team.done, f.team.pauseBefore, f.team.pauseAfter
	f.team.leadMu.Unlock()
	if !done || f.teamRequests() != before {
		return errors.New("reopening the roster changed membership or started a teammate")
	}
	f.mu.Lock()
	refs, started, wrote, saw := maps.Clone(f.team.refs), maps.Clone(f.team.started), maps.Clone(f.team.wrote), maps.Clone(f.team.saw)
	f.mu.Unlock()
	return writeJSON(filepath.Join(directory, "team-roster.json"), map[string]any{"root": root, "members": refs, "started": started, "wrote": wrote, "saw": saw, "pausedMember": refs["alpha"], "pauseStateBeforeReopen": pauseBefore, "pauseStateAfterReopen": pauseAfter, "memberRequestsBeforeReopen": before, "memberRequestsAfterReopen": f.teamRequests(), "firstArgv": append([]string{pig}, firstArgs...), "reopenArgv": append([]string{pig}, reopenArgs...)})
}

func (f *fixture) teamRequests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, request := range f.requests {
		if strings.HasPrefix(request.Model, "team_") {
			count++
		}
	}
	return count
}
