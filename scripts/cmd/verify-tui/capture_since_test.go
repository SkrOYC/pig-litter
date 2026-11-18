package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCaptureVersionMustBelongToNewGeneration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "pty.raw")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := file.WriteString(versionBanner); err != nil {
		t.Fatal(err)
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	capture := &ptyCapture{evidence: directory, rawInfo: info}
	offset := info.Size()
	if _, err := file.WriteString("resumed output"); err != nil {
		t.Fatal(err)
	}
	if seen, err := capture.containsSince(offset, []byte(versionBanner)); err != nil || seen {
		t.Fatalf("old generation version was accepted: seen=%t err=%v", seen, err)
	}
	if _, err := file.WriteString(versionBanner); err != nil {
		t.Fatal(err)
	}
	if seen, err := capture.containsSince(offset, []byte(versionBanner)); err != nil || !seen {
		t.Fatalf("new generation version was not observed: seen=%t err=%v", seen, err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(versionBanner), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := capture.containsSince(offset, []byte(versionBanner)); err == nil {
		t.Fatal("replaced raw capture was accepted")
	}
}
