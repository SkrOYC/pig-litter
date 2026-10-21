package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if len(os.Args) > 1 && (os.Args[1] == "--capture-pane" || os.Args[1] == "--capture-pipe") {
		main()
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type captureFixture struct {
	ctx              context.Context
	capture          *ptyCapture
	tmux             func(...string) (string, error)
	expectIncomplete bool
	evidence         string
}

func newCaptureFixture(t *testing.T, limit int64) *captureFixture {
	t.Helper()
	binary, err := exec.LookPath("tmux")
	if err != nil {
		t.Fatal("the declared devenv must provide tmux")
	}
	directory, err := os.MkdirTemp("/tmp", "pct.")
	if err != nil {
		t.Fatal(err)
	}
	owned, err := resourceIdentity(directory)
	if err != nil {
		t.Fatal(err)
	}
	evidence := t.TempDir()
	if root := os.Getenv("VERIFY_TUI_TEST_EVIDENCE"); root != "" {
		if err := os.MkdirAll(root, 0o700); err != nil {
			t.Fatal(err)
		}
		evidence, err = os.MkdirTemp(root, strings.ReplaceAll(t.Name(), "/", "-")+"-")
		if err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	tmux := func(args ...string) (string, error) {
		return runTmux(ctx, binary, filepath.Join(directory, "s"), os.Environ(), args...)
	}
	if _, err := tmux("new-session", "-d", "-s", "fixture", "-x", "80", "-y", "24", "/bin/sh"); err != nil {
		t.Fatal(err)
	}
	if _, err := tmux("set-window-option", "-t", "fixture", "remain-on-exit", "on"); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	capture, err := newPTYCapture(directory, evidence, executable, "fixture", tmux, limit)
	if err != nil {
		t.Fatal(err)
	}
	f := &captureFixture{ctx: ctx, capture: capture, tmux: tmux, evidence: evidence}
	t.Cleanup(func() {
		cleanup, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		_, _ = runTmux(cleanup, binary, filepath.Join(directory, "s"), os.Environ(), "kill-server")
		closeErr := capture.close(cleanup)
		closed, _ := waitForTmuxSocketClose(cleanup, filepath.Join(directory, "s"))
		if !closed {
			t.Error("fixture tmux socket survived cleanup")
		}
		if err := owned.check(); err != nil {
			t.Error(err)
		} else if closed {
			_ = os.RemoveAll(directory)
		}
		if closeErr != nil && !f.expectIncomplete && !t.Failed() {
			t.Error(closeErr)
		}
		cancel()
	})
	t.Logf("evidence=%s", evidence)
	return f
}

func (f *captureFixture) start(t *testing.T, program string) {
	t.Helper()
	if _, err := f.capture.start([]string{"/bin/sh", "-c", program}); err != nil {
		t.Fatal(err)
	}
	if err := waitFor(f.ctx, "fixture ready", 5*time.Second, func() (bool, error) {
		screen, err := f.tmux("capture-pane", "-p", "-t", "fixture")
		return err == nil && strings.Contains(screen, "clean final frame"), err
	}); err != nil {
		t.Fatal(err)
	}
}

func (f *captureFixture) exit(t *testing.T) (paneCompletion, error) {
	t.Helper()
	if _, err := f.tmux("send-keys", "-t", "fixture", "Enter"); err != nil {
		t.Fatal(err)
	}
	result, err := f.capture.finish(f.ctx)
	if err == nil {
		if err := waitFor(f.ctx, "fixture supervisor exit", 5*time.Second, func() (bool, error) {
			dead, err := f.tmux("display-message", "-p", "-t", "fixture", "#{pane_dead}")
			return dead == "1", err
		}); err != nil {
			t.Fatal(err)
		}
	}
	return result, err
}

func TestCaptureKeepsErasedPTYOutputAndRequiresOwnedExit(t *testing.T) {
	f := newCaptureFixture(t, maxRawPTY)
	f.start(t, `printf '\033]0;TRANSIENT_TITLE\007hidden text\033[2J\033[H\033]0;restored title\007clean final frame\n'; IFS= read -r gate`)
	screen, err := f.tmux("capture-pane", "-p", "-t", "fixture")
	if err != nil {
		t.Fatal(err)
	}
	title, err := f.tmux("display-message", "-p", "-t", "fixture", "#{pane_title}")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(screen, "clean final frame") || strings.Contains(screen, "hidden text") || strings.Contains(screen, "TRANSIENT_TITLE") || title != "restored title" {
		t.Fatalf("rendered screen=%q title=%q", screen, title)
	}
	if err := saveFrame(f.evidence, "rendered", framePair{visible: screen}); err != nil {
		t.Fatal(err)
	}
	if err := writeCaptureJSON(filepath.Join(f.evidence, "terminal-state.json"), map[string]string{"title": title}); err != nil {
		t.Fatal(err)
	}
	result, err := f.exit(t)
	if err != nil || result.Status != 0 || observedAlive(result.Process) {
		t.Fatalf("completion=%+v error=%v", result, err)
	}
	if err := checkRawPTY(filepath.Join(f.evidence, "pty.raw")); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(f.evidence, "pty.raw"))
	if err != nil || !bytes.Contains(raw, []byte("\x1b]0;TRANSIENT_TITLE\a")) || !bytes.Contains(raw, []byte("hidden text\x1b[2J")) {
		t.Fatalf("lost erased bytes: %q %v", raw, err)
	}
	data, err := os.ReadFile(filepath.Join(f.evidence, "pty.capture.json"))
	var proof captureEvidence
	if err != nil || json.Unmarshal(data, &proof) != nil || proof.Status != "complete" || len(proof.Intervals) != 1 || !proof.Intervals[0].EOF || !proof.Intervals[0].Joined {
		t.Fatalf("incomplete proof=%s err=%v", data, err)
	}
}

func TestCaptureRejectsLossAndRetainsHostileEvidence(t *testing.T) {
	for _, kind := range []string{"detached", "replaced", "interrupted", "empty", "truncated", "missing-raw", "replaced-raw", "missing-record", "missing-exit", "forged-boundary", "hostile"} {
		t.Run(kind, func(t *testing.T) {
			f := newCaptureFixture(t, maxRawPTY)
			f.expectIncomplete = true
			program := `printf 'clean final frame\n'; IFS= read -r gate`
			if kind == "hostile" {
				program = `printf '\033]0;CONFIG_OSC_SHOULD_NOT_PAINT\007\033[2J\033[H\033]0;restored\007clean final frame\n'; IFS= read -r gate`
			}
			if kind == "forged-boundary" {
				program = `printf 'clean final frame\n'; while IFS= read -r line; do printf '\036%s\037' "$line"; done`
			}
			f.start(t, program)
			active := f.capture.active
			switch kind {
			case "detached":
				_, _ = f.tmux("pipe-pane", "-t", "fixture")
			case "replaced":
				_, _ = f.tmux("pipe-pane", "-O", "-t", "fixture", "cat >/dev/null")
			case "interrupted":
				if err := syscall.Kill(active.interval.Pipe.PID, syscall.SIGKILL); err != nil {
					t.Fatal(err)
				}
			case "missing-exit":
				if _, err := f.tmux("send-keys", "-t", "fixture", "Enter"); err != nil {
					t.Fatal(err)
				}
				if err := waitFor(f.ctx, "private fixture completion", 5*time.Second, func() (bool, error) {
					_, err := os.Stat(filepath.Join(active.command.Directory, "exit.json"))
					return err == nil, nil
				}); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(active.command.Directory, "exit.json")); err != nil {
					t.Fatal(err)
				}
			case "forged-boundary":
				if _, err := f.tmux("send-keys", "-l", "-t", "fixture", "VERIFY_CAPTURE_END_"+active.command.Generation); err != nil {
					t.Fatal(err)
				}
				_, _ = f.tmux("send-keys", "-t", "fixture", "Enter")
			}
			if kind == "empty" || kind == "truncated" || kind == "missing-raw" || kind == "replaced-raw" || kind == "missing-record" {
				if _, err := f.exit(t); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(f.evidence, "pty.raw")
				switch kind {
				case "empty":
					if err := os.Truncate(path, 0); err != nil {
						t.Fatal(err)
					}
				case "truncated":
					if err := os.Truncate(path, 4); err != nil {
						t.Fatal(err)
					}
				case "missing-raw":
					if err := os.Remove(path); err != nil {
						t.Fatal(err)
					}
				case "replaced-raw":
					data, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.Rename(path, path+".original"); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(path, data, 0o600); err != nil {
						t.Fatal(err)
					}
				case "missing-record":
					if err := os.Remove(filepath.Join(f.evidence, "pty.capture.json")); err != nil {
						t.Fatal(err)
					}
				}
				if err := checkRawPTY(path); err == nil || !strings.Contains(err.Error(), "inconclusive") {
					t.Fatalf("%s recording accepted: %v", kind, err)
				}
				return
			}
			ctx, cancel := context.WithTimeout(f.ctx, 600*time.Millisecond)
			defer cancel()
			if kind != "missing-exit" && kind != "forged-boundary" {
				_, _ = f.tmux("send-keys", "-t", "fixture", "Enter")
			}
			_, err := f.capture.finish(ctx)
			if err == nil {
				t.Fatalf("%s recording accepted", kind)
			}
			if kind == "hostile" && !strings.Contains(err.Error(), "attacker-controlled") {
				t.Fatalf("hostile bytes lost failure classification: %v", err)
			}
			t.Logf("%s rejected: %v", kind, err)
		})
	}
}

