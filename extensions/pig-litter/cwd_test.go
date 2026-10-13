package piglitter

import (
	"os"
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
