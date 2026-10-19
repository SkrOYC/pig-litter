package piglitter

import (
	"context"
	"encoding/json"
	"errors"
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
)

func awaitHuman(t *testing.T, result <-chan humanResult) humanResult {
	t.Helper()
	select {
	case result := <-result:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("human action did not settle")
		return humanResult{}
	}
}

func TestHumanSteerAndFollowUpReachNativeSessionWithObservedToolStarts(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	path := filepath.Join(directory, "input.txt")
	if err := os.WriteFile(path, []byte("NATIVE_PROGRESS_MARKER"), 0o600); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	requests := make(chan string, 8)
	entered, release := make(chan struct{}), make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requests <- string(body)
		switch calls.Add(1) {
		case 1:
			runtimeToolAnswer(w, "native-read", "read", map[string]any{"path": path})
		case 2:
			close(entered)
			select {
			case <-release:
				runtimeAnswer(w, "initial answer")
			case <-request.Context().Done():
			}
		default:
			runtimeAnswer(w, "MESSAGES_DELIVERED")
		}
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: []string{"read", "ls"}}
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "spawn", map[string]any{"type": "scout", "task": "read the fixture file", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		o.finishCall(ticket, nativeResult("litter_spawn", value, nil), false)
		selected := testRun(t, tree, value["child"].(Snapshot).Ref)
		<-entered
		for _, item := range []struct{ mode, text string }{{"steer", "HUMAN_NATIVE_STEER"}, {"follow_up", "HUMAN_NATIVE_FOLLOW_UP"}} {
			result := awaitHuman(t, o.humanAction(context.Background(), selected, humanAction{kind: humanMessage, mode: item.mode, text: item.text}))
			if result.err != nil || result.feedback != "Message queued" || result.selected != selected {
				t.Fatalf("human %s failed: %#v", item.mode, result)
			}
		}
		close(release)
		if outcome, err := tree.wait(context.Background(), selected, 5000); err != nil || outcome.State != Completed || outcome.Text != "MESSAGES_DELIVERED" {
			t.Fatalf("native messages did not settle: %#v %v", outcome, err)
		}
		last := ""
		for len(requests) != 0 {
			last = <-requests
		}
		if strings.Count(last, "HUMAN_NATIVE_STEER") != 1 || strings.Count(last, "HUMAN_NATIVE_FOLLOW_UP") != 1 || !strings.Contains(last, "NATIVE_PROGRESS_MARKER") {
			t.Fatalf("native history lost or duplicated human messages: %s", last)
		}
		panel := newChildPanel(o, nil, func() int { return 40 })
		defer panel.Dispose()
		panelKey(t, panel, "\r")
		text := strings.Join(panel.Render(100), "\n")
		if !strings.Contains(text, "Tool starts 1") || !strings.Contains(text, "Active tools none") || strings.Contains(text, "Elapsed unknown") {
			t.Fatalf("native Subscribe activity was not observed privately: %s", text)
		}
	})
}

func TestRetirementDrainsHumanActivationBeforeCompetingPassiveAcquire(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		runtimeAnswer(w, "before retirement")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: []string{"read", "ls"}}
	tree := testTree(t, defaultConfig())
	tree.root.id = "fixture"
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "spawn", map[string]any{"type": "scout", "task": "initial", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		o.finishCall(ticket, nativeResult("litter_spawn", value, nil), false)
		selected := testRun(t, tree, value["child"].(Snapshot).Ref)
		if _, err := tree.wait(context.Background(), selected, 3000); err != nil {
			t.Fatal(err)
		}
		tree.acknowledge(selected)
		entered, release := make(chan struct{}), make(chan struct{})
		var gate sync.Once
		tree.persist = func(r *record) error {
			gate.Do(func() { close(entered); <-release })
			return o.saveChild(r)
		}
		input, cancel := context.WithCancel(context.Background())
		defer cancel()
		action := o.humanAction(input, selected, humanAction{kind: humanResume, text: "MUST_NOT_ENTER_NATIVE_HISTORY"})
		<-entered
		retired := make(chan struct{})
		go func() { o.state.retire(); close(retired) }()
		o.actionMu.Lock()
		lifetime := o.actionContext
		o.actionMu.Unlock()
		<-lifetime.Done()
		secondRetirement := make(chan struct{})
		go func() { o.state.retire(); close(secondRetirement) }()
		type acquired struct {
			owner *owner
			err   error
		}
		reopened := make(chan acquired, 1)
		go func() {
			owner, err := o.state.acquire(o.context, false)
			reopened <- acquired{owner, err}
		}()
		select {
		case <-retired:
			t.Fatal("retirement skipped an in-flight human activation")
		case <-secondRetirement:
			t.Fatal("competing retirement skipped the shared retirement barrier")
		case <-reopened:
			t.Fatal("passive acquisition published before old human writers drained")
		case <-time.After(20 * time.Millisecond):
		}
		close(release)
		if result := awaitHuman(t, action); result.err == nil {
			t.Fatalf("retired owner accepted activation: %#v", result)
		}
		for _, done := range []<-chan struct{}{retired, secondRetirement} {
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("retirement deadlocked against the action or initialization lock")
			}
		}
		var next acquired
		select {
		case next = <-reopened:
		case <-time.After(3 * time.Second):
			t.Fatal("fresh passive acquisition stayed blocked after retirement")
		}
		if next.err != nil || next.owner == o || calls.Load() != 1 {
			t.Fatalf("retirement/reopen failed: owner=%p old=%p calls=%d error=%v", next.owner, o, calls.Load(), next.err)
		}
		current := testRun(t, next.owner.tree, selected.ref)
		next.owner.tree.mu.Lock()
		inputs, _ := json.Marshal(current.owner.pendingInputs)
		next.owner.tree.mu.Unlock()
		if strings.Contains(string(inputs), "MUST_NOT_ENTER_NATIVE_HISTORY") || current.state != Unavailable {
			t.Fatalf("retired activation survived restoration: state=%s inputs=%s", current.state, inputs)
		}
	})
}

