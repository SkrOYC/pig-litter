//go:build linux || darwin

package piglitter

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	localFixtureSource = ChildID("11111111-1111-7111-8111-111111111111")
	localFixtureTarget = ChildID("22222222-2222-7222-8222-222222222222")
)

func fixtureOptions(root string, id ChildID, name string, accept bool) localPeerOptions {
	return localPeerOptions{RegistryDir: filepath.Join(root, "r"), StateDir: filepath.Join(root, "s"), Session: id, SessionFile: filepath.Join(root, string(id)+".jsonl"), CWD: root, Name: name, AcceptInbound: accept}
}

func TestLocalPeerFixtureProcess(t *testing.T) {
	root := os.Getenv("LOCAL_PEER_FIXTURE_ROOT")
	if root == "" {
		return
	}
	options := fixtureOptions(root, localFixtureTarget, "target", os.Getenv("LOCAL_PEER_FIXTURE_ACCEPT") == "1")
	options.NativeHostPID = os.Getpid()
	peer, err := startLocalPeer(options)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	defer peer.close()
	fmt.Println(peer.ref.String())
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		switch scanner.Text() {
		case "offer":
			_, found, err := peer.offerLocal()
			fmt.Printf("offer %t %v\n", found, err)
		case "busy":
			fmt.Printf("busy %v\n", peer.setLocalActivity(localActivityBusy))
		case "idle":
			fmt.Printf("idle %v\n", peer.setLocalActivity(localActivityIdle))
		case "unlink":
			fmt.Printf("unlink %v\n", os.Remove(filepath.Join(peer.registryDir, peer.socketName())))
		case "close":
			return
		}
	}
}

type fixtureProcess struct {
	cmd   *exec.Cmd
	input io.WriteCloser
	lines *bufio.Scanner
	ref   localSessionRef
}

func startFixtureProcess(t *testing.T, root string, accept bool) *fixtureProcess {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLocalPeerFixtureProcess$")
	accepted := "0"
	if accept {
		accepted = "1"
	}
	cmd.Env = append(os.Environ(), "LOCAL_PEER_FIXTURE_ROOT="+root, "LOCAL_PEER_FIXTURE_ACCEPT="+accepted)
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	lines := bufio.NewScanner(output)
	if !lines.Scan() {
		t.Fatalf("fixture did not publish its identity: %v", lines.Err())
	}
	ref, err := parseLocalSessionRef(lines.Text())
	if err != nil {
		t.Fatal(err)
	}
	return &fixtureProcess{cmd: cmd, input: input, lines: lines, ref: ref}
}

func (p *fixtureProcess) command(t *testing.T, command string) string {
	t.Helper()
	if _, err := fmt.Fprintln(p.input, command); err != nil {
		t.Fatal(err)
	}
	if !p.lines.Scan() {
		t.Fatalf("fixture command failed: %v", p.lines.Err())
	}
	return p.lines.Text()
}

func (p *fixtureProcess) close(t *testing.T) {
	t.Helper()
	if _, err := fmt.Fprintln(p.input, "close"); err != nil {
		t.Fatal(err)
	}
	if err := p.input.Close(); err != nil {
		t.Fatal(err)
	}
	if err := p.cmd.Wait(); err != nil {
		t.Fatal(err)
	}
}

