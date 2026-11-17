package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type collaborationCLIMediaReceipt struct {
	Status          string `json:"status"`
	ProcessesExited bool   `json:"processes_exited"`
	RecorderReadyNS int64  `json:"recorder_ready_monotonic_ns"`
	Video           struct {
		SHA256 string `json:"sha256"`
	} `json:"video"`
	Screenshot struct {
		SHA256       string `json:"sha256"`
		Origin       string `json:"origin"`
		SourceVideo  string `json:"source_video_sha256"`
		SourceMillis *int64 `json:"source_timestamp_ms"`
	} `json:"screenshot"`
}

func (runner *collaborationCLIRunner) terminal(ctx context.Context, name string, target collaborationCLITarget, caseName, scenario string) error {
	pigletDigest := ""
	if caseName == "piglet" {
		var err error
		pigletDigest, err = collaborationCLIDigest(runner.options.piglet)
		if err != nil {
			return fmt.Errorf("hash Piglet manifest: %w", err)
		}
	}
	root := filepath.Join(runner.directory, name, "evidence")
	command := []string{
		filepath.Join(runner.options.repo, "scripts", "verify-terminal-ui.sh"),
		"--scenario", scenario, "--case", caseName, "--evidence-root", root,
		"--pig", target.Pig, "--extension", target.Extension, "--piglet", runner.options.piglet,
		"--media", "--media-ffmpeg", runner.options.ffmpeg, "--media-ffprobe", runner.options.ffprobe,
		"--media-xvfb", runner.options.xvfb, "--media-xterm", runner.options.xterm,
	}
	if caseName == "source" {
		command = append(command, "--source", filepath.Join(target.Checkout, "extensions", "pig-litter"))
	}
	directory, err := runner.execute(ctx, name, command)
	if err != nil {
		return err
	}
	var summary struct {
		OK       bool   `json:"ok"`
		Case     string `json:"case"`
		Scenario string `json:"scenario"`
		Evidence string `json:"evidence"`
	}
	data, readErr := os.ReadFile(filepath.Join(directory, "stdout.log"))
	if readErr != nil || json.Unmarshal(data, &summary) != nil || !summary.OK || summary.Case != caseName || summary.Scenario != scenario || !collaborationCLIWithin(root, summary.Evidence) {
		return errors.New("terminal driver did not return an attributable successful result")
	}
	var result struct {
		OK                 bool            `json:"ok"`
		Pig                string          `json:"pig"`
		PigSHA256          string          `json:"pigSha256"`
		Extension          string          `json:"extension"`
		ExtensionSHA256    string          `json:"extensionSha256"`
		VerifierSHA256     string          `json:"verifierSha256"`
		Case               string          `json:"case"`
		PigletSource       string          `json:"pigletSource"`
		PigletSourceSHA256 string          `json:"pigletSourceSha256"`
		SourceSelection    json.RawMessage `json:"sourceSelection"`
	}
	data, err = os.ReadFile(filepath.Join(summary.Evidence, "result.json"))
	if err != nil || json.Unmarshal(data, &result) != nil || !result.OK || result.Case != caseName || result.Pig != target.Pig || result.Extension != target.Extension || result.PigSHA256 != target.Identity.HostBinarySHA256 || result.ExtensionSHA256 != target.Identity.ProductBinarySHA256 || !collaborationSHA256(result.VerifierSHA256) {
		return errors.New("terminal result does not identify the selected host, extension, and verifier")
	}
	if caseName == "piglet" {
		digest, err := collaborationCLIDigest(runner.options.piglet)
		if err != nil || result.PigletSource != runner.options.piglet || result.PigletSourceSHA256 != pigletDigest || digest != pigletDigest {
			return errors.New("terminal result does not identify the selected Piglet manifest")
		}
	}
	if caseName == "source" {
		if err := runner.validateSourceSelection(target, summary.Evidence, result.SourceSelection); err != nil {
			return err
		}
	}
	var media collaborationCLIMediaReceipt
	data, err = os.ReadFile(filepath.Join(summary.Evidence, "media", "receipt.json"))
	if err != nil || json.Unmarshal(data, &media) != nil || media.Status != "complete" || media.Screenshot.Origin != "video_frame" || media.Screenshot.SourceVideo != media.Video.SHA256 || media.Screenshot.SourceMillis == nil {
		return errors.New("terminal media receipt is incomplete")
	}
	frames, err := filepath.Glob(filepath.Join(summary.Evidence, "*-ansi.txt"))
	if err != nil || len(frames) == 0 {
		return errors.New("terminal driver retained no ANSI frame")
	}
	var artifacts []collaborationArtifact
	for _, item := range []struct{ path, kind string }{
		{filepath.Join(summary.Evidence, "pty.raw"), "pty"},
		{frames[0], "ansi"},
		{filepath.Join(summary.Evidence, "result.json"), "log"},
		{filepath.Join(summary.Evidence, "media", "receipt.json"), "log"},
		{filepath.Join(summary.Evidence, "media", "terminal.mp4"), "video"},
		{filepath.Join(summary.Evidence, "media", "screenshot.png"), "screenshot"},
	} {
		artifact, err := runner.artifact(item.path, item.kind)
		if err != nil {
			return err
		}
		if item.kind == "video" {
			artifact.Origin = "live_terminal"
			if artifact.SHA256 != media.Video.SHA256 {
				return errors.New("terminal video differs from its capture receipt")
			}
		}
		if item.kind == "screenshot" {
			artifact.Origin = media.Screenshot.Origin
			artifact.SourceVideo = media.Screenshot.SourceVideo
			artifact.SourceTimestamp = media.Screenshot.SourceMillis
			if artifact.SHA256 != media.Screenshot.SHA256 {
				return errors.New("terminal screenshot differs from its capture receipt")
			}
		}
		artifacts = append(artifacts, artifact)
	}
	if caseName == "source" {
		artifact, err := runner.artifact(filepath.Join(summary.Evidence, "source-selection.json"), "log")
		if err != nil {
			return err
		}
		artifacts = append(artifacts, artifact)
	}
	attemptRoot, err := os.OpenRoot(runner.directory)
	if err != nil {
		return err
	}
	_, _, validationErr := collaborationValidateArtifacts(ctx, attemptRoot, artifacts, collaborationMediaTools{FFmpeg: runner.options.ffmpeg, FFprobe: runner.options.ffprobe})
	closeErr := attemptRoot.Close()
	if validationErr != nil || closeErr != nil {
		return fmt.Errorf("terminal media validation: %w", errors.Join(validationErr, closeErr))
	}
	runner.artifacts = append(runner.artifacts, artifacts...)
	runner.drivers[len(runner.drivers)-1].Evidence = summary.Evidence
	return collaborationCLICheckTarget(target)
}

