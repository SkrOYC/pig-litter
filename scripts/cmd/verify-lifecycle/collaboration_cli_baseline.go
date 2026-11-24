package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"
)

type collaborationBaselineObservation struct {
	Tools        []string `json:"tools"`
	Children     []string `json:"children"`
	Completed    string   `json:"completed_child"`
	WaitState    string   `json:"wait_state"`
	ReportSHA256 string   `json:"normalized_report_sha256"`
}

type collaborationBaselineSample struct {
	Sample      collaborationPerformanceSample   `json:"sample"`
	Observation collaborationBaselineObservation `json:"observation"`
}

var collaborationBaselinePrivateMarker = regexp.MustCompile(`PRIVATE_ASSISTANT_[A-Z0-9]+`)

func (runner *collaborationCLIRunner) baselineTerminal(ctx context.Context, baseline, candidate collaborationCLITarget) (resultErr error) {
	samples := []collaborationBaselineSample{}
	defer func() {
		resultErr = errors.Join(resultErr, collaborationCLIWriteNew(filepath.Join(runner.directory, "baseline-samples.json"), samples))
	}()
	binaries, verifierHash, observerHash, err := runner.buildTerminal(ctx, candidate)
	if err != nil {
		return err
	}
	for _, item := range []struct {
		name    string
		target  collaborationCLITarget
		capture bool
	}{
		{"baseline-off", baseline, false}, {"candidate-off", candidate, false},
		{"candidate-on", candidate, true}, {"baseline-on", baseline, true},
	} {
		sample := collaborationPerformanceSample{Target: item.name, ProductCommit: item.target.Identity.ProductCommit, Capture: item.capture}
		if err := runner.performanceSample(ctx, item.name, item.target, binaries, verifierHash, observerHash, &sample); err != nil {
			sample.Error = err.Error()
			samples = append(samples, collaborationBaselineSample{Sample: sample})
			return fmt.Errorf("%s: %w", item.name, err)
		}
		samples = append(samples, collaborationBaselineSample{Sample: sample})
		index := len(samples) - 1
		if item.capture {
			if err := runner.collectFixtureCapture(ctx, sample.Evidence, false); err != nil {
				samples[index].Sample.Error = err.Error()
				return fmt.Errorf("%s capture: %w", item.name, err)
			}
		} else {
			if err := runner.collectBaselineTerminal(sample.Evidence); err != nil {
				samples[index].Sample.Error = err.Error()
				return fmt.Errorf("%s terminal: %w", item.name, err)
			}
		}
		observation, err := runner.baselineObservation(sample.Evidence, sample.Result)
		samples[index].Observation = observation
		if err != nil {
			samples[index].Sample.Error = err.Error()
			return fmt.Errorf("%s result: %w", item.name, err)
		}
	}
	return collaborationCompareBaselineSamples(samples)
}

func collaborationCompareBaselineSamples(samples []collaborationBaselineSample) error {
	want := map[string]bool{"baseline-off": false, "candidate-off": false, "baseline-on": true, "candidate-on": true}
	if len(samples) != len(want) {
		return errors.New("baseline requires both targets with capture off and on")
	}
	seen := make(map[string]bool, len(samples))
	for _, sample := range samples {
		capture, exists := want[sample.Sample.Target]
		if !exists || seen[sample.Sample.Target] || sample.Sample.Capture != capture || sample.Sample.Error != "" || sample.Observation.ReportSHA256 == "" {
			return errors.New("baseline has a missing, duplicate, or incomplete sample")
		}
		seen[sample.Sample.Target] = true
		if !reflect.DeepEqual(samples[0].Observation, sample.Observation) {
			return errors.New("baseline and candidate spawn/wait results differ with or without capture")
		}
	}
	return nil
}

func (runner *collaborationCLIRunner) collectBaselineTerminal(evidence string) error {
	frames, err := filepath.Glob(filepath.Join(evidence, "*-ansi.txt"))
	if err != nil || len(frames) == 0 {
		return errors.New("baseline terminal retained no ANSI frame")
	}
	for _, item := range []struct{ path, kind string }{
		{filepath.Join(evidence, "pty.raw"), "pty"}, {frames[0], "ansi"}, {filepath.Join(evidence, "result.json"), "log"},
	} {
		artifact, err := runner.artifact(item.path, item.kind)
		if err != nil {
			return err
		}
		runner.artifacts = append(runner.artifacts, artifact)
	}
	return nil
}

