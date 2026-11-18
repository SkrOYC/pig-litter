package piglitter

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func (o *owner) shutdownHook(r *record, selected *run) agent.BeforeToolCallHook {
	return func(_ context.Context, id, name string, _ json.RawMessage) agent.ToolCallHookResult {
		o.tree.mu.Lock()
		defer o.tree.mu.Unlock()
		for member := r; member != nil && member != o.tree.root; member = member.parent {
			if member.team == nil || member.controlPhase != "closing" {
				continue
			}
			if name == "litter_spawn" || name == "team_spawn" || name == "litter_message" {
				return agent.ToolCallHookResult{Block: true, Reason: "teammate shutdown prevents new child work"}
			}
			if member == r && selected.shutdown != nil {
				for callID, callName := range selected.shutdown.Batch {
					if id == callID && name == callName || strings.HasPrefix(id, callID+"/") {
						return agent.ToolCallHookResult{}
					}
				}
				return agent.ToolCallHookResult{Block: true, Reason: "teammate shutdown is draining its current batch"}
			}
		}
		return agent.ToolCallHookResult{}
	}
}

func (o *owner) routeControlNotice(notice controlNotice) {
	o.controlMu.Lock()
	if o.controlAuth == nil {
		o.controlAuth = make(map[string]controlNotice)
	}
	o.controlAuth[notice.ID] = notice
	o.controlMu.Unlock()
	o.tree.mu.Lock()
	if o.tree.closed {
		o.tree.mu.Unlock()
		return
	}
	o.tree.teamWG.Add(1)
	o.tree.mu.Unlock()
	go func() {
		defer o.tree.teamWG.Done()
		o.deliverControlNotice(notice)
	}()
}

func (o *owner) deliverControlNotice(notice controlNotice) {
	if o.current() != nil {
		return
	}
	var recipient *record
	var selected *run
	if notice.Recipient == "member" {
		o.tree.mu.Lock()
		recipient = o.tree.records[notice.Request.Member.ID]
		if recipient != nil && recipient.team != nil && recipient.team.Ref == notice.Request.Member && !recipient.paused && !recipient.run.state.terminal() {
			selected = recipient.run
		}
		o.tree.mu.Unlock()
		if selected == nil {
			return
		}
		selected.mu.Lock()
		session := selected.session
		selected.mu.Unlock()
		if session == nil {
			return
		}
	}
	store, err := o.teamStoreForRoot()
	if err != nil {
		return
	}
	changed, err := store.setControlNotice(notice, "pending", "attempted", "")
	if err != nil || !changed {
		return
	}
	payload, err := json.Marshal(map[string]any{"kind": notice.Kind, "requestId": notice.Request, "message": notice.Text})
	if err != nil {
		return
	}
	content := string(payload)
	details := map[string]any{"notice": notice}
	if notice.Recipient == "lead" {
		trigger := false
		err = o.context.SendCustomMessage(sdk.CustomMessage{CustomType: "pig_litter_control", Content: content, Display: true, Details: details}, sdk.SendMessageOptions{TriggerTurn: &trigger})
	} else {
		selected.mu.Lock()
		session := selected.session
		selected.mu.Unlock()
		if session == nil {
			err = reject("unavailable", "teammate session closed before notice delivery")
		} else {
			err = session.SendCustomMessage(selected.ctx, extension.CustomMessageRef{CustomType: "pig_litter_control", Content: content, Display: true, Details: details}, &extension.SendMessageOptions{DeliverAs: extension.DeliverAsSteer})
		}
	}
	if err != nil {
		_, _ = store.setControlNotice(notice, "attempted", "ambiguous", "")
		return
	}
	o.observeControlNotice(notice)
}

func (o *owner) routePendingControls(ref MemberRef) {
	o.controlMu.Lock()
	var pending []controlNotice
	for _, notice := range o.controlAuth {
		if notice.Recipient == "member" && notice.Request.Member == ref {
			pending = append(pending, notice)
		}
	}
	o.controlMu.Unlock()
	for _, notice := range pending {
		o.routeControlNotice(notice)
	}
}

