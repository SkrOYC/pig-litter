package piglitter

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

func testTree(t *testing.T, config Config) *Tree {
	t.Helper()
	tree, err := newTree(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(tree.close)
	return tree
}

func testAdmission(t *testing.T, tree *Tree, parent Ref, kind, name string) Snapshot {
	t.Helper()
	manager, err := coding.NewInMemorySessionManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AppendCustomMessage("fixture", "retained marker", false, nil); err != nil {
		t.Fatal(err)
	}
	agent := tree.config.Agents[kind]
	snapshot, err := tree.admit(admission{parent: parent.ID, parentGeneration: parent.Generation, kind: kind, name: name, model: "fixture/child", cwd: "/fixture", task: "task", agent: agent, history: manager})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func TestAdmissionCountsDescendantsAndRejectsDepthWritersAndNames(t *testing.T) {
	config := defaultConfig()
	config.Concurrency = 2
	tree := testTree(t, config)
	parent := testAdmission(t, tree, Ref{}, "scout", "parent")
	child := testAdmission(t, tree, parent.Ref, "scout", "child")
	if child.Depth != 2 || child.Parent == nil || *child.Parent != parent.ID {
		t.Fatalf("grandchild %#v", child)
	}
	if _, err := tree.admit(admission{agent: config.Agents["scout"]}); err == nil {
		t.Fatal("accepted a third live descendant")
	}
	if _, err := tree.admit(admission{parent: child.ID, parentGeneration: 1, agent: config.Agents["scout"]}); err == nil || !strings.Contains(err.Error(), "depth") {
		t.Fatalf("depth rejection %v", err)
	}
	writers := testTree(t, defaultConfig())
	writer := testAdmission(t, writers, Ref{}, "worker", "writer")
	if _, err := writers.admit(admission{cwd: "/fixture", agent: writers.config.Agents["worker"]}); err == nil || !strings.Contains(err.Error(), "writer conflict") {
		t.Fatalf("writer rejection %v", err)
	}
	if _, _, err := writers.stop(writer.Ref); err != nil {
		t.Fatal(err)
	}
	if _, err := writers.admit(admission{name: "writer", agent: writers.config.Agents["scout"]}); err == nil || !strings.Contains(err.Error(), "name already") {
		t.Fatalf("name rejection %v", err)
	}
}

func TestWaitCancellationAndTimeoutLeaveChildRunning(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "held")
	if _, err := tree.wait(context.Background(), child.Ref, 1); err == nil || err.(*treeError).code != "wait_timeout" {
		t.Fatalf("wait timeout %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tree.wait(ctx, child.Ref, 1000); err == nil || err.(*treeError).code != "cancelled" {
		t.Fatalf("cancelled wait %v", err)
	}
	list, err := tree.list("")
	if err != nil || len(list) != 1 || list[0].State != Starting {
		t.Fatalf("child changed %#v %v", list, err)
	}
	outcome, count, err := tree.stop(child.Ref)
	if err != nil || count != 1 || outcome.State != Stopped {
		t.Fatalf("stop %#v %d %v", outcome, count, err)
	}
	if _, count, err := tree.stop(child.Ref); err != nil || count != 0 {
		t.Fatalf("repeat stop %d %v", count, err)
	}
}

func TestExplicitResumeKeepsHistoryAndIDAndRejectsStaleGeneration(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "retained")
	manager := tree.records[child.ID].history
	if _, _, err := tree.stop(child.Ref); err != nil {
		t.Fatal(err)
	}
	tree.acknowledge(child.Ref)
	resumed, err := tree.message(context.Background(), child.Ref, "resume", "continue")
	if err != nil || resumed.ID != child.ID || resumed.Generation != 2 {
		t.Fatalf("resume %#v %v", resumed, err)
	}
	if tree.records[child.ID].history != manager {
		t.Fatal("resume replaced the actual manager")
	}
	if _, err := tree.wait(context.Background(), child.Ref, 1); err == nil || !strings.Contains(err.Error(), "stale") {
		t.Fatalf("stale generation %v", err)
	}
	inspection, err := tree.inspect(child.ID, true, 0, 10)
	if err != nil || !inspection.HistoryAvailable || len(inspection.Entries) != 1 || !strings.Contains(string(inspection.Entries[0]), "retained marker") {
		t.Fatalf("retained transcript %#v %v", inspection, err)
	}
	tree.retireHistory(tree.records[child.ID])
	if tree.records[child.ID].run.ref.Generation != 1 {
		t.Fatal("retired pending resume did not restore settled generation")
	}
	if _, err := tree.message(context.Background(), child.Ref, "resume", "again"); err == nil || !strings.Contains(err.Error(), "history is unavailable") {
		t.Fatalf("retired history %v", err)
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
	if err := tree.activate(child.Ref); err != nil {
		t.Fatal(err)
	}
	<-entered
	stopped := make(chan struct{})
	go func() { _, _, _ = tree.stop(child.Ref); close(stopped) }()
	select {
	case <-cleaned:
		t.Fatal("cleanup completed before release")
	default:
	}
	if _, err := tree.admit(admission{agent: config.Agents["scout"]}); err == nil {
		t.Fatal("capacity released before cleanup")
	}
	close(cleanup)
	<-stopped
	if outcome, err := tree.wait(context.Background(), child.Ref, 1000); err != nil || outcome.State != Stopped {
		t.Fatalf("joined stop %#v %v", outcome, err)
	}
}

func TestSubtreeOwnershipAndStopAreGenerationScoped(t *testing.T) {
	tree := testTree(t, defaultConfig())
	parent := testAdmission(t, tree, Ref{}, "scout", "parent")
	child := testAdmission(t, tree, parent.Ref, "scout", "child")
	sibling := testAdmission(t, tree, Ref{}, "scout", "sibling")
	list, err := tree.list(parent.ID)
	if err != nil || len(list) != 1 || list[0].ID != child.ID {
		t.Fatalf("owned list %#v %v", list, err)
	}
	for _, id := range []ChildID{parent.ID, sibling.ID} {
		if err := tree.owned(parent.ID, id); err == nil {
			t.Fatalf("accepted unrelated child %s", id)
		}
	}
	started := make(chan struct{}, 2)
	tree.execute = func(_ *record, run *run) Outcome {
		started <- struct{}{}
		<-run.ctx.Done()
		return Outcome{State: Stopped}
	}
	if err := tree.activate(parent.Ref); err != nil {
		t.Fatal(err)
	}
	if err := tree.activate(child.Ref); err != nil {
		t.Fatal(err)
	}
	<-started
	<-started
	results := make(chan int, 2)
	for range 2 {
		go func() { _, count, _ := tree.stop(parent.Ref); results <- count }()
	}
	if total := <-results + <-results; total != 2 {
		t.Fatalf("concurrent stop counted %d", total)
	}
	list, _ = tree.list("")
	if list[2].State != Starting {
		t.Fatalf("stop affected sibling %#v", list[2])
	}
}

func TestCompletionReservationWaitsForActualAppend(t *testing.T) {
	config := defaultConfig()
	config.MaxMailbox = 1
	tree := testTree(t, config)
	parent := testAdmission(t, tree, Ref{}, "scout", "parent")
	child := testAdmission(t, tree, parent.Ref, "scout", "child")
	tree.finish(tree.records[child.ID], tree.records[child.ID].run, Outcome{State: Completed, Text: "first"})
	if _, err := tree.message(context.Background(), child.Ref, "resume", "again"); err == nil || !strings.Contains(err.Error(), "mailbox capacity") {
		t.Fatalf("unappended completion released capacity %v", err)
	}
	if !tree.startCompletion(child.Ref) || tree.startCompletion(child.Ref) {
		t.Fatal("completion claim was not unique")
	}
	appended := 0
	receipt := &completionReceipt{fallback: func() error { appended++; return nil }, appended: func() { tree.acknowledge(child.Ref) }}
	receipt.retire()
	receipt.complete()
	receipt.retire()
	if appended != 1 {
		t.Fatalf("fallback appended %d times", appended)
	}
	resumed, err := tree.message(context.Background(), child.Ref, "resume", "again")
	if err != nil || resumed.Generation != 2 {
		t.Fatalf("acknowledged resume %#v %v", resumed, err)
	}
}

func TestFailedCompletionEnqueueRetainsReservation(t *testing.T) {
	config := defaultConfig()
	config.MaxMailbox = 1
	tree := testTree(t, config)
	child := testAdmission(t, tree, Ref{}, "scout", "child")
	tree.finish(tree.records[child.ID], tree.records[child.ID].run, Outcome{State: Completed, Text: "done"})
	acknowledged := false
	receipt := &completionReceipt{fallback: func() error { return errors.New("history unavailable") }, appended: func() { acknowledged = true; tree.acknowledge(child.Ref) }}
	receipt.retire()
	receipt.complete()
	if acknowledged {
		t.Fatal("failed retained append acknowledged delivery")
	}
	if _, err := tree.message(context.Background(), child.Ref, "resume", "again"); err == nil {
		t.Fatal("failed receipt released its reservation")
	}
}

func TestPendingMessagesAreBoundedAndCancelledAdmissionsNeverLaunch(t *testing.T) {
	config := defaultConfig()
	config.MaxMailbox = 1
	tree := testTree(t, config)
	child := testAdmission(t, tree, Ref{}, "scout", "child")
	if snapshot, err := tree.message(context.Background(), child.Ref, "steer", "first"); err != nil || snapshot.State != Starting {
		t.Fatalf("pending message %#v %v", snapshot, err)
	}
	if _, err := tree.message(context.Background(), child.Ref, "steer", "second"); err == nil {
		t.Fatal("accepted full mailbox")
	}
	tree.cancelPending(child.Ref)
	if err := tree.activate(child.Ref); err == nil {
		t.Fatal("activated cancelled admission")
	}
	if outcome, err := tree.wait(context.Background(), child.Ref, 1000); err != nil || outcome.State != Stopped {
		t.Fatalf("cancelled admission %#v %v", outcome, err)
	}
	if _, err := tree.admit(admission{name: "child", agent: config.Agents["scout"]}); err == nil {
		t.Fatal("reused cancelled child name")
	}
}

func TestNormalHandlerCleanupDoesNotCancelSpawnAcknowledgement(t *testing.T) {
	for _, cancelRequest := range []bool{false, true} {
		t.Run(map[bool]string{false: "normal response", true: "request aborted"}[cancelRequest], func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			child := testAdmission(t, tree, Ref{}, "scout", "child")
			o := &owner{tree: tree, acks: map[ackKey]*acknowledgement{}}
			requestDone := make(chan struct{})
			started := make(chan struct{})
			tree.execute = func(_ *record, _ *run) Outcome {
				close(started)
				return Outcome{State: Completed, Text: "started after acknowledgement"}
			}
			key := ackKey{callID: "spawn"}
			o.deferActivation(key, child.Ref, requestDone, func() bool { return cancelRequest })
			close(requestDone)
			if cancelRequest {
				outcome, err := tree.wait(context.Background(), child.Ref, 1000)
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
			outcome, err := tree.wait(context.Background(), child.Ref, 1000)
			if err != nil || outcome.Text != "started after acknowledgement" {
				t.Fatalf("acknowledged spawn %#v %v", outcome, err)
			}
		})
	}
}

func TestOutcomeCapturesCurrentTerminalAssistantAndGenerationUsage(t *testing.T) {
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

func TestInspectionClipsReportWithoutChangingRetainedOutcome(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "child")
	report := strings.Repeat("é", 3000)
	tree.finish(tree.records[child.ID], tree.records[child.ID].run, Outcome{State: Completed, Text: report})
	inspection, err := tree.inspect(child.ID, false, 0, 10)
	if err != nil || len(inspection.Outcome.Text) != 2048 || !inspection.Outcome.HandbackTruncated || tree.records[child.ID].run.outcome.Text != report {
		t.Fatalf("inspection %#v %v", inspection, err)
	}
	if _, err := tree.records[child.ID].history.AppendCustomMessage("large", strings.Repeat("x", 33000), false, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.inspect(child.ID, true, 1, 1); err == nil {
		t.Fatal("allowed unbounded transcript page")
	}
}

func TestCloseJoinsConcurrentCallers(t *testing.T) {
	tree := testTree(t, defaultConfig())
	child := testAdmission(t, tree, Ref{}, "scout", "child")
	entered, release := make(chan struct{}), make(chan struct{})
	tree.execute = func(_ *record, run *run) Outcome {
		close(entered)
		<-run.ctx.Done()
		<-release
		return Outcome{State: Stopped}
	}
	if err := tree.activate(child.Ref); err != nil {
		t.Fatal(err)
	}
	<-entered
	var wait sync.WaitGroup
	wait.Add(2)
	for range 2 {
		go func() { defer wait.Done(); tree.close() }()
	}
	close(release)
	wait.Wait()
	if tree.live != 0 {
		t.Fatalf("close retained %d live slots", tree.live)
	}
}
