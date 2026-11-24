package piglitter

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestTreeCloseWaitsForSettledMemberCleanup(t *testing.T) {
	tree := newTree(defaultConfig())
	_, selected, err := tree.admit(admission{kind: "worker", name: "member", model: "fixture/fixed", cwd: "/fixture", task: "work", agent: AgentDefinition{Role: "worker"}})
	if err != nil {
		t.Fatal(err)
	}
	settled := make(chan struct{})
	release := make(chan struct{})
	tree.execute = func(r *record, run *run) Outcome {
		tree.finish(r, run, Outcome{State: Completed, Text: "done"})
		close(settled)
		<-release
		return Outcome{State: Completed, Text: "done"}
	}
	if err := tree.activate(selected); err != nil {
		t.Fatal(err)
	}
	<-settled
	closed := make(chan struct{})
	go func() {
		tree.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Error("tree closed before the settled member released its resources")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(5 * time.Second):
		t.Fatal("tree did not close after the member released its resources")
	}
}

func TestNativeCleanupFailureAppearsInOwnerInspection(t *testing.T) {
	nativeCompletionFixture(t, func(o *owner, binding *memberBinding, _ *run, _ string, host *runtimeHost) {
		o.tree.mu.Lock()
		r := binding.record
		r.backend = backendTmux
		r.run.state = Completed
		r.latest = &Outcome{Run: r.run.ref, State: Completed, Model: r.model, Text: "work completed"}
		r.run.outcome = r.latest
		o.tree.mu.Unlock()
		o.noteNativeCleanupFailure(r, r.run, errors.New("synthetic cleanup failure"))
		value, _, err := runtimeOperation(t, o, "litter_inspect", "cleanup-status", map[string]any{"id": r.id})
		if err != nil {
			t.Fatal(err)
		}
		status, ok := value["backend"].(*BackendStatus)
		outcome, outcomeOK := value["outcome"].(*Outcome)
		if !ok || status.Kind != "tmux" || status.Ownership != "cleanup_failed" || !strings.Contains(status.Error, "synthetic cleanup failure") || !outcomeOK || outcome.State != Completed || outcome.Text != "work completed" {
			t.Fatalf("native cleanup inspection = %#v", value)
		}
		host.mu.Lock()
		defer host.mu.Unlock()
		found := false
		for _, entry := range host.entries {
			data, _ := json.Marshal(entry["data"])
			var saved savedChild
			if json.Unmarshal(data, &saved) == nil && saved.ID == r.id && saved.Native != nil && strings.Contains(saved.NativeCleanupError, "synthetic cleanup failure") {
				found = true
			}
		}
		if !found {
			t.Fatal("native cleanup failure and ownership fence were not persisted")
		}
	})
}
