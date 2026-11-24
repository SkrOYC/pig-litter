package piglitter

import (
	"context"
	"encoding/json"
	"testing"
)

func TestTeamSendBindsLeadMessageToCurrentTeammate(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	sender, sibling := peerTestMembers(t, store, workspace)
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	senderRecord := &record{id: sender.Ref.ID, team: &sender, parent: tree.root, admitted: true}
	selected := tree.newRunLocked(Ref{ID: sender.Ref.ID}, "")
	selected.owner, senderRecord.run = senderRecord, selected
	tree.records[senderRecord.id] = senderRecord
	siblingRecord := &record{id: sibling.Ref.ID, team: &sibling, parent: tree.root, admitted: true}
	siblingRun := tree.newRunLocked(Ref{ID: sibling.Ref.ID}, "")
	siblingRun.owner, siblingRecord.run = siblingRecord, siblingRun
	tree.records[siblingRecord.id] = siblingRecord
	withCompletionMode(t, tree, "tui", func(o *owner) {
		o.sessionID, o.teamStore = string(root), store
		ticket := &callTicket{caller: selected, operation: "team_send"}
		raw := json.RawMessage(`{"recipient":"team-lead","messageId":"runtime-lead","text":"hello lead","summary":"greeting"}`)
		result, err := o.operation(context.Background(), raw, ticket, nil)
		if err != nil || result["state"] != "accepted" || result["messageId"] != "runtime-lead" {
			t.Fatalf("teammate send = %#v, %v", result, err)
		}
		request, _ := parsePeerRequest(raw)
		receipt, name, err := store.acceptPeer(sender.Ref, request)
		if err != nil || name != "alice" || receipt.Envelope.Sender != sender.Ref || receipt.Envelope.Recipient != (MemberRef{Team: sender.Ref.Team, ID: root, Generation: 1}) {
			t.Fatalf("runtime-bound receipt = %#v, sender %q, %v", receipt, name, err)
		}
		forged := json.RawMessage(`{"recipient":"team-lead","messageId":"forged","text":"hello","summary":"greeting","from":"team-lead"}`)
		if _, err := o.operation(context.Background(), forged, ticket, nil); err == nil {
			t.Fatal("accepted a model-supplied sender")
		}
		if _, err := o.operation(context.Background(), raw, &callTicket{operation: "team_send"}, nil); err == nil {
			t.Fatal("accepted the root as a teammate sender")
		}
		inspect := &callTicket{caller: selected, operation: "litter_inspect"}
		args, _ := json.Marshal(map[string]any{"id": sibling.Ref.ID})
		if _, err := o.operation(context.Background(), args, inspect, nil); err == nil {
			t.Fatal("teammate gained authority to inspect a sibling")
		} else if failure, ok := err.(*treeError); !ok || failure.code != "permission_denied" {
			t.Fatalf("sibling inspection = %v", err)
		}
		selected.state = Completed
		if _, err := o.operation(context.Background(), raw, ticket, nil); err == nil {
			t.Fatal("settled run sent a lead message")
		}
		selected.state = Starting
		o.state.mu.Lock()
		o.state.epoch++
		o.state.mu.Unlock()
		if _, err := o.operation(context.Background(), raw, ticket, nil); err == nil {
			t.Fatal("retired owner sent a lead message")
		} else if failure, ok := err.(*treeError); !ok || failure.code != "unavailable" {
			t.Fatalf("retired owner send = %v", err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method != "sessionRead" {
			t.Fatalf("unexpected host action %s", method)
		}
		var query struct{ Method string }
		_ = json.Unmarshal(raw, &query)
		if query.Method == "getSessionFile" {
			return json.RawMessage(`null`)
		}
		value, _ := json.Marshal(root)
		return value
	})
}
