package piglitter

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

func TestNativeMemberEnvironmentHandoff(t *testing.T) {
	if os.Getenv("P08_ENV_TEST_CHILD") == "1" {
		var environment map[string]string
		if err := json.NewDecoder(os.Stdin).Decode(&environment); err != nil {
			t.Fatal(err)
		}
		if err := applyNativeMemberEnvironment(environment); err != nil {
			t.Fatal(err)
		}
		for name, expected := range map[string]string{"OPENAI_API_KEY": "fixture-only", "P08_CUSTOM": "lead-value", "HOME": "/lead-home", "TERM": "pane-term", "ITERM_SESSION_ID": "pane-session", "TERM_SESSION_ID": "pane-term-session"} {
			if os.Getenv(name) != expected {
				t.Fatalf("environment variable %s was not transferred correctly", name)
			}
		}
		for _, name := range []string{"PIG_EXT_SOCKET", "TMUX_PANE", "APP_ONLY"} {
			if _, present := os.LookupEnv(name); present {
				t.Fatalf("environment variable %s unexpectedly survived", name)
			}
		}
		return
	}
	lead := []string{"OPENAI_API_KEY=fixture-only", "P08_CUSTOM=lead-value", "HOME=/lead-home", "TERM=lead-term", "TERM_SESSION_ID=lead-term-session", "PIG_EXT_SOCKET=lead-socket", "TMUX_PANE=%1"}
	environment := nativeMemberEnvironment(lead)
	data, err := json.Marshal(environment)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestNativeMemberEnvironmentHandoff$")
	cmd.Env = []string{"P08_ENV_TEST_CHILD=1", "OPENAI_API_KEY=app-only", "TERM=pane-term", "ITERM_SESSION_ID=pane-session", "TERM_SESSION_ID=pane-term-session", "PIG_EXT_SOCKET=app-socket", "TMUX_PANE=%7", "APP_ONLY=value"}
	cmd.Stdin = bytes.NewReader(data)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("environment handoff failed: %v: %s", err, output)
	}
}
