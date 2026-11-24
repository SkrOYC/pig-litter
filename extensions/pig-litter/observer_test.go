package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

func TestEmbeddedObserverReportsAndIsolation(t *testing.T) {
	for _, target := range []string{"worker", "coordinator"} {
		t.Run(target, func(t *testing.T) { testEmbeddedObserver(t, target, false, false, false, false) })
	}
	t.Run("silence", func(t *testing.T) { testEmbeddedObserver(t, "worker", true, false, false, false) })
	t.Run("team_observer_worker", func(t *testing.T) { testEmbeddedObserver(t, "worker", false, true, false, false) })
	t.Run("team_observer_coordinator", func(t *testing.T) { testEmbeddedObserver(t, "coordinator", false, true, false, false) })
	t.Run("observed_teammate", func(t *testing.T) { testEmbeddedObserver(t, "worker", false, false, true, false) })
	t.Run("changed_activity", func(t *testing.T) { testEmbeddedObserver(t, "worker", false, false, false, true) })
}

func testEmbeddedObserver(t *testing.T, target string, silent, team, teamWorker, activity bool) {
	t.Helper()
	t.Setenv("PIG_OFFLINE", "1")
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	if err := os.WriteFile(filepath.Join(dir, "litter.yaml"), []byte("agents:\n  observer:\n    role: observer\n    instructions: Observe only structural activity.\n    tools: [read, ls]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("PROJECT_SECRET_NEVER_EXPOSE"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "secret.txt"), []byte("FILESYSTEM_SECRET_NEVER_EXPOSE"), 0o600); err != nil {
		t.Fatal(err)
	}
	workerStarted := make(chan struct{})
	workerRelease := make(chan struct{})
	activityStart := make(chan struct{})
	activityDone := make(chan struct{})
	var release sync.Once
	defer release.Do(func() { close(workerRelease) })
	var requestsMu sync.Mutex
	var observerRequests []string
	var observerCalls atomic.Int32
	var workerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if strings.Contains(string(body), "WORKER_PRIVATE_TASK") {
			workerCall := workerCalls.Add(1)
			if activity && workerCall <= 2 {
				if workerCall == 1 {
					close(workerStarted)
					select {
					case <-activityStart:
					case <-request.Context().Done():
						return
					}
				}
				runtimeToolAnswer(w, fmt.Sprintf("activity-read-%d", workerCall), "ls", map[string]any{"path": dir})
				return
			}
			if activity && workerCall == 3 {
				close(activityDone)
			}
			if teamWorker && workerCall == 1 {
				runtimeToolAnswer(w, "team-worker-read", "ls", map[string]any{"path": dir})
				return
			}
			select {
			case <-workerStarted:
			default:
				close(workerStarted)
			}
			select {
			case <-workerRelease:
			case <-request.Context().Done():
				return
			}
			runtimeAnswer(w, "ordinary worker result")
			return
		}
		requestsMu.Lock()
		observerRequests = append(observerRequests, string(body))
		requestsMu.Unlock()
		call := observerCalls.Add(1)
		if activity && call == 1 {
			close(activityStart)
			select {
			case <-activityDone:
			case <-request.Context().Done():
				return
			}
			time.Sleep(1200 * time.Millisecond)
		}
		if silent {
			runtimeAnswer(w, "OBSERVER_FINAL_PRIVATE_SENTINEL")
			return
		}
		switch call {
		case 1:
			runtimeToolAnswer(w, "list-activity", "observer_ls", map[string]any{"path": "/observer"})
		case 2:
			runtimeToolAnswer(w, "read-activity", "observer_read", map[string]any{"path": "/observer/activity.json"})
		case 3:
			runtimeToolAnswer(w, "report-once", "observer_report", map[string]any{"reportId": "report-1", "text": "PAIRED_REPORT_BODY"})
		case 4:
			runtimeToolAnswer(w, "report-again", "observer_report", map[string]any{"reportId": "report-1", "text": "PAIRED_REPORT_BODY"})
		case 5:
			runtimeRawObserverCall(w, "duplicate-fields", `{"reportId":"duplicate","text":"first","text":"MALFORMED_DUPLICATE_BODY"}`)
		case 6:
			runtimeToolAnswer(w, "conflicting-id", "observer_report", map[string]any{"reportId": "report-1", "text": "DIFFERENT_CONFLICT_BODY"})
		case 7:
			runtimeRawObserverCall(w, "extra-recipient", `{"reportId":"extra","text":"INVALID_EXTRA_BODY","recipient":"worker"}`)
		case 8:
			runtimeRawObserverCall(w, "trailing-value", `{"reportId":"trailing","text":"INVALID_TRAILING_BODY"} {}`)
		case 9:
			runtimeToolAnswer(w, "forbidden-read", "observer_read", map[string]any{"path": filepath.Join(dir, "secret.txt")})
		case 10:
			runtimeToolAnswer(w, "forbidden-bash", "bash", map[string]any{"command": "touch " + filepath.Join(dir, "forbidden-side-effect")})
		default:
			runtimeAnswer(w, "OBSERVER_FINAL_PRIVATE_SENTINEL")
		}
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: dir, AgentDir: agentDir})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	rootSession, err := runtime.New(coding.SessionStartOptions{SessionDir: filepath.Join(dir, "root-sessions"), Model: services.ModelRuntime().GetModel("fixture", "child"), ResourceLoader: coding.NoResources, SkipBuiltinTools: true})
	if err != nil {
		t.Fatal(err)
	}
	defer rootSession.Close()
	go func() {
		for event := range rootSession.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	manager := rootSession.SessionManager()
	if _, err := manager.AppendMessage(agent.AgentMessage{User: &agent.UserMessage{Role: "user", Content: ai.UserContentBlocks{ai.TextContent{Text: "root task"}}}}); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, config)
	tree.root.id = ChildID(manager.ID())
	host := &runtimeHost{tools: append([]string{"read", "ls", "write", "edit", "bash", "codemode", "tool_search"}, controlNames...)}
	withCompletionHost(t, tree, func(o *owner) {
		o.sessionID, o.cwd, o.agentDir, o.services = manager.ID(), dir, agentDir, services
		tree.execute, tree.persist, tree.completion = o.executeRun, o.saveChild, o.sendCompletion
		tree.observerArm, tree.observerRetire = o.armObserver, o.retireObservers
		tree.teamFinished, tree.teamCanceled, tree.teamPaused = o.finishTeamMember, o.cancelTeamMember, o.pauseTeamMember
		tree.teamSettled = o.settleShutdown
		defer o.state.retire()
		teamAdmission := func(callID string, args map[string]any) (map[string]any, *callTicket) {
			ticket, err := o.callScope(nil).begin(nil, callID, "team_spawn")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(args)
			value, err := o.operation(context.Background(), raw, ticket, &o.context)
			if err != nil {
				t.Fatal(err)
			}
			content, _ := resultContent("team_spawn", value, nil)
			if _, err := manager.AppendMessage(agent.AgentMessage{ToolResult: &agent.ToolResultMessage{Role: "toolResult", ToolCallID: callID, ToolName: "team_spawn", Content: []ai.ToolResultMessageContent{ai.TextContent{Text: content}}}}); err != nil {
				t.Fatal(err)
			}
			return value, ticket
		}
		workerArgs := map[string]any{"type": "scout", "task": "WORKER_PRIVATE_TASK", "model": "fixture/child"}
		var workerValue map[string]any
		var workerTicket *callTicket
		var worker *run
		if teamWorker {
			workerArgs["name"] = "observed-worker"
			workerArgs["workspace"] = map[string]any{"kind": "shared_checkout"}
			workerValue, workerTicket = teamAdmission("worker-spawn", workerArgs)
			worker = testRun(t, tree, Ref{ID: workerValue["member"].(MemberRef).ID})
		} else {
			workerValue, workerTicket, err = runtimeOperation(t, o, "litter_spawn", "worker-spawn", workerArgs)
			if err != nil {
				t.Fatal(err)
			}
			worker = testRun(t, tree, workerValue["child"].(Snapshot).Ref)
		}
		o.settleAck(workerTicket, false)
		select {
		case <-workerStarted:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not reach provider")
		}
		args := map[string]any{"type": "observer", "task": "OBSERVER_TASK", "model": "fixture/child", "observe": map[string]any{"id": worker.ref.ID, "reportTo": target}}
		var value map[string]any
		var ticket *callTicket
		var observer *run
		if team {
			args["name"] = "activity-observer"
			args["workspace"] = map[string]any{"kind": "shared_checkout"}
			value, ticket = teamAdmission("observer-spawn", args)
			member := value["member"].(MemberRef)
			observer = testRun(t, tree, Ref{ID: member.ID})
		} else {
			value, ticket, err = runtimeOperation(t, o, "litter_spawn", "observer-spawn", args)
			if err != nil {
				t.Fatal(err)
			}
			observer = testRun(t, tree, value["child"].(Snapshot).Ref)
		}
		if _, err := tree.wait(context.Background(), observer, 1); err == nil {
			t.Fatal("observer started before admission acknowledgement")
		}
		o.settleAck(ticket, false)
		outcome, err := tree.wait(context.Background(), observer, 5000)
		if err != nil || outcome.Text != "" || strings.Contains(outcome.Error, "OBSERVER_FINAL_PRIVATE_SENTINEL") {
			t.Fatalf("observer settlement leaked text: %#v %v", outcome, err)
		}
		if silent && outcome.State != Completed {
			t.Fatalf("silent observer did not complete: %#v", outcome)
		}
		if _, _, err := tree.message(context.Background(), nil, observer, "resume", "steer observer"); err == nil {
			t.Fatal("observer accepted ordinary resume input")
		}
		if _, err := os.Stat(filepath.Join(dir, "forbidden-side-effect")); !os.IsNotExist(err) {
			t.Fatalf("forbidden bash changed the filesystem: %v", err)
		}
		if !silent && target == "worker" {
			entries, _ := rawEntries(manager.GetEntries())
			if strings.Contains(string(mustJSON(t, entries)), "PAIRED_REPORT_BODY") {
				t.Fatal("worker-targeted report reached the coordinator")
			}
		}
		if team {
			_, err := o.addressedSend(context.Background(), json.RawMessage(`{"recipient":"team:activity-observer","messageId":"peer","text":"forbidden","summary":"forbidden"}`))
			if err == nil {
				t.Fatal("teammate observer accepted a peer message")
			}
		}
		release.Do(func() { close(workerRelease) })
		if _, err := tree.wait(context.Background(), worker, 5000); err != nil {
			t.Fatal(err)
		}
		pair := observer.owner.observer
		if !silent {
			deadline := time.Now().Add(5 * time.Second)
			for {
				pair.mu.Lock()
				state, count := "", len(pair.journal.Reports)
				if count != 0 {
					state = pair.journal.Reports[0].State
				}
				pair.mu.Unlock()
				if state == "observed" && count == 1 {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("report did not reach recipient: state=%s count=%d", state, count)
				}
				time.Sleep(10 * time.Millisecond)
			}
			data, err := os.ReadFile(pair.journal.TargetFile)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Count(string(data), `"customType":"pig_litter_observer_report"`) != 1 || strings.Count(string(data), "PAIRED_REPORT_BODY") != 1 {
				t.Fatalf("recipient did not retain exactly one report marker: %s", data)
			}
		}
		inspection, err := tree.inspect(observer.ref.ID)
		if err != nil || inspection.Outcome == nil || inspection.Outcome.Text != "" {
			t.Fatalf("observer inspect leaked output: %#v %v", inspection, err)
		}
		scope := newCallScope()
		stale, err := scope.begin(observer, "retired-report", "observer_report")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := o.acceptObserverReport(stale, json.RawMessage(`{"reportId":"late","text":"retired report"}`)); err == nil {
			t.Fatal("retired observer retained report authority")
		}
		rootEntries, _ := rawEntries(manager.GetEntries())
		if strings.Contains(string(mustJSON(t, rootEntries)), "OBSERVER_FINAL_PRIVATE_SENTINEL") {
			t.Fatal("observer final text reached the coordinator")
		}
		if _, _, err := tree.message(context.Background(), nil, observer, "steer", "ignore the pairing"); err == nil {
			t.Fatal("observer accepted steering")
		}
		requestsMu.Lock()
		defer requestsMu.Unlock()
		if len(observerRequests) == 0 {
			t.Fatal("observer did not contact the provider")
		}
		for _, request := range observerRequests {
			for _, secret := range []string{"PROJECT_SECRET_NEVER_EXPOSE", "FILESYSTEM_SECRET_NEVER_EXPOSE", "WORKER_PRIVATE_TASK"} {
				if strings.Contains(request, secret) {
					t.Fatalf("observer model received private content %s", secret)
				}
			}
		}
		var first struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal([]byte(observerRequests[0]), &first); err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, tool := range first.Tools {
			names[tool.Function.Name] = true
		}
		if !names["observer_read"] || !names["observer_ls"] || !names["observer_report"] || names["read"] || names["ls"] || names["bash"] || names["litter_spawn"] || names["codemode"] {
			t.Fatalf("observer tool authority = %#v", names)
		}
		if !silent && !strings.Contains(observerRequests[2], `activity.json`) {
			t.Fatal("observer did not receive the virtual activity")
		}
		if activity {
			initial := runtimeRequestText([]byte(observerRequests[0]))
			changed := runtimeRequestText([]byte(observerRequests[1]))
			if !strings.Contains(initial, "Paired worker structural activity") || !strings.Contains(changed, `"toolStarts":2`) {
				t.Fatalf("observer did not receive the initial and changed structural digests: initial=%s changed=%s", initial, changed)
			}
			if strings.Count(changed, `"toolStarts":`) != 2 {
				t.Fatalf("burst did not coalesce to one changed digest: %s", changed)
			}
		}
		if !silent && !strings.Contains(observerRequests[len(observerRequests)-1], "observer report arguments are invalid or unavailable") {
			t.Fatal("the provider's duplicate report fields were not rejected")
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		var result any
		switch method {
		case "sessionRead":
			var query struct{ Method string }
			_ = json.Unmarshal(raw, &query)
			switch query.Method {
			case "getSessionId":
				result = manager.ID()
			case "getSessionFile":
				result = manager.GetSessionFile()
			case "getSessionDir":
				result = manager.GetSessionDir()
			case "getLeafId":
				result = manager.GetLeafID()
			case "getEntries":
				result = manager.GetEntries()
			default:
				t.Fatalf("unexpected session read %s", query.Method)
			}
		case "appendEntry":
			var entry struct {
				CustomType string
				Data       json.RawMessage
			}
			if err := json.Unmarshal(raw, &entry); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.AppendCustomEntry(entry.CustomType, entry.Data); err != nil {
				t.Fatal(err)
			}
			result = map[string]any{}
		case "sendMessage":
			var payload struct {
				Message extension.CustomMessageRef
				Options extension.SendMessageOptions
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			if err := rootSession.SendCustomMessage(context.Background(), payload.Message, &payload.Options); err != nil {
				t.Fatal(err)
			}
			result = map[string]any{}
		case "host.getProcessIdentity":
			identity, err := embeddedPeerHost()
			if err != nil {
				t.Fatal(err)
			}
			result = identity
		case "isIdle":
			result = map[string]bool{"idle": true}
		default:
			return host.call(t, method, raw)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return data
	})
}

func runtimeRawObserverCall(w http.ResponseWriter, id, arguments string) {
	w.Header().Set("Content-Type", "text/event-stream")
	encoded, _ := json.Marshal(arguments)
	fmt.Fprintf(w, "data: {\"id\":\"native\",\"object\":\"chat.completion.chunk\",\"model\":\"child\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":\"observer_report\",\"arguments\":%s}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"native\",\"object\":\"chat.completion.chunk\",\"model\":\"child\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", id, encoded)
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestObserverReportBoundaryRejectsMalformedInput(t *testing.T) {
	for _, raw := range []string{
		`{"reportId":"one","text":"ok","to":"worker"}`,
		`{"reportId":"one","reportId":"two","text":"ok"}`,
		`{"reportId":"one","text":"ok"} {}`,
		`{"reportId":"one","text":" "}`,
		`{"reportId":"one","text":"` + strings.Repeat("x", 4097) + `"}`,
		"{\"reportId\":\"one\",\"text\":\"\xff\"}",
	} {
		if _, _, err := parseObserverReport(json.RawMessage(raw)); err == nil {
			t.Errorf("accepted malformed report %q", raw)
		}
	}
	id, text, err := parseObserverReport(json.RawMessage(`{"reportId":"one","text":"hello"}`))
	if err != nil || id != "one" || text != "hello" {
		t.Fatalf("valid report = %q %q %v", id, text, err)
	}
}

func TestCompletedObserverRecoveryAfterOwnerReplacement(t *testing.T) {
	for _, target := range []string{"worker", "coordinator"} {
		t.Run(target, func(t *testing.T) { testCompletedObserverRecovery(t, target, false) })
	}
	t.Run("deleted_and_recreated_team_target", func(t *testing.T) { testCompletedObserverRecovery(t, "worker", true) })
}

func testCompletedObserverRecovery(t *testing.T, target string, deletedTeam bool) {
	t.Helper()
	t.Setenv("PIG_OFFLINE", "1")
	agentDir := t.TempDir()
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	started := make(chan struct{})
	var observerCalls, workerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if strings.Contains(string(body), "RECOVERY_WORKER_TASK") {
			call := workerCalls.Add(1)
			if deletedTeam && call == 1 {
				runtimeToolAnswer(w, "team-commit", "ls", map[string]any{"path": "."})
				return
			}
			select {
			case <-started:
			default:
				close(started)
			}
			<-request.Context().Done()
			return
		}
		if observerCalls.Add(1) == 1 {
			runtimeToolAnswer(w, "accepted-report", "observer_report", map[string]any{"reportId": "recovery-1", "text": "RECOVERED_PAIRED_REPORT"})
			return
		}
		runtimeAnswer(w, "RECOVERY_FINAL_PRIVATE")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	tree := testTree(t, defaultConfig())
	var manager *coding.SessionManager
	var rootSession *coding.Session
	var allowRoot atomic.Bool
	if target == "worker" {
		allowRoot.Store(true)
	}
	host := &runtimeHost{tools: append([]string{"read", "ls"}, controlNames...)}
	withCompletionHost(t, tree, func(previous *owner) {
		dir := previous.context.Cwd()
		if err := os.WriteFile(filepath.Join(dir, "litter.yaml"), []byte("agents:\n  observer:\n    role: observer\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		rootServices, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: dir, AgentDir: agentDir})
		if err != nil {
			t.Fatal(err)
		}
		defer rootServices.Close()
		rootRuntime, err := coding.NewRuntime(coding.RuntimeOptions{Services: rootServices})
		if err != nil {
			t.Fatal(err)
		}
		defer rootRuntime.Close()
		rootSession, err = rootRuntime.New(coding.SessionStartOptions{SessionDir: filepath.Join(dir, "root-sessions"), Model: rootServices.ModelRuntime().GetModel("fixture", "child"), ResourceLoader: coding.NoResources, SkipBuiltinTools: true})
		if err != nil {
			t.Fatal(err)
		}
		defer rootSession.Close()
		go func() {
			for event := range rootSession.Events() {
				coding.AcknowledgeEvent(event)
			}
		}()
		manager = rootSession.SessionManager()
		if _, err := manager.AppendMessage(agent.AgentMessage{User: &agent.UserMessage{Role: "user", Content: ai.UserContentBlocks{ai.TextContent{Text: "root recovery task"}}}}); err != nil {
			t.Fatal(err)
		}
		config, err := LoadConfig(dir)
		if err != nil {
			t.Fatal(err)
		}
		tree.config, tree.root.id = config, ChildID(manager.ID())
		ownerServices, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: dir, AgentDir: agentDir})
		if err != nil {
			t.Fatal(err)
		}
		previous.sessionID, previous.cwd, previous.agentDir, previous.services, previous.config = manager.ID(), dir, agentDir, ownerServices, config
		tree.execute, tree.persist, tree.completion = previous.executeRun, previous.saveChild, previous.sendCompletion
		tree.observerArm, tree.observerRetire = previous.armObserver, previous.retireObservers
		tree.teamFinished, tree.teamCanceled, tree.teamPaused = previous.finishTeamMember, previous.cancelTeamMember, previous.pauseTeamMember
		tree.teamSettled = previous.settleShutdown
		workerArgs := map[string]any{"type": "scout", "task": "RECOVERY_WORKER_TASK", "model": "fixture/child"}
		var workerValue map[string]any
		var workerTicket *callTicket
		var worker *run
		if deletedTeam {
			workerArgs["name"] = "recovery-worker"
			workerArgs["workspace"] = map[string]any{"kind": "shared_checkout"}
			workerTicket, err = previous.callScope(nil).begin(nil, "recovery-worker", "team_spawn")
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(workerArgs)
			workerValue, err = previous.operation(context.Background(), raw, workerTicket, &previous.context)
			if err != nil {
				t.Fatal(err)
			}
			worker = testRun(t, tree, Ref{ID: workerValue["member"].(MemberRef).ID})
			content, _ := resultContent("team_spawn", workerValue, nil)
			if _, err := manager.AppendMessage(agent.AgentMessage{ToolResult: &agent.ToolResultMessage{Role: "toolResult", ToolCallID: "recovery-worker", ToolName: "team_spawn", Content: []ai.ToolResultMessageContent{ai.TextContent{Text: content}}}}); err != nil {
				t.Fatal(err)
			}
		} else {
			workerValue, workerTicket, err = runtimeOperation(t, previous, "litter_spawn", "recovery-worker", workerArgs)
			if err != nil {
				t.Fatal(err)
			}
			worker = testRun(t, tree, workerValue["child"].(Snapshot).Ref)
		}
		previous.settleAck(workerTicket, false)
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("worker did not reach provider")
		}
		value, ticket, err := runtimeOperation(t, previous, "litter_spawn", "recovery-observer", map[string]any{"type": "observer", "task": "RECOVERY_OBSERVER_TASK", "model": "fixture/child", "observe": map[string]any{"id": worker.ref.ID, "reportTo": target}})
		if err != nil {
			t.Fatal(err)
		}
		observer := testRun(t, tree, value["child"].(Snapshot).Ref)
		var held *observerStore
		if deletedTeam {
			previous.observerMu.Lock()
			held = previous.observers
			previous.observerMu.Unlock()
			held.mu.Lock()
			defer func() {
				if held != nil {
					held.mu.Unlock()
				}
			}()
		}
		previous.settleAck(ticket, false)
		outcome, err := tree.wait(context.Background(), observer, 5000)
		if err != nil || outcome.State != Completed || outcome.Text != "" {
			t.Fatalf("sender did not complete silently: %#v %v", outcome, err)
		}
		pair := observer.owner.observer
		deadline := time.Now().Add(5 * time.Second)
		for {
			pair.mu.Lock()
			state, reason := "", ""
			if len(pair.journal.Reports) == 1 {
				state, reason = pair.journal.Reports[0].State, pair.journal.Reports[0].Reason
			}
			pair.mu.Unlock()
			if deletedTeam && state == "accepted" || !deletedTeam && target == "worker" && state == "attempted" || target == "coordinator" && state == "accepted" && reason != "" {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("expected unresolved accepted report, got %s %q", state, reason)
			}
			time.Sleep(10 * time.Millisecond)
		}
		path, pairID, creation := pair.journal.TargetFile, pair.journal.ID, pair.journal.TeamCreation
		if err := previous.state.retire(); err != nil {
			t.Fatal(err)
		}
		if deletedTeam {
			held.mu.Unlock()
			held = nil
			original := *worker.owner.team
			teamDir := filepath.Join(agentDir, "pig-litter", "roots", manager.ID())
			if err := os.RemoveAll(teamDir); err != nil {
				t.Fatal(err)
			}
			recreated, err := openTeamStore(agentDir, ChildID(manager.ID()))
			if err != nil {
				t.Fatal(err)
			}
			member, err := recreated.Reserve(original.Ref.ID, original.AdmissionID, original.Name, original.Workspace, original.PlanRequired)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := recreated.Commit(member.AdmissionID); err != nil {
				t.Fatal(err)
			}
			newCreation, valid, err := recreated.observerMembers(&observerMemberPin{Ref: member.Ref, Admission: member.AdmissionID})
			if err != nil || !valid || member.Ref != original.Ref || newCreation == creation {
				t.Fatalf("recreated team identity = %q, %t, %v", newCreation, valid, err)
			}
			if err := recreated.Close(); err != nil {
				t.Fatal(err)
			}
		}
		allowRoot.Store(true)
		for replacement := 0; replacement < 2; replacement++ {
			state := &extensionState{}
			owner, err := state.ensure(previous.context)
			if err != nil {
				t.Fatalf("replacement %d: %v", replacement, err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				owner.observerMu.Lock()
				store := owner.observers
				owner.observerMu.Unlock()
				if store == nil {
					t.Fatal("replacement lost the observer store")
				}
				store.mu.Lock()
				recovered := store.pairs[pairID]
				store.mu.Unlock()
				if recovered == nil {
					t.Fatal("replacement could not authenticate the accepted report")
				}
				recovered.mu.Lock()
				disposition := ""
				if len(recovered.journal.Reports) == 1 {
					disposition = recovered.journal.Reports[0].State
				}
				recovered.mu.Unlock()
				if disposition == "observed" && !deletedTeam || disposition == "undeliverable" && deletedTeam {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("replacement did not recover report: %s", disposition)
				}
				time.Sleep(10 * time.Millisecond)
			}
			if deletedTeam {
				listing, _, err := runtimeOperation(t, owner, "litter_list", fmt.Sprintf("listing-%d", replacement), map[string]any{})
				if err != nil || !strings.Contains(string(mustJSON(t, listing)), `"state":"undeliverable"`) || strings.Contains(string(mustJSON(t, listing)), "RECOVERED_PAIRED_REPORT") {
					t.Fatalf("removed recipient disposition was not visible: %#v %v", listing, err)
				}
			}
			if err := state.retire(); err != nil {
				t.Fatal(err)
			}
		}
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if deletedTeam && strings.Contains(string(data), "RECOVERED_PAIRED_REPORT") {
			t.Fatal("report was delivered to a recreated teammate")
		}
		if !deletedTeam && (strings.Count(string(data), `"customType":"pig_litter_observer_report"`) != 1 || strings.Count(string(data), "RECOVERED_PAIRED_REPORT") != 1) {
			t.Fatalf("owner replacement did not retain exactly one report: %s", data)
		}
		wantWorkerCalls := int32(1)
		if deletedTeam {
			wantWorkerCalls = 2
		}
		if workerCalls.Load() != wantWorkerCalls || observerCalls.Load() != 2 {
			t.Fatalf("recovery resumed a sender or worker: worker=%d observer=%d", workerCalls.Load(), observerCalls.Load())
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		var result any
		switch method {
		case "sessionRead":
			var query struct{ Method string }
			_ = json.Unmarshal(raw, &query)
			switch query.Method {
			case "getSessionId":
				result = manager.ID()
			case "getSessionFile":
				result = manager.GetSessionFile()
			case "getSessionDir":
				result = manager.GetSessionDir()
			case "getLeafId":
				result = manager.GetLeafID()
			case "getEntries":
				result = manager.GetEntries()
			default:
				t.Fatalf("unexpected session read %s", query.Method)
			}
		case "appendEntry":
			var entry struct {
				CustomType string
				Data       json.RawMessage
			}
			if err := json.Unmarshal(raw, &entry); err != nil {
				t.Fatal(err)
			}
			if _, err := manager.AppendCustomEntry(entry.CustomType, entry.Data); err != nil {
				t.Fatal(err)
			}
			result = map[string]any{}
		case "sendMessage":
			var payload struct {
				Message extension.CustomMessageRef
				Options extension.SendMessageOptions
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			if err := rootSession.SendCustomMessage(context.Background(), payload.Message, &payload.Options); err != nil {
				t.Fatal(err)
			}
			result = map[string]any{}
		case "host.getProcessIdentity":
			if allowRoot.Load() {
				identity, err := embeddedPeerHost()
				if err != nil {
					t.Fatal(err)
				}
				result = identity
			} else {
				result = map[string]any{}
			}
		case "isIdle":
			result = map[string]bool{"idle": true}
		default:
			return host.call(t, method, raw)
		}
		data, err := json.Marshal(result)
		if err != nil {
			t.Fatal(err)
		}
		return data
	})
}

func TestObserverTeamCreationIdentitySurvivesReopenAndChangesAfterRecreation(t *testing.T) {
	dir := t.TempDir()
	root := ChildID("018f0000-0000-7000-8000-000000000001")
	store, err := openTeamStore(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	member, err := store.Reserve(ChildID("018f0000-0000-7000-8000-000000000002"), "018f0000-0000-7000-8000-000000000003", "worker", WorkspacePolicy{Kind: "shared_checkout", Cwd: "/work"}, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(member.AdmissionID); err != nil {
		t.Fatal(err)
	}
	pin := &observerMemberPin{Ref: member.Ref, Admission: member.AdmissionID}
	first, valid, err := store.observerMembers(pin)
	if err != nil || !valid || first == "" {
		t.Fatalf("initial team pin = %q %t %v", first, valid, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openTeamStore(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	second, valid, err := store.observerMembers(pin)
	if err != nil || !valid || second != first {
		t.Fatalf("reopened team pin = %q %t %v", second, valid, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pig-litter", "roots", string(root), "team.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var legacy map[string]any
	if err := json.Unmarshal(data, &legacy); err != nil {
		t.Fatal(err)
	}
	delete(legacy, "creationId")
	data, _ = json.Marshal(legacy)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err = openTeamStore(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	migrated, valid, err := store.observerMembers(pin)
	if err != nil || !valid || migrated == "" {
		t.Fatalf("legacy team pin = %q %t %v", migrated, valid, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = openTeamStore(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	again, valid, err := store.observerMembers(pin)
	if err != nil || !valid || again != migrated {
		t.Fatalf("migrated team pin = %q %t %v", again, valid, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(filepath.Dir(path)); err != nil {
		t.Fatal(err)
	}
	store, err = openTeamStore(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recreated, valid, err := store.observerMembers(pin)
	if err != nil || valid || recreated == migrated {
		t.Fatalf("recreated team reused its identity: %q %t %v", recreated, valid, err)
	}
}