func (o *owner) observeControlNotice(notice controlNotice) {
	o.controlMu.Lock()
	trusted, ok := o.controlAuth[notice.ID]
	o.controlMu.Unlock()
	if !ok || trusted != notice {
		return
	}
	parentID := notice.Request.Member.ID
	if notice.Recipient == "lead" {
		parentID = o.tree.root.id
	}
	entries, err := o.completionEntries(parentID)
	if err != nil {
		return
	}
	payload, err := json.Marshal(map[string]any{"kind": notice.Kind, "requestId": notice.Request, "message": notice.Text})
	if err != nil {
		return
	}
	var observed string
	for _, raw := range entries {
		var entry struct {
			Type       string          `json:"type"`
			ID         string          `json:"id"`
			CustomType string          `json:"customType"`
			Content    string          `json:"content"`
			Details    json.RawMessage `json:"details"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Type != "custom_message" || entry.CustomType != "pig_litter_control" {
			continue
		}
		var details struct {
			Notice controlNotice `json:"notice"`
		}
		if json.Unmarshal(entry.Details, &details) != nil || details.Notice != notice || entry.Content != string(payload) {
			continue
		}
		if entry.ID == "" || observed != "" {
			return
		}
		observed = entry.ID
	}
	if observed == "" {
		return
	}
	store, err := o.teamStoreForRoot()
	if err == nil {
		_, _ = store.setControlNotice(notice, "attempted", "native_observed", observed)
	}
}

func (o *owner) observeControlNotices(ref MemberRef, recipient string) {
	o.controlMu.Lock()
	var notices []controlNotice
	for _, notice := range o.controlAuth {
		if notice.Request.Member == ref && notice.Recipient == recipient {
			notices = append(notices, notice)
		}
	}
	o.controlMu.Unlock()
	for _, notice := range notices {
		o.observeControlNotice(notice)
	}
}

func (o *owner) observeLeadControls() {
	o.controlMu.Lock()
	var notices []controlNotice
	for _, notice := range o.controlAuth {
		if notice.Recipient == "lead" {
			notices = append(notices, notice)
		}
	}
	o.controlMu.Unlock()
	for _, notice := range notices {
		o.observeControlNotice(notice)
	}
}

func (o *owner) settleShutdown(r *record, selected *run) {
	o.tree.mu.Lock()
	fence := selected.shutdown
	if fence == nil {
		o.tree.mu.Unlock()
		return
	}
	var descendants []*run
	for _, id := range o.tree.order {
		child := o.tree.records[id]
		if child != r && o.tree.belongsLocked(r, child) {
			descendants = append(descendants, child.run)
		}
	}
	o.tree.mu.Unlock()
	for _, child := range descendants {
		select {
		case <-child.done:
		case <-o.tree.ctx.Done():
			return
		}
	}
	o.tree.lifecycleMu.Lock()
	o.tree.mu.Lock()
	current := !o.tree.closed && o.tree.records[r.id] == r && r.run == selected && r.controlPhase == "closing" && !r.paused && fence.Drained && selected.teamFinishErr == nil && selected.outcome != nil && selected.outcome.Error == ""
	o.tree.mu.Unlock()
	store, err := o.teamStoreForRoot()
	if err != nil {
		o.tree.lifecycleMu.Unlock()
		return
	}
	if !current || o.current() != nil {
		_, _ = store.finishShutdown(fence.Request, selected.noticeID, false)
		o.tree.mu.Lock()
		if r.controlPhase == "closing" {
			r.controlPhase = "recovery_pending"
		}
		o.tree.mu.Unlock()
		o.tree.lifecycleMu.Unlock()
		return
	}
	if err := o.saveChildState(r, selected); err != nil {
		o.tree.lifecycleMu.Unlock()
		return
	}
	notice, err := store.finishShutdown(fence.Request, selected.noticeID, true)
	if err != nil {
		o.tree.lifecycleMu.Unlock()
		return
	}
	if err := store.SetState(r.team.Ref, "terminated"); err != nil {
		o.tree.lifecycleMu.Unlock()
		return
	}
	o.tree.mu.Lock()
	selected.state = Terminated
	selected.outcome.State = Terminated
	r.latest = selected.outcome
	r.controlPhase = "terminated"
	o.tree.mu.Unlock()
	o.tree.lifecycleMu.Unlock()
	o.routeControlNotice(notice)
	o.tree.notify()
}