func (runner *collaborationCLIRunner) baselineObservation(evidence string, data []byte) (collaborationBaselineObservation, error) {
	var result collaborationCLIFixtureResult
	if err := json.Unmarshal(data, &result); err != nil || !result.OK || len(result.Fixture.RootSessions) != 1 || result.QuickChild.Name != "quick-newer" || !filepath.IsLocal(result.QuickChild.ID) || filepath.Base(result.QuickChild.ID) != result.QuickChild.ID {
		return collaborationBaselineObservation{}, errors.New("baseline result lacks the completed child and native root session")
	}
	observation := collaborationBaselineObservation{Tools: append([]string(nil), result.SelectedToolNames...), Completed: result.QuickChild.Name, WaitState: "completed"}
	seen := map[string]bool{result.QuickChild.ID: true}
	for _, child := range result.ChildRefs {
		if child.ID == "" || child.Name == "" || seen[child.ID] {
			return observation, errors.New("baseline child identities are missing or duplicated")
		}
		seen[child.ID] = true
		observation.Children = append(observation.Children, child.Name)
	}
	sort.Strings(observation.Tools)
	sort.Strings(observation.Children)
	if !reflect.DeepEqual(observation.Children, []string{"held-a", "held-b"}) || !collaborationContains(observation.Tools, "litter_spawn") || !collaborationContains(observation.Tools, "litter_wait") {
		return observation, errors.New("baseline did not expose its expected spawn and wait tools and children")
	}
	root := result.Fixture.RootSessions[0]
	if !filepath.IsLocal(root.ID) || filepath.Base(root.ID) != root.ID || seen[root.ID] || !collaborationSHA256(root.SHA256) {
		return observation, errors.New("baseline root session identity is invalid")
	}
	rootData, digest, err := runner.baselineNative(filepath.Join(evidence, "native-final", root.ID+".jsonl"))
	if err != nil || digest != root.SHA256 {
		return observation, errors.New("baseline native root session differs from its receipt")
	}
	childData, _, err := runner.baselineNative(filepath.Join(evidence, "native-final", result.QuickChild.ID+".jsonl"))
	if err != nil {
		return observation, err
	}
	report, err := collaborationBaselineWaitReport(rootData, root.ID, result.QuickChild.ID)
	if err != nil {
		return observation, err
	}
	if err := collaborationBaselineChildReport(childData, result.QuickChild.ID, report); err != nil {
		return observation, err
	}
	marker := collaborationBaselinePrivateMarker.FindString(report)
	if marker == "" || !strings.HasPrefix(report, "QUICK_REPORT "+marker) {
		return observation, errors.New("baseline wait result lacks its expected private fixture report")
	}
	observation.ReportSHA256 = collaborationTestDataDigest([]byte(strings.ReplaceAll(report, marker, "PRIVATE_ASSISTANT_TOKEN")))
	return observation, nil
}

func (runner *collaborationCLIRunner) baselineNative(path string) ([]byte, string, error) {
	artifact, err := runner.artifact(path, "log")
	if err != nil {
		return nil, "", err
	}
	root, err := os.OpenRoot(runner.directory)
	if err != nil {
		return nil, "", err
	}
	file, err := collaborationOpenFile(root, artifact.Path)
	if err != nil {
		_ = root.Close()
		return nil, "", err
	}
	data, readErr := collaborationReadArtifact(file, collaborationMaxJSONBytes, artifact.SHA256)
	closeErr := errors.Join(file.Close(), root.Close())
	if err := errors.Join(readErr, closeErr); err != nil {
		return nil, "", err
	}
	runner.artifacts = append(runner.artifacts, artifact)
	return data, artifact.SHA256, nil
}

type collaborationBaselineNativeEntry struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Message struct {
		Role     string          `json:"role"`
		ToolName string          `json:"toolName"`
		IsError  bool            `json:"isError"`
		Content  json.RawMessage `json:"content"`
	} `json:"message"`
}

func collaborationBaselineText(content json.RawMessage) ([]string, error) {
	var text string
	if json.Unmarshal(content, &text) == nil {
		return []string{text}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &parts); err != nil {
		return nil, err
	}
	var result []string
	for _, part := range parts {
		if part.Type == "text" {
			result = append(result, part.Text)
		}
	}
	return result, nil
}

func collaborationBaselineEntries(data []byte, sessionID string) ([]collaborationBaselineNativeEntry, error) {
	var entries []collaborationBaselineNativeEntry
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), collaborationMaxJSONBytes)
	for scanner.Scan() {
		var entry collaborationBaselineNativeEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(entries) == 0 || entries[0].Type != "session" || entries[0].ID != sessionID {
		return nil, errors.New("native session header does not match its recorded ID")
	}
	return entries, nil
}

func collaborationBaselineWaitReport(data []byte, sessionID, childID string) (string, error) {
	entries, err := collaborationBaselineEntries(data, sessionID)
	if err != nil {
		return "", err
	}
	var report string
	for _, entry := range entries {
		if entry.Type != "message" || entry.Message.Role != "toolResult" || entry.Message.ToolName != "litter_wait" || entry.Message.IsError {
			continue
		}
		parts, err := collaborationBaselineText(entry.Message.Content)
		if err != nil {
			return "", err
		}
		for _, part := range parts {
			var result struct {
				Operation         string `json:"operation"`
				State             string `json:"state"`
				HandbackTruncated bool   `json:"handbackTruncated"`
				Outcome           struct {
					Run struct {
						ID string `json:"id"`
					} `json:"run"`
					State            string `json:"state"`
					Text             string `json:"text"`
					Truncated        bool   `json:"truncated"`
					ReportProvenance string `json:"reportProvenance"`
				} `json:"outcome"`
			}
			if json.Unmarshal([]byte(part), &result) != nil || result.Outcome.Run.ID != childID {
				continue
			}
			if report != "" || result.Operation != "litter_wait" || result.State != "completed" || result.HandbackTruncated || result.Outcome.State != "completed" || result.Outcome.Truncated || result.Outcome.ReportProvenance != "child_assistant" || result.Outcome.Text == "" {
				return "", errors.New("baseline wait result is duplicated or incomplete")
			}
			report = result.Outcome.Text
		}
	}
	if report == "" {
		return "", errors.New("baseline native session lacks the completed wait result")
	}
	return report, nil
}

func collaborationBaselineChildReport(data []byte, sessionID, report string) error {
	entries, err := collaborationBaselineEntries(data, sessionID)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type != "message" || entry.Message.Role != "assistant" {
			continue
		}
		parts, err := collaborationBaselineText(entry.Message.Content)
		if err != nil {
			return err
		}
		if strings.Join(parts, "") == report {
			return nil
		}
	}
	return errors.New("completed wait result differs from the native child report")
}
