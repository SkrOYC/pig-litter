package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestCollaborationHookPhaseVerdicts(t *testing.T) {
	direct := collaborationHookPhase{
		Tool: "write", Deny: true, Target: "/fixture/deny", OuterCallID: "direct",
		Events:     []collaborationHookEvent{{Tool: "write", CallID: "direct", Path: "/fixture/deny", Refused: true}},
		ToolResult: json.RawMessage(`{"toolName":"write","toolCallId":"direct","isError":true}`), CallerText: collaborationHookRefusal,
	}
	nested := collaborationHookPhase{
		Tool: "write", Nested: true, Deny: true, Target: "/fixture/nested", OuterCallID: "outer",
		Events:     []collaborationHookEvent{{Tool: "write", CallID: "outer/1", ParentID: "outer", Path: "/fixture/nested", Refused: true}},
		ToolResult: json.RawMessage(`{"toolName":"codemode","toolCallId":"outer","nestedCalls":{"complete":true,"calls":[{"id":"outer/1","name":"write","status":"error","error":"COLLAB_HOOK_REFUSED"}]}}`), CallerText: collaborationHookRefusal,
	}
	tests := []struct {
		name  string
		phase collaborationHookPhase
		want  string
	}{
		{"direct refusal", direct, "observed"},
		{"nested refusal", nested, "observed"},
		{"side effect despite missing result", collaborationHookPhase{Deny: true, FileExists: true}, "provisional_bypass"},
		{"unobserved hook", collaborationHookPhase{Tool: "write", Deny: true, ToolResult: direct.ToolResult}, "inconclusive"},
		{"missing nested record", collaborationHookPhase{Tool: "write", Nested: true, Deny: true, Target: "/fixture/nested", OuterCallID: "outer", Events: nested.Events, ToolResult: json.RawMessage(`{"toolName":"codemode","toolCallId":"outer"}`), CallerText: collaborationHookRefusal}, "inconclusive"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			collaborationAssessHookPhase(&test.phase)
			if test.phase.Verdict != test.want {
				t.Fatalf("verdict = %q, want %q: %s", test.phase.Verdict, test.want, test.phase.Reason)
			}
		})
	}
}

func TestCollaborationHookProbePreservesExistingResult(t *testing.T) {
	root := t.TempDir()
	resultPath := filepath.Join(root, "result.json")
	if err := os.WriteFile(resultPath, []byte("existing evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := collaborationRunHostHookProbe(context.Background(), root); err == nil {
		t.Fatal("accepted an already-used evidence directory")
	}
	data, err := os.ReadFile(resultPath)
	if err != nil || string(data) != "existing evidence" {
		t.Fatalf("existing evidence = %q, %v", data, err)
	}
}

func TestCollaborationHostHookProbe(t *testing.T) {
	if root := os.Getenv("COLLAB_HOOK_PROBE_CHILD"); root != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		evidence, err := collaborationRunHostHookProbe(ctx, root)
		if err != nil {
			t.Fatalf("host hook research is %s: %v", evidence.Status, err)
		}
		return
	}
	parent := os.Getenv("COLLAB_HOOK_PROBE_ROOT")
	if parent == "" {
		parent = t.TempDir()
	}
	if !filepath.IsAbs(parent) {
		t.Fatal("COLLAB_HOOK_PROBE_ROOT must be absolute")
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	root, err := os.MkdirTemp(parent, "attempt-")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"home", "tmp", "config", "cache"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-test.run=^TestCollaborationHostHookProbe$", "-test.v")
	cmd.Dir = root
	cmd.Env = []string{
		"HOME=" + filepath.Join(root, "home"),
		"PIG_HOME=" + filepath.Join(root, "home"),
		"PIG_CODING_AGENT_DIR=" + filepath.Join(root, "agent"),
		"PIG_CODING_AGENT_SESSION_DIR=" + filepath.Join(root, "sessions"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "config"),
		"XDG_CACHE_HOME=" + filepath.Join(root, "cache"),
		"TMPDIR=" + filepath.Join(root, "tmp"),
		"PIG_OFFLINE=1", "PI_OFFLINE=1", "PIG_USE_PI_DIRS=0", "PI_TELEMETRY=0",
		"PATH=/usr/bin:/bin", "LANG=C", "COLLAB_HOOK_PROBE_CHILD=" + root,
	}
	output, runErr := cmd.CombinedOutput()
	logErr := os.WriteFile(filepath.Join(root, "runner.log"), output, 0o600)
	t.Logf("host hook research evidence: %s", root)
	if logErr != nil {
		t.Fatal(logErr)
	}
	if runErr != nil {
		t.Fatal(fmt.Errorf("host hook probe failed: %w\n%s", runErr, output))
	}
}
