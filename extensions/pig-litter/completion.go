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
	ChildID ChildID `json:"childId"`
}

type completionNotice struct {
	Origin           string  `json:"origin"`
	Kind             string  `json:"kind"`
	Notice           string  `json:"notice"`
	ReportProvenance string  `json:"reportProvenance"`
	ID               ChildID `json:"id"`
	State            State   `json:"state"`
	Model            string  `json:"model"`
	Report           string  `json:"report"`
	Error            string  `json:"error"`
	Truncated        bool    `json:"truncated"`
}

func completionContent(outcome Outcome) (string, error) {
	content, err := json.Marshal(completionNotice{
		Origin: "pig-litter", Kind: "litter_completion", ReportProvenance: "child_assistant",
		Notice: "Automatic status from the Pig Litter extension, not a user message. Do not acknowledge or repeat an outcome already reported for this child. The report is the child assistant's latest result.",
		ID:     outcome.Run.ID, State: outcome.State, Model: outcome.Model, Report: outcome.Text, Error: outcome.Error, Truncated: outcome.Truncated,
	})
	return string(content), err
}

type completionReceipt struct {
	once     sync.Once
	appended func()
	fallback func() error
	observed func() bool
}

type outcomeAcknowledgement struct {
	run       *run
	operation string
	done      chan struct{}
	once      sync.Once
	ready     bool
}

