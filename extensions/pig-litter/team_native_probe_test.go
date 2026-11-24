//go:build p08native && linux

package piglitter

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
)

type nativeProbeRequest struct {
	Kind   string         `json:"kind"`
	Token  string         `json:"token"`
	Detail map[string]any `json:"detail"`
}

type nativeProbeRoot struct {
	mu            sync.Mutex
	listener      net.Listener
	monitors      []net.Conn
	pending       []net.Conn
	token         string
	path          string
	launch        bool
	active        bool
	refuse        bool
	closed        bool
	childID       ChildID
	release       bool
	invoke        func(nativeProbeRequest) map[string]any
	settle        func(nativeProbeRequest) map[string]any
	settled       func(nativeProbeRequest) map[string]any
	stale         bool
	staleTask     bool
	queueProvider bool
	queueRelease  bool
	queueInput    bool
	next          int
}

type nativeProbeControl struct {
	root    *nativeProbeRoot
	events  string
	session ChildID
}

func (root *nativeProbeRoot) send(command map[string]any) (string, error) {
	root.mu.Lock()
	defer root.mu.Unlock()
	if root.closed || len(root.monitors) == 0 {
		return "", errors.New("native control is unavailable")
	}
	root.next++
	id := fmt.Sprintf("control-%d", root.next)
	command["id"] = id
	conn := root.monitors[len(root.monitors)-1]
	err := json.NewEncoder(conn).Encode(command)
	if err != nil {
		return "", err
	}
	return id, nil
}

