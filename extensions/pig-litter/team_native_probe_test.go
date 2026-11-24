//go:build p08native && linux

package piglitter

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type nativeProbeRequest struct {
	Kind   string         `json:"kind"`
	Token  string         `json:"token"`
	Detail map[string]any `json:"detail"`
}

type nativeProbeRoot struct {
	mu       sync.Mutex
	listener net.Listener
	monitors []net.Conn
	pending  []net.Conn
	token    string
	path     string
	launch   bool
	active   bool
	refuse   bool
	closed   bool
}

func (root *nativeProbeRoot) serve() {
	for {
		conn, err := root.listener.Accept()
		if err != nil {
			return
		}
		go root.handle(conn)
	}
}

func (root *nativeProbeRoot) handle(conn net.Conn) {
	var request nativeProbeRequest
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&request); err != nil || request.Token != root.token {
		conn.Close()
		return
	}
	root.mu.Lock()
	if root.closed {
		root.mu.Unlock()
		conn.Close()
		return
	}
	file, _ := os.OpenFile(filepath.Join(root.path, "root.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if file != nil {
		_ = json.NewEncoder(file).Encode(map[string]any{"kind": request.Kind, "detail": request.Detail})
		file.Close()
	}
	if request.Kind == "monitor" {
		root.monitors = append(root.monitors, conn)
		root.mu.Unlock()
		return
	}
	if request.Kind == "tool" && strings.HasSuffix(fmt.Sprint(request.Detail["path"]), "direct-write-lost.txt") {
		root.pending = append(root.pending, conn)
		root.mu.Unlock()
		return
	}
	answer := map[string]any{"kind": "hold", "revision": "root-revision-1"}
	if request.Kind == "launch" && root.launch || request.Kind != "launch" && root.active {
		answer["kind"] = "allow"
	}
	if request.Kind == "tool" && strings.HasSuffix(fmt.Sprint(request.Detail["path"]), "-deny.txt") {
		answer["kind"] = "deny"
	}
	if request.Kind == "settle" {
		answer["boundary"] = request.Detail["boundary"]
		if root.refuse {
			root.refuse = false
			answer["kind"] = "deny"
		}
	}
	root.mu.Unlock()
	_ = json.NewEncoder(conn).Encode(answer)
	conn.Close()
}

func (root *nativeProbeRoot) disconnect() {
	root.mu.Lock()
	root.closed = true
	_ = root.listener.Close()
	for _, conn := range root.monitors {
		_ = conn.Close()
	}
	for _, conn := range root.pending {
		_ = conn.Close()
	}
	root.mu.Unlock()
}

func probeRows(path string) []map[string]any {
	data, _ := os.ReadFile(path)
	var rows []map[string]any
	for _, line := range strings.Split(string(data), "\n") {
		var row map[string]any
		if json.Unmarshal([]byte(line), &row) == nil {
			rows = append(rows, row)
		}
	}
	return rows
}

func probeCount(path, field, value string) int {
	count := 0
	for _, row := range probeRows(path) {
		if row[field] == value {
			count++
		}
	}
	return count
}

func probeWait(t *testing.T, ctx context.Context, label string, predicate func() bool) {
	t.Helper()
	for !predicate() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", label)
		case <-time.After(20 * time.Millisecond):
		}
	}
}

