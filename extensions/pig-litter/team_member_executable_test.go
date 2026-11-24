package piglitter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNativeMemberExecutableUsesTheExtensionSource(t *testing.T) {
	if _, err := nativeMemberExecutable(context.Background(), ""); err == nil {
		t.Fatal("accepted an unknown extension source")
	}
	directory, err := os.MkdirTemp("", "p8-helper-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(directory)
	source := filepath.Join(directory, "source")
	if err := os.MkdirAll(filepath.Join(source, "dist"), 0o700); err != nil {
		t.Fatal(err)
	}
	helper := filepath.Join(source, "dist", "pig-litter")
	if _, err := nativeMemberExecutable(context.Background(), source); err == nil || !strings.Contains(err.Error(), "build-pig-litter.sh") {
		t.Fatalf("missing companion did not explain how to build it: %v", err)
	}
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf 'pig-litter-native-member-v1\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for _, origin := range []string{source, helper} {
		got, err := nativeMemberExecutable(context.Background(), origin)
		if err != nil || got != helper {
			t.Fatalf("helper for %s = %q, %v", origin, got, err)
		}
	}
	if err := os.WriteFile(helper, []byte("#!/bin/sh\nprintf 'wrong-protocol\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := nativeMemberExecutable(context.Background(), source); err == nil {
		t.Fatal("accepted an executable without native member support")
	}
}
