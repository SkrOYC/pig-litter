package piglitter

import (
	"testing"
	"time"
)

func peerWakeTestOwner(t *testing.T, finished bool) (*owner, *teamStore, teamMember, teamMember, *record, *run, <-chan *run, chan struct{}) {
	t.Helper()
	store, _, root, workspace := storeTestOpen(t)
	sender, recipient := peerTestMembers(t, store, workspace)
	if err := store.SetState(recipient.Ref, "idle"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.acceptPeer(sender.Ref, peerRequest{Recipient: "bob", MessageID: "wake-1", Text: "hello", Summary: "greeting"}); err != nil {
		t.Fatal(err)
	}
	tree := newTree(Config{Concurrency: 4, MaxMailbox: 1})
	tree.root.id = root
	r := &record{id: recipient.Ref.ID, team: &recipient, parent: tree.root, cwd: workspace.Cwd, admitted: true}
	previous := tree.newRunLocked(Ref{r.id}, "")
	previous.owner, previous.state, previous.started, previous.admission = r, Completed, true, admissionReady
	previous.outcome = &Outcome{Run: previous.ref, State: Completed}
	previous.cancel()
	close(previous.done)
	if finished {
		close(previous.teamFinishDone)
	}
	r.run = previous
	tree.records[r.id] = r
	tree.order = append(tree.order, r.id)
	state := &extensionState{epoch: 1}
	owner := &owner{state: state, epoch: 1, tree: tree, teamStore: store}
	state.owner = owner
	tree.persist = func(*record) error { return nil }
	tree.teamFinished = owner.finishTeamMember
	started, release := make(chan *run, 2), make(chan struct{})
	tree.execute = func(_ *record, selected *run) Outcome {
		_, _, _, _ = store.offerPeer(recipient.Ref)
		started <- selected
		<-release
		return Outcome{Run: selected.ref, State: Completed}
	}
	t.Cleanup(func() { tree.close() })
	return owner, store, sender, recipient, r, previous, started, release
}

func TestPeerWakeRequiresLiveAndStoredUnpausedState(t *testing.T) {
	owner, store, _, recipient, r, previous, started, release := peerWakeTestOwner(t, true)
	defer close(release)
	if _, _, err := owner.tree.resume(nil, previous, "ordinary resume"); err == nil {
		t.Fatal("ordinary child control resumed a teammate")
	}
	r.paused = true
	if woke, err := owner.reservePeerWake(recipient.Ref, previous); err != nil || woke {
		t.Fatalf("live paused member woke: %v, %v", woke, err)
	}
	r.paused = false
	if err := store.SetState(recipient.Ref, "paused"); err != nil {
		t.Fatal(err)
	}
	if woke, err := owner.reservePeerWake(recipient.Ref, previous); err != nil || woke {
		t.Fatalf("durably paused member woke: %v, %v", woke, err)
	}
	if err := store.SetState(recipient.Ref, "idle"); err != nil {
		t.Fatal(err)
	}
	if woke, err := owner.reservePeerWake(recipient.Ref, previous); err != nil || !woke {
		t.Fatalf("idle member did not wake: %v, %v", woke, err)
	}
	select {
	case selected := <-started:
		if selected.peerWake == nil || selected.peerWake.Recipient != recipient.Ref || !selected.resume || selected.continuation != "" {
			t.Fatalf("wake input = %#v", selected)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("idle recipient run did not start")
	}
	_, members, err := store.Snapshot()
	if err != nil || len(members) != 2 || members[1].State != "running" {
		t.Fatalf("running roster = %#v, %v", members, err)
	}
}

func TestPeerWakeWaitsForFinishAndCoalescesRequests(t *testing.T) {
	owner, store, _, recipient, r, previous, started, release := peerWakeTestOwner(t, false)
	defer close(release)
	if err := store.SetState(recipient.Ref, "running"); err != nil {
		t.Fatal(err)
	}
	owner.requestPeerWake(recipient.Ref)
	owner.requestPeerWake(recipient.Ref)
	select {
	case <-started:
		t.Fatal("wake started before the prior roster update completed")
	case <-time.After(50 * time.Millisecond):
	}
	if err := store.SetState(recipient.Ref, "idle"); err != nil {
		t.Fatal(err)
	}
	close(previous.teamFinishDone)
	select {
	case selected := <-started:
		if selected == previous || selected.peerWake == nil || selected.peerWake.Recipient != recipient.Ref {
			t.Fatalf("unexpected wake run = %#v", selected)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("wake did not start after the prior roster update")
	}
	select {
	case <-started:
		t.Fatal("duplicate wake requests started multiple runs")
	case <-time.After(50 * time.Millisecond):
	}
	owner.tree.mu.Lock()
	current := r.run
	owner.tree.mu.Unlock()
	if current == previous {
		t.Fatal("the retained member did not receive a new run")
	}
}