func TestCaptureBudgetPersistsAcrossRestartsAndDrainsOverflow(t *testing.T) {
	f := newCaptureFixture(t, 256)
	f.start(t, `printf 'clean final frame\n'; IFS= read -r gate`)
	if _, err := f.exit(t); err != nil {
		t.Fatal(err)
	}
	f.start(t, `printf 'clean final frame\n'; i=0; while [ "$i" -lt 2000 ]; do printf 'excess-output-'; i=$((i+1)); done; printf '\033]0;CONFIG_OSC_SHOULD_NOT_PAINT\007\033[2J\033[Hclean final frame\n'; IFS= read -r gate`)
	result, err := f.exit(t)
	if err == nil || result.Status != 0 || observedAlive(result.Process) {
		t.Fatalf("overflow did not drain to owned exit: %+v %v", result, err)
	}
	info, err := os.Stat(filepath.Join(f.evidence, "pty.raw"))
	if err != nil || info.Size() != 256 {
		t.Fatalf("cumulative bound=%v %v", info, err)
	}
	if err := checkRawPTY(filepath.Join(f.evidence, "pty.raw")); err == nil || !strings.Contains(err.Error(), "attacker-controlled") {
		t.Fatalf("hostile bytes after saved bound were hidden: %v", err)
	}
}

