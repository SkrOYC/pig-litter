package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"
)

func TestShutdownLifecycleRefusalApprovalAndRecovery(t *testing.T) {
	store, _, _, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	first, notice, created, err := store.requestShutdown(member.Ref, "shutdown-1", "Please stop", 60000)
	if err != nil || !created || notice.Delivery != "pending" {
		t.Fatalf("request: %#v %#v %v", first, notice, err)
	}
	retry, _, created, err := store.requestShutdown(member.Ref, "shutdown-1", "Please stop", 60000)
	if err != nil || created || retry.ID != first.ID {
		t.Fatalf("retry: %#v %v", retry, err)
	}
	if _, _, _, err := store.requestShutdown(member.Ref, "shutdown-1", "Different reason", 60000); err == nil {
		t.Fatal("conflicting retry accepted")
	}
	if _, _, err := store.respondShutdown(first.ID, "execution-1", "reject", ""); err == nil {
		t.Fatal("unreasoned refusal accepted")
	}
	rejected, feedback, err := store.respondShutdown(first.ID, "execution-1", "reject", "Finishing important work")
	if err != nil || rejected.State != "rejected" || rejected.Response == "" || feedback.Recipient != "lead" {
		t.Fatalf("rejection: %#v %#v %v", rejected, feedback, err)
	}
	if _, _, err := store.respondShutdown(first.ID, "execution-1", "approve", ""); err == nil {
		t.Fatal("refusal replay accepted")
	}
	second, _, _, err := store.requestShutdown(member.Ref, "shutdown-2", "Stop after your current work", 60000)
	if err != nil || second.ID.Sequence != 2 || second.ID.Nonce == first.ID.Nonce {
		t.Fatalf("second request: %#v %v", second, err)
	}
	wrong := second.ID
	wrong.Nonce = first.ID.Nonce
	if _, _, err := store.respondShutdown(wrong, "execution-2", "approve", ""); err == nil {
		t.Fatal("wrong identity accepted")
	}
	approved, _, err := store.respondShutdown(second.ID, "execution-2", "approve", "")
	if err != nil || approved.State != "approved" {
		t.Fatalf("approval: %#v %v", approved, err)
	}
	if _, err := store.finishShutdown(second.ID, "wrong-execution", true); err == nil {
		t.Fatal("wrong execution terminated")
	}
	phase, err := store.recoverLifecycle(member.Ref, "closing")
	if err != nil || phase != "recovery_pending" {
		t.Fatalf("recovery: %q %v", phase, err)
	}
	state, err := store.readLifecycle(member.Ref)
	if err != nil || state.Requests[1].State != "interrupted" || state.Phase != "recovery_pending" {
		t.Fatalf("recovered state: %#v %v", state, err)
	}
	if _, err := store.finishShutdown(second.ID, "execution-2", true); err == nil {
		t.Fatal("interrupted shutdown acknowledged")
	}
}

