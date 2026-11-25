package piglitter

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"
)

func BenchmarkTeamTaskOperations(b *testing.B) {
	if b.N != 1 {
		b.Fatal("run with -benchtime=1x")
	}
	evidence := os.Getenv("TEAM_TASK_PERF_DIR")
	source := os.Getenv("TEAM_TASK_PERF_SOURCE")
	if evidence == "" || len(source) != 40 {
		b.Fatal("TEAM_TASK_PERF_DIR and TEAM_TASK_PERF_SOURCE are required")
	}
	root := storeTestUUID(1)
	store, err := openTeamStore(b.TempDir(), root)
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	workspace := WorkspacePolicy{Kind: "shared_checkout", Cwd: b.TempDir()}
	members := make([]MemberRef, 4)
	for index := range members {
		admission := string(storeTestUUID(index + 20))
		if _, err := store.Reserve(storeTestUUID(index+10), admission, fmt.Sprintf("worker-%d", index), workspace, false); err != nil {
			b.Fatal(err)
		}
		member, err := store.Commit(admission)
		if err != nil {
			b.Fatal(err)
		}
		members[index] = member.Ref
	}
	if err := store.taskTransaction(func(graph *teamTaskGraph) (bool, error) {
		graph.HighWater = 1000
		for number := uint64(1); number <= graph.HighWater; number++ {
			graph.Tasks = append(graph.Tasks, teamTask{ID: TaskID{Team: graph.Team, Number: number}, Subject: fmt.Sprintf("task-%04d", number), State: TaskPending, BlockedBy: []TaskID{}, Metadata: map[string]string{}})
		}
		return true, nil
	}); err != nil {
		b.Fatal(err)
	}
	team := members[0].Team
	listSamples := make([]int64, 0, 100)
	for range 100 {
		started := time.Now()
		tasks, err := store.listTasks()
		listSamples = append(listSamples, time.Since(started).Nanoseconds())
		if err != nil || len(tasks) != 1000 {
			b.Fatalf("list = %d tasks, %v", len(tasks), err)
		}
	}
	claimSamples := make([]int64, 0, 100)
	for number := uint64(1); number <= 100; number++ {
		started := time.Now()
		task, state, err := store.claimTask(TaskID{Team: team, Number: number}, members[0], false)
		claimSamples = append(claimSamples, time.Since(started).Nanoseconds())
		if err != nil || state != "claimed" || task.Owner == nil || *task.Owner != members[0] {
			b.Fatalf("uncontended claim = %#v, %q, %v", task, state, err)
		}
	}
	type claimResult struct {
		duration int64
		task     teamTaskView
		state    string
		err      error
	}
	contendedSamples := make([]int64, 0, 100)
	for number := uint64(101); number <= 125; number++ {
		start := make(chan struct{})
		results := make(chan claimResult, len(members))
		for _, member := range members {
			go func() {
				<-start
				started := time.Now()
				task, state, err := store.claimTask(TaskID{Team: team, Number: number}, member, false)
				results <- claimResult{time.Since(started).Nanoseconds(), task, state, err}
			}()
		}
		close(start)
		wins := 0
		var owner *MemberRef
		for range members {
			result := <-results
			contendedSamples = append(contendedSamples, result.duration)
			if result.err != nil || result.task.Owner == nil || (result.state != "claimed" && result.state != "already_claimed") {
				b.Fatalf("contended claim = %#v", result)
			}
			if owner == nil {
				copy := *result.task.Owner
				owner = &copy
			} else if *owner != *result.task.Owner {
				b.Fatalf("contenders observed different owners: %#v and %#v", owner, result.task.Owner)
			}
			if result.state == "claimed" {
				wins++
			}
		}
		if wins != 1 {
			b.Fatalf("task %d had %d successful claims", number, wins)
		}
	}
	tasks, err := store.listTasks()
	if err != nil || len(tasks) != 1000 {
		b.Fatalf("final list = %d tasks, %v", len(tasks), err)
	}
	for index, task := range tasks {
		if task.ID.Number != uint64(index+1) || task.Subject != fmt.Sprintf("task-%04d", index+1) || (index < 125) != (task.Owner != nil) {
			b.Fatalf("lost or changed task %d: %#v", index+1, task)
		}
	}
	percentiles := func(samples []int64) map[string]int64 {
		ordered := append([]int64(nil), samples...)
		sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
		return map[string]int64{"p50_ns": ordered[(len(ordered)+1)/2-1], "p95_ns": ordered[(95*len(ordered)+99)/100-1]}
	}
	stats := map[string]map[string]int64{"list": percentiles(listSamples), "claim_uncontended": percentiles(claimSamples), "claim_contended": percentiles(contendedSamples)}
	report := map[string]any{"source": source, "clock": "Go monotonic time.Since", "seeded_tasks": 1000, "contenders": 4, "list_ns": listSamples, "claim_uncontended_ns": claimSamples, "claim_contended_ns": contendedSamples, "stats": stats, "duplicate_claims": 0, "lost_tasks": 0}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "task-performance.json"), append(data, '\n'), 0o600); err != nil {
		b.Fatal(err)
	}
	for name, values := range stats {
		if values["p95_ns"] >= int64(200*time.Millisecond) {
			b.Errorf("%s p95 exceeded 200 ms: %d ns", name, values["p95_ns"])
		}
	}
}