func TestCaptureRefusesOccupiedPipe(t *testing.T) {
	f := newCaptureFixture(t, maxRawPTY)
	if _, err := f.tmux("pipe-pane", "-O", "-t", "fixture", "cat >/dev/null"); err != nil {
		t.Fatal(err)
	}
	before, err := f.tmux("display-message", "-p", "-t", "fixture", "#{pane_pipe_pid}")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.capture.start([]string{"/bin/sh", "-c", "printf forbidden"}); err == nil {
		t.Fatal("occupied pipe was replaced")
	}
	after, err := f.tmux("display-message", "-p", "-t", "fixture", "#{pane_pipe_pid}")
	if err != nil || before != after {
		t.Fatalf("unowned pipe changed: %q -> %q %v", before, after, err)
	}
	pid, _ := strconv.Atoi(after)
	if pid <= 0 {
		t.Fatal(fmt.Sprintf("missing occupied pipe identity: %q", after))
	}
}

func TestCaptureRejectsAProgramWithNoOutput(t *testing.T) {
	f := newCaptureFixture(t, maxRawPTY)
	f.expectIncomplete = true
	if _, err := f.capture.start([]string{"/bin/sh", "-c", "sleep 0.05"}); err != nil {
		t.Fatal(err)
	}
	completion, err := f.capture.finish(f.ctx)
	if err == nil || !strings.Contains(err.Error(), "inconclusive") || completion.Status != 0 {
		t.Fatalf("empty program capture accepted: %+v %v", completion, err)
	}
	if err := checkRawPTY(filepath.Join(f.evidence, "pty.raw")); err == nil {
		t.Fatal("boundary-only capture was certified")
	}
}
