package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
)

type collaborationPerformanceSample struct {
	Target           string          `json:"target"`
	ProductCommit    string          `json:"product_commit"`
	Pair             int             `json:"pair"`
	Warmup           bool            `json:"warmup"`
	Capture          bool            `json:"capture"`
	DurationNS       int64           `json:"startup_duration_ns,omitempty"`
	DriverDurationNS int64           `json:"driver_duration_ns,omitempty"`
	Evidence         string          `json:"evidence,omitempty"`
	Result           json.RawMessage `json:"result,omitempty"`
	MediaReceipt     json.RawMessage `json:"media_receipt,omitempty"`
	StdoutSHA256     string          `json:"stdout_sha256,omitempty"`
	StderrSHA256     string          `json:"stderr_sha256,omitempty"`
	Error            string          `json:"error,omitempty"`
}

type collaborationPerformanceStartup struct {
	Clock   string `json:"clock"`
	Process struct {
		PID        int    `json:"pid"`
		StartTicks string `json:"startTicks"`
		Executable string `json:"executable"`
	} `json:"process"`
	StartNS      int64 `json:"start_ns"`
	ReadyNS      int64 `json:"ready_ns"`
	DurationNS   int64 `json:"duration_ns"`
	MediaEnabled bool  `json:"media_enabled"`
}

type collaborationPerformanceResult struct {
	OK              bool                            `json:"ok"`
	Case            string                          `json:"case"`
	Scenario        string                          `json:"scenario"`
	Pig             string                          `json:"pig"`
	PigSHA256       string                          `json:"pigSha256"`
	Extension       string                          `json:"extension"`
	ExtensionSHA256 string                          `json:"extensionSha256"`
	VerifierSHA256  string                          `json:"verifierSha256"`
	IdleObserver    string                          `json:"idleObserver"`
	ObserverSHA256  string                          `json:"idleObserverSha256"`
	Startup         collaborationPerformanceStartup `json:"startup"`
	Cleanup         struct {
		ProviderJoined          bool   `json:"providerJoined"`
		ObservedProcessesExited bool   `json:"observedProcessesExited"`
		PaneJoined              bool   `json:"paneJoined"`
		TmuxSocketClosed        bool   `json:"tmuxSocketClosed"`
		TmuxServerAlive         bool   `json:"tmuxServerAlive"`
		TmuxFailureCount        int    `json:"tmuxFailureCount"`
		ScratchRetained         string `json:"scratchRetainedAt"`
	} `json:"cleanup"`
}

type collaborationPerformanceStats struct {
	Count int   `json:"count"`
	P50NS int64 `json:"p50_ns"`
	P95NS int64 `json:"p95_ns"`
}

type collaborationPerformanceFailure struct{ message string }

func (failure *collaborationPerformanceFailure) Error() string { return failure.message }

func collaborationPercentile(values []int64, fraction float64) int64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	return ordered[int(math.Ceil(fraction*float64(len(ordered))))-1]
}

func collaborationPerformanceStatsFor(values []int64) collaborationPerformanceStats {
	return collaborationPerformanceStats{Count: len(values), P50NS: collaborationPercentile(values, 0.50), P95NS: collaborationPercentile(values, 0.95)}
}

func collaborationPerformanceRegression(base, head []int64) error {
	if len(base) == 0 || len(base) != len(head) {
		return errors.New("startup comparison requires paired samples")
	}
	for _, fraction := range []float64{0.50, 0.95} {
		before, after := collaborationPercentile(base, fraction), collaborationPercentile(head, fraction)
		if after-before > 50_000_000 && float64(after) > float64(before)*1.10 {
			return &collaborationPerformanceFailure{message: fmt.Sprintf("startup p%.0f exceeds both the 10 percent and 50 ms limits", fraction*100)}
		}
	}
	return nil
}

