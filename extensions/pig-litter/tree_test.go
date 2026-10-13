package piglitter

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

func testTree(t *testing.T, config Config) *Tree {
	t.Helper()
	tree := newTree(config)
	t.Cleanup(tree.close)
	return tree
}

func testCaller(selected *run) *run {
	return selected.owner.parent.run
}

func testRun(t *testing.T, tree *Tree, ref Ref) *run {
	t.Helper()
	selected, err := tree.selectRun(ref.ID)
	if err != nil {
		t.Fatal(err)
	}
	return selected
}

func testAdmission(t *testing.T, tree *Tree, parent Ref, kind, name string) Snapshot {
	definition, ok := tree.config.Agents[kind]
	if !ok {
		t.Fatalf("unknown test agent %q", kind)
	}
	return testAdmissionWithAgent(t, tree, parent, kind, name, definition)
}

func testAdmissionWithAgent(t *testing.T, tree *Tree, parent Ref, kind, name string, definition AgentDefinition) Snapshot {
	t.Helper()
	manager, err := coding.NewInMemorySessionManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendCustomMessage("fixture", "retained marker", false, nil); err != nil {
		t.Fatal(err)
	}
	var parentRun *run
	if parent.ID != "" {
		parentRun = testRun(t, tree, parent)
	}
	snapshot, _, err := tree.admit(admission{parentRun: parentRun, kind: kind, name: name, model: "fixture/child", cwd: "/fixture", task: "task", agent: definition, history: manager})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestAdmissionCountsDescendantsAndRejectsDepthWritersAndNames(t *testing.T) {
	config := defaultConfig()
	config.Concurrency = 2
	tree := testTree(t, config)
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate child tasks", Tools: []string{"read"}, CanDelegate: true}
	parent := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	child := testAdmissionWithAgent(t, tree, parent.Ref, "coordinator", "child", coordinator)
	if child.Depth != 2 || child.Parent == nil || *child.Parent != parent.ID {
		t.Fatalf("grandchild %#v", child)
	}
	if _, _, err := tree.admit(admission{agent: config.Agents["scout"]}); err == nil {
		t.Fatal("accepted a third live descendant")
	}
	if _, _, err := tree.admit(admission{parentRun: testRun(t, tree, child.Ref), agent: config.Agents["scout"]}); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("depth rejection %v", err)
	}
	writers := testTree(t, defaultConfig())
	writer := testAdmission(t, writers, Ref{}, "worker", "writer")
	if _, _, err := writers.admit(admission{cwd: "/fixture", agent: writers.config.Agents["worker"]}); err == nil || !strings.Contains(err.Error(), "writer conflict") {
		t.Fatalf("writer rejection %v", err)
	}
	if _, _, err := writers.stop(nil, testRun(t, writers, writer.Ref)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := writers.admit(admission{name: "writer", agent: writers.config.Agents["scout"]}); err == nil || !strings.Contains(err.Error(), "name already") {
		t.Fatalf("name rejection %v", err)
	}
}

func TestAdmissionUsesEffectiveRoleForDelegationAndBashForWriterConflict(t *testing.T) {
	tree := testTree(t, defaultConfig())
	scoutNameWorker := AgentDefinition{Role: "worker", Instructions: "delegate within the read ceiling", Tools: []string{"read"}, CanDelegate: true}
	parent := testAdmissionWithAgent(t, tree, Ref{}, "scout", "coordinator", scoutNameWorker)
	if _, _, err := tree.admit(admission{parentRun: testRun(t, tree, parent.Ref), agent: tree.config.Agents["scout"]}); err != nil {
		t.Fatalf("worker role named scout could not delegate: %v", err)
	}

	leaf := testAdmissionWithAgent(t, tree, Ref{}, "worker", "leaf", AgentDefinition{Role: "scout", Instructions: "inspect only", Tools: []string{"read"}, CanDelegate: true})
	if _, _, err := tree.admit(admission{parentRun: testRun(t, tree, leaf.Ref), agent: tree.config.Agents["scout"]}); err == nil || !strings.Contains(err.Error(), "cannot delegate") {
		t.Fatalf("scout role delegated under a different name: %v", err)
	}

	writers := testTree(t, defaultConfig())
	bashOnly := AgentDefinition{Role: "worker", Instructions: "run commands", Tools: []string{"bash"}}
	testAdmissionWithAgent(t, writers, Ref{}, "worker", "shell", bashOnly)
	if _, _, err := writers.admit(admission{agent: writers.config.Agents["worker"], cwd: "/fixture"}); err == nil || !strings.Contains(err.Error(), "writer conflict") {
		t.Fatalf("bash-only child did not reserve the shared directory for writes: %v", err)
	}
}

func TestWaitCancellationAndTimeoutLeaveChildRunning(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "held")
	selected := testRun(t, tree, child.Ref)
	if _, err := tree.wait(context.Background(), selected, 1); err == nil || err.(*treeError).code != "wait_timeout" {
		t.Fatalf("wait timeout %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tree.wait(ctx, selected, 1000); err == nil || err.(*treeError).code != "cancelled" {
		t.Fatalf("cancelled wait %v", err)
	}
	list, err := tree.list(nil)
	if err != nil || len(list) != 1 || list[0].State != Starting {
		t.Fatalf("child changed %#v %v", list, err)
	}
	outcome, count, err := tree.stop(testCaller(selected), selected)
	if err != nil || count != 1 || outcome.State != Paused {
		t.Fatalf("stop %#v %d %v", outcome, count, err)
	}
	if _, count, err := tree.stop(testCaller(selected), selected); err != nil || count != 0 {
		t.Fatalf("repeat stop %d %v", count, err)
	}
}

func TestResumeRetainsHistoryAndStaleRunCallbacksCannotAffectReplacement(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "retained")
	original := testRun(t, tree, child.Ref)
	manager := tree.records[child.ID].history
	tree.finish(tree.records[child.ID], original, Outcome{State: Completed, Text: "before resume"})
	tree.acknowledge(original)
	inspection, err := tree.inspect(child.ID)
	if err != nil || inspection.Child.State != Completed || inspection.Outcome == nil || inspection.Outcome.Text != "before resume" {
		t.Fatalf("inspect did not return current result %#v %v", inspection, err)
	}
	if tree.records[child.ID].history != manager {
		t.Fatal("initial completion replaced retained history")
	}

	started := make(chan struct{})
	results := make(chan struct {
		snapshot Snapshot
		selected *run
		err      error
	}, 2)
	for range 2 {
		go func() {
			<-started
			snapshot, selected, err := tree.message(context.Background(), testCaller(original), original, "resume", "continue")
			results <- struct {
				snapshot Snapshot
				selected *run
				err      error
			}{snapshot, selected, err}
		}()
	}
	close(started)
	var resumed Snapshot
	var replacement *run
	successes, failures := 0, 0
	for range 2 {
		result := <-results
		if result.err != nil {
			failures++
			continue
		}
		successes++
		resumed, replacement = result.snapshot, result.selected
	}
	if successes != 1 || failures != 1 || resumed.ID != child.ID || replacement == nil || replacement == original {
		t.Fatalf("concurrent resume results successes=%d failures=%d snapshot=%#v", successes, failures, resumed)
	}
	if tree.records[child.ID].history != manager {
		t.Fatal("resume replaced the retained manager")
	}
	if err := tree.activate(original); err == nil {
		t.Fatal("stale run activated its replacement")
	}
	tree.cancelPending(original)
	if _, _, err := tree.stop(testCaller(original), original); err == nil {
		t.Fatal("stale run stopped its replacement")
	}
	if outcome, err := tree.wait(context.Background(), original, 1); err != nil || outcome.State != Completed || outcome.Text != "before resume" {
		t.Fatalf("wait did not retain its selected run: %#v %v", outcome, err)
	}
	if _, err := tree.wait(context.Background(), replacement, 1); err == nil || err.(*treeError).code != "wait_timeout" {
		t.Fatalf("stale cancellation changed the replacement: %v", err)
	}
	if current := testRun(t, tree, child.Ref); current != replacement {
		t.Fatal("stale run callback replaced the current execution")
	}
}

