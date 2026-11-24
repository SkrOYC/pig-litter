package piglitter

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTeamTaskEditValuesAndDependencies(t *testing.T) {
	store, agentDir, root, _ := storeTestOpen(t)
	first, err := store.createTask("first", "before", nil, map[string]string{"keep": "yes", "remove": "old"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.createTask("second", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	third, err := store.createTask("third", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	subject, description, added := "renamed", "after", "new"
	edit := teamTaskEdit{
		Subject:      &subject,
		Description:  &description,
		AddBlockedBy: []TaskID{third.ID},
		AddBlocks:    []TaskID{second.ID},
		Metadata:     map[string]*string{"add": &added, "remove": nil},
	}
	updated, state, err := store.editTask(first.ID, edit, nil)
	if err != nil || state != "updated" || updated.Subject != subject || updated.Description != description || !updated.Blocked || len(updated.BlockedBy) != 1 || updated.BlockedBy[0] != third.ID || updated.Metadata["keep"] != "yes" || updated.Metadata["add"] != "new" {
		t.Fatalf("updated task = %#v, %q, %v", updated, state, err)
	}
	if _, present := updated.Metadata["remove"]; present {
		t.Fatal("metadata deletion did not remove the key")
	}
	dependent, err := store.getTask(second.ID)
	if err != nil || !dependent.Blocked || len(dependent.BlockedBy) != 1 || dependent.BlockedBy[0] != first.ID {
		t.Fatalf("dependent task = %#v, %v", dependent, err)
	}
	_, state, err = store.editTask(first.ID, edit, nil)
	if err != nil || state != "unchanged" {
		t.Fatalf("idempotent edit = %q, %v", state, err)
	}
	path := filepath.Join(agentDir, "pig-litter", "roots", string(root), "tasks.json")
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	foreign := second.ID
	foreign.Team.Root = storeTestUUID(99)
	for _, invalid := range []teamTaskEdit{
		{AddBlockedBy: []TaskID{second.ID}},
		{AddBlockedBy: []TaskID{foreign}},
		{AddBlockedBy: []TaskID{third.ID}},
	} {
		if _, _, err := store.editTask(third.ID, invalid, nil); err == nil {
			t.Fatalf("accepted invalid dependencies %#v", invalid.AddBlockedBy)
		}
		after, readErr := os.ReadFile(path)
		if readErr != nil || !bytes.Equal(before, after) {
			t.Fatalf("invalid edit changed the task graph: %v", readErr)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTeamStore(agentDir, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	retained, err := reopened.getTask(first.ID)
	if err != nil || retained.Subject != subject || retained.Metadata["add"] != "new" || !retained.Blocked {
		t.Fatalf("reopened task = %#v, %v", retained, err)
	}
}

func TestTeamTaskConcurrentDependencyCycleIsAtomic(t *testing.T) {
	store, _, _, _ := storeTestOpen(t)
	first, err := store.createTask("first", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.createTask("second", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	errors := make(chan error, 2)
	var workers sync.WaitGroup
	for _, pair := range [][2]TaskID{{first.ID, second.ID}, {second.ID, first.ID}} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, _, err := store.editTask(pair[0], teamTaskEdit{AddBlockedBy: []TaskID{pair[1]}}, nil)
			errors <- err
		}()
	}
	close(start)
	workers.Wait()
	close(errors)
	succeeded := 0
	for err := range errors {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("concurrent dependency edits succeeded %d times, want 1", succeeded)
	}
	tasks, err := store.listTasks()
	if err != nil || len(tasks) != 2 || len(tasks[0].BlockedBy)+len(tasks[1].BlockedBy) != 1 {
		t.Fatalf("task graph after concurrent edits = %#v, %v", tasks, err)
	}
}

func TestTeamTaskInterruptedConcurrentCreationRetainsUniqueRecords(t *testing.T) {
	store, agentDir, root, _ := storeTestOpen(t)
	if _, err := store.createTask("before-close", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			_, _ = store.createTask(fmt.Sprintf("create-%02d", index), "", nil, nil)
		}()
	}
	close(start)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	workers.Wait()
	reopened, err := openTeamStore(agentDir, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	tasks, err := reopened.listTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) == 0 {
		t.Fatal("the committed task disappeared after reopening")
	}
	ids, subjects := map[TaskID]bool{}, map[string]bool{}
	var highest uint64
	for _, task := range tasks {
		if ids[task.ID] || subjects[task.Subject] {
			t.Fatalf("duplicate task after interrupted creation: %#v", task)
		}
		ids[task.ID], subjects[task.Subject] = true, true
		highest = max(highest, task.ID.Number)
	}
	next, err := reopened.createTask("after-reopen", "", nil, nil)
	if err != nil || next.ID.Number <= highest {
		t.Fatalf("allocation after interrupted creation = %#v, %v", next, err)
	}
}

func TestTeamTaskCreationRetryAfterUncertainSync(t *testing.T) {
	store, agentDir, root, _ := storeTestOpen(t)
	if _, err := store.createTask("seed", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	store.syncDir = func(string) error { return errors.New("injected directory sync failure") }
	if _, _, err := store.createTaskWithID("durable", "body", nil, nil, "submission-1", nil); err == nil {
		t.Fatal("creation unexpectedly confirmed a failed directory sync")
	}
	store.syncDir = syncDir
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := openTeamStore(agentDir, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	task, state, err := reopened.createTaskWithID("durable", "body", nil, nil, "submission-1", nil)
	if err != nil || state != "already_created" || task.ID.Number != 2 {
		t.Fatalf("creation retry = %#v, %q, %v", task, state, err)
	}
	if _, _, err := reopened.createTaskWithID("different", "body", nil, nil, "submission-1", nil); err == nil {
		t.Fatal("accepted different content for a creation submission")
	}
	tasks, err := reopened.listTasks()
	if err != nil || len(tasks) != 2 {
		t.Fatalf("tasks after creation retry = %#v, %v", tasks, err)
	}
	if _, err := reopened.deleteTask(task.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := reopened.createTaskWithID("durable", "body", nil, nil, "submission-1", nil); err == nil {
		t.Fatal("recreated a deleted task using its old submission ID")
	}
	next, err := reopened.createTask("next", "", nil, nil)
	if err != nil || next.ID.Number <= task.ID.Number {
		t.Fatalf("next task after deletion = %#v, %v", next, err)
	}
}

func TestTeamTaskConcurrentCreationSubmissionIsIdempotent(t *testing.T) {
	store, _, _, _ := storeTestOpen(t)
	type result struct {
		task  teamTaskView
		state string
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 4)
	for range 4 {
		go func() {
			<-start
			task, state, err := store.createTaskWithID("shared", "body", nil, nil, "submission-1", nil)
			results <- result{task, state, err}
		}()
	}
	close(start)
	created := 0
	var identity TaskID
	for range 4 {
		got := <-results
		if got.err != nil || (got.state != "created" && got.state != "already_created") {
			t.Fatalf("concurrent creation = %#v", got)
		}
		if identity.Number == 0 {
			identity = got.task.ID
		} else if identity != got.task.ID {
			t.Fatalf("creation retries returned different IDs: %#v and %#v", identity, got.task.ID)
		}
		if got.state == "created" {
			created++
		}
	}
	if created != 1 {
		t.Fatalf("successful creation count = %d, want 1", created)
	}
}

func TestTeamTaskCreationSubmissionIsBoundToCaller(t *testing.T) {
	store, _, _, workspace := storeTestOpen(t)
	alice, bob := peerTestMembers(t, store, workspace)
	first, firstState, err := store.createTaskWithID("same", "", nil, nil, "submission-1", &alice.Ref)
	if err != nil || firstState != "created" {
		t.Fatalf("first caller = %#v, %q, %v", first, firstState, err)
	}
	second, secondState, err := store.createTaskWithID("same", "", nil, nil, "submission-1", &bob.Ref)
	if err != nil || secondState != "created" || second.ID == first.ID {
		t.Fatalf("second caller = %#v, %q, %v", second, secondState, err)
	}
	for _, entry := range []struct {
		caller MemberRef
		want   TaskID
	}{{alice.Ref, first.ID}, {bob.Ref, second.ID}} {
		task, state, err := store.createTaskWithID("same", "", nil, nil, "submission-1", &entry.caller)
		if err != nil || state != "already_created" || task.ID != entry.want {
			t.Fatalf("caller retry = %#v, %q, %v", task, state, err)
		}
	}
	stale := alice.Ref
	stale.Generation++
	if _, _, err := store.createTaskWithID("same", "", nil, nil, "submission-1", &stale); err == nil {
		t.Fatal("accepted a stale caller generation")
	}
}