func TestTmuxNativeProbe(t *testing.T) {
	probe := os.Getenv("P08_NATIVE_PROBE")
	rootPath := os.Getenv("P08_LIVE_ROOT")
	source := os.Getenv("P08_SOURCE_ROOT")
	if probe == "" || rootPath == "" || source == "" {
		t.Fatal("P08_NATIVE_PROBE, P08_LIVE_ROOT, and P08_SOURCE_ROOT are required")
	}
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	cwd := filepath.Join(rootPath, `cwd q's "$;[]`)
	for _, directory := range []string{cwd, filepath.Join(rootPath, "home"), filepath.Join(rootPath, "agent"), filepath.Join(rootPath, "sessions"), filepath.Join(rootPath, "files")} {
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
	root := &nativeProbeRoot{listener: listener, token: hex.EncodeToString(nonce[:]), path: rootPath}
	go root.serve()
	defer root.disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
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
	providerLog := filepath.Join(rootPath, "provider.jsonl")
	probeWait(t, ctx, "fixture listener", func() bool { return probeCount(providerLog, "event", "listening") != 0 })
	var address string
	for _, row := range probeRows(providerLog) {
		if row["event"] == "listening" {
			address, _ = row["address"].(string)
		}
	}
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
	tmux("new-session", "-d", "-s", "probe", "-x", "160", "-y", "50", "sleep", "600")
	defer func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() }()
	tmux("set-option", "-w", "-t", "probe:0", "remain-on-exit", "on")
	identity := strings.Split(tmux("display-message", "-p", "-t", "probe:0.0", "#{pid}\t#{pane_id}"), "\t")
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
	environment := map[string]string{"PATH": os.Getenv("PATH"), "TERM": "xterm-256color", "LANG": "C.UTF-8", "HOME": filepath.Join(rootPath, "home"), "PIG_HOME": filepath.Join(rootPath, "home", ".pig"), "PIG_CODING_AGENT_DIR": filepath.Join(rootPath, "agent"), "PIG_SDK_GO_ROOT": source, "PROBE_ROOT": rootPath, "PROBE_TOKEN": root.token, "TMPDIR": rootPath, "PI_SKIP_VERSION_CHECK": "1"}
	args := []string{"--session-id", "70800000-0000-4000-8000-000000000001", "--session-dir", filepath.Join(rootPath, "sessions"), "--model", "fixture/fixed", "--tools", "write,codemode", "--no-extensions", "-e", "builtin:codemode", "--no-skills", "--no-prompt-templates", "--no-context-files", "--approve"}
	var launchNonce [16]byte
	if _, err := rand.Read(launchNonce[:]); err != nil {
		t.Fatal(err)
	}
	launchID := hex.EncodeToString(launchNonce[:])
	manifest := filepath.Join(rootPath, "member-"+launchID+".json")
	data, err := json.Marshal(map[string]any{"cwd": cwd, "env": environment, "args": args})
	if err != nil || os.WriteFile(manifest, data, 0o600) != nil {
		t.Fatal("write member manifest", err)
	}
	pane, err := server.launch(ctx, tmuxLaunch{executable: probe, manifest: manifest, cwd: cwd, nonce: launchID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pane.close(context.Background()) }()
	tmux("pipe-pane", "-o", "-t", pane.id, "cat > "+filepath.Join(rootPath, "terminal.raw")+"; printf eof > "+filepath.Join(rootPath, "terminal.eof"))
	_ = os.WriteFile(filepath.Join(rootPath, "process.json"), []byte(fmt.Sprintf(`{"serverPid":%d,"pane":%q,"panePid":%d,"birth":%q,"cwd":%q}`, server.pid, pane.id, pane.pid, pane.birth, cwd)), 0o600)
	root.mu.Lock()
	root.launch = true
	root.mu.Unlock()
	events := filepath.Join(rootPath, "events.jsonl")
	probeWait(t, ctx, "native session start", func() bool { return probeCount(events, "event", "session_start") > 0 })
	probeWait(t, ctx, "root monitor", func() bool { root.mu.Lock(); defer root.mu.Unlock(); return len(root.monitors) > 0 })
	capture := func(name string) {
		_ = os.WriteFile(filepath.Join(rootPath, name+".ansi"), []byte(tmux("capture-pane", "-p", "-e", "-t", pane.id)), 0o600)
	}
	send := func(text string) {
		file, err := os.OpenFile(filepath.Join(rootPath, "inputs.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewEncoder(file).Encode(map[string]any{"text": text, "keys": []string{"literal", "Enter"}}); err != nil {
			file.Close()
			t.Fatal(err)
		}
		if err := file.Close(); err != nil {
			t.Fatal(err)
		}
		tmux("send-keys", "-t", pane.id, "-l", "--", text)
		tmux("send-keys", "-t", pane.id, "Enter")
	}
	capture("before")
	send("held baseline")
	probeWait(t, ctx, "held input", func() bool { return probeCount(events, "event", "input") > 0 })
	if n := probeCount(providerLog, "event", "request"); n != 0 {
		t.Fatalf("held member made %d provider requests", n)
	}
	root.mu.Lock()
	root.active = true
	root.mu.Unlock()
	turn := func(text string) {
		t.Helper()
		before := probeCount(events, "event", "native_settled")
		send(text)
		probeWait(t, ctx, text+" settlement", func() bool { return probeCount(events, "event", "native_settled") > before })
	}
	turn("baseline fixed")
	turn("case direct-write-allow")
	turn("case direct-write-deny")
	turn("case codemode-write-allow")
	turn("case codemode-write-deny")
	for name, exists := range map[string]bool{"direct-write-allow.txt": true, "direct-write-deny.txt": false, "codemode-write-allow.txt": true, "codemode-write-deny.txt": false} {
		_, err := os.Stat(filepath.Join(rootPath, "files", name))
		if (err == nil) != exists {
			t.Fatalf("%s exists=%t, expected %t: %v", name, err == nil, exists, err)
		}
	}
	send("!touch " + filepath.Join(rootPath, "files", "bash-denied"))
	probeWait(t, ctx, "terminal bash veto", func() bool { return probeCount(events, "event", "user_bash") > 0 })
	if _, err := os.Stat(filepath.Join(rootPath, "files", "bash-denied")); !os.IsNotExist(err) {
		t.Fatalf("terminal bash created a file: %v", err)
	}
	started := probeCount(events, "event", "session_start")
	send("/reload")
	probeWait(t, ctx, "reload", func() bool { return probeCount(events, "event", "session_start") > started })
	denials := 0
	for _, row := range probeRows(events) {
		if row["event"] == "tool_call" && row["denied"] == true {
			denials++
		}
	}
	turn("case direct-write-deny")
	newDenials := 0
	for _, row := range probeRows(events) {
		if row["event"] == "tool_call" && row["denied"] == true {
			newDenials++
		}
	}
	if newDenials != denials+1 {
		t.Fatal("reload did not preserve the write denial")
	}
	root.mu.Lock()
	root.refuse = true
	root.mu.Unlock()
	before := probeCount(providerLog, "event", "request")
	turn("settle continuation")
	if got := probeCount(providerLog, "event", "request") - before; got < 2 {
		t.Fatalf("settlement refusal did not continue: %d provider requests", got)
	}
	starts, nestedDenial, refused, continued := 0, false, false, false
	var refusedBoundary float64
	for _, row := range probeRows(events) {
		switch row["event"] {
		case "session_start":
			starts++
			identity, _ := row["identity"].(map[string]any)
			model, _ := identity["model"].(map[string]any)
			tools, _ := identity["tools"].([]any)
			if identity["uuid"] != "70800000-0000-4000-8000-000000000001" || identity["cwd"] != cwd || model["provider"] != "fixture" || model["id"] != "fixed" || !slices.Equal(tools, []any{"write", "codemode"}) {
				t.Fatalf("unexpected native identity: %#v", identity)
			}
		case "tool_call":
			if row["tool"] == "write" && row["parent"] == "probe_call" && row["denied"] == true {
				nestedDenial = true
			}
		case "before_settle":
			decision, _ := row["decision"].(map[string]any)
			boundary, _ := decision["boundary"].(float64)
			if decision["kind"] == "deny" {
				refused, refusedBoundary = true, boundary
			} else if refused && decision["kind"] == "allow" && boundary > refusedBoundary {
				continued = true
			}
		}
	}
	if starts != 2 || !nestedDenial || !refused || !continued {
		t.Fatalf("native observations: starts=%d nestedDenial=%t refused=%t continued=%t", starts, nestedDenial, refused, continued)
	}
	for _, row := range probeRows(providerLog) {
		if row["event"] == "request" {
			body, _ := row["body"].(map[string]any)
			if body["model"] != "fixed" {
				t.Fatalf("unexpected provider model: %#v", body["model"])
			}
		}
	}
	capture("after")
	approved := probeCount(events, "event", "root_settle_approved")
	send("case direct-write-lost")
	probeWait(t, ctx, "held tool authorization", func() bool {
		root.mu.Lock()
		defer root.mu.Unlock()
		return len(root.pending) != 0
	})
	root.disconnect()
	probeWait(t, ctx, "root loss observed", func() bool { return probeCount(events, "event", "root_lost") > 0 })
	probeWait(t, ctx, "native process exit", func() bool { birth, err := processBirth(pane.pid); return err != nil || birth != pane.birth })
	probeWait(t, ctx, "native pane exit status", func() bool {
		return tmux("display-message", "-p", "-t", pane.id, "#{pane_dead} #{pane_dead_status}") == "1 0"
	})
	probeWait(t, ctx, "terminal recorder EOF", func() bool { _, err := os.Stat(filepath.Join(rootPath, "terminal.eof")); return err == nil })
	if _, err := os.Stat(filepath.Join(rootPath, "files", "direct-write-lost.txt")); !os.IsNotExist(err) {
		t.Fatalf("root loss allowed the pending write: %v", err)
	}
	if got := probeCount(events, "event", "root_settle_approved"); got != approved {
		t.Fatalf("root loss approved a new settlement: before=%d after=%d", approved, got)
	}
	blockedAfterLoss := false
	for _, row := range probeRows(events) {
		input, _ := row["input"].(map[string]any)
		if row["event"] == "tool_call" && strings.HasSuffix(fmt.Sprint(input["path"]), "direct-write-lost.txt") && row["denied"] == true {
			blockedAfterLoss = true
		}
	}
	if !blockedAfterLoss {
		t.Fatal("root loss did not return an explicit tool block")
	}
	if err := pane.close(ctx); err != nil {
		t.Fatal(err)
	}
	panes := tmux("list-panes", "-a", "-F", "#{pane_id}")
	if !strings.Contains(panes, unrelated) || strings.Contains(panes, pane.id) {
		t.Fatalf("unexpected panes after root loss: %q", panes)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "result.json"), []byte(`{"passed":true,"scope":"private native hook and root-loss prototype; product team integration not exercised"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestTmuxNativeStartupRootLoss(t *testing.T) {
	probe := os.Getenv("P08_NATIVE_PROBE")
	rootPath := os.Getenv("P08_LIVE_ROOT") + "-startup"
	if probe == "" || rootPath == "-startup" {
		t.Fatal("P08_NATIVE_PROBE and P08_LIVE_ROOT are required")
	}
	if err := os.Mkdir(rootPath, 0o700); err != nil {
		t.Fatal(err)
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("unix", filepath.Join(rootPath, "root.sock"))
	if err != nil {
		t.Fatal(err)
	}
	root := &nativeProbeRoot{listener: listener, token: hex.EncodeToString(nonce[:]), path: rootPath}
	go root.serve()
	defer root.disconnect()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	socket := filepath.Join(rootPath, "tmux.sock")
	tmux := func(args ...string) string {
		t.Helper()
		output, err := exec.CommandContext(ctx, "tmux", append([]string{"-S", socket}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("tmux %q: %v: %s", args, err, output)
		}
		return strings.TrimSuffix(string(output), "\n")
	}
	tmux("new-session", "-d", "-s", "startup", "-x", "120", "-y", "40", "sleep", "600")
	defer func() { _ = exec.Command("tmux", "-S", socket, "kill-server").Run() }()
	tmux("set-option", "-w", "-t", "startup:0", "remain-on-exit", "on")
	identity := strings.Split(tmux("display-message", "-p", "-t", "startup:0.0", "#{pid}\t#{pane_id}"), "\t")
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
	var launchNonce [16]byte
	if _, err := rand.Read(launchNonce[:]); err != nil {
		t.Fatal(err)
	}
	launchID := hex.EncodeToString(launchNonce[:])
	manifest := filepath.Join(rootPath, "member-"+launchID+".json")
	data, err := json.Marshal(map[string]any{"cwd": rootPath, "env": map[string]string{"PROBE_ROOT": rootPath, "PROBE_TOKEN": root.token}, "args": []string{}})
	if err != nil || os.WriteFile(manifest, data, 0o600) != nil {
		t.Fatal("write member manifest", err)
	}
	pane, err := server.launch(ctx, tmuxLaunch{executable: probe, manifest: manifest, cwd: rootPath, nonce: launchID})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pane.close(context.Background()) }()
	identityData, err := json.Marshal(map[string]any{"serverPid": server.pid, "pane": pane.id, "panePid": pane.pid, "birth": pane.birth, "cwd": rootPath})
	if err != nil || os.WriteFile(filepath.Join(rootPath, "process.json"), identityData, 0o600) != nil {
		t.Fatal("write startup process identity", err)
	}
	probeWait(t, ctx, "held launch", func() bool { return probeCount(filepath.Join(rootPath, "root.jsonl"), "kind", "launch") > 0 })
	root.disconnect()
	probeWait(t, ctx, "startup exit", func() bool {
		return tmux("display-message", "-p", "-t", pane.id, "#{pane_dead} #{pane_dead_status}") == "1 1"
	})
	if err := pane.close(ctx); err != nil {
		t.Fatal(err)
	}
	if panes := tmux("list-panes", "-a", "-F", "#{pane_id}"); panes != server.target {
		t.Fatalf("startup rollback changed the lead pane: %q", panes)
	}
	if err := os.WriteFile(filepath.Join(rootPath, "result.json"), []byte(`{"passed":true,"exit":1,"scope":"private held-launch root-loss prototype"}`), 0o600); err != nil {
		t.Fatal(err)
	}
}
