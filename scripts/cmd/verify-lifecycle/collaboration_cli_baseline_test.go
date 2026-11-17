package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func collaborationBaselineFixture(t *testing.T, marker, childText string) (collaborationCLIRunner, string, []byte) {
	t.Helper()
	runner := collaborationCLIRunner{directory: t.TempDir()}
	evidence := filepath.Join(runner.directory, "evidence")
	if err := os.MkdirAll(filepath.Join(evidence, "native-final"), 0o700); err != nil {
		t.Fatal(err)
	}
	report := "QUICK_REPORT " + marker + "\nchild result"
	wait, err := json.Marshal(map[string]any{
		"operation": "litter_wait", "state": "completed", "outcome": map[string]any{
			"run": map[string]string{"id": "quick"}, "state": "completed", "text": report, "reportProvenance": "child_assistant",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	encode := func(value any) string {
		t.Helper()
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return string(data) + "\n"
	}
	root := encode(map[string]string{"type": "session", "id": "root"}) + encode(map[string]any{
		"type": "message", "message": map[string]any{"role": "toolResult", "toolName": "litter_wait", "content": []map[string]string{{"type": "text", "text": string(wait)}}},
	})
	if childText == "" {
		childText = report
	}
	child := encode(map[string]string{"type": "session", "id": "quick"}) + encode(map[string]any{
		"type": "message", "message": map[string]any{"role": "assistant", "content": []map[string]string{{"type": "text", "text": childText}}},
	})
	for name, content := range map[string]string{"root.jsonl": root, "quick.jsonl": child} {
		if err := os.WriteFile(filepath.Join(evidence, "native-final", name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var result collaborationCLIFixtureResult
	result.OK = true
	result.SelectedToolNames = []string{"litter_wait", "litter_spawn"}
	result.QuickChild.ID, result.QuickChild.Name = "quick", "quick-newer"
	result.ChildRefs = append(result.ChildRefs, struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{ID: "a", Name: "held-a"}, struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}{ID: "b", Name: "held-b"})
	result.Fixture.RootSessions = append(result.Fixture.RootSessions, struct {
		ID     string                          `json:"id"`
		File   collaborationCLIFixtureResource `json:"file"`
		SHA256 string                          `json:"sha256"`
	}{ID: "root", SHA256: collaborationTestDataDigest([]byte(root))})
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	return runner, evidence, data
}

func TestCollaborationCLIBaselineComparesRetainedSpawnAndWaitResults(t *testing.T) {
	first, firstDir, firstResult := collaborationBaselineFixture(t, "PRIVATE_ASSISTANT_A1", "")
	second, secondDir, secondResult := collaborationBaselineFixture(t, "PRIVATE_ASSISTANT_B2", "")
	a, err := first.baselineObservation(firstDir, firstResult)
	if err != nil {
		t.Fatal(err)
	}
	b, err := second.baselineObservation(secondDir, secondResult)
	if err != nil {
		t.Fatal(err)
	}
	samples := []collaborationBaselineSample{
		{Sample: collaborationPerformanceSample{Target: "baseline-off"}, Observation: a},
		{Sample: collaborationPerformanceSample{Target: "candidate-off"}, Observation: b},
		{Sample: collaborationPerformanceSample{Target: "candidate-on", Capture: true}, Observation: a},
		{Sample: collaborationPerformanceSample{Target: "baseline-on", Capture: true}, Observation: b},
	}
	if err := collaborationCompareBaselineSamples(samples); err != nil {
		t.Fatalf("matching normalized results = %v", err)
	}
	if err := collaborationCompareBaselineSamples(samples[:3]); err == nil {
		t.Fatal("accepted a missing capture mode")
	}
	samples[3].Observation.ReportSHA256 = strings.Repeat("f", 64)
	if err := collaborationCompareBaselineSamples(samples); err == nil {
		t.Fatal("accepted a changed wait report")
	}
	bad, badDir, badResult := collaborationBaselineFixture(t, "PRIVATE_ASSISTANT_C3", "different native child output")
	if _, err := bad.baselineObservation(badDir, badResult); err == nil || !strings.Contains(err.Error(), "native child report") {
		t.Fatalf("mismatched native child report = %v", err)
	}
}
