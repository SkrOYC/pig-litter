package piglitter

import (
	"context"
	"encoding/json"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

type completionDetails struct {
	ChildID ChildID `json:"childId"`
	Receipt string  `json:"receipt"`
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
	mu       sync.Mutex
	retired  bool
	failed   bool
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

func (o *owner) beginOutcomeAck(key *callTicket, selected *run, operation string, request *sdk.Context) {
	if key.callID == "" {
		return
	}
	o.tree.mu.Lock()
	pending := o.tree.pending[selected]
	if pending == nil {
		pending = selected.reservation
	}
	valid := pending != nil && o.completionOwner(pending, key.caller) && !o.tree.closed
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

func (o *owner) observeOutcome(key *callTicket, operation, content string, failed bool) {
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

func (o *owner) readyOutcomeAck(key *callTicket, returned bool) {
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

func (o *owner) settleOutcomeAck(key *callTicket, received bool) {
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
			pending := o.tree.pending[ack.run]
			if pending == nil {
				pending = ack.run.reservation
			}
			if pending != nil && o.completionOwner(pending, key.caller) {
				pending.outcomeObserved = true
				ack.run.reservation = pending
				if parent := o.completionParentLocked(pending.parent); parent != nil && pending.sent {
					if parent.notices == nil {
						parent.notices = map[string]*reservation{}
					}
					parent.notices[pending.nonce] = pending
				}
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
		if pending == nil {
			pending = selected.reservation
		}
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
	r.mu.Lock()
	failed := r.failed
	r.mu.Unlock()
	if r.observed != nil && r.observed() || r.observed == nil && !failed {
		r.once.Do(r.appended)
	}
}

func (r *completionReceipt) retire() {
	r.mu.Lock()
	if r.retired {
		r.mu.Unlock()
		return
	}
	r.retired = true
	r.mu.Unlock()
	err := r.fallback()
	r.mu.Lock()
	r.failed = err != nil
	r.mu.Unlock()
	if err == nil {
		r.complete()
	}
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
	var selected *run
	for candidate, pending := range o.tree.pending {
		if candidate.ref.ID == details.ChildID && pending.nonce == details.Receipt && pending.sent && o.completionOwner(pending, caller) {
			selected = candidate
			break
		}
	}
	valid := selected != nil && !o.tree.closed
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
	var active *coding.SessionManager
	if r != nil {
		active = r.activeLog
	}
	o.tree.mu.Unlock()
	if r == nil {
		return nil, reject("unavailable", "parent UUID is unavailable")
	}
	if active != nil {
		return rawEntries(active.Entries())
	}
	r.deliveryMu.Lock()
	defer r.deliveryMu.Unlock()
	var entries []json.RawMessage
	err := o.withChildLogLocked(r, func(log *coding.SessionManager) error {
		var err error
		entries, err = rawEntries(log.Entries())
		return err
	})
	return entries, err
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

func completionStored(entries []json.RawMessage, details completionDetails, content string) bool {
	for _, raw := range entries {
		var entry completionEntry
		if json.Unmarshal(raw, &entry) != nil || entry.ID == "" {
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
	return o.storeRetained(parentID, details, content, false)
}

func (o *owner) storeRetained(parentID ChildID, details completionDetails, content string, audit bool) error {
	if err := o.current(); err != nil {
		return err
	}
	if o.tree.rootID(parentID) {
		if audit {
			return o.context.AppendEntry("litter_completion", json.RawMessage(content))
		}
		trigger := false
		return o.context.SendCustomMessage(sdk.CustomMessage{CustomType: "litter_completion", Content: content, Display: false, Details: details}, sdk.SendMessageOptions{TriggerTurn: &trigger})
	}
	o.tree.mu.Lock()
	parent := o.tree.records[parentID]
	o.tree.mu.Unlock()
	if parent == nil {
		return reject("unavailable", "parent UUID is unavailable")
	}
	return o.withChildLogLocked(parent, func(log *coding.SessionManager) error {
		var err error
		if audit {
			_, err = log.AppendCustomEntry("litter_completion", json.RawMessage(content))
		} else {
			_, err = log.AppendCustomMessage("litter_completion", content, false, details)
		}
		return err
	})
}

func (o *owner) auditCompletion(selected *run) error {
	o.tree.mu.Lock()
	notice := selected.reservation
	var parent *record
	if notice != nil {
		parent = o.completionParentLocked(notice.parent)
	}
	o.tree.mu.Unlock()
	if notice == nil {
		return nil
	}
	notice.auditOnce.Do(func() {
		if parent != nil {
			parent.deliveryMu.Lock()
			defer parent.deliveryMu.Unlock()
		}
		details := completionDetails{ChildID: selected.ref.ID, Receipt: notice.nonce}
		notice.auditErr = o.storeRetained(notice.parent, details, notice.content, true)
		if notice.auditErr == nil {
			o.tree.acknowledge(selected)
		}
	})
	o.tree.deliveryFailed(selected, notice.auditErr)
	return notice.auditErr
}

func (o *owner) sendCompletion(r *record, selected *run, outcome Outcome) {
	if err := o.current(); err != nil || o.tree.ctx.Err() != nil {
		return
	}
	content, err := completionContent(outcome)
	if err != nil {
		o.tree.deliveryFailed(selected, err)
		return
	}
	if !o.tree.startCompletion(selected, content) {
		return
	}
	details := completionDetails{ChildID: outcome.Run.ID, Receipt: selected.noticeID}
	o.tree.mu.Lock()
	notice := selected.reservation
	parent := o.completionParentLocked(notice.parent)
	o.tree.mu.Unlock()
	if o.awaitOutcomeAck(selected) {
		_ = o.auditCompletion(selected)
		return
	}
	observed := func() bool {
		o.tree.mu.Lock()
		stored := notice.stored
		o.tree.mu.Unlock()
		if stored {
			return true
		}
		entries, err := o.completionEntries(notice.parent)
		return err == nil && completionStored(entries, details, content)
	}
	var receipt *completionReceipt
	receipt = &completionReceipt{
		appended: func() {
			if parent != nil {
				parent.receiptMu.Lock()
				delete(parent.receipts, selected)
				parent.receiptMu.Unlock()
			}
			selected.mu.Lock()
			selected.deliveryReceipt = nil
			selected.mu.Unlock()
			o.tree.acknowledge(selected)
		},
		observed: observed,
		fallback: func() (err error) {
			defer func() { o.tree.deliveryFailed(selected, err) }()
			o.tree.mu.Lock()
			consumed := notice.outcomeObserved
			o.tree.mu.Unlock()
			if consumed {
				return o.auditCompletion(selected)
			}
			if observed() {
				return nil
			}
			if o.tree.rootID(notice.parent) {
				return o.storeRetained(notice.parent, details, content, false)
			}
			return o.deliverRetained(parent, details, content)
		},
	}
	selected.mu.Lock()
	selected.deliveryReceipt = receipt
	selected.mu.Unlock()
	if parent != nil {
		parent.receiptMu.Lock()
		if parent.receipts == nil {
			parent.receipts = map[*run]*completionReceipt{}
		}
		parent.receipts[selected] = receipt
		parent.receiptMu.Unlock()
		o.tree.mu.Lock()
		if parent.notices == nil {
			parent.notices = map[string]*reservation{}
		}
		parent.notices[notice.nonce] = notice
		o.tree.mu.Unlock()
	}
	if o.tree.rootID(notice.parent) {
		parent.deliveryMu.Lock()
		idle, stateErr := o.context.IsIdle()
		if stateErr != nil {
			parent.deliveryMu.Unlock()
			o.tree.deliveryFailed(selected, stateErr)
			return
		}
		options := o.completionOptions(parent, details, !idle)
		err = o.context.SendCustomMessage(sdk.CustomMessage{CustomType: "litter_completion", Content: content, Display: false, Details: details}, sdk.SendMessageOptions{TriggerTurn: options.TriggerTurn, DeliverAs: string(options.DeliverAs)})
		parent.deliveryMu.Unlock()
	} else {
		err = o.deliverRetained(parent, details, content)
	}
	if err != nil {
		o.tree.deliveryFailed(selected, err)
		return
	}
	receipt.complete()
}

func (o *owner) completionOwner(pending *reservation, caller *run) bool {
	if caller == nil {
		return o.tree.rootID(pending.parent)
	}
	return pending.parent == caller.ref.ID
}

func (r *record) markAgentEnded(ended bool) {
	r.deliveryMu.Lock()
	r.agentEnded = ended
	r.deliveryMu.Unlock()
}

func (o *owner) completionOptions(parent *record, details completionDetails, busy bool) extension.SendMessageOptions {
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	active := busy && !parent.paused
	if notice := parent.notices[details.Receipt]; notice != nil {
		notice.continueTurn = active
	}
	if active && parent.agentEnded {
		return extension.SendMessageOptions{DeliverAs: extension.DeliverAsSteer}
	}
	trigger := false
	return extension.SendMessageOptions{TriggerTurn: &trigger}
}

func (o *owner) deliverRetained(parent *record, details completionDetails, content string) error {
	parent.deliveryMu.Lock()
	defer parent.deliveryMu.Unlock()
	if err := o.tree.ctx.Err(); err != nil {
		return err
	}
	o.tree.mu.Lock()
	current := parent.run
	o.tree.mu.Unlock()
	current.mu.Lock()
	session := current.session
	current.mu.Unlock()
	if session != nil {
		ctx, cancel := context.WithTimeout(o.tree.ctx, 5*time.Second)
		defer cancel()
		options := o.completionOptions(parent, details, session.IsStreaming())
		return session.SendCustomMessage(ctx, extension.CustomMessageRef{CustomType: "litter_completion", Content: content, Display: false, Details: details}, &options)
	}
	return o.appendRetained(parent.id, details, content)
}

func (o *owner) retireReceipts(parent *record) {
	if parent == nil {
		return
	}
	parent.receiptMu.Lock()
	receipts := make([]*completionReceipt, 0, len(parent.receipts))
	for _, receipt := range parent.receipts {
		receipts = append(receipts, receipt)
	}
	parent.receiptMu.Unlock()
	for _, receipt := range receipts {
		receipt.retire()
	}
	o.tree.mu.Lock()
	var audits []*run
	for _, notice := range parent.notices {
		if notice.outcomeObserved {
			audits = append(audits, notice.selected)
		}
	}
	o.tree.mu.Unlock()
	for _, selected := range audits {
		_ = o.auditCompletion(selected)
	}
}

func (o *owner) completionParentLocked(id ChildID) *record {
	if o.tree.rootID(id) {
		return o.tree.root
	}
	return o.tree.records[id]
}

func (o *owner) continueForCompletions(parent *record, pending []any) bool {
	parent.receiptMu.Lock()
	receipts := make([]*completionReceipt, 0, len(parent.receipts))
	for _, receipt := range parent.receipts {
		receipts = append(receipts, receipt)
	}
	parent.receiptMu.Unlock()
	for _, receipt := range receipts {
		receipt.complete()
	}
	queued := map[string]ChildID{}
	for _, message := range pending {
		if details, ok := contextCompletion(message); ok {
			queued[details.Receipt] = details.ChildID
		}
	}
	o.tree.mu.Lock()
	if o.tree.closed {
		o.tree.mu.Unlock()
		return false
	}
	continuation := false
	var audits []*run
	for id, notice := range parent.notices {
		if notice.outcomeObserved {
			audits = append(audits, notice.selected)
		} else if !parent.paused && notice.continueTurn && (notice.stored || queued[id] == notice.selected.ref.ID) {
			continuation = true
			notice.continueTurn = false
			delete(parent.notices, id)
		}
	}
	o.tree.mu.Unlock()
	for _, selected := range audits {
		_ = o.auditCompletion(selected)
	}
	return continuation
}

func contextCompletion(message any) (completionDetails, bool) {
	var custom map[string]any
	switch message := message.(type) {
	case agent.AgentMessage:
		custom = message.Custom
	case map[string]any:
		if message["role"] == "custom" {
			custom = message
		}
	}
	if custom["customType"] != "litter_completion" {
		return completionDetails{}, false
	}
	raw, err := json.Marshal(custom["details"])
	var details completionDetails
	if err != nil || json.Unmarshal(raw, &details) != nil {
		return completionDetails{}, false
	}
	return details, details.Receipt != ""
}

func (o *owner) completionContext(parent *record, messages []any) []any {
	kept := make([]any, 0, len(messages))
	seen := map[completionDetails]bool{}
	var audits []*run
	o.tree.mu.Lock()
	for _, message := range messages {
		details, ok := contextCompletion(message)
		if ok {
			if seen[details] {
				continue
			}
			seen[details] = true
			if notice := parent.notices[details.Receipt]; notice != nil && notice.selected.ref.ID == details.ChildID {
				if notice.outcomeObserved {
					audits = append(audits, notice.selected)
					continue
				}
				notice.continueTurn = false
				delete(parent.notices, details.Receipt)
			}
		}
		kept = append(kept, message)
	}
	for id, notice := range parent.notices {
		if notice.stored && !seen[completionDetails{ChildID: notice.selected.ref.ID, Receipt: id}] {
			delete(parent.notices, id)
		}
	}
	o.tree.mu.Unlock()
	for _, selected := range audits {
		_ = o.auditCompletion(selected)
	}
	return kept
}

func (o *owner) completionHooks(parent *record) extension.Extension {
	return extension.Extension{Name: "pig-litter-completions", Handlers: map[string][]extension.HandlerFn{
		"turn_start": {func(...any) (any, error) {
			parent.markAgentEnded(false)
			return nil, nil
		}},
		"agent_end": {func(...any) (any, error) {
			parent.markAgentEnded(true)
			return nil, nil
		}},
		"agent_settled": {func(...any) (any, error) {
			parent.markAgentEnded(false)
			return nil, nil
		}},
		"context": {func(args ...any) (any, error) {
			event := args[0].(extension.ContextEvent)
			return &extension.ContextEventResult{Messages: o.completionContext(parent, event.Messages)}, nil
		}},
		"agent_before_settle": {func(args ...any) (any, error) {
			event := args[0].(*extension.AgentBeforeSettleEvent)
			if o.continueForCompletions(parent, event.Context.PendingMessages) {
				continuation := true
				return extension.BoundaryResult{Continue: &continuation}, nil
			}
			return nil, nil
		}},
	}}
}
