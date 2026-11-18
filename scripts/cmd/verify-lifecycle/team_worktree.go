package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const worktreeSentinelFile = "team-isolated.txt"
const worktreeSentinel = "isolated teammate sentinel\n"

type fixtureWorkspacePolicy struct {
	Kind         string `json:"kind"`
	Cwd          string `json:"cwd"`
	Repository   string `json:"repository"`
	GitCommonDir string `json:"gitCommonDir"`
	AllocationID string `json:"allocationId"`
	Branch       string `json:"branch"`
	BaseCommit   string `json:"baseCommit"`
	Phase        string `json:"phase"`
}

type fixtureWorktreeMember struct {
	Ref       fixtureMemberRef       `json:"ref"`
	Name      string                 `json:"name"`
	Role      string                 `json:"role"`
	State     string                 `json:"state"`
	Cwd       string                 `json:"cwd"`
	Workspace fixtureWorkspacePolicy `json:"workspace"`
}

type fixtureWorktreeList struct {
	Operation string                  `json:"operation"`
	State     string                  `json:"state"`
	Team      fixtureTeamID           `json:"team"`
	Members   []fixtureWorktreeMember `json:"members"`
}

type teamWorktreeFixture struct {
	leadMu      sync.Mutex
	stage       int
	polls       int
	done        bool
	ref         fixtureMemberRef
	workspace   fixtureWorkspacePolicy
	memberSaw   bool
	memberWrote bool
	repository  string
	agentDir    string
	commonDir   string
	baseCommit  string
	git         string
	environment []string
	gitCommands [][]string
}

func (f *fixture) expectedWorktree(ref fixtureMemberRef) fixtureWorkspacePolicy {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fixtureWorkspacePolicy{
		Kind:         "isolated_worktree",
		Cwd:          filepath.Join(f.worktree.agentDir, "pig-litter", "roots", ref.Team.Root, "worktrees", ref.ID),
		Repository:   f.worktree.repository,
		GitCommonDir: f.worktree.commonDir,
		AllocationID: ref.ID,
		Branch:       "pig-litter/" + ref.Team.Root + "/" + ref.ID,
		BaseCommit:   f.worktree.baseCommit,
		Phase:        "prepared",
	}
}

func (f *fixture) checkWorktreeList(message message, ref fixtureMemberRef, idle bool) (fixtureWorkspacePolicy, error) {
	var result fixtureWorktreeList
	if err := json.Unmarshal([]byte(content(message.Content)), &result); err != nil {
		return fixtureWorkspacePolicy{}, fmt.Errorf("decode team list: %w", err)
	}
	if result.Operation != "team_list" || result.State != "listed" || result.Team != ref.Team || len(result.Members) != 2 {
		return fixtureWorkspacePolicy{}, fmt.Errorf("unexpected isolated team list: %#v", result)
	}
	lead, teammate := false, false
	var workspace fixtureWorkspacePolicy
	for _, member := range result.Members {
		switch member.Role {
		case "lead":
			if lead || member.Name != "team-lead" || member.Ref.Team != ref.Team || member.Ref.ID != ref.Team.Root || member.Cwd != f.worktree.repository {
				return fixtureWorkspacePolicy{}, fmt.Errorf("invalid lead identity: %#v", member)
			}
			lead = true
		case "teammate":
			if teammate || member.Name != "isolated" || member.Ref != ref || member.Cwd != member.Workspace.Cwd || member.Workspace != f.expectedWorktree(ref) {
				return fixtureWorkspacePolicy{}, fmt.Errorf("invalid isolated teammate identity: %#v", member)
			}
			if idle && member.State != "idle" {
				return fixtureWorkspacePolicy{}, fmt.Errorf("isolated teammate is %s", member.State)
			}
			teammate, workspace = true, member.Workspace
		default:
			return fixtureWorkspacePolicy{}, fmt.Errorf("unexpected team role %q", member.Role)
		}
	}
	if !lead || !teammate {
		return fixtureWorkspacePolicy{}, errors.New("isolated team list is incomplete")
	}
	return workspace, nil
}

