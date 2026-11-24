package piglitter

import (
	"context"
	"fmt"
	"runtime"
	"strings"
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

func waitForPeerWakeWaiter(t *testing.T, owner *owner) {
	t.Helper()
	target := fmt.Sprintf("(*owner).drivePeerWake(%p,", owner)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		buffer := make([]byte, 1<<20)
		n := runtime.Stack(buffer, true)
		for _, stack := range strings.Split(string(buffer[:n]), "\n\n") {
			if strings.Contains(stack, "[select]") && strings.Contains(stack, target) {
				return
			}
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("peer wake did not wait for the pending resume")
}

func TestPeerWakeAfterCancelledResumeAcknowledgement(t *testing.T) {
	for _, paused := range []bool{false, true} {
		t.Run(map[bool]string{false: "idle", true: "paused"}[paused], func(t *testing.T) {
			owner, store, sender, recipient, r, previous, started, release := peerWakeTestOwner(t, true)
			defer close(release)
			if paused {
				owner.tree.mu.Lock()
				r.paused = true
				owner.tree.mu.Unlock()
				if err := store.SetState(recipient.Ref, "paused"); err != nil {
					t.Fatal(err)
				}
			}
			_, pending, err := owner.tree.resume(nil, previous, "continue")
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			owner.acks = make(map[*callTicket]*acknowledgement)
			ticket := &callTicket{callID: "resume", operation: "litter_message"}
			owner.deferActivation(ticket, pending, ctx.Done(), func() bool { return ctx.Err() != nil }, "litter_message")
			receipt, _, err := store.acceptPeer(sender.Ref, peerRequest{Recipient: "bob", MessageID: "wake-1", Text: "hello", Summary: "greeting"})
			if err != nil || receipt.State != peerAccepted || receipt.Envelope.Sender != sender.Ref || receipt.Envelope.Recipient != recipient.Ref {
				t.Fatalf("same-ID retry: %#v, %v", receipt, err)
			}
			owner.requestPeerWake(recipient.Ref)
			waitForPeerWakeWaiter(t, owner)
			cancel()
			owner.ackWG.Wait()
			if paused {
				finished := make(chan struct{})
				go func() {
					owner.tree.peerWakeWG.Wait()
					close(finished)
				}()
				select {
				case <-finished:
				case <-time.After(3 * time.Second):
					t.Fatal("peer wake stayed blocked after the cancelled resume")
				}
				select {
				case <-started:
					t.Fatal("cancelled resume woke a paused teammate")
				default:
				}
			} else {
				select {
				case selected := <-started:
					if selected == pending || selected.peerWake == nil || selected.peerWake.Recipient != recipient.Ref {
						t.Fatalf("unexpected wake after cancelled resume: %#v", selected)
					}
				case <-time.After(3 * time.Second):
					t.Fatal("accepted peer message remained stranded after the cancelled resume")
				}
			}
			key, err := peerKey(receipt.Envelope)
			if err != nil {
				t.Fatal(err)
			}
			_, path := peerPaths(store.dir, key)
			stored, err := readPeerReceipt(path)
			want := peerAttempted
			if paused {
				want = peerAccepted
			}
			if err != nil || stored.Envelope != receipt.Envelope || stored.State != want {
				t.Fatalf("peer receipt after cancelled resume: %#v, %v", stored, err)
			}
			_, members, err := store.Snapshot()
			wantState := "running"
			if paused {
				wantState = "paused"
			}
			if err != nil || len(members) != 2 || members[0].Ref != sender.Ref || members[1].Ref != recipient.Ref || members[1].State != wantState {
				t.Fatalf("cancelled resume changed team membership: %#v, %v", members, err)
			}
		})
	}
}

func TestPeerWakeWaitsForSettlementAlreadyInProgress(t *testing.T) {
	owner, _, _, recipient, r, previous, started, release := peerWakeTestOwner(t, true)
	defer close(release)
	_, pending, err := owner.tree.resume(nil, previous, "continue")
	if err != nil {
		t.Fatal(err)
	}
	owner.requestPeerWake(recipient.Ref)
	waitForPeerWakeWaiter(t, owner)
	settling, allowSettlement := make(chan struct{}), make(chan struct{})
	settlementReleased := false
	defer func() {
		if !settlementReleased {
			close(allowSettlement)
		}
	}()
	owner.tree.teamFinished = func(r *record, selected *run, outcome Outcome) error {
		close(settling)
		<-allowSettlement
		return owner.finishTeamMember(r, selected, outcome)
	}
	settled := make(chan struct{})
	go func() {
		owner.tree.finish(r, pending, Outcome{State: Paused})
		close(settled)
	}()
	<-settling
	owner.tree.cancelPending(pending)
	waitForPeerWakeWaiter(t, owner)
	select {
	case <-started:
		t.Fatal("peer wake started before the prior settlement finished")
	default:
	}
	close(allowSettlement)
	settlementReleased = true
	<-settled
	owner.tree.teamFinished = owner.finishTeamMember
	select {
	case selected := <-started:
		if selected == pending || selected.peerWake == nil || selected.peerWake.Recipient != recipient.Ref {
			t.Fatalf("unexpected wake after settlement: %#v", selected)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("peer wake did not resume after settlement finished")
	}
}

func TestCancelledTeamAdmissionReleasesItsReservation(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "alice", workspace)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	owner := &owner{tree: tree, teamStore: store}
	tree.teamCanceled = owner.cancelTeamMember
	worker := AgentDefinition{Role: "worker", Tools: []string{"read"}, CanDelegate: true}
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	tree.cancelPending(selected)
	tree.cancelPending(selected)
	_, members, err := store.Snapshot()
	if err != nil || len(members) != 0 {
		t.Fatalf("cancelled admission retained a member: %#v, %v", members, err)
	}
	if _, err := tree.selectRun(member.Ref.ID); err == nil {
		t.Fatal("cancelled admission remained selectable")
	}
	replacement, err := store.Reserve(storeTestUUID(4), string(storeTestUUID(5)), "alice", workspace)
	if err != nil || replacement.Name != "alice" {
		t.Fatalf("cancelled admission did not release its name: %#v, %v", replacement, err)
	}
}

func TestPeerWakeWaitingForRoomStopsOnPauseOrRootClosure(t *testing.T) {
	for _, action := range []string{"pause", "close"} {
		t.Run(action, func(t *testing.T) {
			owner, _, _, recipient, _, previous, started, release := peerWakeTestOwner(t, true)
			defer close(release)
			owner.tree.mu.Lock()
			owner.tree.config.Concurrency = 1
			owner.tree.live = 1
			owner.tree.mu.Unlock()
			owner.requestPeerWake(recipient.Ref)
			owner.requestPeerWake(recipient.Ref)
			waitForPeerWakeWaiter(t, owner)
			if action == "pause" {
				if _, _, err := owner.tree.stop(nil, previous); err != nil {
					t.Fatal(err)
				}
			} else {
				owner.tree.close()
			}
			finished := make(chan struct{})
			go func() { owner.tree.peerWakeWG.Wait(); close(finished) }()
			select {
			case <-finished:
			case <-time.After(3 * time.Second):
				t.Fatal("wake did not stop while waiting for capacity")
			}
			select {
			case <-started:
				t.Fatal("paused or closed recipient started")
			default:
			}
		})
	}
}
