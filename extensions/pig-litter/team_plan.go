package piglitter

import (
	"encoding/json"
	"strings"
	"unicode/utf8"
)

type planCommand struct {
	Action       string        `json:"action"`
	SubmissionID string        `json:"submissionId"`
	Text         string        `json:"text"`
	TimeoutMs    int           `json:"timeoutMs"`
	RequestID    PlanRequestID `json:"requestId"`
	Decision     string        `json:"decision"`
	Reason       string        `json:"reason"`
}

type planView struct {
	Request planRequest `json:"request"`
	Gate    string      `json:"gate"`
}

func parsePlanCommand(raw json.RawMessage) (planCommand, error) {
	var command planCommand
	if len(raw) > 16384 || !utf8.Valid(raw) || checkUniqueJSON(raw) != nil {
		return command, reject("rejected", "invalid plan arguments")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil || json.Unmarshal(fields["action"], &command.Action) != nil {
		return command, reject("rejected", "invalid plan action")
	}
	keys := []string{"action"}
	switch command.Action {
	case "submit":
		keys = append(keys, "submissionId", "text", "timeoutMs")
	case "get", "cancel":
		keys = append(keys, "requestId")
	case "list":
	case "decide":
		keys = append(keys, "requestId", "decision")
		var decision string
		if json.Unmarshal(fields["decision"], &decision) != nil {
			return command, reject("rejected", "invalid plan decision")
		}
		if decision == "reject" {
			keys = append(keys, "reason")
		} else if decision != "approve" {
			return command, reject("rejected", "invalid plan decision")
		}
	default:
		return command, reject("rejected", "unknown plan action")
	}
	if _, err := exactJSONObject(raw, keys...); err != nil || json.Unmarshal(raw, &command) != nil {
		return command, reject("rejected", "invalid action-specific plan arguments")
	}
	if command.Action == "submit" {
		if !peerMessageIDPattern.MatchString(command.SubmissionID) || strings.TrimSpace(command.Text) == "" || len(command.Text) > 4096 || command.TimeoutMs < 1000 || command.TimeoutMs > 3600000 {
			return command, reject("rejected", "invalid plan submission")
		}
	}
	if command.Action == "get" || command.Action == "cancel" || command.Action == "decide" {
		request, err := exactJSONObject(fields["requestId"], "member", "sequence")
		if err != nil {
			return command, reject("rejected", "invalid plan request identity")
		}
		member, err := exactJSONObject(request["member"], "team", "id", "generation")
		if err != nil {
			return command, reject("rejected", "invalid plan member identity")
		}
		if _, err := exactJSONObject(member["team"], "root", "epoch"); err != nil || command.RequestID.Sequence == 0 || command.RequestID.Member.Generation == 0 || command.RequestID.Member.Team.Epoch == 0 || !childUUID.MatchString(string(command.RequestID.Member.ID)) || !childUUID.MatchString(string(command.RequestID.Member.Team.Root)) {
			return command, reject("rejected", "invalid plan member identity")
		}
	}
	if command.Action == "decide" && command.Decision == "reject" && (strings.TrimSpace(command.Reason) == "" || len(command.Reason) > 4096) {
		return command, reject("rejected", "a plan rejection needs a reason")
	}
	return command, nil
}

func (o *owner) planOperation(raw json.RawMessage, caller *run, parent *record) (map[string]any, error) {
	command, err := parsePlanCommand(raw)
	if err != nil {
		return nil, err
	}
	if caller != nil && (parent == nil || parent.team == nil) {
		return nil, reject("permission_denied", "a registered teammate is required")
	}
	if caller != nil && command.Action == "decide" {
		return nil, reject("permission_denied", "only the team lead can decide a plan")
	}
	if caller == nil && command.Action == "submit" {
		return nil, reject("permission_denied", "only a teammate can submit a plan")
	}
	if caller != nil && command.Action != "submit" && command.Action != "list" && command.RequestID.Member != parent.team.Ref {
		return nil, reject("permission_denied", "the plan belongs to another teammate")
	}
	if caller != nil && !parent.team.PlanRequired {
		return map[string]any{"state": "plan_not_required", "gate": "not_required"}, nil
	}
	o.tree.lifecycleMu.Lock()
	defer o.tree.lifecycleMu.Unlock()
	if err := o.current(); err != nil {
		return nil, err
	}
	o.tree.mu.Lock()
	if caller != nil {
		current, err := o.tree.callerLocked(caller)
		if err != nil || current != parent || current.team == nil {
			o.tree.mu.Unlock()
			return nil, reject("unavailable", "the teammate execution is no longer current")
		}
	}
	var execution string
	if command.Action == "decide" {
		member := o.tree.records[command.RequestID.Member.ID]
		if o.tree.closed || member == nil || member.team == nil || member.team.Ref != command.RequestID.Member || member.paused || member.run == nil || member.run.state != Starting && member.run.state != Running {
			o.tree.mu.Unlock()
			return nil, reject("unavailable", "the plan requester is no longer active")
		}
		execution = member.run.noticeID
	}
	o.tree.mu.Unlock()
	store, err := o.teamStoreForRoot()
	if err != nil {
		return nil, err
	}
	switch command.Action {
	case "submit":
		request, err := store.submitPlan(parent.team.Ref, caller.noticeID, command.SubmissionID, command.Text, command.TimeoutMs)
		return map[string]any{"state": request.State, "request": request, "gate": "closed"}, err
	case "decide":
		request, err := store.decidePlan(command.RequestID, execution, command.Decision, command.Reason)
		gate := "closed"
		if err == nil && request.State == "approved" {
			gate = "granted"
		}
		return map[string]any{"state": request.State, "request": request, "gate": gate}, err
	case "cancel":
		request, err := store.cancelPlan(command.RequestID)
		return map[string]any{"state": request.State, "request": request, "gate": "closed"}, err
	case "get", "list":
		var members []teamMember
		if caller == nil {
			_, members, err = store.Snapshot()
		} else {
			members = []teamMember{*parent.team}
		}
		if err != nil {
			return nil, err
		}
		views := []planView{}
		for _, member := range members {
			if !member.PlanRequired || command.Action == "get" && member.Ref != command.RequestID.Member {
				continue
			}
			requests, err := store.readPlans(member.Ref)
			if err != nil {
				return nil, err
			}
			granted, err := store.planGranted(member.Ref)
			if err != nil {
				return nil, err
			}
			gate := "closed"
			if granted {
				gate = "granted"
			}
			for _, request := range requests {
				if command.Action == "list" || request.ID == command.RequestID {
					views = append(views, planView{Request: request, Gate: gate})
				}
			}
		}
		if command.Action == "get" {
			if len(views) != 1 {
				return nil, reject("rejected", "plan request is unavailable")
			}
			return map[string]any{"state": "found", "request": views[0].Request, "gate": views[0].Gate}, nil
		}
		return map[string]any{"state": "listed", "plans": views}, nil
	}
	return nil, reject("rejected", "unknown plan action")
}