func (f *fixture) parentTeamWorktree(ctx context.Context, body modelRequest) (string, []call, error) {
	f.worktree.leadMu.Lock()
	defer f.worktree.leadMu.Unlock()
	tools := toolMessages(body)
	switch f.worktree.stage {
	case 0:
		f.worktree.stage = 1
		return "", []call{{"team_spawn", map[string]any{"type": "worker", "name": "isolated", "task": "List your team and write the isolated fixture file.", "model": "fixture/team_isolated", "workspace": map[string]string{"kind": "isolated_worktree"}}}}, nil
	case 1:
		if len(tools) == 0 {
			return "", nil, errors.New("lead did not receive isolated teammate admission")
		}
		admission, err := parseTeamResult(tools[len(tools)-1])
		if err != nil || admission.Operation != "team_spawn" || admission.State != "starting" || admission.Name != "isolated" || admission.Member.ID == "" || admission.Member.Generation == 0 || admission.Member.Team.Root == "" || admission.Member.Team.Epoch == 0 {
			return "", nil, fmt.Errorf("invalid isolated teammate admission: %#v, %v", admission, err)
		}
		f.mu.Lock()
		f.worktree.ref = admission.Member
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return f.worktree.memberWrote }); err != nil {
			return "", nil, fmt.Errorf("isolated teammate did not list and write: %w", err)
		}
		f.worktree.stage = 2
		return "", []call{{"team_list", map[string]any{}}}, nil
	case 2:
		if len(tools) == 0 {
			return "", nil, errors.New("lead did not receive the isolated team list")
		}
		f.mu.Lock()
		ref, memberWorkspace := f.worktree.ref, f.worktree.workspace
		f.mu.Unlock()
		workspace, err := f.checkWorktreeList(tools[len(tools)-1], ref, true)
		if err != nil {
			f.worktree.polls++
			if f.worktree.polls > 100 || !strings.Contains(err.Error(), "isolated teammate is running") {
				return "", nil, err
			}
			time.Sleep(25 * time.Millisecond)
			return "", []call{{"team_list", map[string]any{}}}, nil
		}
		if workspace != memberWorkspace {
			return "", nil, errors.New("lead and teammate observed different isolated workspaces")
		}
		f.worktree.stage = 3
		return "TEAM_WORKTREE_FIRST_DONE", nil, nil
	case 3:
		if !strings.Contains(latestUserText(body), "ROOT_TEAM_WORKTREE_RESTART") {
			return "", nil, errors.New("lead continued before the isolated worktree restart")
		}
		f.worktree.stage = 4
		return "", []call{{"team_list", map[string]any{}}}, nil
	case 4:
		if len(tools) == 0 {
			return "", nil, errors.New("reopened lead did not receive the isolated team list")
		}
		f.mu.Lock()
		ref, memberWorkspace := f.worktree.ref, f.worktree.workspace
		f.mu.Unlock()
		workspace, err := f.checkWorktreeList(tools[len(tools)-1], ref, true)
		if err != nil || workspace != memberWorkspace {
			return "", nil, fmt.Errorf("reopened lead changed the isolated identity: %#v, %v", workspace, err)
		}
		f.worktree.stage, f.worktree.done = 5, true
		return "TEAM_WORKTREE_REOPENED", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected isolated lead stage %d", f.worktree.stage)
}

