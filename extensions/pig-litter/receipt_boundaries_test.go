package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

func nativeReceiptManager(t *testing.T, directory, agentDir, id, sessionDir string) *coding.SessionManager {
	t.Helper()
	trusted := true
	services, err := coding.NewServices(coding.ServicesOptions{CWD: directory, AgentDir: agentDir, ProjectTrusted: &trusted})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	session, err := runtime.New(coding.SessionStartOptions{SessionID: id, SessionDir: sessionDir, Model: services.ModelRuntime().GetModel("fixture", "child"), ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	manager := session.SessionManager()
	if _, err := manager.AppendMessage(agent.AgentMessage{User: &agent.UserMessage{Role: "user", Content: ai.UserContentBlocks{ai.TextContent{Text: "READER_PRIVATE_NATIVE_INPUT"}}}}); err != nil {
		t.Fatal(err)
	}
	return manager
}

func TestTypedCompletionAuditRequiresItsPersistedHandback(t *testing.T) {
	details := completionDetails{ChildID: "00000000-0000-7000-8000-000000000001", Receipt: "stable-receipt"}
	outcome := Outcome{Run: Ref{details.ChildID}, State: Completed, Text: "DURABLE_HANDBACK"}
	content, _ := completionContent(outcome)
	handback, _ := resultContent("litter_wait", map[string]any{"outcome": &outcome}, nil)
	entries := []map[string]any{
		{"id": "before-wait", "type": "custom", "customType": "fixture"},
		{"id": "receipt", "type": "custom", "customType": "litter_completion", "data": map[string]any{"content": content, "details": details, "handback": map[string]any{"callId": "wait-call", "operation": "litter_wait", "after": "before-wait"}}},
	}
	raw, _ := rawEntries(entries)
	if completionStored(raw, details, content) {
		t.Fatal("receipt without a persisted handback suppressed recovery")
	}
	entries = append(entries, map[string]any{"id": "final-wait", "type": "message", "message": map[string]any{"role": "toolResult", "toolCallId": "wait-call", "toolName": "litter_wait", "isError": false, "content": []map[string]any{{"type": "text", "text": handback}}}})
	raw, _ = rawEntries(entries)
	if !completionStored(raw, details, content) {
		t.Fatal("typed audit with its persisted final handback was not recovered")
	}
}

func TestFreshOwnersReconcileReceiptCrashForks(t *testing.T) {
	for _, stored := range []bool{false, true} {
		for _, consumed := range []bool{false, true} {
			t.Run(fmt.Sprintf("result-stored=%t/consumption-stored=%t", stored, consumed), func(t *testing.T) {
				t.Setenv("PIG_OFFLINE", "1")
				t.Setenv("PIG_CODING_AGENT_DIR", t.TempDir())
				id, err := ai.UUIDv7(nil)
				if err != nil {
					t.Fatal(err)
				}
				details := completionDetails{ChildID: ChildID(id), Receipt: "crash-receipt"}
				outcome := Outcome{Run: Ref{ChildID(id)}, State: Completed, Text: "RECOVERED_CRASH_REPORT"}
				content, _ := completionContent(outcome)
				after := "before-wait"
				handback := &outcomeHandback{CallID: "crash-wait", Operation: "litter_wait", After: &after}
				saved := savedChild{RootID: "fixture", ID: ChildID(id), ParentID: "fixture", Name: "crash-child", Kind: "scout", Cwd: t.TempDir(), Model: "fixture/child", Tools: []string{"read", "ls"}, Admitted: true, ReadOnly: true, State: Completed, Latest: &outcome, NoticeID: details.Receipt, Notices: []savedNotice{{Nonce: details.Receipt, Outcome: outcome, Consumed: consumed, Handback: handback}}}
				entries := []map[string]any{
					{"id": after, "type": "custom", "customType": "fixture"},
					{"id": "crash-snapshot", "type": "custom", "customType": childEntryType, "data": saved},
					{"id": "crash-audit", "type": "custom", "customType": "litter_completion", "data": completionAudit{Content: content, Details: details, Handback: handback}},
				}
				if stored {
					result, _ := resultContent("litter_wait", map[string]any{"outcome": &outcome}, nil)
					entries = append(entries, map[string]any{"id": "crash-tool-result", "type": "message", "message": map[string]any{"role": "toolResult", "toolName": "litter_wait", "toolCallId": "crash-wait", "isError": false, "content": []map[string]any{{"type": "text", "text": result}}}})
				}
				var sends atomic.Int32
				withCompletionHost(t, testTree(t, defaultConfig()), func(previous *owner) {
					for replacement := 0; replacement < 2; replacement++ {
						state := &extensionState{}
						o, err := state.ensure(previous.context)
						if err != nil {
							t.Fatal(err)
						}
						o.tree.completionWG.Wait()
						o.tree.mu.Lock()
						remaining := len(o.tree.pending)
						o.tree.mu.Unlock()
						state.retire()
						if remaining != 0 {
							t.Errorf("replacement retained %d resolved crash receipts", remaining)
						}
					}
				}, func(method string, raw json.RawMessage) json.RawMessage {
					switch method {
					case "sessionRead":
						var query struct{ Method string }
						_ = json.Unmarshal(raw, &query)
						if query.Method == "getSessionId" {
							return json.RawMessage(`"fixture"`)
						}
						if query.Method == "getSessionFile" {
							return json.RawMessage(`null`)
						}
						if query.Method == "getEntries" {
							data, _ := json.Marshal(entries)
							return data
						}
					case "appendEntry", "sendMessage":
						var entry map[string]any
						_ = json.Unmarshal(raw, &entry)
						if method == "sendMessage" {
							sends.Add(1)
							entry = map[string]any{"type": "message", "message": entry["message"]}
						} else {
							entry["type"] = "custom"
						}
						entry["id"] = fmt.Sprintf("stored-%d", len(entries))
						entries = append(entries, entry)
						return json.RawMessage(`{}`)
					case "isIdle":
						return json.RawMessage(`{"idle":true}`)
					}
					t.Fatalf("unexpected crash-recovery host call %s", method)
					return nil
				})
				wanted := int32(1)
				if stored {
					wanted = 0
				}
				if sends.Load() != wanted {
					t.Errorf("crash fork delivered %d notices, want %d", sends.Load(), wanted)
				}
			})
		}
	}
}

func TestConsumedCompletionStaysPendingBeforeFinalResultPersistence(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "consumed")
	selected := testRun(t, tree, child.Ref)
	tree.finish(selected.owner, selected, Outcome{State: Completed, Text: "WAIT_RESULT_NOT_STORED"})
	tree.pending[selected].outcomeObserved = true
	entries := []map[string]any{}
	withCompletionHost(t, tree, func(o *owner) {
		o.sendCompletion(selected.owner, selected, *selected.outcome)
		if len(tree.pending) != 1 {
			t.Error("an audit retired the receipt before the final tool result was persisted")
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		switch method {
		case "appendEntry":
			var entry map[string]any
			_ = json.Unmarshal(raw, &entry)
			entry["id"], entry["type"] = "audit-without-result", "custom"
			entries = append(entries, entry)
			return json.RawMessage(`{}`)
		case "sessionRead":
			var query struct{ Method string }
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getEntries" {
				data, _ := json.Marshal(entries)
				return data
			}
			if query.Method == "getSessionFile" {
				return json.RawMessage(`null`)
			}
			return json.RawMessage(`"fixture"`)
		}
		t.Fatalf("unexpected consumed-receipt host call %s", method)
		return nil
	})
}

func TestFailedNativeAppendCannotRetireACompletionFromMemory(t *testing.T) {
	tree := testTree(t, defaultConfig())
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read", "ls"}, CanDelegate: true}
	parentRef := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	childRef := testAdmission(t, tree, parentRef.Ref, "scout", "child")
	parent, child := tree.records[parentRef.ID], tree.records[childRef.ID]
	tree.finish(child, child.run, Outcome{State: Completed, Text: "RETRY_AFTER_NATIVE_WRITE_FAILURE"})
	directory, agentDir := t.TempDir(), t.TempDir()
	manager := nativeReceiptManager(t, directory, agentDir, string(parentRef.ID), "")
	parent.activeLog, parent.cwd = manager, directory
	original, err := os.ReadFile(manager.Path())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(manager.Path()); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(manager.Path(), 0o700); err != nil {
		t.Fatal(err)
	}
	withCompletionHost(t, tree, func(o *owner) {
		o.cwd, o.agentDir = directory, agentDir
		o.sendCompletion(child, child.run, *child.run.outcome)
		o.retireReceipts(parent)
		if len(tree.pending) != 2 {
			t.Error("failed native append was acknowledged from live manager memory")
		}
		if err := os.Remove(manager.Path()); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(manager.Path(), original, 0o600); err != nil {
			t.Fatal(err)
		}
		parent.activeLog = nil
		o.retireReceipts(parent)
		if tree.pending[child.run] != nil || len(parent.receipts) != 0 {
			t.Error("successful UUID retry did not retire the preserved reservation")
		}
		entries, err := o.persistedCompletionEntries(parent.id)
		if err != nil {
			t.Fatal(err)
		}
		details := completionDetails{ChildID: child.id, Receipt: child.run.noticeID}
		copies := 0
		for _, raw := range entries {
			var entry completionEntry
			if json.Unmarshal(raw, &entry) == nil && entry.CustomType == "litter_completion" && entry.Details == details {
				copies++
			}
		}
		if copies != 1 {
			t.Errorf("native UUID retry stored %d completions, want one", copies)
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method != "sessionRead" {
			t.Fatalf("unexpected failed-append host call %s", method)
		}
		return json.RawMessage(`"fixture"`)
	})
}

func TestPersistedRootReceiptReaderUsesUUIDWithoutInference(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory, agentDir := t.TempDir(), t.TempDir()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		runtimeAnswer(w, "UNEXPECTED_INFERENCE")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	id, err := ai.UUIDv7(nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := nativeReceiptManager(t, directory, agentDir, id, filepath.Join(directory, "private-root-log"))
	tree := testTree(t, defaultConfig())
	withCompletionHost(t, tree, func(o *owner) {
		o.sessionID, o.cwd, o.agentDir = id, directory, agentDir
		entries, err := o.persistedCompletionEntries(tree.root.id)
		if err != nil {
			t.Fatal(err)
		}
		data, _ := json.Marshal(entries)
		if !strings.Contains(string(data), "READER_PRIVATE_NATIVE_INPUT") || requests.Load() != 0 {
			t.Errorf("UUID receipt reader lost native history or inferred: requests=%d entries=%s", requests.Load(), data)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method != "sessionRead" {
			t.Fatalf("unexpected UUID reader host call %s", method)
		}
		var query struct{ Method string }
		_ = json.Unmarshal(raw, &query)
		var value string
		switch query.Method {
		case "getSessionFile":
			value = manager.Path()
		case "getSessionDir":
			value = manager.GetSessionDir()
		default:
			t.Fatalf("unexpected UUID reader session read %s", query.Method)
		}
		data, _ := json.Marshal(value)
		return data
	})
}

func TestAttemptedNativeInputWithAnUnavailableHeaderIsNotRecreated(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory, agentDir := t.TempDir(), t.TempDir()
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		runtimeAnswer(w, "RECREATED_CORRUPT_IDENTITY")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	id, err := ai.UUIDv7(nil)
	if err != nil {
		t.Fatal(err)
	}
	manager := nativeReceiptManager(t, directory, agentDir, id, "")
	if err := os.WriteFile(manager.Path(), []byte("INVALID_NATIVE_HEADER\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	after := "before-native-input"
	tree := testTree(t, defaultConfig())
	saved := savedChild{RootID: tree.root.id, ID: ChildID(id), ParentID: tree.root.id, Name: "attempted", Kind: "scout", Model: "fixture/child", Cwd: directory, Tools: []string{"read", "ls"}, ReadOnly: true, Admitted: true, State: Paused, NoticeID: "attempted-input", PendingTask: "ORIGINAL_TASK", PendingInputs: []pendingInput{{Text: "Task:\nORIGINAL_TASK", After: &after}}}
	host := &runtimeHost{tools: []string{"read", "ls"}, entries: []map[string]any{{"id": "attempted-metadata", "type": "custom", "customType": childEntryType, "data": saved}}}
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		if err := o.restoreChildren(); err != nil {
			t.Fatal(err)
		}
		_, ticket, err := runtimeOperation(t, o, "litter_message", "unavailable-resume", map[string]any{"id": id, "resume": true, "text": "continue"})
		if err != nil {
			t.Fatal(err)
		}
		o.settleAck(ticket, false)
		outcome, err := tree.wait(context.Background(), testRun(t, tree, Ref{ChildID(id)}), 3000)
		if err != nil || outcome.State != Unavailable || requests.Load() != 0 {
			t.Errorf("unavailable native header was recreated: %#v requests=%d %v", outcome, requests.Load(), err)
		}
	})
}
