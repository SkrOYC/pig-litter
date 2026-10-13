package main

import (
	"path/filepath"
	"testing"
)

func TestDeniedPathMatchesOnlyTheWorkspaceTargets(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "workspace")
	for _, path := range []string{
		"denied-read.txt",
		filepath.Join(workspace, "denied-read.txt"),
		filepath.Join("sub", "..", "denied-read.txt"),
	} {
		if !deniedPath(workspace, "read", path) {
			t.Errorf("deniedPath(%q) did not match the workspace read target", path)
		}
	}
	if !deniedPath(workspace, "write", filepath.Join(workspace, "denied-write.txt")) {
		t.Fatal("absolute workspace write target was not denied")
	}
	if deniedPath(workspace, "read", filepath.Join(filepath.Dir(workspace), "denied-read.txt")) {
		t.Fatal("same-basename file outside the workspace was denied")
	}
	if deniedPath(workspace, "edit", filepath.Join(workspace, "denied-write.txt")) {
		t.Fatal("unconfigured edit operation was denied")
	}
}