func (control *nativeProbeControl) send(ctx context.Context, command map[string]any) error {
	id, err := control.root.send(command)
	if err != nil {
		return err
	}
	for {
		for _, row := range probeRows(control.events) {
			if row["event"] == "control_applied" && row["id"] == id {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func (control *nativeProbeControl) Queue(ctx context.Context, input memberInput) (memberInputDisposition, error) {
	var command map[string]any
	switch input := input.(type) {
	case memberCompletionInput:
		command = map[string]any{"kind": "completion", "content": input.content, "details": input.details}
	case memberTextInput:
		command = map[string]any{"kind": "queue", "content": input.text}
	default:
		return memberInputUncertain, errors.New("unsupported probe input")
	}
	err := control.send(ctx, command)
	if err != nil {
		return memberInputUncertain, err
	}
	return memberInputHandled, nil
}

func (control *nativeProbeControl) Snapshot(context.Context, memberHistoryBound) (memberSnapshot, error) {
	snapshot := memberSnapshot{session: control.session, phase: memberIdle}
	for _, row := range probeRows(control.events) {
		if row["event"] != "session_start" {
			continue
		}
		identity, _ := row["identity"].(map[string]any)
		if id, ok := identity["uuid"].(string); ok {
			snapshot.session = ChildID(id)
		}
		snapshot.cwd, _ = identity["cwd"].(string)
		model, _ := identity["model"].(map[string]any)
		snapshot.model = fmt.Sprint(model["provider"]) + "/" + fmt.Sprint(model["id"])
		snapshot.tools = nil
		if tools, ok := identity["tools"].([]any); ok {
			for _, tool := range tools {
				snapshot.tools = append(snapshot.tools, fmt.Sprint(tool))
			}
		}
	}
	return snapshot, nil
}

func (control *nativeProbeControl) RecordCompletion(ctx context.Context, audit completionAudit) error {
	return control.send(ctx, map[string]any{"kind": "audit", "audit": audit})
}

func (*nativeProbeControl) Cancel()                     {}
func (*nativeProbeControl) Focus(context.Context) error { return nil }

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
	if request.Kind == "litter_spawn" || request.Kind == "litter_wait" || request.Kind == "tool-end" {
		invoke := root.invoke
		root.mu.Unlock()
		answer := map[string]any{"kind": "deny"}
		if invoke != nil {
			answer = invoke(request)
		}
		_ = json.NewEncoder(conn).Encode(answer)
		conn.Close()
		return
	}
	if request.Kind == "settle" && root.settle != nil || request.Kind == "settled" && root.settled != nil {
		callback := root.settle
		if request.Kind == "settled" {
			callback = root.settled
		}
		root.mu.Unlock()
		_ = json.NewEncoder(conn).Encode(callback(request))
		conn.Close()
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
			answer["continue"] = true
		}
	}
	if request.Kind == "child-gate" && !root.release {
		answer["kind"] = "hold"
	}
	if request.Kind == "queue-provider" && !root.queueProvider || request.Kind == "queue-release" && !root.queueRelease {
		answer["kind"] = "hold"
	}
	if request.Kind == "queue-input" && !root.queueInput {
		answer["kind"] = "hold"
	}
	if request.Kind == "child-id" {
		answer["result"] = root.childID
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

func probeRequestContains(path, text string) bool {
	for _, row := range probeRows(path) {
		if row["event"] == "request" {
			body, _ := json.Marshal(row["body"])
			if strings.Contains(string(body), text) {
				return true
			}
		}
	}
	return false
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
	args := []string{"--session-id", "018f0000-0000-7000-8000-000000000002", "--session-dir", filepath.Join(rootPath, "sessions"), "--model", "fixture/fixed", "--tools", "read,ls,write,codemode,litter_spawn,litter_wait", "--no-extensions", "-e", "builtin:codemode", "--no-skills", "--no-prompt-templates", "--no-context-files", "--approve"}
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
	launch := tmuxLaunch{executable: probe, manifest: manifest, cwd: cwd, nonce: launchID}
	pane, err := server.launch(ctx, launch)
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
		var settled map[string]any
		for _, row := range probeRows(events) {
			if row["event"] == "native_settled" {
				settled = row
			}
		}
		decision, _ := settled["decision"].(map[string]any)
		if decision["kind"] == "allow" {
			boundary := settled["boundary"]
			probeWait(t, ctx, text+" root approval", func() bool {
				for _, row := range probeRows(events) {
					if row["event"] == "root_settle_approved" && row["boundary"] == boundary {
						return true
					}
				}
				return false
			})
		}
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
			if identity["uuid"] != "018f0000-0000-7000-8000-000000000002" || identity["cwd"] != cwd || model["provider"] != "fixture" || model["id"] != "fixed" || !slices.Equal(tools, []any{"read", "ls", "write", "codemode", "litter_spawn", "litter_wait"}) {
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
	probeWait(t, ctx, "reloaded root monitor", func() bool { root.mu.Lock(); defer root.mu.Unlock(); return len(root.monitors) >= 2 })
	var sessionFile string
	for _, row := range probeRows(events) {
		if row["event"] == "session_start" {
			identity, _ := row["identity"].(map[string]any)
			sessionFile, _ = identity["sessionFile"].(string)
		}
	}
	exerciseNativeDescendant(t, ctx, root, cwd, filepath.Join(rootPath, "agent"), filepath.Join(rootPath, "sessions"), sessionFile, server, launch, pane, turn)
	control := &nativeProbeControl{root: root, events: events, session: "018f0000-0000-7000-8000-000000000002"}
	gate := probeCount(providerLog, "event", "queue_gate_wait")
	settled := probeCount(events, "event", "native_settled")
	send("QUEUE_GATE")
	probeWait(t, ctx, "queue provider gate", func() bool { return probeCount(providerLog, "event", "queue_gate_wait") > gate })
	for index, text := range []string{"QUEUED_ONE", "QUEUED_TWO"} {
		disposition, err := control.Queue(ctx, memberTextInput{mode: "follow_up", text: text})
		if err != nil || disposition != memberInputHandled {
			t.Fatalf("queue %s = %v, %v", text, disposition, err)
		}
		if index == 0 {
			probeWait(t, ctx, text+" input hook", func() bool { return probeCount(events, "text", text) > 0 })
		}
		if err := control.send(ctx, map[string]any{"kind": "inspect"}); err != nil {
			t.Fatal(err)
		}
	}
	var queueObservations []map[string]any
	var pendingChecks []map[string]any
	for _, row := range probeRows(events) {
		if row["event"] == "control_applied" && row["kind"] == "queue" {
			queueObservations = append(queueObservations, row)
		} else if row["event"] == "control_applied" && row["kind"] == "inspect" {
			pendingChecks = append(pendingChecks, row)
		}
	}
	if len(queueObservations) != 2 || queueObservations[0]["idle"] != false || queueObservations[1]["idle"] != false || queueObservations[0]["after"] != false || queueObservations[1]["after"] != false || len(pendingChecks) != 2 || pendingChecks[0]["pending"] != false || pendingChecks[1]["pending"] != false {
		t.Fatalf("native pending observations = %#v, checks = %#v", queueObservations, pendingChecks)
	}
	root.mu.Lock()
	root.queueInput = true
	root.mu.Unlock()
	probeWait(t, ctx, "second queued input hook", func() bool { return probeCount(events, "text", "QUEUED_TWO") > 0 })
	root.mu.Lock()
	root.queueProvider = true
	root.mu.Unlock()
	probeWait(t, ctx, "queued messages observed", func() bool {
		return probeRequestContains(providerLog, "QUEUED_ONE") && probeRequestContains(providerLog, "QUEUED_TWO") && probeCount(events, "event", "native_settled") > settled
	})
	root.mu.Lock()
	root.queueProvider = false
	root.mu.Unlock()
	gate = probeCount(providerLog, "event", "queue_gate_wait")
	settled = probeCount(events, "event", "native_settled")
	send("QUEUE_GATE_STALE")
	probeWait(t, ctx, "stale queue provider gate", func() bool { return probeCount(providerLog, "event", "queue_gate_wait") > gate })
	queueID, err := root.send(map[string]any{"kind": "queue-hold", "content": "STALE_FOLLOW_UP"})
	if err != nil {
		t.Fatal(err)
	}
	probeWait(t, ctx, "stale queue snapshot", func() bool {
		for _, row := range probeRows(events) {
			if row["event"] == "queue_checked" && row["id"] == queueID && row["idle"] == false {
				return true
			}
		}
		return false
	})
	root.mu.Lock()
	root.queueProvider = true
	root.mu.Unlock()
	probeWait(t, ctx, "settlement after queue snapshot", func() bool { return probeCount(events, "event", "native_settled") > settled })
	root.mu.Lock()
	root.queueRelease = true
	root.mu.Unlock()
	probeWait(t, ctx, "stale queue started a turn", func() bool { return probeRequestContains(providerLog, "STALE_FOLLOW_UP") })
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

type nativeProbeCall struct {
	ticket *callTicket
	value  map[string]any
	err    error
}

func exerciseNativeDescendant(t *testing.T, ctx context.Context, root *nativeProbeRoot, cwd, agentDir, sessionDir, sessionFile string, server tmuxServer, launch tmuxLaunch, pane tmuxPane, turn func(string)) {
	t.Helper()
	rootID := ChildID("018f0000-0000-7000-8000-000000000001")
	memberID := ChildID("018f0000-0000-7000-8000-000000000002")
	tree := testTree(t, defaultConfig())
	tree.root.id = rootID
	store, err := openTeamStore(agentDir, rootID)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	member, err := store.Reserve(memberID, "018f0000-0000-7000-8000-000000000003", "member", WorkspacePolicy{Kind: "shared_checkout", Cwd: cwd}, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.createTask("Finish the review", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.claimTask(task.ID, member.Ref, false); err != nil {
		t.Fatal(err)
	}
	worker, _ := agentDefaults("worker")
	_, parent, err := tree.admit(admission{kind: "worker", name: "member", model: "fixture/fixed", cwd: cwd, task: "work", agent: worker, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	parent.calls = newCallScope()
	host := &runtimeHost{tools: []string{"read", "ls", "write", "codemode", "litter_spawn", "litter_wait"}}
	withCompletionHost(t, tree, func(o *owner) {
		trusted := true
		services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: cwd, AgentDir: agentDir, ProjectTrusted: &trusted})
		if err != nil {
			t.Fatal(err)
		}
		defer services.Close()
		o.cwd, o.agentDir, o.services = cwd, agentDir, services
		o.teamStore = store
		policyMarker := filepath.Join(root.path, "policy-ran")
		o.policy.TeammateIdle = []string{"/bin/sh", "-c", `if [ -e "$1" ]; then printf '{"permit":true}'; else : > "$1"; printf '{"permit":false,"feedback":"Finish the review."}'; fi`, "policy", policyMarker}
		tree.execute, tree.persist, tree.completion = o.executeRun, o.saveChild, o.sendCompletion
		control := &nativeProbeControl{root: root, events: filepath.Join(root.path, "events.jsonl"), session: memberID}
		ownership, err := o.reserveMemberLaunch(ctx, parent.owner, parent, server, launch, sessionDir)
		if err != nil {
			t.Fatal(err)
		}
		if err := o.recordMemberPane(ctx, parent.owner, parent, ownership, pane); err != nil {
			t.Fatal(err)
		}
		binding, err := o.installMemberHandle(ctx, parent.owner, parent, sessionDir, sessionFile, launch.nonce, control)
		if err != nil {
			t.Fatal(err)
		}
		var callsMu sync.Mutex
		calls := map[string]nativeProbeCall{}
		var child *run
		var callbackErr error
		var commits, stale atomic.Int64
		setError := func(err error) {
			callsMu.Lock()
			callbackErr = err
			callsMu.Unlock()
		}
		root.mu.Lock()
		root.settle = func(request nativeProbeRequest) map[string]any {
			boundary, _ := request.Detail["boundary"].(float64)
			report, _ := request.Detail["report"].(string)
			decision, err := o.decideMemberSettlement(ctx, binding, memberSettlementCandidate{boundary: uint64(boundary), report: report, completed: request.Detail["outcome"] == "completed", continuing: request.Detail["continuing"] == true})
			if err != nil {
				setError(err)
				return map[string]any{"kind": "deny", "boundary": boundary, "feedback": err.Error()}
			}
			if !decision.allow {
				return map[string]any{"kind": "deny", "boundary": boundary, "feedback": memberSettlementFeedback(decision), "continue": decision.continueRun}
			}
			root.mu.Lock()
			changeWake := root.stale
			changeTask := root.staleTask
			root.stale = false
			root.staleTask = false
			root.mu.Unlock()
			if changeWake {
				tree.mu.Lock()
				parent.owner.peerWakeVersion++
				tree.mu.Unlock()
			}
			if changeTask {
				tree.lifecycleMu.Lock()
				_, err := store.createTask("Changed during native settlement", "", nil, nil)
				tree.lifecycleMu.Unlock()
				if err != nil {
					setError(err)
					return map[string]any{"kind": "deny", "boundary": boundary, "feedback": err.Error()}
				}
			}
			return map[string]any{"kind": "allow", "boundary": boundary}
		}
		root.settled = func(request nativeProbeRequest) map[string]any {
			boundary, _ := request.Detail["boundary"].(float64)
			decision, err := o.finalizeMemberSettlement(binding, uint64(boundary), request.Detail["aborted"] == true, func() error {
				commits.Add(1)
				return nil
			})
			if err != nil {
				setError(err)
				return map[string]any{"kind": "deny", "feedback": err.Error()}
			}
			if !decision.allow {
				stale.Add(1)
				if request.Detail["aborted"] == true {
					return map[string]any{"kind": "deny"}
				}
				return map[string]any{"kind": "deny", "feedback": memberSettlementFeedback(decision), "continue": decision.continueRun}
			}
			return map[string]any{"kind": "allow"}
		}
		root.invoke = func(request nativeProbeRequest) map[string]any {
			callID, _ := request.Detail["call"].(string)
			if request.Kind == "tool-end" {
				callsMu.Lock()
				call, ok := calls[callID]
				delete(calls, callID)
				callsMu.Unlock()
				if !ok || request.Detail["name"] != call.ticket.operation || request.Detail["isError"] == true {
					setError(errors.New("native tool result did not match its root call"))
					return map[string]any{"kind": "deny"}
				}
				result, _ := request.Detail["result"].(map[string]any)
				blocks, _ := result["content"].([]any)
				var text string
				if len(blocks) == 1 {
					block, _ := blocks[0].(map[string]any)
					text, _ = block["text"].(string)
				}
				expected, _ := resultContent(call.ticket.operation, call.value, call.err)
				if text != expected {
					setError(errors.New("native tool result content changed before acknowledgement"))
					return map[string]any{"kind": "deny"}
				}
				o.finishCall(call.ticket, nativeResult(call.ticket.operation, call.value, call.err), false)
				return map[string]any{"kind": "allow"}
			}
			leaf, _ := request.Detail["leaf"].(string)
			ticket, err := o.memberCallTicket(binding, callID, request.Kind, leaf)
			if err != nil {
				setError(err)
				return map[string]any{"kind": "deny", "result": err.Error()}
			}
			raw, err := json.Marshal(request.Detail["args"])
			if err != nil {
				setError(err)
				return map[string]any{"kind": "deny", "result": err.Error()}
			}
			var value map[string]any
			if request.Kind == "litter_wait" {
				type result struct {
					value map[string]any
					err   error
				}
				done := make(chan result, 1)
				go func() {
					value, err := o.operation(ctx, raw, ticket, nil)
					done <- result{value: value, err: err}
				}()
				for {
					o.ackMu.Lock()
					pending := o.outcomeAcks[ticket] != nil
					o.ackMu.Unlock()
					if pending {
						root.mu.Lock()
						root.release = true
						root.mu.Unlock()
						break
					}
					select {
					case result := <-done:
						setError(result.err)
						return map[string]any{"kind": "deny", "result": "wait ended before acknowledgement"}
					case <-ctx.Done():
						setError(ctx.Err())
						return map[string]any{"kind": "deny", "result": ctx.Err().Error()}
					case <-time.After(10 * time.Millisecond):
					}
				}
				reply := <-done
				value, err = reply.value, reply.err
			} else {
				value, err = o.operation(ctx, raw, ticket, nil)
				if err == nil {
					spawned := value["child"].(Snapshot)
					selected, selectErr := tree.selectRun(spawned.ID)
					err = selectErr
					callsMu.Lock()
					child = selected
					callsMu.Unlock()
					root.mu.Lock()
					root.childID = spawned.ID
					root.mu.Unlock()
				}
			}
			if err != nil {
				setError(err)
			}
			callsMu.Lock()
			calls[callID] = nativeProbeCall{ticket: ticket, value: value, err: err}
			callsMu.Unlock()
			content, failed := resultContent(request.Kind, value, err)
			kind := "allow"
			if failed {
				kind = "deny"
			}
			return map[string]any{"kind": kind, "result": content}
		}
		root.mu.Unlock()
		defer func() { root.mu.Lock(); root.invoke, root.settle, root.settled = nil, nil, nil; root.mu.Unlock() }()
		providerLog := filepath.Join(root.path, "provider.jsonl")
		beforePolicy := probeCount(providerLog, "event", "request")
		turn("native policy check")
		if got := probeCount(providerLog, "event", "request") - beforePolicy; got < 2 {
			t.Fatalf("root idle refusal did not continue: %d provider requests", got)
		}
		root.mu.Lock()
		root.stale = true
		root.mu.Unlock()
		beforeStale := probeCount(filepath.Join(root.path, "events.jsonl"), "event", "native_settled")
		turn("native wake race")
		probeWait(t, ctx, "stale native continuation", func() bool {
			return stale.Load() == 1 && probeCount(filepath.Join(root.path, "events.jsonl"), "event", "native_settled") >= beforeStale+2
		})
		if commits.Load() < 2 {
			t.Fatalf("root committed %d approved settlements", commits.Load())
		}
		root.mu.Lock()
		root.staleTask = true
		root.mu.Unlock()
		beforeStale = probeCount(filepath.Join(root.path, "events.jsonl"), "event", "native_settled")
		turn("native task revision race")
		probeWait(t, ctx, "stale task continuation", func() bool {
			return stale.Load() == 2 && probeCount(filepath.Join(root.path, "events.jsonl"), "event", "native_settled") >= beforeStale+2
		})
		turn("case spawn-child")
		probeWait(t, ctx, "embedded descendant provider request", func() bool {
			return probeCount(filepath.Join(root.path, "root.jsonl"), "kind", "child-gate") > 0
		})
		turn("case wait-child")
		callsMu.Lock()
		currentChild, currentErr := child, callbackErr
		callsMu.Unlock()
		if currentErr != nil {
			t.Fatal(currentErr)
		}
		if currentChild == nil {
			t.Fatal("embedded descendant was not retained")
		}
		outcome, err := tree.wait(ctx, currentChild, 1000)
		if err != nil || outcome.State != Completed || outcome.Text != "CHILD_COMPLETED" {
			t.Fatalf("embedded descendant outcome = %#v, %v", outcome, err)
		}
		probeWait(t, ctx, "native wait audit", func() bool {
			_ = o.auditCompletion(currentChild)
			entries, err := o.memberLogEntries(ctx, binding)
			return err == nil && completionStored(entries, completionDetails{ChildID: currentChild.ref.ID, Receipt: currentChild.noticeID}, currentChild.reservation.content)
		})
		if currentChild.reservation.handback == nil || currentChild.reservation.handback.After == nil || *currentChild.reservation.handback.After == "" {
			t.Fatalf("native wait lost its captured leaf: %#v", currentChild.reservation.handback)
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
			t.Fatalf("native wait audit count = %d", audits)
		}
		turn("case spawn-notice")
		callsMu.Lock()
		noticeChild, currentErr := child, callbackErr
		callsMu.Unlock()
		if currentErr != nil || noticeChild == nil || noticeChild == currentChild {
			t.Fatalf("automatic completion child = %p, %v", noticeChild, currentErr)
		}
		noticeOutcome, err := tree.wait(ctx, noticeChild, 10000)
		if err != nil || noticeOutcome.State != Completed || noticeOutcome.Text != "PROBE_DONE" {
			t.Fatalf("automatic child outcome = %#v, %v", noticeOutcome, err)
		}
		noticeContent, err := completionContent(noticeOutcome)
		if err != nil {
			t.Fatal(err)
		}
		details := completionDetails{ChildID: noticeChild.ref.ID, Receipt: noticeChild.noticeID}
		probeWait(t, ctx, "native automatic completion", func() bool {
			entries, err := o.memberLogEntries(ctx, binding)
			return err == nil && completionStored(entries, details, noticeContent)
		})
		o.confirmCompletions(parent.owner)
		entries, err = o.memberLogEntries(ctx, binding)
		if err != nil {
			t.Fatal(err)
		}
		notices := 0
		for _, raw := range entries {
			var entry completionEntry
			if json.Unmarshal(raw, &entry) == nil {
				if entry.CustomType == "litter_completion" && entry.Details == details && entry.Content == noticeContent {
					notices++
				} else if entry.Message != nil && entry.Message.CustomType == "litter_completion" && entry.Message.Details == details {
					notices++
				}
			}
		}
		if notices != 1 {
			t.Fatalf("native automatic completion count = %d", notices)
		}
		if err := os.WriteFile(filepath.Join(root.path, "descendant.json"), []byte(fmt.Sprintf(`{"child":%q,"state":%q,"report":%q,"waitLeaf":%q,"audits":%d,"noticeChild":%q,"notices":%d}`, currentChild.ref.ID, outcome.State, outcome.Text, *currentChild.reservation.handback.After, audits, noticeChild.ref.ID, notices)), 0o600); err != nil {
			t.Fatal(err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method == "getModelRegistryState" {
			return json.RawMessage(`{"models":[{"provider":"fixture","id":"fixed"}],"providers":{"fixture":{"configured":true}},"registered":[]}`)
		}
		return host.call(t, method, raw)
	})
}
