package piglitter

import (
	"fmt"
	"testing"
)

func TestPeerMailboxReleasesCapacityAfterNativeObservation(t *testing.T) {
	store, _, _, workspace := storeTestOpen(t)
	sender, recipient := peerTestMembers(t, store, workspace)
	request := peerRequest{Recipient: "bob", Text: "hello", Summary: "greeting"}
	checkRejection := func(code string) {
		t.Helper()
		_, _, err := store.acceptPeer(sender.Ref, request)
		failure, ok := err.(*treeError)
		if !ok || failure.code != code {
			t.Fatalf("send %q: expected %s, got %v", request.MessageID, code, err)
		}
	}
	for i := range peerInboxLimit {
		request.MessageID = fmt.Sprintf("capacity-%d", i)
		if _, _, err := store.acceptPeer(sender.Ref, request); err != nil {
			t.Fatalf("accept message %d: %v", i, err)
		}
	}
	request.MessageID = "capacity-next"
	checkRejection("capacity")
	offered, content, found, err := store.offerPeer(recipient.Ref)
	if err != nil || !found || offered.State != peerAttempted {
		t.Fatalf("offer = %#v, found=%v, %v", offered, found, err)
	}
	checkRejection("capacity")
	manager := nativeTestManager(t, t.TempDir(), string(recipient.Ref.ID))
	entryID, err := manager.AppendCustomMessageEntry("pig_litter_peer", content, true, map[string]any{"envelope": offered.Envelope})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.observePeers(recipient.Ref, *manager.GetSessionFile()); err != nil {
		t.Fatal(err)
	}
	request.MessageID = offered.Envelope.MessageID
	observed, _, err := store.acceptPeer(sender.Ref, request)
	if err != nil || observed.State != peerObserved || observed.NativeEntryID != entryID {
		t.Fatalf("observed receipt retry = %#v, %v", observed, err)
	}
	request.Text = "different"
	checkRejection("id_conflict")
	request.Text = "hello"
	request.MessageID = "capacity-next"
	if accepted, _, err := store.acceptPeer(sender.Ref, request); err != nil || accepted.State != peerAccepted {
		t.Fatalf("observed message did not release capacity: %#v, %v", accepted, err)
	}
	request.MessageID = "capacity-overflow"
	checkRejection("capacity")
}
