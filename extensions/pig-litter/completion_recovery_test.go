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

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

func TestRootCompletionRecoveryAfterQueuedAbort(t *testing.T) {
	for _, clearQueue := range []bool{false, true} {
		for _, consumed := range []bool{false, true} {
			t.Run(fmt.Sprintf("clear=%t/consumed=%t", clearQueue, consumed), func(t *testing.T) {
				testRootCompletionRecovery(t, clearQueue, consumed)
			})
		}
	}
}

func testRootCompletionRecovery(t *testing.T, clearQueue, consumed bool) {
	t.Helper()
	directory := t.TempDir()
	t.Setenv("PIG_OFFLINE", "1")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var calls atomic.Int32
	requests := make(chan string, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requests <- string(body)
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"recovery\",\"object\":\"chat.completion.chunk\",\"model\":\"parent\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"parent answer\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"recovery\",\"object\":\"chat.completion.chunk\",\"model\":\"parent\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer server.Close()
	models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"fixture-only","models":[{"id":"parent","name":"parent","contextWindow":32000,"maxTokens":1024}]}}}`, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(directory, "models.json"), []byte(models), 0o600); err != nil {
		t.Fatal(err)
	}
	services, err := coding.NewServices(coding.ServicesOptions{CWD: directory, AgentDir: directory})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	var manager *coding.SessionManager
	config := defaultConfig()
	config.MaxMailbox = 1
	tree := testTree(t, config)
	child := testAdmission(t, tree, Ref{}, "scout", "finished child")
	selected := testRun(t, tree, child.Ref)
	tree.finish(tree.records[child.ID], selected, Outcome{State: Completed, Text: "ABORT_RECOVERY_REPORT"})
	var session *coding.Session
	withCompletionHost(t, tree, func(o *owner) {
		entered, release := make(chan struct{}), make(chan struct{})
		var gate, releaseOnce sync.Once
		defer releaseOnce.Do(func() { close(release) })
		hooks := o.completionHooks(tree.root)
		hooks.Handlers["agent_start"] = []extension.HandlerFn{func(...any) (any, error) {
			o.watchRoot(session.Signal())
			return nil, nil
		}}
		hooks.Handlers["agent_settled"] = append(hooks.Handlers["agent_settled"], func(...any) (any, error) {
			o.settleRoot()
			o.retireReceipts(tree.root)
			return nil, nil
		})
		hooks.Handlers["agent_before_settle"] = append(hooks.Handlers["agent_before_settle"], func(...any) (any, error) {
			gate.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-ctx.Done():
				}
			})
			return nil, nil
		})
		runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, NewExtensions: []extension.Extension{hooks}})
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		session, err = runtime.New(coding.SessionStartOptions{SessionDir: filepath.Join(directory, "sessions"), Model: services.ModelRuntime().GetModel("fixture", "parent"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
		if err != nil {
			t.Fatal(err)
		}
		manager = session.SessionManager()
		defer session.Close()
		go func() {
			for event := range session.Events() {
				coding.AcknowledgeEvent(event)
			}
		}()
		done := make(chan error, 1)
		go func() { _, err := session.Send(ctx, "initial root task"); done <- err }()
		select {
		case <-entered:
		case <-ctx.Done():
			t.Fatal("root did not reach the settlement barrier")
		}
		<-requests
		o.sendCompletion(tree.records[child.ID], selected, *selected.outcome)
		if len(tree.pending) != 1 || len(session.Agent().PeekQueuedMessages()) != 1 {
			t.Fatal("completion was not queued with a pending receipt")
		}
		if clearQueue {
			session.ClearQueue()
		}
		session.RequestAbort()
		releaseOnce.Do(func() { close(release) })
		<-done
		entries, _ := rawEntries(manager.Entries())
		details := completionDetails{ChildID: child.ID, Receipt: selected.noticeID}
		if !completionStored(entries, details, selected.reservation.content) || len(tree.pending) != 0 {
			t.Fatal("root recovery did not store the receipt and release capacity")
		}
		queued := len(session.Agent().PeekQueuedMessages())
		wantQueued := 1
		if clearQueue {
			wantQueued = 0
		}
		if queued != wantQueued || calls.Load() != 1 {
			t.Fatalf("abort changed queued work or started inference: queued=%d calls=%d", queued, calls.Load())
		}
		if consumed {
			ticket := testCall(t, o, nil, "consume", "litter_wait")
			o.beginOutcomeAck(ticket, selected, "litter_wait", nil)
			o.readyOutcomeAck(ticket, true)
			o.finishCall(ticket, nativeResult("litter_wait", map[string]any{"outcome": selected.outcome}, nil), false)
		}
		for _, prompt := range []string{"next root task", "following root task"} {
			if _, err := session.Send(ctx, prompt); err != nil {
				t.Fatal(err)
			}
			next := <-requests
			copies := strings.Count(next, "ABORT_RECOVERY_REPORT")
			wantCopies := 1
			if consumed {
				wantCopies = 0
			}
			t.Logf("stored=true queueAfterAbort=%d consumed=%t prompt=%q reportCopies=%d", queued, consumed, prompt, copies)
			if copies != wantCopies {
				t.Errorf("provider received %d completion copies, want %d: %s", copies, wantCopies, next)
			}
		}
		if calls.Load() != 3 {
			t.Errorf("recovery caused an extra inference: calls=%d", calls.Load())
		}
		tree.execute = func(*record, *run) Outcome {
			return Outcome{State: Completed, Text: "ABORT_RECOVERY_REPORT"}
		}
		resumed, next, err := tree.resume(nil, selected, "report again")
		if err != nil || resumed.ID != child.ID || next.noticeID == selected.noticeID {
			t.Fatalf("resume did not retain the child with a new completion receipt: %#v %v", resumed, err)
		}
		if err := tree.activate(next); err != nil {
			t.Fatal(err)
		}
		outcome, err := tree.wait(ctx, next, 1000)
		if err != nil {
			t.Fatal(err)
		}
		o.sendCompletion(tree.records[child.ID], next, outcome)
		if _, err := session.Send(ctx, "read the new result"); err != nil {
			t.Fatal(err)
		}
		copies := strings.Count(<-requests, "ABORT_RECOVERY_REPORT")
		wantCopies := 2
		if consumed {
			wantCopies = 1
		}
		t.Logf("distinct receipt for same child and report: reportCopies=%d", copies)
		if copies != wantCopies || calls.Load() != 4 {
			t.Errorf("distinct outcome was lost or repeated: copies=%d want=%d calls=%d", copies, wantCopies, calls.Load())
		}
		replacement := testAdmission(t, tree, Ref{}, "scout", "replacement")
		replacementRun := testRun(t, tree, replacement.Ref)
		if err := tree.activate(replacementRun); err != nil {
			t.Fatal(err)
		}
		replacementOutcome, err := tree.wait(ctx, replacementRun, 1000)
		if err != nil {
			t.Fatal(err)
		}
		o.sendCompletion(tree.records[replacement.ID], replacementRun, replacementOutcome)
		if _, err := session.Send(ctx, "read both child results"); err != nil {
			t.Fatal(err)
		}
		copies = strings.Count(<-requests, "ABORT_RECOVERY_REPORT")
		wantCopies++
		t.Logf("distinct child with same report: reportCopies=%d", copies)
		if copies != wantCopies || calls.Load() != 5 {
			t.Errorf("distinct child outcome was lost or repeated: copies=%d want=%d calls=%d", copies, wantCopies, calls.Load())
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		switch method {
		case "sessionRead":
			var query struct{ Method string }
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getEntries" {
				entries, _ := json.Marshal(manager.Entries())
				return entries
			}
			return json.RawMessage(`"fixture"`)
		case "isIdle":
			result, _ := json.Marshal(map[string]bool{"idle": session.IsIdle()})
			return result
		case "sendMessage":
			var payload struct {
				Message extension.CustomMessageRef
				Options extension.SendMessageOptions
			}
			if err := json.Unmarshal(raw, &payload); err != nil {
				t.Fatal(err)
			}
			if err := session.SendMessage(payload.Message, &payload.Options); err != nil {
				t.Error(err)
			}
			return json.RawMessage(`{}`)
		case "appendEntry":
			var payload struct {
				CustomType string
				Data       json.RawMessage
			}
			_ = json.Unmarshal(raw, &payload)
			if _, err := manager.AppendCustomEntry(payload.CustomType, payload.Data); err != nil {
				t.Fatal(err)
			}
			return json.RawMessage(`{}`)
		default:
			t.Fatalf("unexpected root action %s", method)
			return nil
		}
	})
}
