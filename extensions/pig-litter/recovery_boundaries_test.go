package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
)

func TestRecoveryDoesNotProcessANewPendingAdmission(t *testing.T) {
	tree := testTree(t, defaultConfig())
	withCompletionHost(t, tree, func(o *owner) {
		recovered := o.recoveryBatch()
		testAdmission(t, tree, Ref{}, "scout", "new admission")
		defer func() {
			if value := recover(); value != nil {
				t.Errorf("completion recovery processed an unsettled live admission: %v", value)
			}
		}()
		o.recoverCompletions(recovered)
	}, func(method string, _ json.RawMessage) json.RawMessage {
		t.Fatalf("new pending admission triggered recovery host call %s", method)
		return nil
	})
}

func TestAcknowledgedCodemodeAdmissionSurvivesRetirementBeforeExecution(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	writeRuntimeModels(t, agentDir, "http://127.0.0.1:1")
	host := &runtimeHost{tools: []string{"read", "ls"}}
	tree := testTree(t, defaultConfig())
	var id ChildID
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		entered, release := make(chan struct{}), make(chan struct{})
		original := tree.execute
		tree.execute = func(r *record, selected *run) Outcome {
			close(entered)
			<-release
			return original(r, selected)
		}
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "script/1", map[string]any{"type": "scout", "task": "ADMITTED_BEFORE_EXECUTION", "name": "permanent", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		id = value["child"].(Snapshot).ID
		o.observeCall(nil, "litter_spawn", ticket.callID, nativeResult("litter_spawn", value, nil), false)
		o.observeToolEvent(map[string]any{"toolCallId": "script", "toolName": "codemode", "isError": true})
		<-entered
		retired := make(chan struct{})
		go func() { o.state.retire(); close(retired) }()
		<-tree.ctx.Done()
		close(release)
		<-retired
	})
	restored := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, restored, directory, agentDir, func(o *owner) {
		if err := o.restoreChildren(); err != nil {
			t.Fatal(err)
		}
		value, _, err := runtimeOperation(t, o, "litter_inspect", "inspect-admitted", map[string]any{"id": id})
		if err != nil || value["child"].(Snapshot).ID != id {
			t.Errorf("acknowledged codemode admission disappeared on replacement: %#v %v", value, err)
			return
		}
		if restored.names[string(restored.root.id)+"\x00permanent"] != id {
			t.Fatal("acknowledged admission lost its permanent name")
		}
	})
}

func TestInterruptedResumeDoesNotReturnThePreviousCompletedOutcome(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	id, err := ai.UUIDv7(nil)
	if err != nil {
		t.Fatal(err)
	}
	previous := &Outcome{Run: Ref{ChildID(id)}, State: Completed, Text: "PREVIOUS_COMPLETED_REPORT"}
	tree := testTree(t, defaultConfig())
	saved := savedChild{RootID: tree.root.id, ID: ChildID(id), ParentID: tree.root.id, Name: "interrupted", Kind: "scout", Model: "fixture/child", Cwd: directory, Tools: []string{"read", "ls"}, ReadOnly: true, Admitted: true, State: Starting, NoticeID: "interrupted-resume", Latest: previous}
	host := &runtimeHost{entries: []map[string]any{{"id": "starting-resume", "type": "custom", "customType": childEntryType, "data": saved}}}
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		if err := o.restoreChildren(); err != nil {
			t.Fatal(err)
		}
		value, _, err := runtimeOperation(t, o, "litter_wait", "wait-interrupted", map[string]any{"id": id, "timeout_ms": 1000})
		if err != nil {
			t.Fatal(err)
		}
		outcome := value["outcome"].(*Outcome)
		if outcome.State != Paused || outcome.Text == previous.Text {
			t.Errorf("interrupted continuation returned its previous completed report: %#v", outcome)
		}
	})
}

func TestAcceptedContinuationSurvivesReplacementBeforeNativeInput(t *testing.T) {
	for _, stored := range []bool{false, true} {
		t.Run(map[bool]string{false: "before native input", true: "after native input before journal clear"}[stored], func(t *testing.T) { testAcceptedContinuationRecovery(t, stored) })
	}
}

