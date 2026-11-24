package piglitter

import (
	"context"
	"encoding/json"
	"regexp"
	"slices"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

const childEntryType = "litter_child_v1"

var childUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type savedNotice struct {
	Nonce           string           `json:"nonce"`
	Outcome         Outcome          `json:"outcome"`
	Consumed        bool             `json:"consumed"`
	Handback        *outcomeHandback `json:"handback,omitempty"`
	AuditAttempted  bool             `json:"auditAttempted,omitempty"`
	NativeAttempted bool             `json:"nativeAttempted,omitempty"`
}

type savedChild struct {
	RootID             ChildID          `json:"rootId"`
	ID                 ChildID          `json:"id"`
	ParentID           ChildID          `json:"parentId"`
	Name               string           `json:"name"`
	Kind               string           `json:"kind"`
	Model              string           `json:"model"`
	Cwd                string           `json:"cwd"`
	Instructions       string           `json:"instructions"`
	Tools              []string         `json:"tools"`
	CanDelegate        bool             `json:"canDelegate"`
	ReadOnly           bool             `json:"readOnly"`
	PendingTask        string           `json:"pendingTask"`
	PendingInputs      []pendingInput   `json:"pendingInputs,omitempty"`
	Admitted           bool             `json:"admitted"`
	State              State            `json:"state"`
	Paused             bool             `json:"paused"`
	NoticeID           string           `json:"noticeId"`
	Latest             *Outcome         `json:"latest,omitempty"`
	Notices            []savedNotice    `json:"notices,omitempty"`
	Team               *teamMember      `json:"team,omitempty"`
	Backend            backendRequest   `json:"backend,omitempty"`
	Native             *memberOwnership `json:"native,omitempty"`
	NativeCleanupError string           `json:"nativeCleanupError,omitempty"`
	NativeSession      *memberSession   `json:"nativeSession,omitempty"`
}

func (o *owner) saveChild(r *record) error {
	return o.saveChildState(r, nil)
}

func (o *owner) saveChildState(r *record, terminated *run) error {
	o.persistMu.Lock()
	defer o.persistMu.Unlock()
	return o.saveChildStateLocked(r, terminated)
}

func (o *owner) saveChildStateLocked(r *record, terminated *run) error {
	o.tree.mu.Lock()
	if o.tree.records[r.id] != r {
		o.tree.mu.Unlock()
		return nil
	}
	selected, paused := r.run, r.paused
	if r.previous != nil {
		selected, paused = r.previous, selected.resumePaused
	}
	if terminated != nil && (selected != terminated || r.previous != nil) {
		o.tree.mu.Unlock()
		return reject("unavailable", "teammate execution changed before termination")
	}
	saved := savedChild{RootID: o.tree.root.id, ID: r.id, ParentID: r.parentID(), Name: r.name, Kind: r.kind, Model: r.model, Cwd: r.cwd, Instructions: r.instructions, Tools: slices.Clone(r.tools), CanDelegate: r.canDelegate, ReadOnly: r.readOnly, PendingTask: r.pendingTask, Admitted: r.admitted, State: selected.state, Paused: paused, NoticeID: selected.noticeID, Backend: r.backend}
	if terminated != nil {
		saved.State = Terminated
	}
	if r.team != nil {
		copy := *r.team
		saved.Team = &copy
	}
	saved.Native = cloneMemberOwnership(r.nativeOwnership)
	saved.NativeCleanupError = r.nativeCleanupError
	saved.NativeSession = cloneMemberSession(r.nativeSession)
	saved.PendingInputs = slices.Clone(r.pendingInputs)
	if r.latest != nil {
		copy := *r.latest
		if terminated != nil {
			copy.State = Terminated
		}
		saved.Latest = &copy
	}
	for pending, receipt := range o.tree.pending {
		if pending.owner == r && pending.outcome != nil {
			saved.Notices = append(saved.Notices, savedNotice{Nonce: receipt.nonce, Outcome: *pending.outcome, Consumed: receipt.outcomeObserved, Handback: receipt.handback, AuditAttempted: receipt.auditAttempted, NativeAttempted: receipt.nativeAttempted})
		}
	}
	o.tree.mu.Unlock()
	return o.context.AppendEntry(childEntryType, saved)
}

type recoveredCompletion struct {
	selected *run
	outcome  Outcome
}

func (o *owner) recoveryBatch() []recoveredCompletion {
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	pending := make([]recoveredCompletion, 0, len(o.tree.pending))
	for selected := range o.tree.pending {
		if selected.outcome != nil {
			pending = append(pending, recoveredCompletion{selected: selected, outcome: *selected.outcome})
		}
	}
	return pending
}

func (o *owner) recoverCompletions(pending []recoveredCompletion) {
	for _, recovered := range pending {
		selected := recovered.selected
		content, err := completionContent(recovered.outcome)
		if err != nil {
			o.tree.deliveryFailed(selected, err)
			continue
		}
		entries, err := o.persistedCompletionEntries(selected.owner.parentID())
		if err == nil && completionStored(entries, completionDetails{ChildID: selected.ref.ID, Receipt: selected.noticeID}, content) {
			o.acknowledgeCompletion(selected)
			continue
		}
		o.tree.mu.Lock()
		notice := o.tree.pending[selected]
		consumed := notice != nil && notice.outcomeObserved
		var handback *outcomeHandback
		if notice != nil {
			handback = notice.handback
		}
		o.tree.mu.Unlock()
		if consumed && err == nil && handbackStored(entries, handback, selected.ref.ID) {
			o.acknowledgeCompletion(selected)
			continue
		}
		if notice != nil {
			o.tree.mu.Lock()
			notice.outcomeObserved = false
			o.tree.mu.Unlock()
		}
		o.sendCompletion(selected.owner, selected, recovered.outcome)
	}
}

func spawnStored(entries []json.RawMessage, id ChildID) bool {
	for _, raw := range entries {
		var entry struct {
			Message struct {
				Role    string `json:"role"`
				IsError bool   `json:"isError"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Message.Role != "toolResult" || entry.Message.IsError {
			continue
		}
		for _, content := range entry.Message.Content {
			var result struct {
				Operation string   `json:"operation"`
				State     State    `json:"state"`
				Child     Snapshot `json:"child"`
			}
			if content.Type == "text" && json.Unmarshal([]byte(content.Text), &result) == nil && result.Operation == "litter_spawn" && result.State == Starting && result.Child.ID == id {
				return true
			}
		}
	}
	return false
}

func teamAdmissionMode(entries []json.RawMessage, member teamMember) (bool, bool, error) {
	calls := map[string]bool{}
	var mode, found bool
	schema, err := json.Marshal(schemas["team_spawn"])
	if err != nil {
		return false, false, err
	}
	for _, raw := range entries {
		var entry struct {
			Message struct {
				Role       string            `json:"role"`
				ToolCallID string            `json:"toolCallId"`
				ToolName   string            `json:"toolName"`
				IsError    bool              `json:"isError"`
				Content    []json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		if entry.Message.Role == "assistant" {
			calls = map[string]bool{}
			for _, block := range entry.Message.Content {
				var call struct {
					Type      string          `json:"type"`
					ID        string          `json:"id"`
					Name      string          `json:"name"`
					Arguments json.RawMessage `json:"arguments"`
				}
				if json.Unmarshal(block, &call) != nil || call.Type != "toolCall" || call.Name != "team_spawn" || call.ID == "" || checkUniqueJSON(call.Arguments) != nil {
					continue
				}
				normalized, err := ai.ValidateToolArgumentsJSON("team_spawn", schema, call.Arguments)
				if err != nil {
					continue
				}
				var args struct {
					PlanRequired bool `json:"planRequired"`
				}
				if json.Unmarshal(normalized, &args) != nil {
					continue
				}
				calls[call.ID] = args.PlanRequired
			}
			continue
		}
		if entry.Message.Role != "toolResult" || entry.Message.ToolName != "team_spawn" || entry.Message.IsError {
			continue
		}
		for _, block := range entry.Message.Content {
			var content struct {
				Type string `json:"type"`
				Text string `json:"text"`
			}
			if json.Unmarshal(block, &content) != nil || content.Type != "text" {
				continue
			}
			var result struct {
				Operation    string    `json:"operation"`
				State        string    `json:"state"`
				Admission    string    `json:"admission"`
				Member       MemberRef `json:"member"`
				PlanRequired *bool     `json:"planRequired"`
			}
			if checkUniqueJSON([]byte(content.Text)) != nil || json.Unmarshal([]byte(content.Text), &result) != nil || result.Operation != "team_spawn" || result.Member.ID != member.Ref.ID {
				continue
			}
			requested, exists := calls[entry.Message.ToolCallID]
			if !exists || result.State != "starting" || result.Member != member.Ref || result.Admission != member.AdmissionID || result.PlanRequired == nil && requested || result.PlanRequired != nil && *result.PlanRequired != requested || found && mode != requested {
				return false, false, reject("unavailable", "teammate planning provenance conflicts")
			}
			mode, found = requested, true
		}
	}
	return mode, found, nil
}

func (o *owner) restoreChildren() error {
	members := map[ChildID]teamMember{}
	reservations := map[ChildID]teamReservation{}
	if o.teamStore != nil {
		roster, pending, err := o.teamStore.recoverySnapshot()
		if err != nil {
			return err
		}
		for _, member := range roster {
			if member.Workspace.Kind == "isolated_worktree" {
				ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
				err := verifyTeamWorktree(ctx, member.Workspace)
				cancel()
				if err != nil {
					if err := o.teamStore.SetState(member.Ref, "unavailable"); err != nil {
						return err
					}
					member.State = "unavailable"
				}
			}
			members[member.Ref.ID] = member
		}
		for _, reservation := range pending {
			reservations[reservation.Member.Ref.ID] = reservation
		}
	}
	entries, err := o.context.SessionManager().GetEntries()
	if err != nil {
		return err
	}
	raw, err := rawEntries(entries)
	if err != nil {
		return err
	}
	latest := map[ChildID]savedChild{}
	invalidTeam := map[ChildID]bool{}
	type admissionProof struct {
		mode, known bool
		err         error
	}
	proofs := map[ChildID]admissionProof{}
	order := []ChildID{}
	seen := map[ChildID]bool{}
	for _, entry := range entries {
		if entry["customType"] != childEntryType {
			continue
		}
		data, err := json.Marshal(entry["data"])
		var saved savedChild
		if err != nil || json.Unmarshal(data, &saved) != nil {
			return reject("unavailable", "stored child metadata is invalid")
		}
		if saved.RootID != o.tree.root.id {
			continue
		}
		if !childUUID.MatchString(string(saved.ID)) || saved.ID == saved.ParentID || saved.Cwd == "" || saved.Name == "" {
			return reject("unavailable", "stored child metadata is invalid")
		}
		if saved.Backend != "" && (saved.Team == nil || saved.Backend != backendTmux && saved.Backend != backendITerm) {
			return reject("unavailable", "stored child backend is invalid")
		}
		if len(saved.NativeCleanupError) > 1024 || saved.NativeCleanupError != "" && saved.Team == nil {
			return reject("unavailable", "stored native cleanup status is invalid")
		}
		if saved.NativeSession != nil && (saved.Team == nil || saved.Backend != "" && saved.Backend != backendTmux || !validMemberSession(saved.NativeSession, saved.ID, saved.Cwd, saved.Model)) {
			return reject("unavailable", "stored native session identity is invalid")
		}
		if saved.Team != nil {
			member, exists := members[saved.ID]
			reservation, provisional := reservations[saved.ID]
			if !exists && provisional {
				member, exists = reservation.Member, true
				if saved.Native == nil && saved.NativeSession == nil {
					delete(latest, saved.ID)
					continue
				}
				launch := ""
				if saved.Native != nil {
					launch = saved.Native.Launch
				} else {
					launch = saved.NativeSession.Launch
				}
				if reservation.NativeLaunch != "" && reservation.NativeLaunch != launch {
					return reject("unavailable", "provisional native launch identity does not match its reservation")
				}
			}
			proof, cached := proofs[saved.ID]
			if exists && !cached {
				proof.mode, proof.known, proof.err = teamAdmissionMode(raw, member)
				proofs[saved.ID] = proof
			}
			if !exists || invalidTeam[saved.ID] || saved.ParentID != o.tree.root.id || member.Ref != saved.Team.Ref || member.AdmissionID != saved.Team.AdmissionID || member.Name != saved.Name || member.Workspace.Cwd != saved.Cwd || member.Workspace != saved.Team.Workspace || member.PlanRequired != saved.Team.PlanRequired || proof.err != nil || proof.known && proof.mode != member.PlanRequired || !proof.known && member.PlanRequired && !provisional {
				if provisional && (saved.Native != nil || saved.NativeSession != nil) {
					return reject("unavailable", "provisional native ownership does not match its reservation")
				}
				invalidTeam[saved.ID] = true
				delete(latest, saved.ID)
				continue
			}
			saved.Team = &member
			if member.State == "paused" {
				saved.Paused = true
			}
		}
		if saved.Native != nil && !validMemberOwnership(saved.Native, saved.Team, saved.Cwd, saved.Model, saved.NoticeID) {
			return reject("unavailable", "stored native member ownership is invalid")
		}
		if saved.Native != nil && saved.Backend != "" && saved.Backend != saved.Native.Backend {
			return reject("unavailable", "stored native member backend does not match its ownership")
		}
		if saved.Backend == "" && saved.Native != nil {
			saved.Backend = saved.Native.Backend
		}
		if !seen[saved.ID] {
			order = append(order, saved.ID)
			seen[saved.ID] = true
		}
		latest[saved.ID] = saved
	}
	order = slices.DeleteFunc(order, func(id ChildID) bool { _, exists := latest[id]; return invalidTeam[id] || !exists })
	controlPhases := map[ChildID]string{}
	for id, member := range members {
		if member.PlanRequired && latest[id].Team != nil {
			if err := o.teamStore.recoverPlans(member.Ref); err != nil {
				invalidTeam[id] = true
				delete(latest, id)
			}
		}
		if latest[id].Team != nil && !invalidTeam[id] {
			phase, err := o.teamStore.recoverLifecycle(member.Ref, member.State)
			if err != nil || phase == "terminated" && latest[id].State != Terminated {
				invalidTeam[id] = true
				delete(latest, id)
				continue
			}
			controlPhases[id] = phase
			if phase == "recovery_pending" && latest[id].State == Terminated {
				saved := latest[id]
				saved.State, saved.Paused = Unavailable, true
				saved.Latest = &Outcome{Run: Ref{id}, Model: saved.Model, State: Unavailable, Error: "shutdown did not finish before recovery"}
				latest[id] = saved
			}
		}
	}
	order = slices.DeleteFunc(order, func(id ChildID) bool { return invalidTeam[id] })
	for id, saved := range latest {
		if saved.Native == nil && (saved.NativeSession == nil || reservations[id].Member.Ref.ID == "") {
			continue
		}
		member, committed := members[id]
		if !committed {
			reservation, exists := reservations[id]
			if !exists {
				return reject("unavailable", "provisional native reservation is unavailable")
			}
			member = reservation.Member
			saved.State = Unavailable
		} else if member.State != "terminated" {
			if err := o.teamStore.SetState(member.Ref, "unavailable"); err != nil {
				return err
			}
			member.State = "unavailable"
			saved.State = Unavailable
		}
		saved.Paused = true
		saved.Team = &member
		message := "native teammate requires explicit resume"
		if saved.Native != nil && saved.NativeCleanupError == "" {
			saved.NativeCleanupError = "native pane ownership requires reconciliation"
		}
		if saved.Native != nil {
			message = "native pane ownership requires reconciliation"
		}
		saved.Latest = &Outcome{Run: Ref{id}, Model: saved.Model, State: saved.State, Error: message}
		latest[id] = saved
	}
	for len(order) != 0 {
		remaining := []ChildID{}
		for _, id := range order {
			saved := latest[id]
			parent := o.tree.root
			if saved.ParentID != parent.id {
				parent = o.tree.records[saved.ParentID]
				if parent == nil {
					if _, exists := latest[saved.ParentID]; !exists {
						return reject("unavailable", "stored child parent is unavailable")
					}
					remaining = append(remaining, id)
					continue
				}
			}
			if !saved.Admitted && saved.Team == nil {
				parentEntries := raw
				if parent != o.tree.root {
					parent.deliveryMu.Lock()
					err = o.withChildLogLocked(parent, func(log *coding.SessionManager) error {
						parentEntries, err = rawEntries(log.GetEntries())
						return err
					})
					parent.deliveryMu.Unlock()
					if err != nil {
						return err
					}
				}
				if !spawnStored(parentEntries, id) {
					delete(latest, id)
					continue
				}
			}
			key := string(parent.id) + "\x00" + saved.Name
			if _, exists := o.tree.names[key]; exists && saved.Team == nil {
				return reject("unavailable", "stored child names conflict")
			}
			r := &record{id: id, team: saved.Team, backend: saved.Backend, nativeOwnership: cloneMemberOwnership(saved.Native), nativeCleanupError: saved.NativeCleanupError, nativeSession: cloneMemberSession(saved.NativeSession), parent: parent, depth: parent.depth + 1, name: saved.Name, kind: saved.Kind, model: saved.Model, cwd: saved.Cwd, instructions: saved.Instructions, tools: saved.Tools, canDelegate: saved.CanDelegate, readOnly: saved.ReadOnly, pendingTask: saved.PendingTask, admitted: true, paused: saved.Paused, latest: saved.Latest, controlPhase: controlPhases[id]}
			r.pendingInputs = saved.PendingInputs
			r.run = o.tree.newRunLocked(Ref{id}, "")
			r.run.owner, r.run.noticeID, r.run.state = r, saved.NoticeID, saved.State
			r.run.admission = admissionReady
			if !saved.State.terminal() {
				r.run.state, r.paused = Paused, true
				r.latest = &Outcome{Run: Ref{id}, Model: r.model, State: Paused, Error: "previous execution was interrupted; resume to continue"}
			}
			if r.latest == nil {
				r.latest = &Outcome{Run: Ref{id}, Model: r.model, State: r.run.state, Error: "previous execution ended; resume to continue"}
			}
			r.run.outcome = r.latest
			r.run.cancel()
			close(r.run.done)
			close(r.run.teamFinishDone)
			o.tree.records[id] = r
			if saved.Team == nil {
				o.tree.names[key] = id
			} else {
				delete(members, id)
			}
			o.tree.order = append(o.tree.order, id)
			for _, notice := range saved.Notices {
				selected := r.run
				if notice.Nonce != selected.noticeID {
					selected = o.tree.newRunLocked(Ref{id}, "")
					selected.owner, selected.noticeID, selected.state = r, notice.Nonce, notice.Outcome.State
					selected.admission = admissionReady
					selected.cancel()
					close(selected.done)
				}
				outcome := notice.Outcome
				selected.outcome = &outcome
				selected.reservation = &reservation{parent: parent.id, nonce: notice.Nonce, selected: selected, outcomeObserved: notice.Consumed, handback: notice.Handback, auditAttempted: notice.AuditAttempted, nativeAttempted: notice.NativeAttempted}
				o.tree.pending[selected] = selected.reservation
			}
		}
		if len(remaining) == len(order) {
			return reject("unavailable", "stored child parents form a cycle")
		}
		order = remaining
	}
	for _, member := range members {
		if err := o.teamStore.SetState(member.Ref, "unavailable"); err != nil {
			return err
		}
	}
	return nil
}

func (o *owner) withChildLogLocked(r *record, apply func(*coding.SessionManager) error) error {
	o.tree.mu.Lock()
	log := r.activeLog
	native := r.native
	ownership, releasing := r.nativeOwnership, r.nativeReleasing
	o.tree.mu.Unlock()
	if native != nil || ownership != nil || releasing {
		return reject("unavailable", "the native member still owns its session log")
	}
	if log != nil {
		return apply(log)
	}
	trusted := true
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: r.cwd, AgentDir: o.agentDir, ProjectTrusted: &trusted})
	if err != nil {
		return err
	}
	defer services.Close()
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		return err
	}
	defer runtime.Close()
	session, err := runtime.Resume(string(r.id), coding.SessionStartOptions{ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		return reject("unavailable", "persisted child UUID is unavailable: "+err.Error())
	}
	defer session.Close()
	go func() {
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	return apply(session.SessionManager())
}
