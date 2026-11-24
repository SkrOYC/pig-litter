//go:build p08native && linux

package piglitter

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type productMemberCase struct {
	name, task, marker, report string
	tools                      []string
	planRequired, exists       bool
	failure                    bool
	starting, running          bool
	preapprove, nestedGate     bool
	dropReplies                bool
	causal                     bool
	policyRefuse, staleWake    bool
	rootLoss, startupFailure   bool
	terminalControls           bool
}

func TestTmuxNativeMCP(t *testing.T) {
	for _, test := range []productMemberCase{
		{name: "mcp_direct", task: "case direct-mcp-allow", tools: []string{"mcp__probe__write", "read"}, marker: "direct-mcp-allow.txt", report: "PROBE_TOOL_DONE", exists: true},
		{name: "mcp_nested", task: "case codemode-mcp-allow", tools: []string{"codemode", "mcp__probe__write", "read"}, marker: "codemode-mcp-allow.txt", report: "PROBE_TOOL_DONE", exists: true},
		{name: "mcp_denied", task: "case direct-mcp-deny", tools: []string{"mcp__probe__write", "read"}, marker: "direct-mcp-deny.txt", report: "PROBE_TOOL_DONE", planRequired: true},
	} {
		t.Run(test.name, func(t *testing.T) { testTmuxCompiledProductMember(t, test) })
	}
}

func TestTmuxCompiledProductMember(t *testing.T) {
	for _, test := range []productMemberCase{
		{name: "starting", task: "QUEUE_GATE", tools: []string{"read"}, report: "PROBE_DONE", starting: true},
		{name: "running", task: "QUEUE_GATE", tools: []string{"read"}, report: "PROBE_DONE", running: true},
		{name: "lost_starting", task: "QUEUE_GATE", tools: []string{"read"}, report: "PROBE_DONE", starting: true, dropReplies: true},
		{name: "lost_running", task: "QUEUE_GATE", tools: []string{"read"}, report: "PROBE_DONE", running: true, dropReplies: true},
		{name: "causal", task: "QUEUE_GATE", tools: []string{"read"}, report: "PROBE_DONE", running: true, causal: true},
		{name: "policy_refuse", task: "review the task", tools: []string{"read"}, report: "PROBE_DONE", policyRefuse: true},
		{name: "stale_wake", task: "review the task", tools: []string{"read"}, report: "PROBE_DONE", staleWake: true},
		{name: "root_loss", task: "case direct-write-lost", tools: []string{"read", "write"}, marker: "direct-write-lost.txt", rootLoss: true},
		{name: "startup_failure", task: "review the task", tools: []string{"read", "write"}, startupFailure: true},
		{name: "terminal_controls", task: "review the task", tools: []string{"read"}, report: "PROBE_DONE", terminalControls: true},
		{name: "direct_allow", task: "case direct-write-allow", tools: []string{"read", "write"}, marker: "direct-write-allow.txt", report: "PROBE_TOOL_DONE", exists: true},
		{name: "direct_deny", task: "case direct-write-deny", tools: []string{"read", "write"}, marker: "direct-write-deny.txt", report: "PROBE_TOOL_DONE", planRequired: true},
		{name: "direct_failure", task: "case direct-write-fail", tools: []string{"read", "write"}, report: "PROBE_TOOL_DONE", failure: true},
		{name: "nested_allow", task: "case codemode-write-allow", tools: []string{"codemode", "read", "write"}, marker: "codemode-write-allow.txt", report: "PROBE_TOOL_DONE", exists: true},
		{name: "nested_deny", task: "case codemode-write-deny", tools: []string{"codemode", "read", "write"}, marker: "codemode-write-deny.txt", report: "PROBE_TOOL_DONE", planRequired: true, preapprove: true, nestedGate: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			testTmuxCompiledProductMember(t, test)
		})
	}
}

