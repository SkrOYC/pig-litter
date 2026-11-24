//go:build linux

package piglitter

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func init() {
	if len(os.Args) != 3 || os.Args[1] != "--pig-litter-member" {
		return
	}
	cwd, err := os.Getwd()
	if err != nil || os.WriteFile(os.Args[2]+".ready", []byte(cwd), 0o600) != nil {
		os.Exit(2)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestTmuxPaneOwnershipAndArgumentIsolation(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Fatal("tmux is required for the pane ownership test")
	}
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
	tmux("new-session", "-d", "-s", "fixture", "-x", "160", "-y", "50", "sleep", "600")
	t.Cleanup(func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() })
	identity := strings.Split(tmux("display-message", "-p", "-t", "fixture:0.0", "#{pid}\t#{pane_id}"), "\t")
	if len(identity) != 2 {
		t.Fatalf("unexpected server identity: %q", identity)
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
	cwd := filepath.Join(root, `cwd ' quote " $(touch bad)`)
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	nonce := "0123456789abcdef0123456789abcdef"
	manifest := filepath.Join(root, "member-"+nonce+".json")
	pane, err := server.launch(ctx, tmuxLaunch{executable: executable, manifest: manifest, cwd: cwd, nonce: nonce})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pane.close(context.Background()) }()
	for {
		contents, err := os.ReadFile(manifest + ".ready")
		if err == nil {
			if string(contents) != cwd {
				t.Fatalf("member cwd = %q, want %q", contents, cwd)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("member did not become ready")
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := os.Stat(filepath.Join(root, "bad")); !os.IsNotExist(err) {
		t.Fatalf("cwd was interpreted by a shell: %v", err)
	}
	wrong := pane
	wrong.birth = "different"
	if err := wrong.close(ctx); err == nil {
		t.Fatal("cleanup accepted a different process birth")
	}
	if err := pane.close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := pane.close(ctx); err != nil {
		t.Fatalf("repeated cleanup: %v", err)
	}
	panes := strings.Split(tmux("list-panes", "-a", "-F", "#{pane_id}"), "\n")
	if len(panes) != 2 || !strings.Contains(strings.Join(panes, " "), unrelated) {
		t.Fatalf("cleanup changed an unrelated pane: %q", panes)
	}
	bad := tmuxLaunch{executable: executable, manifest: manifest, cwd: cwd + "\n", nonce: nonce}
	if _, err := server.launch(ctx, bad); err == nil {
		t.Fatal("accepted a control character in cwd")
	}
	panes = strings.Split(tmux("list-panes", "-a", "-F", "#{pane_id}"), "\n")
	if len(panes) != 2 {
		t.Fatalf("invalid startup allocated a pane: %q", panes)
	}
	wrapper := filepath.Join(root, "drop-tmux-reply")
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal(err)
	}
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$3\" = split-window ]; then\n  %s \"$@\" >/dev/null\n  exit 7\nfi\nexec %s \"$@\"\n", tmuxPath, tmuxPath)
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	dropped := server
	dropped.command = wrapper
	newNonce := "fedcba9876543210fedcba9876543210"
	_, err = dropped.launch(ctx, tmuxLaunch{executable: executable, manifest: filepath.Join(root, "member-"+newNonce+".json"), cwd: cwd, nonce: newNonce})
	if err == nil || !strings.Contains(err.Error(), "owned pane was removed") {
		t.Fatalf("lost allocation reply = %v", err)
	}
	panes = strings.Split(tmux("list-panes", "-a", "-F", "#{pane_id}"), "\n")
	if len(panes) != 2 || !strings.Contains(strings.Join(panes, " "), unrelated) {
		t.Fatalf("lost reply cleanup changed an unrelated pane: %q", panes)
	}
}
