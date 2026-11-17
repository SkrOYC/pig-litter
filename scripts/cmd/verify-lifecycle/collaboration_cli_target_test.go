package main

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCollaborationCLITargetBranchMustContainCommit(t *testing.T) {
	git, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	checkout := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		command := exec.Command(git, append([]string{"-C", checkout}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
		return strings.TrimSpace(string(output))
	}
	run("init", "-q")
	run("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "first")
	first := run("rev-parse", "HEAD")
	run("branch", "tested")
	run("-c", "user.name=Fixture", "-c", "user.email=fixture@example.invalid", "commit", "--allow-empty", "-qm", "second")
	second := run("rev-parse", "HEAD")
	run("switch", "--detach", "-q", first)
	root := t.TempDir()
	events, err := os.Create(filepath.Join(root, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	runner := collaborationCLIRunner{directory: root, started: time.Now(), eventFile: events}
	target := collaborationCLITarget{Checkout: checkout, Identity: collaborationTarget{Branch: "tested", ProductCommit: first}}
	if err := runner.verifyTargetBranch(context.Background(), "valid", git, target); err != nil {
		t.Fatalf("detached checkout at the declared branch: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "valid-branch-identity.json"))
	var receipt map[string]string
	if err != nil || json.Unmarshal(data, &receipt) != nil || receipt["ref"] != "refs/heads/tested" || receipt["ref_commit"] != first || receipt["target_commit"] != first {
		t.Fatalf("branch identity receipt = %s, %v", data, err)
	}
	target.Identity.ProductCommit = second
	if err := runner.verifyTargetBranch(context.Background(), "wrong", git, target); err == nil || !strings.Contains(err.Error(), "not contained") {
		t.Fatalf("commit outside declared branch = %v", err)
	}
	target.Identity.ProductCommit = first
	target.Identity.Branch = "not a branch"
	if err := runner.verifyTargetBranch(context.Background(), "invalid", git, target); err == nil {
		t.Fatal("accepted an invalid branch name")
	}
}
