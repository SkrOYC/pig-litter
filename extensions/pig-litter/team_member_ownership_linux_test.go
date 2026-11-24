//go:build linux

package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
)

func TestNativeOwnershipPersistsAndReleasesAfterProcessExit(t *testing.T) {
	root := t.TempDir()
	socket := filepath.Join(root, "tmux.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tmux := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "tmux", append([]string{"-S", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %q: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	tmux("new-session", "-d", "-s", "owner", "-x", "160", "-y", "50", "sleep", "600")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	identity := strings.Split(tmux("display-message", "-p", "-t", "owner:0.0", "#{pid}\t#{pane_id}"), "\t")
	if len(identity) != 2 {
		t.Fatalf("invalid tmux identity: %q", identity)
	}
	pid, err := strconv.Atoi(identity[0])
	if err != nil {
		t.Fatal(err)
	}
	server, err := tmuxFromEnvironment(ctx, fmt.Sprintf("%s,%d,0", socket, pid), identity[1])
	if err != nil {
		t.Fatal(err)
	}
	unrelated := tmux("split-window", "-d", "-P", "-F", "#{pane_id}", "-t", server.target, "sleep", "600")
	rootID := ChildID("018f0000-0000-7000-8000-000000000001")
	memberID := ChildID("018f0000-0000-7000-8000-000000000002")
	member := teamMember{Ref: MemberRef{Team: TeamID{Root: rootID, Epoch: 1}, ID: memberID, Generation: 1}, AdmissionID: "admission", Workspace: WorkspacePolicy{Kind: "shared_checkout", Cwd: root}}
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	worker, _ := agentDefaults("worker")
	_, selected, err := tree.admit(admission{kind: "worker", name: "member", model: "fixture/fixed", cwd: root, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	sessionDir := filepath.Join(root, "sessions")
	if err := os.Mkdir(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	log := filepath.Join(sessionDir, "session_"+string(memberID)+".jsonl")
	header, _ := json.Marshal(map[string]any{"type": "session", "id": memberID, "cwd": root})
	if err := os.WriteFile(log, append(header, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nonce := "0123456789abcdef0123456789abcdef"
	launch := tmuxLaunch{executable: executable, manifest: filepath.Join(root, "member-"+nonce+".json"), cwd: root, nonce: nonce}
	host := &runtimeHost{}
	var failAppend atomic.Bool
	var heldDuringAppend atomic.Bool
	var holdRelease atomic.Bool
	releaseEntered, releaseContinue := make(chan struct{}), make(chan struct{})
	withCompletionReplies(t, tree, "print", func(o *owner) {
		ownership, err := o.reserveMemberLaunch(ctx, selected.owner, selected, server, launch, sessionDir)
		if err != nil {
			t.Fatal(err)
		}
		if !hasSavedMemberOwnership(host, false) {
			t.Fatal("launch intent was not persisted before pane allocation")
		}
		pane, err := server.launch(ctx, launch)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = pane.close(context.Background()) }()
		if err := o.recordMemberPane(ctx, selected.owner, selected, ownership, pane); err != nil {
			t.Fatal(err)
		}
		if !hasSavedMemberOwnership(host, true) {
			t.Fatal("pane identity was not persisted")
		}
		wrongModel := testMemberControl{snapshot: memberSnapshot{session: memberID, cwd: root, model: "fixture/other", phase: memberIdle}}
		if _, err := o.installMemberHandle(ctx, selected.owner, selected, sessionDir, log, nonce, wrongModel); err == nil {
			t.Fatal("installed a native handle with a different model")
		}
		wrongTools := testMemberControl{snapshot: memberSnapshot{session: memberID, cwd: root, model: "fixture/fixed", tools: []string{"write"}, phase: memberIdle}}
		if _, err := o.installMemberHandle(ctx, selected.owner, selected, sessionDir, log, nonce, wrongTools); err == nil {
			t.Fatal("installed a native handle with a wider tool set")
		}
		tree.mu.Lock()
		actualDigest := ownership.ExecutableSHA256
		ownership.ExecutableSHA256 = strings.Repeat("0", 64)
		tree.mu.Unlock()
		cancelled := make(chan struct{}, 1)
		control := testMemberControl{snapshot: memberSnapshot{session: memberID, cwd: root, model: "fixture/fixed", phase: memberIdle}, cancel: func() { cancelled <- struct{}{} }}
		if _, err := o.installMemberHandle(ctx, selected.owner, selected, sessionDir, log, nonce, control); err == nil {
			t.Fatal("installed a native handle with a different executable digest")
		}
		tree.mu.Lock()
		ownership.ExecutableSHA256 = actualDigest
		tree.mu.Unlock()
		if err := os.Remove(log); err != nil {
			t.Fatal(err)
		}
		provisional, err := o.installProvisionalMemberHandle(ctx, selected.owner, selected, sessionDir, nonce, control)
		if err != nil || provisional.ready {
			t.Fatalf("provisional process handle = %#v, %v", provisional, err)
		}
		if _, err := o.memberLogEntries(ctx, provisional); err == nil {
			t.Fatal("provisional handle exposed an unpinned history")
		}
		tree.mu.Lock()
		selected.owner.native = nil
		selected.mu.Lock()
		selected.member = nil
		selected.mu.Unlock()
		tree.mu.Unlock()
		if err := os.WriteFile(log, append(header, '\n'), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := o.installMemberHandle(ctx, selected.owner, selected, sessionDir, log, nonce, control); err != nil {
			t.Fatal(err)
		}
		selected.abort()
		select {
		case <-cancelled:
		default:
			t.Fatal("run cancellation did not reach the native handle")
		}
		short, stop := context.WithTimeout(ctx, 50*time.Millisecond)
		err = o.releaseMemberLaunch(short, selected.owner, selected, ownership, pane)
		stop()
		if err == nil {
			t.Fatal("released ownership while the native process was alive")
		}
		if birth, err := processBirth(pane.pid); err != nil || birth != pane.birth {
			t.Fatalf("live member changed during failed release: %q %v", birth, err)
		}
		process, err := os.FindProcess(pane.pid)
		if err != nil || process.Signal(syscall.SIGTERM) != nil {
			t.Fatal("stop owned test process", err)
		}
		holdRelease.Store(true)
		released := make(chan error, 1)
		go func() { released <- o.releaseMemberLaunch(ctx, selected.owner, selected, ownership, pane) }()
		select {
		case <-releaseEntered:
		case <-ctx.Done():
			t.Fatal("native ownership release did not reach persistence")
		}
		if _, _, err := tree.resume(nil, selected, "continue"); err == nil || !strings.Contains(err.Error(), "ownership requires reconciliation") {
			t.Errorf("resume passed an uncommitted ownership release: %v", err)
		}
		if err := o.withChildLogLocked(selected.owner, func(*coding.SessionManager) error { return nil }); err == nil {
			t.Error("child log was reopened before ownership release persisted")
		}
		close(releaseContinue)
		if err := <-released; err != nil {
			t.Fatal(err)
		}
		tree.mu.Lock()
		bound, retained := selected.owner.native, selected.owner.nativeOwnership
		tree.mu.Unlock()
		if bound != nil || retained != nil || !hasSavedMemberRelease(host) {
			t.Fatalf("native ownership was not released: %v %v", bound, retained)
		}
		if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, unrelated) || strings.Contains(panes, pane.id) {
			t.Fatalf("release changed unrelated panes: %q", panes)
		}
		failedNonce := "fedcba9876543210fedcba9876543210"
		failedLaunch := tmuxLaunch{executable: executable, manifest: filepath.Join(root, "member-"+failedNonce+".json"), cwd: root, nonce: failedNonce}
		intent, err := o.reserveMemberLaunch(ctx, selected.owner, selected, server, failedLaunch, sessionDir)
		if err != nil {
			t.Fatal(err)
		}
		snapshot := cloneMemberOwnership(intent)
		failAppend.Store(true)
		if err := o.clearMemberOwnership(ctx, selected.owner, selected, intent, snapshot); err == nil {
			t.Fatal("ownership release ignored a persistence failure")
		}
		tree.mu.Lock()
		retained = selected.owner.nativeOwnership
		releasing := selected.owner.nativeReleasing
		tree.mu.Unlock()
		if retained != intent || releasing {
			t.Fatal("failed ownership release did not restore the fence")
		}
		failAppend.Store(false)
		if err := o.clearMemberOwnership(ctx, selected.owner, selected, intent, snapshot); err != nil {
			t.Fatal(err)
		}
		failAppend.Store(true)
		if _, err := o.reserveMemberLaunch(ctx, selected.owner, selected, server, failedLaunch, sessionDir); err == nil {
			t.Fatal("allocated after launch intent persistence failed")
		}
		tree.mu.Lock()
		fenced := selected.owner.nativeOwnership != nil && selected.owner.nativeOwnership.Pane == nil
		tree.mu.Unlock()
		if !fenced {
			t.Fatal("failed launch intent did not retain its uncertainty fence")
		}
		if panes := strings.Split(tmux("list-panes", "-a", "-F", "#{pane_id}"), "\n"); len(panes) != 2 {
			t.Fatalf("failed intent allocated a pane: %q", panes)
		}
	}, func(method string, raw json.RawMessage) *subprocess.CallResultPayload {
		if method == "appendEntry" {
			if tree.lifecycleMu.TryLock() {
				tree.lifecycleMu.Unlock()
			} else {
				heldDuringAppend.Store(true)
			}
			if holdRelease.CompareAndSwap(true, false) {
				close(releaseEntered)
				<-releaseContinue
			}
		}
		if method == "appendEntry" && failAppend.Load() {
			return &subprocess.CallResultPayload{Error: &subprocess.ErrorInfo{Message: "synthetic storage failure"}}
		}
		return &subprocess.CallResultPayload{Result: host.call(t, method, raw)}
	})
	if heldDuringAppend.Load() {
		t.Fatal("native ownership persistence held the lifecycle lock across the host call")
	}
}

func hasSavedMemberOwnership(host *runtimeHost, pane bool) bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	for _, entry := range host.entries {
		data, _ := json.Marshal(entry["data"])
		var saved savedChild
		if json.Unmarshal(data, &saved) == nil && saved.Native != nil && (saved.Native.Pane != nil) == pane {
			return true
		}
	}
	return false
}

func hasSavedMemberRelease(host *runtimeHost) bool {
	host.mu.Lock()
	defer host.mu.Unlock()
	if len(host.entries) == 0 {
		return false
	}
	data, _ := json.Marshal(host.entries[len(host.entries)-1]["data"])
	var saved savedChild
	return json.Unmarshal(data, &saved) == nil && saved.Native == nil
}
