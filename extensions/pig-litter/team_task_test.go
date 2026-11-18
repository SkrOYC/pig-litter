package piglitter

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTeamTasksCompetingClaimsAndBlockedAssignment(t *testing.T) {
	store, _, root, _ := storeTestOpen(t)
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
	team := TeamID{Root: root, Epoch: 1}
	members := make([]MemberRef, 4)
	for i := range members {
		members[i] = MemberRef{Team: team, ID: storeTestUUID(i + 10), Generation: uint64(i + 1)}
	}
	result, state, err := store.claimTask(blocked.ID, members[0], false)
	if err != nil || state != "blocked" || !result.Blocked || result.Owner != nil {
		t.Fatalf("blocked claim = %#v, %q, %v", result, state, err)
	}
	result, state, err = store.claimTask(blocked.ID, members[0], true)
	if err != nil || state != "assigned" || !result.Blocked || result.Owner == nil || *result.Owner != members[0] {
		t.Fatalf("explicit assignment = %#v, %q, %v", result, state, err)
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
