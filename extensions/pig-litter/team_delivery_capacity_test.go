package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestTeamSendWakesIdleRecipientWhenSenderReleasesCapacity(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	sender, recipient := peerTestMembers(t, store, workspace)
	if err := store.SetState(recipient.Ref, "idle"); err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, Config{Concurrency: 1, MaxMailbox: 1})
	tree.root.id = root
	senderRecord := &record{id: sender.Ref.ID, team: &sender, parent: tree.root, cwd: workspace.Cwd, admitted: true}
	senderRun := tree.newRunLocked(Ref{sender.Ref.ID}, "")
	senderRun.owner, senderRun.started, senderRun.admission = senderRecord, true, admissionReady
	senderRecord.run = senderRun
	recipientRecord := &record{id: recipient.Ref.ID, team: &recipient, parent: tree.root, cwd: workspace.Cwd, admitted: true}
	previous := tree.newRunLocked(Ref{recipient.Ref.ID}, "")
	previous.owner, previous.state, previous.started, previous.admission = recipientRecord, Completed, true, admissionReady
	previous.outcome = &Outcome{Run: previous.ref, State: Completed}
	previous.cancel()
	close(previous.done)
	close(previous.teamFinishDone)
	recipientRecord.run = previous
	tree.records[sender.Ref.ID], tree.records[recipient.Ref.ID] = senderRecord, recipientRecord
	tree.live = 1
	tree.persist = func(*record) error { return nil }
	started, release := make(chan *run, 1), make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	tree.execute = func(_ *record, selected *run) Outcome {
		_, _, _, _ = store.offerPeer(recipient.Ref)
		started <- selected
		<-release
		return Outcome{State: Completed}
	}
	withCompletionMode(t, tree, "tui", func(o *owner) {
		o.sessionID, o.teamStore = string(root), store
		tree.teamFinished = o.finishTeamMember
		ticket := &callTicket{caller: senderRun, operation: "team_send"}
		raw := json.RawMessage(`{"recipient":"bob","messageId":"capacity-1","text":"hello","summary":"greeting"}`)
		result, err := o.operation(context.Background(), raw, ticket, nil)
		if err != nil || result["state"] != "accepted" || result["delivery"] != "accepted" {
			t.Fatalf("team send = %#v, %v", result, err)
		}
		for i := 0; i < 2; i++ {
			retry, err := o.operation(context.Background(), raw, ticket, nil)
			if err != nil || retry["state"] != "accepted" {
				t.Fatalf("repeated team send = %#v, %v", retry, err)
			}
		}
		done := make(chan struct{})
		go func() { tree.peerWakeWG.Wait(); close(done) }()
		target := fmt.Sprintf("(*owner).drivePeerWake(%p,", o)
		deadline := time.Now().Add(5 * time.Second)
		for {
			select {
			case <-done:
				goto wakeSettled
			default:
			}
			buffer := make([]byte, 1<<20)
			n := runtime.Stack(buffer, true)
			for _, stack := range strings.Split(string(buffer[:n]), "\n\n") {
				if strings.Contains(stack, "[select]") && strings.Contains(stack, target) {
					goto wakeSettled
				}
			}
			if time.Now().After(deadline) {
				t.Fatal("wake did not settle while capacity was occupied")
			}
			time.Sleep(time.Millisecond)
		}
	wakeSettled:
		select {
		case <-started:
			t.Fatal("recipient started while the sender held the only slot")
		default:
		}
		tree.finish(senderRecord, senderRun, Outcome{State: Completed})
		select {
		case selected := <-started:
			if selected.peerWake == nil || selected.peerWake.Recipient != recipient.Ref {
				t.Fatalf("unexpected wake run = %#v", selected)
			}
			close(release)
			released = true
			tree.peerWakeWG.Wait()
			select {
			case <-started:
				t.Error("repeated sends started a duplicate wake")
			default:
			}
		case <-time.After(time.Second):
			t.Error("accepted peer message remained stranded after the sender released capacity")
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		switch method {
		case "sessionRead":
			var query struct{ Method string }
			if err := json.Unmarshal(raw, &query); err != nil {
				t.Errorf("invalid session read: %v", err)
				return nil
			}
			if query.Method == "getEntries" {
				return json.RawMessage(`[]`)
			}
			value, _ := json.Marshal(root)
			return value
		case "sendMessage":
			var args struct {
				Message struct {
					CustomType string `json:"customType"`
					Details    struct {
						Notice turnNotice `json:"notice"`
					} `json:"details"`
				} `json:"message"`
				Options struct {
					TriggerTurn *bool `json:"triggerTurn"`
				} `json:"options"`
			}
			if err := json.Unmarshal(raw, &args); err != nil || args.Message.CustomType != "pig_litter_team_turn" || args.Options.TriggerTurn == nil || *args.Options.TriggerTurn || (args.Message.Details.Notice.Member != sender.Ref && args.Message.Details.Notice.Member != recipient.Ref) {
				t.Errorf("unexpected host message %s: %v", raw, err)
			}
			return json.RawMessage(`{}`)
		default:
			t.Errorf("unexpected host call %s", method)
			return nil
		}
	})
}
