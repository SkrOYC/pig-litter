package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollaborationHostCrashReplay(t *testing.T) {
	if stage := os.Getenv("COLLAB_CRASH_WORKER_STAGE"); stage != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
		defer cancel()
		if err := collaborationRunCrashWorker(ctx, stage, os.Getenv("COLLAB_CRASH_WORKER_ROOT")); err != nil {
			t.Fatal(err)
		}
		return
	}
	for _, stage := range []string{"after-accept-before-append", "after-append-before-return", "after-return"} {
		t.Run(stage, func(t *testing.T) {
			root := collaborationProbeDirectory(t, "crash-"+stage)
			collaborationProbeTestEnvironment(t, root)
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			args := []string{os.Args[0], "-test.run=^TestCollaborationHostCrashReplay$"}
			env := []string{"COLLAB_CRASH_WORKER_STAGE=" + stage, "COLLAB_CRASH_WORKER_ROOT=" + root}
			evidence, err := collaborationRunHostCrashProbeCommand(ctx, root, stage, args, env)
			if err != nil {
				t.Fatalf("crash and replay: %v", err)
			}
			wantBefore := 1
			if stage == "after-accept-before-append" {
				wantBefore = 0
			}
			if evidence.Status != "observed" || evidence.CopiesBeforeRestart != wantBefore || evidence.CopiesAfterReplay != wantBefore+1 || !evidence.SameSession || evidence.DuplicateExistingEntry != (wantBefore > 0) {
				t.Fatalf("crash evidence = %#v", evidence)
			}
			if evidence.WorkerPID == evidence.RestartPID || evidence.Cut.ProcessID != evidence.WorkerPID || evidence.ReplayOffsetNS < evidence.CrashOffsetNS || evidence.CrashOffsetNS < evidence.CutOffsetNS {
				t.Fatalf("crash process identity or offsets = %#v", evidence)
			}
		})
	}
}

func TestCollaborationCrashWorkerFailureRetainsEvidence(t *testing.T) {
	root := collaborationProbeDirectory(t, "crash-worker-failure")
	collaborationProbeTestEnvironment(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	args := []string{os.Args[0], "-test.run=^$"}
	evidence, err := collaborationRunHostCrashProbeCommand(ctx, root, "after-accept-before-append", args, nil)
	if err == nil || evidence.Status != "inconclusive" || evidence.WorkerPID <= 0 {
		t.Fatalf("missing crash worker result = %#v, %v", evidence, err)
	}
	data, err := os.ReadFile(filepath.Join(root, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var retained collaborationCrashEvidence
	if err := json.Unmarshal(data, &retained); err != nil || retained.Status != "inconclusive" || retained.Error == "" {
		t.Fatalf("retained crash worker failure = %#v, %v", retained, err)
	}
}
