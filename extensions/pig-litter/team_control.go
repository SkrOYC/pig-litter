package piglitter

import (
	"encoding/json"
	"maps"
	"strings"
	"unicode/utf8"
)

type shutdownCommand struct {
	Action       string            `json:"action"`
	Member       MemberRef         `json:"member"`
	SubmissionID string            `json:"submissionId"`
	Reason       string            `json:"reason"`
	TimeoutMs    int               `json:"timeoutMs"`
	RequestID    ShutdownRequestID `json:"requestId"`
	Decision     string            `json:"decision"`
}

type shutdownView struct {
	Request shutdownRequest `json:"request"`
	Phase   string          `json:"phase"`
	Notices []controlNotice `json:"notices"`
}

func parseShutdownCommand(raw json.RawMessage) (shutdownCommand, error) {
	var command shutdownCommand
	if len(raw) > 16384 || !utf8.Valid(raw) || checkUniqueJSON(raw) != nil {
		return command, reject("rejected", "invalid shutdown arguments")
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil || json.Unmarshal(fields["action"], &command.Action) != nil {
		return command, reject("rejected", "invalid shutdown action")
	}
	keys := []string{"action"}
	switch command.Action {
	case "request":
		keys = append(keys, "member", "submissionId", "reason", "timeoutMs")
	case "get", "cancel":
		keys = append(keys, "requestId")
	case "list":
	case "respond":
		keys = append(keys, "requestId", "decision")
		var decision string
		if json.Unmarshal(fields["decision"], &decision) != nil {
			return command, reject("rejected", "invalid shutdown response")
		}
		if decision == "reject" {
			keys = append(keys, "reason")
		} else if decision != "approve" {
			return command, reject("rejected", "invalid shutdown response")
		}
	default:
		return command, reject("rejected", "unknown shutdown action")
	}
	if _, err := exactJSONObject(raw, keys...); err != nil || json.Unmarshal(raw, &command) != nil {
		return command, reject("rejected", "invalid action-specific shutdown arguments")
	}
	if command.Action == "request" {
		if !validControlMember(fields["member"], command.Member) {
			return command, reject("rejected", "invalid shutdown request")
		}
	}
	if command.Action == "respond" || command.Action == "get" || command.Action == "cancel" {
		request, err := exactJSONObject(fields["requestId"], "member", "sequence", "nonce")
		if err != nil || command.RequestID.Sequence == 0 || !validPlanNonce(command.RequestID.Nonce) || !validControlMember(request["member"], command.RequestID.Member) {
			return command, reject("rejected", "invalid shutdown request identity")
		}
	}
	return command, validateShutdownCommand(command)
}

func validControlMember(raw json.RawMessage, member MemberRef) bool {
	fields, err := exactJSONObject(raw, "team", "id", "generation")
	if err != nil {
		return false
	}
	_, err = exactJSONObject(fields["team"], "root", "epoch")
	return err == nil && validControlRef(member)
}

func validControlRef(member MemberRef) bool {
	return childUUID.MatchString(string(member.Team.Root)) && member.Team.Epoch > 0 && childUUID.MatchString(string(member.ID)) && member.Generation > 0
}

func validateShutdownCommand(command shutdownCommand) error {
	switch command.Action {
	case "request":
		if !validControlRef(command.Member) || command.RequestID != (ShutdownRequestID{}) || command.Decision != "" || !peerMessageIDPattern.MatchString(command.SubmissionID) || strings.TrimSpace(command.Reason) == "" || len(command.Reason) > 4096 || !utf8.ValidString(command.Reason) || command.TimeoutMs < 1000 || command.TimeoutMs > 3600000 {
			return reject("rejected", "invalid shutdown request")
		}
	case "respond", "get", "cancel":
		if command.Member != (MemberRef{}) || command.SubmissionID != "" || command.TimeoutMs != 0 || !validControlRef(command.RequestID.Member) || command.RequestID.Sequence == 0 || !validPlanNonce(command.RequestID.Nonce) {
			return reject("rejected", "invalid shutdown request identity")
		}
		if command.Action != "respond" && (command.Decision != "" || command.Reason != "") {
			return reject("rejected", "unexpected shutdown response fields")
		}
		if command.Action == "respond" && (command.Decision != "approve" && command.Decision != "reject" || command.Decision == "approve" && command.Reason != "" || command.Decision == "reject" && (strings.TrimSpace(command.Reason) == "" || len(command.Reason) > 4096 || !utf8.ValidString(command.Reason))) {
			return reject("rejected", "invalid shutdown response")
		}
	case "list":
		if command.Member != (MemberRef{}) || command.SubmissionID != "" || command.Reason != "" || command.TimeoutMs != 0 || command.RequestID != (ShutdownRequestID{}) || command.Decision != "" {
			return reject("rejected", "unexpected shutdown list fields")
		}
	default:
		return reject("rejected", "unknown shutdown action")
	}
	return nil
}

func (o *owner) shutdownOperation(raw json.RawMessage, authority controlAuthority) (map[string]any, error) {
	command, err := parseShutdownCommand(raw)
	if err != nil {
		return nil, err
	}
	return o.applyShutdown(authority, command)
}

func (o *owner) applyShutdown(authority controlAuthority, command shutdownCommand) (result map[string]any, err error) {
	if err := validateShutdownCommand(command); err != nil {
		return nil, err
	}
	var notice controlNotice
	var route bool
	o.tree.lifecycleMu.Lock()
	defer func() {
		o.tree.lifecycleMu.Unlock()
		if route {
			o.routeControlNotice(notice)
		}
	}()
	if err := o.current(); err != nil {
		return nil, err
	}
	o.tree.mu.Lock()
	lead := authority.caller == nil
	var actor *record
	if authority.human != nil {
		if err := authority.human(); err != nil {
			o.tree.mu.Unlock()
			return nil, err
		}
		lead = true
	} else if authority.caller != nil {
		actor, err = o.tree.callerLocked(authority.caller)
		if err != nil || actor.team == nil {
			o.tree.mu.Unlock()
			return nil, reject("permission_denied", "a current teammate is required")
		}
	} else if _, err := o.tree.callerLocked(nil); err != nil {
		o.tree.mu.Unlock()
		return nil, err
	}
	if command.Action == "request" || command.Action == "cancel" {
		if !lead {
			o.tree.mu.Unlock()
			return nil, reject("permission_denied", "only the team lead can request or cancel shutdown")
		}
	}
	if command.Action == "respond" && (lead || actor.team.Ref != command.RequestID.Member) {
		o.tree.mu.Unlock()
		return nil, reject("permission_denied", "only the addressed teammate can respond")
	}
	if !lead && command.Action == "get" && actor.team.Ref != command.RequestID.Member {
		o.tree.mu.Unlock()
		return nil, reject("permission_denied", "the shutdown belongs to another teammate")
	}
	var target *record
	if command.Action == "request" {
		target = o.tree.records[command.Member.ID]
		if target == nil || target.team == nil || target.team.Ref != command.Member || !target.admitted || target.controlPhase == "terminated" || target.controlPhase == "recovery_pending" {
			o.tree.mu.Unlock()
			return nil, reject("unavailable", "the teammate is unavailable for shutdown")
		}
		if target.role == "observer" {
			o.tree.mu.Unlock()
			return nil, reject("permission_denied", "observers do not accept shutdown requests")
		}
	}
	if command.Action == "respond" {
		target = actor
	}
	o.tree.mu.Unlock()
	store, err := o.teamStoreForRoot()
	if err != nil {
		return nil, err
	}
	switch command.Action {
	case "request":
		request, createdNotice, created, err := store.requestShutdown(command.Member, command.SubmissionID, command.Reason, command.TimeoutMs)
		if err != nil {
			return nil, err
		}
		notice, route = createdNotice, created || request.State == "pending" && createdNotice.Delivery == "pending"
		return map[string]any{"state": request.State, "request": request, "delivery": notice.Delivery}, nil
	case "respond":
		request, responseNotice, err := store.respondShutdown(command.RequestID, authority.caller.noticeID, command.Decision, command.Reason)
		if err != nil {
			return nil, err
		}
		notice, route = responseNotice, true
		phase := "open"
		if command.Decision == "approve" {
			o.tree.mu.Lock()
			if target.run != authority.caller || target.paused || o.tree.records[target.id] != target {
				o.tree.mu.Unlock()
				return nil, reject("unavailable", "the teammate execution changed during shutdown")
			}
			authority.caller.shutdown = &shutdownFence{Request: request.ID, Batch: maps.Clone(authority.caller.batchCalls)}
			target.controlPhase = "closing"
			o.tree.mu.Unlock()
			phase = "closing"
			if err := store.SetState(target.team.Ref, "closing"); err != nil {
				return nil, err
			}
		}
		return map[string]any{"state": request.State, "request": request, "phase": phase, "delivery": notice.Delivery}, nil
	case "cancel":
		request, cancelNotice, err := store.cancelShutdown(command.RequestID)
		if err != nil {
			return nil, err
		}
		notice, route = cancelNotice, true
		return map[string]any{"state": request.State, "request": request, "delivery": notice.Delivery}, nil
	case "get", "list":
		var members []teamMember
		if lead {
			_, members, err = store.Snapshot()
		} else {
			members = []teamMember{*actor.team}
		}
		if err != nil {
			return nil, err
		}
		views, err := o.shutdownViews(store, members, lead, command, false)
		if err != nil {
			return nil, err
		}
		if command.Action == "get" {
			if len(views) != 1 {
				return nil, reject("rejected", "shutdown request is unavailable")
			}
			return map[string]any{"state": "found", "shutdown": views[0]}, nil
		}
		return map[string]any{"state": "listed", "shutdowns": views}, nil
	}
	return nil, reject("rejected", "unknown shutdown action")
}

func (o *owner) shutdownViews(store *teamStore, members []teamMember, lead bool, command shutdownCommand, passive bool) ([]shutdownView, error) {
	views := []shutdownView{}
	for _, member := range members {
		if command.Action == "get" && member.Ref != command.RequestID.Member {
			continue
		}
		var lifecycle memberLifecycle
		var err error
		if passive {
			lifecycle, err = store.snapshotLifecycle(member.Ref)
		} else {
			if lead {
				o.observeControlNotices(member.Ref, "lead")
			} else {
				o.observeControlNotices(member.Ref, "member")
			}
			lifecycle, err = store.readLifecycle(member.Ref)
		}
		if err != nil {
			return nil, err
		}
		if lifecycle.Phase == "terminated" {
			o.tree.mu.Lock()
			current := o.tree.records[member.Ref.ID]
			confirmed := member.State == "terminated" && current != nil && current.team != nil && current.team.Ref == member.Ref && current.controlPhase == "terminated"
			o.tree.mu.Unlock()
			if !confirmed {
				return nil, reject("unavailable", "teammate termination is not fully committed")
			}
		}
		for _, request := range lifecycle.Requests {
			if command.Action == "get" && request.ID != command.RequestID {
				continue
			}
			notices := []controlNotice{}
			for _, candidate := range lifecycle.Notices {
				if candidate.Request == request.ID && (lead || candidate.Recipient == "member") {
					notices = append(notices, candidate)
				}
			}
			views = append(views, shutdownView{Request: request, Phase: lifecycle.Phase, Notices: notices})
		}
	}
	return views, nil
}

func (o *owner) shutdownSnapshot(authority controlAuthority) ([]shutdownView, error) {
	if authority.caller != nil {
		return nil, reject("permission_denied", "only the team lead can view all shutdowns")
	}
	o.tree.lifecycleMu.Lock()
	defer o.tree.lifecycleMu.Unlock()
	if err := o.current(); err != nil {
		return nil, err
	}
	o.tree.mu.Lock()
	var err error
	if authority.human != nil {
		err = authority.human()
	} else {
		_, err = o.tree.callerLocked(nil)
	}
	o.tree.mu.Unlock()
	if err != nil {
		return nil, err
	}
	store, err := o.teamStoreForRoot()
	if err != nil {
		return nil, err
	}
	_, members, err := store.Snapshot()
	if err != nil {
		return nil, err
	}
	return o.shutdownViews(store, members, true, shutdownCommand{Action: "list"}, true)
}
