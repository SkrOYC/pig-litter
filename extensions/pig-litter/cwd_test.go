package piglitter

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestChildDirectoryResolvesAndChecksCallerDirectory(t *testing.T) {
	base := t.TempDir()
	child := filepath.Join(base, "child's workspace")
	if err := os.Mkdir(child, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, request := range []string{"child's workspace", child} {
		got, err := childDirectory(base, request)
		if err != nil || got != child {
			t.Fatalf("resolved %q to %q: %v", request, got, err)
		}
	}
	if got, err := childDirectory(child, ""); err != nil || got != child {
		t.Fatalf("inherited cwd %q: %v", got, err)
	}
	file := filepath.Join(base, "input.txt")
	if err := os.WriteFile(file, []byte("source"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, request := range []string{"missing", file} {
		if _, err := childDirectory(base, request); err == nil {
			t.Fatalf("accepted non-directory %q", request)
		}
	}
}

func TestChildToolArgsUseSelectedDirectoryWithoutMutatingInput(t *testing.T) {
	cwd := filepath.Join(t.TempDir(), "child")
	for _, name := range []string{"read", "write", "edit", "ls"} {
		args := map[string]any{"path": "input.txt"}
		got := childToolArgs(name, cwd, args)
		if got["path"] != filepath.Join(cwd, "input.txt") || args["path"] != "input.txt" {
			t.Fatalf("%s args %#v, input %#v", name, got, args)
		}
	}
	if got := childToolArgs("ls", cwd, map[string]any{}); got["path"] != cwd {
		t.Fatalf("default ls path %#v", got)
	}
	abs := filepath.Join(t.TempDir(), "outside.txt")
	if got := childToolArgs("read", cwd, map[string]any{"path": abs}); got["path"] != abs {
		t.Fatalf("absolute path %#v", got)
	}
}

func TestChildBashRunsInQuotedDirectory(t *testing.T) {
	base := t.TempDir()
	cwd := filepath.Join(base, "child's workspace")
	if err := os.Mkdir(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "input.txt"), []byte("private child marker"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := childToolArgs("bash", cwd, map[string]any{"command": "cat input.txt > output.txt; pwd"})
	command := exec.Command("bash", "-c", args["command"].(string))
	command.Dir = base
	output, err := command.CombinedOutput()
	if err != nil || string(output) != cwd+"\n" {
		t.Fatalf("bash cwd output %q: %v", output, err)
	}
	got, err := os.ReadFile(filepath.Join(cwd, "output.txt"))
	if err != nil || string(got) != "private child marker" {
		t.Fatalf("bash file %q: %v", got, err)
	}
	if _, err := os.Stat(filepath.Join(base, "output.txt")); !os.IsNotExist(err) {
		t.Fatalf("bash wrote in caller directory: %v", err)
	}
}
