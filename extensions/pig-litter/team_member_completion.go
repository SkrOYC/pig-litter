package piglitter

import (
	"context"
	"fmt"
	"time"
)

func (o *owner) deliverMemberCompletion(parent *record, binding *memberBinding, details completionDetails, content, mode string, trigger bool) error {
	o.tree.mu.Lock()
	var notice *reservation
	for selected, pending := range o.tree.pending {
		if selected.ref.ID == details.ChildID && pending.parent == parent.id && pending.nonce == details.Receipt && pending.sent && pending.content == content {
			notice = pending
			break
		}
	}
	if notice == nil || !o.memberBindingCurrentLocked(binding) {
		o.tree.mu.Unlock()
		return reject("unavailable", "native completion no longer belongs to this execution")
	}
	attempted := notice.nativeAttempted
	if !attempted {
		notice.nativeAttempted = true
	}
	o.tree.mu.Unlock()
	observed := func() bool {
		entries, err := o.memberLogEntries(o.tree.ctx, binding)
		return err == nil && completionStored(entries, details, content)
	}
	if attempted {
		if observed() {
			return nil
		}
		return reject("unavailable", "native completion delivery is uncertain")
	}
	if err := o.saveChild(notice.selected.owner); err != nil {
		return fmt.Errorf("persist native completion attempt: %w", err)
	}
	ctx, cancel := context.WithTimeout(o.tree.ctx, 5*time.Second)
	defer cancel()
	disposition, err := binding.control.Queue(ctx, memberCompletionInput{details: details, content: content, mode: mode, trigger: trigger})
	if err != nil || disposition == memberInputUncertain {
		if observed() {
			return nil
		}
		return reject("unavailable", "native completion delivery is uncertain")
	}
	if disposition != memberInputQueued && disposition != memberInputHandled {
		return reject("unavailable", "native completion delivery returned an invalid disposition")
	}
	return nil
}

func (o *owner) auditMemberCompletion(binding *memberBinding, selected *run, notice *reservation, audit completionAudit) error {
	observed := func() bool {
		entries, err := o.memberLogEntries(o.tree.ctx, binding)
		return err == nil && completionStored(entries, audit.Details, audit.Content)
	}
	if observed() {
		return nil
	}
	o.tree.mu.Lock()
	if o.tree.pending[selected] != notice || !o.memberBindingCurrentLocked(binding) {
		o.tree.mu.Unlock()
		return reject("unavailable", "native completion audit no longer belongs to this execution")
	}
	attempted := notice.auditAttempted
	if !attempted {
		notice.auditAttempted = true
	}
	o.tree.mu.Unlock()
	if attempted {
		return reject("unavailable", "native completion audit is uncertain")
	}
	if err := o.saveChild(selected.owner); err != nil {
		return fmt.Errorf("persist native audit attempt: %w", err)
	}
	ctx, cancel := context.WithTimeout(o.tree.ctx, 5*time.Second)
	defer cancel()
	_ = binding.control.RecordCompletion(ctx, audit)
	if !observed() {
		return reject("unavailable", "native completion audit is uncertain")
	}
	return nil
}
