package piglitter

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

func peerTestHost(t *testing.T) (peerHostIdentity, func()) {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stopped := false
	stop := func() {
		if !stopped {
			stopped = true
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

func TestLeadReceiptRecoversOnlyAfterPriorHostExit(t *testing.T) {
	for _, stored := range []bool{false, true} {
		name := "before-native-append"
		if stored {
			name = "after-native-append"
		}
		t.Run(name, func(t *testing.T) {
			store, dir, root, workspace := storeTestOpen(t)
			sender, _ := peerTestMembers(t, store, workspace)
			manager := nativeTestManager(t, t.TempDir(), string(root))
			file := *manager.GetSessionFile()
			lead := MemberRef{Team: sender.Ref.Team, ID: root, Generation: 1}
			request := peerRequest{Recipient: "team-lead", MessageID: "crash-cut", Text: "hello lead", Summary: "greeting"}
			if _, _, err := store.acceptPeer(sender.Ref, request); err != nil {
				t.Fatal(err)
			}
			prior, stop := peerTestHost(t)
			current, err := peerObservedHost(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			oldAttempt := peerLeadAttempt{Host: prior, SessionFile: file}
			receipt, content, found, err := store.offerPeerAttempt(lead, &oldAttempt)
			if err != nil || !found || receipt.State != peerAttempted {
				t.Fatalf("old host offer = %#v, found=%v, %v", receipt, found, err)
			}
			newAttempt := peerLeadAttempt{Host: current, SessionFile: file}
			if _, _, found, err := store.offerPeerAttempt(lead, &newAttempt); err != nil || found {
				t.Fatalf("live prior host was retried: found=%v, %v", found, err)
			}
			var nativeID string
			if stored {
				nativeID, err = manager.AppendCustomMessageEntry("pig_litter_peer", content, true, map[string]any{"envelope": receipt.Envelope})
				if err != nil {
					t.Fatal(err)
				}
			}
			stop()
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := openTeamStore(dir, root)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = reopened.Close() })
			if _, _, found, err := reopened.offerPeerAttempt(lead, &newAttempt); err != nil || found {
				t.Fatalf("retried without current sender authentication: found=%v, %v", found, err)
			}
			if _, _, err := reopened.acceptPeer(sender.Ref, request); err != nil {
				t.Fatal(err)
			}
			otherManager := nativeTestManager(t, t.TempDir(), string(root))
			wrongFile := peerLeadAttempt{Host: current, SessionFile: *otherManager.GetSessionFile()}
			if _, _, found, err := reopened.offerPeerAttempt(lead, &wrongFile); err != nil || found {
				t.Fatalf("retried with a different session file: found=%v, %v", found, err)
			}
			retry, retryContent, found, err := reopened.offerPeerAttempt(lead, &newAttempt)
			if err != nil {
				t.Fatal(err)
			}
			if stored {
				if found {
					t.Fatal("reoffered a native message already in the transcript")
				}
			} else {
				if !found || retry.State != peerAttempted || retry.Attempt == nil || retry.Attempt.Host != current || retryContent != content {
					t.Fatalf("dead host retry = %#v, found=%v", retry, found)
				}
				if _, _, found, err := reopened.offerPeerAttempt(lead, &newAttempt); err != nil || found {
					t.Fatalf("same-host retry duplicated the offer: found=%v, %v", found, err)
				}
				nativeID, err = manager.AppendCustomMessageEntry("pig_litter_peer", retryContent, true, map[string]any{"envelope": retry.Envelope})
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := reopened.observePeers(lead, file); err != nil {
				t.Fatal(err)
			}
			final, _, err := reopened.acceptPeer(sender.Ref, request)
			if err != nil || final.State != peerObserved || final.NativeEntryID != nativeID || final.SessionID != root {
				t.Fatalf("observed receipt = %#v, %v", final, err)
			}
			if id, err := nativePeerEntry(file, receipt.Envelope, content); err != nil || id != nativeID {
				t.Fatalf("native transcript = %q, %v", id, err)
			}
		})
	}
}

func TestLeadFilteredProcessAbsenceIsNotDeath(t *testing.T) {
	if os.Getenv("P04_LEAD_FILTER_CHILD") == "" {
		cmd := exec.Command(os.Args[0], "-test.run=^TestLeadFilteredProcessAbsenceIsNotDeath$", "-test.v")
		cmd.Env = append(os.Environ(), "P04_LEAD_FILTER_CHILD=1")
		output, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("filtered process fixture failed: %v\n%s", err, output)
		}
		return
	}
	prior, _ := peerTestHost(t)
	current, err := peerObservedHost(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		t.Fatal(err)
	}
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: 0},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, Jf: 1, K: unix.SYS_PIDFD_OPEN},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.ESRCH)},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
	}
	program := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, unix.SECCOMP_MODE_FILTER, uintptr(unsafe.Pointer(&program)), 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := unix.PidfdOpen(prior.PID, 0); err != unix.ESRCH {
		t.Fatalf("filtered pidfd_open = %v", err)
	}
	if peerHostGone(prior, current) {
		t.Fatal("classified a live host as dead under a seccomp filter")
	}
}

