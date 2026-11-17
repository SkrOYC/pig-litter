package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

type collaborationCrashCut struct {
	Stage        string `json:"stage"`
	SessionID    string `json:"session_id"`
	SessionFile  string `json:"session_file"`
	ProcessID    int    `json:"process_id"`
	SendReturned bool   `json:"send_returned"`
	DiskCopies   int    `json:"disk_copies"`
}

func collaborationCrashWorker(t *testing.T, stage, root string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	trusted := true
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: root, AgentDir: filepath.Join(root, "agent"), ProjectTrusted: &trusted})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	const sessionID = "550e8400-e29b-41d4-a716-446655440000"
	session, err := runtime.New(coding.SessionStartOptions{SessionID: sessionID, SessionDir: filepath.Join(root, "sessions"), Model: services.ModelRuntime().GetModel("fixture", "probe"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	go func() {
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	if _, err := session.Send(ctx, "bootstrap before crash"); err != nil {
		t.Fatal(err)
	}
	path := session.SessionManager().GetSessionFile()
	if path == nil {
		t.Fatal("native session did not provide a persistence path")
	}
	id := "same-message-id"
	cut := func(returned bool) {
		data, err := os.ReadFile(*path)
		if err != nil {
			fmt.Fprintf(os.Stdout, "P02_ERROR %v\n", err)
			return
		}
		count, err := collaborationCountMessage(data, id, true)
		if err != nil {
			fmt.Fprintf(os.Stdout, "P02_ERROR %v\n", err)
			return
		}
		value := collaborationCrashCut{Stage: stage, SessionID: sessionID, SessionFile: *path, ProcessID: os.Getpid(), SendReturned: returned, DiskCopies: count}
		encoded, _ := json.Marshal(value)
		fmt.Fprintf(os.Stdout, "P02_CUT %s\n", encoded)
	}
	if stage == "after-append-before-return" {
		session.Subscribe(func(event agent.AgentEvent) {
			if end, ok := event.(agent.MessageEndEvent); ok && end.Message.Custom != nil && end.Message.Custom["customType"] == "p02_peer" {
				cut(false)
				<-ctx.Done()
			}
		})
	}
	if stage == "after-accept-before-append" {
		go func() { _, _ = session.Send(ctx, "hold before custom append") }()
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			t.Fatal(err)
		}
	}
	trigger := false
	if err := session.SendCustomMessage(ctx, collaborationPeerMessage(id), &extension.SendMessageOptions{TriggerTurn: &trigger}); err != nil {
		t.Fatal(err)
	}
	cut(true)
	<-ctx.Done()
}

func TestCollaborationHostCrashReplay(t *testing.T) {
	if stage := os.Getenv("P02_CRASH_WORKER_STAGE"); stage != "" {
		collaborationCrashWorker(t, stage, os.Getenv("P02_CRASH_WORKER_ROOT"))
		return
	}
	for _, stage := range []string{"after-accept-before-append", "after-append-before-return", "after-return"} {
		t.Run(stage, func(t *testing.T) {
			holdAt := 0
			if stage == "after-accept-before-append" {
				holdAt = 2
			}
			fixture := collaborationMessageFixtureNew(t, holdAt)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestCollaborationHostCrashReplay$")
			command.Env = append(os.Environ(), "P02_CRASH_WORKER_STAGE="+stage, "P02_CRASH_WORKER_ROOT="+fixture.root)
			stdin, err := command.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := command.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stderr, err := os.Create(filepath.Join(fixture.root, "crash-worker.stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer stderr.Close()
			workerOutput, err := os.Create(filepath.Join(fixture.root, "crash-worker.stdout"))
			if err != nil {
				t.Fatal(err)
			}
			defer workerOutput.Close()
			command.Stderr = stderr
			if err := command.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if command.ProcessState == nil {
					_ = command.Process.Kill()
					_ = command.Wait()
				}
			})
			lines := make(chan string, 16)
			outputDone := make(chan struct{})
			go func() {
				defer close(outputDone)
				defer close(lines)
				scanner := bufio.NewScanner(io.TeeReader(stdout, workerOutput))
				for scanner.Scan() {
					lines <- scanner.Text()
				}
			}()
			if stage == "after-accept-before-append" {
				select {
				case <-fixture.holdReached:
					if _, err := fmt.Fprintln(stdin, "continue"); err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("worker did not reach the busy provider barrier")
				}
			}
			var cut collaborationCrashCut
			for cut.Stage == "" {
				select {
				case line, ok := <-lines:
					if !ok {
						t.Fatal("worker exited before the crash cut")
					}
					if strings.HasPrefix(line, "P02_ERROR ") {
						t.Fatal(line)
					}
					if strings.HasPrefix(line, "P02_CUT ") {
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "P02_CUT ")), &cut); err != nil {
							t.Fatal(err)
						}
					}
				case <-ctx.Done():
					t.Fatal("worker did not reach the requested crash cut")
				}
			}
			if cut.Stage != stage || cut.ProcessID != command.Process.Pid || cut.SessionID == "" || !filepath.IsAbs(cut.SessionFile) {
				t.Fatalf("invalid crash cut: %#v", cut)
			}
			if (stage == "after-append-before-return") == cut.SendReturned {
				t.Fatalf("incorrect send-return boundary: %#v", cut)
			}
			if err := command.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err := command.Wait(); err == nil {
				t.Fatal("crash worker exited without a forced termination")
			} else {
				var exitErr *exec.ExitError
				if !errors.As(err, &exitErr) {
					t.Fatalf("crash worker wait: %v", err)
				}
			}
			<-outputDone
			before, err := os.ReadFile(cut.SessionFile)
			if err != nil {
				t.Fatal(err)
			}
			beforeCount, err := collaborationCountMessage(before, "same-message-id", true)
			if err != nil {
				t.Fatal(err)
			}
			wantBefore := 1
			if stage == "after-accept-before-append" {
				wantBefore = 0
			}
			if beforeCount != wantBefore || cut.DiskCopies != wantBefore {
				t.Fatalf("native file before restart has %d copies, cut reports %d, want %d", beforeCount, cut.DiskCopies, wantBefore)
			}
			if err := os.WriteFile(filepath.Join(fixture.root, "after-crash-session.jsonl"), before, 0o600); err != nil {
				t.Fatal(err)
			}
			resumed, err := fixture.runtime.Resume(cut.SessionID, coding.SessionStartOptions{SessionDir: filepath.Join(fixture.root, "sessions"), Model: fixture.services.ModelRuntime().GetModel("fixture", "probe"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
			if err != nil {
				t.Fatal(err)
			}
			if resumed.SessionManager().ID() != cut.SessionID {
				_ = resumed.Close()
				t.Fatalf("restart session ID = %q, want %q", resumed.SessionManager().ID(), cut.SessionID)
			}
			drained := make(chan struct{})
			go func() {
				defer close(drained)
				for event := range resumed.Events() {
					coding.AcknowledgeEvent(event)
				}
			}()
			trigger := false
			if err := resumed.SendCustomMessage(ctx, collaborationPeerMessage("same-message-id"), &extension.SendMessageOptions{TriggerTurn: &trigger}); err != nil {
				_ = resumed.Close()
				<-drained
				t.Fatal(err)
			}
			if err := resumed.Close(); err != nil {
				t.Fatal(err)
			}
			<-drained
			after, err := os.ReadFile(cut.SessionFile)
			if err != nil {
				t.Fatal(err)
			}
			afterCount, err := collaborationCountMessage(after, "same-message-id", true)
			if err != nil || afterCount != beforeCount+1 {
				t.Fatalf("same-ID replay count = %d, want %d: %v", afterCount, beforeCount+1, err)
			}
			if err := os.WriteFile(filepath.Join(fixture.root, "after-replay-session.jsonl"), after, 0o600); err != nil {
				t.Fatal(err)
			}
			data, _ := json.MarshalIndent(map[string]any{"cut": cut, "restart_process_id": os.Getpid(), "copies_before_restart": beforeCount, "copies_after_replay": afterCount, "same_session": resumed.SessionManager().ID() == cut.SessionID, "duplicate_existing_entry": beforeCount > 0 && afterCount > beforeCount}, "", "  ")
			if err := os.WriteFile(filepath.Join(fixture.root, "crash-result.json"), append(data, '\n'), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Logf("%s: before restart=%d after same-ID replay=%d same session=%t", stage, beforeCount, afterCount, resumed.SessionManager().ID() == cut.SessionID)
		})
	}
}
