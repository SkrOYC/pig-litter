package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCollaborationHostIndependentSelectedProcess(t *testing.T) {
	root := collaborationProbeDirectory(t, "process")
	collaborationProbeTestEnvironment(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	pig := os.Getenv("P02_PROCESS_PIG")
	if pig == "" {
		var err error
		pig, err = exec.LookPath("pig")
		if err != nil {
			t.Fatalf("resolve the actual PiG executable: %v", err)
		}
	}
	pig, err := filepath.Abs(pig)
	if err != nil {
		t.Fatal(err)
	}
	binary := os.Getenv("P02_PROCESS_EXTENSION")
	if binary == "" {
		binary = filepath.Join(root, "pig-litter")
		build := exec.CommandContext(ctx, "go", "build", "-mod=readonly", "-o", binary, "./cmd/pig-litter")
		build.Dir = filepath.Clean(filepath.Join("..", ".."))
		output, buildErr := build.CombinedOutput()
		if err := os.WriteFile(filepath.Join(root, "extension-build.log"), output, 0o600); err != nil {
			t.Fatal(err)
		}
		if buildErr != nil {
			t.Fatalf("build selected extension: %v: %s", buildErr, output)
		}
	}
	evidence, err := collaborationRunHostIndependentProcessProbe(ctx, root, pig, binary)
	if err != nil {
		t.Fatalf("independent process: %v", err)
	}
	if evidence.Status != "observed" || evidence.HostPID == evidence.ParentPID || evidence.SessionID == "" || len(evidence.ProviderTools) != 6 || len(evidence.ProviderRequests) != 1 || evidence.ExitCode == nil || *evidence.ExitCode != 0 {
		t.Fatalf("independent process evidence = %#v", evidence)
	}
}