func TestPanelEscapeDuringResumeStorageCancelsUnacceptedInput(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		runtimeAnswer(w, "initial native result")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: []string{"read", "ls"}}
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "spawn", map[string]any{"type": "scout", "task": "initial", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		o.finishCall(ticket, nativeResult("litter_spawn", value, nil), false)
		selected := testRun(t, tree, value["child"].(Snapshot).Ref)
		if _, err := tree.wait(context.Background(), selected, 3000); err != nil {
			t.Fatal(err)
		}
		tree.acknowledge(selected)
		entered, release := make(chan struct{}), make(chan struct{})
		var gate sync.Once
		tree.persist = func(r *record) error {
			gate.Do(func() { close(entered); <-release })
			return o.saveChild(r)
		}
		panel := newChildPanel(o, nil, func() int { return 32 })
		defer panel.Dispose()
		panelKey(t, panel, "r")
		panelKey(t, panel, "CANCELLED_DRAFT_MUST_NOT_RUN")
		panelKey(t, panel, "\r")
		<-entered
		panelKey(t, panel, "\x1b")
		close(release)
		settled := make(chan struct{})
		go func() { o.actionWG.Wait(); close(settled) }()
		select {
		case <-settled:
		case <-time.After(3 * time.Second):
			t.Fatal("cancelled panel input did not leave owner action draining")
		}
		text := strings.Join(panel.Render(100), "\n")
		inspection, _ := tree.inspect(selected.ref.ID)
		if panel.mode != panelList || panel.draft != "" || !strings.Contains(text, "Action failed") || inspection.Child.State != Unavailable || calls.Load() != 1 {
			t.Fatalf("Escape accepted pending input: mode=%d calls=%d inspection=%#v rows=%s", panel.mode, calls.Load(), inspection, text)
		}
		tree.mu.Lock()
		inputs, _ := json.Marshal(selected.owner.pendingInputs)
		tree.mu.Unlock()
		if strings.Contains(string(inputs), "CANCELLED_DRAFT_MUST_NOT_RUN") {
			t.Fatalf("cancelled draft survived in retained pending inputs: %s", inputs)
		}
	})
}

