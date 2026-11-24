package piglitter

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func createTerminationTask(t *testing.T, store *teamStore, subject string, blockedBy []TaskID) teamTaskView {
	t.Helper()
	task, err := store.createTask(subject, "description", blockedBy, map[string]string{"key": "value"})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func claimTerminationTask(t *testing.T, store *teamStore, id TaskID, member MemberRef, explicit bool) teamTaskView {
	t.Helper()
	task, state, err := store.claimTask(id, member, explicit)
	want := "claimed"
	if explicit {
		want = "assigned"
	}
	if err != nil || state != want || task.Owner == nil || *task.Owner != member {
		t.Fatalf("claim = %#v, %q, %v", task, state, err)
	}
	return task
}

func TestTeamTasksTerminateWithoutInitializingGraph(t *testing.T) {
	store, _, _, workspace := storeTestOpen(t)
	alice, _ := peerTestMembers(t, store, workspace)
	if err := store.SetState(alice.Ref, "terminated"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tasks.lock", "tasks.json"} {
		if _, err := os.Lstat(filepath.Join(store.dir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("termination created %s or could not inspect it: %v", name, err)
		}
	}
	task := createTerminationTask(t, store, "later", nil)
	if _, _, err := store.claimTask(task.ID, alice.Ref, true); err == nil {
		t.Fatal("assigned a later task to the terminated member")
	}
}

func TestTeamTasksReleaseTerminatedMemberOnly(t *testing.T) {
	store, _, _, workspace := storeTestOpen(t)
	alice, bob := peerTestMembers(t, store, workspace)
	completed := createTerminationTask(t, store, "completed", nil)
	claimTerminationTask(t, store, completed.ID, alice.Ref, false)
	if _, state, err := store.completeTask(completed.ID, &alice.Ref); err != nil || state != "completed" {
		t.Fatalf("completion = %q, %v", state, err)
	}
	unfinished := createTerminationTask(t, store, "unfinished", nil)
	claimTerminationTask(t, store, unfinished.ID, alice.Ref, false)
	blocked := createTerminationTask(t, store, "blocked", []TaskID{unfinished.ID})
	claimTerminationTask(t, store, blocked.ID, alice.Ref, true)
	other := createTerminationTask(t, store, "other", nil)
	claimTerminationTask(t, store, other.ID, bob.Ref, false)

	for _, state := range []string{"idle", "paused", "unavailable", "interrupted", "running"} {
		if err := store.SetState(alice.Ref, state); err != nil {
			t.Fatal(err)
		}
		task, err := store.getTask(unfinished.ID)
		if err != nil || task.State != TaskInProgress || task.Owner == nil || *task.Owner != alice.Ref {
			t.Fatalf("owner after %s = %#v, %v", state, task, err)
		}
	}
	before, err := loadTeamTasks(store.dir, store.root)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(alice.Ref, "terminated"); err != nil {
		t.Fatal(err)
	}
	tasks, err := store.listTasks()
	if err != nil || len(tasks) != 4 {
		t.Fatalf("tasks after termination = %#v, %v", tasks, err)
	}
	if tasks[0].ID != completed.ID || tasks[0].State != TaskCompleted || tasks[0].Owner == nil || *tasks[0].Owner != alice.Ref {
		t.Fatalf("completed task changed = %#v", tasks[0])
	}
	if tasks[1].ID != unfinished.ID || tasks[1].State != TaskPending || tasks[1].Owner != nil || tasks[1].Metadata["key"] != "value" {
		t.Fatalf("unfinished task was not released = %#v", tasks[1])
	}
	if tasks[2].ID != blocked.ID || tasks[2].State != TaskPending || tasks[2].Owner != nil || !tasks[2].Blocked || len(tasks[2].BlockedBy) != 1 || tasks[2].BlockedBy[0] != unfinished.ID {
		t.Fatalf("blocked assignment was not preserved = %#v", tasks[2])
	}
	if tasks[3].ID != other.ID || tasks[3].State != TaskInProgress || tasks[3].Owner == nil || *tasks[3].Owner != bob.Ref {
		t.Fatalf("another member's task changed = %#v", tasks[3])
	}
	after, err := loadTeamTasks(store.dir, store.root)
	if err != nil || after.Revision != before.Revision+1 || after.HighWater != before.HighWater {
		t.Fatalf("release revision or watermark = %#v, %v", after, err)
	}
	if err := store.SetState(alice.Ref, "terminated"); err != nil {
		t.Fatal(err)
	}
	retried, err := loadTeamTasks(store.dir, store.root)
	if err != nil || retried.Revision != after.Revision || retried.HighWater != after.HighWater {
		t.Fatalf("termination retry changed task metadata = %#v, %v", retried, err)
	}
	if err := store.SetState(alice.Ref, "running"); err == nil {
		t.Fatal("terminated member returned to running")
	}
	if _, err := store.PauseMember(alice.Ref); err == nil {
		t.Fatal("terminated member was paused")
	}
	if _, _, err := store.claimTask(unfinished.ID, alice.Ref, true); err == nil {
		t.Fatal("assigned a task to a terminated member")
	}
	stale := bob.Ref
	stale.Generation++
	if _, _, err := store.claimTask(unfinished.ID, stale, true); err == nil {
		t.Fatal("assigned a task to a stale generation")
	}
	claimTerminationTask(t, store, unfinished.ID, bob.Ref, false)
	next := createTerminationTask(t, store, "next", nil)
	if next.ID.Number <= other.ID.Number {
		t.Fatalf("task allocation reused an ID: %#v after %#v", next.ID, other.ID)
	}
}

func TestTeamTasksRecoverTerminationAfterReleaseFailure(t *testing.T) {
	store, dir, root, workspace := storeTestOpen(t)
	alice, bob := peerTestMembers(t, store, workspace)
	task := createTerminationTask(t, store, "recover", nil)
	claimTerminationTask(t, store, task.ID, alice.Ref, false)
	path := filepath.Join(store.dir, "tasks.json")
	hidden := filepath.Join(store.dir, "tasks-hidden.json")
	if err := os.Rename(path, hidden); err != nil {
		t.Fatal(err)
	}
	if err := store.SetState(alice.Ref, "terminated"); err == nil {
		t.Fatal("termination succeeded without the task snapshot")
	}
	if _, err := store.listTasks(); err == nil {
		t.Fatal("listed tasks while the snapshot was missing")
	}
	if err := os.Rename(hidden, path); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTeamStore(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	recovered, err := reopened.getTask(task.ID)
	if err != nil || recovered.ID != task.ID || recovered.State != TaskPending || recovered.Owner != nil || recovered.Metadata["key"] != "value" {
		t.Fatalf("recovered task = %#v, %v", recovered, err)
	}
	claimTerminationTask(t, reopened, task.ID, bob.Ref, false)
	if err := reopened.SetState(alice.Ref, "terminated"); err != nil {
		t.Fatal(err)
	}
	claimed, err := reopened.getTask(task.ID)
	if err != nil || claimed.Owner == nil || *claimed.Owner != bob.Ref || claimed.State != TaskInProgress {
		t.Fatalf("termination retry cleared another owner = %#v, %v", claimed, err)
	}
}

func TestTeamTasksRetryAmbiguousTerminationRelease(t *testing.T) {
	store, _, _, workspace := storeTestOpen(t)
	alice, _ := peerTestMembers(t, store, workspace)
	task := createTerminationTask(t, store, "sync", nil)
	claimTerminationTask(t, store, task.ID, alice.Ref, false)
	before, err := loadTeamTasks(store.dir, store.root)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected task directory sync failure")
	calls := 0
	fail := true
	store.syncDir = func(path string) error {
		calls++
		if fail && calls >= 2 {
			return injected
		}
		return syncDir(path)
	}
	if err := store.SetState(alice.Ref, "terminated"); !errors.Is(err, injected) {
		t.Fatalf("termination sync failure = %v", err)
	}
	if _, err := store.getTask(task.ID); !errors.Is(err, injected) {
		t.Fatalf("task read did not propagate uncertain release = %v", err)
	}
	fail = false
	if err := store.SetState(alice.Ref, "terminated"); err != nil {
		t.Fatal(err)
	}
	after, err := loadTeamTasks(store.dir, store.root)
	if err != nil || after.Revision != before.Revision+1 {
		t.Fatalf("termination retry revision = %#v, %v", after, err)
	}
	recovered, err := store.getTask(task.ID)
	if err != nil || recovered.State != TaskPending || recovered.Owner != nil {
		t.Fatalf("termination retry task = %#v, %v", recovered, err)
	}
}

func TestTeamTasksReconcileUncertainTerminatedRoster(t *testing.T) {
	store, _, _, workspace := storeTestOpen(t)
	alice, _ := peerTestMembers(t, store, workspace)
	task := createTerminationTask(t, store, "roster-sync", nil)
	claimTerminationTask(t, store, task.ID, alice.Ref, false)
	injected := errors.New("injected roster directory sync failure")
	fail := true
	store.syncDir = func(path string) error {
		if fail {
			return injected
		}
		return syncDir(path)
	}
	if err := store.SetState(alice.Ref, "terminated"); !errors.Is(err, injected) {
		t.Fatalf("termination sync failure = %v", err)
	}
	if _, err := store.getTask(task.ID); !errors.Is(err, injected) {
		t.Fatalf("task read did not propagate uncertain roster = %v", err)
	}
	fail = false
	recovered, err := store.getTask(task.ID)
	if err != nil || recovered.State != TaskPending || recovered.Owner != nil {
		t.Fatalf("task after roster reconciliation = %#v, %v", recovered, err)
	}
}

func TestTeamTasksTerminateAndAssignAreSerialized(t *testing.T) {
	for range 16 {
		store, _, _, workspace := storeTestOpen(t)
		alice, _ := peerTestMembers(t, store, workspace)
		task := createTerminationTask(t, store, "race", nil)
		start := make(chan struct{})
		var wg sync.WaitGroup
		var assignErr, terminateErr error
		var state string
		wg.Add(2)
		go func() {
			defer wg.Done()
			<-start
			_, state, assignErr = store.claimTask(task.ID, alice.Ref, true)
		}()
		go func() {
			defer wg.Done()
			<-start
			terminateErr = store.SetState(alice.Ref, "terminated")
		}()
		close(start)
		wg.Wait()
		if terminateErr != nil || assignErr == nil && state != "assigned" {
			t.Fatalf("concurrent assignment = %q, %v; termination = %v", state, assignErr, terminateErr)
		}
		if assignErr != nil {
			var failure *treeError
			if !errors.As(assignErr, &failure) || failure.code != "unavailable" {
				t.Fatalf("unexpected assignment error = %v", assignErr)
			}
		}
		result, err := store.getTask(task.ID)
		if err != nil || result.State != TaskPending || result.Owner != nil {
			t.Fatalf("task after termination and assignment = %#v, %v", result, err)
		}
		if _, _, err := store.claimTask(task.ID, alice.Ref, true); err == nil {
			t.Fatal("assigned a task after termination")
		}
	}
}
