package piglitter

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"unicode"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/MichaelKinsy/PiG/tui/widthx"
)

var controlNames = []string{"litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}
var teamToolNames = []string{"team_spawn", "team_list", "team_send", "team_task_create", "team_task_list", "team_task_update"}
var descriptions = map[string]string{
	"team_task_create": "Create a shared task with optional dependencies.",
	"team_task_list":   "List shared tasks and whether their dependencies block them.",
	"team_task_update": "Claim, assign, complete, or delete a shared task.",
	"team_spawn":       "Start a named teammate with an explicit workspace policy.",
	"team_list":        "List the team and classify the caller's inbox receipts.",
	"team_send":        "Send an ordinary message to a named teammate.",
	"litter_spawn":     "Start a background child. Use litter_list to discover named agent types.",
	"litter_list":      "List available agent types and active immediate children.",
	"litter_inspect":   "Get a child's current status and latest result by ID.",
	"litter_message":   "Steer a live child or explicitly resume a settled child with the same ID.",
	"litter_stop":      "Pause an immediate child and its descendants. Resume each node explicitly with litter_message.",
	"litter_wait":      "Wait for a child result by ID. Outcome text is the child assistant's report. A timeout leaves the child running.",
}

var promptGuidelines = map[string][]string{
	"litter_spawn":   {"Use litter_list to discover agent types. Give each child a self-contained task and save its ID for controls. cwd defaults to the caller's directory and relative cwd paths resolve there."},
	"litter_inspect": {"Inspect returns current status and the latest child report. Use the same ID after resume."},
	"litter_wait":    {"Partial or failed outcomes remain failures even if later tools succeeded. Automatic litter_completion notices are status updates from Pig Litter and need no duplicate acknowledgment."},
}

func object(required []string, properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}
func text(maximum int) map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": maximum}
}
func integer(minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}

var taskIDSchema = object([]string{"team", "number"}, map[string]any{
	"team":   object([]string{"root", "epoch"}, map[string]any{"root": text(128), "epoch": map[string]any{"type": "integer", "minimum": 1}}),
	"number": map[string]any{"type": "integer", "minimum": 1},
})

var schemas = map[string]map[string]any{
	"team_task_create": object([]string{"subject"}, map[string]any{"subject": text(256), "description": map[string]any{"type": "string", "maxLength": 16384}, "blockedBy": map[string]any{"type": "array", "maxItems": 128, "items": taskIDSchema}, "metadata": map[string]any{"type": "object", "maxProperties": 64, "additionalProperties": map[string]any{"type": "string", "maxLength": 4096}}}),
	"team_task_list":   object([]string{}, map[string]any{}),
	"team_task_update": object([]string{"taskId", "action"}, map[string]any{"taskId": taskIDSchema, "action": map[string]any{"type": "string", "enum": []string{"claim", "assign", "complete", "delete"}}, "assignee": text(64)}),
	"team_spawn":       object([]string{"type", "task", "name", "workspace"}, map[string]any{"type": text(64), "task": text(16384), "name": text(64), "model": text(256), "workspace": object([]string{"kind"}, map[string]any{"kind": map[string]any{"type": "string", "enum": []string{"shared_checkout", "isolated_worktree"}}})}),
	"team_list":        object([]string{}, map[string]any{}),
	"team_send":        object([]string{"recipient", "messageId", "text", "summary"}, map[string]any{"recipient": text(64), "messageId": text(128), "text": text(16384), "summary": text(256)}),
	"litter_spawn":     object([]string{"type", "task"}, map[string]any{"type": text(64), "task": text(16384), "name": text(128), "model": text(256), "cwd": text(4096)}),
	"litter_list":      object([]string{}, map[string]any{"offset": integer(0, 1000000), "limit": integer(1, 50), "agentOffset": integer(0, 1000000), "agentLimit": integer(1, 50)}),
	"litter_inspect":   object([]string{"id"}, map[string]any{"id": text(128)}),
	"litter_message":   object([]string{"id", "text"}, map[string]any{"id": text(128), "text": text(16384), "mode": map[string]any{"type": "string", "enum": []string{"steer", "follow_up"}}, "resume": map[string]any{"type": "boolean"}}),
	"litter_stop":      object([]string{"id"}, map[string]any{"id": text(128)}),
	"litter_wait":      object([]string{"id"}, map[string]any{"id": text(128), "timeoutMs": integer(1, 300000)}),
}