func (runner *collaborationCLIRunner) probe(ctx context.Context, self string, target collaborationCLITarget) error {
	name := "host-probe"
	root := filepath.Join(runner.directory, name)
	var command []string
	switch runner.options.scenario {
	case "host-tool-veto":
		command = []string{self, "collaboration", "--hook-child", root}
	case "host-busy-message":
		command = []string{self, "collaboration", "--probe-child", "busy", root}
	case "host-idle-message":
		command = []string{self, "collaboration", "--probe-child", "idle", root}
	case "host-crash-after-acceptance":
		command = []string{self, "collaboration", "--probe-child", "after-accept-before-append", root}
	case "host-crash-after-append":
		command = []string{self, "collaboration", "--probe-child", "after-append-before-return", root}
	case "host-crash-after-acknowledgement", "host-same-id-replay":
		command = []string{self, "collaboration", "--probe-child", "after-return", root}
	case "host-independent-process":
		command = []string{self, "collaboration", "--probe-child", "independent", root, target.Pig, target.Extension}
	default:
		return errors.New("research scenario has no production probe")
	}
	_, err := runner.execute(ctx, name, command)
	if err != nil {
		return fmt.Errorf("host probe is provisional and incomplete: %w", err)
	}
	if err := collaborationCLICheckTarget(target); err != nil {
		return err
	}
	if runner.options.scenario == "host-independent-process" {
		return runner.independentTerminal(ctx, target, root)
	}
	if runner.options.scenario == "host-crash-after-acknowledgement" || runner.options.scenario == "host-crash-after-append" {
		return errors.New("the send-return cut was observed, but the native acknowledgement boundary and attributable live media remain unverified")
	}
	return errors.New("host observations are provisional; this lane has no attributable live terminal capture")
}

func (runner *collaborationCLIRunner) artifact(path, kind string) (collaborationArtifact, error) {
	relative, err := filepath.Rel(runner.directory, path)
	if err != nil || !filepath.IsLocal(relative) || relative == "." {
		return collaborationArtifact{}, fmt.Errorf("artifact path escapes attempt: %s", path)
	}
	root, err := os.OpenRoot(runner.directory)
	if err != nil {
		return collaborationArtifact{}, err
	}
	file, err := collaborationOpenFile(root, relative)
	if err != nil {
		root.Close()
		return collaborationArtifact{}, err
	}
	digest, _, err := collaborationFileDigest(file, collaborationMaxArtifactBytes)
	closeErr := errors.Join(file.Close(), root.Close())
	if err != nil || closeErr != nil {
		return collaborationArtifact{}, errors.Join(err, closeErr)
	}
	return collaborationArtifact{Path: relative, SHA256: digest, Kind: kind}, nil
}