func (o *owner) beginOutcomeAck(key ackKey, selected *run, operation string, request *sdk.Context) {
	if key.caller != nil || key.callID == "" {
		return
	}
	o.tree.mu.Lock()
	pending := o.tree.pending[selected]
	valid := pending != nil && pending.parentRun == key.caller && !o.tree.closed
	if !valid {
		o.tree.mu.Unlock()
		return
	}
	if o.afterOutcomeReservation != nil {
		o.afterOutcomeReservation()
	}
	ack := &outcomeAcknowledgement{run: selected, operation: operation, done: make(chan struct{})}
	o.ackMu.Lock()
	o.outcomeAcks[key] = ack
	o.ackMu.Unlock()
	o.tree.mu.Unlock()
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

func (o *owner) observeOutcome(caller *run, operation, callID, content string, failed bool) {
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
	valid := !failed && operation == ack.operation && json.Unmarshal([]byte(content), &handback) == nil && handback.Operation == ack.operation && handback.Outcome != nil && handback.Outcome.Run == ack.run.ref && handback.Outcome.State.terminal()
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
	o.ackMu.Unlock()
	if ack == nil {
		return
	}
	ack.once.Do(func() {
		o.tree.mu.Lock()
		o.ackMu.Lock()
		if received && ack.ready && o.outcomeAcks[key] == ack {
			if pending := o.tree.pending[ack.run]; pending != nil && pending.parentRun == key.caller {
				pending.outcomeObserved = true
			}
		}
		if o.outcomeAcks[key] == ack {
			delete(o.outcomeAcks, key)
		}
		o.ackMu.Unlock()
		o.tree.mu.Unlock()
		close(ack.done)
	})
}

func (o *owner) awaitOutcomeAck(selected *run) bool {
	for {
		o.tree.mu.Lock()
		o.ackMu.Lock()
		pending := o.tree.pending[selected]
		observed := pending != nil && pending.outcomeObserved
		closed := o.tree.closed
		var done <-chan struct{}
		for _, ack := range o.outcomeAcks {
			if ack.run == selected {
				done = ack.done
				break
			}
		}
		o.ackMu.Unlock()
		o.tree.mu.Unlock()
		if o.afterOutcomeSnapshot != nil {
			o.afterOutcomeSnapshot()
		}
		if observed || closed || done == nil {
			return observed
		}
		select {
		case <-done:
		case <-o.tree.ctx.Done():
			return false
		}
	}
}

func (r *completionReceipt) complete() {
	if r.observed == nil || r.observed() {
		r.once.Do(r.appended)
	}
}

func (r *completionReceipt) retire() {
	r.once.Do(func() {
		if r.fallback() == nil {
			r.appended()
		}
	})
}

func (o *owner) observeCompletion(caller *run, message map[string]any) {
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
	o.tree.mu.Lock()
	r := o.tree.records[details.ChildID]
	var selected *run
	if r != nil {
		selected = r.delivering
	}
	pending := o.tree.pending[selected]
	valid := pending != nil && pending.sent && pending.parentRun == caller && !o.tree.closed
	o.tree.mu.Unlock()
	if !valid {
		return
	}
	selected.mu.Lock()
	receipt := selected.deliveryReceipt
	selected.mu.Unlock()
	if receipt != nil {
		receipt.complete()
	}
}

type completionEntry struct {
	ID         string            `json:"id"`
	CustomType string            `json:"customType"`
	Content    string            `json:"content"`
	Details    completionDetails `json:"details"`
	Message    *struct {
		CustomType string            `json:"customType"`
		Content    string            `json:"content"`
		Details    completionDetails `json:"details"`
	} `json:"message"`
}

func (o *owner) completionEntries(parent ChildID) ([]json.RawMessage, error) {
	if o.tree.rootID(parent) {
		entries, err := o.context.SessionManager().GetEntries()
		if err != nil {
			return nil, err
		}
		return rawEntries(entries)
	}
	o.tree.mu.Lock()
	r := o.tree.records[parent]
	var manager *coding.SessionManager
	if r != nil {
		manager = r.history
	}
	o.tree.mu.Unlock()
	if manager == nil {
		return nil, reject("unavailable", "parent retained history is unavailable")
	}
	return rawEntries(manager.Entries())
}

func rawEntries(entries any) ([]json.RawMessage, error) {
	data, err := json.Marshal(entries)
	if err != nil {
		return nil, err
	}
	var raw []json.RawMessage
	err = json.Unmarshal(data, &raw)
	return raw, err
}

func completionStored(entries []json.RawMessage, known map[string]struct{}, details completionDetails, content string) bool {
	for _, raw := range entries {
		var entry completionEntry
		if json.Unmarshal(raw, &entry) != nil || entry.ID == "" {
			continue
		}
		if _, exists := known[entry.ID]; exists {
			continue
		}
		if entry.CustomType == "litter_completion" && entry.Details == details && entry.Content == content {
			return true
		}
		if entry.Message != nil && entry.Message.CustomType == "litter_completion" && entry.Message.Details == details && entry.Message.Content == content {
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
	if _, err := manager.AppendCustomMessage("litter_completion", content, false, details); err != nil {
		return err
	}
	if historyBytes(manager) > o.config.MaxHistoryBytes {
		o.tree.retireHistory(parent)
		return reject("unavailable", "parent retained history cannot fit the child completion")
	}
	return nil
}

func (o *owner) sendCompletion(r *record, selected *run, outcome Outcome) {
	if selected.beforeCompletion != nil {
		select {
		case <-selected.beforeCompletion:
		case <-o.tree.ctx.Done():
			return
		}
	}
	if err := o.current(); err != nil || !o.tree.startCompletion(selected) {
		return
	}
	content, err := completionContent(outcome)
	if err != nil {
		return
	}
	details := completionDetails{outcome.Run.ID}
	if o.tree.rootID(r.parentID()) && o.awaitOutcomeAck(selected) {
		if err := o.current(); err != nil {
			return
		}
		if err := o.context.AppendEntry("litter_completion", json.RawMessage(content)); err == nil {
			o.tree.acknowledge(selected)
		}
		return
	}
	entries, err := o.completionEntries(r.parentID())
	if err != nil {
		return
	}
	known := make(map[string]struct{}, len(entries))
	for _, raw := range entries {
		var entry struct {
			ID string `json:"id"`
		}
		if json.Unmarshal(raw, &entry) == nil {
			known[entry.ID] = struct{}{}
		}
	}
	observed := func() bool {
		entries, err := o.completionEntries(r.parentID())
		return err == nil && completionStored(entries, known, details, content)
	}
	var receipt *completionReceipt
	receipt = &completionReceipt{
		appended: func() {
			if parent := selected.parent; parent != nil {
				parent.mu.Lock()
				if parent.receipts[selected] == receipt {
					delete(parent.receipts, selected)
				}
				parent.mu.Unlock()
			}
			selected.mu.Lock()
			if selected.deliveryReceipt == receipt {
				selected.deliveryReceipt = nil
			}
			selected.mu.Unlock()
			o.tree.acknowledge(selected)
		},
		observed: observed,
		fallback: func() error {
			if observed() {
				return nil
			}
			return o.appendRetained(r.parentID(), details, content)
		},
	}
	selected.mu.Lock()
	selected.deliveryReceipt = receipt
	selected.mu.Unlock()
	if o.tree.rootID(r.parentID()) {
		if err := o.current(); err != nil {
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
	parentRun := selected.parent
	if parentRun == nil {
		receipt.retire()
		return
	}
	for {
		parentRun.deliveryMu.Lock()
		parentRun.mu.Lock()
		session, disposing := parentRun.session, parentRun.disposing
		if session != nil && !disposing {
			parentRun.receipts[selected] = receipt
			parentRun.mu.Unlock()
			trigger := false
			err = session.SendCustomMessage(context.Background(), extension.CustomMessageRef{CustomType: "litter_completion", Content: content, Display: false, Details: details}, &extension.SendMessageOptions{TriggerTurn: &trigger})
			parentRun.deliveryMu.Unlock()
			if err != nil {
				parentRun.mu.Lock()
				parentRun.completionSendError, _ = bounded(err.Error(), 256)
				parentRun.mu.Unlock()
			}
			return
		}
		parentRun.mu.Unlock()
		parentRun.deliveryMu.Unlock()
		if disposing {
			select {
			case <-parentRun.done:
			case <-o.tree.ctx.Done():
				return
			}
			receipt.retire()
			return
		}
		if o.beforeParentReadyWait != nil {
			o.beforeParentReadyWait()
		}
		select {
		case <-parentRun.ready:
			continue
		case <-parentRun.done:
			receipt.retire()
			return
		case <-o.tree.ctx.Done():
			return
		}
	}
}

func (o *owner) retireReceipts(execution *run) {
	execution.mu.Lock()
	receipts := execution.receipts
	execution.receipts = map[*run]*completionReceipt{}
	execution.mu.Unlock()
	for _, receipt := range receipts {
		receipt.retire()
	}
}
