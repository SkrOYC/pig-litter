package piglitter

import "github.com/MichaelKinsy/PiG/extensions/sdk"

func (s *extensionState) rootContext(ctx sdk.Context, data map[string]any) (_ any, err error) {
	if ctx.Mode() == "tui" {
		defer func() { err = paintError(err) }()
	}
	messages, ok := data["messages"].([]any)
	if !ok {
		return nil, nil
	}
	signal := ctx.Signal()
	genuine := ctx.Mode() == "tui" && signal != nil && signal.Err() == nil
	if ctx.Mode() == "tui" && !genuine {
		return nil, nil
	}
	if genuine {
		if _, err := s.acquire(ctx, false); err != nil {
			return nil, err
		}
	}
	return s.observe(ctx, func(o *owner) (any, error) {
		if genuine {
			if signal.Err() != nil || ctx.Signal() != signal {
				return nil, nil
			}
			o.watchRoot(signal)
			o.startRecovery(nil)
			return o.reconcileRootContext(messages)
		}
		return map[string]any{"messages": o.completionContext(o.tree.root, messages)}, nil
	})
}

func (o *owner) reconcileRootContext(messages []any) (any, error) {
	o.recoveryMu.Lock()
	defer o.recoveryMu.Unlock()
	entries, err := o.persistedCompletionEntries(o.tree.root.id)
	if err != nil {
		return nil, err
	}
	consumed := consumedCompletions(entries, o.tree.root.id)
	o.tree.mu.Lock()
	var pending []recoveredCompletion
	for selected, notice := range o.tree.pending {
		if notice.parent != o.tree.root.id || selected.finishing || selected.outcome == nil || selected.admission != admissionReady || !selected.owner.admitted {
			continue
		}
		pending = append(pending, recoveredCompletion{selected: selected, outcome: *selected.outcome})
	}
	o.tree.mu.Unlock()
	for _, recovered := range pending {
		selected := recovered.selected
		content, err := completionContent(recovered.outcome)
		if err != nil {
			return nil, err
		}
		details := completionDetails{ChildID: selected.ref.ID, Receipt: selected.noticeID}
		o.tree.mu.Lock()
		notice := o.tree.pending[selected]
		if notice == nil || o.tree.closed {
			o.tree.mu.Unlock()
			continue
		}
		notice.content, notice.selected = content, selected
		if o.tree.root.notices == nil {
			o.tree.root.notices = map[string]*reservation{}
		}
		o.tree.root.notices[notice.nonce] = notice
		handback := notice.handback
		waiting := false
		if handback == nil {
			o.ackMu.Lock()
			for _, ack := range o.outcomeAcks {
				if ack.run == selected {
					copy := ack.handback
					handback, waiting = &copy, true
					break
				}
			}
			o.ackMu.Unlock()
		}
		o.tree.mu.Unlock()
		if handbackMatches(entries, handback, details, content) {
			o.tree.mu.Lock()
			notice.outcomeObserved, notice.handback = true, handback
			o.tree.mu.Unlock()
			if err := o.auditCompletion(selected); err != nil {
				return nil, err
			}
			consumed[details] = content
			o.acknowledgeCompletion(selected)
			continue
		}
		if completionStored(entries, details, content) {
			o.acknowledgeCompletion(selected)
			continue
		}
		if waiting {
			continue
		}
		o.tree.mu.Lock()
		if notice.handback != handback {
			o.tree.mu.Unlock()
			continue
		}
		notice.outcomeObserved = false
		o.tree.mu.Unlock()
		selected.mu.Lock()
		receipt := selected.deliveryReceipt
		selected.mu.Unlock()
		if receipt != nil && !receipt.retry() {
			continue
		}
		o.tree.mu.Lock()
		if o.tree.closed {
			o.tree.mu.Unlock()
			continue
		}
		o.tree.completionWG.Add(1)
		o.tree.mu.Unlock()
		go func() {
			defer o.tree.completionWG.Done()
			if receipt != nil {
				receipt.send()
			} else {
				o.sendCompletion(selected.owner, selected, recovered.outcome)
			}
		}()
	}
	kept := make([]any, 0, len(messages))
	for _, message := range messages {
		details, ok := contextCompletion(message)
		content, _ := contextCustom(message)["content"].(string)
		if ok && content != "" && consumed[details] == content {
			continue
		}
		kept = append(kept, message)
	}
	return map[string]any{"messages": o.completionContext(o.tree.root, kept)}, nil
}