func (runner *collaborationCLIRunner) performance(ctx context.Context, baseline, candidate collaborationCLITarget) (resultErr error) {
	samplesPath := filepath.Join(runner.directory, "samples.jsonl")
	samplesFile, err := os.OpenFile(samplesPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	var samples []collaborationPerformanceSample
	defer func() {
		resultErr = errors.Join(resultErr, samplesFile.Close())
		summary := make(map[string]collaborationPerformanceStats)
		driverSummary := make(map[string]collaborationPerformanceStats)
		for _, label := range []string{"baseline", "candidate"} {
			for _, capture := range []bool{false, true} {
				var values, driverValues []int64
				for _, sample := range samples {
					if sample.Target == label && sample.Capture == capture && !sample.Warmup && sample.Error == "" {
						values = append(values, sample.DurationNS)
						driverValues = append(driverValues, sample.DriverDurationNS)
					}
				}
				summary[fmt.Sprintf("%s_capture_%t", label, capture)] = collaborationPerformanceStatsFor(values)
				driverSummary[fmt.Sprintf("%s_capture_%t", label, capture)] = collaborationPerformanceStatsFor(driverValues)
			}
		}
		overhead := make(map[string]map[string]int64)
		driverOverhead := make(map[string]map[string]int64)
		for _, label := range []string{"baseline", "candidate"} {
			without, with := summary[label+"_capture_false"], summary[label+"_capture_true"]
			if without.Count > 0 && with.Count > 0 {
				overhead[label] = map[string]int64{"p50_ns": with.P50NS - without.P50NS, "p95_ns": with.P95NS - without.P95NS}
				driverWithout, driverWith := driverSummary[label+"_capture_false"], driverSummary[label+"_capture_true"]
				driverOverhead[label] = map[string]int64{"p50_ns": driverWith.P50NS - driverWithout.P50NS, "p95_ns": driverWith.P95NS - driverWithout.P95NS}
			}
		}
		report := map[string]any{
			"startup": summary, "driver_duration": driverSummary, "startup_capture_overhead": overhead, "driver_capture_overhead": driverOverhead,
			"event_serialization": collaborationPerformanceStatsFor(runner.eventDurations),
			"serialization_scope": "JSON encoding and write of each event, without fsync",
		}
		resultErr = errors.Join(resultErr, collaborationCLIWriteNew(filepath.Join(runner.directory, "performance-summary.json"), report))
	}()
	binaries, verifierHash, observerHash, err := runner.buildTerminal(ctx, candidate)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(samplesFile)
	for pair := 0; pair < runner.options.performanceWarmups+runner.options.performancePairs; pair++ {
		warmup := pair < runner.options.performanceWarmups
		for _, capture := range []bool{false, true} {
			targets := []struct {
				label string
				value collaborationCLITarget
			}{{"baseline", baseline}, {"candidate", candidate}}
			if pair%2 == 1 {
				targets[0], targets[1] = targets[1], targets[0]
			}
			for _, target := range targets {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				step := fmt.Sprintf("sample-%02d-%s-capture-%t", pair, target.label, capture)
				sample := collaborationPerformanceSample{Target: target.label, ProductCommit: target.value.Identity.ProductCommit, Pair: pair, Warmup: warmup, Capture: capture}
				stepErr := runner.performanceSample(ctx, step, target.value, binaries, verifierHash, observerHash, &sample)
				if stepErr != nil {
					sample.Error = stepErr.Error()
				}
				samples = append(samples, sample)
				if err := encoder.Encode(sample); err != nil {
					return err
				}
				if stepErr != nil {
					return fmt.Errorf("%s: %w", step, stepErr)
				}
			}
		}
	}
	if runner.options.performanceWarmups != 5 || runner.options.performancePairs != 20 {
		return errors.New("partial performance samples retained; five warmups and 20 measured pairs per capture mode are required")
	}
	var base, head []int64
	for _, sample := range samples {
		if !sample.Warmup && !sample.Capture {
			if sample.Target == "baseline" {
				base = append(base, sample.DurationNS)
			} else {
				head = append(head, sample.DurationNS)
			}
		}
	}
	if err := collaborationPerformanceRegression(base, head); err != nil {
		return err
	}
	if collaborationPercentile(runner.eventDurations, 0.95) >= 50_000_000 {
		return &collaborationPerformanceFailure{message: "event serialization p95 is at least 50 ms"}
	}
	return nil
}

func (runner *collaborationCLIRunner) buildTerminal(ctx context.Context, candidate collaborationCLITarget) (map[string]string, string, string, error) {
	if candidate.Identity.ProductCommit != candidate.Identity.DriverCommit {
		return nil, "", "", errors.New("terminal driver must be built from the candidate commit")
	}
	devenv, err := exec.LookPath("devenv")
	if err != nil {
		return nil, "", "", err
	}
	root := filepath.Join(runner.directory, "terminal-build")
	binaries := map[string]string{"verify-tui": filepath.Join(root, "verify-tui"), "tui-idle-observer": filepath.Join(root, "tui-idle-observer")}
	script := "set -eu\ngo -C scripts build -p 2 -mod=readonly -buildvcs=false -o \"$1\" ./cmd/verify-tui\ngo -C scripts build -p 2 -mod=readonly -buildvcs=false -o \"$2\" ./cmd/tui-idle-observer"
	command := []string{devenv, "shell", "--", "bash", "-c", script, "collaboration-terminal-build", binaries["verify-tui"], binaries["tui-idle-observer"]}
	if _, err := runner.executeAt(ctx, "terminal-build", command, candidate.Checkout); err != nil {
		return nil, "", "", fmt.Errorf("build terminal driver: %w", err)
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return nil, "", "", err
	}
	statusDir, err := runner.executeAt(ctx, "terminal-build-status", []string{git, "status", "--porcelain", "--untracked-files=all"}, candidate.Checkout)
	if err != nil {
		return nil, "", "", err
	}
	status, err := os.ReadFile(filepath.Join(statusDir, "stdout.log"))
	if err != nil || len(status) != 0 {
		return nil, "", "", errors.New("candidate checkout changed while building the terminal driver")
	}
	verifierHash, err := collaborationCLIDigest(binaries["verify-tui"])
	if err != nil {
		return nil, "", "", err
	}
	observerHash, err := collaborationCLIDigest(binaries["tui-idle-observer"])
	if err != nil {
		return nil, "", "", err
	}
	identities := map[string]string{"driver_commit": candidate.Identity.DriverCommit, "verify_tui": binaries["verify-tui"], "verify_tui_sha256": verifierHash, "idle_observer": binaries["tui-idle-observer"], "idle_observer_sha256": observerHash}
	if err := collaborationCLIWriteNew(filepath.Join(runner.directory, "terminal-build-identities.json"), identities); err != nil {
		return nil, "", "", err
	}
	return binaries, verifierHash, observerHash, nil
}

func (runner *collaborationCLIRunner) performanceSample(ctx context.Context, name string, target collaborationCLITarget, binaries map[string]string, verifierHash, observerHash string, sample *collaborationPerformanceSample) error {
	root := filepath.Join(runner.directory, name, "evidence")
	command := runner.performanceCommand(target, binaries, root, sample.Capture)
	directory, err := runner.execute(ctx, name, command)
	if len(runner.drivers) > 0 && runner.drivers[len(runner.drivers)-1].Name == name {
		driver := runner.drivers[len(runner.drivers)-1]
		sample.DriverDurationNS = driver.FinishedNS - driver.StartedNS
	}
	if directory != "" {
		for _, item := range []struct {
			name   string
			digest *string
		}{{"stdout.log", &sample.StdoutSHA256}, {"stderr.log", &sample.StderrSHA256}} {
			path := filepath.Join(directory, item.name)
			if info, statErr := os.Stat(path); statErr == nil && info.Size() > 0 {
				digest, digestErr := collaborationCLIDigest(path)
				*item.digest = digest
				err = errors.Join(err, digestErr)
			}
		}
	}
	if err != nil {
		return err
	}
	var summary struct {
		OK       bool   `json:"ok"`
		Case     string `json:"case"`
		Scenario string `json:"scenario"`
		Evidence string `json:"evidence"`
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout.log"))
	if err != nil || json.Unmarshal(data, &summary) != nil || !summary.OK || summary.Case != "direct" || summary.Scenario != "receipt-control" || !collaborationCLIWithin(root, summary.Evidence) {
		return errors.New("performance driver did not return the selected direct receipt")
	}
	sample.Evidence = summary.Evidence
	sample.Result, err = os.ReadFile(filepath.Join(summary.Evidence, "result.json"))
	if err != nil {
		return err
	}
	var result collaborationPerformanceResult
	if json.Unmarshal(sample.Result, &result) != nil || !result.OK || result.Case != "direct" || result.Scenario != "receipt-control" || result.Pig != target.Pig || result.PigSHA256 != target.Identity.HostBinarySHA256 || result.Extension != target.Extension || result.ExtensionSHA256 != target.Identity.ProductBinarySHA256 || result.VerifierSHA256 != verifierHash || result.IdleObserver != binaries["tui-idle-observer"] || result.ObserverSHA256 != observerHash {
		return errors.New("performance result does not identify the selected binaries and driver")
	}
	startup := result.Startup
	if startup.Clock != "CLOCK_MONOTONIC" || startup.Process.PID <= 0 || startup.Process.StartTicks == "" || startup.StartNS <= 0 || startup.ReadyNS < startup.StartNS || startup.DurationNS != startup.ReadyNS-startup.StartNS || startup.MediaEnabled != sample.Capture {
		return errors.New("performance result lacks an attributable monotonic startup interval")
	}
	selectedHost, selectedErr := os.Stat(target.Pig)
	observedHost, observedErr := os.Stat(startup.Process.Executable)
	if selectedErr != nil || observedErr != nil || !os.SameFile(selectedHost, observedHost) {
		return errors.New("timed process executable differs from the selected PiG host")
	}
	cleanup := result.Cleanup
	if !cleanup.ProviderJoined || !cleanup.ObservedProcessesExited || !cleanup.PaneJoined || !cleanup.TmuxSocketClosed || cleanup.TmuxServerAlive || cleanup.TmuxFailureCount != 0 || cleanup.ScratchRetained != "" {
		return errors.New("performance driver did not confirm owned process and scratch cleanup")
	}
	if sample.Capture {
		sample.MediaReceipt, err = os.ReadFile(filepath.Join(summary.Evidence, "media", "receipt.json"))
		if err != nil {
			return err
		}
		var receipt collaborationCLIMediaReceipt
		if json.Unmarshal(sample.MediaReceipt, &receipt) != nil || receipt.RecorderReadyNS <= 0 || receipt.RecorderReadyNS > startup.StartNS {
			return errors.New("recorder was not observed ready before PiG startup")
		}
		if err := runner.performanceMedia(ctx, summary.Evidence, sample.MediaReceipt); err != nil {
			return err
		}
	}
	sample.DurationNS = startup.DurationNS
	runner.drivers[len(runner.drivers)-1].Evidence = summary.Evidence
	return collaborationCLICheckTarget(target)
}

func (runner *collaborationCLIRunner) performanceCommand(target collaborationCLITarget, binaries map[string]string, root string, capture bool) []string {
	command := []string{binaries["verify-tui"], "--scenario", "receipt-control", "--case", "direct", "--pig", target.Pig, "--extension", target.Extension, "--idle-observer", binaries["tui-idle-observer"], "--piglet", runner.options.piglet, "--evidence-root", root}
	if capture {
		command = append(command, "--media", "--media-ffmpeg", runner.options.ffmpeg, "--media-ffprobe", runner.options.ffprobe, "--media-xvfb", runner.options.xvfb, "--media-xterm", runner.options.xterm)
	}
	return command
}

func (runner *collaborationCLIRunner) performanceMedia(ctx context.Context, evidence string, data []byte) error {
	var receipt collaborationCLIMediaReceipt
	if json.Unmarshal(data, &receipt) != nil || receipt.Status != "complete" || receipt.Screenshot.Origin != "video_frame" || receipt.Screenshot.SourceVideo != receipt.Video.SHA256 || receipt.Screenshot.SourceMillis == nil {
		return errors.New("performance capture receipt is incomplete")
	}
	video, err := runner.artifact(filepath.Join(evidence, "media", "terminal.mp4"), "video")
	if err != nil || video.SHA256 != receipt.Video.SHA256 {
		return errors.New("performance video does not match its receipt")
	}
	video.Origin = "live_terminal"
	screenshot, err := runner.artifact(filepath.Join(evidence, "media", "screenshot.png"), "screenshot")
	if err != nil || screenshot.SHA256 != receipt.Screenshot.SHA256 {
		return errors.New("performance screenshot does not match its receipt")
	}
	screenshot.Origin, screenshot.SourceVideo, screenshot.SourceTimestamp = "video_frame", receipt.Screenshot.SourceVideo, receipt.Screenshot.SourceMillis
	root, err := os.OpenRoot(runner.directory)
	if err != nil {
		return err
	}
	_, _, validationErr := collaborationValidateArtifacts(ctx, root, []collaborationArtifact{video, screenshot}, collaborationMediaTools{FFmpeg: runner.options.ffmpeg, FFprobe: runner.options.ffprobe})
	return errors.Join(validationErr, root.Close())
}