func TestLocalPeerAcrossProcesses(t *testing.T) {
	root, err := os.MkdirTemp("", "lp-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	peer, err := startLocalPeer(fixtureOptions(root, localFixtureSource, "source", true))
	if err != nil {
		t.Fatal(err)
	}
	defer peer.close()
	child := startFixtureProcess(t, root, true)
	defer func() {
		if child != nil {
			child.close(t)
		}
	}()
	page, err := peer.discover("")
	if err != nil || len(page.Rows) != 1 || page.Rows[0].Ref != child.ref || len(page.Issues) != 0 {
		t.Fatalf("discovery = %+v, %v", page, err)
	}
	if _, err := startLocalPeer(fixtureOptions(root, localFixtureSource, "duplicate", true)); err == nil {
		t.Fatal("another owner acquired the same retained session")
	}
	message := localMessage{ID: "lost-ack", Text: "Review the parser.", Summary: "Parser review"}
	presence, err := peer.lookup(child.ref)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(message)
	packet := localPacket{From: peer.ref, To: child.ref, Operation: "send", Body: body}
	signed, _ := localPacketBytes(packet)
	packet.Signature = localSign(peer.privateKey, "pig-litter-request-v1", signed)
	data, _ := json.Marshal(packet)
	conn, err := net.Dial("unix", filepath.Join(root, "r", presence.Socket))
	if err != nil {
		t.Fatal(err)
	}
	if err := writeLocalFrame(conn, data); err != nil {
		t.Fatal(err)
	}
	if err := conn.Close(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var first localResult
	for {
		first, err = peer.receipt(ctx, child.ref, message.ID)
		if err == nil && first.Status == "accepted" {
			break
		}
		if ctx.Err() != nil {
			t.Fatalf("receipt after lost acknowledgement = %+v, %v", first, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	retry, err := peer.send(ctx, child.ref, message)
	if err != nil || retry.Status != "accepted" || retry.Receipt == nil || retry.Receipt.Envelope != first.Receipt.Envelope {
		t.Fatalf("idempotent retry = %+v, %v", retry, err)
	}
	conflict := message
	conflict.Text = "Review a different parser."
	if result, err := peer.send(ctx, child.ref, conflict); err != nil || result.Status != "id_conflict" {
		t.Fatalf("same ID with new text = %+v, %v", result, err)
	}
	if result, err := peer.exchange(ctx, child.ref, "pause", struct{}{}); err != nil || result.Status != "unsupported" {
		t.Fatalf("peer control = %+v, %v", result, err)
	}
	if got := child.command(t, "offer"); got != "offer true <nil>" {
		t.Fatalf("offer = %q", got)
	}
	if result, err := peer.receipt(ctx, child.ref, message.ID); err != nil || result.Status != "uncertain" || result.Receipt.State != peerAttempted {
		t.Fatalf("attempted receipt = %+v, %v", result, err)
	}
	retained := child.ref
	child.close(t)
	child = startFixtureProcess(t, root, true)
	if child.ref != retained {
		t.Fatalf("reopen changed identity from %s to %s", retained, child.ref)
	}
	if result, err := peer.send(ctx, child.ref, message); err != nil || result.Status != "uncertain" || result.Receipt.State != peerAttempted {
		t.Fatalf("retry after reopen = %+v, %v", result, err)
	}
	if got := child.command(t, "offer"); got != "offer false <nil>" {
		t.Fatalf("attempt was replayed after restart: %q", got)
	}
	if _, err := peer.send(ctx, child.ref, localMessage{ID: "large", Text: strings.Repeat("x", 16385), Summary: "Large"}); err == nil {
		t.Fatal("oversized message was accepted")
	}
	wrong := child.ref
	wrong.Key = strings.Repeat("0", 64)
	if _, err := peer.send(ctx, wrong, message); !errors.Is(err, errLocalStale) {
		t.Fatalf("changed identity = %v", err)
	}
}

func TestLocalPeerNativeObservation(t *testing.T) {
	root, err := os.MkdirTemp("", "ln-")
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
	message := localMessage{ID: "observation", Text: "The result is ready.", Summary: "Result"}
	result, err := source.send(context.Background(), target.ref, message)
	if err != nil || result.Status != "accepted" {
		t.Fatalf("send = %+v, %v", result, err)
	}
	receipt, found, err := target.offerLocal()
	if err != nil || !found {
		t.Fatalf("offer = %+v, %t, %v", receipt, found, err)
	}
	if _, err := target.observeLocal(receipt); !errors.Is(err, errLocalUncertain) {
		t.Fatalf("missing native file = %v", err)
	}
	content, err := localMessageContent(receipt)
	if err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]any{"type": "session", "id": string(target.ref.Session)})
	entry, _ := json.Marshal(map[string]any{"type": "custom_message", "id": "entry-1", "customType": "pig_litter_local_peer", "content": content, "details": map[string]any{"envelope": receipt.Envelope}})
	file := append(append([]byte{}, header...), '\n')
	file = append(file, []byte("{\"type\":\"message\",\"content\":[{\"type\":\"text\",\"text\":\"hello\"}]}\n")...)
	file = append(file, entry...)
	file = append(file, '\n')
	if err := os.WriteFile(target.sessionFile, file, 0o600); err != nil {
		t.Fatal(err)
	}
	observed, err := target.observeLocal(receipt)
	if err != nil || observed.State != peerObserved || observed.NativeEntryID != "entry-1" {
		t.Fatalf("observation = %+v, %v", observed, err)
	}
	result, err = source.receipt(context.Background(), target.ref, message.ID)
	if err != nil || result.Status != "native_observed" || result.Receipt.NativeEntryID != "entry-1" {
		t.Fatalf("observed receipt = %+v, %v", result, err)
	}
}

func TestLocalPeerSenderSeesUncertainAfterLostReply(t *testing.T) {
	root, err := os.MkdirTemp("", "lf-")
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
	originalSocket := filepath.Join(target.registryDir, target.socketName())
	proxyIncarnation, err := localRandom(16)
	if err != nil {
		t.Fatal(err)
	}
	proxyPath := filepath.Join(target.registryDir, "s-"+proxyIncarnation)
	proxy, err := net.ListenUnix("unix", &net.UnixAddr{Name: proxyPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer proxy.Close()
	if err := os.Chmod(proxyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	originalIncarnation := target.incarnation
	target.incarnation = proxyIncarnation
	err = target.writePresence()
	target.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	proxyDone := make(chan error, 1)
	go func() {
		client, err := proxy.AcceptUnix()
		if err != nil {
			proxyDone <- err
			return
		}
		defer client.Close()
		_ = client.SetDeadline(time.Now().Add(5 * time.Second))
		request, err := readLocalFrame(client)
		if err != nil {
			proxyDone <- err
			return
		}
		destination, err := net.DialUnix("unix", nil, &net.UnixAddr{Name: originalSocket, Net: "unix"})
		if err != nil {
			proxyDone <- err
			return
		}
		defer destination.Close()
		_ = destination.SetDeadline(time.Now().Add(5 * time.Second))
		if err = writeLocalFrame(destination, request); err == nil {
			_, err = readLocalFrame(destination)
		}
		proxyDone <- err
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	message := localMessage{ID: "drop-reply", Text: "Inspect the result.", Summary: "Inspect"}
	if _, err := source.send(ctx, target.ref, message); !errors.Is(err, errLocalUncertain) {
		t.Fatalf("lost reply returned %v, want uncertain", err)
	}
	if err := <-proxyDone; err != nil {
		t.Fatal(err)
	}
	target.mu.Lock()
	target.incarnation = originalIncarnation
	err = target.writePresence()
	target.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	result, err := source.receipt(ctx, target.ref, message.ID)
	if err != nil || result.Status != "accepted" {
		t.Fatalf("receipt after lost reply = %+v, %v", result, err)
	}
	retry, err := source.send(ctx, target.ref, message)
	if err != nil || retry.Status != "accepted" || retry.Receipt.Envelope != result.Receipt.Envelope {
		t.Fatalf("retry after lost reply = %+v, %v", retry, err)
	}
	if _, found, err := target.offerLocal(); err != nil || !found {
		t.Fatalf("first offer = %t, %v", found, err)
	}
	if _, found, err := target.offerLocal(); err != nil || found {
		t.Fatalf("duplicate offer = %t, %v", found, err)
	}
}

func awaitLocalOffer(t *testing.T, peer *localPeer) localReceipt {
	t.Helper()
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		receipt, found, err := peer.offerLocal()
		if err != nil {
			t.Fatal(err)
		}
		if found {
			return receipt
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for local notice")
	return localReceipt{}
}

func TestLocalPeerIdleSubscriptionSurvivesSourceRestart(t *testing.T) {
	root, err := os.MkdirTemp("", "li-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	options := fixtureOptions(root, localFixtureSource, "source", true)
	source, err := startLocalPeer(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.close() }()
	child := startFixtureProcess(t, root, true)
	defer func() {
		if child != nil {
			child.close(t)
		}
	}()
	if got := child.command(t, "busy"); got != "busy <nil>" {
		t.Fatalf("busy = %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	expires := time.Now().Add(time.Minute).UTC().Round(0)
	result, err := source.subscribe(ctx, child.ref, "idle-after-restart", expires)
	if err != nil || result.Status != "subscribed" || result.ExitMonitoring != "armed" {
		t.Fatalf("subscribe = %+v, %v", result, err)
	}
	if _, found, err := source.offerLocal(); err != nil || found {
		t.Fatalf("busy session produced a notice = %t, %v", found, err)
	}
	retained := source.ref
	if err := source.close(); err != nil {
		t.Fatal(err)
	}
	if got := child.command(t, "idle"); got != "idle <nil>" {
		t.Fatalf("idle = %q", got)
	}
	source, err = startLocalPeer(options)
	if err != nil {
		t.Fatal(err)
	}
	if source.ref != retained {
		t.Fatal("source identity changed after restart")
	}
	notice := awaitLocalOffer(t, source)
	if notice.Envelope.Sender != child.ref || !strings.HasPrefix(notice.Envelope.MessageID, "localnotice:") || !strings.Contains(notice.Body.Text, "became idle") {
		t.Fatalf("idle notice = %+v", notice)
	}
	presence, err := source.lookup(child.ref)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := source.recordLocalNotice(child.ref, "idle-after-restart", presence.Incarnation, localNoticeIdle)
	if duplicate.Status != "notice_accepted" || duplicate.Receipt == nil || duplicate.Receipt.State != peerAttempted {
		t.Fatalf("duplicate idle notice = %+v", duplicate)
	}
	if _, found, err := source.offerLocal(); err != nil || found {
		t.Fatalf("duplicate idle notice was offered = %t, %v", found, err)
	}
	result, err = source.subscribe(ctx, child.ref, "already-idle", expires)
	if err != nil || result.Status != "already_idle" {
		t.Fatalf("already idle subscription = %+v, %v", result, err)
	}
	immediate := awaitLocalOffer(t, source)
	if !strings.Contains(immediate.Body.Text, "became idle") || immediate.Envelope.MessageID == notice.Envelope.MessageID {
		t.Fatalf("already idle notice = %+v", immediate)
	}
	child.close(t)
	child = nil
	time.Sleep(100 * time.Millisecond)
	if _, found, err := source.offerLocal(); err != nil || found {
		t.Fatalf("exit duplicated an idle notice = %t, %v", found, err)
	}
}

func TestLocalPeerExitNeedsProcessEvidence(t *testing.T) {
	root, err := os.MkdirTemp("", "le-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	source, err := startLocalPeer(fixtureOptions(root, localFixtureSource, "source", true))
	if err != nil {
		t.Fatal(err)
	}
	defer source.close()
	child := startFixtureProcess(t, root, true)
	defer func() {
		if child != nil {
			child.close(t)
		}
	}()
	if got := child.command(t, "busy"); got != "busy <nil>" {
		t.Fatalf("busy = %q", got)
	}
	presence, err := source.lookup(child.ref)
	if err != nil || presence.NativeHost == nil {
		t.Fatalf("native host identity = %+v, %v", presence.NativeHost, err)
	}
	if _, err := watchLocalProcess(localProcessIdentity{PID: presence.NativeHost.PID, Birth: "incorrect"}); err == nil {
		t.Fatal("a changed native process identity was accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	result, err := source.subscribe(ctx, child.ref, "exit-only", time.Now().Add(time.Minute))
	if err != nil || result.Status != "subscribed" || result.ExitMonitoring != "armed" {
		t.Fatalf("subscribe = %+v, %v", result, err)
	}
	if got := child.command(t, "unlink"); got != "unlink <nil>" {
		t.Fatalf("unlink = %q", got)
	}
	time.Sleep(150 * time.Millisecond)
	if _, found, err := source.offerLocal(); err != nil || found {
		t.Fatalf("socket loss was reported as exit = %t, %v", found, err)
	}
	child.close(t)
	child = nil
	notice := awaitLocalOffer(t, source)
	if !strings.Contains(notice.Body.Text, "exited") || notice.Envelope.Sender != presence.Ref {
		t.Fatalf("exit notice = %+v", notice)
	}
	if result := source.recordLocalNotice(presence.Ref, "exit-only", presence.Incarnation, localNoticeIdle); result.Status != "already_notified" {
		t.Fatalf("idle after exit = %+v", result)
	}
	if _, found, err := source.offerLocal(); err != nil || found {
		t.Fatalf("exit was offered more than once = %t, %v", found, err)
	}
}

func TestLocalPeerSubscriptionConsentAndQuota(t *testing.T) {
	root, err := os.MkdirTemp("", "lc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	sourceOptions := fixtureOptions(root, localFixtureSource, "source", true)
	source, err := startLocalPeer(sourceOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.close() }()
	targetOptions := fixtureOptions(root, localFixtureTarget, "target", true)
	target, err := startLocalPeer(targetOptions)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = target.close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	expires := time.Now().Add(time.Minute).UTC().Round(0)
	result, err := source.subscribe(ctx, target.ref, "unknown-state", expires)
	if err != nil || result.Status != "unavailable" {
		t.Fatalf("unknown activity = %+v, %v", result, err)
	}
	if err := target.setLocalActivity(localActivityBusy); err != nil {
		t.Fatal(err)
	}
	result, err = source.subscribe(ctx, target.ref, "unknown-state", expires)
	if err != nil || result.Status != "subscribed" || result.ExitMonitoring != "unavailable" {
		t.Fatalf("retry with known activity = %+v, %v", result, err)
	}
	result, err = source.subscribe(ctx, target.ref, "unknown-state", expires.Add(time.Second))
	if err != nil || result.Status != "id_conflict" {
		t.Fatalf("changed subscription = %+v, %v", result, err)
	}
	if result, err := target.exchange(ctx, source.ref, "notice", localNotice{ID: "unknown-state", Incarnation: target.incarnation, Event: localNoticeExit}); err != nil || result.Status != "invalid" {
		t.Fatalf("remote exit assertion = %+v, %v", result, err)
	}
	for i := 1; i < localSubscriptionSenderLimit; i++ {
		result, err := source.subscribe(ctx, target.ref, fmt.Sprintf("quota-%d", i), expires)
		if err != nil || result.Status != "subscribed" {
			t.Fatalf("subscription %d = %+v, %v", i, result, err)
		}
	}
	result, err = source.subscribe(ctx, target.ref, "over-quota", expires)
	if err != nil || result.Status != "busy" {
		t.Fatalf("subscription quota = %+v, %v", result, err)
	}
	if err := target.close(); err != nil {
		t.Fatal(err)
	}
	targetOptions.AcceptInbound = false
	target, err = startLocalPeer(targetOptions)
	if err != nil {
		t.Fatal(err)
	}
	result, err = source.subscribe(ctx, target.ref, "target-denied", expires)
	if err != nil || result.Status != "denied" {
		t.Fatalf("target consent = %+v, %v", result, err)
	}
	if err := source.close(); err != nil {
		t.Fatal(err)
	}
	sourceOptions.AcceptInbound = false
	source, err = startLocalPeer(sourceOptions)
	if err != nil {
		t.Fatal(err)
	}
	result, err = source.subscribe(ctx, target.ref, "source-denied", expires)
	if err != nil || result.Status != "denied" {
		t.Fatalf("source consent = %+v, %v", result, err)
	}
}

func TestLocalPeerNoticeWaitsForInboxCapacity(t *testing.T) {
	root, err := os.MkdirTemp("", "lq-")
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
	if err := target.setLocalActivity(localActivityBusy); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	result, err := source.subscribe(ctx, target.ref, "capacity", time.Now().Add(time.Minute))
	if err != nil || result.Status != "subscribed" {
		t.Fatalf("subscribe = %+v, %v", result, err)
	}
	for i := 0; i < localSenderLimit; i++ {
		message := localMessage{ID: fmt.Sprintf("fill-%d", i), Text: "Pending work.", Summary: "Pending"}
		result, err := target.send(ctx, source.ref, message)
		if err != nil || result.Status != "accepted" {
			t.Fatalf("fill inbox %d = %+v, %v", i, result, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := target.setLocalActivity(localActivityIdle); err != nil {
		t.Fatal(err)
	}
	subscription := localSubscription{Source: source.ref, Target: target.ref, ID: "capacity"}
	path := localSubscriptionPath(source.stateDir, "subout-", subscription)
	deadline := time.Now().Add(4 * time.Second)
	for {
		record, err := readLocalSubscription(path)
		if err == nil && record.Fired == localNoticeIdle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notice intent was not saved: %+v, %v", record, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	noticeID := "localnotice:" + localMessageKey(source.ref, target.ref, "capacity")
	if result := source.queryLocal(target.ref, noticeID); result.Status != "not_found" {
		t.Fatalf("notice entered full inbox: %+v", result)
	}
	first, found, err := source.offerLocal()
	if err != nil || !found {
		t.Fatalf("offer pending message = %t, %v", found, err)
	}
	content, err := localMessageContent(first)
	if err != nil {
		t.Fatal(err)
	}
	header, _ := json.Marshal(map[string]any{"type": "session", "id": string(source.ref.Session)})
	entry, _ := json.Marshal(map[string]any{"type": "custom_message", "id": "completed", "customType": "pig_litter_local_peer", "content": content, "details": map[string]any{"envelope": first.Envelope}})
	data := append(append(append([]byte{}, header...), '\n'), entry...)
	data = append(data, '\n')
	if err := os.WriteFile(source.sessionFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := source.observeLocal(first); err != nil {
		t.Fatal(err)
	}
	for {
		result := source.queryLocal(target.ref, noticeID)
		if result.Status == "accepted" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("notice did not recover after capacity freed: %+v", result)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestLocalPeerExitDuringSourceDowntimeIsUncertain(t *testing.T) {
	root, err := os.MkdirTemp("", "lu-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	options := fixtureOptions(root, localFixtureSource, "source", true)
	source, err := startLocalPeer(options)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = source.close() }()
	child := startFixtureProcess(t, root, true)
	if got := child.command(t, "busy"); got != "busy <nil>" {
		t.Fatalf("busy = %q", got)
	}
	result, err := source.subscribe(context.Background(), child.ref, "downtime", time.Now().Add(time.Minute))
	if err != nil || result.Status != "subscribed" || result.ExitMonitoring != "armed" {
		t.Fatalf("subscribe = %+v, %v", result, err)
	}
	retained := child.ref
	if err := source.close(); err != nil {
		t.Fatal(err)
	}
	child.close(t)
	source, err = startLocalPeer(options)
	if err != nil {
		t.Fatal(err)
	}
	result = source.localSubscriptionStatus(retained, "downtime")
	if result.Status != "subscribed" || result.ExitMonitoring != "uncertain" {
		t.Fatalf("missing process proof = %+v", result)
	}
	if _, found, err := source.offerLocal(); err != nil || found {
		t.Fatalf("source claimed an unseen exit = %t, %v", found, err)
	}
}
