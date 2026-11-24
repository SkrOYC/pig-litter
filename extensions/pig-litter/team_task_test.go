package piglitter

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTeamTasksCompetingClaimsAndBlockedAssignment(t *testing.T) {
	store, _, _, workspace := storeTestOpen(t)
	prerequisite, err := store.createTask("prerequisite", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	blocked, err := store.createTask("blocked", "", []TaskID{prerequisite.ID}, nil)
	if err != nil {
		t.Fatal(err)
	}
	available, err := store.createTask("available", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	fetched, err := store.getTask(available.ID)
	if err != nil || fetched.Subject != "available" || fetched.ID != available.ID || fetched.State != TaskPending {
		t.Fatalf("task lookup = %#v, %v", fetched, err)
	}
	foreign := available.ID
	foreign.Team.Root = storeTestUUID(99)
	if _, err := store.getTask(foreign); err == nil {
		t.Fatal("looked up a task from a different team")
	}
	members := make([]MemberRef, 4)
	for i := range members {
		admission := string(storeTestUUID(i + 20))
		if _, err := store.Reserve(storeTestUUID(i+10), admission, fmt.Sprintf("worker-%d", i), workspace); err != nil {
			t.Fatal(err)
		}
		member, err := store.Commit(admission)
		if err != nil {
			t.Fatal(err)
		}
		members[i] = member.Ref
	}
	result, state, err := store.claimTask(blocked.ID, members[0], false)
	if err != nil || state != "blocked" || !result.Blocked || result.Owner != nil {
		t.Fatalf("blocked claim = %#v, %q, %v", result, state, err)
	}
	result, state, err = store.claimTask(blocked.ID, members[0], true)
	if err != nil || state != "assigned" || !result.Blocked || result.Owner == nil || *result.Owner != members[0] {
		t.Fatalf("explicit assignment = %#v, %q, %v", result, state, err)
	}
	if _, _, err := store.completeTask(blocked.ID, &members[1], nil); err == nil {
		t.Fatal("a different teammate completed the assigned task")
	}
	result, state, err = store.completeTask(blocked.ID, &members[0], nil)
	if err != nil || state != "blocked" || result.State != TaskInProgress {
		t.Fatalf("blocked completion = %#v, %q, %v", result, state, err)
	}
	result, state, err = store.completeTask(prerequisite.ID, nil, nil)
	if err != nil || state != "completed" || result.State != TaskCompleted {
		t.Fatalf("prerequisite completion = %#v, %q, %v", result, state, err)
	}
	_, state, err = store.completeTask(prerequisite.ID, nil, nil)
	if err != nil || state != "already_completed" {
		t.Fatalf("repeated completion = %q, %v", state, err)
	}
	if _, err := store.deleteTask(prerequisite.ID); err == nil {
		t.Fatal("deleted a task that still has a dependent")
	}
	tasks, err := store.listTasks()
	if err != nil || len(tasks) != 3 || tasks[1].Blocked {
		t.Fatalf("tasks after prerequisite completion = %#v, %v", tasks, err)
	}
	result, state, err = store.completeTask(blocked.ID, &members[0], nil)
	if err != nil || state != "completed" || result.State != TaskCompleted || result.Blocked {
		t.Fatalf("owner completion = %#v, %q, %v", result, state, err)
	}
	_, state, err = store.completeTask(blocked.ID, &members[0], nil)
	if err != nil || state != "already_completed" {
		t.Fatalf("owner completion retry = %q, %v", state, err)
	}
	type claim struct {
		task  teamTaskView
		state string
		err   error
	}
	claims := make(chan claim, len(members))
	var workers sync.WaitGroup
	for _, member := range members {
		workers.Add(1)
		go func() {
			defer workers.Done()
			task, state, err := store.claimTask(available.ID, member, false)
			claims <- claim{task, state, err}
		}()
	}
	workers.Wait()
	close(claims)
	winners := 0
	var owner MemberRef
	for claim := range claims {
		if claim.err != nil || claim.task.Owner == nil || (claim.state != "claimed" && claim.state != "already_claimed") {
			t.Fatalf("claim = %#v", claim)
		}
		if owner.ID == "" {
			owner = *claim.task.Owner
		} else if owner != *claim.task.Owner {
			t.Fatalf("claimants saw different owners: %#v and %#v", owner, *claim.task.Owner)
		}
		if claim.state == "claimed" {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("successful claims = %d, want 1", winners)
	}
}

func TestTeamTasksMissingSnapshotFailsClosed(t *testing.T) {
	store, dir, root, _ := storeTestOpen(t)
	if _, err := store.createTask("first", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "pig-litter", "roots", string(root), "tasks.json")
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := store.createTask("second", "", nil, nil); err == nil {
		t.Fatal("created a task after the durable allocation watermark disappeared")
	}
}
