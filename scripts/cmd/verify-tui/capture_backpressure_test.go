package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestCaptureDrainsAfterExitWithBackpressure(t *testing.T) {
	for _, suppliedBoundary := range []bool{true, false} {
		name := "ordinary-prefix"
		if suppliedBoundary {
			name = "supplied-generation-boundary"
		}
		t.Run(name, func(t *testing.T) {
			f := newCaptureFixture(t, maxRawPTY)
			f.start(t, `set -e; printf 'clean final frame\n'; IFS= read -r token; printf '\036%s\037' "$token"; IFS= read -r gate; head -c 2097152 /dev/zero | tr '\000' 'x'; printf '\033]0;CONFIG_OSC_SHOULD_NOT_PAINT\007SHIPPING_FINAL_TAIL\n'`)
			active := f.capture.active
			token := "ordinary-boundary-text"
			if suppliedBoundary {
				token = "VERIFY_CAPTURE_END_" + active.command.Generation
			}
			if _, err := f.tmux("send-keys", "-l", "-t", "fixture", token); err != nil {
				t.Fatal(err)
			}
			if _, err := f.tmux("send-keys", "-t", "fixture", "Enter"); err != nil {
				t.Fatal(err)
			}
			if err := waitFor(f.ctx, "producer prefix captured", time.Second, func() (bool, error) {
				raw, err := os.ReadFile(filepath.Join(f.evidence, "pty.raw"))
				return bytes.Contains(raw, []byte("\x1e"+token+"\x1f")), err
			}); err != nil {
				t.Fatal(err)
			}
			if err := syscall.Kill(active.interval.Pipe.PID, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			defer syscall.Kill(active.interval.Pipe.PID, syscall.SIGCONT)
			if err := waitFor(f.ctx, "stopped recorder relay", time.Second, func() (bool, error) {
				data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(active.interval.Pipe.PID), "stat"))
				if err != nil {
					return false, err
				}
				end := strings.LastIndexByte(string(data), ')')
				return strings.Fields(string(data)[end+1:])[0] == "T", nil
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := f.tmux("send-keys", "-t", "fixture", "Enter"); err != nil {
				t.Fatal(err)
			}
			var completion paneCompletion
			if err := waitFor(f.ctx, "producer exit behind stopped relay", 5*time.Second, func() (bool, error) {
				data, err := os.ReadFile(filepath.Join(active.command.Directory, "exit.json"))
				if os.IsNotExist(err) {
					return false, nil
				}
				if err != nil {
					return false, err
				}
				if err := json.Unmarshal(data, &completion); err != nil {
					return false, err
				}
				return completion.ExitedAt != "" && !observedAlive(completion.Process), nil
			}); err != nil {
				t.Fatal(err)
			}
			if completion.Status != 0 {
				t.Fatalf("producer did not complete its writes: %+v", completion)
			}
			if err := writeCaptureJSON(filepath.Join(f.evidence, "private-exit-copy.json"), completion); err != nil {
				t.Fatal(err)
			}
			blocked, cancel := context.WithTimeout(f.ctx, 500*time.Millisecond)
			_, blockedErr := f.capture.finish(blocked)
			cancel()
			pipe, err := f.tmux("display-message", "-p", "-t", "fixture", "#{pane_pipe_pid}")
			t.Logf("closure with stopped relay: %v; pipe=%q", blockedErr, pipe)
			if blockedErr == nil || !strings.Contains(blockedErr.Error(), "inconclusive") || err != nil || pipe != strconv.Itoa(active.interval.Pipe.PID) {
				t.Fatalf("incomplete closure lost its owned transport: error=%v pipe=%q tmux=%v", blockedErr, pipe, err)
			}
			if err := syscall.Kill(active.interval.Pipe.PID, syscall.SIGCONT); err != nil {
				t.Fatal(err)
			}
			_, finishErr := f.capture.finish(f.ctx)
			raw, err := os.ReadFile(filepath.Join(f.evidence, "pty.raw"))
			if err != nil {
				t.Fatal(err)
			}
			checkErr := checkRawPTY(filepath.Join(f.evidence, "pty.raw"))
			trace := map[string]any{
				"finishedWithoutError": finishErr == nil, "checkWithoutError": checkErr == nil,
				"rawBytes": len(raw), "expectedPayloadBytes": 2097152,
				"hasTail":                 bytes.Contains(raw, []byte("SHIPPING_FINAL_TAIL")),
				"hasHostile":              bytes.Contains(raw, []byte("\x1b]0;CONFIG_OSC_SHOULD_NOT_PAINT\a")),
				"generationBoundaryCount": bytes.Count(raw, captureBoundary(active.command.Generation)),
				"programExit":             completion,
			}
			if err := writeCaptureJSON(filepath.Join(f.evidence, "shipping-trace.json"), trace); err != nil {
				t.Fatal(err)
			}
			t.Logf("finish=%v check=%v raw=%d tail=%v hostile=%v", finishErr, checkErr, len(raw), trace["hasTail"], trace["hasHostile"])
			if !bytes.Contains(raw, bytes.Repeat([]byte("x"), 2097152)) || !bytes.Contains(raw, []byte("SHIPPING_FINAL_TAIL")) || !bytes.Contains(raw, []byte("\x1b]0;CONFIG_OSC_SHOULD_NOT_PAINT\a")) {
				t.Fatal("the recorder lost the producer payload or final hostile tail")
			}
			if finishErr == nil || !strings.Contains(finishErr.Error(), "attacker-controlled") || checkErr == nil || !strings.Contains(checkErr.Error(), "attacker-controlled") {
				t.Fatalf("the complete hostile stream did not fail: finish=%v check=%v", finishErr, checkErr)
			}
		})
	}
}

func TestCaptureRecognizesClosingBoundaryBeforeOwnerReadsExit(t *testing.T) {
	for _, replaceClosingID := range []bool{false, true} {
		name := "matching-private-completion"
		if replaceClosingID {
			name = "substituted-private-completion"
		}
		t.Run(name, func(t *testing.T) {
			f := newCaptureFixture(t, maxRawPTY)
			f.expectIncomplete = replaceClosingID
			f.start(t, `printf 'clean final frame\n'; IFS= read -r gate`)
			active := f.capture.active
			if _, err := f.tmux("send-keys", "-t", "fixture", "Enter"); err != nil {
				t.Fatal(err)
			}
			select {
			case <-active.boundary:
			case <-f.ctx.Done():
				t.Fatal("recorder did not recognize the closing identity before the owner read exit.json")
			}
			data, err := os.ReadFile(filepath.Join(active.command.Directory, "exit.json"))
			if err != nil {
				t.Fatal(err)
			}
			var completion paneCompletion
			if err := json.Unmarshal(data, &completion); err != nil {
				t.Fatal(err)
			}
			if completion.ClosingID == "" || completion.ClosingID == active.command.Generation || completion.Status != 0 || observedAlive(completion.Process) {
				t.Fatalf("closing identity was not associated with an exited producer: %+v", completion)
			}
			if replaceClosingID {
				completion.ClosingID = "substituted-closing-identity"
				if err := writeCaptureJSON(filepath.Join(active.command.Directory, "exit.json"), completion); err != nil {
					t.Fatal(err)
				}
			}
			_, finishErr := f.capture.finish(f.ctx)
			checkErr := checkRawPTY(filepath.Join(f.evidence, "pty.raw"))
			if replaceClosingID {
				if finishErr == nil || !strings.Contains(finishErr.Error(), "inconclusive") || checkErr == nil || !strings.Contains(checkErr.Error(), "inconclusive") {
					t.Fatalf("owner accepted a different private completion: finish=%v check=%v", finishErr, checkErr)
				}
			} else if finishErr != nil || checkErr != nil {
				t.Fatalf("matching marker read before the owner did not complete: finish=%v check=%v", finishErr, checkErr)
			}
		})
	}
}
