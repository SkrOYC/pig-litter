package piglitter

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

func TestSiblingHandoffBindsEmbeddedSession(t *testing.T) {
	for _, mode := range []string{"busy-boundary", "idle-wake", "canceled-wake", "failed-wake"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("PIG_OFFLINE", "1")
			store, _, _, workspace := storeTestOpen(t)
			sender, recipient := peerTestMembers(t, store, workspace)
			request := peerRequest{Recipient: "bob", MessageID: "sibling-attempt", Text: "hello", Summary: "greeting"}
			accepted, _, err := store.acceptPeer(sender.Ref, request)
			if err != nil {
				t.Fatal(err)
			}
			directory := t.TempDir()
			var providerFailed atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if mode == "failed-wake" && !providerFailed.Swap(true) {
					http.Error(w, "fixture provider failed", http.StatusBadRequest)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"id\":\"peer\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"peer\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			}))
			defer server.Close()
			models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"fixture-only","models":[{"id":"fixture","name":"fixture","contextWindow":32000,"maxTokens":1024}]}}}`, server.URL+"/v1")
			if err := os.WriteFile(filepath.Join(directory, "models.json"), []byte(models), 0o600); err != nil {
				t.Fatal(err)
			}
			trusted := true
			services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: directory, AgentDir: directory, ProjectTrusted: &trusted})
			if err != nil {
				t.Fatal(err)
			}
			defer services.Close()
			tree := testTree(t, defaultConfig())
			r := &record{id: recipient.Ref.ID, team: &recipient, parent: tree.root, admitted: true}
			selected := tree.newRunLocked(Ref{recipient.Ref.ID}, "")
			selected.owner, r.run = r, selected
			tree.records[r.id] = r
			o := &owner{tree: tree, teamStore: store}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			hooks := o.peerHooks(r, selected)
			if mode == "canceled-wake" {
				hooks.Handlers["before_agent_start"] = []extension.HandlerFn{func(...any) (any, error) {
					selected.cancel()
					return nil, nil
				}}
			}
			runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, AbortContext: ctx, NewExtensions: []extension.Extension{hooks}})
			if err != nil {
				t.Fatal(err)
			}
			defer runtime.Close()
			session, err := runtime.New(coding.SessionStartOptions{SessionID: string(r.id), SessionDir: filepath.Join(directory, "sessions"), Model: services.ModelRuntime().GetModel("fixture", "fixture"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			var failedTerminal atomic.Bool
			session.Subscribe(func(event agent.AgentEvent) {
				if end, ok := event.(agent.MessageEndEvent); ok && end.Message.Assistant != nil && end.Message.Assistant.StopReason == ai.StopReasonError {
					failedTerminal.Store(true)
				}
			})
			selected.session = session
			if _, err := session.SessionManager().AppendMessage(agent.AgentMessage{User: &agent.UserMessage{Role: "user", Content: ai.UserContentBlocks{ai.TextContent{Text: "retained marker"}}}}); err != nil {
				t.Fatal(err)
			}
			go func() {
				for event := range session.Events() {
					coding.AcknowledgeEvent(event)
				}
			}()
			if mode != "busy-boundary" {
				selected.peerWake = &peerWakeInput{Recipient: recipient.Ref}
				err = o.sendPeerWake(session, r, selected)
			} else {
				_, err = session.Send(ctx, "finish the current task")
			}
			if err != nil && mode != "canceled-wake" && mode != "failed-wake" {
				t.Fatal(err)
			}
			key, _ := peerKey(accepted.Envelope)
			_, path := peerPaths(store.dir, key)
			receipt, err := readPeerReceipt(path)
			if err != nil {
				t.Fatal(err)
			}
			host, err := peerObservedHost(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			if receipt.Attempt == nil || receipt.Attempt.Host != host || receipt.Attempt.SessionFile != *session.SessionManager().GetSessionFile() {
				t.Fatalf("embedded sibling attempt = %#v, want host %#v and session %q", receipt.Attempt, host, *session.SessionManager().GetSessionFile())
			}
			if mode == "failed-wake" {
				if !providerFailed.Load() || err == nil && !failedTerminal.Load() {
					t.Fatalf("provider failure did not fail the wake: %v", err)
				}
				content, contentErr := peerMessageContent(accepted.Envelope, "alice", peerBody{Text: request.Text, Summary: request.Summary})
				if contentErr != nil {
					t.Fatal(contentErr)
				}
				priorID, entryErr := nativePeerEntry(receipt.Attempt.SessionFile, accepted.Envelope, content)
				if entryErr != nil || priorID == "" {
					t.Fatalf("failed wake native entry = %q, %v", priorID, entryErr)
				}
				if retry, _, retryErr := store.acceptPeer(sender.Ref, request); retryErr != nil || retry.State == peerAccepted {
					t.Fatalf("failed wake authenticated retry = %#v, %v", retry, retryErr)
				}
				if _, err := session.Send(ctx, "resume after failed wake"); err != nil {
					t.Fatal(err)
				}
				final, finalErr := readPeerReceipt(path)
				if finalErr != nil || final.State != peerObserved || final.NativeEntryID != priorID {
					t.Fatalf("failed wake recovery = %#v, %v", final, finalErr)
				}
				if id, err := nativePeerEntry(receipt.Attempt.SessionFile, accepted.Envelope, content); err != nil || id != priorID {
					t.Fatalf("failed wake retry native entry = %q, %v", id, err)
				}
			}
			if mode == "canceled-wake" {
				content, err := peerMessageContent(accepted.Envelope, "alice", peerBody{Text: request.Text, Summary: request.Summary})
				if err != nil {
					t.Fatal(err)
				}
				priorID, err := nativePeerEntry(receipt.Attempt.SessionFile, accepted.Envelope, content)
				if err != nil || priorID == "" {
					t.Fatalf("cancelled wake native entry = %q, %v", priorID, err)
				}
				if err := session.Close(); err != nil {
					t.Fatal(err)
				}
				if err := runtime.Close(); err != nil {
					t.Fatal(err)
				}
				if retry, _, err := store.acceptPeer(sender.Ref, request); err != nil || retry.State != peerAttempted {
					t.Fatalf("failed wake retry = %#v, %v", retry, err)
				}
				next := tree.newRunLocked(Ref{recipient.Ref.ID}, "")
				next.owner, r.run = r, next
				restarted, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, AbortContext: ctx, NewExtensions: []extension.Extension{o.peerHooks(r, next)}})
				if err != nil {
					t.Fatal(err)
				}
				defer restarted.Close()
				resumed, err := restarted.Resume(string(r.id), coding.SessionStartOptions{SessionDir: filepath.Join(directory, "sessions"), Model: services.ModelRuntime().GetModel("fixture", "fixture"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
				if err != nil {
					t.Fatal(err)
				}
				defer resumed.Close()
				next.session = resumed
				go func() {
					for event := range resumed.Events() {
						coding.AcknowledgeEvent(event)
					}
				}()
				if _, err := resumed.Send(ctx, "resume after failed wake"); err != nil {
					t.Fatal(err)
				}
				final, err := readPeerReceipt(path)
				if err != nil || final.State != peerObserved || final.NativeEntryID != priorID {
					t.Fatalf("cancelled wake retry = %#v, %v", final, err)
				}
				if id, err := nativePeerEntry(receipt.Attempt.SessionFile, accepted.Envelope, content); err != nil || id != priorID {
					t.Fatalf("same-host retry native entry = %q, %v", id, err)
				}
			}
		})
	}
}

func TestSiblingUncertainAttemptIsNotRetried(t *testing.T) {
	for _, kind := range []string{"same-live-host", "different-live-host", "different-scope", "legacy"} {
		t.Run(kind, func(t *testing.T) {
			store, _, _, workspace := storeTestOpen(t)
			sender, recipient := peerTestMembers(t, store, workspace)
			request := peerRequest{Recipient: "bob", MessageID: "uncertain", Text: "hello", Summary: "greeting"}
			if _, _, err := store.acceptPeer(sender.Ref, request); err != nil {
				t.Fatal(err)
			}
			manager := nativeTestManager(t, t.TempDir(), string(recipient.Ref.ID))
			current, err := peerObservedHost(os.Getpid())
			if err != nil {
				t.Fatal(err)
			}
			attempt := &peerDeliveryAttempt{Host: current, SessionFile: *manager.GetSessionFile()}
			if kind == "legacy" {
				if _, _, found, err := store.offerPeer(recipient.Ref); err != nil || !found {
					t.Fatalf("legacy offer found=%v, %v", found, err)
				}
			} else {
				old := *attempt
				if kind == "different-live-host" || kind == "different-scope" {
					prior, stop := peerTestHost(t)
					if kind == "different-scope" {
						prior.Scope += ":unknown"
						stop()
					}
					old.Host = prior
				}
				if _, _, found, err := store.offerPeerAttempt(recipient.Ref, &old); err != nil || !found {
					t.Fatalf("initial offer found=%v, %v", found, err)
				}
			}
			if retry, _, err := store.acceptPeer(sender.Ref, request); err != nil || retry.State != peerAttempted {
				t.Fatalf("authenticated retry = %#v, %v", retry, err)
			}
			if _, _, found, err := store.offerPeerAttempt(recipient.Ref, attempt); err != nil || found {
				t.Fatalf("uncertain sibling attempt was reoffered: found=%v, %v", found, err)
			}
			if receipt, _, err := store.acceptPeer(sender.Ref, request); err != nil || receipt.State != peerAttempted {
				t.Fatalf("uncertain receipt = %#v, %v", receipt, err)
			}
		})
	}
}
