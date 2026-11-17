package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

type collaborationCLIFixtureResource struct {
	Path     string `json:"path"`
	Identity string `json:"identity"`
}

type collaborationCLIFixtureEvidence struct {
	Clock                  string                          `json:"clock"`
	BootID                 string                          `json:"boot_id"`
	TimeNamespace          string                          `json:"time_namespace"`
	ProviderPort           int                             `json:"provider_port"`
	ProviderStartedNS      int64                           `json:"provider_started_ns"`
	ProviderCloseStartedNS int64                           `json:"provider_close_started_ns"`
	Workspace              collaborationCLIFixtureResource `json:"workspace"`
	FixtureFile            collaborationCLIFixtureResource `json:"fixture_file"`
	RootSessions           []struct {
		ID     string                          `json:"id"`
		File   collaborationCLIFixtureResource `json:"file"`
		SHA256 string                          `json:"sha256"`
	} `json:"root_sessions"`
}

type collaborationCLIFixtureResult struct {
	OK        bool                            `json:"ok"`
	Error     string                          `json:"error"`
	Startup   collaborationPerformanceStartup `json:"startup"`
	ChildRefs []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"childRefs"`
	SelectedToolNames []string                        `json:"selectedToolNames"`
	Fixture           collaborationCLIFixtureEvidence `json:"fixture"`
	Cleanup           struct {
		ProviderJoined          bool   `json:"providerJoined"`
		ObservedProcessesExited bool   `json:"observedProcessesExited"`
		PaneJoined              bool   `json:"paneJoined"`
		TmuxSocketClosed        bool   `json:"tmuxSocketClosed"`
		TmuxServerAlive         bool   `json:"tmuxServerAlive"`
		ScratchRetained         string `json:"scratchRetainedAt"`
	} `json:"cleanup"`
}

func (runner *collaborationCLIRunner) fixtureRunner(name string) (*collaborationCLIRunner, error) {
	directory := filepath.Join(runner.directory, name)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(filepath.Join(directory, "events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &collaborationCLIRunner{options: runner.options, directory: directory, started: runner.started, eventFile: file}, nil
}

func (runner *collaborationCLIRunner) adoptFixture(name string, child *collaborationCLIRunner) error {
	if err := errors.Join(child.eventErr, child.eventFile.Close()); err != nil {
		return err
	}
	artifact, err := runner.artifact(filepath.Join(child.directory, "events.jsonl"), "timing")
	if err != nil {
		return err
	}
	runner.artifacts = append(runner.artifacts, artifact)
	for _, driver := range child.drivers {
		driver.Name = filepath.Join(name, driver.Name)
		runner.drivers = append(runner.drivers, driver)
	}
	return nil
}

func (runner *collaborationCLIRunner) fixtureIsolation(ctx context.Context, target collaborationCLITarget) error {
	binaries, verifierHash, observerHash, err := runner.buildTerminal(ctx, target)
	if err != nil {
		return err
	}
	children := make([]*collaborationCLIRunner, 2)
	for index, name := range []string{"fixture-a", "fixture-b"} {
		children[index], err = runner.fixtureRunner(name)
		if err != nil {
			return err
		}
	}
	type outcome struct {
		index  int
		sample collaborationPerformanceSample
		err    error
	}
	results := make(chan outcome, 2)
	start := make(chan struct{})
	for index, child := range children {
		go func(index int, child *collaborationCLIRunner) {
			<-start
			sample := collaborationPerformanceSample{Target: "candidate", ProductCommit: target.Identity.ProductCommit, Capture: true}
			err := child.performanceSample(ctx, "terminal", target, binaries, verifierHash, observerHash, &sample)
			results <- outcome{index: index, sample: sample, err: err}
		}(index, child)
	}
	close(start)
	var samples [2]collaborationPerformanceSample
	var failures []error
	for range children {
		outcome := <-results
		samples[outcome.index] = outcome.sample
		if outcome.err != nil {
			failures = append(failures, fmt.Errorf("fixture %d: %w", outcome.index, outcome.err))
		}
	}
	for index, name := range []string{"fixture-a", "fixture-b"} {
		if err := runner.adoptFixture(name, children[index]); err != nil {
			failures = append(failures, err)
		}
	}
	if err := collaborationCLIWriteNew(filepath.Join(runner.directory, "fixture-samples.json"), samples); err != nil {
		return errors.Join(errors.Join(failures...), err)
	}
	if len(failures) > 0 {
		return errors.Join(failures...)
	}
	var fixtureResults [2]collaborationCLIFixtureResult
	for index, sample := range samples {
		if err := json.Unmarshal(sample.Result, &fixtureResults[index]); err != nil {
			return err
		}
		if err := runner.collectFixtureCapture(ctx, sample.Evidence, false); err != nil {
			return err
		}
		if err := runner.validateFixtureSessions(sample.Evidence, fixtureResults[index]); err != nil {
			return err
		}
	}
	return collaborationCompareFixtureResults(fixtureResults[0], fixtureResults[1])
}

func collaborationCompareFixtureResults(left, right collaborationCLIFixtureResult) error {
	a, b := left.Fixture, right.Fixture
	if !left.OK || !right.OK || !left.Cleanup.ProviderJoined || !right.Cleanup.ProviderJoined || a.Clock != "CLOCK_MONOTONIC" || b.Clock != a.Clock || a.BootID == "" || a.BootID != b.BootID || a.TimeNamespace == "" || a.TimeNamespace != b.TimeNamespace {
		return errors.New("fixtures do not share an attributable monotonic clock and completed provider lifecycle")
	}
	if left.Startup.Process.PID <= 0 || right.Startup.Process.PID <= 0 || left.Startup.Process.PID == right.Startup.Process.PID {
		return errors.New("fixtures did not run in distinct PiG processes")
	}
	if a.ProviderPort <= 0 || b.ProviderPort <= 0 || a.ProviderPort == b.ProviderPort || a.ProviderStartedNS <= 0 || b.ProviderStartedNS <= 0 || a.ProviderCloseStartedNS <= a.ProviderStartedNS || b.ProviderCloseStartedNS <= b.ProviderStartedNS || a.ProviderStartedNS >= b.ProviderCloseStartedNS || b.ProviderStartedNS >= a.ProviderCloseStartedNS {
		return errors.New("fixture ports are not distinct or provider lifetimes did not overlap")
	}
	for _, pair := range [][2]collaborationCLIFixtureResource{{a.Workspace, b.Workspace}, {a.FixtureFile, b.FixtureFile}} {
		if pair[0].Path == "" || pair[1].Path == "" || pair[0].Path == pair[1].Path || pair[0].Identity == "" || pair[1].Identity == "" || pair[0].Identity == pair[1].Identity {
			return errors.New("fixture writable resources are not distinct")
		}
	}
	if len(a.RootSessions) == 0 || len(b.RootSessions) == 0 {
		return errors.New("fixture root sessions are absent")
	}
	ids := make(map[string]bool)
	files := make(map[string]bool)
	for _, session := range a.RootSessions {
		ids[session.ID] = true
		files[session.File.Identity] = true
	}
	for _, session := range b.RootSessions {
		if ids[session.ID] || files[session.File.Identity] {
			return errors.New("fixture root sessions or file identities overlap")
		}
	}
	var leftNames, rightNames []string
	leftIDs := make(map[string]bool)
	for _, child := range left.ChildRefs {
		if child.ID == "" || child.Name == "" {
			return errors.New("fixture child identity is missing")
		}
		leftNames = append(leftNames, child.Name)
		leftIDs[child.ID] = true
	}
	for _, child := range right.ChildRefs {
		if child.ID == "" || child.Name == "" || leftIDs[child.ID] {
			return errors.New("fixture child IDs are missing or overlap")
		}
		rightNames = append(rightNames, child.Name)
	}
	sort.Strings(leftNames)
	sort.Strings(rightNames)
	if len(leftNames) == 0 || !reflect.DeepEqual(leftNames, rightNames) {
		return errors.New("fixtures did not execute the same named child tasks")
	}
	return nil
}

func (runner *collaborationCLIRunner) validateFixtureSessions(evidence string, result collaborationCLIFixtureResult) error {
	for _, session := range result.Fixture.RootSessions {
		if !filepath.IsLocal(session.ID) || filepath.Base(session.ID) != session.ID || session.File.Path == "" || session.File.Identity == "" || !collaborationSHA256(session.SHA256) {
			return errors.New("invalid root session identity receipt")
		}
		path := filepath.Join(evidence, "native-final", session.ID+".jsonl")
		artifact, err := runner.artifact(path, "log")
		if err != nil || artifact.SHA256 != session.SHA256 {
			return errors.New("retained native session differs from its identity receipt")
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		scanner := bufio.NewScanner(file)
		var header struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		}
		valid := scanner.Scan() && json.Unmarshal(scanner.Bytes(), &header) == nil && header.Type == "session" && header.ID == session.ID
		closeErr := errors.Join(scanner.Err(), file.Close())
		if !valid || closeErr != nil {
			return errors.New("retained native session header differs from its identity receipt")
		}
		runner.artifacts = append(runner.artifacts, artifact)
	}
	return nil
}

func (runner *collaborationCLIRunner) collectFixtureCapture(ctx context.Context, evidence string, interrupted bool) error {
	data, err := os.ReadFile(filepath.Join(evidence, "media", "receipt.json"))
	var media collaborationCLIMediaReceipt
	if err != nil || json.Unmarshal(data, &media) != nil || (!interrupted && media.Status != "complete") || (interrupted && (media.Status != "inconclusive" || !media.ProcessesExited)) || media.Screenshot.Origin != "video_frame" || media.Screenshot.SourceVideo != media.Video.SHA256 || media.Screenshot.SourceMillis == nil {
		return errors.New("fixture capture receipt is incomplete")
	}
	frames, err := filepath.Glob(filepath.Join(evidence, "*-ansi.txt"))
	if err != nil || len(frames) == 0 {
		return errors.New("fixture ANSI frame is absent")
	}
	var artifacts []collaborationArtifact
	for _, item := range []struct{ path, kind string }{
		{filepath.Join(evidence, "pty.raw"), "pty"}, {frames[0], "ansi"},
		{filepath.Join(evidence, "result.json"), "log"}, {filepath.Join(evidence, "media", "receipt.json"), "log"},
		{filepath.Join(evidence, "media", "terminal.mp4"), "video"}, {filepath.Join(evidence, "media", "screenshot.png"), "screenshot"},
	} {
		artifact, err := runner.artifact(item.path, item.kind)
		if err != nil {
			return err
		}
		if item.kind == "video" {
			artifact.Origin = "live_terminal"
			if artifact.SHA256 != media.Video.SHA256 {
				return errors.New("fixture video digest differs from capture receipt")
			}
		}
		if item.kind == "screenshot" {
			artifact.Origin, artifact.SourceVideo, artifact.SourceTimestamp = "video_frame", media.Screenshot.SourceVideo, media.Screenshot.SourceMillis
			if artifact.SHA256 != media.Screenshot.SHA256 {
				return errors.New("fixture screenshot digest differs from capture receipt")
			}
		}
		artifacts = append(artifacts, artifact)
	}
	root, err := os.OpenRoot(runner.directory)
	if err != nil {
		return err
	}
	_, _, validationErr := collaborationValidateArtifacts(ctx, root, artifacts, collaborationMediaTools{FFmpeg: runner.options.ffmpeg, FFprobe: runner.options.ffprobe})
	if err := errors.Join(validationErr, root.Close()); err != nil {
		return err
	}
	runner.artifacts = append(runner.artifacts, artifacts...)
	return nil
}

func (runner *collaborationCLIRunner) fixtureInterrupt(ctx context.Context, target collaborationCLITarget) (resultErr error) {
	binaries, verifierHash, observerHash, err := runner.buildTerminal(ctx, target)
	if err != nil {
		return err
	}
	child, err := runner.fixtureRunner("interrupted-attempt")
	if err != nil {
		return err
	}
	child.attempt = collaborationAttempt{
		Version: 1, ID: runner.attempt.ID + "-child", Scenario: "startup-standalone", ScenarioRevision: 1,
		Target: target.Identity, Purpose: "verification", Channel: "live", Platform: runner.attempt.Platform,
		Architecture: runner.attempt.Architecture, Host: runner.attempt.Host,
		Command:     child.performanceCommand(target, binaries, filepath.Join(child.directory, "terminal", "evidence"), true),
		Observation: "terminal interruption verification attempt", Outcome: "inconclusive",
		Reason: "interrupted child did not finish", Events: []collaborationEvent{}, Claims: []string{}, Artifacts: []collaborationArtifact{},
	}
	if err := collaborationCLIWriteNew(filepath.Join(child.directory, "driver-identity.json"), map[string]string{"path": binaries["verify-tui"], "sha256": verifierHash, "declared_commit": target.Identity.DriverCommit}); err != nil {
		_ = child.eventFile.Close()
		return err
	}
	if err := collaborationCLIWriteNew(filepath.Join(child.directory, "started.json"), child.attempt); err != nil {
		_ = child.eventFile.Close()
		return err
	}
	child.event("started")
	defer func() {
		child.event("finished")
		if resultErr != nil {
			child.attempt.Reason = resultErr.Error()
		} else {
			child.attempt.Reason = "the terminal fixture was intentionally interrupted after startup"
		}
		closeErr := errors.Join(child.eventErr, child.eventFile.Close())
		finishErr := child.finish(false)
		resultErr = errors.Join(resultErr, closeErr, finishErr)
		for _, driver := range child.drivers {
			driver.Name = filepath.Join("interrupted-attempt", driver.Name)
			runner.drivers = append(runner.drivers, driver)
		}
		for _, artifact := range child.attempt.Artifacts {
			path := filepath.Join(child.directory, artifact.Path)
			kind := artifact.Kind
			if kind == "result" {
				kind = "log"
			}
			adopted, err := runner.artifact(path, kind)
			if err != nil {
				resultErr = errors.Join(resultErr, err)
				continue
			}
			adopted.Origin, adopted.SourceVideo, adopted.SourceTimestamp = artifact.Origin, artifact.SourceVideo, artifact.SourceTimestamp
			runner.artifacts = append(runner.artifacts, adopted)
		}
		for _, name := range []string{"started.json", "manifest.json", "selection.json"} {
			artifact, err := runner.artifact(filepath.Join(child.directory, name), "log")
			if err != nil {
				resultErr = errors.Join(resultErr, err)
				continue
			}
			runner.artifacts = append(runner.artifacts, artifact)
		}
	}()
	childCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		sample := collaborationPerformanceSample{Target: "candidate", ProductCommit: target.Identity.ProductCommit, Capture: true}
		done <- child.performanceSample(childCtx, "terminal", target, binaries, verifierHash, observerHash, &sample)
	}()
	deadline := time.NewTimer(4 * time.Minute)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var observedAt time.Time
	for {
		select {
		case err := <-done:
			return fmt.Errorf("fixture exited before the intended interruption: %v", err)
		case <-ctx.Done():
			cancel()
			<-done
			return ctx.Err()
		case <-deadline.C:
			cancel()
			<-done
			return errors.New("fixture did not reach an observable startup frame")
		case <-ticker.C:
			frames, _ := filepath.Glob(filepath.Join(child.directory, "terminal", "evidence", "*direct*", "startup-100-ansi.txt"))
			if observedAt.IsZero() && len(frames) == 1 {
				if info, err := os.Stat(frames[0]); err == nil && info.Size() > 0 {
					observedAt = time.Now()
					runner.event("child-startup-frame-observed")
				}
			}
			if !observedAt.IsZero() && time.Since(observedAt) >= 2*time.Second {
				cancel()
				if err := <-done; err == nil {
					return errors.New("fixture completed successfully instead of being interrupted")
				}
				return runner.validateInterruptedFixture(ctx, child)
			}
		}
	}
}

func (runner *collaborationCLIRunner) validateInterruptedFixture(ctx context.Context, child *collaborationCLIRunner) error {
	paths, err := filepath.Glob(filepath.Join(child.directory, "terminal", "evidence", "*direct*", "result.json"))
	if err != nil || len(paths) != 1 || len(child.drivers) != 1 || child.drivers[0].ExitCode == 0 || !child.drivers[0].Joined || child.drivers[0].ForcedCleanup {
		return errors.New("interrupted driver did not retain a failing terminal result")
	}
	data, err := os.ReadFile(paths[0])
	var result collaborationCLIFixtureResult
	if err != nil || json.Unmarshal(data, &result) != nil || result.OK || !strings.Contains(strings.ToLower(result.Error), "cancel") || !result.Cleanup.ProviderJoined || !result.Cleanup.ObservedProcessesExited || !result.Cleanup.PaneJoined || !result.Cleanup.TmuxSocketClosed || result.Cleanup.TmuxServerAlive || result.Cleanup.ScratchRetained == "" {
		return errors.New("interrupted terminal result does not prove cancellation and owned-process cleanup")
	}
	if err := child.collectFixtureCapture(ctx, filepath.Dir(paths[0]), true); err != nil {
		return err
	}
	child.attempt.Observation = "terminal fixture intentionally interrupted after its startup frame"
	child.drivers[0].Evidence = filepath.Dir(paths[0])
	return nil
}

func (runner *collaborationCLIRunner) independentTerminal(ctx context.Context, target collaborationCLITarget, probeRoot string) error {
	data, err := os.ReadFile(filepath.Join(probeRoot, "result.json"))
	var probe collaborationProcessEvidence
	if err != nil || json.Unmarshal(data, &probe) != nil || probe.Status != "observed" || probe.HostPID <= 0 || probe.ParentPID <= 0 || probe.HostPID == probe.ParentPID || probe.Pig != target.Pig || probe.PigSHA256 != target.Identity.HostBinarySHA256 || probe.Extension != target.Extension || probe.ExtensionSHA256 != target.Identity.ProductBinarySHA256 || probe.SessionID == "" || !collaborationCLIWithin(probeRoot, probe.SessionFile) {
		return errors.New("independent JSON process did not retain its selected tools and native session")
	}
	artifact, err := runner.artifact(probe.SessionFile, "log")
	if err != nil {
		return err
	}
	runner.artifacts = append(runner.artifacts, artifact)
	if err := runner.terminal(ctx, "independent-terminal", target, "direct", "receipt-control"); err != nil {
		return err
	}
	evidence := runner.drivers[len(runner.drivers)-1].Evidence
	data, err = os.ReadFile(filepath.Join(evidence, "result.json"))
	var terminal collaborationCLIFixtureResult
	if err != nil || json.Unmarshal(data, &terminal) != nil || !terminal.OK || terminal.Startup.Process.PID <= 0 || terminal.Startup.Process.PID == probe.HostPID || len(terminal.Fixture.RootSessions) == 0 {
		return errors.New("independent terminal did not retain its own process and native session identity")
	}
	selectedHost, selectedErr := os.Stat(target.Pig)
	observedHost, observedErr := os.Stat(terminal.Startup.Process.Executable)
	if selectedErr != nil || observedErr != nil || !os.SameFile(selectedHost, observedHost) {
		return errors.New("independent terminal ran a different PiG executable")
	}
	for _, tool := range []string{"litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"} {
		if !collaborationContains(terminal.SelectedToolNames, tool) || !collaborationContains(probe.ProviderTools, tool) {
			return fmt.Errorf("independent process registration lacks %q", tool)
		}
	}
	for _, session := range terminal.Fixture.RootSessions {
		if session.ID == probe.SessionID {
			return errors.New("independent JSON and terminal observations reused the same session identity")
		}
	}
	if err := runner.validateFixtureSessions(evidence, terminal); err != nil {
		return err
	}
	runner.attempt.Observation = "Two independent selected PiG processes were observed. The retained terminal media belongs to the TUI process; the JSON process has a separate native session and log."
	return nil
}
