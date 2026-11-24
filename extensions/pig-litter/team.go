package piglitter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
)

type TeamID struct {
	Root  ChildID `json:"root"`
	Epoch uint64  `json:"epoch"`
}

type MemberRef struct {
	Team       TeamID  `json:"team"`
	ID         ChildID `json:"id"`
	Generation uint64  `json:"generation"`
}

type WorkspacePolicy struct {
	Kind         string `json:"kind"`
	Cwd          string `json:"cwd"`
	Repository   string `json:"repository,omitempty"`
	GitCommonDir string `json:"gitCommonDir,omitempty"`
	AllocationID string `json:"allocationId,omitempty"`
	Branch       string `json:"branch,omitempty"`
	BaseCommit   string `json:"baseCommit,omitempty"`
	Phase        string `json:"phase,omitempty"`
}

type teamMember struct {
	Ref         MemberRef       `json:"ref"`
	AdmissionID string          `json:"admissionId"`
	Name        string          `json:"name"`
	Workspace   WorkspacePolicy `json:"workspace"`
	State       string          `json:"state"`
}

var teamNamePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)
var teamAddressPattern = regexp.MustCompile(`^a(?:[a-z0-9_-]{1,63}-)?[0-9a-f]{16}$`)
var gitObjectID = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

func validateTeamName(name string) error {
	lower := strings.ToLower(name)
	if !teamNamePattern.MatchString(name) || teamAddressPattern.MatchString(lower) || childUUID.MatchString(lower) {
		return reject("rejected", "invalid teammate name")
	}
	switch lower {
	case "main", "team-lead", "user", "system":
		return reject("rejected", "reserved teammate name")
	}
	return nil
}

func teamGit(ctx context.Context, cwd string, args ...string) (string, error) {
	command := exec.CommandContext(ctx, "git", append([]string{"-C", cwd}, args...)...)
	var stderr bytes.Buffer
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		detail, _ := bounded(strings.TrimSpace(stderr.String()), 1024)
		return "", fmt.Errorf("git %s failed: %w: %s", args[0], err, detail)
	}
	return strings.TrimSuffix(string(output), "\n"), nil
}

func canonicalTeamDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || strings.ContainsAny(resolved, "\r\n") {
		return "", errors.New("invalid Git workspace directory")
	}
	return filepath.Clean(resolved), nil
}

func teamWorktreeIntent(ctx context.Context, store *teamStore, cwd string, id ChildID) (WorkspacePolicy, error) {
	root, err := teamGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return WorkspacePolicy{}, err
	}
	root, err = canonicalTeamDirectory(root)
	if err != nil {
		return WorkspacePolicy{}, err
	}
	common, err := teamGit(ctx, root, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return WorkspacePolicy{}, err
	}
	common, err = canonicalTeamDirectory(common)
	if err != nil {
		return WorkspacePolicy{}, err
	}
	base, err := teamGit(ctx, root, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || !gitObjectID.MatchString(base) {
		return WorkspacePolicy{}, errors.New("isolated workspace requires a committed Git HEAD")
	}
	directory, err := canonicalTeamDirectory(store.dir)
	if err != nil {
		return WorkspacePolicy{}, err
	}
	return WorkspacePolicy{Kind: "isolated_worktree", Cwd: filepath.Join(directory, "worktrees", string(id)), Repository: root, GitCommonDir: common, AllocationID: string(id), Branch: "pig-litter/" + string(store.root) + "/" + string(id), BaseCommit: base, Phase: "intent"}, nil
}

