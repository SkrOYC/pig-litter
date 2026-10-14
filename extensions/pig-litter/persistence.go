package piglitter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"slices"

	"github.com/MichaelKinsy/PiG/coding"
)

const childEntryType = "litter_child_v1"

var childUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type savedNotice struct {
	Nonce    string  `json:"nonce"`
	Outcome  Outcome `json:"outcome"`
	Consumed bool    `json:"consumed"`
}

type savedChild struct {
	RootID       ChildID       `json:"rootId"`
	ID           ChildID       `json:"id"`
	ParentID     ChildID       `json:"parentId"`
	Name         string        `json:"name"`
	Kind         string        `json:"kind"`
	Model        string        `json:"model"`
	Cwd          string        `json:"cwd"`
	Instructions string        `json:"instructions"`
	Tools        []string      `json:"tools"`
	CanDelegate  bool          `json:"canDelegate"`
	ReadOnly     bool          `json:"readOnly"`
	PendingTask  string        `json:"pendingTask"`
	Admitted     bool          `json:"admitted"`
	State        State         `json:"state"`
	Paused       bool          `json:"paused"`
	NoticeID     string        `json:"noticeId"`
	Latest       *Outcome      `json:"latest,omitempty"`
	Notices      []savedNotice `json:"notices,omitempty"`
}

func (o *owner) saveChild(r *record) error {
	o.persistMu.Lock()
	defer o.persistMu.Unlock()
	o.tree.mu.Lock()
	if o.tree.closed || o.tree.records[r.id] != r {
		o.tree.mu.Unlock()
		return nil
	}
	selected, paused := r.run, r.paused
	if r.previous != nil {
		selected, paused = r.previous, selected.resumePaused
	}
	saved := savedChild{RootID: o.tree.root.id, ID: r.id, ParentID: r.parentID(), Name: r.name, Kind: r.kind, Model: r.model, Cwd: r.cwd, Instructions: r.instructions, Tools: slices.Clone(r.tools), CanDelegate: r.canDelegate, ReadOnly: r.readOnly, PendingTask: r.pendingTask, Admitted: r.admitted, State: selected.state, Paused: paused, NoticeID: selected.noticeID}
	if r.latest != nil {
		copy := *r.latest
		saved.Latest = &copy
	}
	for pending, receipt := range o.tree.pending {
		if pending.owner == r && pending.outcome != nil {
			saved.Notices = append(saved.Notices, savedNotice{Nonce: receipt.nonce, Outcome: *pending.outcome, Consumed: receipt.outcomeObserved})
		}
	}
	o.tree.mu.Unlock()
	return o.context.AppendEntry(childEntryType, saved)
}

func (o *owner) persistChild(r *record) {
	if err := o.saveChild(r); err != nil {
		o.tree.mu.Lock()
		description, _ := bounded(fmt.Sprintf("child metadata storage failed: %v", err), 1024)
		r.run.deliveryError = description
		if r.latest != nil {
			copy := *r.latest
			copy.Error = description
			if copy.State == Completed {
				copy.State = Partial
			}
			r.latest = &copy
		}
		o.tree.mu.Unlock()
	}
}

func (o *owner) recoverCompletions() {
	o.tree.mu.Lock()
	pending := make([]*run, 0, len(o.tree.pending))
	for selected := range o.tree.pending {
		pending = append(pending, selected)
	}
	o.tree.mu.Unlock()
	for _, selected := range pending {
		content, err := completionContent(*selected.outcome)
		if err != nil {
			o.tree.deliveryFailed(selected, err)
			continue
		}
		entries, err := o.completionEntries(selected.owner.parentID())
		if err == nil && completionStored(entries, completionDetails{ChildID: selected.ref.ID, Receipt: selected.noticeID}, content) {
			o.tree.acknowledge(selected)
			continue
		}
		o.sendCompletion(selected.owner, selected, *selected.outcome)
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

func (o *owner) restoreChildren() error {
	entries, err := o.context.SessionManager().GetEntries()
	if err != nil {
		return err
	}
	raw, err := rawEntries(entries)
	if err != nil {
		return err
	}
	latest := map[ChildID]savedChild{}
	order := []ChildID{}
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
		if _, exists := latest[saved.ID]; !exists {
			order = append(order, saved.ID)
		}
		latest[saved.ID] = saved
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
			if !saved.Admitted {
				parentEntries := raw
				if parent != o.tree.root {
					parent.deliveryMu.Lock()
					err = o.withChildLogLocked(parent, func(log *coding.SessionManager) error {
						parentEntries, err = rawEntries(log.Entries())
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
			if _, exists := o.tree.names[key]; exists {
				return reject("unavailable", "stored child names conflict")
			}
			r := &record{id: id, parent: parent, depth: parent.depth + 1, name: saved.Name, kind: saved.Kind, model: saved.Model, cwd: saved.Cwd, instructions: saved.Instructions, tools: saved.Tools, canDelegate: saved.CanDelegate, readOnly: saved.ReadOnly, pendingTask: saved.PendingTask, admitted: true, paused: saved.Paused, latest: saved.Latest}
			r.run = o.tree.newRunLocked(Ref{id}, "")
			r.run.owner, r.run.noticeID, r.run.state = r, saved.NoticeID, saved.State
			if !saved.State.terminal() {
				r.run.state, r.paused = Paused, true
			}
			if r.latest == nil {
				r.latest = &Outcome{Run: Ref{id}, Model: r.model, State: r.run.state, Error: "previous execution ended; resume to continue"}
			}
			r.run.outcome = r.latest
			r.run.cancel()
			close(r.run.done)
			o.tree.records[id], o.tree.names[key] = r, id
			o.tree.order = append(o.tree.order, id)
			for _, notice := range saved.Notices {
				selected := r.run
				if notice.Nonce != selected.noticeID {
					selected = o.tree.newRunLocked(Ref{id}, "")
					selected.owner, selected.noticeID, selected.state = r, notice.Nonce, notice.Outcome.State
					selected.cancel()
					close(selected.done)
				}
				outcome := notice.Outcome
				selected.outcome = &outcome
				selected.reservation = &reservation{parent: parent.id, nonce: notice.Nonce, selected: selected, outcomeObserved: notice.Consumed}
				o.tree.pending[selected] = selected.reservation
			}
		}
		if len(remaining) == len(order) {
			return reject("unavailable", "stored child parents form a cycle")
		}
		order = remaining
	}
	return nil
}

func (o *owner) withChildLogLocked(r *record, apply func(*coding.SessionManager) error) error {
	o.tree.mu.Lock()
	log := r.activeLog
	o.tree.mu.Unlock()
	if log != nil {
		return apply(log)
	}
	trusted := true
	services, err := coding.NewServices(coding.ServicesOptions{CWD: r.cwd, AgentDir: o.agentDir, ProjectTrusted: &trusted})
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