func testTmuxCompiledProductMember(t *testing.T, test productMemberCase) {
	name, task, marker, report, tools := test.name, test.task, test.marker, test.report, test.tools
	planRequired, exists, starting, running, failure := test.planRequired, test.exists, test.starting, test.running, test.failure
	preapprove, nestedGate, dropReplies, causal := test.preapprove, test.nestedGate, test.dropReplies, test.causal
	policyRefuse, staleWake, rootLoss, startupFailure := test.policyRefuse, test.staleWake, test.rootLoss, test.startupFailure
	terminalControls := test.terminalControls
	fixtureBinary, memberBinary := os.Getenv("P08_NATIVE_PROBE"), os.Getenv("P08_PRODUCT_MEMBER")
	rootPath := os.Getenv("P08_LIVE_ROOT") + "-product-" + name
	if fixtureBinary == "" || memberBinary == "" || os.Getenv("P08_LIVE_ROOT") == "" {
		t.Fatal("P08_NATIVE_PROBE, P08_PRODUCT_MEMBER, and P08_LIVE_ROOT are required")
	}
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd, agentDir, sessionDir := filepath.Join(rootPath, "work"), filepath.Join(rootPath, "agent"), filepath.Join(rootPath, "sessions")
	for _, directory := range []string{cwd, agentDir, sessionDir, filepath.Join(rootPath, "home"), filepath.Join(rootPath, "files")} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	gatePath := filepath.Join(rootPath, "files", "nested-gate")
	if nestedGate {
		if err := syscall.Mkfifo(gatePath, 0o600); err != nil {
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
	root := &nativeProbeRoot{listener: listener, token: hex.EncodeToString(capability[:]), path: rootPath, active: true}
	go root.serve()
	defer root.disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	fixture := exec.CommandContext(ctx, fixtureBinary, "--fixture")
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
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(name, "mcp_") {
		var requests atomic.Int32
		mcp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/v1/chat/completions" {
				body, _ := io.ReadAll(r.Body)
				line, _ := json.Marshal(map[string]any{"event": "request", "body": json.RawMessage(body)})
				if file, err := os.OpenFile(provider, os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
					_, _ = file.Write(append(line, '\n'))
					_ = file.Close()
				}
				if requests.Add(1) == 1 {
					target := filepath.Join(rootPath, "files", marker)
					tool, args := "mcp__probe__write", any(map[string]any{"path": target, "content": "PROBE_WRITTEN"})
					if name == "mcp_nested" {
						tool = "codemode"
						args = map[string]any{"code": fmt.Sprintf(`if ("mcp__probe__blocked" in tools) throw new Error("excluded tool is visible"); return await tools.mcp__probe__write({path:%q,content:"PROBE_WRITTEN"});`, target)}
					}
					runtimeToolAnswer(w, "mcp-call", tool, args)
				} else {
					runtimeAnswer(w, "PROBE_TOOL_DONE")
				}
				return
			}
			if r.Method == http.MethodDelete {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			var request struct {
				ID     json.RawMessage `json:"id"`
				Method string          `json:"method"`
				Params struct {
					ProtocolVersion string `json:"protocolVersion"`
					Name            string `json:"name"`
					Arguments       struct {
						Path    string `json:"path"`
						Content string `json:"content"`
					} `json:"arguments"`
				} `json:"params"`
			}
			if json.NewDecoder(r.Body).Decode(&request) != nil {
				http.Error(w, "invalid request", http.StatusBadRequest)
				return
			}
			if len(request.ID) == 0 {
				w.WriteHeader(http.StatusAccepted)
				return
			}
			var result any
			switch request.Method {
			case "initialize":
				w.Header().Set("Mcp-Session-Id", "p08-fixture")
				result = map[string]any{"protocolVersion": request.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "probe", "version": "1"}}
			case "tools/list":
				result = map[string]any{"tools": []any{map[string]any{"name": "write", "description": "Write a fixture file", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"path", "content"}}}, map[string]any{"name": "blocked", "description": "Excluded fixture tool", "inputSchema": map[string]any{"type": "object"}}}}
			case "tools/call":
				target := request.Params.Arguments.Path
				if request.Params.Name != "write" || !strings.HasPrefix(target, filepath.Join(rootPath, "files")+string(os.PathSeparator)) || request.Params.Arguments.Content != "PROBE_WRITTEN" || os.WriteFile(target, []byte(request.Params.Arguments.Content), 0o600) != nil {
					http.Error(w, "invalid write", http.StatusBadRequest)
					return
				}
				result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "PROBE_MCP_WROTE"}}}
			default:
				http.Error(w, "unknown method", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request.ID, "result": result})
		}))
		defer mcp.Close()
		config := fmt.Sprintf(`{"mcpServers":{"probe":{"url":%q,"exposure":"direct"}}}`, mcp.URL)
		if err := os.WriteFile(filepath.Join(agentDir, "mcp.json"), []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
		models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"fixture-only","models":[{"id":"fixed","name":"fixed","contextWindow":32000,"maxTokens":1024}]}}}`, mcp.URL+"/v1")
		if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
			t.Fatal(err)
		}
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
	tmux("new-session", "-d", "-s", "product", "-x", "120", "-y", "40", "sleep", "600")
	defer func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() }()
	tmux("set-option", "-w", "-t", "product:0", "remain-on-exit", "on")
	identity := strings.Split(tmux("display-message", "-p", "-t", "product:0.0", "#{pid}\t#{pane_id}"), "\t")
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
	rootID := ChildID("018f0000-0000-7000-8000-000000000001")
	memberID := ChildID("018f0000-0000-7000-8000-000000000002")
	store, err := openTeamStore(agentDir, rootID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	member, err := store.Reserve(memberID, string(storeTestUUID(3)), "member", WorkspacePolicy{Kind: "shared_checkout", Cwd: cwd}, planRequired)
	if err != nil {
		t.Fatal(err)
	}
	if preapprove {
		member, err = store.Commit(member.AdmissionID)
		if err != nil {
			t.Fatal(err)
		}
	}
	config := defaultConfig()
	config.MaxMailbox = 2
	tree := testTree(t, config)
	tree.root.id = rootID
	worker, _ := agentDefaults("worker")
	_, selected, err := tree.admit(admission{kind: "worker", name: "member", model: "fixture/fixed", cwd: cwd, task: task, agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	if preapprove {
		plan, err := store.submitPlan(member.Ref, selected.noticeID, "fixture-plan", "Write the marker.", 60000)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.decidePlan(plan.ID, selected.noticeID, "approve", ""); err != nil {
			t.Fatal(err)
		}
	}
	if starting {
		for _, text := range []string{"START_ONE", "START_TWO"} {
			if _, _, err := tree.message(ctx, nil, selected, "follow_up", text); err != nil {
				t.Fatal(err)
			}
		}
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	launchID := hex.EncodeToString(nonce[:])
	manifestPath := filepath.Join(rootPath, "member-"+launchID+".json")
	controlPath := filepath.Join(rootPath, "member-control.sock")
	if dropReplies || causal || staleWake || rootLoss || terminalControls {
		controlPath = filepath.Join(rootPath, "control-proxy.sock")
	}
	manifest := nativeMemberManifest{Version: 1, Launch: launchID, Session: memberID, Cwd: cwd, Model: "fixture/fixed", Tools: tools, SessionDirectory: sessionDir, AgentDirectory: agentDir, PigHome: filepath.Join(rootPath, "home", ".pig"), Socket: controlPath, Capability: hex.EncodeToString(capability[:]), Prompt: selected.prompt, Instructions: worker.Instructions, MailboxLimit: 2, ProjectTrusted: true}
	data, err := json.Marshal(manifest)
	if err != nil || os.WriteFile(manifestPath, data, 0o600) != nil {
		t.Fatal("write product manifest", err)
	}
	available := tools
	if startupFailure {
		available = []string{"read"}
	}
	host := &runtimeHost{tools: available}
	withCompletionHost(t, tree, func(o *owner) {
		o.cwd, o.agentDir, o.teamStore = cwd, agentDir, store
		if policyRefuse {
			o.policy.TeammateIdle = []string{"/bin/sh", "-c", `if [ -e "$1" ]; then printf '{"permit":true}'; else : > "$1"; printf '{"permit":false,"feedback":"Complete the review."}'; fi`, "policy", filepath.Join(rootPath, "policy-ran")}
		}
		tree.persist, tree.teamFinished = o.saveChild, o.finishTeamMember
		launch := tmuxLaunch{executable: memberBinary, manifest: manifestPath, cwd: cwd, nonce: launchID}
		ownership, err := o.reserveMemberLaunch(ctx, selected.owner, selected, server, launch, sessionDir)
		if err != nil {
			t.Fatal(err)
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
		queue, err := o.newMemberControlQueueWithCapability(selected.owner, selected, ownership, capability)
		if err != nil {
			t.Fatal(err)
		}
		defer queue.Close()
		binding, err := o.installProvisionalMemberHandle(ctx, selected.owner, selected, sessionDir, launchID, queue)
		if err != nil {
			t.Fatal(err)
		}
		tree.mu.Lock()
		selected.started, selected.admission, selected.owner.admitted = true, admissionReady, true
		tree.mu.Unlock()
		if err := queue.attachAuthority(binding); err != nil {
			t.Fatal(err)
		}
		if probeCount(provider, "event", "request") != 0 {
			t.Fatal("product member ran before its root handle was installed")
		}
		control, err := startMemberControlServer(ctx, rootPath, queue)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = control.Close() }()
		var dropper *productReplyDropper
		var proxyServer *http.Server
		if dropReplies || causal || staleWake || rootLoss || terminalControls {
			dropper = &productReplyDropper{queue: queue, drop: dropReplies, input: running, causal: causal, staleWake: staleWake, holdTool: rootLoss, holdFinalize: terminalControls, reentrant: make(chan error, 1), toolEntered: make(chan struct{}), toolRelease: make(chan struct{}), finalizeEntered: make(chan struct{}), finalizeRelease: make(chan struct{}), innerEntered: make(chan struct{}), innerRelease: make(chan struct{}), outerClosed: make(chan struct{})}
			proxy, err := net.Listen("unix", controlPath)
			if err != nil {
				t.Fatal(err)
			}
			proxyServer = &http.Server{Handler: dropper}
			go func() { _ = proxyServer.Serve(proxy) }()
			defer func() { _ = proxyServer.Close(); _ = os.Remove(controlPath) }()
			if terminalControls {
				defer dropper.releaseFinalization()
			}
			if causal {
				defer dropper.releaseInner()
			}
		}
		if startupFailure || rootLoss {
			if rootLoss {
				select {
				case <-dropper.toolEntered:
				case <-ctx.Done():
					t.Fatal("native write did not reach the held root decision")
				}
				queue.Close()
				_ = proxyServer.Close()
				close(dropper.toolRelease)
			}
			probeWait(t, ctx, "unavailable product member exit", func() bool {
				return strings.HasPrefix(tmux("display-message", "-p", "-t", pane.id, "#{pane_dead}"), "1")
			})
			if startupFailure && probeCount(provider, "event", "request") != 0 {
				t.Fatal("failed readiness reached the provider")
			}
			if rootLoss {
				if _, err := os.Stat(filepath.Join(rootPath, "files", marker)); !os.IsNotExist(err) {
					t.Fatalf("root loss allowed the held write: %v", err)
				}
			}
			queue.Close()
			tree.finish(selected.owner, selected, Outcome{State: Unavailable, Error: "native member ended before approved settlement"})
			tree.teamWG.Wait()
			tree.mu.Lock()
			outcome := *selected.outcome
			tree.mu.Unlock()
			if outcome.State == Completed {
				t.Fatal("unavailable native member was approved as completed")
			}
			_ = os.WriteFile(filepath.Join(rootPath, "after.ansi"), []byte(tmux("capture-pane", "-p", "-e", "-t", pane.id)), 0o600)
			result, _ := json.Marshal(map[string]any{"outcome": outcome, "panePid": pane.pid, "paneBirth": pane.birth, "executableSha256": ownership.ExecutableSHA256})
			if err := os.WriteFile(filepath.Join(rootPath, "outcome.json"), result, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := o.releaseMemberLaunch(ctx, selected.owner, selected, ownership, pane); err != nil {
				t.Fatal(err)
			}
			if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, unrelated) || strings.Contains(panes, pane.id) {
				t.Fatalf("unavailable member cleanup changed unrelated panes: %q", panes)
			}
			return
		}
		if terminalControls {
			select {
			case <-dropper.finalizeEntered:
			case <-ctx.Done():
				t.Fatal("native settlement did not reach the held root decision")
			}
			send := func(text string) {
				t.Helper()
				tmux("send-keys", "-t", pane.id, "-l", "--", text)
				tmux("send-keys", "-t", pane.id, "Enter")
			}
			bashDenied := func(path string) bool {
				entries, err := o.memberLogEntries(ctx, binding)
				if err != nil {
					return false
				}
				for _, raw := range entries {
					var entry struct {
						Message struct {
							Role, Command, Output string
							ExitCode              int `json:"exitCode"`
						} `json:"message"`
					}
					if json.Unmarshal(raw, &entry) == nil && entry.Message.Role == "bashExecution" && entry.Message.Command == "touch "+path && entry.Message.ExitCode == 1 && strings.Contains(entry.Message.Output, "Terminal shell commands are unavailable") {
						return true
					}
				}
				return false
			}
			before := filepath.Join(rootPath, "files", "bash-before")
			send("!touch " + before)
			probeWait(t, ctx, "terminal shell denial", func() bool { return bashDenied(before) })
			if _, err := os.Stat(before); !os.IsNotExist(err) {
				t.Fatalf("terminal shell created a file: %v", err)
			}
			send("/reload")
			probeWait(t, ctx, "native reload", func() bool {
				return strings.Contains(tmux("capture-pane", "-p", "-t", pane.id), "Reloaded keybindings")
			})
			if snapshot, err := queue.Snapshot(ctx, memberHistoryBound{}); err != nil || snapshot.session != memberID || snapshot.model != "fixture/fixed" {
				t.Fatalf("root control after reload = %#v, %v", snapshot, err)
			}
			after := filepath.Join(rootPath, "files", "bash-after")
			send("!touch " + after)
			probeWait(t, ctx, "terminal shell denial after reload", func() bool { return bashDenied(after) })
			if _, err := os.Stat(after); !os.IsNotExist(err) {
				t.Fatalf("terminal shell after reload created a file: %v", err)
			}
			dropper.releaseFinalization()
		}
		if starting || running {
			probeWait(t, ctx, "product seed or failure", func() bool {
				return probeCount(provider, "event", "queue_gate_wait") > 0 || strings.HasPrefix(tmux("display-message", "-p", "-t", pane.id, "#{pane_dead}"), "1")
			})
			if probeCount(provider, "event", "queue_gate_wait") == 0 {
				t.Fatalf("product member failed before its seed: %s", tmux("capture-pane", "-p", "-t", pane.id))
			}
			probeWait(t, ctx, "product starting messages", func() bool {
				selected.messageMu.Lock()
				defer selected.messageMu.Unlock()
				return selected.pendingMessages == 0
			})
			if !queue.ready() {
				t.Fatal("product member was not admitted by the root readiness check")
			}
			if running && !causal {
				for _, text := range []string{"RUN_ONE", "RUN_TWO"} {
					if _, _, err := tree.message(ctx, nil, selected, "follow_up", text); err != nil {
						t.Fatalf("running message %s: %v", text, err)
					}
				}
				if _, _, err := tree.message(ctx, nil, selected, "follow_up", "RUN_OVERFLOW"); err == nil {
					t.Fatal("full native input owner accepted a third message")
				}
			}
			if causal {
				if _, _, err := tree.message(ctx, nil, selected, "follow_up", "CAUSE_OUTER"); err != nil {
					t.Fatalf("outer callback message: %v", err)
				}
				if err := <-dropper.reentrant; err == nil || !strings.Contains(err.Error(), "remains pending") {
					t.Fatalf("dependent callback result = %v", err)
				}
				select {
				case <-dropper.innerEntered:
				case <-ctx.Done():
					t.Fatal("dependent preflight did not reach its hold")
				}
				select {
				case <-dropper.outerClosed:
				case <-ctx.Done():
					t.Fatal("outer receipt was not released while the dependent preflight was held")
				}
				dropper.releaseInner()
				var disposition string
				probeWait(t, ctx, "dependent native input outcome", func() bool {
					queue.mu.Lock()
					defer queue.mu.Unlock()
					for _, input := range queue.running {
						if input.input.Text == "CAUSE_INNER" && input.interim != nil && input.interim.Disposition == "would_block" && input.result != nil {
							disposition = input.result.Disposition
							return true
						}
					}
					return false
				})
				evidence, _ := json.Marshal(map[string]any{"innerHeld": true, "outerReceiptClosed": true, "dependentDisposition": disposition})
				if err := os.WriteFile(filepath.Join(rootPath, "causal-order.json"), evidence, 0o600); err != nil {
					t.Fatal(err)
				}
				if disposition != "queued" {
					t.Errorf("dependent input after outer receipt release = %q, want queued", disposition)
				}
			}
			root.mu.Lock()
			root.queueProvider = true
			root.mu.Unlock()
		}
		if nestedGate {
			probeWait(t, ctx, "nested read authorization", func() bool {
				queue.authority.mu.Lock()
				defer queue.authority.mu.Unlock()
				for id, call := range queue.authority.approved {
					if strings.Contains(id, "/") && call.name == "read" && call.ready && !call.decision.Block {
						return true
					}
				}
				return false
			})
			if err := store.recoverPlans(member.Ref); err != nil {
				t.Fatal(err)
			}
			written := make(chan error, 1)
			go func() { written <- os.WriteFile(gatePath, []byte("go"), 0o600) }()
			select {
			case err := <-written:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("nested read did not reach its gate")
			}
		}
		probeWait(t, ctx, "product settlement or exit", func() bool {
			select {
			case <-selected.done:
				return true
			default:
				return strings.HasPrefix(tmux("display-message", "-p", "-t", pane.id, "#{pane_dead}"), "1")
			}
		})
		select {
		case <-selected.done:
		default:
			t.Fatalf("product member exited without a root outcome: %s", tmux("capture-pane", "-p", "-t", pane.id))
		}
		tree.teamWG.Wait()
		tree.mu.Lock()
		outcome := *selected.outcome
		tree.mu.Unlock()
		_, roster, pending, err := store.SnapshotWithReservations()
		wantState, wantRoster := Completed, "idle"
		if failure {
			wantState, wantRoster = Partial, "unavailable"
		}
		if err != nil || outcome.State != wantState || outcome.Text != report || outcome.Usage.TotalTokens == 0 || len(roster) != 1 || roster[0].State != wantRoster || len(pending) != 0 || failure && !strings.Contains(outcome.Error, "write") || starting && (!probeRequestContains(provider, "START_ONE") || !probeRequestContains(provider, "START_TWO")) || running && !causal && (!probeRequestContains(provider, "RUN_ONE") || !probeRequestContains(provider, "RUN_TWO") || probeRequestContains(provider, "RUN_OVERFLOW")) || causal && (!probeRequestContains(provider, "CAUSE_OUTER") || !probeRequestContains(provider, "CAUSE_INNER")) || (policyRefuse || staleWake) && probeCount(provider, "event", "request") < 2 {
			t.Errorf("product member outcome = %#v, roster=%#v, pending=%#v, %v", outcome, roster, pending, err)
		}
		if marker != "" {
			data, err := os.ReadFile(filepath.Join(rootPath, "files", marker))
			if exists && (err != nil || string(data) != "PROBE_WRITTEN") || !exists && !os.IsNotExist(err) {
				t.Fatalf("tool side effect %s = %q, %v", marker, data, err)
			}
		}
		queue.mu.Lock()
		var inputs []map[string]any
		for _, input := range queue.running {
			inputs = append(inputs, map[string]any{"window": input.input.Window, "transfer": input.input.Transfer, "text": input.input.Text, "hasCause": input.input.Cause != "", "interim": input.interim, "result": input.result})
		}
		queue.mu.Unlock()
		inputEvidence, err := json.Marshal(inputs)
		if err != nil || os.WriteFile(filepath.Join(rootPath, "running.json"), inputEvidence, 0o600) != nil {
			t.Fatal("write native input evidence", err)
		}
		if dropReplies {
			if dropper.startDropped.Load() != 1 || dropper.installDropped.Load() != 1 || running && dropper.inputDropped.Load() != 1 {
				t.Fatalf("lost replies = start %d, installation %d, input %d", dropper.startDropped.Load(), dropper.installDropped.Load(), dropper.inputDropped.Load())
			}
			drops, _ := json.Marshal(map[string]uint64{"start": dropper.startDropped.Load(), "installation": dropper.installDropped.Load(), "input": dropper.inputDropped.Load()})
			if err := os.WriteFile(filepath.Join(rootPath, "lost-replies.json"), drops, 0o600); err != nil {
				t.Fatal(err)
			}
		}
		if staleWake && dropper.staleCount.Load() != 1 {
			t.Fatal("the root wake did not change between decision and settlement")
		}
		probeWait(t, ctx, "product process exit", func() bool {
			return strings.HasPrefix(tmux("display-message", "-p", "-t", pane.id, "#{pane_dead}"), "1")
		})
		if planRequired {
			entries, err := o.memberLogEntries(ctx, binding)
			if err != nil {
				t.Fatal(err)
			}
			observed := false
			for _, entry := range entries {
				if strings.Contains(string(entry), "plan approval is required before this tool can run") && (!nestedGate || strings.Contains(string(entry), `"nestedCalls"`) && strings.Contains(string(entry), `"name":"write"`)) {
					observed = true
				}
			}
			if !observed {
				t.Fatal("native history did not retain the root plan denial")
			}
		}
		_ = os.WriteFile(filepath.Join(rootPath, "after.ansi"), []byte(tmux("capture-pane", "-p", "-e", "-t", pane.id)), 0o600)
		result, _ := json.Marshal(map[string]any{"outcome": outcome, "roster": roster, "pending": pending, "panePid": pane.pid, "paneBirth": pane.birth, "executableSha256": ownership.ExecutableSHA256})
		if err := os.WriteFile(filepath.Join(rootPath, "outcome.json"), result, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := o.releaseMemberLaunch(ctx, selected.owner, selected, ownership, pane); err != nil {
			t.Fatal(err)
		}
		if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, unrelated) || strings.Contains(panes, pane.id) {
			t.Fatalf("product shutdown changed unrelated panes: %q", panes)
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
}

type productReplyDropper struct {
	queue            *memberControlQueue
	drop             bool
	input            bool
	causal           bool
	staleWake        bool
	holdTool         bool
	holdFinalize     bool
	reentrant        chan error
	toolEntered      chan struct{}
	toolRelease      chan struct{}
	toolOnce         atomic.Bool
	finalizeEntered  chan struct{}
	finalizeRelease  chan struct{}
	finalizeOnce     atomic.Bool
	finalizeDone     sync.Once
	startOnce        atomic.Bool
	installOnce      atomic.Bool
	inputOnce        atomic.Bool
	startDropped     atomic.Uint64
	installDropped   atomic.Uint64
	inputDropped     atomic.Uint64
	staleOnce        atomic.Bool
	staleCount       atomic.Uint64
	innerEntered     chan struct{}
	innerRelease     chan struct{}
	outerClosed      chan struct{}
	innerEnteredOnce sync.Once
	innerReleaseOnce sync.Once
	outerClosedOnce  sync.Once
}

func (dropper *productReplyDropper) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if dropper.causal && strings.HasPrefix(r.URL.Path, "/fixture/") && r.URL.Path != "/fixture/reentrant" {
		if !dropper.queue.authorize(r) {
			http.Error(w, "callback authority is unavailable", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/fixture/hold-inner":
			dropper.innerEnteredOnce.Do(func() { close(dropper.innerEntered) })
			<-dropper.innerRelease
		case "/fixture/outer-closing":
			<-dropper.innerEntered
		case "/fixture/outer-closed":
			dropper.outerClosedOnce.Do(func() { close(dropper.outerClosed) })
		default:
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	if dropper.holdFinalize && r.URL.Path == "/member/settlement/finalize" && dropper.finalizeOnce.CompareAndSwap(false, true) {
		close(dropper.finalizeEntered)
		<-dropper.finalizeRelease
	}
	if dropper.holdTool && r.URL.Path == "/member/tool" {
		data, err := io.ReadAll(io.LimitReader(r.Body, memberControlLimit))
		if err != nil {
			http.Error(w, "invalid tool decision", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var request memberToolRequest
		if json.Unmarshal(data, &request) == nil && request.Name == "write" && dropper.toolOnce.CompareAndSwap(false, true) {
			close(dropper.toolEntered)
			<-dropper.toolRelease
		}
	}
	if dropper.staleWake && r.URL.Path == "/member/settlement/decide" && dropper.staleOnce.CompareAndSwap(false, true) {
		recorder := httptest.NewRecorder()
		dropper.queue.ServeHTTP(recorder, r)
		var decision memberSettleReply
		if recorder.Code == http.StatusOK && json.Unmarshal(recorder.Body.Bytes(), &decision) == nil && decision.Allow {
			tree := dropper.queue.owner.tree
			tree.lifecycleMu.Lock()
			tree.mu.Lock()
			dropper.queue.record.peerWakeVersion++
			tree.mu.Unlock()
			tree.lifecycleMu.Unlock()
			dropper.staleCount.Add(1)
		}
		for key, values := range recorder.Header() {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.WriteHeader(recorder.Code)
		_, _ = w.Write(recorder.Body.Bytes())
		return
	}
	if dropper.causal && r.URL.Path == "/fixture/reentrant" {
		cause := r.Header.Get("X-Pig-Litter-Cause")
		if !dropper.queue.authorize(r) || cause == "" {
			http.Error(w, "callback authority is unavailable", http.StatusForbidden)
			return
		}
		ctx := context.WithValue(r.Context(), memberCauseKey{}, memberCause{queue: dropper.queue, id: cause})
		_, _, err := dropper.queue.owner.tree.message(ctx, nil, dropper.queue.run, "follow_up", "CAUSE_INNER")
		dropper.reentrant <- err
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		return
	}
	if !dropper.drop {
		dropper.queue.ServeHTTP(w, r)
		return
	}
	var count *atomic.Uint64
	switch r.URL.Path {
	case "/input/start":
		if dropper.startOnce.CompareAndSwap(false, true) {
			count = &dropper.startDropped
		}
	case "/input/installed":
		if dropper.installOnce.CompareAndSwap(false, true) {
			count = &dropper.installDropped
		}
	case "/control/result":
		data, err := io.ReadAll(io.LimitReader(r.Body, memberControlLimit))
		if err != nil {
			http.Error(w, "invalid control result", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(data))
		var result memberControlReply
		if dropper.input && json.Unmarshal(data, &result) == nil && result.Kind == "input" && dropper.inputOnce.CompareAndSwap(false, true) {
			count = &dropper.inputDropped
		}
	}
	if count == nil {
		dropper.queue.ServeHTTP(w, r)
		return
	}
	dropper.queue.ServeHTTP(&productDroppedReply{ResponseWriter: w, count: count}, r)
}

func (dropper *productReplyDropper) releaseFinalization() {
	dropper.finalizeDone.Do(func() { close(dropper.finalizeRelease) })
}

func (dropper *productReplyDropper) releaseInner() {
	dropper.innerReleaseOnce.Do(func() { close(dropper.innerRelease) })
}

type productDroppedReply struct {
	http.ResponseWriter
	count   *atomic.Uint64
	dropped bool
}

func (reply *productDroppedReply) WriteHeader(status int) {
	if status >= 200 && status < 300 {
		if hijacker, ok := reply.ResponseWriter.(http.Hijacker); ok {
			connection, _, err := hijacker.Hijack()
			if err == nil {
				reply.dropped = true
				reply.count.Add(1)
				_ = connection.Close()
				return
			}
		}
	}
	reply.ResponseWriter.WriteHeader(status)
}

func (reply *productDroppedReply) Write(data []byte) (int, error) {
	if !reply.dropped {
		reply.WriteHeader(http.StatusOK)
	}
	if reply.dropped {
		return len(data), nil
	}
	return reply.ResponseWriter.Write(data)
}
