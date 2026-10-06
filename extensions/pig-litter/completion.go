package piglitter

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

type completionDetails struct {
	ChildID    ChildID `json:"childId"`
	Generation int     `json:"generation"`
}
type completionReceipt struct {
	once     sync.Once
	appended func()
	fallback func() error
}

func (r *completionReceipt) complete() { r.once.Do(r.appended) }
func (r *completionReceipt) retire() {
	r.once.Do(func() {
		if r.fallback() == nil {
			r.appended()
		}
	})
}

func (o *owner) observeCompletion(caller Ref, message map[string]any) {
	if message["customType"] != "litter_completion" {
		return
	}
	raw, err := json.Marshal(message["details"])
	if err != nil {
		return
	}
	var details completionDetails
	if json.Unmarshal(raw, &details) != nil {
		return
	}
	ref := Ref{details.ChildID, details.Generation}
	o.tree.mu.Lock()
	pending := o.tree.pending[ref]
	valid := pending != nil && pending.sent && pending.parent == caller.ID && pending.generation == caller.Generation && !o.tree.closed
	r := o.tree.records[ref.ID]
	o.tree.mu.Unlock()
	if !valid || r == nil {
		return
	}
	if caller.ID == "" {
		o.tree.acknowledge(ref)
		return
	}
	parentRun := r.parentRun
	if parentRun == nil {
		return
	}
	parentRun.mu.Lock()
	receipt := parentRun.receipts[ref]
	delete(parentRun.receipts, ref)
	parentRun.mu.Unlock()
	if receipt != nil {
		receipt.complete()
	}
}

func completionExists(manager *coding.SessionManager, details completionDetails) bool {
	for _, entry := range manager.Entries() {
		raw, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		var item struct {
			CustomType string            `json:"customType"`
			Details    completionDetails `json:"details"`
			Message    *struct {
				CustomType string            `json:"customType"`
				Details    completionDetails `json:"details"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &item) != nil {
			continue
		}
		if item.CustomType == "litter_completion" && item.Details == details {
			return true
		}
		if item.Message != nil && item.Message.CustomType == "litter_completion" && item.Message.Details == details {
			return true
		}
	}
	return false
}

func (o *owner) appendRetained(parentID ChildID, details completionDetails, content string) error {
	if err := o.current(); err != nil {
		return err
	}
	o.tree.mu.Lock()
	parent := o.tree.records[parentID]
	var manager *coding.SessionManager
	if parent != nil {
		manager = parent.history
	}
	o.tree.mu.Unlock()
	if manager == nil {
		return reject("unavailable", "parent retained history is unavailable")
	}
	if !completionExists(manager, details) {
		if _, err := manager.AppendCustomMessage("litter_completion", content, false, details); err != nil {
			return err
		}
	}
	if historyBytes(manager) > o.config.MaxHistoryBytes {
		o.tree.retireHistory(parent)
		return reject("unavailable", "parent retained history cannot fit the child completion")
	}
	return nil
}

func (o *owner) sendCompletion(r *record, outcome Outcome) {
	if err := o.current(); err != nil || !o.tree.startCompletion(outcome.Run) {
		return
	}
	content, err := json.Marshal(map[string]any{"id": outcome.Run.ID, "generation": outcome.Run.Generation, "state": outcome.State, "model": outcome.Model, "report": outcome.Text, "error": outcome.Error, "truncated": outcome.Truncated})
	if err != nil {
		return
	}
	details := completionDetails{outcome.Run.ID, outcome.Run.Generation}
	if r.parent == "" {
		idle, err := o.context.IsIdle()
		if err != nil {
			return
		}
		options := sdk.SendMessageOptions{DeliverAs: "followUp"}
		if idle {
			trigger := false
			options = sdk.SendMessageOptions{TriggerTurn: &trigger}
		}
		_ = o.context.SendCustomMessage(sdk.CustomMessage{CustomType: "litter_completion", Content: string(content), Display: false, Details: details}, options)
		return
	}
	parentRun := r.parentRun
	fallback := func() error { return o.appendRetained(r.parent, details, string(content)) }
	if parentRun == nil {
		if fallback() == nil {
			o.tree.acknowledge(outcome.Run)
		}
		return
	}
	receipt := &completionReceipt{appended: func() { o.tree.acknowledge(outcome.Run) }, fallback: fallback}
	parentRun.mu.Lock()
	session := parentRun.session
	if session == nil || parentRun.disposing {
		parentRun.mu.Unlock()
		receipt.retire()
		return
	}
	parentRun.receipts[outcome.Run] = receipt
	parentRun.mu.Unlock()
	trigger := false
	err = session.SendCustomMessage(context.Background(), extension.CustomMessageRef{CustomType: "litter_completion", Content: string(content), Display: false, Details: details}, &extension.SendMessageOptions{TriggerTurn: &trigger})
	if err != nil {
		parentRun.mu.Lock()
		parentRun.completionSendError, _ = bounded(err.Error(), 256)
		parentRun.mu.Unlock()
	}
}

func (o *owner) retireReceipts(run *run) {
	run.mu.Lock()
	receipts := run.receipts
	run.receipts = map[Ref]*completionReceipt{}
	run.mu.Unlock()
	for _, receipt := range receipts {
		receipt.retire()
	}
}
