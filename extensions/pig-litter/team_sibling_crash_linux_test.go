package piglitter

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

func TestSiblingCrashRecovery(t *testing.T) {
	if os.Getenv("P04_SIBLING_CRASH_CHILD") == "1" {
		runSiblingCrashChild(t)
		return
	}
	for _, mode := range []string{"busy-boundary", "idle-wake"} {
		for _, cut := range []string{"before-append", "after-append"} {
			t.Run(mode+"/"+cut, func(t *testing.T) {
				t.Setenv("PIG_OFFLINE", "1")
				store, agentDir, root, workspace := storeTestOpen(t)
				sender, recipient := peerTestMembers(t, store, workspace)
				request := peerRequest{Recipient: "bob", MessageID: "sibling-crash", Text: "hello", Summary: "greeting"}
				accepted, _, err := store.acceptPeer(sender.Ref, request)
				if err != nil {
					t.Fatal(err)
				}
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
				var hold atomic.Bool
				hold.Store(true)
				appended := make(chan struct{}, 1)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					body, _ := io.ReadAll(r.Body)
					if cut == "after-append" && hold.Load() && strings.Contains(string(body), "sibling-crash") {
						select {
						case appended <- struct{}{}:
						default:
						}
						<-r.Context().Done()
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, "data: {\"id\":\"peer\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"peer\",\"object\":\"chat.completion.chunk\",\"model\":\"fixture\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
				}))
				defer server.Close()
				directory := t.TempDir()
				models := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"fixture-only","models":[{"id":"fixture","name":"fixture","contextWindow":32000,"maxTokens":1024}]}}}`, server.URL+"/v1")
				if err := os.WriteFile(filepath.Join(directory, "models.json"), []byte(models), 0o600); err != nil {
					t.Fatal(err)
				}
				cmd := exec.Command(os.Args[0], "-test.run=^TestSiblingCrashRecovery$", "-test.v")
				cmd.Env = append(os.Environ(), "P04_SIBLING_CRASH_CHILD=1", "P04_SIBLING_AGENT_DIR="+agentDir, "P04_SIBLING_ROOT="+string(root), "P04_SIBLING_SESSION_DIR="+directory, "P04_SIBLING_MODE="+mode, "P04_SIBLING_CUT="+cut)
				output, err := cmd.StdoutPipe()
				if err != nil {
					t.Fatal(err)
				}
				cmd.Stderr = os.Stderr
				if err := cmd.Start(); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
				barrier := make(chan string, 1)
				go func() {
					scanner := bufio.NewScanner(output)
					for scanner.Scan() {
						if scanner.Text() == "SIBLING_BARRIER" {
							barrier <- scanner.Text()
							return
						}
					}
					barrier <- "child exited before barrier"
				}()
				if cut == "before-append" {
					select {
					case got := <-barrier:
						if got != "SIBLING_BARRIER" {
							t.Fatal(got)
						}
					case <-time.After(10 * time.Second):
						t.Fatal("child did not reach the pre-append barrier")
					}
				} else {
					select {
					case <-appended:
					case got := <-barrier:
						t.Fatalf("child stopped before native append: %s", got)
					case <-time.After(10 * time.Second):
						t.Fatal("child did not reach the post-append provider barrier")
					}
				}
				key, _ := peerKey(accepted.Envelope)
				_, receiptPath := peerPaths(store.dir, key)
				receipt, err := readPeerReceipt(receiptPath)
				if err != nil || receipt.State != peerAttempted || receipt.Attempt == nil || receipt.Attempt.Host.PID != cmd.Process.Pid {
					t.Fatalf("attempt at crash barrier = %#v, %v", receipt, err)
				}
				content, err := peerMessageContent(accepted.Envelope, "alice", peerBody{Text: request.Text, Summary: request.Summary})
				if err != nil {
					t.Fatal(err)
				}
				priorID, err := nativePeerEntry(receipt.Attempt.SessionFile, accepted.Envelope, content)
				if err != nil || (priorID != "") != (cut == "after-append") {
					t.Fatalf("native entry at %s = %q, %v", cut, priorID, err)
				}
				if err := cmd.Process.Kill(); err != nil {
					t.Fatal(err)
				}
				_ = cmd.Wait()
				hold.Store(false)
				reopened, err := openTeamStore(agentDir, root)
				if err != nil {
					t.Fatal(err)
				}
				defer reopened.Close()
				retrySiblingAfterCrash(t, reopened, root, sender, recipient, request, receipt.Attempt.SessionFile, directory, priorID, content)
			})
		}
	}
}

func runSiblingCrashChild(t *testing.T) {
	root := ChildID(os.Getenv("P04_SIBLING_ROOT"))
	store, err := openTeamStore(os.Getenv("P04_SIBLING_AGENT_DIR"), root)
	if err != nil {
		t.Fatal(err)
	}
	_, members, err := store.Snapshot()
	if err != nil || len(members) != 2 {
		t.Fatalf("members = %#v, %v", members, err)
	}
	sender, recipient := members[0], members[1]
	request := peerRequest{Recipient: "bob", MessageID: "sibling-crash", Text: "hello", Summary: "greeting"}
	if _, _, err := store.acceptPeer(sender.Ref, request); err != nil {
		t.Fatal(err)
	}
	directory := os.Getenv("P04_SIBLING_SESSION_DIR")
	trusted := true
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: directory, AgentDir: directory, ProjectTrusted: &trusted})
	if err != nil {
		t.Fatal(err)
	}
	tree := newTree(defaultConfig())
	r := &record{id: recipient.Ref.ID, team: &recipient, parent: tree.root, admitted: true}
	selected := tree.newRunLocked(Ref{r.id}, "")
	selected.owner, r.run = r, selected
	tree.records[r.id] = r
	o := &owner{tree: tree, teamStore: store}
	hooks := o.peerHooks(r, selected)
	mode, cut := os.Getenv("P04_SIBLING_MODE"), os.Getenv("P04_SIBLING_CUT")
	block := func() { fmt.Println("SIBLING_BARRIER"); select {} }
	if cut == "before-append" && mode == "busy-boundary" {
		original := hooks.Handlers["agent_before_settle"][0]
		hooks.Handlers["agent_before_settle"][0] = func(args ...any) (any, error) {
			result, err := original(args...)
			if result != nil && err == nil {
				block()
			}
			return result, err
		}
	}
	if cut == "before-append" && mode == "idle-wake" {
		hooks.Handlers["before_agent_start"] = []extension.HandlerFn{func(...any) (any, error) { block(); return nil, nil }}
	}
	if cut == "after-append" {
		delete(hooks.Handlers, "turn_start")
	}
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, AbortContext: context.Background(), NewExtensions: []extension.Extension{hooks}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := runtime.New(coding.SessionStartOptions{SessionID: string(r.id), SessionDir: filepath.Join(directory, "sessions"), Model: services.ModelRuntime().GetModel("fixture", "fixture"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	selected.session = session
	if _, err := session.SessionManager().AppendMessage(agent.AgentMessage{User: &agent.UserMessage{Role: "user", Content: ai.UserContentBlocks{ai.TextContent{Text: "retained marker"}}}}); err != nil {
		t.Fatal(err)
	}
	go func() {
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	if mode == "idle-wake" {
		selected.peerWake = &peerWakeInput{Recipient: recipient.Ref}
		err = o.sendPeerWake(session, r, selected)
	} else {
		_, err = session.Send(context.Background(), "finish the current task")
	}
	t.Fatalf("handoff returned before the crash cut: %v", err)
}

func retrySiblingAfterCrash(t *testing.T, store *teamStore, root ChildID, sender, recipient teamMember, request peerRequest, file, directory, priorID, content string) {
	t.Helper()
	trusted := true
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: directory, AgentDir: directory, ProjectTrusted: &trusted})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	senderRecord := &record{id: sender.Ref.ID, team: &sender, parent: tree.root, admitted: true}
	senderRun := tree.newRunLocked(Ref{sender.Ref.ID}, "")
	senderRun.owner, senderRecord.run = senderRecord, senderRun
	r := &record{id: recipient.Ref.ID, team: &recipient, parent: tree.root, admitted: true}
	selected := tree.newRunLocked(Ref{r.id}, "")
	selected.owner, r.run = r, selected
	tree.records[senderRecord.id], tree.records[r.id] = senderRecord, r
	withCompletionMode(t, tree, "tui", func(o *owner) {
		o.sessionID, o.teamStore = string(root), store
		runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, NewExtensions: []extension.Extension{o.peerHooks(r, selected)}})
		if err != nil {
			t.Fatal(err)
		}
		defer runtime.Close()
		session, err := runtime.Resume(string(r.id), coding.SessionStartOptions{SessionDir: filepath.Dir(file), Model: services.ModelRuntime().GetModel("fixture", "fixture"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		selected.session = session
		go func() {
			for event := range session.Events() {
				coding.AcknowledgeEvent(event)
			}
		}()
		if _, err := session.Send(context.Background(), "resume before sender authentication"); err != nil {
			t.Fatal(err)
		}
		key, _ := peerKey(peerEnvelope{Team: sender.Ref.Team, Sender: sender.Ref, Recipient: recipient.Ref, MessageID: request.MessageID})
		_, path := peerPaths(store.dir, key)
		before, err := readPeerReceipt(path)
		if err != nil || before.State != peerAttempted {
			t.Fatalf("unauthenticated resume = %#v, %v", before, err)
		}
		raw, _ := json.Marshal(request)
		result, err := o.operation(context.Background(), raw, &callTicket{caller: senderRun, operation: "team_send"}, nil)
		if err != nil || result["state"] != "accepted" || result["delivery"] != "attempted" {
			t.Fatalf("authenticated retry = %#v, %v", result, err)
		}
		if _, err := session.Send(context.Background(), "resume after sender authentication"); err != nil {
			t.Fatal(err)
		}
		final, err := readPeerReceipt(path)
		if err != nil || final.State != peerObserved || final.SessionID != recipient.Ref.ID {
			t.Fatalf("recovered receipt = %#v, %v", final, err)
		}
		id, err := nativePeerEntry(file, final.Envelope, content)
		if err != nil || id == "" || final.NativeEntryID != id || priorID != "" && id != priorID {
			t.Fatalf("recovered native entry = %q, prior %q, %v", id, priorID, err)
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method != "sessionRead" {
			t.Fatalf("unexpected host call %s", method)
		}
		value, _ := json.Marshal(root)
		return value
	})
}
