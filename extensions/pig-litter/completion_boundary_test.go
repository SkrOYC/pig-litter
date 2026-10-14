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
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

func TestNativeCompletionAdmissionAtSessionBoundaries(t *testing.T) {
	for _, phase := range []string{"model", "tool", "restart_tool", "continue_tool", "before_agent_end", "before_hook", "after_hook", "idle", "paused"} {
		for _, consumed := range []bool{false, true} {
			if consumed && phase != "model" && phase != "tool" && phase != "restart_tool" && phase != "continue_tool" {
				continue
			}
			t.Run(fmt.Sprintf("%s/consumed=%t", phase, consumed), func(t *testing.T) {
				testNativeCompletionBoundary(t, phase, consumed)
			})
		}
	}
}

func testNativeCompletionBoundary(t *testing.T, phase string, consumed bool) {
	t.Helper()
	toolPhase := phase == "tool" || phase == "restart_tool" || phase == "continue_tool"
	directory := t.TempDir()
	t.Setenv("PIG_OFFLINE", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, release := make(chan struct{}), make(chan struct{})
	var gate, releaseOnce sync.Once
	hold := func() {
		gate.Do(func() {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
			}
		})
	}
	defer releaseOnce.Do(func() { close(release) })
	var calls atomic.Int32
	var outcomeConsumed atomic.Bool
	var reportAfterConsumption atomic.Bool
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		if outcomeConsumed.Load() && strings.Contains(string(body), "BOUNDARY_COMPLETION_REPORT") {
			reportAfterConsumption.Store(true)
		}
		requests <- string(body)
		n := calls.Add(1)
		if n == 1 && (phase == "model" || phase == "paused") {
			hold()
		}
		if request.Context().Err() != nil {
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 && phase == "tool" || n == 2 && (phase == "restart_tool" || phase == "continue_tool") {
			fmt.Fprint(w, "data: {\"id\":\"boundary\",\"object\":\"chat.completion.chunk\",\"model\":\"parent\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"hold-call\",\"type\":\"function\",\"function\":{\"name\":\"hold\",\"arguments\":\"{}\"}}]},\"finish_reason\":null}]}\n\ndata: {\"id\":\"boundary\",\"object\":\"chat.completion.chunk\",\"model\":\"parent\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, "data: {\"id\":\"boundary\",\"object\":\"chat.completion.chunk\",\"model\":\"parent\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"parent response\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"boundary\",\"object\":\"chat.completion.chunk\",\"model\":\"parent\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
	}))
	defer server.Close()
	models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"fixture-only","models":[{"id":"parent","name":"parent","contextWindow":32000,"maxTokens":1024}]}}}`, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(directory, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read", "ls"}, CanDelegate: true}
	parentRef := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	childRef := testAdmission(t, tree, parentRef.Ref, "scout", "child")
	parent, child := tree.records[parentRef.ID], tree.records[childRef.ID]
	services, err := coding.NewServices(coding.ServicesOptions{CWD: parent.activeLog.GetCwd(), AgentDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	selected := child.run
	tree.finish(child, selected, Outcome{State: Completed, Text: "BOUNDARY_COMPLETION_REPORT"})
	withCompletionHost(t, tree, func(o *owner) {
		hooks := o.completionHooks(parent)
		blocker := extension.HandlerFn(func(...any) (any, error) { hold(); return nil, nil })
		switch phase {
		case "restart_tool", "continue_tool":
			var starts int
			hooks.Handlers["agent_start"] = []extension.HandlerFn{func(...any) (any, error) {
				starts++
				if starts == 2 {
					hold()
				}
				return nil, nil
			}}
			if phase == "continue_tool" {
				var once sync.Once
				hooks.Handlers["agent_before_settle"] = append(hooks.Handlers["agent_before_settle"], func(...any) (any, error) {
					var result any
					once.Do(func() {
						yes := true
						entries := []extension.SessionBoundaryDraft{{Type: "custom_message", CustomType: "fixture_continuation", Content: "continue the parent work"}}
						result = extension.BoundaryResult{Continue: &yes, Entries: &entries}
					})
					return result, nil
				})
			}
		case "before_agent_end":
			hooks.Handlers["agent_end"] = append([]extension.HandlerFn{blocker}, hooks.Handlers["agent_end"]...)
		case "before_hook":
			hooks.Handlers["agent_before_settle"] = append([]extension.HandlerFn{blocker}, hooks.Handlers["agent_before_settle"]...)
		case "after_hook":
			hooks.Handlers["agent_before_settle"] = append(hooks.Handlers["agent_before_settle"], blocker)
		}
		runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, NewExtensions: []extension.Extension{hooks}})
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		consume := func() {
			ticket := testCall(t, o, parent.run, "consume", "litter_wait")
			o.beginOutcomeAck(ticket, selected, "litter_wait", nil)
			o.readyOutcomeAck(ticket, true)
			o.finishCall(ticket, nativeResult("litter_wait", map[string]any{"outcome": selected.outcome}, nil), false)
			outcomeConsumed.Store(true)
		}
		tools := []agent.AgentTool{}
		allowed := map[string]struct{}{}
		if toolPhase {
			tools = append(tools, &nativeTool{name: "hold", schema: object([]string{}, map[string]any{}), execute: func(context.Context, string, json.RawMessage) (agent.AgentToolResult, error) {
				hold()
				if consumed {
					consume()
				}
				return nativeResult("hold", map[string]any{"result": "tool finished"}, nil), nil
			}})
			allowed["hold"] = struct{}{}
		}
		session, err := runtime.Resume(string(parent.id), coding.SessionStartOptions{SessionDir: parent.activeLog.GetSessionDir(), Model: services.ModelRuntime().GetModel("fixture", "parent"), ResourceLoader: coding.NoResources, ExtraTools: tools, AllowedTools: allowed})
		if err != nil {
			t.Fatal(err)
		}
		parent.activeLog = session.SessionManager()
		defer session.Close()
		parent.run.session = session
		go func() {
			for event := range session.Events() {
				coding.AcknowledgeEvent(event)
			}
		}()
		unsubscribe := session.Subscribe(func(event agent.AgentEvent) {
			if end, ok := event.(agent.MessageEndEvent); ok && end.Message.Custom != nil {
				o.observeCompletion(parent.run, end.Message.Custom)
			}
		})
		defer unsubscribe()
		if phase == "restart_tool" {
			if _, err := session.Send(ctx, "prior task"); err != nil {
				t.Fatal(err)
			}
		}
		done := make(chan error, 1)
		if phase != "idle" {
			go func() { _, err := session.Send(ctx, "ordinary parent task"); done <- err }()
			select {
			case <-entered:
			case <-ctx.Done():
				t.Fatal("native phase barrier not reached")
			}
		}
		if phase == "paused" {
			tree.mu.Lock()
			parent.paused = true
			tree.mu.Unlock()
			session.RequestAbort()
		}
		o.sendCompletion(child, selected, *selected.outcome)
		if consumed && !toolPhase {
			consume()
		}
		releaseOnce.Do(func() { close(release) })
		if phase != "idle" {
			if err := <-done; err != nil && phase != "paused" {
				t.Fatal(err)
			}
		}
		o.retireReceipts(parent)
		wantCalls := int32(2)
		if phase == "restart_tool" || phase == "continue_tool" {
			wantCalls = 3
		} else if phase == "idle" {
			wantCalls = 0
		} else if phase == "paused" || consumed && phase == "model" {
			wantCalls = 1
		}
		if calls.Load() != wantCalls {
			t.Fatalf("provider calls=%d, want %d", calls.Load(), wantCalls)
		}
		reportRequests := 0
		for n := int32(0); n < calls.Load(); n++ {
			if strings.Contains(<-requests, "BOUNDARY_COMPLETION_REPORT") {
				reportRequests++
			}
		}
		wantReports := 1
		if consumed || phase == "idle" || phase == "paused" {
			wantReports = 0
		}
		if phase == "continue_tool" {
			wantReports++
		}
		if reportRequests != wantReports {
			t.Fatalf("provider requests with completion=%d, want %d", reportRequests, wantReports)
		}
		if reportAfterConsumption.Load() {
			t.Fatal("a provider request repeated the completion after a successful wait")
		}
		entries, _ := rawEntries(parent.activeLog.Entries())
		details := completionDetails{ChildID: child.id, Receipt: selected.noticeID}
		if !completionStored(entries, details, selected.reservation.content) {
			t.Fatal("native delivery did not retain the child completion")
		}
		tree.mu.Lock()
		pending := tree.pending[selected] != nil
		tree.mu.Unlock()
		if pending {
			t.Fatal("stored completion retained delivery capacity")
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method != "sessionRead" {
			t.Fatalf("unexpected root action %s", method)
		}
		return json.RawMessage(`"fixture"`)
	})
}