func TestHumanRejectsProvisionalModelAdmissionsAndPreservesTheirFinalAck(t *testing.T) {
	for _, resumed := range []bool{false, true} {
		for _, settlement := range []string{"success", "failure", "cancel"} {
			t.Run(fmt.Sprintf("resume=%t/%s", resumed, settlement), func(t *testing.T) {
				t.Setenv("PIG_OFFLINE", "1")
				directory := t.TempDir()
				agentDir := filepath.Join(directory, "agent")
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					calls.Add(1)
					runtimeAnswer(w, "GENUINE_ACK_RESULT")
				}))
				defer server.Close()
				writeRuntimeModels(t, agentDir, server.URL)
				host := &runtimeHost{tools: []string{"read", "ls"}}
				tree := testTree(t, defaultConfig())
				withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
					value, ticket, err := runtimeOperation(t, o, "litter_spawn", "spawn", map[string]any{"type": "scout", "task": "report", "model": "fixture/child"})
					if err != nil {
						t.Fatal(err)
					}
					selected := testRun(t, tree, value["child"].(Snapshot).Ref)
					var previous *run
					if resumed {
						o.finishCall(ticket, nativeResult("litter_spawn", value, nil), false)
						if outcome, err := tree.wait(context.Background(), selected, 3000); err != nil || outcome.Text != "GENUINE_ACK_RESULT" {
							t.Fatalf("initial result %#v %v", outcome, err)
						}
						tree.acknowledge(selected)
						previous = selected
						value, ticket, err = runtimeOperation(t, o, "litter_message", "resume", map[string]any{"id": selected.ref.ID, "resume": true, "text": "MODEL_OWNED_CONTINUATION"})
						if err != nil {
							t.Fatal(err)
						}
						selected = testRun(t, tree, selected.ref)
					}
					for _, action := range []humanAction{{kind: humanMessage, mode: "steer", text: "HUMAN_MUST_NOT_TAKE_OVER"}, {kind: humanPause}, {kind: humanResume, text: "HUMAN_MUST_NOT_TAKE_OVER"}} {
						result := awaitHuman(t, o.humanAction(context.Background(), selected, action))
						if result.err == nil || !strings.Contains(result.err.Error(), "admission is pending") {
							t.Fatalf("provisional action was not rejected: %#v", result)
						}
					}
					tree.mu.Lock()
					untouched := !selected.started && selected.state == Starting && !selected.owner.paused && selected.pendingMessages == 0
					tree.mu.Unlock()
					if !untouched {
						t.Fatal("human controls changed the model's provisional admission")
					}
					if settlement == "cancel" {
						o.retireScope(ticket.scope)
					} else {
						o.finishCall(ticket, nativeResult(ticket.operation, value, nil), settlement == "failure")
					}
					outcome, err := tree.wait(context.Background(), selected, 3000)
					if err != nil {
						t.Fatal(err)
					}
					wantCalls := int32(0)
					if resumed {
						wantCalls++
					}
					if settlement == "success" {
						wantCalls++
						if outcome.State != Completed || outcome.Text != "GENUINE_ACK_RESULT" {
							t.Fatalf("genuine success lost activation: %#v", outcome)
						}
					} else if resumed {
						if current := testRun(t, tree, selected.ref); current != previous || outcome.State != Stopped {
							t.Fatalf("failed provisional resume lost the prior run: current=%p prior=%p outcome=%#v", current, previous, outcome)
						}
					} else if len(tree.retained()) != 0 || outcome.State != Stopped {
						t.Fatalf("failed provisional spawn retained a row: %#v", outcome)
					}
					if got := calls.Load(); got != wantCalls {
						t.Fatalf("provider calls=%d want=%d", got, wantCalls)
					}
				})
			})
		}
	}
}

