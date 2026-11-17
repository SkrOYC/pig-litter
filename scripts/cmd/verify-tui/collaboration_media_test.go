//go:build linux

package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCollaborationObserverHelper(t *testing.T) {
	if os.Getenv("P02_OBSERVER_HELPER") != "1" {
		return
	}
	if err := runCollaborationMediaObserver(os.Getenv("P02_OBSERVER_RECEIPT"), os.Getenv("P02_TMUX"), os.Getenv("P02_SOCKET"), "proof"); err != nil {
		t.Fatal(err)
	}
}

func TestCollaborationObserverPreservesControllerAndBlocksInput(t *testing.T) {
	tmux, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is unavailable")
	}
	directory, err := os.MkdirTemp("/tmp", "p02-media-")
	if err != nil {
		t.Fatal(err)
	}
	identity, err := resourceIdentity(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := identity.check(); err != nil {
			t.Error(err)
			return
		}
		if err := os.RemoveAll(directory); err != nil {
			t.Error(err)
		}
	})
	socket := filepath.Join(directory, "tmux.sock")
	output := filepath.Join(directory, "pane-input")
	env := append(withoutEnvironment(os.Environ(), "TERM"), "TERM=xterm-256color")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	call := func(args ...string) string {
		t.Helper()
		value, err := runTmux(ctx, tmux, socket, env, args...)
		if err != nil {
			t.Fatalf("tmux %v: %v", args, err)
		}
		return value
	}
	call("new-session", "-d", "-s", "proof", "-x", "100", "-y", "32", "cat > "+shellQuote(output))
	defer func() { _, _ = runTmux(context.Background(), tmux, socket, env, "kill-server") }()
	geometry := func() string { return call("display-message", "-p", "-t", "proof", "#{pane_width}x#{pane_height}") }
	if got := geometry(); got != "100x32" {
		t.Fatalf("initial geometry = %q", got)
	}
	master, slave, err := collaborationObserverPTY()
	if err != nil {
		t.Fatal(err)
	}
	defer master.Close()
	receipt := filepath.Join(directory, "observer.json")
	command := exec.Command(os.Args[0], "-test.run=^TestCollaborationObserverHelper$")
	command.Env = append(env, "P02_OBSERVER_HELPER=1", "P02_OBSERVER_RECEIPT="+receipt, "P02_TMUX="+tmux, "P02_SOCKET="+socket)
	command.Stdin, command.Stdout, command.Stderr = slave, slave, slave
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	_ = slave.Close()
	defer func() {
		if command.ProcessState == nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
	}()
	var observer collaborationObserverIdentity
	if err := waitFor(ctx, "test observer", 5*time.Second, func() (bool, error) {
		data, err := os.ReadFile(receipt)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if err := json.Unmarshal(data, &observer); err != nil {
			return false, err
		}
		clients := call("list-clients", "-F", "#{client_pid} #{client_flags}")
		return strings.Contains(clients, strconv.Itoa(observer.Client.PID)+" ") && strings.Contains(clients, "ignore-size") && !strings.Contains(clients, "read-only"), nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := geometry(); got != "100x32" {
		t.Fatalf("geometry after attach = %q", got)
	}
	if _, err := master.Write([]byte("OBSERVER-INPUT-MUST-NOT-ARRIVE\n")); err != nil {
		t.Fatal(err)
	}
	time.Sleep(250 * time.Millisecond)
	if data, err := os.ReadFile(output); err == nil && len(data) != 0 {
		t.Fatalf("observer input reached pane: %q", data)
	}
	call("send-keys", "-l", "-t", "proof", "CONTROLLER-INPUT-ARRIVED")
	call("send-keys", "-t", "proof", "Enter")
	if err := waitFor(ctx, "controller input", 5*time.Second, func() (bool, error) {
		data, err := os.ReadFile(output)
		return err == nil && string(data) == "CONTROLLER-INPUT-ARRIVED\n", nil
	}); err != nil {
		t.Fatal(err)
	}
	call("resize-window", "-t", "proof", "-x", "32", "-y", "32")
	if got := geometry(); got != "32x32" {
		t.Fatalf("geometry after controller resize = %q", got)
	}
	call("resize-window", "-t", "proof", "-x", "100", "-y", "32")
	if got := geometry(); got != "100x32" {
		t.Fatalf("geometry after restoring size = %q", got)
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	if err := command.Wait(); err != nil {
		t.Fatalf("observer cleanup: %v", err)
	}
	if observedAlive(observer.Observer) || observedAlive(observer.Client) {
		t.Fatal("observer or attached tmux client survived cleanup")
	}
}

func TestCollaborationMediaRetainsInconclusiveReceiptWhenToolUnavailable(t *testing.T) {
	directory := t.TempDir()
	missing := filepath.Join(directory, "missing")
	media, err := newCollaborationMedia(directory, options{mediaXvfb: missing, mediaXterm: missing, mediaFFmpeg: missing, mediaFFprobe: missing}, os.Args[0], missing, filepath.Join(directory, "socket"), "proof", os.Environ())
	if err != nil {
		t.Fatal(err)
	}
	runErr := media.start(context.Background())
	if runErr == nil {
		t.Fatal("missing media tools unexpectedly started")
	}
	if err := media.finish(runErr); err == nil {
		t.Fatal("missing media tools unexpectedly produced complete evidence")
	}
	data, err := os.ReadFile(filepath.Join(directory, "media", "receipt.json"))
	if err != nil {
		t.Fatal(err)
	}
	var receipt collaborationMediaReceipt
	if err := json.Unmarshal(data, &receipt); err != nil {
		t.Fatal(err)
	}
	if receipt.Status != "inconclusive" || receipt.Error == "" || !receipt.ProcessesExited || receipt.Video.SHA256 != "" {
		t.Fatalf("missing-tool receipt = %+v", receipt)
	}
}

func TestCollaborationStartupRequiresMatchingLiveProcess(t *testing.T) {
	directory := t.TempDir()
	process, err := readObservedProcess(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	capture := &ptyCapture{active: &paneCapture{command: paneCommand{Generation: "expected", Directory: directory}}}
	started := paneCompletion{Generation: "expected", Process: process, StartedMonotonicNS: 1000}
	if err := writeCaptureJSON(filepath.Join(directory, "started.json"), started); err != nil {
		t.Fatal(err)
	}
	measurement, err := collaborationStartup(capture, 1700, true)
	if err != nil {
		t.Fatal(err)
	}
	if measurement.Clock != "CLOCK_MONOTONIC" || measurement.Process != process || measurement.DurationNS != 700 || !measurement.MediaEnabled {
		t.Fatalf("startup measurement = %+v", measurement)
	}
	if _, err := collaborationStartup(capture, 999, false); err == nil {
		t.Fatal("accepted a readiness observation before process start")
	}
	capture.active.command.Generation = "different"
	if _, err := collaborationStartup(capture, 1700, false); err == nil {
		t.Fatal("accepted a process from another pane generation")
	}
}

func TestCollaborationScreenshotFollowsStartupReadiness(t *testing.T) {
	media := &collaborationMedia{receipt: collaborationMediaReceipt{RecordingNS: int64(10 * time.Second)}}
	media.markStartupReady(int64(12 * time.Second))
	if got := *media.receipt.Screenshot.SourceTimestampMS; got != 3000 {
		t.Fatalf("screenshot time = %d ms, want 3000 ms", got)
	}
	media.markStartupReady(int64(9 * time.Second))
	if got := *media.receipt.Screenshot.SourceTimestampMS; got != 1000 {
		t.Fatalf("screenshot time before recording = %d ms, want 1000 ms", got)
	}
}
