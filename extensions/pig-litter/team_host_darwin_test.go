package piglitter

import (
	"os/exec"
	"testing"
)

func darwinTestHost(t *testing.T) (peerHostIdentity, func()) {
	t.Helper()
	cmd := exec.Command("/bin/sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stop := func() {
		if cmd.ProcessState == nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
	}
	t.Cleanup(stop)
	identity, err := peerObservedHost(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	return identity, stop
}

func TestDarwinPeerHostGone(t *testing.T) {
	prior, stop := darwinTestHost(t)
	current, err := embeddedPeerHost()
	if err != nil {
		t.Fatal(err)
	}
	if peerHostGone(prior, current) || peerHostGone(current, current) {
		t.Fatal("classified a live host as finished")
	}
	invalid := []peerHostIdentity{
		{},
		{PID: 1 << 31, Birth: prior.Birth, Scope: prior.Scope},
		{PID: prior.PID, Birth: "darwin:0:0", Scope: prior.Scope},
		{PID: prior.PID, Birth: "darwin:01:1", Scope: prior.Scope},
		{PID: prior.PID, Birth: "darwin:1:1000000", Scope: prior.Scope},
		{PID: prior.PID, Birth: "darwin:1:0:extra", Scope: prior.Scope},
		{PID: prior.PID, Birth: prior.Birth, Scope: prior.Scope + ":unknown"},
	}
	wrongCurrent := current
	wrongCurrent.Birth = "darwin:1:0"
	assertUnknown := func() {
		t.Helper()
		for _, identity := range invalid {
			if peerHostGone(identity, current) {
				t.Fatalf("classified an unknown prior identity as finished: %#v", identity)
			}
		}
		if peerHostGone(prior, wrongCurrent) {
			t.Fatal("accepted an unverified current host")
		}
	}
	assertUnknown()
	reused := prior
	reused.Birth = "darwin:1:0"
	if !peerHostGone(reused, current) {
		t.Fatal("did not recognize a different birth for the live PID")
	}
	stop()
	assertUnknown()
	if !peerHostGone(prior, current) {
		t.Fatal("did not recognize the reaped host")
	}
}

func TestDarwinPeerRecovery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		finished bool
		entry    bool
	}{
		{name: "live-without-entry"},
		{name: "live-with-entry", entry: true},
		{name: "finished-without-entry", finished: true},
		{name: "finished-with-entry", finished: true, entry: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, dir, root, workspace := storeTestOpen(t)
			sender, _ := peerTestMembers(t, store, workspace)
			manager := nativeTestManager(t, t.TempDir(), string(root))
			file := *manager.GetSessionFile()
			lead := MemberRef{Team: sender.Ref.Team, ID: root, Generation: 1}
			request := peerRequest{Recipient: "team-lead", MessageID: "darwin-recovery", Text: "hello lead", Summary: "greeting"}
			if _, _, err := store.acceptPeer(sender.Ref, request); err != nil {
				t.Fatal(err)
			}
			prior, stop := darwinTestHost(t)
			current, err := embeddedPeerHost()
			if err != nil {
				t.Fatal(err)
			}
			old := peerDeliveryAttempt{Host: prior, SessionFile: file}
			receipt, content, found, err := store.offerPeerAttempt(lead, &old)
			if err != nil || !found || receipt.State != peerAttempted {
				t.Fatalf("initial offer = %#v, found=%v, %v", receipt, found, err)
			}
			var entryID string
			if tc.entry {
				entryID, err = manager.AppendCustomMessageEntry("pig_litter_peer", content, true, map[string]any{"envelope": receipt.Envelope})
				if err != nil {
					t.Fatal(err)
				}
			}
			if tc.finished {
				stop()
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := openTeamStore(dir, root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			attempt := peerDeliveryAttempt{Host: current, SessionFile: file}
			if _, _, found, err := reopened.offerPeerAttempt(lead, &attempt); err != nil || found {
				t.Fatalf("offer before sender authentication: found=%v, %v", found, err)
			}
			if _, _, err := reopened.acceptPeer(sender.Ref, request); err != nil {
				t.Fatal(err)
			}
			retry, retryContent, found, err := reopened.offerPeerAttempt(lead, &attempt)
			if err != nil {
				t.Fatal(err)
			}
			if tc.finished && !tc.entry {
				if !found || retry.State != peerAttempted || retry.Attempt == nil || retry.Attempt.Host != current || retryContent != content {
					t.Fatalf("finished host retry = %#v, found=%v", retry, found)
				}
				entryID, err = manager.AppendCustomMessageEntry("pig_litter_peer", retryContent, true, map[string]any{"envelope": retry.Envelope})
				if err != nil {
					t.Fatal(err)
				}
				if err := reopened.observePeers(lead, file); err != nil {
					t.Fatal(err)
				}
			} else if found {
				t.Fatal("reoffered an entry or an uncertain attempt")
			}
			result, _, err := reopened.acceptPeer(sender.Ref, request)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.finished && !tc.entry {
				if result.State != peerAttempted {
					t.Fatalf("live attempt = %#v", result)
				}
				return
			}
			if result.State != peerObserved || result.NativeEntryID != entryID || result.SessionID != root {
				t.Fatalf("observed receipt = %#v, expected entry %q", result, entryID)
			}
			if id, err := nativePeerEntry(file, receipt.Envelope, content); err != nil || id != entryID {
				t.Fatalf("native entry = %q, %v", id, err)
			}
		})
	}
}

func TestDarwinPeerUnknownAttemptRemainsPending(t *testing.T) {
	for _, kind := range []string{"legacy", "unknown-scope"} {
		t.Run(kind, func(t *testing.T) {
			store, _, root, workspace := storeTestOpen(t)
			sender, _ := peerTestMembers(t, store, workspace)
			manager := nativeTestManager(t, t.TempDir(), string(root))
			file := *manager.GetSessionFile()
			lead := MemberRef{Team: sender.Ref.Team, ID: root, Generation: 1}
			request := peerRequest{Recipient: "team-lead", MessageID: "darwin-unknown", Text: "hello lead", Summary: "greeting"}
			if _, _, err := store.acceptPeer(sender.Ref, request); err != nil {
				t.Fatal(err)
			}
			if kind == "legacy" {
				if _, _, found, err := store.offerPeer(lead); err != nil || !found {
					t.Fatalf("legacy offer: found=%v, %v", found, err)
				}
			} else {
				prior, stop := darwinTestHost(t)
				prior.Scope += ":unknown"
				old := peerDeliveryAttempt{Host: prior, SessionFile: file}
				if _, _, found, err := store.offerPeerAttempt(lead, &old); err != nil || !found {
					t.Fatalf("unknown-scope offer: found=%v, %v", found, err)
				}
				stop()
			}
			current, err := embeddedPeerHost()
			if err != nil {
				t.Fatal(err)
			}
			attempt := peerDeliveryAttempt{Host: current, SessionFile: file}
			if _, _, found, err := store.offerPeerAttempt(lead, &attempt); err != nil || found {
				t.Fatalf("unknown attempt reoffered: found=%v, %v", found, err)
			}
			result, _, err := store.acceptPeer(sender.Ref, request)
			if err != nil || result.State != peerAttempted {
				t.Fatalf("unknown receipt = %#v, %v", result, err)
			}
		})
	}
}