func TestHumanResumeStorageFailureRollsBackOnlyRejectedInputAndRetriesNativeHistory(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	requests := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		requests <- string(body)
		runtimeAnswer(w, "NATIVE_HISTORY_RETAINED")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: []string{"read", "ls"}}
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		var fail, unavailable atomic.Bool
		failedStoreNotified, releaseFinish := make(chan struct{}), make(chan struct{})
		var failureNotification sync.Once
		tree.execute = func(r *record, selected *run) Outcome {
			if unavailable.Load() {
				return Outcome{State: Unavailable, Error: "setup failed before native input"}
			}
			return o.executeRun(r, selected)
		}
		tree.persist = func(r *record) error {
			if fail.Load() {
				return errors.New("injected activation storage failure")
			}
			return o.saveChild(r)
		}
		tree.changed = func() {
			tree.mu.Lock()
			storing := false
			for _, child := range tree.records {
				storing = storing || child.run.admission == admissionStoring
			}
			tree.mu.Unlock()
			if fail.Load() && storing {
				failureNotification.Do(func() { close(failedStoreNotified); <-releaseFinish })
			}
		}
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "initial", map[string]any{"type": "scout", "task": "ORIGINAL_NATIVE_TASK", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		o.finishCall(ticket, nativeResult("litter_spawn", value, nil), false)
		initial := testRun(t, tree, value["child"].(Snapshot).Ref)
		if outcome, err := tree.wait(context.Background(), initial, 3000); err != nil || outcome.Text != "NATIVE_HISTORY_RETAINED" {
			t.Fatalf("initial native result %#v %v", outcome, err)
		}
		<-requests
		tree.acknowledge(initial)
		unavailable.Store(true)
		accepted := awaitHuman(t, o.humanAction(context.Background(), initial, humanAction{kind: humanResume, text: "EARLIER_ACCEPTED_PENDING"}))
		if accepted.err != nil || accepted.feedback != "Resumed" {
			t.Fatalf("earlier input was not accepted: %#v", accepted)
		}
		if outcome, err := tree.wait(context.Background(), accepted.selected, 3000); err != nil || outcome.State != Unavailable {
			t.Fatalf("setup result %#v %v", outcome, err)
		}
		tree.acknowledge(accepted.selected)
		unavailable.Store(false)
		fail.Store(true)
		rejectedAction := o.humanAction(context.Background(), accepted.selected, humanAction{kind: humanResume, text: "REJECTED_UI_CONTINUATION"})
		<-failedStoreNotified
		failing := testRun(t, tree, initial.ref)
		steer := awaitHuman(t, o.humanAction(context.Background(), failing, humanAction{kind: humanMessage, mode: "steer", text: "MUST_NOT_ENTER_FAILED_ADMISSION"}))
		if steer.err == nil || !strings.Contains(steer.err.Error(), "admission is pending") {
			t.Fatalf("storage-rejected activation accepted a message before settlement: %#v", steer)
		}
		close(releaseFinish)
		rejected := awaitHuman(t, rejectedAction)
		if rejected.err == nil || !strings.Contains(rejected.err.Error(), "injected activation storage failure") {
			t.Fatalf("activation failure was reported as accepted: %#v", rejected)
		}
		inspection, err := tree.inspect(initial.ref.ID)
		if err != nil || inspection.Child.State != Unavailable || inspection.Outcome.State != Unavailable {
			t.Fatalf("failed activation did not finalize: %#v %v", inspection, err)
		}
		tree.mu.Lock()
		inputs, _ := json.Marshal(initial.owner.pendingInputs)
		tree.mu.Unlock()
		if string(inputs) != `[{"text":"EARLIER_ACCEPTED_PENDING"}]` {
			t.Fatalf("rejected input damaged earlier accepted work: %s", inputs)
		}
		select {
		case request := <-requests:
			t.Fatalf("rejected activation started native inference: %s", request)
		default:
		}
		fail.Store(false)
		tree.acknowledge(rejected.selected)
		retry := awaitHuman(t, o.humanAction(context.Background(), rejected.selected, humanAction{kind: humanResume, text: "ACCEPTED_RETRY"}))
		if retry.err != nil || retry.selected.ref.ID != initial.ref.ID {
			t.Fatalf("explicit retry failed: %#v", retry)
		}
		if outcome, err := tree.wait(context.Background(), retry.selected, 3000); err != nil || outcome.Text != "NATIVE_HISTORY_RETAINED" {
			t.Fatalf("retry native result %#v %v", outcome, err)
		}
		request := <-requests
		for _, marker := range []string{"ORIGINAL_NATIVE_TASK", "EARLIER_ACCEPTED_PENDING", "ACCEPTED_RETRY"} {
			if copies := strings.Count(request, marker); copies != 1 {
				t.Fatalf("native history contains %s %d times: %s", marker, copies, request)
			}
		}
		if strings.Contains(request, "REJECTED_UI_CONTINUATION") || lastInput(t, request) != "EARLIER_ACCEPTED_PENDING\n\nResume instructions:\nACCEPTED_RETRY" {
			t.Fatalf("native retry replayed rejected instructions: %s", request)
		}
	})
}