func TestCleanupKeepsCapacityUntilItJoins(t *testing.T) {
	config := defaultConfig()
	config.Concurrency = 1
	tree := testTree(t, config)
	child := testAdmission(t, tree, Ref{}, "scout", "owned")
	entered, cleanup, cleaned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	tree.execute = func(_ *record, run *run) Outcome {
		close(entered)
		<-run.ctx.Done()
		<-cleanup
		close(cleaned)
		return Outcome{State: Stopped}
	}
	selected := testRun(t, tree, child.Ref)
	if err := tree.activate(selected); err != nil {
		t.Fatal(err)
	}
	<-entered
	stopped := make(chan struct{})
	go func() { _, _, _ = tree.stop(testCaller(selected), selected); close(stopped) }()
	select {
	case <-cleaned:
		t.Fatal("cleanup completed before release")
	default:
	}
	if _, _, err := tree.admit(admission{agent: config.Agents["scout"]}); err == nil {
		t.Fatal("capacity released before cleanup")
	}
	close(cleanup)
	<-stopped
	if outcome, err := tree.wait(context.Background(), selected, 1000); err != nil || outcome.State != Paused {
		t.Fatalf("joined stop %#v %v", outcome, err)
	}
}

func TestSubtreeOwnershipAndStopAreScopedToSelectedRun(t *testing.T) {
	tree := testTree(t, defaultConfig())
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate child tasks", Tools: []string{"read"}, CanDelegate: true}
	parent := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	child := testAdmissionWithAgent(t, tree, parent.Ref, "coordinator", "child", coordinator)
	sibling := testAdmission(t, tree, Ref{}, "scout", "sibling")
	list, err := tree.list(testRun(t, tree, parent.Ref))
	if err != nil || len(list) != 1 || list[0].ID != child.ID {
		t.Fatalf("owned list %#v %v", list, err)
	}
	for _, id := range []ChildID{parent.ID, sibling.ID} {
		if _, err := tree.selectOwned(testRun(t, tree, parent.Ref), id); err == nil {
			t.Fatalf("accepted unrelated child %s", id)
		}
	}
	started := make(chan struct{}, 2)
	tree.execute = func(_ *record, run *run) Outcome {
		started <- struct{}{}
		<-run.ctx.Done()
		return Outcome{State: Stopped}
	}
	parentRun := testRun(t, tree, parent.Ref)
	childRun := testRun(t, tree, child.Ref)
	if err := tree.activate(parentRun); err != nil {
		t.Fatal(err)
	}
	if err := tree.activate(childRun); err != nil {
		t.Fatal(err)
	}
	<-started
	<-started
	results := make(chan int, 2)
	for range 2 {
		go func() { _, count, _ := tree.stop(testCaller(parentRun), parentRun); results <- count }()
	}
	if total := <-results + <-results; total != 2 {
		t.Fatalf("concurrent stop counted %d", total)
	}
	list, _ = tree.list(nil)
	for _, snapshot := range list {
		if snapshot.ID == sibling.ID && snapshot.State != Starting {
			t.Fatalf("stop affected sibling %#v", snapshot)
		}
	}
}