func (f *fixture) teamWorktreeMember(ctx context.Context, body modelRequest) (string, []call, error) {
	tools := toolMessages(body)
	switch len(tools) {
	case 0:
		return "", []call{{"team_list", map[string]any{}}}, nil
	case 1:
		if err := f.wait(ctx, func() bool { return f.worktree.ref.ID != "" }); err != nil {
			return "", nil, fmt.Errorf("isolated admission reference was not available: %w", err)
		}
		f.mu.Lock()
		ref := f.worktree.ref
		f.mu.Unlock()
		workspace, err := f.checkWorktreeList(tools[0], ref, false)
		if err != nil {
			return "", nil, fmt.Errorf("teammate's own team list: %w", err)
		}
		f.mu.Lock()
		f.worktree.workspace, f.worktree.memberSaw = workspace, true
		f.signalLocked()
		f.mu.Unlock()
		return "", []call{{"write", map[string]any{"path": worktreeSentinelFile, "content": worktreeSentinel}}}, nil
	case 2:
		if receipt := content(tools[1].Content); receipt != "Successfully wrote to "+worktreeSentinelFile {
			return "", nil, fmt.Errorf("isolated write receipt = %q", receipt)
		}
		f.mu.Lock()
		workspace := f.worktree.workspace
		f.mu.Unlock()
		data, err := os.ReadFile(filepath.Join(workspace.Cwd, worktreeSentinelFile))
		if err != nil || string(data) != worktreeSentinel {
			return "", nil, fmt.Errorf("isolated teammate write = %q, %v", data, err)
		}
		if _, err := os.Stat(filepath.Join(f.worktree.repository, worktreeSentinelFile)); !errors.Is(err, os.ErrNotExist) {
			return "", nil, fmt.Errorf("isolated write appeared in the lead checkout: %v", err)
		}
		f.mu.Lock()
		f.worktree.memberWrote = true
		f.signalLocked()
		f.mu.Unlock()
		return "TEAM_ISOLATED_DONE", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected isolated teammate turn %d", len(tools))
}

func (f *fixture) worktreeGit(ctx context.Context, cwd string, args ...string) (string, error) {
	argv := append([]string{f.worktree.git, "-C", cwd}, args...)
	f.mu.Lock()
	f.worktree.gitCommands = append(f.worktree.gitCommands, append([]string(nil), argv...))
	f.mu.Unlock()
	command := exec.CommandContext(ctx, argv[0], argv[1:]...)
	command.Env = f.worktree.environment
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSuffix(string(output), "\n"), nil
}

func (f *fixture) checkWorktreeDisk(ctx context.Context, ref fixtureMemberRef, workspace fixtureWorkspacePolicy) error {
	if workspace != f.expectedWorktree(ref) || workspace.Cwd == workspace.Repository {
		return fmt.Errorf("invalid persisted worktree identity: %#v", workspace)
	}
	for _, check := range []struct {
		cwd  string
		args []string
		want string
	}{
		{workspace.Repository, []string{"rev-parse", "--show-toplevel"}, workspace.Repository},
		{workspace.Cwd, []string{"rev-parse", "--show-toplevel"}, workspace.Cwd},
		{workspace.Repository, []string{"rev-parse", "--path-format=absolute", "--git-common-dir"}, workspace.GitCommonDir},
		{workspace.Cwd, []string{"rev-parse", "--path-format=absolute", "--git-common-dir"}, workspace.GitCommonDir},
		{workspace.Cwd, []string{"symbolic-ref", "--quiet", "HEAD"}, "refs/heads/" + workspace.Branch},
		{workspace.Cwd, []string{"rev-parse", "--verify", "HEAD^{commit}"}, workspace.BaseCommit},
		{workspace.Cwd, []string{"status", "--porcelain=v1", "--untracked-files=all"}, "?? " + worktreeSentinelFile},
		{workspace.Repository, []string{"status", "--porcelain=v1", "--untracked-files=all"}, ""},
	} {
		got, err := f.worktreeGit(ctx, check.cwd, check.args...)
		if err != nil || got != check.want {
			return fmt.Errorf("worktree Git check %v in %q = %q, want %q: %v", check.args, check.cwd, got, check.want, err)
		}
	}
	list, err := f.worktreeGit(ctx, workspace.Repository, "worktree", "list", "--porcelain")
	if err != nil || !strings.Contains(list, "worktree "+workspace.Cwd+"\nHEAD "+workspace.BaseCommit+"\nbranch refs/heads/"+workspace.Branch+"\n") {
		return fmt.Errorf("Git does not list the exact linked worktree and branch: %q, %v", list, err)
	}
	data, err := os.ReadFile(filepath.Join(workspace.Cwd, worktreeSentinelFile))
	if err != nil || string(data) != worktreeSentinel {
		return fmt.Errorf("retained isolated sentinel = %q, %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(workspace.Repository, worktreeSentinelFile)); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("sentinel is present or unavailable in the lead checkout: %v", err)
	}
	return nil
}

func (f *fixture) checkWorktreeRoster(agentDir string, ref fixtureMemberRef, workspace fixtureWorkspacePolicy) (string, error) {
	path := filepath.Join(agentDir, "pig-litter", "roots", ref.Team.Root, "team.json")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read persisted team roster: %w", err)
	}
	var roster struct {
		Team    fixtureTeamID `json:"team"`
		Members []struct {
			Ref       fixtureMemberRef       `json:"ref"`
			Name      string                 `json:"name"`
			State     string                 `json:"state"`
			Workspace fixtureWorkspacePolicy `json:"workspace"`
		} `json:"members"`
	}
	if err := json.Unmarshal(data, &roster); err != nil {
		return "", fmt.Errorf("decode persisted team roster: %w", err)
	}
	if roster.Team != ref.Team || len(roster.Members) != 1 || roster.Members[0].Ref != ref || roster.Members[0].Name != "isolated" || roster.Members[0].State != "idle" || roster.Members[0].Workspace != workspace {
		return "", fmt.Errorf("persisted isolated team identity changed: %#v", roster)
	}
	return path, nil
}

func nativeSessionPath(directory, id, cwd string) (string, error) {
	var found string
	err := filepath.WalkDir(directory, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), "_"+id+".jsonl") {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		var header struct{ Type, ID, Cwd string }
		if !scanner.Scan() {
			err = errors.Join(errors.New("native session has no header"), scanner.Err())
		} else if err = json.Unmarshal(scanner.Bytes(), &header); err == nil && (header.Type != "session" || header.ID != id || header.Cwd != cwd) {
			err = fmt.Errorf("native session header has the wrong identity or cwd: %#v", header)
		}
		err = errors.Join(err, file.Close())
		if err != nil {
			return err
		}
		if found != "" {
			return fmt.Errorf("multiple native sessions have ID %s", id)
		}
		found = path
		return nil
	})
	if err != nil {
		return "", err
	}
	if found == "" {
		return "", fmt.Errorf("native session %s was not found under %s", id, directory)
	}
	return found, nil
}