func TestRootAbortReplayCannotRepauseHumanResumedWorkerOrItsExplicitChildResume(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { runtimeAnswer(w, "unused") }))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: append([]string{"read", "ls"}, controlNames...)}
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"read", "ls"}, CanDelegate: true}
		worker := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "worker", coordinator)
		grandchild := testAdmission(t, tree, worker.Ref, "scout", "grandchild")
		sibling := testAdmission(t, tree, Ref{}, "scout", "sibling")
		oldWorker, oldGrandchild, oldSibling := testRun(t, tree, worker.Ref), testRun(t, tree, grandchild.Ref), testRun(t, tree, sibling.Ref)
		tree.execute = func(_ *record, selected *run) Outcome {
			<-selected.ctx.Done()
			return Outcome{State: Stopped}
		}
		for _, selected := range []*run{oldWorker, oldGrandchild, oldSibling} {
			if err := tree.activate(selected); err != nil {
				t.Fatal(err)
			}
		}
		signal, cancel := context.WithCancel(context.Background())
		o.watchRoot(signal)
		cancel()
		o.settleRoot()
		for _, selected := range []*run{oldWorker, oldGrandchild, oldSibling} {
			tree.acknowledge(selected)
		}
		resumed := awaitHuman(t, o.humanAction(context.Background(), oldWorker, humanAction{kind: humanResume, text: "resume selected worker"}))
		if resumed.err != nil || resumed.selected == oldWorker || resumed.selected.ref.ID != worker.ID {
			t.Fatalf("selected human resume failed: %#v", resumed)
		}
		for range 8 {
			o.settleRoot()
			o.watchRoot(signal)
		}
		tree.mu.Lock()
		isolated := tree.root.paused && !resumed.selected.owner.paused && oldGrandchild.owner.paused && oldSibling.owner.paused && oldGrandchild.owner.run == oldGrandchild && oldSibling.owner.run == oldSibling && resumed.selected.ctx.Err() == nil
		tree.mu.Unlock()
		if !isolated {
			t.Fatal("abort replay repaused selected work or resumed another node")
		}
		if _, err := tree.selectOwned(nil, worker.ID); err == nil {
			t.Fatal("paused root model caller regained control")
		}
		if result := awaitHuman(t, o.humanAction(context.Background(), oldGrandchild, humanAction{kind: humanResume, text: "root cannot reach grandchild"})); result.err == nil {
			t.Fatal("human root gained grandchild authority")
		}
		ticket := testCall(t, o, resumed.selected, "nested-resume", "litter_message")
		raw, _ := json.Marshal(map[string]any{"id": grandchild.ID, "resume": true, "text": "explicit parent-owned continuation"})
		value, err := o.operation(context.Background(), raw, ticket, nil)
		if err != nil {
			t.Fatalf("active immediate parent could not resume below paused root: %v", err)
		}
		nested := testRun(t, tree, grandchild.Ref)
		o.finishCall(ticket, nativeResult("litter_message", value, nil), false)
		tree.mu.Lock()
		valid := tree.root.paused && !nested.owner.paused && nested.started && nested.ref.ID == grandchild.ID && oldSibling.owner.paused
		tree.mu.Unlock()
		if !valid {
			t.Fatal("model-owned nested resume changed root or sibling pause state")
		}
		if _, _, err := tree.message(context.Background(), oldWorker, nested, "resume", "stale parent"); err == nil {
			t.Fatal("stale parent retained authority after human replacement")
		}
		newSignal, newCancel := context.WithCancel(context.Background())
		o.watchRoot(newSignal)
		newCancel()
		o.settleRoot()
		inspection, _ := tree.inspect(worker.ID)
		if inspection.Child.State != Paused || resumed.selected.ctx.Err() == nil || nested.ctx.Err() == nil {
			t.Fatalf("new root cancellation did not win: %#v", inspection)
		}
	})
}

func TestClosedPanelLeavesAcceptedNativeRunAliveAndRejectsStaleActions(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	entered, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if calls.Add(1) == 2 {
			close(entered)
			select {
			case <-release:
			case <-request.Context().Done():
				return
			}
		}
		runtimeAnswer(w, "ACCEPTED_WORK_FINISHED")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: []string{"read", "ls"}}
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "spawn", map[string]any{"type": "scout", "task": "first native turn", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		o.finishCall(ticket, nativeResult("litter_spawn", value, nil), false)
		old := testRun(t, tree, value["child"].(Snapshot).Ref)
		if _, err := tree.wait(context.Background(), old, 3000); err != nil {
			t.Fatal(err)
		}
		tree.acknowledge(old)
		panel := newChildPanel(o, nil, func() int { return 24 })
		result := awaitHuman(t, o.humanAction(panel.ctx, old, humanAction{kind: humanResume, text: "accepted native continuation"}))
		if result.err != nil {
			t.Fatal(result.err)
		}
		<-entered
		panel.Dispose()
		if result.selected.ctx.Err() != nil {
			t.Fatal("panel closure cancelled accepted native work")
		}
		stale := awaitHuman(t, o.humanAction(context.Background(), old, humanAction{kind: humanPause}))
		if stale.err == nil || !strings.Contains(stale.err.Error(), "no longer current") {
			t.Fatalf("stale selection was silently retargeted: %#v", stale)
		}
		close(release)
		if outcome, err := tree.wait(context.Background(), result.selected, 3000); err != nil || outcome.State != Completed || outcome.Text != "ACCEPTED_WORK_FINISHED" {
			t.Fatalf("closed-panel native result %#v %v", outcome, err)
		}
		o.state.mu.Lock()
		o.state.epoch++
		o.state.mu.Unlock()
		retired := awaitHuman(t, o.humanAction(context.Background(), result.selected, humanAction{kind: humanPause}))
		if retired.err == nil || !strings.Contains(retired.err.Error(), "retired") {
			t.Fatalf("stale owner retained human control: %#v", retired)
		}
	})
}
