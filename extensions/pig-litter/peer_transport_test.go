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
	peer, err := startLocalPeer(fixtureOptions(root, localFixtureTarget, "target", os.Getenv("LOCAL_PEER_FIXTURE_ACCEPT") == "1"))
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
