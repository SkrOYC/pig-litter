package piglitter

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"os"
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
	Kind string `json:"kind"`
	Cwd  string `json:"cwd"`
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

func (o *owner) closeTeamStore() {
	o.teamMu.Lock()
	defer o.teamMu.Unlock()
	if o.teamStore != nil {
		_ = o.teamStore.Close()
		o.teamStore = nil
	}
}

func (o *owner) cancelTeamMember(r *record) {
	if store, err := o.teamStoreForRoot(); err == nil {
		_ = store.Rollback(r.team.AdmissionID)
	}
}

func (o *owner) finishTeamMember(r *record, _ *run, outcome Outcome) {
	store, err := o.teamStoreForRoot()
	if err != nil {
		return
	}
	state := "unavailable"
	if outcome.State == Completed {
		state = "idle"
	} else if outcome.State == Paused {
		state = "paused"
	}
	if err := store.SetState(r.team.Ref, state); err != nil {
		_ = store.Rollback(r.team.AdmissionID)
	}
}

func (o *owner) teamResultStored(ticket *callTicket, selected *run) bool {
	input, err := os.Open(selected.teamSessionFile)
	if err != nil {
		return false
	}
	defer input.Close()
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 4096), 4*1024*1024)
	root := false
	for scanner.Scan() {
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
		if json.Unmarshal(scanner.Bytes(), &entry) != nil {
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
	return false
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

func (g *teamAdmissionGate) commit() error {
	if err := g.owner.current(); err != nil {
		return err
	}
	g.owner.tree.mu.Lock()
	r, err := g.owner.tree.currentLocked(g.run)
	if err == nil && (r != g.record || g.run.ctx.Err() != nil) {
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

func (g *teamAdmissionGate) before(_ context.Context, id, name string, _ json.RawMessage) agent.ToolCallHookResult {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.committed {
		return agent.ToolCallHookResult{}
	}
	if !g.seen || g.stop != ai.StopReasonToolUse || g.calls[id] != name {
		return agent.ToolCallHookResult{Block: true, Reason: "teammate admission could not verify the first provider response"}
	}
	if err := g.commit(); err != nil {
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
	if err := g.commit(); err != nil {
		outcome.State, outcome.Error = Unavailable, err.Error()
	}
}
