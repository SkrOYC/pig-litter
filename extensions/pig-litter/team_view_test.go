package piglitter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/tui/widthx"
)

func waitTeamFrame(t *testing.T, view *teamView, ready func(*teamViewFrame) bool) *teamViewFrame {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frame := view.Snapshot()
		if ready(frame) {
			return frame
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("team frame did not reach expected state: %+v", view.Snapshot())
	return nil
}

func teamViewFixture(t *testing.T) (*owner, *teamStore, []teamMember, []*run) {
	t.Helper()
	dir := t.TempDir()
	root := ChildID("018f0000-0000-7000-8000-000000000001")
	store, err := openTeamStore(dir, root)
	if err != nil {
		t.Fatal(err)
	}
	tree := newTree(defaultConfig())
	tree.root.id = root
	o := testCallOwner(tree)
	o.agentDir, o.teamStore = dir, store
	var members []teamMember
	var runs []*run
	for _, item := range []struct{ id, admission, name string }{
		{"018f0000-0000-7000-8000-000000000002", "018f0000-0000-7000-8000-000000000003", "Alice"},
		{"018f0000-0000-7000-8000-000000000004", "018f0000-0000-7000-8000-000000000005", "Bob"},
	} {
		member, err := store.Reserve(ChildID(item.id), item.admission, item.name, WorkspacePolicy{Kind: "shared_checkout", Cwd: dir}, false)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := store.Commit(member.AdmissionID); err != nil {
			t.Fatal(err)
		}
		if err := store.SetState(member.Ref, "idle"); err != nil {
			t.Fatal(err)
		}
		scout, _ := agentDefaults("scout")
		_, run, err := tree.admit(admission{kind: "scout", name: member.Name, model: "fixture/child", cwd: dir, task: "task", agent: scout, member: &member})
		if err != nil {
			t.Fatal(err)
		}
		tree.finish(run.owner, run, Outcome{State: Completed, Text: "report " + member.Name})
		members, runs = append(members, member), append(runs, run)
	}
	t.Cleanup(func() {
		tree.close()
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return o, store, members, runs
}

func TestTeamPanelRetainsMemberAndDisplaysOwnedTasksAndPrivateHistory(t *testing.T) {
	o, store, members, _ := teamViewFixture(t)
	task, err := store.createTask("Assigned marker", "Task description", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.claimTask(task.ID, members[1].Ref, true); err != nil {
		t.Fatal(err)
	}
	manager := nativeTestManager(t, o.agentDir, string(members[1].Ref.ID))
	path := manager.GetSessionFile()
	if path == nil {
		t.Fatal("native history was not persisted")
	}
	original, err := os.ReadFile(*path)
	if err != nil {
		t.Fatal(err)
	}
	original = []byte(strings.TrimSuffix(string(original), "\n"))
	if err := os.WriteFile(*path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	panel := newChildPanel(o, nil, func() int { return 32 })
	panel.mu.Lock()
	panel.team = newTeamView(panel.ctx, o, panel.requestRender)
	panel.mu.Unlock()
	waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool {
		return len(frame.Members) == 2 && len(frame.Tasks) == 1 && frame.PreferenceLoaded
	})
	panel.Render(80)
	panelKey(t, panel, "\x1b[B")
	panelKey(t, panel, "\r")
	for _, width := range []int{80, 120, 150} {
		rows := panel.Render(width)
		text := strings.Join(rows, "\n")
		if !strings.Contains(text, "Name Bob") || !strings.Contains(text, string(members[1].Ref.ID)) || !strings.Contains(text, "Assigned marker") {
			t.Fatalf("detail omitted identity or task at %d columns: %s", width, text)
		}
		for _, row := range rows {
			if widthx.VisibleWidth(row) > width {
				t.Fatalf("row exceeds %d columns: %q", width, row)
			}
		}
	}
	panelKey(t, panel, "v")
	if text := strings.Join(panel.Render(120), "\n"); !strings.Contains(text, "report Bob") || !strings.Contains(text, "Latest settled usage unknown") {
		t.Fatalf("report omitted retained outcome or unknown usage: %s", text)
	}
	panelKey(t, panel, "\x1b")
	panelKey(t, panel, "t")
	waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool {
		return frame.Transcript.Request == panel.teamRequest && frame.Transcript.Health.Status != "loading"
	})
	if text := strings.Join(panel.Render(120), "\n"); !strings.Contains(text, "user | retained marker") {
		t.Fatalf("private transcript did not display the retained marker: %s", text)
	}
	panel.Dispose()
	after, err := os.ReadFile(*path)
	if err != nil || string(after) != string(original) {
		t.Fatalf("transcript inspection changed source history: %v", err)
	}
	reopened := newChildPanel(o, nil, func() int { return 32 })
	reopened.mu.Lock()
	reopened.team = newTeamView(reopened.ctx, o, reopened.requestRender)
	reopened.mu.Unlock()
	defer reopened.Dispose()
	waitTeamFrame(t, reopened.team, func(frame *teamViewFrame) bool { return frame.PreferenceLoaded && len(frame.Members) == 2 })
	text := strings.Join(reopened.Render(120), "\n")
	if !strings.Contains(text, "> Bob") || reopened.member != members[1].Ref {
		t.Fatalf("selection did not survive panel closure: %s", text)
	}
}

func TestTeamRefreshAndCloseDoNotWaitForHistory(t *testing.T) {
	o, store, members, runs := teamViewFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	view := newTeamView(ctx, o, func() {})
	waitTeamFrame(t, view, func(frame *teamViewFrame) bool { return len(frame.Members) == 2 })
	runs[0].owner.deliveryMu.Lock()
	view.Inspect(members[0].Ref)
	if err := store.SetState(members[1].Ref, "paused"); err != nil {
		runs[0].owner.deliveryMu.Unlock()
		t.Fatal(err)
	}
	waitTeamFrame(t, view, func(frame *teamViewFrame) bool {
		return len(frame.Members) == 2 && frame.Members[1].Member.State == "paused"
	})
	closed := make(chan struct{})
	go func() { view.Close(); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		runs[0].owner.deliveryMu.Unlock()
		t.Fatal("closing the panel waited for history")
	}
	runs[0].owner.deliveryMu.Unlock()
	o.actionWG.Wait()
}

func TestTeamViewKeepsStaleSectionsWhenRosterReadFails(t *testing.T) {
	o, store, members, _ := teamViewFixture(t)
	if _, err := store.createTask("Retained task", "", nil, nil); err != nil {
		t.Fatal(err)
	}
	panel := newChildPanel(o, nil, func() int { return 32 })
	panel.mu.Lock()
	panel.team = newTeamView(panel.ctx, o, panel.requestRender)
	panel.mu.Unlock()
	defer panel.Dispose()
	waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool {
		return len(frame.Members) == 2 && len(frame.Tasks) == 1 && frame.TaskHealth.Status == "fresh"
	})
	path := filepath.Join(store.dir, "team.json")
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	err = os.WriteFile(path, []byte("{"), 0o600)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	stale := waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool {
		return frame.RosterHealth.Status == "stale" && frame.TaskHealth.Status == "stale"
	})
	if len(stale.Members) != 2 || stale.Members[1].Member.Ref != members[1].Ref || len(stale.Tasks) != 1 || stale.Tasks[0].Subject != "Retained task" {
		t.Fatalf("failed read discarded previously observed facts: %+v", stale)
	}
	text := strings.Join(panel.Render(120), "\n")
	if !strings.Contains(text, "Roster stale") || !strings.Contains(text, "Tasks stale") || !strings.Contains(text, "Alice") || !strings.Contains(text, "Bob") {
		t.Fatalf("panel did not disclose stale roster and task data: %s", text)
	}
	store.mu.Lock()
	err = os.WriteFile(path, original, 0o600)
	store.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool {
		return frame.RosterHealth.Status == "fresh" && frame.TaskHealth.Status == "fresh"
	})
}

