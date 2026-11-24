package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRenderedNoticeCheckUsesRecordedTerminalOutput(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pty.raw")
	if err := os.WriteFile(path, []byte("Teammate Alice | available\nTeammate Bob | available\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	proof := &panelProof{ctx: context.Background(), evidence: dir, rawActive: func() error { return nil }}
	expected := []string{"Teammate Alice | available", "Teammate Bob | available"}
	forbidden := []string{`"kind":"available"`}
	if err := proof.waitRenderedNotices(expected, forbidden); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(`{"kind":"available"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := proof.waitRenderedNotices(expected, forbidden); err == nil {
		t.Fatal("raw notice JSON was accepted as rendered output")
	}
}
