//go:build linux || darwin

package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLocalPeerReceiptQueryRequiresDurablePublication(t *testing.T) {
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
	message := localMessage{ID: "sync-failure", Text: "Keep this message.", Summary: "Durability"}
	body := peerBody{Text: message.Text, Summary: message.Summary}
	encodedBody, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	receipt := localReceipt{Envelope: localEnvelope{Sender: source.ref, Recipient: target.ref, MessageID: message.ID, BodySHA256: localDigest(encodedBody)}, Body: body, State: peerAccepted}
	encodedReceipt, err := json.Marshal(receipt)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(target.stateDir, 0o300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(target.stateDir, 0o700)
	path := target.localReceiptPath(source.ref, message.ID)
	if err := publishPeerFile(path, encodedReceipt, false); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("published receipt with inaccessible directory sync = %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("receipt was not visible after the sync failure: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if result, err := source.send(ctx, target.ref, message); err != nil || result.Status != "uncertain" {
		t.Fatalf("resend before directory sync = %+v, %v", result, err)
	}
	if result, err := source.receipt(ctx, target.ref, message.ID); err != nil || result.Status != "uncertain" {
		t.Fatalf("receipt query before directory sync = %+v, %v", result, err)
	}
	if err := os.Chmod(target.stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if result, err := source.receipt(ctx, target.ref, message.ID); err != nil || result.Status != "accepted" || result.Receipt == nil || result.Receipt.Envelope != receipt.Envelope {
		t.Fatalf("receipt query after directory sync = %+v, %v", result, err)
	}
	if result, err := source.send(ctx, target.ref, message); err != nil || result.Status != "accepted" || result.Receipt == nil || result.Receipt.Envelope != receipt.Envelope {
		t.Fatalf("resend after directory sync = %+v, %v", result, err)
	}
}

func TestLocalPeerIdleNoticeReachesHealthySubscriberBehindStalledPeer(t *testing.T) {
	t.Run("first subscriber stalls", func(t *testing.T) { testLocalPeerIdleNoticeWithStalledSubscriber(t, true) })
	t.Run("second subscriber stalls", func(t *testing.T) { testLocalPeerIdleNoticeWithStalledSubscriber(t, false) })
}

func testLocalPeerIdleNoticeWithStalledSubscriber(t *testing.T, stallFirst bool) {
	t.Helper()
	root, err := os.MkdirTemp("", "ln-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	target, err := startLocalPeer(fixtureOptions(root, localFixtureTarget, "target", true))
	if err != nil {
		t.Fatal(err)
	}
	defer target.close()
	sourceA, err := startLocalPeer(fixtureOptions(root, localFixtureSource, "source-a", true))
	if err != nil {
		t.Fatal(err)
	}
	defer sourceA.close()
	sourceB, err := startLocalPeer(fixtureOptions(root, ChildID("33333333-3333-7333-8333-333333333333"), "source-b", true))
	if err != nil {
		t.Fatal(err)
	}
	defer sourceB.close()
	if err := target.setLocalActivity(localActivityBusy); err != nil {
		t.Fatal(err)
	}
	expires := time.Now().Add(30 * time.Second)
	for _, source := range []*localPeer{sourceA, sourceB} {
		result, err := source.subscribe(context.Background(), target.ref, "idle", expires)
		if err != nil || result.Status != "subscribed" {
			t.Fatalf("subscribe = %+v, %v", result, err)
		}
	}
	entries, err := boundedLocalStateEntries(target.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	var first localSubscription
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "subin-") {
			record, err := readLocalSubscription(filepath.Join(target.stateDir, entry.Name()))
			if err != nil {
				t.Fatal(err)
			}
			first = record.Subscription
			break
		}
	}
	stalled, healthy := sourceA, sourceB
	if first.Source == sourceB.ref {
		stalled, healthy = sourceB, sourceA
	} else if first.Source != sourceA.ref {
		t.Fatalf("unexpected first subscription = %+v", first)
	}
	if !stallFirst {
		stalled, healthy = healthy, stalled
	}
	socket := filepath.Join(stalled.registryDir, stalled.socketName())
	forward := socket + "-held"
	if err := os.Rename(socket, forward); err != nil {
		t.Fatal(err)
	}
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socket, 0o600); err != nil {
		t.Fatal(err)
	}
	arrived := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			conn, err := listener.AcceptUnix()
			if err != nil {
				return
			}
			if _, err := readLocalFrame(conn); err == nil {
				select {
				case arrived <- struct{}{}:
				default:
				}
				_, _ = io.Copy(io.Discard, conn)
			}
			_ = conn.Close()
		}
	}()
	defer func() {
		_ = listener.Close()
		<-done
		_ = os.Rename(forward, socket)
	}()
	if err := target.setLocalActivity(localActivityIdle); err != nil {
		t.Fatal(err)
	}
	select {
	case <-arrived:
	case <-time.After(4 * time.Second):
		t.Fatal("the stalled subscriber did not receive a notice request")
	}
	entries, err = boundedLocalStateEntries(target.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "subin-") {
			record, err := readLocalSubscription(filepath.Join(target.stateDir, entry.Name()))
			if err != nil || record.Subscription.Source != first.Source {
				t.Fatalf("fixture changed subscription scan order: %+v, %v", record, err)
			}
			break
		}
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		notice, found, err := healthy.offerLocal()
		if err != nil {
			t.Fatal(err)
		}
		if found {
			if notice.Envelope.Sender != target.ref || !strings.Contains(notice.Body.Text, "became idle") {
				t.Fatalf("healthy subscriber notice = %+v", notice)
			}
			closed := make(chan error, 1)
			go func() { closed <- target.close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(4 * time.Second):
				t.Fatal("shutdown did not finish while a subscriber stalled")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	entries, _ = boundedLocalStateEntries(target.stateDir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "subin-") {
			record, readErr := readLocalSubscription(filepath.Join(target.stateDir, entry.Name()))
			t.Logf("subscription %s = %+v, %v", entry.Name(), record, readErr)
		}
	}
	t.Fatalf("a stalled subscriber prevented the healthy subscriber from receiving its idle notice: %+v", healthy.localSubscriptionStatus(target.ref, "idle"))
}

func TestLocalPeerRequiresDurableNewStateDirectory(t *testing.T) {
	root, err := os.MkdirTemp("", "ld-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	parent := filepath.Join(root, "parent")
	if err := os.Mkdir(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(parent, 0o300); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(parent, 0o700)
	options := fixtureOptions(root, localFixtureTarget, "target", true)
	options.StateDir = filepath.Join(parent, "new", "state")
	target, err := startLocalPeer(options)
	if target != nil {
		_ = target.close()
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("startup without parent directory sync = %v", err)
	}
	if _, err := os.Stat(filepath.Join(parent, "new")); err != nil {
		t.Fatalf("created directory was not visible after the failed sync: %v", err)
	}
	target, err = startLocalPeer(options)
	if target != nil {
		_ = target.close()
	}
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("retry without parent directory sync = %v", err)
	}
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	target, err = startLocalPeer(options)
	if err != nil {
		t.Fatalf("startup after parent directory recovery = %v", err)
	}
	defer target.close()
	source, err := startLocalPeer(fixtureOptions(root, localFixtureSource, "source", true))
	if err != nil {
		t.Fatal(err)
	}
	defer source.close()
	message := localMessage{ID: "durable-directory", Text: "Ready.", Summary: "Ready"}
	result, err := source.send(context.Background(), target.ref, message)
	if err != nil || result.Status != "accepted" {
		t.Fatalf("send after parent directory recovery = %+v, %v", result, err)
	}
}

func TestLocalPeerDirectoryValidationKeepsPrivateAndSymlinkChecks(t *testing.T) {
	root, err := os.MkdirTemp("", "lv-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	private := filepath.Join(root, "private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareLocalDirectory(link); err == nil {
		t.Fatal("a symlink was accepted as the private directory")
	}
	if err := os.Chmod(private, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := prepareLocalDirectory(private); err == nil {
		t.Fatal("a non-private directory was accepted")
	}
	if err := os.Chmod(private, 0o700); err != nil {
		t.Fatal(err)
	}
	if got, err := prepareLocalDirectory(private); err != nil || got != private {
		t.Fatalf("private directory = %q, %v", got, err)
	}
}