func TestTeamTranscriptRejectsAmbiguousRetainedIdentity(t *testing.T) {
	o, _, members, _ := teamViewFixture(t)
	manager := nativeTestManager(t, o.agentDir, string(members[0].Ref.ID))
	path := manager.GetSessionFile()
	if path == nil {
		t.Fatal("native history was not persisted")
	}
	original, err := os.ReadFile(*path)
	if err != nil {
		t.Fatal(err)
	}
	duplicate := filepath.Join(filepath.Dir(*path), "duplicate_"+string(members[0].Ref.ID)+".jsonl")
	if err := os.WriteFile(duplicate, original, 0o600); err != nil {
		t.Fatal(err)
	}
	view := newTeamView(context.Background(), o, func() {})
	defer view.Close()
	waitTeamFrame(t, view, func(frame *teamViewFrame) bool { return len(frame.Members) == 2 })
	request := view.Inspect(members[0].Ref)
	frame := waitTeamFrame(t, view, func(frame *teamViewFrame) bool {
		return frame.Transcript.Request == request && frame.Transcript.Health.Status != "loading"
	})
	if frame.Transcript.Health.Status != "unavailable" || !strings.Contains(frame.Transcript.Health.Error, "2 matching files") || len(frame.Transcript.Entries) != 0 {
		t.Fatalf("ambiguous history was displayed: %+v", frame.Transcript)
	}
	if err := os.Remove(duplicate); err != nil {
		t.Fatal(err)
	}
	request = view.Inspect(members[0].Ref)
	frame = waitTeamFrame(t, view, func(frame *teamViewFrame) bool {
		return frame.Transcript.Request == request && frame.Transcript.Health.Status != "loading"
	})
	if frame.Transcript.Health.Status != "fresh" || len(frame.Transcript.Entries) != 1 || frame.Transcript.Entries[0].Text != "retained marker" {
		t.Fatalf("unique retained history was not displayed: %+v", frame.Transcript)
	}
	after, err := os.ReadFile(*path)
	if err != nil || string(after) != string(original) {
		t.Fatalf("history inspection changed the source: %v", err)
	}
}
