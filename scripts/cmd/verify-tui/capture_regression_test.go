package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRawPTYDoesNotCertifyAnUnownedPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pty.raw")
	if err := os.WriteFile(path, []byte("clean final frame\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".stderr", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := checkRawPTY(path); err == nil || !strings.Contains(err.Error(), "inconclusive") {
		t.Fatalf("unowned prefix must be inconclusive, got %v", err)
	}
}

func TestTerminalTextRejectsUnobservedEnhancedInput(t *testing.T) {
	if err := validateTerminalText("ordinary A_界🦊É"); err != nil {
		t.Fatal(err)
	}
	for _, input := range []string{"\x1b[13u", "\x1b[97;1;97u", "\x1b[107;1:3u", "\x1b[1;1B", "\u009b13u", "\x9b13u"} {
		if err := validateTerminalText(input); err == nil || !strings.Contains(err.Error(), "unsupported live terminal input") {
			t.Fatalf("unsupported input %q was accepted: %v", input, err)
		}
	}
}