func TestCompletionReservationWaitsForActualAppend(t *testing.T) {
	config := defaultConfig()
	config.MaxMailbox = 1
	tree := testTree(t, config)
	coordinator := AgentDefinition{Role: "worker", Instructions: "coordinate child tasks", Tools: []string{"read"}, CanDelegate: true}
	parent := testAdmissionWithAgent(t, tree, Ref{}, "coordinator", "parent", coordinator)
	child := testAdmission(t, tree, parent.Ref, "scout", "child")
	selected := testRun(t, tree, child.Ref)
	tree.finish(tree.records[child.ID], selected, Outcome{State: Completed, Text: "first"})
	if _, _, err := tree.message(context.Background(), testCaller(selected), selected, "resume", "again"); err == nil || !strings.Contains(err.Error(), "mailbox capacity") {
		t.Fatalf("unappended completion released capacity %v", err)
	}
	if !tree.startCompletion(selected) || tree.startCompletion(selected) {
		t.Fatal("completion claim was not unique")
	}
	appended := 0
	receipt := &completionReceipt{fallback: func() error { appended++; return nil }, appended: func() { tree.acknowledge(selected) }}
	receipt.retire()
	receipt.complete()
	receipt.retire()
	if appended != 1 {
		t.Fatalf("fallback appended %d times", appended)
	}
	resumed, _, err := tree.message(context.Background(), testCaller(selected), selected, "resume", "again")
	if err != nil || resumed.ID != child.ID {
		t.Fatalf("acknowledged resume %#v %v", resumed, err)
	}
}

func TestFailedCompletionEnqueueRetainsReservation(t *testing.T) {
	config := defaultConfig()
	config.MaxMailbox = 1
	tree := testTree(t, config)
	child := testAdmission(t, tree, Ref{}, "scout", "child")
	selected := testRun(t, tree, child.Ref)
	tree.finish(tree.records[child.ID], selected, Outcome{State: Completed, Text: "done"})
	acknowledged := false
	receipt := &completionReceipt{fallback: func() error { return errors.New("history unavailable") }, appended: func() { acknowledged = true; tree.acknowledge(selected) }}
	receipt.retire()
	receipt.complete()
	if acknowledged {
		t.Fatal("failed retained append acknowledged delivery")
	}
	if _, _, err := tree.message(context.Background(), testCaller(selected), selected, "resume", "again"); err == nil {
		t.Fatal("failed receipt released its reservation")
	}
}

func TestPendingMessagesAreBoundedAndCancelledAdmissionsNeverLaunch(t *testing.T) {
	config := defaultConfig()
	config.MaxMailbox = 1
	tree := testTree(t, config)
	child := testAdmission(t, tree, Ref{}, "scout", "child")
	selected := testRun(t, tree, child.Ref)
	if snapshot, _, err := tree.message(context.Background(), testCaller(selected), selected, "steer", "first"); err != nil || snapshot.State != Starting {
		t.Fatalf("pending message %#v %v", snapshot, err)
	}
	if _, _, err := tree.message(context.Background(), testCaller(selected), selected, "steer", "second"); err == nil {
		t.Fatal("accepted full mailbox")
	}
	tree.cancelPending(selected)
	if err := tree.activate(selected); err == nil {
		t.Fatal("activated cancelled admission")
	}
	if outcome, err := tree.wait(context.Background(), selected, 1000); err != nil || outcome.State != Stopped {
		t.Fatalf("cancelled admission %#v %v", outcome, err)
	}
	if _, _, err := tree.admit(admission{name: "child", agent: config.Agents["scout"]}); err != nil {
		t.Fatalf("failed admission retained its name or capacity: %v", err)
	}
}

