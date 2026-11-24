package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCollaborationSourceSnapshotCopiesAndIdentifiesSelectedFiles(t *testing.T) {
	root := t.TempDir()
	source := filepath.Join(root, "source")
	fixture := filepath.Join(root, "fixture")
	for _, directory := range []string{source, fixture, filepath.Join(source, "dist"), filepath.Join(source, "nested")} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	for path, content := range map[string]string{
		"go.mod": "module example\n", "nested/source.go": "package example\n", "dist/output": "excluded binary",
	} {
		if err := os.WriteFile(filepath.Join(source, path), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	files, sourceDigest, err := collaborationSnapshotSource(source, fixture)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].Path != "go.mod" || files[1].Path != "nested/source.go" {
		t.Fatalf("selected files = %+v", files)
	}
	if _, err := os.Stat(filepath.Join(fixture, "dist")); !os.IsNotExist(err) {
		t.Fatalf("distribution output entered source fixture: %v", err)
	}
	_, copiedDigest, err := collaborationSnapshotSource(fixture, "")
	if err != nil || copiedDigest != sourceDigest {
		t.Fatalf("copied source digest = %q, original = %q, error = %v", copiedDigest, sourceDigest, err)
	}
	if err := os.WriteFile(filepath.Join(fixture, "nested/source.go"), []byte("package changed\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, changedDigest, err := collaborationSnapshotSource(fixture, "")
	if err != nil || changedDigest == sourceDigest {
		t.Fatalf("changed source digest = %q, original = %q, error = %v", changedDigest, sourceDigest, err)
	}
	if err := os.Symlink("go.mod", filepath.Join(source, "link")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collaborationSnapshotSource(source, ""); err == nil {
		t.Fatal("source snapshot accepted a symbolic link")
	}
}

func TestCollaborationSourceCaseRequiresExplicitSource(t *testing.T) {
	directory := t.TempDir()
	pig := filepath.Join(directory, "pig")
	extension := filepath.Join(directory, "pig-litter")
	source := filepath.Join(directory, "source")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	for path, mode := range map[string]os.FileMode{pig: 0o600, extension: 0o700, filepath.Join(source, "go.mod"): 0o600} {
		if err := os.WriteFile(path, []byte("fixture"), mode); err != nil {
			t.Fatal(err)
		}
	}
	args := []string{"--case", "source", "--source", source, "--pig", pig, "--extension", extension, "--idle-observer", extension, "--evidence-root", directory}
	selected, err := parseOptions(args)
	if err != nil || selected.caseName != caseSource || selected.source != source {
		t.Fatalf("source selection = %+v, error = %v", selected, err)
	}
	if _, err := parseOptions([]string{"--case", "source", "--pig", pig, "--extension", extension, "--idle-observer", extension, "--evidence-root", directory}); err == nil {
		t.Fatal("source case accepted no source directory")
	}
}

func TestCollaborationSourceRejectsLocalModuleReplacement(t *testing.T) {
	directory := t.TempDir()
	data := []byte("module example.com/example\n\ngo 1.26.0\n\nreplace example.com/local => ../local\n")
	if err := os.WriteFile(filepath.Join(directory, "go.mod"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collaborationValidateSourceModule(directory); err == nil {
		t.Fatal("source fixture accepted a local module replacement")
	}
}
