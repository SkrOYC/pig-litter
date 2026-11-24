//go:build linux || darwin

package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalSessionUserOptIn(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PIG_CODING_AGENT_DIR", dir)
	t.Setenv("PIG_LITTER_LOCAL_SESSIONS", "")
	if err := os.Unsetenv("PIG_LITTER_LOCAL_SESSIONS"); err != nil {
		t.Fatal(err)
	}
	if enabled, accept, err := localOptIn(); enabled || accept || err != nil {
		t.Fatalf("missing user policy = %t, %t, %v", enabled, accept, err)
	}
	path := filepath.Join(dir, "local-sessions.json")
	if err := os.WriteFile(path, []byte(`{"mode":"deny"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if enabled, accept, err := localOptIn(); !enabled || accept || err != nil {
		t.Fatalf("user denial = %t, %t, %v", enabled, accept, err)
	}
	if err := os.Setenv("PIG_LITTER_LOCAL_SESSIONS", "accept"); err != nil {
		t.Fatal(err)
	}
	if enabled, accept, err := localOptIn(); !enabled || !accept || err != nil {
		t.Fatalf("process acceptance = %t, %t, %v", enabled, accept, err)
	}
	if err := os.Setenv("PIG_LITTER_LOCAL_SESSIONS", "off"); err != nil {
		t.Fatal(err)
	}
	if enabled, _, err := localOptIn(); enabled || err != nil {
		t.Fatalf("process opt-out = %t, %v", enabled, err)
	}
	if err := os.Unsetenv("PIG_LITTER_LOCAL_SESSIONS"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"mode":"accept","mode":"deny"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := localOptIn(); err == nil {
		t.Fatal("accepted duplicate policy keys")
	}
}

func TestLocalRootRoutingAndStablePin(t *testing.T) {
	root, err := os.MkdirTemp("", "lr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	source, err := startLocalPeer(fixtureOptions(root, localFixtureSource, "source", true))
	if err != nil {
		t.Fatal(err)
	}
	defer source.close()
	target, err := startLocalPeer(fixtureOptions(root, localFixtureTarget, "target", true))
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	tree := newTree(defaultConfig())
	defer tree.close()
	owner := &owner{tree: tree, local: &localRuntime{peer: source}}
	view := owner.localDiscovery("")
	rows, ok := view["sessions"].([]map[string]any)
	if !ok || len(rows) != 1 || rows[0]["kind"] != "independent_session" || rows[0]["name"] != "target" || rows[0]["ref"] != target.ref.String() {
		t.Fatalf("peer discovery = %#v", view)
	}
	request := json.RawMessage(`{"recipient":"local:target","messageId":"m1","text":"hello","summary":"greeting"}`)
	value, err := owner.addressedSend(context.Background(), request)
	if err != nil || value["state"] != "accepted" || value["recipient"] != target.ref.String() {
		t.Fatalf("local send = %#v, %v", value, err)
	}
	again, err := owner.addressedSend(context.Background(), request)
	if err != nil || again["state"] != "accepted" {
		t.Fatalf("same ID retry = %#v, %v", again, err)
	}
	target.mu.Lock()
	target.name = "renamed"
	err = target.writePresence()
	target.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	pinned, err := owner.resolveLocal(target.ref.String())
	if err != nil || pinned != target.ref {
		t.Fatalf("renamed exact pin = %s, %v", pinned.String(), err)
	}
	if _, err := owner.resolveLocal("local:target"); err == nil {
		t.Fatal("old name resolved after rename")
	}
	if _, err := owner.resolveLocal(target.ref.String() + "x"); err == nil {
		t.Fatal("malformed pin fell back to a name")
	}
	if _, err := owner.addressedSend(context.Background(), json.RawMessage(`{"recipient":"local:renamed","messageId":"m2","text":"hi","summary":"hi","from":"system"}`)); err == nil {
		t.Fatal("accepted claimed sender")
	}
	if value, err := owner.addressedSend(context.Background(), json.RawMessage(`{"recipient":"cloud:any","messageId":"m3","text":"hi","summary":"hi"}`)); err != nil || value["state"] != "unsupported" {
		t.Fatalf("cloud route = %#v, %v", value, err)
	}

	childID := ChildID("33333333-3333-7333-8333-333333333333")
	childRun := &run{ref: Ref{ID: childID}, state: Completed}
	child := &record{id: childID, parent: tree.root, name: "renamed", run: childRun}
	childRun.owner = child
	tree.records[childID] = child
	defer delete(tree.records, childID)
	view = owner.localDiscovery("")
	rows = view["sessions"].([]map[string]any)
	collisions := rows[0]["collisions"].([]string)
	if len(collisions) != 1 || collisions[0] != "child" {
		t.Fatalf("collision disclosure = %#v", view)
	}
	_, err = owner.addressedSend(context.Background(), json.RawMessage(`{"recipient":"renamed","messageId":"m4","text":"hi","summary":"hi"}`))
	if err == nil || !strings.Contains(err.Error(), "child is not live") {
		t.Fatalf("bare child precedence = %v", err)
	}
	pinnedRequest, _ := json.Marshal(peerRequest{Recipient: target.ref.String(), MessageID: "m5", Text: "hi", Summary: "hi"})
	value, err = owner.addressedSend(context.Background(), pinnedRequest)
	if err != nil || value["state"] != "accepted" {
		t.Fatalf("exact local pin with collision = %#v, %v", value, err)
	}
}

func TestLocalRootSubscriptionRetriesAndDenial(t *testing.T) {
	root, err := os.MkdirTemp("", "ls-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	source, err := startLocalPeer(fixtureOptions(root, localFixtureSource, "source", true))
	if err != nil {
		t.Fatal(err)
	}
	defer source.close()
	target, err := startLocalPeer(fixtureOptions(root, localFixtureTarget, "target", true))
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	tree := newTree(defaultConfig())
	defer tree.close()
	owner := &owner{tree: tree, local: &localRuntime{peer: source}}
	if err := target.setLocalActivity(localActivityBusy); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().UTC().Add(time.Hour).Format(time.RFC3339Nano)
	raw, _ := json.Marshal(map[string]any{"recipient": target.ref.String(), "subscriptionId": "once", "expiresAt": expires})
	for attempt := 0; attempt < 2; attempt++ {
		value, err := owner.localOperation(context.Background(), "team_subscribe", raw)
		if err != nil || value["state"] != "subscribed" {
			t.Fatalf("subscription attempt %d = %#v, %v", attempt, value, err)
		}
	}
	if err := target.setLocalActivity(localActivityIdle); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		notice, found, err := source.offerLocal()
		if err != nil {
			t.Fatal(err)
		}
		if found {
			if notice.Envelope.Sender != target.ref || !strings.Contains(notice.Body.Text, "became idle") {
				t.Fatalf("idle notice = %#v", notice)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("idle notice did not arrive")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, found, err := source.offerLocal(); err != nil || found {
		t.Fatalf("duplicate notice: found %t, %v", found, err)
	}
	denied, err := startLocalPeer(fixtureOptions(root, ChildID("33333333-3333-7333-8333-333333333333"), "denied", false))
	if err != nil {
		t.Fatal(err)
	}
	defer denied.close()
	raw, _ = json.Marshal(map[string]any{"recipient": denied.ref.String(), "messageId": "denied", "text": "hi", "summary": "hi"})
	value, err := owner.localOperation(context.Background(), "team_send", raw)
	if err != nil || value["state"] != "denied" {
		t.Fatalf("inbound denial = %#v, %v", value, err)
	}
}

func TestLocalControlAndChildSubscriptionAreRefused(t *testing.T) {
	tree := testTree(t, defaultConfig())
	dir := t.TempDir()
	withRuntimeOwner(t, &runtimeHost{}, tree, dir, filepath.Join(dir, "agent"), func(owner *owner) {
		pin := "local:22222222-2222-7222-8222-222222222222@" + strings.Repeat("a", 64)
		for _, name := range []string{"litter_inspect", "litter_stop", "litter_wait", "litter_message"} {
			raw, _ := json.Marshal(map[string]any{"id": pin, "text": "hi"})
			_, err := owner.operation(context.Background(), raw, &callTicket{operation: name}, nil)
			var denied *treeError
			if !errors.As(err, &denied) || denied.code != "permission_denied" {
				t.Fatalf("%s local control = %v", name, err)
			}
		}
		id := ChildID("33333333-3333-7333-8333-333333333333")
		child := &record{id: id, parent: tree.root}
		childRun := &run{ref: Ref{ID: id}, owner: child, state: Running}
		child.run = childRun
		tree.records[id] = child
		defer delete(tree.records, id)
		raw, _ := json.Marshal(map[string]any{"recipient": pin, "subscriptionId": "sub", "expiresAt": time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)})
		_, err := owner.operation(context.Background(), raw, &callTicket{operation: "team_subscribe", caller: childRun}, nil)
		var denied *treeError
		if !errors.As(err, &denied) || denied.code != "permission_denied" {
			t.Fatalf("child subscription = %v", err)
		}
	})
}

func TestLeadMessageUsesTeamReceipts(t *testing.T) {
	store, dir, root, workspace := storeTestOpen(t)
	_, recipient := peerTestMembers(t, store, workspace)
	lead := MemberRef{Team: recipient.Ref.Team, ID: root, Generation: 1}
	request := peerRequest{Recipient: recipient.Name, MessageID: "lead-message", Text: "Please inspect the change.", Summary: "Review request"}
	receipt, sender, err := store.acceptPeerTo(lead, request, &recipient.Ref)
	if err != nil || sender != "team-lead" || receipt.State != peerAccepted || receipt.Envelope.Sender != lead {
		t.Fatalf("lead acceptance = %#v, %q, %v", receipt, sender, err)
	}
	repeated, _, err := store.acceptPeerTo(lead, request, &recipient.Ref)
	if err != nil || repeated != receipt {
		t.Fatalf("lead retry = %#v, %v", repeated, err)
	}
	stale := recipient.Ref
	stale.Generation++
	if _, _, err := store.acceptPeerTo(lead, request, &stale); err == nil {
		t.Fatal("accepted a changed teammate generation")
	}
	offered, content, found, err := store.offerPeer(recipient.Ref)
	if err != nil || !found || offered.State != peerAttempted {
		t.Fatalf("lead delivery offer = %#v, %t, %v", offered, found, err)
	}
	header, _ := json.Marshal(map[string]any{"type": "session", "id": recipient.Ref.ID})
	entry, _ := json.Marshal(map[string]any{"type": "custom_message", "id": "lead-entry", "customType": "pig_litter_peer", "content": content, "details": map[string]any{"envelope": receipt.Envelope}})
	path := filepath.Join(dir, "lead-session.jsonl")
	if err := os.WriteFile(path, append(append(append(header, '\n'), entry...), '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := store.observePeers(recipient.Ref, path); err != nil {
		t.Fatal(err)
	}
	observed, _, err := store.acceptPeerTo(lead, request, &recipient.Ref)
	if err != nil || observed.State != peerObserved || observed.NativeEntryID != "lead-entry" {
		t.Fatalf("lead observation = %#v, %v", observed, err)
	}
}