type operationArgs struct {
	ID                     ChildID
	Type, Task, Text, Mode string
	Name                   *string
	Model                  string
	Cwd                    string
	Offset                 int
	Limit                  *int
	AgentOffset            int
	AgentLimit             *int
	Resume                 bool
	TimeoutMs              *int
	Workspace              *WorkspacePolicy
	TaskID                 TaskID `json:"taskId"`
	Subject                string
	Description            string
	BlockedBy              []TaskID `json:"blockedBy"`
	Metadata               map[string]string
	Action                 string
	Assignee               string
}

func pageLimit(request *int, fallback int) int {
	if request == nil {
		return fallback
	}
	return *request
}

func safeLabel(value string) (string, error) {
	if len(value) > 128 {
		return "", reject("rejected", "child name must be at most 128 bytes")
	}
	label := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, widthx.StripTerminalSequences(value))
	label = strings.Join(strings.Fields(label), " ")
	if label == "" {
		return "", reject("rejected", "child name must contain visible text")
	}
	return label, nil
}

func (o *owner) operation(ctx context.Context, raw json.RawMessage, key *callTicket, toolContext *sdk.Context) (map[string]any, error) {
	if err := o.current(); err != nil {
		return nil, err
	}
	var args operationArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, reject("rejected", "invalid tool arguments")
	}
	name, caller := key.operation, key.caller
	activationContext := ctx
	if toolContext != nil {
		activationContext = toolContext.Signal()
		if activationContext == nil {
			activationContext = o.tree.ctx
		}
	}
	var parent *record
	if caller != nil {
		o.tree.mu.Lock()
		var err error
		parent, err = o.tree.callerLocked(caller)
		o.tree.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	if name == "team_send" {
		if caller == nil || parent == nil || parent.team == nil {
			return nil, reject("permission_denied", "a registered teammate is required")
		}
		request, err := parsePeerRequest(raw)
		if err != nil {
			return nil, err
		}
		o.teamMu.Lock()
		store := o.teamStore
		o.teamMu.Unlock()
		if store == nil {
			return nil, reject("unavailable", "team store is unavailable")
		}
		receipt, _, err := store.acceptPeer(parent.team.Ref, request)
		if err != nil {
			return nil, err
		}
		if receipt.Envelope.Recipient == teamLeadRef(receipt.Envelope.Team) {
			if receipt.State != peerObserved {
				o.sendRootPeers(ctx, store, receipt.Envelope.Team)
			}
		} else if receipt.State == peerAccepted {
			o.requestPeerWake(receipt.Envelope.Recipient)
		}
		delivery := string(receipt.State)
		if receipt.State == peerObserved {
			delivery = "unverified"
		}
		return map[string]any{"state": "accepted", "messageId": request.MessageID, "delivery": delivery}, nil
	}
	if strings.HasPrefix(name, "team_task_") {
		if caller != nil && (parent == nil || parent.team == nil) {
			return nil, reject("permission_denied", "team membership is required")
		}
		store, err := o.teamStoreForRoot()
		if err != nil {
			return nil, err
		}
		switch name {
		case "team_task_create":
			task, err := store.createTask(args.Subject, args.Description, args.BlockedBy, args.Metadata)
			return map[string]any{"state": "created", "task": task}, err
		case "team_task_list":
			tasks, err := store.listTasks()
			return map[string]any{"state": "listed", "tasks": tasks}, err
		case "team_task_update":
			if args.Action == "complete" && args.Assignee == "" {
				var actor *MemberRef
				if caller != nil {
					actor = &parent.team.Ref
				}
				task, state, err := store.completeTask(args.TaskID, actor)
				return map[string]any{"state": state, "task": task}, err
			}
			if args.Action == "delete" && args.Assignee == "" {
				if caller != nil {
					return nil, reject("permission_denied", "only the team lead can delete a task")
				}
				task, err := store.deleteTask(args.TaskID)
				return map[string]any{"state": "deleted", "task": task}, err
			}
			var member MemberRef
			explicit := args.Action == "assign"
			if explicit {
				if caller != nil {
					return nil, reject("permission_denied", "only the team lead can assign a task")
				}
				_, members, err := store.Snapshot()
				if err != nil {
					return nil, err
				}
				for _, candidate := range members {
					if candidate.Name == args.Assignee {
						member = candidate.Ref
						break
					}
				}
				if member.ID == "" {
					return nil, reject("rejected", "assignee is not a committed teammate")
				}
			} else if args.Action == "claim" && caller != nil && args.Assignee == "" {
				member = parent.team.Ref
			} else {
				return nil, reject("rejected", "a teammate must claim for itself or the lead must assign a task")
			}
			task, state, err := store.claimTask(args.TaskID, member, explicit)
			return map[string]any{"state": state, "task": task}, err
		}
		return nil, reject("rejected", "unknown task operation")
	}
	if name == "team_list" {
		if caller != nil && (parent == nil || parent.team == nil) {
			return nil, reject("permission_denied", "team membership is required")
		}
		store, err := o.teamStoreForRoot()
		if err != nil {
			return nil, err
		}
		var team TeamID
		var members, pending []teamMember
		if caller == nil {
			team, members, pending, err = store.SnapshotWithReservations()
		} else {
			team, members, err = store.Snapshot()
		}
		if err != nil {
			return nil, err
		}
		roster := []map[string]any{{"ref": teamLeadRef(team), "name": "team-lead", "role": "lead", "state": "running", "cwd": o.cwd}}
		for _, member := range members {
			roster = append(roster, map[string]any{"ref": member.Ref, "name": member.Name, "role": "teammate", "state": member.State, "cwd": member.Workspace.Cwd, "workspace": member.Workspace})
		}
		recipient := teamLeadRef(team)
		if caller != nil {
			recipient = parent.team.Ref
		}
		inbox, err := store.peerInbox(recipient)
		if err != nil {
			return nil, err
		}
		result := map[string]any{"state": "listed", "team": team, "members": roster, "inbox": inbox}
		if caller == nil {
			result["pending"] = pending
		}
		return result, nil
	}
	if name == "litter_spawn" || name == "team_spawn" {
		isTeam := name == "team_spawn"
		if isTeam && caller != nil {
			return nil, reject("lead_required", "only the team lead can start a teammate")
		}
		if isTeam && (args.Name == nil || args.Workspace == nil || *args.Workspace != (WorkspacePolicy{Kind: args.Workspace.Kind}) || args.Workspace.Kind != "shared_checkout" && args.Workspace.Kind != "isolated_worktree") {
			return nil, reject("rejected", "a teammate needs a name and explicit workspace policy")
		}
		base := o.cwd
		if parent != nil {
			base = parent.cwd
		}
		cwd, err := childDirectory(base, args.Cwd)
		if err != nil {
			return nil, err
		}
		config, err := LoadConfig(cwd)
		if err != nil {
			return nil, err
		}
		agent, exists := config.Agents[args.Type]
		if !exists || strings.TrimSpace(args.Task) == "" || len(args.Task) > 16384 {
			return nil, reject("rejected", "choose a named agent and a bounded task")
		}
		if agent.Model != "" && args.Model != "" && args.Model != agent.Model {
			return nil, reject("permission_denied", "the named agent has a fixed model")
		}
		model := agent.Model
		if model == "" {
			model = args.Model
		}
		if model == "" && parent != nil {
			model = parent.model
		}
		if model == "" {
			model = o.context.ModelQualified()
		}
		if _, err := o.modelFor(ctx, model, o.services.ModelRuntime()); err != nil {
			return nil, err
		}
		label := ""
		if args.Name != nil {
			if isTeam {
				label = *args.Name
				err = validateTeamName(label)
			} else {
				label, err = safeLabel(*args.Name)
			}
			if err != nil {
				return nil, err
			}
		}
		available, err := o.rootToolNames()
		if err != nil {
			return nil, err
		}
		var member *teamMember
		var store *teamStore
		var teamSessionFile string
		if isTeam {
			if toolContext == nil {
				return nil, reject("unavailable", "team admission needs a retained root session")
			}
			file, fileErr := toolContext.SessionManager().GetSessionFile()
			if fileErr != nil || file == nil || *file == "" {
				return nil, reject("unavailable", "team admission needs a retained root session file")
			}
			teamSessionFile = *file
			store, err = o.teamStoreForRoot()
			if err != nil {
				return nil, err
			}
			id, idErr := ai.UUIDv7(nil)
			operation, operationErr := ai.UUIDv7(nil)
			if idErr != nil || operationErr != nil {
				return nil, reject("unavailable", "cannot allocate teammate identity")
			}
			workspace := WorkspacePolicy{Kind: "shared_checkout", Cwd: cwd}
			if args.Workspace.Kind == "isolated_worktree" {
				workspace, err = teamWorktreeIntent(ctx, store, cwd, ChildID(id))
				if err != nil {
					return nil, err
				}
			}
			reserved, reserveErr := store.Reserve(ChildID(id), operation, label, workspace)
			if reserveErr != nil {
				return nil, reserveErr
			}
			member, label = &reserved, reserved.Name
			if workspace.Kind == "isolated_worktree" {
				cwd = workspace.Cwd
			}
		}
		snapshot, execution, err := o.tree.admit(admission{parentRun: caller, kind: args.Type, name: label, model: model, cwd: cwd, task: args.Task, agent: agent, available: available, member: member})
		if err != nil {
			if member != nil {
				if cleanupErr := o.rollbackTeamMember(member); cleanupErr != nil {
					return nil, fmt.Errorf("%w; %v", err, cleanupErr)
				}
			}
			return nil, err
		}
		if member != nil && member.Workspace.Kind == "isolated_worktree" {
			if err := prepareTeamWorktree(ctx, member.Workspace); err != nil {
				return nil, o.cancelTeamStart(execution, err)
			}
			prepared, err := store.SetWorkspacePhase(member.Ref, "prepared")
			if err != nil {
				return nil, o.cancelTeamStart(execution, err)
			}
			member = &prepared
			o.tree.mu.Lock()
			execution.owner.team = member
			o.tree.mu.Unlock()
			if err := o.current(); err != nil {
				return nil, o.cancelTeamStart(execution, err)
			}
		}
		if err := o.saveChild(execution.owner); err != nil {
			if member != nil && member.Workspace.Kind == "isolated_worktree" {
				return nil, o.cancelTeamStart(execution, err)
			}
			o.tree.cancelPending(execution)
			return nil, err
		}
		value := map[string]any{"state": snapshot.State, "child": snapshot}
		if member != nil {
			value = map[string]any{"state": "starting", "admission": member.AdmissionID, "member": member.Ref, "name": member.Name, "cwd": member.Workspace.Cwd, "workspace": member.Workspace}
			content, _ := resultContent(name, value, nil)
			execution.teamResult = sha256.Sum256([]byte(content))
			execution.teamSessionFile = teamSessionFile
		}
		o.deferActivation(key, execution, activationContext.Done(), func() bool { return activationContext.Err() != nil }, name)
		if toolContext != nil {
			o.watchRequest(key, *toolContext)
		}
		o.widget()
		return value, nil
	}
	if name == "litter_list" {
		children, err := o.tree.list(caller)
		if err != nil {
			return nil, err
		}
		limit, agentLimit := pageLimit(args.Limit, 20), pageLimit(args.AgentLimit, 20)
		if args.Offset < 0 || args.Offset > 1000000 || args.AgentOffset < 0 || args.AgentOffset > 1000000 || limit < 1 || limit > 50 || agentLimit < 1 || agentLimit > 50 {
			return nil, reject("rejected", "invalid list page")
		}
		directory := o.cwd
		if parent != nil {
			directory = parent.cwd
		}
		config, err := LoadConfig(directory)
		if err != nil {
			return nil, err
		}
		names := make([]string, 0, len(config.Agents))
		for name := range config.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		agents := []map[string]any{}
		for _, name := range names[min(args.AgentOffset, len(names)):min(args.AgentOffset+agentLimit, len(names))] {
			definition := config.Agents[name]
			description := []rune(definition.Instructions)
			description = description[:min(160, len(description))]
			value := map[string]any{"name": name, "role": definition.Role, "description": string(description), "tools": definition.Tools, "canDelegate": definition.CanDelegate}
			if definition.Model != "" {
				value["model"] = definition.Model
			}
			agents = append(agents, value)
		}
		page := children[min(args.Offset, len(children)):min(args.Offset+limit, len(children))]
		return map[string]any{"state": "listed", "children": page, "nextOffset": args.Offset + len(page), "more": len(children) > args.Offset+limit, "totalChildren": len(children), "availableAgentTypes": agents, "nextAgentOffset": args.AgentOffset + len(agents), "moreAgentTypes": len(names) > args.AgentOffset+agentLimit, "totalAgentTypes": len(names)}, nil
	}
	selected, err := o.tree.selectOwned(caller, args.ID)
	if err != nil {
		return nil, err
	}
	if name == "litter_inspect" {
		inspection, err := o.tree.inspect(args.ID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"state": inspection.Child.State, "child": inspection.Child, "outcome": inspection.Outcome}, nil
	}
	if name == "litter_wait" {
		o.beginOutcomeAck(key, selected, name, toolContext)
		outcome, err := o.tree.wait(ctx, selected, pageLimit(args.TimeoutMs, 30000))
		o.readyOutcomeAck(key, err == nil && outcome.State.terminal())
		return map[string]any{"state": outcome.State, "outcome": &outcome}, err
	}
	if name == "litter_stop" {
		o.beginOutcomeAck(key, selected, name, toolContext)
		outcome, count, err := o.tree.stop(caller, selected)
		o.readyOutcomeAck(key, err == nil && outcome != nil && outcome.State.terminal())
		if err != nil {
			return nil, err
		}
		state := outcome.State
		o.tree.mu.Lock()
		if selected.owner.team != nil && selected.owner.paused {
			state = Paused
		}
		o.tree.mu.Unlock()
		return map[string]any{"state": state, "outcome": outcome, "stoppedCount": count}, nil
	}
	if name == "litter_message" {
		mode := args.Mode
		if mode == "" {
			mode = "steer"
		}
		if args.Resume {
			mode = "resume"
			o.tree.mu.Lock()
			target, err := o.tree.currentLocked(selected)
			var model string
			if err == nil {
				model = target.model
			}
			o.tree.mu.Unlock()
			if err != nil {
				return nil, err
			}
			if _, err := o.modelFor(ctx, model, o.services.ModelRuntime()); err != nil {
				return nil, err
			}
			if _, err := o.rootToolNames(); err != nil {
				return nil, err
			}
		}
		snapshot, execution, err := o.tree.message(ctx, caller, selected, mode, args.Text)
		if err != nil {
			return nil, err
		}
		delivery := "queued"
		if mode == "resume" {
			delivery = "resumed"
			o.deferActivation(key, execution, activationContext.Done(), func() bool { return activationContext.Err() != nil }, name)
			if toolContext != nil {
				o.watchRequest(key, *toolContext)
			}
		}
		o.widget()
		return map[string]any{"state": snapshot.State, "child": snapshot, "delivery": delivery}, nil
	}
	return nil, reject("rejected", "unknown child operation")
}

