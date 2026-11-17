package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

type collaborationCLIDriver struct {
	Name                  string   `json:"name"`
	Command               []string `json:"command"`
	ExecutableSHA256      string   `json:"executable_sha256"`
	StartedNS             int64    `json:"started_ns"`
	FinishedNS            int64    `json:"finished_ns"`
	ExitCode              int      `json:"exit_code"`
	ProcessID             int      `json:"process_id,omitempty"`
	ProcessStart          string   `json:"process_start_ticks,omitempty"`
	Joined                bool     `json:"joined"`
	ForcedCleanup         bool     `json:"forced_cleanup"`
	TemporaryAlias        string   `json:"temporary_alias,omitempty"`
	TemporaryAliasTarget  string   `json:"temporary_alias_target,omitempty"`
	TemporaryAliasRemoved bool     `json:"temporary_alias_removed"`
	Error                 string   `json:"error,omitempty"`
	Evidence              string   `json:"evidence,omitempty"`
}

type collaborationCLIRunner struct {
	options        collaborationCLIOptions
	directory      string
	started        time.Time
	attempt        collaborationAttempt
	drivers        []collaborationCLIDriver
	artifacts      []collaborationArtifact
	eventFile      *os.File
	eventDurations []int64
	eventErr       error
}

func collaborationCLIRun(ctx context.Context, options collaborationCLIOptions, args []string) (returnErr error) {
	catalogData, err := os.ReadFile(options.catalog)
	if err != nil {
		return err
	}
	catalog, err := collaborationReadCatalog(catalogData)
	if err != nil {
		return err
	}
	known, catalogImplemented := false, false
	for _, scenario := range catalog.Scenarios {
		if scenario.ID == options.scenario && scenario.Owner == "P02" {
			known = true
			catalogImplemented = scenario.Status == "implemented"
			break
		}
	}
	if !known {
		return fmt.Errorf("unknown or unimplemented P02 scenario %q", options.scenario)
	}
	target, err := collaborationReadTarget(options.target)
	if err != nil {
		return fmt.Errorf("target: %w", err)
	}
	if digest := collaborationTestDataDigest(catalogData); digest != target.CatalogSHA256 {
		return errors.New("target does not identify the supplied catalog")
	}
	if err := os.MkdirAll(options.root, 0o700); err != nil {
		return err
	}
	directory, err := os.MkdirTemp(options.root, options.scenario+"-")
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	selfDigest, err := collaborationCLIDigest(self)
	if err != nil {
		return err
	}
	if err := collaborationCLIWriteNew(filepath.Join(directory, "driver-identity.json"), map[string]string{"path": self, "sha256": selfDigest, "declared_commit": target.DriverCommit}); err != nil {
		return err
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		return errors.New("cannot identify the host")
	}
	definition, implemented := collaborationCLIScenarios[options.scenario]
	trusted, registered := collaborationP02Scenarios[options.scenario]
	if implemented && (!registered || trusted.Purpose != definition.purpose || trusted.Channel != definition.channel || trusted.Revision != 1) {
		return errors.New("scenario dispatch differs from the trusted P02 registry")
	}
	if !implemented {
		definition = collaborationCLIScenario{purpose: "verification", channel: "live"}
		if registered {
			definition.purpose, definition.channel = trusted.Purpose, trusted.Channel
		}
	}
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	runner := &collaborationCLIRunner{
		options: options, directory: directory, started: time.Now(),
		attempt: collaborationAttempt{
			Version: 1, ID: filepath.Base(directory), Scenario: options.scenario, ScenarioRevision: 1,
			Target: target, Purpose: definition.purpose, Channel: definition.channel, Platform: platform,
			Architecture: runtime.GOARCH, Host: host, Command: append([]string{self, "collaboration"}, args...),
			Observation: "P02 verification; product evidence remains unverified", Outcome: "inconclusive",
			Reason: "attempt did not complete", Events: []collaborationEvent{}, Claims: []string{}, Artifacts: []collaborationArtifact{},
		},
	}
	if err := collaborationCLIWriteNew(filepath.Join(directory, "started.json"), runner.attempt); err != nil {
		return err
	}
	runner.eventFile, err = os.OpenFile(filepath.Join(directory, "events.jsonl"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "attempt evidence: %s\n", directory)
	defer func() {
		if ctx.Err() != nil {
			returnErr = errors.Join(returnErr, ctx.Err())
		}
		if returnErr == nil && !catalogImplemented {
			returnErr = errors.New("scenario remains planned in the catalog; retained observations are provisional")
		}
		runner.event("finished")
		if options.scenario == "capture-performance" && returnErr == nil && collaborationPercentile(runner.eventDurations, 0.95) >= 50_000_000 {
			returnErr = &collaborationPerformanceFailure{message: "event serialization p95 is at least 50 ms"}
		}
		if returnErr != nil {
			runner.attempt.Outcome = "inconclusive"
			var failure *collaborationPerformanceFailure
			if ctx.Err() == nil && errors.As(returnErr, &failure) {
				runner.attempt.Outcome = "failed"
			}
			runner.attempt.Reason = returnErr.Error()
		} else {
			runner.attempt.Outcome = "passed"
			runner.attempt.Reason = ""
		}
		if err := errors.Join(runner.eventErr, runner.eventFile.Close()); err != nil {
			returnErr = errors.Join(returnErr, fmt.Errorf("write event evidence: %w", err))
			runner.attempt.Outcome = "inconclusive"
			runner.attempt.Reason = returnErr.Error()
		}
		finalErr := runner.finish(true)
		if finalErr != nil {
			returnErr = errors.Join(returnErr, finalErr)
		}
	}()
	runner.event("started")
	if !implemented {
		return fmt.Errorf("scenario %q is planned but has no integrated production driver", options.scenario)
	}
	selected := collaborationCLITarget{Identity: target, Pig: options.pig, Extension: options.extension, Checkout: options.checkout}
	if err := collaborationCLICheckTarget(selected); err != nil {
		return err
	}
	driverCommit, err := exec.Command("git", "-C", options.repo, "rev-parse", "HEAD").Output()
	if err != nil || strings.TrimSpace(string(driverCommit)) != target.DriverCommit {
		return fmt.Errorf("driver commit does not match repository HEAD: %v", err)
	}
	driverStatus, err := exec.Command("git", "-C", options.repo, "status", "--porcelain", "--untracked-files=all").Output()
	if err != nil || strings.TrimSpace(string(driverStatus)) != "" {
		return errors.New("driver checkout is not clean")
	}
	if err := runner.verifyTarget(ctx, "candidate", selected); err != nil {
		return fmt.Errorf("candidate identity: %w", err)
	}
	if definition.purpose == "research" {
		return runner.probe(ctx, self, selected)
	}
	if options.scenario == "fixture-isolation" {
		return runner.fixtureIsolation(ctx, selected)
	}
	if options.scenario == "fixture-interrupt" {
		return runner.fixtureInterrupt(ctx, selected)
	}
	if options.scenario != "baseline" && options.scenario != "capture-performance" {
		terminalScenario := "receipt-control"
		if definition.caseName == "unselected" {
			terminalScenario = "all"
		}
		return runner.terminal(ctx, "candidate-terminal", selected, definition.caseName, terminalScenario)
	}
	baselineIdentity, err := collaborationReadTarget(options.baselineTarget)
	if err != nil {
		return fmt.Errorf("baseline target: %w", err)
	}
	baseline := collaborationCLITarget{Identity: baselineIdentity, Pig: options.baselinePig, Extension: options.baselineExtension, Checkout: options.baselineCheckout}
	if baseline.Identity.DriverCommit != target.DriverCommit || baseline.Identity.CatalogSHA256 != target.CatalogSHA256 {
		return errors.New("baseline and candidate must use the same driver commit and catalog")
	}
	if err := collaborationCLICheckTarget(baseline); err != nil {
		return err
	}
	if err := runner.verifyTarget(ctx, "baseline", baseline); err != nil {
		return fmt.Errorf("baseline identity: %w", err)
	}
	if err := collaborationCLIWriteNew(filepath.Join(directory, "targets.json"), []collaborationCLITarget{baseline, selected}); err != nil {
		return err
	}
	if options.scenario == "capture-performance" {
		return runner.performance(ctx, baseline, selected)
	}
	var failures []error
	for _, item := range []struct {
		name   string
		target collaborationCLITarget
	}{
		{"baseline-lifecycle", baseline},
		{"candidate-lifecycle", selected},
	} {
		if ctx.Err() != nil {
			break
		}
		if stepErr := runner.lifecycle(ctx, self, item.name, item.target); stepErr != nil {
			failures = append(failures, fmt.Errorf("%s: %w", item.name, stepErr))
		}
	}
	if ctx.Err() == nil {
		if err := runner.baselineTerminal(ctx, baseline, selected); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func collaborationTestDataDigest(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func (runner *collaborationCLIRunner) event(name string) {
	event := collaborationEvent{Name: name, OffsetNS: time.Since(runner.started).Nanoseconds()}
	runner.attempt.Events = append(runner.attempt.Events, event)
	started := time.Now()
	runner.eventErr = errors.Join(runner.eventErr, json.NewEncoder(runner.eventFile).Encode(event))
	runner.eventDurations = append(runner.eventDurations, time.Since(started).Nanoseconds())
}

func (runner *collaborationCLIRunner) execute(ctx context.Context, name string, command []string) (string, error) {
	return runner.executeAt(ctx, name, command, runner.options.repo)
}

func (runner *collaborationCLIRunner) executeAt(ctx context.Context, name string, command []string, workingDirectory string) (string, error) {
	directory := filepath.Join(runner.directory, name)
	if err := os.Mkdir(directory, 0o700); err != nil {
		return "", err
	}
	for _, child := range []string{"home", "tmp", "config", "cache", "state", "data", "runtime"} {
		if err := os.Mkdir(filepath.Join(directory, child), 0o700); err != nil {
			return "", err
		}
	}
	alias, err := collaborationNewTempAlias(filepath.Join(directory, "tmp"))
	if err != nil {
		return directory, err
	}
	stdout, err := os.Create(filepath.Join(directory, "stdout.log"))
	if err != nil {
		_, _ = alias.release(true)
		return "", err
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(directory, "stderr.log"))
	if err != nil {
		_, _ = alias.release(true)
		return "", err
	}
	defer stderr.Close()
	cmd := exec.Command(command[0], command[1:]...)
	prepareProcess(cmd)
	cmd.Dir = workingDirectory
	cmd.Env = []string{
		"PATH=" + os.Getenv("PATH"), "LANG=C.UTF-8", "TERM=xterm-256color",
		"HOME=" + filepath.Join(directory, "home"), "USERPROFILE=" + filepath.Join(directory, "home"),
		"PIG_HOME=" + filepath.Join(directory, "home"), "PIG_CODING_AGENT_DIR=" + filepath.Join(directory, "agent"),
		"PIG_CODING_AGENT_SESSION_DIR=" + filepath.Join(directory, "sessions"), "TMPDIR=" + alias.path,
		"XDG_CONFIG_HOME=" + filepath.Join(directory, "config"), "XDG_CACHE_HOME=" + filepath.Join(directory, "cache"),
		"XDG_STATE_HOME=" + filepath.Join(directory, "state"), "XDG_DATA_HOME=" + filepath.Join(directory, "data"),
		"XDG_RUNTIME_DIR=" + filepath.Join(directory, "runtime"), "PIG_OFFLINE=1", "PI_OFFLINE=1", "PIG_USE_PI_DIRS=0", "PI_TELEMETRY=0",
	}
	for _, name := range []string{"GOCACHE", "GOMODCACHE", "GOPATH", "GOTMPDIR", "GOENV", "GOTELEMETRY"} {
		if value := os.Getenv(name); value != "" {
			cmd.Env = append(cmd.Env, name+"="+value)
		}
	}
	cmd.Stdout, cmd.Stderr = stdout, stderr
	digest, err := collaborationCLIDigest(command[0])
	if err != nil {
		_, _ = alias.release(true)
		return directory, err
	}
	driver := collaborationCLIDriver{Name: name, Command: command, ExecutableSHA256: digest, StartedNS: time.Since(runner.started).Nanoseconds(), ExitCode: -1}
	runner.event(name + "-start")
	err = ctx.Err()
	if err == nil {
		err = cmd.Start()
	}
	if err == nil {
		driver.ProcessID = cmd.Process.Pid
		driver.ProcessStart, _ = collaborationCLIProcessIdentity(cmd)
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		select {
		case err = <-done:
			driver.Joined = true
		case <-ctx.Done():
			signalErr := cmd.Process.Signal(os.Interrupt)
			timer := time.NewTimer(20 * time.Second)
			select {
			case err = <-done:
				driver.Joined = true
			case <-timer.C:
				select {
				case err = <-done:
					driver.Joined = true
				default:
					driver.ForcedCleanup = true
					killErr := collaborationCLIForceOwned(cmd, driver.ProcessStart)
					select {
					case err = <-done:
						driver.Joined = true
					case <-time.After(5 * time.Second):
						err = errors.New("owned process did not join after forced cleanup; resources retained")
					}
					if killErr != nil && !errors.Is(killErr, os.ErrNotExist) {
						err = errors.Join(err, fmt.Errorf("forced cleanup: %w", killErr))
					}
				}
			}
			timer.Stop()
			if signalErr != nil && !errors.Is(signalErr, os.ErrProcessDone) {
				err = errors.Join(err, fmt.Errorf("signal child: %w", signalErr))
			}
			err = errors.Join(err, ctx.Err())
		}
		if driver.Joined && cmd.ProcessState != nil {
			driver.ExitCode = cmd.ProcessState.ExitCode()
		}
	}
	retained, releaseErr := alias.release(driver.Joined || driver.ProcessID == 0)
	driver.TemporaryAlias, driver.TemporaryAliasTarget = alias.path, alias.target
	driver.TemporaryAliasRemoved = retained == ""
	if releaseErr != nil {
		err = errors.Join(err, fmt.Errorf("temporary alias cleanup: %w", releaseErr))
	} else if retained != "" && err == nil {
		err = errors.New("driver left temporary resources in its private allocation")
	}
	if err != nil {
		driver.Error = err.Error()
	}
	driver.FinishedNS = time.Since(runner.started).Nanoseconds()
	runner.event(name + "-exit")
	runner.drivers = append(runner.drivers, driver)
	return directory, err
}

func (runner *collaborationCLIRunner) lifecycle(ctx context.Context, self, name string, target collaborationCLITarget) error {
	root := filepath.Join(runner.directory, name, "evidence")
	command := []string{self, "--scenario", "core", "--pig-bin", target.Pig, "--extension", target.Extension, "--evidence-root", root}
	directory, err := runner.execute(ctx, name, command)
	if err != nil {
		return err
	}
	var result struct {
		Directory string `json:"directory"`
		Passed    bool   `json:"passed"`
		Scenario  string `json:"scenario"`
	}
	data, readErr := os.ReadFile(filepath.Join(directory, "stdout.log"))
	if readErr != nil || json.Unmarshal(data, &result) != nil || !result.Passed || result.Scenario != "core" || !collaborationCLIWithin(root, result.Directory) {
		return errors.New("lifecycle driver did not return an attributable core result")
	}
	resultData, err := os.ReadFile(filepath.Join(result.Directory, "result.json"))
	if err != nil {
		return err
	}
	passed, err := collaborationResultPassed(resultData, "lifecycle result")
	if err != nil || !passed {
		return errors.New("lifecycle result did not pass")
	}
	for _, name := range []string{"result.json", "requests.json", "stdout.jsonl", "stderr.log"} {
		path := filepath.Join(result.Directory, name)
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			artifact, err := runner.artifact(path, "log")
			if err != nil {
				return err
			}
			runner.artifacts = append(runner.artifacts, artifact)
		}
	}
	runner.drivers[len(runner.drivers)-1].Evidence = result.Directory
	return collaborationCLICheckTarget(target)
}

func collaborationCLIWithin(root, child string) bool {
	relative, err := filepath.Rel(root, child)
	return err == nil && relative != "." && filepath.IsLocal(relative) && filepath.IsAbs(child)
}
