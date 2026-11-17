package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCollaborationCLIShortTempSupportsUnixSocketsAndRetainsFiles(t *testing.T) {
	temporary, err := collaborationNewTempDirectory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = temporary.release(true) })
	physical, err := filepath.EvalSymlinks(temporary.path)
	if err != nil || physical != temporary.path {
		t.Fatalf("temporary directory is not physical: %q, %v", physical, err)
	}
	listener, err := net.Listen("unix", filepath.Join(temporary.path, "fixture.sock"))
	if err != nil {
		t.Fatalf("listen through short alias: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(temporary.path, "retained.txt"), []byte("fixture evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if retained, err := temporary.release(true); err != nil || retained != temporary.path {
		t.Fatalf("non-empty temporary directory = %q, %v", retained, err)
	}
	if err := os.Remove(filepath.Join(temporary.path, "retained.txt")); err != nil {
		t.Fatal(err)
	}
	if retained, err := temporary.release(true); err != nil || retained != "" {
		t.Fatalf("empty temporary directory = %q, %v", retained, err)
	}
}

func TestCollaborationCLIDriverUsesAndRemovesShortTemp(t *testing.T) {
	root := t.TempDir()
	events, err := os.Create(filepath.Join(root, "events.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer events.Close()
	runner := collaborationCLIRunner{directory: root, started: time.Now(), eventFile: events}
	directory, err := runner.execute(context.Background(), "driver", []string{"/bin/sh", "-c", `printf '%s' "$TMPDIR"`})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout.log"))
	if err != nil || len(runner.drivers) != 1 {
		t.Fatalf("driver result = %q, %#v, %v", data, runner.drivers, err)
	}
	driver := runner.drivers[0]
	if string(data) != driver.TemporaryDirectory || !driver.TemporaryDirectoryRemoved || len(driver.TemporaryDirectory) > 60 {
		t.Fatalf("temporary directory receipt = %#v, output = %q", driver, data)
	}
	if _, err := os.Lstat(driver.TemporaryDirectory); !os.IsNotExist(err) {
		t.Fatalf("temporary directory remains after joined driver: %v", err)
	}
}
