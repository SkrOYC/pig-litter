package piglitter

import (
	"errors"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

var errPeerWakeBlocked = errors.New("peer wake is waiting for room")

func (o *owner) sendPeerWake(session *coding.Session, r *record, selected *run) error {
	store, ok := o.peerRunStore(r, selected)
	if !ok || selected.peerWake == nil || selected.peerWake.Recipient != r.team.Ref {
		return reject("unavailable", "peer wake is no longer current")
	}
	attempt, err := embeddedPeerAttempt(session, selected.peerWake.Recipient)
	if err != nil {
		return err
	}
	receipt, content, found, err := store.offerPeerAttempt(selected.peerWake.Recipient, attempt)
	if err != nil {
		return err
	}
	if !found {
		return reject("unavailable", "no authenticated peer message is pending")
	}
	if _, ok := o.peerRunStore(r, selected); !ok {
		return reject("cancelled", "peer wake was canceled before native delivery")
	}
	trigger := true
	message := extension.CustomMessageRef{CustomType: "pig_litter_peer", Content: content, Display: true, Details: map[string]any{"envelope": receipt.Envelope}}
	return session.SendCustomMessage(selected.ctx, message, &extension.SendMessageOptions{TriggerTurn: &trigger})
}

func (o *owner) requestPeerWake(recipient MemberRef) {
	t := o.tree
	t.mu.Lock()
	r := t.records[recipient.ID]
	if t.closed || r == nil || r.team == nil || r.team.Ref != recipient {
		t.mu.Unlock()
		return
	}
	r.peerWakeVersion++
	if r.peerWakePending {
		t.mu.Unlock()
		return
	}
	r.peerWakePending = true
	t.peerWakeWG.Add(1)
	t.mu.Unlock()
	go o.drivePeerWake(recipient)
}

func (o *owner) finishPeerWakeRequest(recipient MemberRef, version uint64) bool {
	t := o.tree
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.records[recipient.ID]
	if r == nil || r.team == nil || r.team.Ref != recipient {
		return true
	}
	if !t.closed && r.peerWakeVersion != version {
		return false
	}
	r.peerWakePending = false
	return true
}

func (o *owner) drivePeerWake(recipient MemberRef) {
	t := o.tree
	defer t.peerWakeWG.Done()
	for {
		t.mu.Lock()
		r := t.records[recipient.ID]
		if t.closed || r == nil || r.team == nil || r.team.Ref != recipient {
			t.mu.Unlock()
			return
		}
		version, selected, roomChanged := r.peerWakeVersion, r.run, t.roomChanged
		if r.paused || r.controlPhase == "closing" || r.controlPhase == "terminated" || r.controlPhase == "recovery_pending" || t.root.paused || selected == nil {
			r.peerWakePending = false
			t.mu.Unlock()
			return
		}
		done := selected.teamFinishDone
		t.mu.Unlock()
		if done == nil {
			if o.finishPeerWakeRequest(recipient, version) {
				return
			}
			continue
		}
		select {
		case <-done:
		case <-t.ctx.Done():
			o.finishPeerWakeRequest(recipient, version)
			return
		}
		t.mu.Lock()
		r = t.records[recipient.ID]
		current := r != nil && r.team != nil && r.team.Ref == recipient && r.run == selected
		finished := current && selected.teamFinishErr == nil
		t.mu.Unlock()
		if !current {
			continue
		}
		if finished {
			started, err := o.reservePeerWake(recipient, selected)
			if started {
				continue
			}
			if err == errPeerWakeBlocked {
				select {
				case <-roomChanged:
					continue
				case <-t.ctx.Done():
					o.finishPeerWakeRequest(recipient, version)
					return
				}
			}
		}
		if o.finishPeerWakeRequest(recipient, version) {
			return
		}
	}
}

func (o *owner) reservePeerWake(recipient MemberRef, previous *run) (bool, error) {
	t := o.tree
	t.lifecycleMu.Lock()
	o.state.mu.Lock()
	ownerCurrent := o.state.owner == o && o.state.epoch == o.epoch
	o.state.mu.Unlock()
	if !ownerCurrent {
		t.lifecycleMu.Unlock()
		return false, nil
	}
	o.teamMu.Lock()
	store := o.teamStore
	o.teamMu.Unlock()
	if store == nil {
		t.lifecycleMu.Unlock()
		return false, nil
	}
	eligible, err := store.peerWakeEligible(recipient)
	if err != nil || !eligible {
		t.lifecycleMu.Unlock()
		return false, err
	}
	t.mu.Lock()
	r := t.records[recipient.ID]
	if t.closed || t.root.paused || r == nil || r.team == nil || r.team.Ref != recipient || !r.admitted || r.paused || r.run != previous || previous.state != Completed || previous.finishing {
		t.mu.Unlock()
		t.lifecycleMu.Unlock()
		return false, nil
	}
	if err := t.roomLocked(r.parentID(), !r.readOnly, r.cwd, r.id, r.team); err != nil {
		t.mu.Unlock()
		t.lifecycleMu.Unlock()
		return false, errPeerWakeBlocked
	}
	selected := t.newRunLocked(Ref{r.id}, "")
	selected.owner, selected.resume, selected.peerWake = r, true, &peerWakeInput{Recipient: recipient}
	r.previous, r.run = previous, selected
	t.live++
	t.mu.Unlock()
	err = store.SetState(recipient, "running")
	t.lifecycleMu.Unlock()
	if err != nil {
		t.finish(r, selected, Outcome{State: Unavailable, Error: "peer wake roster transition failed: " + err.Error()})
		return false, err
	}
	if err := t.activate(selected); err != nil {
		t.finish(r, selected, Outcome{State: Unavailable, Error: "peer wake activation failed: " + err.Error()})
		return false, err
	}
	return true, nil
}