func testAcceptedContinuationRecovery(t *testing.T, stored bool) {
	t.Helper()
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	requests := make(chan string, 3)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		text := runtimeRequestText(body)
		requests <- text
		if stored && strings.Contains(text, "ACCEPTED_PENDING_CONTINUATION") && !strings.Contains(text, "FRESH_CONTINUATION") {
			<-request.Context().Done()
			return
		}
		runtimeAnswer(w, "NATIVE_REPORT")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: []string{"read", "ls"}}
	tree := testTree(t, defaultConfig())
	var id ChildID
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "initial-input", map[string]any{"type": "scout", "task": "INITIAL_TASK", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		id = value["child"].(Snapshot).ID
		o.settleAck(ticket, false)
		initial := testRun(t, tree, Ref{id})
		if outcome, err := tree.wait(context.Background(), initial, 3000); err != nil || outcome.State != Completed {
			t.Fatalf("initial native run failed: %#v %v", outcome, err)
		}
		tree.acknowledge(initial)
		<-requests
		entered, release := make(chan struct{}), make(chan struct{})
		original := tree.execute
		if !stored {
			tree.execute = func(r *record, selected *run) Outcome {
				if err := o.saveChild(r); err != nil {
					return Outcome{State: Failed, Error: err.Error()}
				}
				close(entered)
				<-release
				return original(r, selected)
			}
		}
		_, ticket, err = runtimeOperation(t, o, "litter_message", "accepted-resume", map[string]any{"id": id, "resume": true, "text": "ACCEPTED_PENDING_CONTINUATION"})
		if err != nil {
			t.Fatal(err)
		}
		o.settleAck(ticket, false)
		if stored {
			<-requests
			host.mu.Lock()
			var prefix []map[string]any
			for index, entry := range host.entries {
				data, _ := json.Marshal(entry["data"])
				var snapshot savedChild
				if json.Unmarshal(data, &snapshot) == nil && len(snapshot.PendingInputs) != 0 && snapshot.PendingInputs[0].After != nil && strings.Contains(snapshot.PendingInputs[0].Text, "ACCEPTED_PENDING_CONTINUATION") {
					prefix = append([]map[string]any{}, host.entries[:index+1]...)
				}
			}
			host.mu.Unlock()
			o.state.retire()
			if prefix == nil {
				t.Error("native input had no durable pending-input anchor")
				return
			}
			host.mu.Lock()
			host.entries = prefix
			host.mu.Unlock()
			return
		}
		<-entered
		retired := make(chan struct{})
		go func() { o.state.retire(); close(retired) }()
		<-tree.ctx.Done()
		close(release)
		<-retired
	})
	restored := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, restored, directory, agentDir, func(o *owner) {
		if err := o.restoreChildren(); err != nil {
			t.Fatal(err)
		}
		_, ticket, err := runtimeOperation(t, o, "litter_message", "continue-after-replacement", map[string]any{"id": id, "resume": true, "text": "FRESH_CONTINUATION"})
		if err != nil {
			t.Fatal(err)
		}
		o.settleAck(ticket, false)
		selected := testRun(t, restored, Ref{id})
		if outcome, err := restored.wait(context.Background(), selected, 3000); err != nil || outcome.State != Completed {
			t.Fatalf("replacement continuation failed: %#v %v", outcome, err)
		}
		restored.acknowledge(selected)
	})
	request := <-requests
	if strings.Count(request, "ACCEPTED_PENDING_CONTINUATION") != 1 || strings.Count(request, "FRESH_CONTINUATION") != 1 {
		t.Fatalf("replacement lost or duplicated accepted continuation: %s", request)
	}
}

func TestTerminalPublicationWaitsForStorageAndKeepsOneOutcome(t *testing.T) {
	for _, storageFailure := range []bool{false, true} {
		t.Run(map[bool]string{false: "stored", true: "storage error"}[storageFailure], func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			child := testAdmission(t, tree, Ref{}, "scout", "terminal")
			selected := testRun(t, tree, child.Ref)
			selected.started, selected.state = true, Running
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			tree.persist = func(*record) error {
				close(entered)
				<-release
				if storageFailure {
					return errors.New("STORAGE_UNAVAILABLE")
				}
				return nil
			}
			delivered := make(chan Outcome, 1)
			tree.completion = func(_ *record, _ *run, outcome Outcome) { delivered <- outcome }
			go func() {
				tree.finish(selected.owner, selected, Outcome{State: Completed, Text: "TERMINAL_REPORT"})
				close(finished)
			}()
			<-entered
			if outcome, err := tree.wait(context.Background(), selected, 1); err == nil || err.(*treeError).code != "wait_timeout" {
				t.Errorf("wait published before terminal storage finished: %#v %v", outcome, err)
			}
			if inspection, err := tree.inspect(child.ID); err != nil || inspection.Child.State != Running || inspection.Outcome != nil {
				t.Errorf("inspection published before terminal storage: %#v %v", inspection, err)
			}
			var resumed *run
			if _, next, err := tree.resume(nil, selected, "RESUME_DURING_STORAGE"); err == nil {
				t.Error("resume replaced a run whose terminal storage is unfinished")
				resumed = next
			}
			close(release)
			<-finished
			tree.persist = nil
			if resumed != nil {
				tree.cancelPending(resumed)
			}
			outcome, err := tree.wait(context.Background(), selected, 1000)
			if err != nil {
				t.Fatal(err)
			}
			notice := <-delivered
			if outcome != notice || outcome.Text != "TERMINAL_REPORT" {
				t.Fatalf("wait and completion published different terminal results: %#v %#v", outcome, notice)
			}
			if storageFailure && (outcome.State != Partial || !strings.Contains(outcome.Error, "STORAGE_UNAVAILABLE")) {
				t.Fatalf("terminal storage failure was lost: %#v", outcome)
			}
			if !storageFailure && outcome.State != Completed {
				t.Fatalf("stored terminal result changed: %#v", outcome)
			}
		})
	}
}
