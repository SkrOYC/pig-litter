package piglitter

import (
	"context"
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

func TestBeforeSettleReconcilesDeferredStorageNotObservedByExtensionEvents(t *testing.T) {
	directory := t.TempDir()
	t.Setenv("PIG_OFFLINE", "1")
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	requests := make(chan string, 4)
	var requestCount atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requests <- string(body)
		if requestCount.Add(1) == 1 {
			close(entered)
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"boundary\",\"object\":\"chat.completion.chunk\",\"model\":\"parent\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"parent response\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"boundary\",\"object\":\"chat.completion.chunk\",\"model\":\"parent\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
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
	manager, err := coding.NewInMemorySessionManager(directory)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	o := testCallOwner(tree)
	child := testAdmission(t, tree, Ref{}, "scout", "finished child")
	selected := testRun(t, tree, child.Ref)
	tree.finish(tree.records[child.ID], selected, Outcome{State: Completed, Text: "UNCONSUMED_COMPLETION_MARKER"})
	content, _ := completionContent(*selected.outcome)
	if !tree.startCompletion(selected, content) {
		t.Fatal("completion did not reserve delivery")
	}
	notice := selected.reservation
	notice.continueTurn = true
	tree.root.notices = map[string]*reservation{notice.nonce: notice}
	details := completionDetails{ChildID: child.ID, Receipt: notice.nonce}
	receipt := &completionReceipt{
		observed: func() bool {
			entries, err := rawEntries(manager.Entries())
			return err == nil && completionStored(entries, details, content)
		},
		appended: func() { tree.acknowledge(selected) },
	}
	tree.root.receipts = map[*run]*completionReceipt{selected: receipt}
	var extensionEvents, publicEvents atomic.Int32
	hooks := o.completionHooks(tree.root)
	hooks.Handlers["message_end"] = []extension.HandlerFn{func(args ...any) (any, error) {
		if _, ok := contextCompletion(args[0].(extension.MessageEndEvent).Message); ok {
			extensionEvents.Add(1)
		}
		return nil, nil
	}}
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, NewExtensions: []extension.Extension{hooks}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	session, err := runtime.New(coding.SessionStartOptions{SessionManager: manager, Model: services.ModelRuntime().GetModel("fixture", "parent"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	go func() {
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	unsubscribe := session.Subscribe(func(event agent.AgentEvent) {
		if end, ok := event.(agent.MessageEndEvent); ok && end.Message.Custom != nil {
			publicEvents.Add(1)
		}
	})
	defer unsubscribe()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	finished := make(chan error, 1)
	go func() { _, err := session.Send(ctx, "ordinary parent task"); finished <- err }()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("native provider did not start")
	}
	trigger := false
	if err := session.SendCustomMessage(ctx, extension.CustomMessageRef{CustomType: "litter_completion", Content: content, Display: false, Details: details}, &extension.SendMessageOptions{TriggerTurn: &trigger}); err != nil {
		t.Fatal(err)
	}
	if receipt.observed() {
		t.Fatal("deferred custom message was stored before the turn boundary")
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if publicEvents.Load() != 1 || extensionEvents.Load() != 0 {
		t.Fatalf("deferred event routing changed: public=%d extension=%d", publicEvents.Load(), extensionEvents.Load())
	}
	if requestCount.Load() != 2 {
		t.Fatalf("stored completion caused %d provider requests, want initial request plus automatic continuation", requestCount.Load())
	}
	<-requests
	if next := <-requests; !strings.Contains(next, "UNCONSUMED_COMPLETION_MARKER") {
		t.Fatalf("continuation omitted stored completion: %s", next)
	}
	if len(tree.pending) != 0 {
		t.Fatal("stored completion retained delivery capacity")
	}
}
