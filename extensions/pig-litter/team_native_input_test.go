//go:build p08native && linux

package piglitter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTmuxNativeInputStartup(t *testing.T) {
	probe, base, source := os.Getenv("P08_NATIVE_PROBE"), os.Getenv("P08_LIVE_ROOT"), os.Getenv("P08_SOURCE_ROOT")
	if probe == "" || base == "" || source == "" {
		t.Fatal("P08_NATIVE_PROBE, P08_LIVE_ROOT, and P08_SOURCE_ROOT are required")
	}
	for _, mode := range []string{"success", "fail"} {
		t.Run(mode, func(t *testing.T) {
			rootPath := base + "-input-" + mode
			if err := os.Mkdir(rootPath, 0o700); err != nil {
				t.Fatal(err)
			}
			cwd := filepath.Join(rootPath, "work")
			for _, directory := range []string{cwd, filepath.Join(rootPath, "home"), filepath.Join(rootPath, "agent"), filepath.Join(rootPath, "sessions")} {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var nonce [16]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(rootPath, "root.sock"))
			if err != nil {
				t.Fatal(err)
			}
			root := &nativeProbeRoot{listener: listener, token: hex.EncodeToString(nonce[:]), path: rootPath, launch: true, active: true}
			go root.serve()
			defer root.disconnect()
			ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
			defer cancel()
			fixture := exec.CommandContext(ctx, probe, "--fixture")
			fixture.Env = append(os.Environ(), "PROBE_ROOT="+rootPath, "PROBE_TOKEN="+root.token)
			fixtureLog, err := os.Create(filepath.Join(rootPath, "fixture.stderr"))
			if err != nil {
				t.Fatal(err)
			}
			defer fixtureLog.Close()
			fixture.Stderr = fixtureLog
			if err := fixture.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = fixture.Process.Kill(); _ = fixture.Wait() }()
			provider := filepath.Join(rootPath, "provider.jsonl")
			probeWait(t, ctx, "fixture listener", func() bool { return probeCount(provider, "event", "listening") > 0 })
			address, _ := probeRows(provider)[0]["address"].(string)
			models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":"http://%s/v1","apiKey":"fixture-only","models":[{"id":"fixed","name":"fixed","contextWindow":32000,"maxTokens":1024}]}}}`, address)
			if err := os.WriteFile(filepath.Join(rootPath, "agent", "models.json"), []byte(models), 0o600); err != nil {
				t.Fatal(err)
			}
			socket := filepath.Join(rootPath, "tmux.sock")
			tmux := func(args ...string) string {
				t.Helper()
				output, err := exec.CommandContext(ctx, "tmux", append([]string{"-S", socket}, args...)...).CombinedOutput()
				if err != nil {
					t.Fatalf("tmux %q: %v: %s", args, err, output)
				}
				return strings.TrimSuffix(string(output), "\n")
			}
			tmux("new-session", "-d", "-s", "input", "-x", "120", "-y", "40", "sleep", "600")
			defer func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() }()
			tmux("set-option", "-w", "-t", "input:0", "remain-on-exit", "on")
			identity := strings.Split(tmux("display-message", "-p", "-t", "input:0.0", "#{pid}\t#{pane_id}"), "\t")
			if len(identity) != 2 {
				t.Fatalf("invalid tmux identity: %q", identity)
			}
			pid, err := strconv.Atoi(identity[0])
			if err != nil {
				t.Fatal(err)
			}
			server, err := tmuxFromEnvironment(ctx, fmt.Sprintf("%s,%d,0", socket, pid), identity[1])
			if err != nil {
				t.Fatal(err)
			}
			unrelated := tmux("split-window", "-d", "-P", "-F", "#{pane_id}", "-t", server.target, "sleep", "600")
			if _, err := rand.Read(nonce[:]); err != nil {
				t.Fatal(err)
			}
			launchID := hex.EncodeToString(nonce[:])
			manifest := filepath.Join(rootPath, "member-"+launchID+".json")
			environment := map[string]string{"PATH": os.Getenv("PATH"), "TERM": "xterm-256color", "LANG": "C.UTF-8", "HOME": filepath.Join(rootPath, "home"), "PIG_HOME": filepath.Join(rootPath, "home", ".pig"), "PIG_CODING_AGENT_DIR": filepath.Join(rootPath, "agent"), "PIG_SDK_GO_ROOT": source, "PROBE_ROOT": rootPath, "PROBE_TOKEN": root.token, "TMPDIR": rootPath, "PI_SKIP_VERSION_CHECK": "1", "P08_INPUT_TEST": mode}
			args := []string{"--session-id", "018f0000-0000-7000-8000-000000000002", "--session-dir", filepath.Join(rootPath, "sessions"), "--model", "fixture/fixed", "--tools", "read", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files", "--approve", "QUEUE_GATE"}
			data, err := json.Marshal(map[string]any{"cwd": cwd, "env": environment, "args": args})
			if err != nil || os.WriteFile(manifest, data, 0o600) != nil {
				t.Fatal("write native manifest", err)
			}
			pane, err := server.launch(ctx, tmuxLaunch{executable: probe, manifest: manifest, cwd: cwd, nonce: launchID})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = pane.close(context.Background()) }()
			tmux("pipe-pane", "-o", "-t", pane.id, "cat > "+filepath.Join(rootPath, "terminal.raw")+"; printf eof > "+filepath.Join(rootPath, "terminal.eof"))
			events := filepath.Join(rootPath, "events.jsonl")
			probeWait(t, ctx, "provisional transfer", func() bool { return probeCount(events, "event", "input_staged") == 1 })
			if probeCount(events, "event", "session_start") != 0 || probeCount(events, "event", "input") != 0 || probeCount(provider, "event", "request") != 0 {
				t.Fatal("input ran before provisional transfer was released")
			}
			if err := os.WriteFile(filepath.Join(rootPath, "release-window"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			probeWait(t, ctx, "session readiness", func() bool { return probeCount(events, "event", "session_start") == 1 })
			if probeCount(events, "event", "input") != 0 || probeCount(provider, "event", "request") != 0 {
				t.Fatal("input ran while readiness was held")
			}
			var sessionFile string
			for _, row := range probeRows(events) {
				if row["event"] == "session_start" {
					identity, _ := row["identity"].(map[string]any)
					sessionFile, _ = identity["sessionFile"].(string)
				}
			}
			if sessionFile == "" {
				t.Fatal("native readiness did not expose the session path")
			}
			if _, err := os.Stat(sessionFile); !os.IsNotExist(err) {
				t.Fatalf("fresh native session file exists before the seed: %v", err)
			}
			if err := os.WriteFile(filepath.Join(rootPath, "release-ready"), nil, 0o600); err != nil {
				t.Fatal(err)
			}
			if mode == "fail" {
				probeWait(t, ctx, "readiness refusal", func() bool { return probeCount(events, "event", "readiness_refused") == 1 })
				probeWait(t, ctx, "refused member exit", func() bool {
					return strings.HasPrefix(tmux("display-message", "-p", "-t", pane.id, "#{pane_dead} #{pane_dead_status}"), "1 ")
				})
				if probeCount(provider, "event", "request") != 0 || probeCount(events, "text", "START_ONE") != 0 || probeCount(events, "text", "START_TWO") != 0 {
					t.Fatal("refused readiness activated native work")
				}
			} else {
				probeWait(t, ctx, "seed provider gate", func() bool { return probeCount(provider, "event", "queue_gate_wait") > 0 })
				baselineData, err := os.ReadFile(filepath.Join(rootPath, "baseline.json"))
				var baseline []json.RawMessage
				if err != nil || json.Unmarshal(baselineData, &baseline) != nil {
					t.Fatal("cannot read the held native baseline", err)
				}
				pin, err := pinMemberLog(ctx, filepath.Dir(sessionFile), sessionFile, cwd, "018f0000-0000-7000-8000-000000000002")
				if err != nil {
					t.Fatal(err)
				}
				snapshot, err := readMemberLog(ctx, pin)
				if err != nil || len(snapshot.entries) < len(baseline) || memberEntriesDigest(snapshot.entries[:len(baseline)]) != memberEntriesDigest(baseline) {
					t.Fatalf("the persisted session does not retain the held baseline: before=%s, after=%s, %v", baselineData, snapshot.entries, err)
				}
				probeWait(t, ctx, "transferred input decisions", func() bool { return probeCount(events, "event", "input_result") == 2 })
				for _, row := range probeRows(events) {
					if row["event"] == "input_result" {
						result, _ := row["result"].(map[string]any)
						if result["Disposition"] != "queued" {
							t.Fatalf("transferred input result = %#v", row)
						}
					}
				}
				root.mu.Lock()
				root.queueProvider = true
				root.mu.Unlock()
				probeWait(t, ctx, "both transferred inputs", func() bool {
					return probeRequestContains(provider, "START_ONE") && probeRequestContains(provider, "START_TWO")
				})
				root.disconnect()
				probeWait(t, ctx, "native member exit", func() bool {
					return strings.HasPrefix(tmux("display-message", "-p", "-t", pane.id, "#{pane_dead} #{pane_dead_status}"), "1 ")
				})
			}
			if err := os.WriteFile(filepath.Join(rootPath, "after.ansi"), []byte(tmux("capture-pane", "-p", "-e", "-t", pane.id)), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := pane.close(ctx); err != nil {
				t.Fatal(err)
			}
			if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, unrelated) || strings.Contains(panes, pane.id) {
				t.Fatalf("unexpected panes after input test: %q", panes)
			}
		})
	}
}