func (runner *collaborationCLIRunner) finish(emit bool) error {
	if err := collaborationCLIWriteNew(filepath.Join(runner.directory, "drivers.json"), runner.drivers); err != nil {
		return err
	}
	if err := collaborationCLIWriteNew(filepath.Join(runner.directory, "timing.json"), runner.attempt.Events); err != nil {
		return err
	}
	if err := collaborationCLIWriteNew(filepath.Join(runner.directory, "event-serialization.json"), runner.eventDurations); err != nil {
		return err
	}
	for _, item := range []struct{ path, kind string }{
		{filepath.Join(runner.directory, "drivers.json"), "log"},
		{filepath.Join(runner.directory, "driver-identity.json"), "log"},
		{filepath.Join(runner.directory, "timing.json"), "timing"},
		{filepath.Join(runner.directory, "events.jsonl"), "timing"},
		{filepath.Join(runner.directory, "event-serialization.json"), "timing"},
	} {
		artifact, err := runner.artifact(item.path, item.kind)
		if err != nil {
			return err
		}
		runner.artifacts = append(runner.artifacts, artifact)
	}
	for _, driver := range runner.drivers {
		if runner.attempt.Scenario == "capture-performance" && strings.HasPrefix(driver.Name, "sample-") {
			continue
		}
		if runner.attempt.Scenario == "fixture-interrupt" && strings.HasPrefix(driver.Name, "interrupted-attempt/") {
			continue
		}
		for _, log := range []string{"stdout.log", "stderr.log", "result.json"} {
			path := filepath.Join(runner.directory, driver.Name, log)
			if info, err := os.Stat(path); err == nil && info.Size() > 0 {
				artifact, err := runner.artifact(path, "log")
				if err != nil {
					return err
				}
				runner.artifacts = append(runner.artifacts, artifact)
			}
		}
	}
	if path := filepath.Join(runner.directory, "terminal-build-identities.json"); runner.attempt.Scenario == "capture-performance" || runner.attempt.Scenario == "fixture-isolation" || runner.attempt.Scenario == "fixture-interrupt" {
		if _, err := os.Stat(path); err == nil {
			artifact, err := runner.artifact(path, "log")
			if err != nil {
				return err
			}
			runner.artifacts = append(runner.artifacts, artifact)
		}
	}
	if path := filepath.Join(runner.directory, "targets.json"); runner.attempt.Scenario == "baseline" || runner.attempt.Scenario == "capture-performance" {
		if _, err := os.Stat(path); err == nil {
			artifact, err := runner.artifact(path, "log")
			if err != nil {
				return err
			}
			runner.artifacts = append(runner.artifacts, artifact)
		}
	}
	if runner.attempt.Scenario == "capture-performance" {
		for _, item := range []struct{ name, kind string }{{"samples.jsonl", "samples"}, {"performance-summary.json", "timing"}} {
			path := filepath.Join(runner.directory, item.name)
			if info, err := os.Stat(path); err == nil && info.Size() > 0 {
				artifact, err := runner.artifact(path, item.kind)
				if err != nil {
					return err
				}
				runner.artifacts = append(runner.artifacts, artifact)
			}
		}
	}
	if runner.attempt.Scenario == "fixture-isolation" {
		path := filepath.Join(runner.directory, "fixture-samples.json")
		if _, err := os.Stat(path); err == nil {
			artifact, err := runner.artifact(path, "log")
			if err != nil {
				return err
			}
			runner.artifacts = append(runner.artifacts, artifact)
		}
	}
	result := map[string]any{"passed": runner.attempt.Outcome == "passed", "outcome": runner.attempt.Outcome, "reason": runner.attempt.Reason}
	resultPath := filepath.Join(runner.directory, "result.json")
	if err := collaborationCLIWriteNew(resultPath, result); err != nil {
		return err
	}
	artifact, err := runner.artifact(resultPath, "result")
	if err != nil {
		return err
	}
	runner.artifacts = append(runner.artifacts, artifact)
	runner.attempt.Artifacts = runner.artifacts
	manifest := filepath.Join(runner.directory, "manifest.json")
	if err := collaborationCLIWriteNew(manifest, runner.attempt); err != nil {
		return err
	}
	digest, err := collaborationCLIDigest(manifest)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(runner.options.root, manifest)
	if err != nil || !filepath.IsLocal(relative) {
		return errors.New("manifest is outside the evidence root")
	}
	selected := collaborationSelectedRecord{Path: relative, SHA256: digest}
	selectionPath := filepath.Join(runner.directory, "selection.json")
	if err := collaborationCLIWriteNew(selectionPath, collaborationSelection{Version: 1, Target: runner.attempt.Target, Attempts: []collaborationSelectedRecord{selected}}); err != nil {
		return err
	}
	if !emit {
		return nil
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"manifest": manifest, "selection": selectionPath, "selection_record": selected,
		"outcome": runner.attempt.Outcome, "reason": runner.attempt.Reason,
	})
}