func teamWorktreeRecords(ctx context.Context, workspace WorkspacePolicy) (map[string]string, error) {
	output, err := teamGit(ctx, workspace.Repository, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	records := map[string]string{}
	var path, branch string
	for _, field := range strings.Split(output, "\x00") {
		if field == "" {
			if path != "" {
				if _, exists := records[path]; exists {
					return nil, errors.New("Git reported duplicate worktree paths")
				}
				records[path] = branch
			}
			path, branch = "", ""
		} else if value, ok := strings.CutPrefix(field, "worktree "); ok {
			path = value
		} else if value, ok := strings.CutPrefix(field, "branch "); ok {
			branch = value
		}
	}
	return records, nil
}

func verifyTeamWorktree(ctx context.Context, workspace WorkspacePolicy) error {
	info, err := os.Lstat(workspace.Cwd)
	if err != nil || !info.IsDir() {
		return errors.New("isolated workspace directory is absent or ambiguous")
	}
	cwd, err := canonicalTeamDirectory(workspace.Cwd)
	if err != nil || cwd != workspace.Cwd {
		return errors.New("isolated workspace path changed")
	}
	root, err := teamGit(ctx, cwd, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	root, err = canonicalTeamDirectory(root)
	if err != nil || root != cwd {
		return errors.New("isolated workspace Git root changed")
	}
	common, err := teamGit(ctx, cwd, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	common, err = canonicalTeamDirectory(common)
	if err != nil || common != workspace.GitCommonDir {
		return errors.New("isolated workspace repository changed")
	}
	branch, err := teamGit(ctx, cwd, "symbolic-ref", "--quiet", "HEAD")
	if err != nil || branch != "refs/heads/"+workspace.Branch {
		return errors.New("isolated workspace branch changed")
	}
	records, err := teamWorktreeRecords(ctx, workspace)
	if err != nil {
		return err
	}
	if records[cwd] != branch {
		return errors.New("isolated workspace ownership is ambiguous")
	}
	return nil
}

func prepareTeamWorktree(ctx context.Context, workspace WorkspacePolicy) error {
	parent := filepath.Dir(workspace.Cwd)
	if err := os.Mkdir(parent, 0o700); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	if err := checkDir(parent, true); err != nil {
		return err
	}
	if _, err := os.Lstat(workspace.Cwd); !errors.Is(err, os.ErrNotExist) {
		return errors.New("isolated workspace path already exists or is unavailable")
	}
	if _, err := teamGit(ctx, workspace.Repository, "worktree", "add", "-b", workspace.Branch, "--", workspace.Cwd, workspace.BaseCommit); err != nil {
		return err
	}
	return verifyTeamWorktree(ctx, workspace)
}

func removeTeamWorktree(ctx context.Context, workspace WorkspacePolicy) error {
	_, err := os.Lstat(workspace.Cwd)
	if errors.Is(err, os.ErrNotExist) {
		records, listErr := teamWorktreeRecords(ctx, workspace)
		if listErr != nil {
			return listErr
		}
		if _, exists := records[workspace.Cwd]; exists {
			return errors.New("missing isolated workspace is still registered with Git")
		}
		return nil
	}
	if err != nil {
		return err
	}
	if err := verifyTeamWorktree(ctx, workspace); err != nil {
		return err
	}
	head, err := teamGit(ctx, workspace.Cwd, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil || head != workspace.BaseCommit {
		return errors.New("isolated workspace contains commits or has an ambiguous HEAD")
	}
	status, err := teamGit(ctx, workspace.Cwd, "status", "--porcelain=v1", "-z", "--untracked-files=all", "--ignored=matching", "--ignore-submodules=none")
	if err != nil {
		return err
	}
	if status != "" {
		return errors.New("isolated workspace contains changes")
	}
	_, err = teamGit(ctx, workspace.Repository, "worktree", "remove", "--", workspace.Cwd)
	if err != nil {
		return err
	}
	if _, err := os.Lstat(workspace.Cwd); !errors.Is(err, os.ErrNotExist) {
		return errors.New("isolated workspace removal is uncertain")
	}
	records, err := teamWorktreeRecords(ctx, workspace)
	if err != nil {
		return err
	}
	if _, exists := records[workspace.Cwd]; exists {
		return errors.New("isolated workspace remains registered with Git")
	}
	return nil
}

func (o *owner) teamStoreForRoot() (*teamStore, error) {
	o.teamMu.Lock()
	defer o.teamMu.Unlock()
	if o.teamStore != nil {
		return o.teamStore, nil
	}
	store, err := openTeamStore(o.agentDir, ChildID(o.sessionID))
	if err != nil {
		return nil, err
	}
	o.teamStore = store
	return store, nil
}

func (o *owner) restoreTeamStore() error {
	if !childUUID.MatchString(o.sessionID) {
		return nil
	}
	path := filepath.Join(o.agentDir, "pig-litter", "roots", o.sessionID)
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	_, err := o.teamStoreForRoot()
	return err
}

func (o *owner) closeTeamStore() (bool, error) {
	o.teamMu.Lock()
	defer o.teamMu.Unlock()
	if o.teamStore == nil {
		return false, nil
	}
	err := o.teamStore.Close()
	held := o.teamStore.locksHeld()
	if !held {
		o.teamStore = nil
	}
	if held && err == nil {
		err = errors.New("team store locks remain held")
	}
	return held, err
}

func (o *owner) cancelTeamMember(r *record) {
	_ = o.rollbackTeamMember(r.team)
}

func (o *owner) finishTeamMember(r *record, selected *run, outcome Outcome) {
	r.teamStateMu.Lock()
	defer r.teamStateMu.Unlock()
	o.tree.mu.Lock()
	current := o.tree.records[r.id] == r && r.run == selected
	paused := r.paused
	o.tree.mu.Unlock()
	if !current {
		return
	}
	store, err := o.teamStoreForRoot()
	if err != nil {
		return
	}
	state := "unavailable"
	if outcome.State == Completed {
		state = "idle"
	}
	if paused || outcome.State == Paused {
		state = "paused"
	}
	if err := store.SetState(r.team.Ref, state); err != nil {
		_ = o.rollbackTeamMember(r.team)
	}
}

func (o *owner) pauseTeamMember(r *record, selected *run) error {
	r.teamStateMu.Lock()
	defer r.teamStateMu.Unlock()
	o.tree.mu.Lock()
	current := o.tree.records[r.id] == r && r.run == selected && r.paused && r.team != nil
	var ref MemberRef
	if current {
		ref = r.team.Ref
	}
	o.tree.mu.Unlock()
	if !current {
		return reject("unavailable", "teammate pause is no longer current")
	}
	store, err := o.teamStoreForRoot()
	if err != nil {
		return err
	}
	_, err = store.PauseMember(ref)
	return err
}

func (o *owner) rollbackTeamMember(member *teamMember) error {
	store, err := o.teamStoreForRoot()
	if err != nil {
		return err
	}
	reservations, err := store.Reservations()
	if err != nil {
		return err
	}
	var pending *teamMember
	for _, candidate := range reservations {
		if candidate.Ref == member.Ref && candidate.AdmissionID == member.AdmissionID {
			copy := candidate
			pending = &copy
			break
		}
	}
	if pending == nil {
		return errors.New("team admission is no longer provisional")
	}
	if pending.Workspace.Kind == "isolated_worktree" && pending.Workspace.Phase != "released" {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := removeTeamWorktree(ctx, pending.Workspace); err != nil {
			return fmt.Errorf("preserved isolated workspace %s: %w", pending.Workspace.Cwd, err)
		}
		if _, err := store.SetWorkspacePhase(pending.Ref, "released"); err != nil {
			return err
		}
	}
	return store.Rollback(member.AdmissionID)
}

func (o *owner) cancelTeamStart(selected *run, cause error) error {
	member := selected.owner.team
	o.tree.cancelPending(selected)
	store, err := o.teamStoreForRoot()
	if err != nil {
		return fmt.Errorf("%w; isolated workspace recovery is required at %s", cause, member.Workspace.Cwd)
	}
	reservations, err := store.Reservations()
	if err != nil {
		return fmt.Errorf("%w; isolated workspace recovery is required at %s", cause, member.Workspace.Cwd)
	}
	for _, pending := range reservations {
		if pending.Ref == member.Ref && pending.AdmissionID == member.AdmissionID {
			return fmt.Errorf("%w; retained isolated workspace at %s", cause, member.Workspace.Cwd)
		}
	}
	return cause
}

func (o *owner) teamResultStored(ticket *callTicket, selected *run) bool {
	input, err := os.Open(selected.teamSessionFile)
	if err != nil {
		return false
	}
	defer input.Close()
	reader := bufio.NewReaderSize(input, 64*1024)
	root := false
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) || len(line) == 0 {
			return false
		}
		var entry struct {
			Type    string `json:"type"`
			ID      string `json:"id"`
			Message struct {
				Role       string          `json:"role"`
				ToolCallID string          `json:"toolCallId"`
				ToolName   string          `json:"toolName"`
				IsError    bool            `json:"isError"`
				Content    json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &entry) != nil {
			return false
		}
		if entry.Type == "session" {
			root = entry.ID == o.sessionID
			continue
		}
		if !root || entry.Message.Role != "toolResult" || entry.Message.ToolName != "team_spawn" || entry.Message.ToolCallID != ticket.callID || entry.Message.IsError {
			continue
		}
		var content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(entry.Message.Content, &content) != nil {
			return false
		}
		for _, block := range content {
			if block.Type == "text" && sha256.Sum256([]byte(block.Text)) == selected.teamResult {
				return true
			}
		}
	}
}

func (o *owner) activateTeamWhenStored(ticket *callTicket, selected *run) {
	defer o.ackWG.Done()
	ctx, cancel := context.WithTimeout(o.tree.ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if o.teamResultStored(ticket, selected) {
			if err := o.tree.activate(selected); err != nil {
				o.tree.cancelPending(selected)
			}
			return
		}
		select {
		case <-ctx.Done():
			o.tree.cancelPending(selected)
			return
		case <-ticker.C:
		}
	}
}

type teamAdmissionGate struct {
	mu        sync.Mutex
	owner     *owner
	record    *record
	run       *run
	seen      bool
	stop      ai.StopReason
	calls     map[string]string
	committed bool
}

func (g *teamAdmissionGate) observe(message *agent.AssistantMessage) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.seen || message == nil {
		return
	}
	message = message.Observe()
	g.seen, g.stop = true, message.StopReason
	g.calls = map[string]string{}
	for _, block := range message.Content {
		switch call := block.(type) {
		case ai.ToolCall:
			g.calls[call.ID] = call.Name
		case *ai.ToolCall:
			g.calls[call.ID] = call.Name
		}
	}
}

func (g *teamAdmissionGate) commit(ctx context.Context) error {
	g.record.teamStateMu.Lock()
	defer g.record.teamStateMu.Unlock()
	if err := g.owner.current(); err != nil {
		return err
	}
	g.owner.tree.mu.Lock()
	r, err := g.owner.tree.currentLocked(g.run)
	if err == nil && (r != g.record || r.paused || g.run.ctx.Err() != nil) {
		err = reject("unavailable", "teammate execution is no longer current")
	}
	if err == nil && !r.readOnly {
		for _, other := range g.owner.tree.records {
			if other != r && !other.readOnly && other.cwd == r.cwd && !other.run.state.terminal() && (other.team == nil || other.team.Ref.Team != r.team.Ref.Team || other.team.Workspace.Kind != "shared_checkout" || other.team.Workspace.Cwd != r.cwd) {
				err = reject("rejected", "writer conflict in the shared working directory")
				break
			}
		}
	}
	g.owner.tree.mu.Unlock()
	if err != nil {
		return err
	}
	if g.record.team.Workspace.Kind == "isolated_worktree" {
		if err := verifyTeamWorktree(ctx, g.record.team.Workspace); err != nil {
			return err
		}
	}
	store, err := g.owner.teamStoreForRoot()
	if err != nil {
		return err
	}
	member, err := store.Commit(g.record.team.AdmissionID)
	if err != nil {
		return err
	}
	if member.Ref != g.record.team.Ref {
		return reject("unavailable", "committed teammate identity changed")
	}
	g.committed = true
	return nil
}

func (g *teamAdmissionGate) before(ctx context.Context, id, name string, _ json.RawMessage) agent.ToolCallHookResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.committed {
		return agent.ToolCallHookResult{}
	}
	if !g.seen || g.stop != ai.StopReasonToolUse || g.calls[id] != name {
		return agent.ToolCallHookResult{Block: true, Reason: "teammate admission could not verify the first provider response"}
	}
	if err := g.commit(ctx); err != nil {
		return agent.ToolCallHookResult{Block: true, Reason: err.Error()}
	}
	return agent.ToolCallHookResult{}
}

func (g *teamAdmissionGate) finish(outcome *Outcome) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.committed || outcome.State != Completed || !g.seen || g.stop != ai.StopReasonStop || len(g.calls) != 0 {
		return
	}
	if err := g.commit(g.run.ctx); err != nil {
		outcome.State, outcome.Error = Unavailable, err.Error()
	}
}