func TestLeadAcceptedReceiptSurvivesOwnerRestartBeforeHandoff(t *testing.T) {
	store, dir, root, workspace := storeTestOpen(t)
	sender, _ := peerTestMembers(t, store, workspace)
	manager := nativeTestManager(t, t.TempDir(), string(root))
	lead := MemberRef{Team: sender.Ref.Team, ID: root, Generation: 1}
	request := peerRequest{Recipient: "team-lead", MessageID: "accepted-cut", Text: "hello lead", Summary: "greeting"}
	if _, _, err := store.acceptPeer(sender.Ref, request); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTeamStore(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	host, err := peerObservedHost(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	attempt := peerLeadAttempt{Host: host, SessionFile: *manager.GetSessionFile()}
	if _, _, found, err := reopened.offerPeerAttempt(lead, &attempt); err != nil || found {
		t.Fatalf("accepted receipt offered before authenticated retry: found=%v, %v", found, err)
	}
	if _, _, err := reopened.acceptPeer(sender.Ref, request); err != nil {
		t.Fatal(err)
	}
	if receipt, _, found, err := reopened.offerPeerAttempt(lead, &attempt); err != nil || !found || receipt.State != peerAttempted {
		t.Fatalf("accepted receipt after authenticated retry = %#v, found=%v, %v", receipt, found, err)
	}
}

func TestLeadUnknownAttemptRemainsPending(t *testing.T) {
	for _, legacy := range []bool{true, false} {
		name := "scope-mismatch"
		if legacy {
			name = "missing-association"
		}
		t.Run(name, func(t *testing.T) {
			store, _, root, workspace := storeTestOpen(t)
			sender, _ := peerTestMembers(t, store, workspace)
			manager := nativeTestManager(t, t.TempDir(), string(root))
			file := *manager.GetSessionFile()
			lead := MemberRef{Team: sender.Ref.Team, ID: root, Generation: 1}
			request := peerRequest{Recipient: "team-lead", MessageID: "unknown-attempt", Text: "hello lead", Summary: "greeting"}
			if _, _, err := store.acceptPeer(sender.Ref, request); err != nil {
				t.Fatal(err)
			}
			if legacy {
				if _, _, found, err := store.offerPeer(lead); err != nil || !found {
					t.Fatalf("legacy offer found=%v, %v", found, err)
				}
			} else {
				prior, stop := peerTestHost(t)
				prior.Scope += ":different"
				old := peerLeadAttempt{Host: prior, SessionFile: file}
				if _, _, found, err := store.offerPeerAttempt(lead, &old); err != nil || !found {
					t.Fatalf("old offer found=%v, %v", found, err)
				}
				stop()
			}
			current, err := peerObservedHost(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			attempt := peerLeadAttempt{Host: current, SessionFile: file}
			if _, _, found, err := store.offerPeerAttempt(lead, &attempt); err != nil || found {
				t.Fatalf("unknown attempt was reoffered: found=%v, %v", found, err)
			}
			receipt, _, err := store.acceptPeer(sender.Ref, request)
			if err != nil || receipt.State != peerAttempted {
				t.Fatalf("unknown attempt = %#v, %v", receipt, err)
			}
		})
	}
}