func (f *fixture) runTeamWorktree(ctx context.Context, directory, workspace, agentDir, pig, extension string, stdout, stderr *os.File) (runErr error) {
	var err error
	if directory, err = filepath.Abs(directory); err != nil {
		return err
	}
	if workspace, err = filepath.EvalSymlinks(filepath.Join(directory, "workspace")); err != nil {
		return err
	}
	if agentDir, err = filepath.EvalSymlinks(filepath.Join(directory, "agent")); err != nil {
		return err
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return fmt.Errorf("resolve Git for the isolated worktree fixture: %w", err)
	}
	if git, err = filepath.Abs(git); err != nil {
		return err
	}
	environment := fixtureEnvironment(pig, f.bashPath, directory, agentDir)
	environment[0] += string(os.PathListSeparator) + filepath.Dir(git)
	environment = append(environment, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0", "GIT_ATTR_NOSYSTEM=1")
	f.worktree = &teamWorktreeFixture{repository: workspace, agentDir: agentDir, git: git, environment: environment}
	arguments := []string{"--no-extensions", "-e", extension, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--session-dir", filepath.Join(directory, "parent-sessions"), "--approve", "--offline", "--model", "fixture/parent_team_worktree", "--mode", "json", "--print"}
	firstArgs := append(append([]string(nil), arguments...), "TEAM_WORKTREE_FIRST PARENT_PRIVATE_MARKER")
	evidence := map[string]any{"repository": workspace, "agentDir": agentDir, "firstArgv": append([]string{pig}, firstArgs...), "stdout": stdout.Name(), "stderr": stderr.Name()}
	defer func() {
		f.mu.Lock()
		evidence["gitArgv"] = f.worktree.gitCommands
		f.mu.Unlock()
		if runErr != nil {
			evidence["error"] = runErr.Error()
		}
		if err := writeJSON(filepath.Join(directory, "team-worktree.json"), evidence); err != nil {
			runErr = errors.Join(runErr, fmt.Errorf("write isolated worktree evidence: %w", err))
		}
	}()
	for _, args := range [][]string{
		{"-c", "init.defaultBranch=main", "init", "--quiet"},
		{"add", "--all"},
		{"-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "-c", "commit.gpgSign=false", "commit", "--quiet", "-m", "fixture base"},
	} {
		if _, err := f.worktreeGit(ctx, workspace, args...); err != nil {
			return err
		}
	}
	baseCommit, err := f.worktreeGit(ctx, workspace, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		return err
	}
	commonDir, err := f.worktreeGit(ctx, workspace, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	f.mu.Lock()
	f.worktree.baseCommit, f.worktree.commonDir = baseCommit, commonDir
	f.mu.Unlock()
	run := func(args []string) error {
		command := exec.CommandContext(ctx, pig, args...)
		prepareProcess(command)
		defer stopProcess(command)
		command.Dir, command.Env = workspace, environment
		command.Stdout, command.Stderr = stdout, stderr
		if err := command.Run(); err != nil {
			return fmt.Errorf("isolated worktree host exit: %w", err)
		}
		f.mu.Lock()
		defer f.mu.Unlock()
		return f.failure
	}
	if err := run(firstArgs); err != nil {
		return err
	}
	f.worktree.leadMu.Lock()
	stage := f.worktree.stage
	f.worktree.leadMu.Unlock()
	if stage != 3 {
		return fmt.Errorf("first isolated worktree run stopped at stage %d", stage)
	}
	f.mu.Lock()
	ref, policy, memberSaw, memberWrote := f.worktree.ref, f.worktree.workspace, f.worktree.memberSaw, f.worktree.memberWrote
	f.mu.Unlock()
	if !memberSaw || !memberWrote {
		return errors.New("isolated teammate did not list its identity and write")
	}
	evidence["member"], evidence["workspace"] = ref, policy
	rootSession, err := nativeSessionPath(filepath.Join(directory, "parent-sessions"), ref.Team.Root, workspace)
	if err != nil {
		return err
	}
	memberSession, err := nativeSessionPath(filepath.Join(agentDir, "sessions"), ref.ID, policy.Cwd)
	if err != nil {
		return err
	}
	evidence["rootSession"], evidence["memberSession"] = rootSession, memberSession
	rosterPath, err := f.checkWorktreeRoster(agentDir, ref, policy)
	if err != nil {
		return err
	}
	evidence["rosterPath"] = rosterPath
	if err := f.checkWorktreeDisk(ctx, ref, policy); err != nil {
		return fmt.Errorf("before root reopen: %w", err)
	}
	before := f.countRequests("team_isolated", "")
	if before != 3 {
		return fmt.Errorf("isolated member made %d provider requests, want 3", before)
	}
	reopenArgs := append(append([]string(nil), arguments...), "--session", ref.Team.Root, "ROOT_TEAM_WORKTREE_RESTART")
	evidence["reopenArgv"] = append([]string{pig}, reopenArgs...)
	if err := run(reopenArgs); err != nil {
		return err
	}
	f.worktree.leadMu.Lock()
	done := f.worktree.done
	f.worktree.leadMu.Unlock()
	after := f.countRequests("team_isolated", "")
	evidence["memberRequestsBeforeReopen"], evidence["memberRequestsAfterReopen"] = before, after
	if !done || after != before {
		return fmt.Errorf("root reopen changed isolated membership or started a provider request: done=%t, before=%d, after=%d", done, before, after)
	}
	if _, err := f.checkWorktreeRoster(agentDir, ref, policy); err != nil {
		return fmt.Errorf("after root reopen: %w", err)
	}
	if err := f.checkWorktreeDisk(ctx, ref, policy); err != nil {
		return fmt.Errorf("after root reopen: %w", err)
	}
	if reopenedSession, err := nativeSessionPath(filepath.Join(agentDir, "sessions"), ref.ID, policy.Cwd); err != nil || reopenedSession != memberSession {
		return fmt.Errorf("member native session changed after reopen: %q, %v", reopenedSession, err)
	}
	evidence["passed"] = true
	return nil
}
