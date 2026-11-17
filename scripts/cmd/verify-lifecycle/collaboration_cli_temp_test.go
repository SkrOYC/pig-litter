package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCollaborationCLIShortTempSupportsUnixSocketsAndRetainsFiles(t *testing.T) {
	target := filepath.Join(t.TempDir(), strings.Repeat("x", 90))
	if err := os.Mkdir(target, 0o700); err != nil {
		t.Fatal(err)
	}
	alias, err := collaborationNewTempAlias(target)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = alias.release(true) })
	listener, err := net.Listen("unix", filepath.Join(alias.path, "fixture.sock"))
	if err != nil {
		t.Fatalf("listen through short alias: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(alias.path, "retained.txt"), []byte("fixture evidence"), 0o600); err != nil {
		t.Fatal(err)
	}
	if retained, err := alias.release(true); err != nil || retained != alias.path {
		t.Fatalf("non-empty temporary alias = %q, %v", retained, err)
	}
	if err := os.Remove(filepath.Join(target, "retained.txt")); err != nil {
		t.Fatal(err)
	}
	if retained, err := alias.release(true); err != nil || retained != "" {
		t.Fatalf("empty temporary alias = %q, %v", retained, err)
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
	if string(data) != driver.TemporaryAlias || driver.TemporaryAliasTarget != filepath.Join(directory, "tmp") || !driver.TemporaryAliasRemoved || len(driver.TemporaryAlias) > 60 {
		t.Fatalf("temporary alias receipt = %#v, output = %q", driver, data)
	}
	if _, err := os.Lstat(driver.TemporaryAlias); !os.IsNotExist(err) {
		t.Fatalf("temporary alias remains after joined driver: %v", err)
	}
}
