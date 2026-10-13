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
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
)

func TestInitialTaskSurvivesUntilNativeInputIsStored(t *testing.T) {
	for _, interruption := range []string{"setup pause", "setup failure", "pause after storage"} {
		t.Run(interruption, func(t *testing.T) { testPendingInitialTask(t, interruption) })
	}
}

func testPendingInitialTask(t *testing.T, interruption string) {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("PIG_OFFLINE", "1")
	requests := make(chan string, 4)
	var providerCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requests <- string(body)
		if providerCalls.Add(1) == 1 && interruption == "pause after storage" {
			<-request.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"retained-input\",\"object\":\"chat.completion.chunk\",\"model\":\"child\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"retained-input\",\"object\":\"chat.completion.chunk\",\"model\":\"child\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"fixture-only","models":[{"id":"child","name":"child","contextWindow":32000,"maxTokens":1024}]}}}`, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(directory, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	services, err := coding.NewServices(coding.ServicesOptions{CWD: directory, AgentDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	manager, err := coding.NewInMemorySessionManager(directory)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	task := "Inspect ORIGINAL_TASK_BEFORE_NATIVE_SEND and report its contents."
	snapshot, initial, err := tree.admit(admission{kind: "scout", name: "retained task", model: "fixture/child", cwd: directory, task: task, agent: tree.config.Agents["scout"], history: manager})
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	var executions atomic.Int32
	tree.execute = func(node *record, execution *run) Outcome {
		if executions.Add(1) == 1 && interruption != "pause after storage" {
			close(entered)
			if interruption == "setup pause" {
				<-execution.ctx.Done()
			}
			return Outcome{State: Unavailable, Error: "setup ended before native Send"}
		}
		runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, AbortContext: execution.ctx})
		if err != nil {
			return Outcome{State: Failed, Error: err.Error()}
		}
		defer runtime.Close()
		session, err := runtime.New(coding.SessionStartOptions{SessionManager: node.history, Model: services.ModelRuntime().GetModel("fixture", "child"), SystemPrompt: "Probe retained session.", ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
		if err != nil {
			return Outcome{State: Failed, Error: err.Error()}
		}
		drained := make(chan struct{})
		go func() {
			defer close(drained)
			for event := range session.Events() {
				coding.AcknowledgeEvent(event)
			}
		}()
		defer func() { _ = session.Close(); <-drained }()
		if err := tree.sendInput(session, execution); err != nil {
			return Outcome{State: Failed, Error: err.Error()}
		}
		return Outcome{State: Completed, Text: "native request completed"}
	}
	if err := tree.activate(initial); err != nil {
		t.Fatal(err)
	}
	if interruption == "pause after storage" {
		select {
		case request := <-requests:
			if strings.Count(request, "ORIGINAL_TASK_BEFORE_NATIVE_SEND") != 1 {
				t.Fatalf("initial request did not contain exactly one task: %s", request)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("initial native provider request did not start")
		}
	} else {
		<-entered
	}
	if interruption == "setup failure" {
		outcome, err := tree.wait(context.Background(), initial, 5000)
		if err != nil || outcome.State != Unavailable {
			t.Fatalf("setup failure result: %#v %v", outcome, err)
		}
	} else {
		outcome, _, err := tree.stop(nil, initial)
		if err != nil || outcome.State != Paused {
			t.Fatalf("pause result: %#v %v", outcome, err)
		}
	}
	selected := initial
	resume := func(text string) string {
		t.Helper()
		resumed, next, err := tree.resume(nil, selected, text)
		if err != nil || resumed.ID != snapshot.ID {
			t.Fatalf("resume changed the retained session: %#v %v", resumed, err)
		}
		if err := tree.activate(next); err != nil {
			t.Fatal(err)
		}
		outcome, err := tree.wait(context.Background(), next, 5000)
		if err != nil || outcome.State != Completed {
			t.Fatalf("resumed native result: %#v %v", outcome, err)
		}
		selected = next
		return <-requests
	}
	first := resume("Continue.")
	if strings.Count(first, "ORIGINAL_TASK_BEFORE_NATIVE_SEND") != 1 || !strings.Contains(first, "Continue.") {
		t.Fatalf("resume lost or duplicated the original task: %s", first)
	}
	second := resume("Continue after storage.")
	if strings.Count(second, "ORIGINAL_TASK_BEFORE_NATIVE_SEND") != 1 || lastInput(t, second) != "Continue after storage." {
		t.Fatalf("a stored task was replayed: %s", second)
	}
	if _, err := manager.AppendCompaction("COMPACTED_CONTEXT_MARKER", "", 0, nil, false, nil); err != nil {
		t.Fatal(err)
	}
	third := resume("Continue after compaction.")
	if strings.Contains(third, "ORIGINAL_TASK_BEFORE_NATIVE_SEND") || !strings.Contains(third, "COMPACTED_CONTEXT_MARKER") || lastInput(t, third) != "Continue after compaction." {
		t.Fatalf("compaction caused initial input to replay: %s", third)
	}
}

func lastInput(t *testing.T, raw string) string {
	t.Helper()
	var request struct {
		Messages []struct {
			Role    string
			Content json.RawMessage
		}
	}
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatal(err)
	}
	var last string
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		if json.Unmarshal(message.Content, &last) == nil {
			continue
		}
		var blocks []struct{ Text string }
		if err := json.Unmarshal(message.Content, &blocks); err != nil {
			t.Fatal(err)
		}
		last = ""
		for _, block := range blocks {
			last += block.Text
		}
	}
	return last
}
