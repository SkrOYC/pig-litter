package piglitter

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestTeamPanelApprovesAndRejectsOnlySelectedPlans(t *testing.T) {
	store, dir, first := planTestMember(t)
	second, err := store.Reserve(storeTestUUID(4), string(storeTestUUID(5)), "reviewer", first.Workspace, true)
	if err != nil {
		t.Fatal(err)
	}
	second, err = store.Commit(second.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = first.Ref.Team.Root
	var runs []*run
	for _, member := range []teamMember{first, second} {
		_, selected, err := tree.admit(admission{kind: "scout", name: member.Name, model: "fixture/child", cwd: member.Workspace.Cwd, task: "plan", agent: tree.config.Agents["scout"], member: &member})
		if err != nil {
			t.Fatal(err)
		}
		selected.owner.admitted = true
		runs = append(runs, selected)
	}
	firstPlan, err := store.submitPlan(first.Ref, runs[0].noticeID, "first-plan", "Prepare the change.", 60000)
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := store.submitPlan(second.Ref, runs[1].noticeID, "second-plan", "Publish the change.", 60000)
	if err != nil {
		t.Fatal(err)
	}
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore, o.agentDir = store, dir
		panel := newChildPanel(o, nil, func() int { return 32 })
		panel.mu.Lock()
		panel.team = newTeamView(panel.ctx, o, panel.requestRender)
		panel.mu.Unlock()
		defer func() {
			panel.Dispose()
			o.actionWG.Wait()
		}()
		waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool { return len(frame.Members) == 2 && frame.PreferenceLoaded })
		panel.Render(120)
		panelKey(t, panel, "g")
		waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool { return frame.PlanHealth.Status == "fresh" && len(frame.Plans) == 2 })
		if text := strings.Join(panel.Render(120), "\n"); !strings.Contains(text, "> #1 pending | planner | Prepare the change.") || !strings.Contains(text, "#1 pending | reviewer | Publish the change.") {
			t.Fatalf("plan list did not show both requests: %s", text)
		}
		panelKey(t, panel, "a")
		waitFeedback := func(expected string) {
			t.Helper()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if strings.Contains(strings.Join(panel.Render(120), "\n"), expected) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatalf("plan feedback %q did not appear: %s", expected, strings.Join(panel.Render(120), "\n"))
		}
		waitFeedback("Plan #1 approved")
		firstRequests, err := store.readPlans(first.Ref)
		if err != nil || len(firstRequests) != 1 || firstRequests[0].ID != firstPlan.ID || firstRequests[0].State != "approved" {
			t.Fatalf("selected plan was not approved: %+v, %v", firstRequests, err)
		}
		secondRequests, err := store.readPlans(second.Ref)
		if err != nil || len(secondRequests) != 1 || secondRequests[0].ID != secondPlan.ID || secondRequests[0].State != "pending" {
			t.Fatalf("another plan changed during approval: %+v, %v", secondRequests, err)
		}
		panelKey(t, panel, "\x1b[B")
		panelKey(t, panel, "r")
		panelKey(t, panel, "Please revise the rollout.")
		panelKey(t, panel, "\r")
		waitFeedback("Plan #1 rejected")
		secondRequests, err = store.readPlans(second.Ref)
		if err != nil || len(secondRequests) != 1 || secondRequests[0].ID != secondPlan.ID || secondRequests[0].State != "rejected" || secondRequests[0].Reason != "Please revise the rollout." {
			t.Fatalf("rejection lost its selected identity or feedback: %+v, %v", secondRequests, err)
		}
	}, func(string, json.RawMessage) json.RawMessage { return json.RawMessage(`"fixture"`) })
}

func TestSlowPlanReadDoesNotBlockRosterOrPanelClosure(t *testing.T) {
	store, dir, member := planTestMember(t)
	tree := testTree(t, defaultConfig())
	tree.root.id = member.Ref.Team.Root
	_, selected, err := tree.admit(admission{kind: "scout", name: member.Name, model: "fixture/child", cwd: member.Workspace.Cwd, task: "plan", agent: tree.config.Agents["scout"], member: &member})
	if err != nil {
		t.Fatal(err)
	}
	selected.owner.admitted = true
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseRead := func() { releaseOnce.Do(func() { close(release) }) }
	withCompletionHost(t, tree, func(o *owner) {
		defer releaseRead()
		o.teamStore, o.agentDir = store, dir
		panel := newChildPanel(o, nil, func() int { return 32 })
		panel.mu.Lock()
		panel.team = newTeamView(panel.ctx, o, panel.requestRender)
		panel.mu.Unlock()
		waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool { return len(frame.Members) == 1 })
		panelKey(t, panel, "g")
		select {
		case <-entered:
		case <-time.After(5 * time.Second):
			t.Fatal("plan read did not reach the host")
		}
		if err := store.SetState(member.Ref, "paused"); err != nil {
			t.Fatal(err)
		}
		waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool {
			return len(frame.Members) == 1 && frame.Members[0].Member.State == "paused"
		})
		closed := make(chan struct{})
		go func() { panel.Dispose(); close(closed) }()
		select {
		case <-closed:
		case <-time.After(time.Second):
			t.Fatal("panel closure waited for the plan read")
		}
		releaseRead()
		o.actionWG.Wait()
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			select {
			case entered <- struct{}{}:
			default:
			}
			<-release
		}
		return json.RawMessage(`"fixture"`)
	})
}
