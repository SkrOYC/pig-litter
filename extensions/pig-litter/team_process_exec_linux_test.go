//go:build linux

package piglitter

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMemberExecutableWaitsForOwnedExec(t *testing.T) {
	directory := t.TempDir()
	ready, gate := filepath.Join(directory, "ready"), filepath.Join(directory, "gate")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	expected, err := memberExecutableDigest(ctx, executable)
	if err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "sh", "-c", `: > "$1"; while [ ! -e "$2" ]; do sleep 0.01; done; exec "$3" --pig-litter-member "$4"`, "exec-delay", ready, gate, executable, filepath.Join(directory, "member"))
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("delayed process did not start")
		case <-time.After(10 * time.Millisecond):
		}
	}
	birth, err := processBirth(command.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	pane := memberPaneIdentity{PID: command.Process.Pid, Birth: birth}
	result := make(chan error, 1)
	go func() { result <- verifyMemberExecutable(ctx, pane, executable, expected) }()
	early := false
	select {
	case err := <-result:
		early = true
		t.Errorf("executable verification returned before the owned process executed its target: %v", err)
	case <-time.After(500 * time.Millisecond):
	}
	if err := os.WriteFile(gate, []byte("go"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !early {
		select {
		case err := <-result:
			if err != nil {
				t.Fatalf("executable verification after exec: %v", err)
			}
		case <-ctx.Done():
			t.Fatal("executable verification did not finish after exec")
		}
		wrongCtx, cancelWrong := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancelWrong()
		if err := verifyMemberExecutable(wrongCtx, pane, executable, strings.Repeat("0", 64)); err == nil {
			t.Fatal("a permanently incorrect executable digest was accepted")
		}
	}
}