func TestNormalHandlerCleanupDoesNotCancelSpawnAcknowledgement(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal response", true: "request aborted"}[cancelRequest], func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			child := testAdmission(t, tree, Ref{}, "scout", "child")
			o := &owner{tree: tree, acks: map[*callTicket]*acknowledgement{}}
			requestDone := make(chan struct{})
			started := make(chan struct{})
			tree.execute = func(_ *record, _ *run) Outcome {
				close(started)
				return Outcome{State: Completed, Text: "started after acknowledgement"}
			}
			key := &callTicket{callID: "spawn"}
			selected := testRun(t, tree, child.Ref)
			o.deferActivation(key, selected, requestDone, func() bool { return cancelRequest }, "litter_spawn")
			close(requestDone)
			if cancelRequest {
				outcome, err := tree.wait(context.Background(), selected, 1000)
				if err != nil || outcome.State != Stopped {
					t.Fatalf("cancelled pending spawn %#v %v", outcome, err)
				}
				return
			}
			o.settleAck(key, false)
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatal("normal handler cleanup cancelled the admitted child")
			}
			outcome, err := tree.wait(context.Background(), selected, 1000)
			if err != nil || outcome.Text != "started after acknowledgement" {
				t.Fatalf("acknowledged spawn %#v %v", outcome, err)
			}
		})
	}
}

func TestOutcomeCapturesCurrentTerminalAssistantAndUsage(t *testing.T) {
	before := coding.SessionStats{Tokens: coding.SessionStatsTokens{Input: 100, Output: 50, Total: 150}, Cost: 1}
	after := coding.SessionStats{Tokens: coding.SessionStatsTokens{Input: 110, Output: 55, Total: 165}, Cost: 1.2}
	terminal := &agent.AssistantMessage{StopReason: ai.StopReason("stop"), Content: []ai.AssistantContentBlock{ai.TextContent{Text: "resume succeeded"}}}
	outcome := outcomeFrom(terminal, before, after, "", false, false)
	if outcome.State != Completed || outcome.Text != "resume succeeded" || outcome.Usage.TotalTokens != 15 {
		t.Fatalf("resumed outcome %#v", outcome)
	}
	outcome = outcomeFrom(terminal, before, after, "tool read failed", false, false)
	if outcome.State != Partial || outcome.Error != "tool read failed" {
		t.Fatalf("tool failure became findings %#v", outcome)
	}
}

func TestInspectionReturnsLatestOutcome(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "child")
	selected := testRun(t, tree, child.Ref)
	report := "the child's latest report"
	tree.finish(tree.records[child.ID], selected, Outcome{State: Completed, Text: report})
	inspection, err := tree.inspect(child.ID)
	if err != nil || inspection.Outcome == nil || inspection.Outcome.Text != report || tree.records[child.ID].run.outcome.Text != report {
		t.Fatalf("inspection lost the latest result: %v", err)
	}
	if inspection.Child.State != Completed {
		t.Fatalf("inspection omitted current state %#v", inspection.Child)
	}
}

func TestCloseJoinsConcurrentCallers(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "child")
	selected := testRun(t, tree, child.Ref)
	entered, release := make(chan struct{}), make(chan struct{})
	tree.execute = func(_ *record, run *run) Outcome {
		close(entered)
		<-run.ctx.Done()
		<-release
		return Outcome{State: Stopped}
	}
	if err := tree.activate(selected); err != nil {
		t.Fatal(err)
	}
	<-entered
	closed := make(chan struct{}, 2)
	go func() { tree.close(); closed <- struct{}{} }()
	<-tree.ctx.Done()
	select {
	case <-closed:
		t.Fatal("close returned before execution joined")
	default:
	}
	secondStarted := make(chan struct{})
	go func() {
		close(secondStarted)
		tree.close()
		closed <- struct{}{}
	}()
	<-secondStarted
	select {
	case <-closed:
		t.Fatal("concurrent close returned before execution joined")
	default:
	}
	close(release)
	for range 2 {
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("concurrent close did not join execution")
		}
	}
	if _, err := tree.list(nil); err == nil || err.(*treeError).code != "unavailable" {
		t.Fatalf("closed tree remained available: %v", err)
	}
}
