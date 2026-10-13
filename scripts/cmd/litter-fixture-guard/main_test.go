package main

import (
	"path/filepath"
	"testing"
)

func TestFinalSpawnResultFailureTransformsOnlyNamedSpawn(t *testing.T) {
	t.Setenv("LITTER_FIXTURE_REPLACE_DETAILS", "0")
	original := `{"state":"starting","child":{"id":"provisional-id","name":"result-hook-retry"}}`
	data := map[string]any{
		"toolName": "litter_spawn",
		"input":    map[string]any{"name": failedSpawnName, "task": failedSpawnTask},
		"content":  []any{map[string]any{"type": "text", "text": original}},
	}
	result, err := finalSpawnResultFailure(data)
	if err != nil {
		t.Fatal(err)
	}
	replacement, ok := result.(map[string]any)
	if !ok || replacement["isError"] != true || textContent(replacement["content"]) != spawnFailureText+original || replacement["details"] == nil {
		t.Fatalf("tool result replacement = %#v", result)
	}

	t.Setenv("LITTER_FIXTURE_REPLACE_DETAILS", "1")
	data["input"] = map[string]any{"name": failedSpawnName, "task": "RESULT_HOOK_SUCCESS"}
	result, err = finalSpawnResultFailure(data)
	replacement, ok = result.(map[string]any)
	if err != nil || !ok || replacement["details"] == nil || replacement["content"] != nil || replacement["isError"] != nil {
		t.Fatalf("successful litter result did not receive a details-only replacement: %#v, %v", result, err)
	}
	data["toolName"] = "read"
	if result, err := finalSpawnResultFailure(data); err != nil || result != nil {
		t.Fatalf("file tool result was modified: %#v, %v", result, err)
	}
}

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
