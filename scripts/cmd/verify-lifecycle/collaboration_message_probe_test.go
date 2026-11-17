package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func collaborationProbeDirectory(t *testing.T, prefix string) string {
	t.Helper()
	base := os.Getenv("P02_PROBE_EVIDENCE_ROOT")
	if base == "" {
		return t.TempDir()
	}
	if !filepath.IsAbs(base) {
		t.Fatal("P02_PROBE_EVIDENCE_ROOT must be absolute")
	}
	if err := os.MkdirAll(base, 0o700); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(base, prefix+"-")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("probe evidence: %s", dir)
	return dir
}

func collaborationProbeTestEnvironment(t *testing.T, root string) {
	t.Helper()
	t.Setenv("PIG_OFFLINE", "1")
	t.Setenv("PI_OFFLINE", "1")
	t.Setenv("PIG_USE_PI_DIRS", "0")
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("USERPROFILE", filepath.Join(root, "home"))
	t.Setenv("PIG_HOME", root)
	t.Setenv("PIG_CODING_AGENT_DIR", filepath.Join(root, "agent"))
}

func TestCollaborationHostIdleMessagePersistence(t *testing.T) {
	root := collaborationProbeDirectory(t, "idle")
	collaborationProbeTestEnvironment(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	evidence, err := collaborationRunHostMessageProbe(ctx, root, "idle")
	if err != nil {
		t.Fatalf("idle message probe: %v", err)
	}
	if evidence.Status != "observed" || len(evidence.Phases) != 2 || evidence.Phases[1].RequestsWithMessage != 1 {
		t.Fatalf("idle message evidence = %#v", evidence)
	}
	observations := evidence.Phases[0].Observations
	if len(observations) != 3 || observations[0].FileExists || observations[0].MemoryCopies != 1 || observations[1].DiskCopies != 1 || observations[2].DiskCopies != 1 {
		t.Fatalf("idle persistence observations = %#v", observations)
	}
	data, err := os.ReadFile(filepath.Join(root, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var retained collaborationMessageEvidence
	if err := json.Unmarshal(data, &retained); err != nil || retained.Status != "observed" || len(retained.Phases) != 2 {
		t.Fatalf("retained idle evidence = %#v, %v", retained, err)
	}
}

func TestCollaborationHostBusyMessagePersistence(t *testing.T) {
	root := collaborationProbeDirectory(t, "busy")
	collaborationProbeTestEnvironment(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	evidence, err := collaborationRunHostMessageProbe(ctx, root, "busy")
	if err != nil {
		t.Fatalf("busy message probe: %v", err)
	}
	if evidence.Status != "observed" || len(evidence.Phases) != 2 {
		t.Fatalf("busy message evidence = %#v", evidence)
	}
	for _, phase := range evidence.Phases {
		if len(phase.Observations) != 2 || phase.Observations[0].DiskCopies != 0 || phase.Observations[1].DiskCopies != 1 || phase.Observations[0].ProviderRequests != 2 {
			t.Fatalf("busy phase observations = %#v", phase)
		}
	}
	steering := evidence.Phases[1]
	if steering.Observations[1].ProviderRequests != 3 || steering.RequestsWithMessage != 1 {
		t.Fatalf("busy steering provider requests = %#v", steering)
	}
}

func TestCollaborationMessageFailureRetainsEvidence(t *testing.T) {
	root := collaborationProbeDirectory(t, "invalid-message")
	evidence, err := collaborationRunHostMessageProbe(context.Background(), root, "unknown")
	if err == nil || evidence.Status != "inconclusive" {
		t.Fatalf("invalid message mode = %#v, %v", evidence, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var retained collaborationMessageEvidence
	if err := json.Unmarshal(data, &retained); err != nil || retained.Status != "inconclusive" || retained.Error == "" {
		t.Fatalf("retained failure = %#v, %v", retained, err)
	}
	if _, err := collaborationRunHostMessageProbe(context.Background(), root, "idle"); err == nil {
		t.Fatal("reused probe root overwrote evidence")
	}
}
