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

type collaborationCrashEvidence struct {
	Schema                 int                   `json:"schema"`
	Purpose                string                `json:"purpose"`
	Status                 string                `json:"status"`
	Stage                  string                `json:"stage"`
	StartedUTC             string                `json:"started_utc"`
	ElapsedNS              int64                 `json:"elapsed_ns"`
	Command                []string              `json:"command,omitempty"`
	WorkerPID              int                   `json:"worker_pid,omitempty"`
	RestartPID             int                   `json:"restart_pid"`
	CutOffsetNS            int64                 `json:"cut_offset_ns,omitempty"`
	CrashOffsetNS          int64                 `json:"crash_offset_ns,omitempty"`
	ReplayOffsetNS         int64                 `json:"replay_offset_ns,omitempty"`
	Cut                    collaborationCrashCut `json:"cut"`
	CopiesBeforeRestart    int                   `json:"copies_before_restart"`
	CopiesAfterReplay      int                   `json:"copies_after_replay"`
	SameSession            bool                  `json:"same_session"`
	DuplicateExistingEntry bool                  `json:"duplicate_existing_entry"`
	ProviderRequests       []json.RawMessage     `json:"provider_requests,omitempty"`
	Error                  string                `json:"error,omitempty"`
}

func collaborationRunCrashWorker(ctx context.Context, stage, root string) error {
	if !filepath.IsAbs(root) || !collaborationCrashStage(stage) {
		return errors.New("invalid crash worker stage or root")
	}
	trusted := true
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: root, AgentDir: filepath.Join(root, "agent"), ProjectTrusted: &trusted})
	if err != nil {
		return err
	}
	defer services.Close()
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		return err
	}
	defer runtime.Close()
	const sessionID = "550e8400-e29b-41d4-a716-446655440000"
	session, err := runtime.New(coding.SessionStartOptions{SessionID: sessionID, SessionDir: filepath.Join(root, "sessions"), Model: services.ModelRuntime().GetModel("fixture", "probe"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		return err
	}
	defer session.Close()
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	if _, err := session.Send(ctx, "bootstrap before crash"); err != nil {
		return err
	}
	path := session.SessionManager().GetSessionFile()
	if path == nil {
		return errors.New("native session did not provide a persistence path")
	}
	cut := func(returned bool) error {
		data, err := os.ReadFile(*path)
		if err != nil {
			return err
		}
		count, err := collaborationCountMessage(data, "same-message-id", true)
		if err != nil {
			return err
		}
		value := collaborationCrashCut{Stage: stage, SessionID: sessionID, SessionFile: *path, ProcessID: os.Getpid(), SendReturned: returned, DiskCopies: count}
		encoded, err := json.Marshal(value)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(os.Stdout, "COLLAB_CUT %s\n", encoded)
		return err
	}
	if stage == "after-append-before-return" {
		session.Subscribe(func(event agent.AgentEvent) {
			if end, ok := event.(agent.MessageEndEvent); ok && end.Message.Custom != nil && end.Message.Custom["customType"] == "collab_peer" {
				if err := cut(false); err != nil {
					fmt.Fprintf(os.Stdout, "COLLAB_ERROR %v\n", err)
				}
				<-ctx.Done()
			}
		})
	}
	if stage == "after-accept-before-append" {
		go func() { _, _ = session.Send(ctx, "hold before custom append") }()
		if _, err := bufio.NewReader(os.Stdin).ReadString('\n'); err != nil {
			return err
		}
	}
	trigger := false
	if err := session.SendCustomMessage(ctx, collaborationPeerMessage("same-message-id"), &extension.SendMessageOptions{TriggerTurn: &trigger}); err != nil {
		return err
	}
	if err := cut(true); err != nil {
		return err
	}
	<-ctx.Done()
	return ctx.Err()
}

func collaborationCrashStage(stage string) bool {
	return stage == "after-accept-before-append" || stage == "after-append-before-return" || stage == "after-return"
}

func collaborationRunHostCrashProbe(ctx context.Context, root, stage string) (collaborationCrashEvidence, error) {
	self, err := os.Executable()
	if err != nil {
		return collaborationCrashEvidence{Schema: 1, Status: "inconclusive", Stage: stage}, err
	}
	return collaborationRunHostCrashProbeCommand(ctx, root, stage, []string{self, "collaboration", "--crash-worker", stage, root}, nil)
}

func collaborationRunHostCrashProbeCommand(ctx context.Context, root, stage string, args, extraEnv []string) (evidence collaborationCrashEvidence, resultErr error) {
	started := time.Now()
	evidence = collaborationCrashEvidence{Schema: 1, Purpose: "provisional host research; same-ID duplication is not product parity", Status: "inconclusive", Stage: stage, StartedUTC: started.UTC().Format(time.RFC3339Nano), RestartPID: os.Getpid(), Command: append([]string(nil), args...)}
	if err := collaborationProbePrepare(root); err != nil {
		return evidence, err
	}
	defer func() {
		evidence.ElapsedNS = time.Since(started).Nanoseconds()
		if resultErr != nil {
			evidence.Status = "inconclusive"
			evidence.Error = resultErr.Error()
		}
		resultErr = errors.Join(resultErr, collaborationProbeWriteJSON(filepath.Join(root, "result.json"), evidence))
	}()
	if !collaborationCrashStage(stage) || len(args) == 0 {
		return evidence, fmt.Errorf("invalid crash stage or worker command: %q", stage)
	}
	holdAt := 0
	if stage == "after-accept-before-append" {
		holdAt = 2
	}
	fixture, err := collaborationMessageFixtureNew(root, holdAt)
	if err != nil {
		return evidence, err
	}
	defer func() {
		resultErr = errors.Join(resultErr, fixture.close())
		fixture.mu.Lock()
		evidence.ProviderRequests = append([]json.RawMessage(nil), fixture.requests...)
		fixture.mu.Unlock()
	}()
	command := exec.CommandContext(ctx, args[0], args[1:]...)
	command.Env = append(os.Environ(), extraEnv...)
	stdin, err := command.StdinPipe()
	if err != nil {
		return evidence, err
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		return evidence, err
	}
	stderr, err := os.OpenFile(filepath.Join(root, "crash-worker.stderr"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return evidence, err
	}
	defer func() { resultErr = errors.Join(resultErr, stderr.Close()) }()
	workerOutput, err := os.OpenFile(filepath.Join(root, "crash-worker.stdout"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return evidence, err
	}
	defer func() { resultErr = errors.Join(resultErr, workerOutput.Close()) }()
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		return evidence, fmt.Errorf("start crash worker: %w", err)
	}
	evidence.WorkerPID = command.Process.Pid
	waited := false
	lines := make(chan string, 16)
	outputDone := make(chan error, 1)
	go func() {
		defer close(lines)
		scanner := bufio.NewScanner(io.TeeReader(stdout, workerOutput))
		notified := false
		for scanner.Scan() {
			line := scanner.Text()
			if notified || (!strings.HasPrefix(line, "COLLAB_CUT ") && !strings.HasPrefix(line, "COLLAB_ERROR ")) {
				continue
			}
			select {
			case lines <- line:
				notified = true
			case <-ctx.Done():
				outputDone <- ctx.Err()
				return
			}
		}
		outputDone <- scanner.Err()
	}()
	outputWaited := false
	var outputErr error
	waitForOutput := func() error {
		if !outputWaited {
			outputErr = <-outputDone
			outputWaited = true
		}
		return outputErr
	}
	defer func() {
		if !waited {
			_ = command.Process.Kill()
			resultErr = errors.Join(resultErr, waitForOutput())
			_ = command.Wait()
		}
		resultErr = errors.Join(resultErr, waitForOutput())
	}()
	if stage == "after-accept-before-append" {
		select {
		case <-fixture.holdReached:
			if _, err := fmt.Fprintln(stdin, "continue"); err != nil {
				return evidence, err
			}
		case line, ok := <-lines:
			if !ok {
				return evidence, errors.New("worker exited before the busy provider barrier")
			}
			return evidence, fmt.Errorf("worker reported %q before the busy provider barrier", line)
		case <-ctx.Done():
			return evidence, fmt.Errorf("worker did not reach the busy provider barrier: %w", ctx.Err())
		}
	}
	for evidence.Cut.Stage == "" {
		select {
		case line, ok := <-lines:
			if !ok {
				return evidence, errors.New("worker exited before the crash cut")
			}
			if strings.HasPrefix(line, "COLLAB_ERROR ") {
				return evidence, errors.New(line)
			}
			if strings.HasPrefix(line, "COLLAB_CUT ") {
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "COLLAB_CUT ")), &evidence.Cut); err != nil {
					return evidence, err
				}
				evidence.CutOffsetNS = time.Since(started).Nanoseconds()
			}
		case <-ctx.Done():
			return evidence, fmt.Errorf("worker did not reach the requested crash cut: %w", ctx.Err())
		}
	}
	cut := evidence.Cut
	if cut.Stage != stage || cut.ProcessID != command.Process.Pid || cut.ProcessID == os.Getpid() || cut.SessionID == "" || !filepath.IsAbs(cut.SessionFile) {
		return evidence, fmt.Errorf("invalid crash cut: %#v", cut)
	}
	relativeSession, err := filepath.Rel(filepath.Join(root, "sessions"), cut.SessionFile)
	if err != nil || !filepath.IsLocal(relativeSession) || relativeSession == "." {
		return evidence, fmt.Errorf("crash worker selected a session outside the fixture: %s", cut.SessionFile)
	}
	if (stage == "after-append-before-return") == cut.SendReturned {
		return evidence, fmt.Errorf("incorrect send-return boundary: %#v", cut)
	}
	if err := command.Process.Kill(); err != nil {
		return evidence, err
	}
	if err := waitForOutput(); err != nil {
		return evidence, fmt.Errorf("read crash worker output: %w", err)
	}
	waitErr := command.Wait()
	waited = true
	evidence.CrashOffsetNS = time.Since(started).Nanoseconds()
	var exitErr *exec.ExitError
	if !errors.As(waitErr, &exitErr) {
		return evidence, fmt.Errorf("worker did not exit through forced termination: %v", waitErr)
	}
	before, err := os.ReadFile(cut.SessionFile)
	if err != nil {
		return evidence, err
	}
	if err := os.WriteFile(filepath.Join(root, "after-crash-session.jsonl"), before, 0o600); err != nil {
		return evidence, err
	}
	evidence.CopiesBeforeRestart, err = collaborationCountMessage(before, "same-message-id", true)
	if err != nil {
		return evidence, err
	}
	wantBefore := 1
	if stage == "after-accept-before-append" {
		wantBefore = 0
	}
	if evidence.CopiesBeforeRestart != wantBefore || cut.DiskCopies != wantBefore {
		return evidence, fmt.Errorf("native file before restart has %d copies, cut reports %d, want %d", evidence.CopiesBeforeRestart, cut.DiskCopies, wantBefore)
	}
	resumed, err := fixture.runtime.Resume(cut.SessionID, coding.SessionStartOptions{SessionDir: filepath.Join(root, "sessions"), Model: fixture.services.ModelRuntime().GetModel("fixture", "probe"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		return evidence, err
	}
	evidence.SameSession = resumed.SessionManager().ID() == cut.SessionID
	if !evidence.SameSession {
		_ = resumed.Close()
		return evidence, errors.New("restart selected a different session ID")
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range resumed.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	trigger := false
	sendErr := resumed.SendCustomMessage(ctx, collaborationPeerMessage("same-message-id"), &extension.SendMessageOptions{TriggerTurn: &trigger})
	closeErr := resumed.Close()
	<-drained
	if err := errors.Join(sendErr, closeErr); err != nil {
		return evidence, err
	}
	evidence.ReplayOffsetNS = time.Since(started).Nanoseconds()
	after, err := os.ReadFile(cut.SessionFile)
	if err != nil {
		return evidence, err
	}
	if err := os.WriteFile(filepath.Join(root, "after-replay-session.jsonl"), after, 0o600); err != nil {
		return evidence, err
	}
	evidence.CopiesAfterReplay, err = collaborationCountMessage(after, "same-message-id", true)
	if err != nil {
		return evidence, err
	}
	evidence.DuplicateExistingEntry = evidence.CopiesBeforeRestart > 0 && evidence.CopiesAfterReplay > evidence.CopiesBeforeRestart
	if evidence.CopiesAfterReplay != evidence.CopiesBeforeRestart+1 {
		return evidence, fmt.Errorf("same-ID replay count = %d, want %d", evidence.CopiesAfterReplay, evidence.CopiesBeforeRestart+1)
	}
	evidence.Status = "observed"
	return evidence, nil
}
