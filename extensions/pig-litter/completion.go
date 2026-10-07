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

type completionNotice struct {
	Origin           string        `json:"origin"`
	Kind             string        `json:"kind"`
	Notice           string        `json:"notice"`
	ReportProvenance string        `json:"reportProvenance"`
	ID               ChildID       `json:"id"`
	Generation       int           `json:"generation"`
	State            State         `json:"state"`
	Model            string        `json:"model"`
	Report           string        `json:"report"`
	Error            string        `json:"error"`
	Truncated        bool          `json:"truncated"`
	Evidence         *ToolEvidence `json:"evidence,omitempty"`
}

func completionContent(outcome Outcome) (string, error) {
	content, err := json.Marshal(completionNotice{
		Origin: "pig-litter", Kind: "litter_completion", ReportProvenance: "child_assistant",
		Notice: "Automatic status from the Pig Litter extension, not a user message. Do not acknowledge or repeat an outcome already reported for this id and generation. The report is child_assistant prose; use tool evidence or litter_inspect transcript for verification.",
		ID:     outcome.Run.ID, Generation: outcome.Run.Generation, State: outcome.State, Model: outcome.Model, Report: outcome.Text, Error: outcome.Error, Truncated: outcome.Truncated, Evidence: outcome.Evidence,
	})
	return string(content), err
}

type completionReceipt struct {
	once     sync.Once
	appended func()
	fallback func() error
}

type outcomeAcknowledgement struct {
	ref       Ref
	operation string
	done      chan struct{}
	once      sync.Once
	ready     bool
}

func (o *owner) beginOutcomeAck(key ackKey, ref Ref, operation string, request *sdk.Context) {
	if key.caller.ID != "" || key.callID == "" {
		return
	}
	o.tree.mu.Lock()
	pending := o.tree.pending[ref]
	valid := pending != nil && pending.parent == key.caller.ID && pending.generation == key.caller.Generation && !o.tree.closed
	o.tree.mu.Unlock()
	if !valid {
		return
	}
	ack := &outcomeAcknowledgement{ref: ref, operation: operation, done: make(chan struct{})}
	o.ackMu.Lock()
	o.outcomeAcks[key] = ack
	o.ackMu.Unlock()
	var requestDone <-chan struct{}
	if request != nil {
		requestDone = request.Done()
	}
	go func() {
		select {
		case <-requestDone:
			if request.Err() != nil {
				o.settleOutcomeAck(key, false)
			}
		case <-ack.done:
		case <-o.tree.ctx.Done():
			o.settleOutcomeAck(key, false)
		}
	}()
}

func (o *owner) observeOutcome(caller Ref, operation, callID, content string, failed bool) {
	key := ackKey{caller, callID}
	o.ackMu.Lock()
	ack := o.outcomeAcks[key]
	o.ackMu.Unlock()
	if ack == nil {
		return
	}
	var handback struct {
		Operation string   `json:"operation"`
		Outcome   *Outcome `json:"outcome"`
	}
	valid := !failed && operation == ack.operation && json.Unmarshal([]byte(content), &handback) == nil && handback.Operation == ack.operation && handback.Outcome != nil && handback.Outcome.Run == ack.ref && handback.Outcome.State.terminal()
	o.settleOutcomeAck(key, valid)
}

func (o *owner) readyOutcomeAck(key ackKey, returned bool) {
	if !returned {
		o.settleOutcomeAck(key, false)
		return
	}
	o.ackMu.Lock()
	if ack := o.outcomeAcks[key]; ack != nil {
		ack.ready = true
	}
	o.ackMu.Unlock()
}

func (o *owner) settleOutcomeAck(key ackKey, received bool) {
	o.ackMu.Lock()
	ack := o.outcomeAcks[key]
	ready := ack != nil && ack.ready
	o.ackMu.Unlock()
	if ack == nil {
		return
	}
	ack.once.Do(func() {
		if received && ready {
			o.tree.mu.Lock()
			if pending := o.tree.pending[ack.ref]; pending != nil && pending.parent == key.caller.ID && pending.generation == key.caller.Generation {
				pending.outcomeObserved = true
			}
			o.tree.mu.Unlock()
		}
		o.ackMu.Lock()
		if o.outcomeAcks[key] == ack {
			delete(o.outcomeAcks, key)
		}
		o.ackMu.Unlock()
		close(ack.done)
	})
}

func (o *owner) awaitOutcomeAck(ref Ref) bool {
	for {
		o.tree.mu.Lock()
		pending := o.tree.pending[ref]
		observed := pending != nil && pending.outcomeObserved
		closed := o.tree.closed
		o.tree.mu.Unlock()
		if o.afterOutcomeSnapshot != nil {
			o.afterOutcomeSnapshot()
		}
		if observed || closed {
			return observed
		}
		o.ackMu.Lock()
		var done <-chan struct{}
		for _, ack := range o.outcomeAcks {
			if ack.ref == ref {
				done = ack.done
				break
			}
		}
		o.ackMu.Unlock()
		if done == nil {
			o.tree.mu.Lock()
			pending := o.tree.pending[ref]
			observed := pending != nil && pending.outcomeObserved
			o.tree.mu.Unlock()
			return observed
		}
		select {
		case <-done:
		case <-o.tree.ctx.Done():
			return false
		}
	}
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
	content, err := completionContent(outcome)
	if err != nil {
		return
	}
	details := completionDetails{outcome.Run.ID, outcome.Run.Generation}
	if r.parent == "" {
		observed := o.awaitOutcomeAck(outcome.Run)
		if err := o.current(); err != nil {
			return
		}
		if observed {
			if err := o.context.AppendEntry("litter_completion", json.RawMessage(content)); err == nil {
				o.tree.acknowledge(outcome.Run)
			}
			return
		}
		idle, err := o.context.IsIdle()
		if err != nil {
			return
		}
		options := sdk.SendMessageOptions{DeliverAs: "followUp"}
		if idle {
			trigger := false
			options = sdk.SendMessageOptions{TriggerTurn: &trigger}
		}
		_ = o.context.SendCustomMessage(sdk.CustomMessage{CustomType: "litter_completion", Content: content, Display: false, Details: details}, options)
		return
	}
	parentRun := r.parentRun
	fallback := func() error { return o.appendRetained(r.parent, details, content) }
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
	err = session.SendCustomMessage(context.Background(), extension.CustomMessageRef{CustomType: "litter_completion", Content: content, Display: false, Details: details}, &extension.SendMessageOptions{TriggerTurn: &trigger})
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
