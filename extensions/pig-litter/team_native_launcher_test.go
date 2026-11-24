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

	"github.com/MichaelKinsy/PiG/coding"
)

func TestTmuxRootOwnedMemberLifetime(t *testing.T) {
	for _, name := range []string{"success", "root_loss", "existing_history", "startup_failure", "prelaunch_failure", "stubborn", "descendant"} {
		t.Run(name, func(t *testing.T) { testTmuxRootOwnedMemberLifetime(t, name) })
	}
}

func testTmuxRootOwnedMemberLifetime(t *testing.T, name string) {
	fixtureBinary, memberBinary, prefix := os.Getenv("P08_NATIVE_PROBE"), os.Getenv("P08_PRODUCT_MEMBER"), os.Getenv("P08_LIVE_ROOT")
	if fixtureBinary == "" || memberBinary == "" || prefix == "" {
		t.Fatal("P08_NATIVE_PROBE, P08_PRODUCT_MEMBER, and P08_LIVE_ROOT are required")
	}
	if name == "stubborn" {
		memberBinary = os.Getenv("P08_STUBBORN_MEMBER")
		if memberBinary == "" {
			t.Fatal("P08_STUBBORN_MEMBER is required")
		}
	}
	rootPath := prefix + "-lifetime-" + name
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd, agentDir, home := filepath.Join(rootPath, "work"), filepath.Join(rootPath, "agent"), filepath.Join(rootPath, "home")
	for _, directory := range []string{cwd, agentDir, home} {
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
	if name == "startup_failure" {
		models = strings.Replace(models, `"id":"fixed"`, `"id":"other"`, 1)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(models), 0o600); err != nil {
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
	tmux("new-session", "-d", "-s", "lifetime", "-x", "120", "-y", "40", "sleep", "600")
	defer func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() }()
	tmux("set-option", "-w", "-t", "lifetime:0", "remain-on-exit", "on")
	tmux("set-environment", "-g", "PIG_HOME", filepath.Join(rootPath, "wrong-home"))
	tmux("set-environment", "-g", "PIG_USE_PI_DIRS", "1")
	identity := strings.Split(tmux("display-message", "-p", "-t", "lifetime:0.0", "#{pid}\t#{pane_id}"), "\t")
	pid, err := strconv.Atoi(identity[0])
	if err != nil || len(identity) != 2 {
		t.Fatalf("invalid tmux identity: %q", identity)
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
	member, err := store.Reserve(memberID, string(storeTestUUID(3)), "member", WorkspacePolicy{Kind: "shared_checkout", Cwd: cwd}, false)
	if err != nil {
		t.Fatal(err)
	}
	config := defaultConfig()
	config.MaxMailbox = 2
	tree := testTree(t, config)
	tree.root.id = rootID
	worker := AgentDefinition{Role: "worker", Tools: []string{"read"}, Instructions: "Complete the assigned task."}
	task := "QUEUE_GATE"
	if name == "descendant" {
		task, worker.CanDelegate = "case spawn-and-wait", true
	}
	_, selected, err := tree.admit(admission{kind: "worker", name: "member", model: "fixture/fixed", cwd: cwd, task: task, agent: worker, member: &member, backend: backendTmux})
	if err != nil {
		t.Fatal(err)
	}
	if name == "success" {
		if _, _, err := tree.message(ctx, nil, selected, "follow_up", "START_ONE"); err != nil {
			t.Fatal(err)
		}
	}
	if name == "existing_history" {
		directory, err := nativeSessionDirectory(cwd, agentDir, memberID, true)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(directory, "existing_"+string(memberID)+".jsonl")
		if err := os.WriteFile(path, []byte("existing history\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	available := []string{"read"}
	if name == "descendant" {
		available = append(available, "litter_spawn", "litter_wait")
	}
	host := &runtimeHost{tools: available}
	withCompletionHost(t, tree, func(o *owner) {
		var observed *memberOwnership
		o.cwd, o.agentDir, o.teamStore = cwd, agentDir, store
		tree.persist, tree.teamFinished, tree.completion = o.saveChild, o.finishTeamMember, o.sendCompletion
		if name == "descendant" {
			trusted := true
			services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: cwd, AgentDir: agentDir, ProjectTrusted: &trusted})
			if err != nil {
				t.Fatal(err)
			}
			defer services.Close()
			o.services = services
		}
		tree.execute = func(r *record, run *run) Outcome {
			if r.team == nil {
				return o.executeRun(r, run)
			}
			return o.executeTmuxMember(r, run, tmuxMemberEnvironment{server: server, executable: memberBinary, pigHome: filepath.Join(home, ".pig")})
		}
		if err := tree.activate(selected); err != nil {
			t.Fatal(err)
		}
		if name == "stubborn" {
			var ownership *memberOwnership
			probeWait(t, ctx, "stubborn native process", func() bool {
				tree.mu.Lock()
				ownership = cloneMemberOwnership(selected.owner.nativeOwnership)
				tree.mu.Unlock()
				if ownership == nil || ownership.Pane == nil {
					return false
				}
				directory := filepath.Dir(ownership.Manifest)
				_, err := os.Stat(filepath.Join(directory, "stubborn-ready"))
				socket, socketErr := os.Lstat(filepath.Join(directory, "member-control.sock"))
				return err == nil && socketErr == nil && socket.Mode()&os.ModeSocket != 0
			})
			observed = ownership
			if err := os.Remove(filepath.Join(filepath.Dir(ownership.Manifest), "stubborn-ready")); err != nil {
				t.Fatal(err)
			}
			closed := make(chan struct{})
			go func() { tree.close(); close(closed) }()
			select {
			case <-closed:
			case <-ctx.Done():
				t.Fatal("root close did not stop the stubborn native process")
			}
			if _, err := processBirth(ownership.Pane.PID); !os.IsNotExist(err) {
				t.Fatalf("stubborn native process survived root close: %v", err)
			}
			if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, unrelated) || strings.Contains(panes, ownership.Pane.ID) {
				t.Fatalf("stubborn member cleanup changed unrelated panes: %q", panes)
			}
		} else if name == "existing_history" || name == "startup_failure" || name == "prelaunch_failure" {
			select {
			case <-selected.done:
			case <-ctx.Done():
				t.Fatal("existing history did not reject startup")
			}
			<-selected.executorDone
			if selected.outcome.State != Unavailable || name == "existing_history" && !strings.Contains(selected.outcome.Error, "history already exists") || name == "prelaunch_failure" && !strings.Contains(selected.outcome.Error, "permission denied") || probeCount(provider, "event", "request") != 0 {
				t.Fatalf("failed startup outcome = %#v", selected.outcome)
			}
			tree.mu.Lock()
			retained := selected.owner.nativeOwnership
			tree.mu.Unlock()
			if retained != nil {
				t.Fatalf("failed startup retained native ownership: %#v", retained)
			}
			_, roster, pending, err := store.SnapshotWithReservations()
			if err != nil || len(roster) != 0 || len(pending) != 0 {
				t.Fatalf("failed startup did not roll back its admission: %#v %#v %v", roster, pending, err)
			}
			if panes := strings.Split(tmux("list-panes", "-a", "-F", "#{pane_id}"), "\n"); len(panes) != 2 || !strings.Contains(strings.Join(panes, "\n"), unrelated) {
				t.Fatalf("failed startup changed unrelated panes: %q", panes)
			}
		} else if name == "descendant" {
			var child *run
			probeWait(t, ctx, "native descendant reservation", func() bool {
				tree.mu.Lock()
				defer tree.mu.Unlock()
				for _, candidate := range tree.records {
					if candidate.parent == selected.owner {
						child = candidate.run
						return true
					}
				}
				return false
			})
			root.mu.Lock()
			root.childID = child.ref.ID
			root.mu.Unlock()
			probeWait(t, ctx, "descendant wait acknowledgement", func() bool {
				o.ackMu.Lock()
				defer o.ackMu.Unlock()
				return len(o.outcomeAcks) != 0 && probeCount(filepath.Join(rootPath, "root.jsonl"), "kind", "child-gate") > 0
			})
			root.mu.Lock()
			root.release = true
			root.mu.Unlock()
			probeWait(t, ctx, "native descendant report gate", func() bool { return probeCount(provider, "event", "wait_report_gate") > 0 })
			tree.mu.Lock()
			binding := selected.owner.native
			ownership := cloneMemberOwnership(selected.owner.nativeOwnership)
			tree.mu.Unlock()
			if binding == nil || ownership == nil || ownership.Pane == nil {
				t.Fatal("native member lost its handle before descendant audit")
			}
			observed = ownership
			tmux("pipe-pane", "-o", "-t", ownership.Pane.ID, "cat > "+filepath.Join(rootPath, "terminal.raw"))
			_ = os.WriteFile(filepath.Join(rootPath, "during.ansi"), []byte(tmux("capture-pane", "-p", "-e", "-t", ownership.Pane.ID)), 0o600)
			probeWait(t, ctx, "native descendant audit", func() bool {
				tree.mu.Lock()
				content := child.reservation.content
				tree.mu.Unlock()
				if content == "" {
					return false
				}
				_ = o.auditCompletion(child)
				entries, err := o.memberLogEntries(ctx, binding)
				return err == nil && completionStored(entries, completionDetails{ChildID: child.ref.ID, Receipt: child.noticeID}, content)
			})
			childOutcome, err := tree.wait(ctx, child, 1000)
			tree.mu.Lock()
			leaf := ""
			if handback := child.reservation.handback; handback != nil && handback.After != nil {
				leaf = *handback.After
			}
			tree.mu.Unlock()
			if err != nil || childOutcome.State != Completed || childOutcome.Text != "CHILD_COMPLETED" || leaf == "" {
				t.Fatalf("descendant completion = %#v, %v", childOutcome, err)
			}
			entries, err := o.memberLogEntries(ctx, binding)
			if err != nil {
				t.Fatal(err)
			}
			audits := 0
			for _, raw := range entries {
				var entry completionEntry
				if json.Unmarshal(raw, &entry) == nil && entry.CustomType == "litter_completion" {
					audits++
				}
			}
			if audits != 1 {
				t.Fatalf("native descendant audit count = %d", audits)
			}
			root.mu.Lock()
			root.queueProvider = true
			root.mu.Unlock()
			select {
			case <-selected.done:
			case <-ctx.Done():
				t.Fatal("native member did not settle after descendant wait")
			}
			<-selected.executorDone
			if selected.outcome.State != Completed || selected.outcome.Text != "PROBE_TOOL_DONE" {
				t.Fatalf("native descendant parent outcome = %#v", selected.outcome)
			}
			if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, unrelated) || strings.Contains(panes, ownership.Pane.ID) {
				t.Fatalf("descendant member cleanup changed unrelated panes: %q", panes)
			}
			sessions, err := os.ReadDir(ownership.SessionDirectory)
			if err != nil {
				t.Fatal(err)
			}
			parentLogs, childLogs := 0, 0
			for _, session := range sessions {
				if strings.HasSuffix(session.Name(), "_"+string(memberID)+".jsonl") {
					parentLogs++
				}
				if strings.HasSuffix(session.Name(), "_"+string(child.ref.ID)+".jsonl") {
					childLogs++
				}
			}
			if parentLogs != 1 || childLogs != 1 {
				t.Fatalf("descendant session logs: parent=%d child=%d", parentLogs, childLogs)
			}
			data, _ := json.Marshal(map[string]any{"child": child.ref.ID, "outcome": childOutcome, "waitLeaf": leaf, "audits": audits})
			if err := os.WriteFile(filepath.Join(rootPath, "descendant.json"), data, 0o600); err != nil {
				t.Fatal(err)
			}
		} else {
			probeWait(t, ctx, "native seed or exit", func() bool {
				select {
				case <-selected.done:
					return true
				default:
					return probeCount(provider, "event", "queue_gate_wait") > 0
				}
			})
			if probeCount(provider, "event", "queue_gate_wait") == 0 {
				t.Fatalf("native member failed before provider request: %#v", selected.outcome)
			}
			tree.mu.Lock()
			ownership, binding := cloneMemberOwnership(selected.owner.nativeOwnership), selected.owner.native
			ready := binding != nil && binding.ready
			tree.mu.Unlock()
			if ownership == nil || ownership.Pane == nil || !ready {
				t.Fatal("provider ran without an installed, ready root handle")
			}
			observed = ownership
			tmux("pipe-pane", "-o", "-t", ownership.Pane.ID, "cat > "+filepath.Join(rootPath, "terminal.raw"))
			_ = os.WriteFile(filepath.Join(rootPath, "during.ansi"), []byte(tmux("capture-pane", "-p", "-e", "-t", ownership.Pane.ID)), 0o600)
			if name == "root_loss" {
				closed := make(chan struct{})
				go func() { tree.close(); close(closed) }()
				select {
				case <-closed:
				case <-ctx.Done():
					t.Fatal("root close did not release the native pane")
				}
				if _, err := processBirth(ownership.Pane.PID); !os.IsNotExist(err) {
					t.Fatalf("native process survived root close: %v", err)
				}
			} else {
				root.mu.Lock()
				root.queueProvider = true
				root.mu.Unlock()
				select {
				case <-selected.done:
				case <-ctx.Done():
					t.Fatal("native member did not settle")
				}
				<-selected.executorDone
				if selected.outcome.State != Completed || selected.outcome.Text != "PROBE_DONE" || !probeRequestContains(provider, "START_ONE") {
					t.Fatalf("native member outcome = %#v", selected.outcome)
				}
				tree.mu.Lock()
				retained := selected.owner.nativeOwnership
				tree.mu.Unlock()
				if retained != nil {
					t.Fatal("successful member kept native pane ownership")
				}
				entries, err := os.ReadDir(ownership.SessionDirectory)
				if err != nil || len(entries) != 1 || !strings.HasSuffix(entries[0].Name(), "_"+string(memberID)+".jsonl") {
					t.Fatalf("native session files = %#v, %v", entries, err)
				}
			}
			if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); !strings.Contains(panes, unrelated) || strings.Contains(panes, ownership.Pane.ID) {
				t.Fatalf("root lifetime cleanup changed unrelated panes: %q", panes)
			}
		}
		tree.mu.Lock()
		retained := selected.owner.nativeOwnership != nil
		tree.mu.Unlock()
		if name != "stubborn" && probeCount(provider, "event", "request") != 0 {
			if _, err := os.Stat(filepath.Join(rootPath, "wrong-home")); !os.IsNotExist(err) || !probeRequestContains(provider, filepath.Join(home, ".pig", "docs")) {
				t.Fatal("native member did not use the bound configuration root")
			}
		}
		_, roster, pending, err := store.SnapshotWithReservations()
		if err != nil {
			t.Fatal(err)
		}
		evidence := map[string]any{"outcome": selected.outcome, "unrelatedPane": unrelated, "remainingPanes": tmux("list-panes", "-a", "-F", "#{pane_id}"), "ownershipRetained": retained, "rosterCount": len(roster), "reservationCount": len(pending)}
		if observed != nil && observed.Pane != nil {
			birth, err := processBirth(observed.Pane.PID)
			evidence["memberPane"] = observed.Pane.ID
			evidence["memberPid"] = observed.Pane.PID
			evidence["memberBirth"] = observed.Pane.Birth
			evidence["processExited"] = os.IsNotExist(err) || err == nil && birth != observed.Pane.Birth
			evidence["sessionDirectory"] = observed.SessionDirectory
		}
		data, err := json.Marshal(evidence)
		if err != nil || os.WriteFile(filepath.Join(rootPath, "outcome.json"), data, 0o600) != nil {
			t.Fatal("write outcome evidence", err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method == "appendEntry" && name == "prelaunch_failure" {
			var request struct {
				Data savedChild `json:"data"`
			}
			if json.Unmarshal(raw, &request) == nil && request.Data.Native != nil && request.Data.Native.Pane == nil {
				if err := os.Chmod(filepath.Dir(request.Data.Native.Manifest), 0o500); err != nil {
					t.Error(err)
				}
			}
		}
		if method == "getModelRegistryState" {
			return json.RawMessage(`{"models":[{"provider":"fixture","id":"fixed"}],"providers":{"fixture":{"configured":true}},"registered":[]}`)
		}
		if method == "sendMessage" {
			return json.RawMessage(`{}`)
		}
		return host.call(t, method, raw)
	})
}
