//go:build teammate_native && linux

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

func TestTmuxNativeRootStartingHandoff(t *testing.T) {
	probe, base, source := os.Getenv("TEAM_NATIVE_PROBE"), os.Getenv("TEAM_LIVE_ROOT"), os.Getenv("TEAM_SOURCE_ROOT")
	if probe == "" || base == "" || source == "" {
		t.Fatal("TEAM_NATIVE_PROBE, TEAM_LIVE_ROOT, and TEAM_SOURCE_ROOT are required")
	}
	for _, mode := range []string{"success", "bad-ready", "commit"} {
		t.Run(mode, func(t *testing.T) {
			rootPath := base + "-root-" + mode
			if err := os.Mkdir(rootPath, 0o700); err != nil {
				t.Fatal(err)
			}
			cwd := filepath.Join(rootPath, "work")
			sessionDir := filepath.Join(rootPath, "sessions")
			for _, directory := range []string{cwd, filepath.Join(rootPath, "home"), filepath.Join(rootPath, "agent"), sessionDir} {
				if err := os.Mkdir(directory, 0o700); err != nil {
					t.Fatal(err)
				}
			}
			var capability [32]byte
			if _, err := rand.Read(capability[:]); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("unix", filepath.Join(rootPath, "root.sock"))
			if err != nil {
				t.Fatal(err)
			}
			root := &nativeProbeRoot{listener: listener, token: hex.EncodeToString(capability[:]), path: rootPath, launch: true, active: true}
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
			tmux("new-session", "-d", "-s", "root", "-x", "120", "-y", "40", "sleep", "600")
			defer func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() }()
			tmux("set-option", "-w", "-t", "root:0", "remain-on-exit", "on")
			identity := strings.Split(tmux("display-message", "-p", "-t", "root:0.0", "#{pid}\t#{pane_id}"), "\t")
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
			var nonce [16]byte
			if _, err := rand.Read(nonce[:]); err != nil {
				t.Fatal(err)
			}
			launchID := hex.EncodeToString(nonce[:])
			manifest := filepath.Join(rootPath, "member-"+launchID+".json")
			environment := map[string]string{"PATH": os.Getenv("PATH"), "TERM": "xterm-256color", "LANG": "C.UTF-8", "HOME": filepath.Join(rootPath, "home"), "PIG_HOME": filepath.Join(rootPath, "home", ".pig"), "PIG_CODING_AGENT_DIR": filepath.Join(rootPath, "agent"), "PIG_SDK_GO_ROOT": source, "PROBE_ROOT": rootPath, "PROBE_TOKEN": root.token, "TMPDIR": rootPath, "PI_SKIP_VERSION_CHECK": "1", "TEAM_INPUT_TEST": mode, "TEAM_CONTROL_SOCKET": filepath.Join(rootPath, "member-control.sock"), "TEAM_CONTROL_TOKEN": hex.EncodeToString(capability[:])}
			if mode == "commit" {
				environment["TEAM_COMMIT_ROOT"] = "1"
			}
			args := []string{"--session-id", "018f0000-0000-7000-8000-000000000002", "--session-dir", sessionDir, "--model", "fixture/fixed", "--tools", "read", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-context-files", "--approve", "Task:\nQUEUE_GATE"}
			data, err := json.Marshal(map[string]any{"cwd": cwd, "env": environment, "args": args})
			if err != nil || os.WriteFile(manifest, data, 0o600) != nil {
				t.Fatal("write native manifest", err)
			}
			rootID := ChildID("018f0000-0000-7000-8000-000000000001")
			memberID := ChildID("018f0000-0000-7000-8000-000000000002")
			member := teamMember{Ref: MemberRef{Team: TeamID{Root: rootID, Epoch: 1}, ID: memberID, Generation: 1}, AdmissionID: string(storeTestUUID(3)), Workspace: WorkspacePolicy{Kind: "shared_checkout", Cwd: cwd}}
			var store *teamStore
			if mode == "commit" {
				store, err = openTeamStore(filepath.Join(rootPath, "agent"), rootID)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				member, err = store.Reserve(memberID, string(storeTestUUID(3)), "member", member.Workspace, false)
				if err != nil {
					t.Fatal(err)
				}
			}
			config := defaultConfig()
			config.MaxMailbox = 2
			tree := testTree(t, config)
			tree.root.id = rootID
			worker, _ := agentDefaults("worker")
			_, selected, err := tree.admit(admission{kind: "worker", name: "member", model: "fixture/fixed", cwd: cwd, task: "QUEUE_GATE", agent: worker, member: &member})
			if err != nil {
				t.Fatal(err)
			}
			for _, text := range []string{"START_ONE", "START_TWO"} {
				if _, _, err := tree.message(ctx, nil, selected, "follow_up", text); err != nil {
					t.Fatal(err)
				}
			}
			host := &runtimeHost{tools: []string{"read"}}
			withCompletionHost(t, tree, func(o *owner) {
				o.cwd, o.agentDir = cwd, filepath.Join(rootPath, "agent")
				tree.persist = o.saveChild
				if store != nil {
					o.teamStore = store
					tree.teamFinished = o.finishTeamMember
				}
				launch := tmuxLaunch{executable: probe, manifest: manifest, cwd: cwd, nonce: launchID}
				ownership, err := o.reserveMemberLaunch(ctx, selected.owner, selected, server, launch, sessionDir)
				if err != nil {
					t.Fatal(err)
				}
				t.Logf("native launch model %s, tool ceiling %v", ownership.Model, ownership.Tools)
				if !hasSavedMemberOwnership(host, false) {
					t.Fatal("native launch intent was not persisted")
				}
				pane, err := server.launch(ctx, launch)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = pane.close(context.Background()) }()
				tmux("pipe-pane", "-o", "-t", pane.id, "cat > "+filepath.Join(rootPath, "terminal.raw")+"; printf eof > "+filepath.Join(rootPath, "terminal.eof"))
				if err := o.recordMemberPane(ctx, selected.owner, selected, ownership, pane); err != nil {
					t.Fatal(err)
				}
				process, err := json.Marshal(map[string]any{"root": rootID, "member": memberID, "serverPid": server.pid, "pane": pane.id, "panePid": pane.pid, "birth": pane.birth, "cwd": cwd, "model": ownership.Model, "tools": ownership.Tools, "executableSha256": ownership.ExecutableSHA256})
				if err != nil || os.WriteFile(filepath.Join(rootPath, "process.json"), process, 0o600) != nil {
					t.Fatal("write native process evidence", err)
				}
				queue, err := o.newMemberControlQueueWithCapability(selected.owner, selected, ownership, capability)
				if err != nil {
					t.Fatal(err)
				}
				defer queue.Close()
				binding, err := o.installProvisionalMemberHandle(ctx, selected.owner, selected, sessionDir, launchID, queue)
				if err != nil || binding.ready {
					t.Fatalf("provisional native handle = %#v, %v", binding, err)
				}
				if store != nil {
					tree.mu.Lock()
					selected.started, selected.admission, selected.owner.admitted = true, admissionReady, true
					tree.mu.Unlock()
					if err := queue.attachAuthority(binding); err != nil {
						t.Fatal(err)
					}
				}
				control, err := startMemberControlServer(ctx, rootPath, queue)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = control.Close() }()
				events := filepath.Join(rootPath, "events.jsonl")
				probeWait(t, ctx, "root starting transfer", func() bool { return probeCount(events, "event", "input_staged") == 1 })
				queue.mu.Lock()
				installed := queue.starting != nil && queue.starting.installed && len(queue.starting.transfers) == 2
				queue.mu.Unlock()
				if !installed || queue.ready() || probeCount(provider, "event", "request") != 0 || probeCount(events, "event", "session_start") != 0 {
					t.Fatal("native work crossed the root installation barrier")
				}
				if err := os.WriteFile(filepath.Join(rootPath, "release-window"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				probeWait(t, ctx, "native readiness", func() bool { return probeCount(events, "event", "session_start") == 1 })
				if queue.ready() || probeCount(provider, "event", "request") != 0 {
					t.Fatal("native seed ran before root readiness")
				}
				if err := os.WriteFile(filepath.Join(rootPath, "release-ready"), nil, 0o600); err != nil {
					t.Fatal(err)
				}
				if mode == "bad-ready" {
					probeWait(t, ctx, "root readiness refusal", func() bool { return probeCount(events, "event", "readiness_refused") == 1 })
					probeWait(t, ctx, "refused member exit", func() bool {
						return strings.HasPrefix(tmux("display-message", "-p", "-t", pane.id, "#{pane_dead} #{pane_dead_status}"), "1 ")
					})
					if queue.ready() || probeCount(provider, "event", "request") != 0 || probeCount(events, "text", "START_ONE") != 0 || probeCount(events, "text", "START_TWO") != 0 {
						t.Fatal("root refusal activated native work")
					}
				} else {
					probeWait(t, ctx, "seed provider gate or readiness refusal", func() bool {
						return probeCount(provider, "event", "queue_gate_wait") > 0 || probeCount(events, "event", "readiness_refused") > 0
					})
					if probeCount(events, "event", "readiness_refused") > 0 {
						t.Fatalf("root refused valid readiness: %#v", probeRows(events))
					}
					probeWait(t, ctx, "root transfer results", func() bool {
						selected.messageMu.Lock()
						defer selected.messageMu.Unlock()
						return selected.pendingMessages == 0
					})
					if !queue.ready() {
						t.Fatal("root did not accept native readiness")
					}
					if entries, err := o.memberLogEntries(ctx, binding); err != nil || len(entries) <= binding.log.baselineCount {
						t.Fatalf("root could not pin the native seed history: %s, %v", entries, err)
					}
					root.mu.Lock()
					root.queueProvider = true
					root.mu.Unlock()
					probeWait(t, ctx, "root transferred inputs", func() bool {
						return probeRequestContains(provider, "START_ONE") && probeRequestContains(provider, "START_TWO")
					})
					if mode == "commit" {
						select {
						case <-selected.done:
						case <-ctx.Done():
							t.Fatal("root did not commit the native outcome")
						}
						tree.teamWG.Wait()
						tree.mu.Lock()
						outcome := *selected.outcome
						tree.mu.Unlock()
						_, roster, pending, err := store.SnapshotWithReservations()
						if err != nil || outcome.State != Completed || outcome.Text != "PROBE_DONE" || outcome.Usage.TotalTokens == 0 || len(roster) != 1 || roster[0].Ref != member.Ref || roster[0].State != "idle" || len(pending) != 0 {
							t.Fatalf("committed native outcome = %#v, roster=%#v, pending=%#v, %v", outcome, roster, pending, err)
						}
						payload, _ := json.Marshal(map[string]any{"outcome": outcome, "roster": roster, "pending": pending})
						if err := os.WriteFile(filepath.Join(rootPath, "outcome.json"), payload, 0o600); err != nil {
							t.Fatal(err)
						}
					} else {
						root.disconnect()
					}
					probeWait(t, ctx, "member exit after root shutdown", func() bool {
						return strings.HasPrefix(tmux("display-message", "-p", "-t", pane.id, "#{pane_dead} #{pane_dead_status}"), "1 ")
					})
				}
				queue.Close()
				selected.messageMu.Lock()
				pending := selected.pendingMessages
				selected.messageMu.Unlock()
				if pending != 0 {
					t.Fatalf("root retained %d starting claims", pending)
				}
				if err := os.WriteFile(filepath.Join(rootPath, "after.ansi"), []byte(tmux("capture-pane", "-p", "-e", "-t", pane.id)), 0o600); err != nil {
					t.Fatal(err)
				}
				if err := o.releaseMemberLaunch(ctx, selected.owner, selected, ownership, pane); err != nil {
					t.Fatal(err)
				}
				if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, unrelated) || strings.Contains(panes, pane.id) {
					t.Fatalf("owned shutdown changed unrelated panes: %q", panes)
				}
				host.mu.Lock()
				entries, err := json.Marshal(host.entries)
				host.mu.Unlock()
				if err != nil || os.WriteFile(filepath.Join(rootPath, "root-entries.json"), entries, 0o600) != nil {
					t.Fatal("write root persistence evidence", err)
				}
			}, func(method string, raw json.RawMessage) json.RawMessage {
				if method == "sendMessage" {
					return json.RawMessage(`{}`)
				}
				return host.call(t, method, raw)
			})
		})
	}
}