func TestShutdownBoundaryRejectsContradictionsAndProtectsBatch(t *testing.T) {
	invalid := []string{
		`{"action":"respond","requestId":{},"decision":"approve","reason":"also reject"}`,
		`{"action":"list","reason":"extra"}`,
		`{"action":"list","action":"request"}`,
	}
	for _, raw := range invalid {
		if _, err := parseShutdownCommand(json.RawMessage(raw)); err == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
	if err := validateShutdownCommand(shutdownCommand{Action: "respond", Decision: "approve", Reason: "contradiction"}); err == nil {
		t.Fatal("trusted domain accepted invalid response")
	}
	if err := validateShutdownCommand(shutdownCommand{Action: "list", Reason: "unexpected"}); err == nil {
		t.Fatal("trusted domain accepted unrelated fields")
	}
	tree := testTree(t, defaultConfig())
	member := &record{controlPhase: "closing", team: &teamMember{}, parent: tree.root}
	selected := &run{shutdown: &shutdownFence{Batch: map[string]string{"approved-call": "team_shutdown", "held-call": "bash"}}}
	member.run = selected
	o := &owner{tree: tree}
	before := o.shutdownHook(member, selected)
	if before(context.Background(), "held-call", "bash", nil).Block {
		t.Fatal("current batch was blocked")
	}
	if !before(context.Background(), "later-call", "bash", nil).Block {
		t.Fatal("later batch was allowed")
	}
	if !before(context.Background(), "approved-call", "litter_spawn", nil).Block {
		t.Fatal("new descendant work was allowed")
	}
	child := &record{parent: member}
	if !o.shutdownHook(child, &run{})(context.Background(), "spawn", "litter_spawn", nil).Block {
		t.Fatal("descendant admission was allowed")
	}
}

func TestShutdownAuthorityUsesCurrentMemberAndFullIdentity(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	makeMember := func(id, admission int, name string) teamMember {
		member, err := store.Reserve(storeTestUUID(id), string(storeTestUUID(admission)), name, workspace, false)
		if err != nil {
			t.Fatal(err)
		}
		member, err = store.Commit(member.AdmissionID)
		if err != nil {
			t.Fatal(err)
		}
		return member
	}
	member := makeMember(2, 3, "first")
	other := makeMember(4, 5, "second")
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	definition := AgentDefinition{Role: "worker", Tools: []string{"read"}}
	admit := func(member teamMember) *run {
		_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: definition, member: &member})
		if err != nil {
			t.Fatal(err)
		}
		selected.owner.admitted = true
		return selected
	}
	selected, outsider := admit(member), admit(other)
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		human := controlAuthority{human: func() error { return nil }}
		command := shutdownCommand{Action: "request", Member: member.Ref, SubmissionID: "authority", Reason: "Please stop", TimeoutMs: 60000}
		if _, err := o.applyShutdown(controlAuthority{caller: outsider}, command); err == nil {
			exerciseErr = errors.New("teammate requested another member shutdown")
			return
		}
		value, err := o.applyShutdown(human, command)
		if err != nil {
			exerciseErr = err
			return
		}
		request := value["request"].(shutdownRequest)
		response := shutdownCommand{Action: "respond", RequestID: request.ID, Decision: "reject", Reason: "Still working"}
		if _, err := o.applyShutdown(human, response); err == nil {
			exerciseErr = errors.New("lead responded for member")
			return
		}
		if _, err := o.applyShutdown(controlAuthority{caller: outsider}, response); err == nil {
			exerciseErr = errors.New("wrong member responded")
			return
		}
		stale := response
		stale.RequestID.Member.Generation++
		if _, err := o.applyShutdown(controlAuthority{caller: selected}, stale); err == nil {
			exerciseErr = errors.New("stale member generation responded")
			return
		}
		value, err = o.applyShutdown(controlAuthority{caller: selected}, response)
		if err != nil || value["state"] != "rejected" {
			exerciseErr = fmt.Errorf("legitimate refusal: %#v %v", value, err)
			return
		}
		if _, err := o.applyShutdown(controlAuthority{caller: selected}, response); err == nil {
			exerciseErr = errors.New("replayed response accepted")
			return
		}
		o.tree.teamWG.Wait()
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			var query struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getEntries" {
				return json.RawMessage(`[]`)
			}
			return json.RawMessage(`"fixture"`)
		}
		return json.RawMessage(`null`)
	})
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func TestShutdownWaitsForDescendantSettlement(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	definition := AgentDefinition{Role: "worker", Tools: []string{"read"}, CanDelegate: true}
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: definition, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	selected.owner.admitted = true
	_, child, err := tree.admit(admission{parentRun: selected, kind: "worker", name: "nested", cwd: t.TempDir(), task: "work", agent: definition, available: []string{"read"}})
	if err != nil {
		t.Fatal(err)
	}
	request, _, _, err := store.requestShutdown(member.Ref, "settle", "Please stop", 60000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.respondShutdown(request.ID, selected.noticeID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(member.Ref, "closing"); err != nil {
		t.Fatal(err)
	}
	selected.shutdown = &shutdownFence{Request: request.ID, Drained: true}
	selected.owner.controlPhase = "closing"
	selected.state, selected.outcome = Completed, &Outcome{Run: selected.ref, State: Completed}
	selected.owner.latest = selected.outcome
	close(selected.done)
	close(selected.teamFinishDone)
	child.state, child.finishing = Completed, true
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		finished := make(chan struct{})
		go func() { o.settleShutdown(selected.owner, selected); close(finished) }()
		select {
		case <-finished:
			exerciseErr = errors.New("shutdown acknowledged while a terminal-looking descendant was still settling")
			return
		case <-time.After(100 * time.Millisecond):
		}
		state, err := store.readLifecycle(member.Ref)
		if err != nil || state.Phase != "closing" {
			exerciseErr = fmt.Errorf("while descendant settles: %#v %v", state, err)
			return
		}
		child.finishing = false
		close(child.done)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			exerciseErr = errors.New("shutdown did not settle after descendant")
			return
		}
		state, err = store.readLifecycle(member.Ref)
		if err != nil || state.Phase != "terminated" {
			exerciseErr = fmt.Errorf("after descendant settles: %#v %v", state, err)
		}
		o.tree.teamWG.Wait()
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			var query struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getEntries" {
				return json.RawMessage(`[]`)
			}
			return json.RawMessage(`"fixture"`)
		}
		return json.RawMessage(`null`)
	})
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func TestShutdownDoesNotExposeTerminalBeforeStorage(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	selected.owner.admitted = true
	request, _, _, err := store.requestShutdown(member.Ref, "storage", "Please stop", 60000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.respondShutdown(request.ID, selected.noticeID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(member.Ref, "closing"); err != nil {
		t.Fatal(err)
	}
	selected.shutdown = &shutdownFence{Request: request.ID, Drained: true}
	selected.owner.controlPhase = "closing"
	selected.state, selected.outcome = Completed, &Outcome{Run: selected.ref, State: Completed}
	selected.owner.latest = selected.outcome
	close(selected.done)
	close(selected.teamFinishDone)
	entered, release := make(chan struct{}), make(chan struct{})
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		finished := make(chan struct{})
		go func() { o.settleShutdown(selected.owner, selected); close(finished) }()
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			exerciseErr = errors.New("terminal storage was not reached")
			close(release)
			return
		}
		inspection, err := tree.inspect(member.Ref.ID)
		if err != nil || inspection.Child.State == Terminated || inspection.Outcome.State == Terminated {
			exerciseErr = fmt.Errorf("terminal state exposed before storage: %#v %v", inspection, err)
		}
		lifecycle, err := store.readLifecycle(member.Ref)
		if err != nil || lifecycle.Phase != "closing" {
			exerciseErr = fmt.Errorf("terminal ledger exposed before storage: %#v %v", lifecycle, err)
		}
		_, members, err := store.Snapshot()
		if err != nil || len(members) != 1 || members[0].State != "closing" {
			exerciseErr = fmt.Errorf("terminal roster exposed before storage: %#v %v", members, err)
		}
		close(release)
		select {
		case <-finished:
		case <-time.After(5 * time.Second):
			exerciseErr = errors.New("terminal storage did not complete")
			return
		}
		inspection, err = tree.inspect(member.Ref.ID)
		if err != nil || inspection.Child.State != Terminated {
			exerciseErr = fmt.Errorf("terminal state missing after storage: %#v %v", inspection, err)
		}
		o.tree.teamWG.Wait()
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method == "appendEntry" {
			close(entered)
			<-release
			return json.RawMessage(`null`)
		}
		if method == "sessionRead" {
			var query struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getEntries" {
				return json.RawMessage(`[]`)
			}
			return json.RawMessage(`"fixture"`)
		}
		return json.RawMessage(`null`)
	})
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func TestShutdownRosterSyncFailureDoesNotAcknowledge(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	selected.owner.admitted = true
	request, _, _, err := store.requestShutdown(member.Ref, "sync-failure", "Please stop", 60000)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.respondShutdown(request.ID, selected.noticeID, "approve", ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(member.Ref, "closing"); err != nil {
		t.Fatal(err)
	}
	selected.shutdown = &shutdownFence{Request: request.ID, Drained: true}
	selected.owner.controlPhase = "closing"
	selected.state, selected.outcome = Completed, &Outcome{Run: selected.ref, State: Completed}
	selected.owner.latest = selected.outcome
	close(selected.done)
	close(selected.teamFinishDone)
	var calls atomic.Int32
	var failing atomic.Bool
	failing.Store(true)
	store.syncDir = func(path string) error {
		if calls.Add(1) >= 2 && failing.Load() {
			return errors.New("injected roster sync failure")
		}
		return syncDir(path)
	}
	t.Cleanup(func() { failing.Store(false) })
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		o.settleShutdown(selected.owner, selected)
		inspection, err := tree.inspect(member.Ref.ID)
		if err != nil || inspection.Child.State == Terminated {
			exerciseErr = fmt.Errorf("failed roster sync exposed termination: %#v %v", inspection, err)
			return
		}
		failing.Store(false)
		_, err = o.applyShutdown(controlAuthority{human: func() error { return nil }}, shutdownCommand{Action: "get", RequestID: request.ID})
		if err == nil {
			exerciseErr = errors.New("partially committed shutdown was acknowledged")
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			var query struct {
				Method string `json:"method"`
			}
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getEntries" {
				return json.RawMessage(`[]`)
			}
			return json.RawMessage(`"fixture"`)
		}
		return json.RawMessage(`null`)
	})
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}