func resultContent(operation string, value map[string]any, err error) (string, bool) {
	value = maps.Clone(value)
	failed := false
	if err != nil {
		code := "failed"
		var treeErr *treeError
		if errors.As(err, &treeErr) {
			code = treeErr.code
		}
		state := "failed"
		switch code {
		case "wait_timeout":
			state = "timed_out"
		case "cancelled":
			state = "cancelled"
		case "unavailable":
			state = "unavailable"
		case "permission_denied", "rejected":
			state = "rejected"
		}
		value = map[string]any{"state": state, "code": code, "report": err.Error()}
		failed = code != "wait_timeout" && code != "cancelled"
	}
	if value == nil {
		value = map[string]any{}
	}
	value["operation"] = operation
	clipped := false
	if outcome, ok := value["outcome"].(*Outcome); ok && outcome != nil {
		clipped = outcome.HandbackTruncated
	}
	if report, ok := value["report"].(string); ok {
		var truncated bool
		value["report"], truncated = bounded(report, 1024)
		clipped = clipped || truncated
	}
	value["handbackTruncated"] = clipped
	data, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		fallback, _ := json.Marshal(map[string]any{"operation": operation, "state": "failed", "code": "failed", "report": "cannot encode child result", "handbackTruncated": false})
		return string(fallback), true
	}
	return string(data), failed
}

func sdkResult(operation string, value map[string]any, err error) sdk.ToolResult {
	text, failed := resultContent(operation, value, err)
	return sdk.ToolResult{Content: text, IsError: failed}
}
func nativeResult(operation string, value map[string]any, err error) agent.AgentToolResult {
	text, failed := resultContent(operation, value, err)
	return agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: text}}, IsError: failed}
}
