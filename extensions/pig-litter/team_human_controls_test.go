package piglitter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
)

func TestHumanTeamControlsShareAuthorityAndValidation(t *testing.T) {
	store, _, member := planTestMember(t)
	tree := testTree(t, defaultConfig())
	tree.root.id = member.Ref.Team.Root
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: member.Workspace.Cwd, task: "plan", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	selected.owner.admitted = true
	task, err := store.createTask("Review the change", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.claimTask(task.ID, member.Ref, false); err != nil {
		t.Fatal(err)
	}
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		human := controlAuthority{human: func() error { return nil }}
		retired := controlAuthority{human: func() error { return errors.New("retired") }}
		submission := planCommand{Action: "submit", SubmissionID: "ui-plan", Text: "Review before writing.", TimeoutMs: 60000}
		value, err := o.applyPlan(controlAuthority{caller: selected}, submission)
		if err != nil || value["state"] != "pending" {
			exerciseErr = fmt.Errorf("plan submission: %#v %v", value, err)
			return
		}
		request := value["request"].(planRequest)
		plans, err := o.planSnapshot(human)
		if err != nil || len(plans) != 1 || plans[0].Request.ID != request.ID || plans[0].Gate != "closed" {
			exerciseErr = fmt.Errorf("plan snapshot: %#v %v", plans, err)
			return
		}
		decision := planCommand{Action: "decide", RequestID: request.ID, Decision: "approve"}
		wrong := decision
		wrong.RequestID.Nonce = "00000000000000000000000000000000"
		contradiction := decision
		contradiction.Reason = "also rejected"
		if _, err := o.applyPlan(human, wrong); err == nil {
			exerciseErr = errors.New("wrong request nonce was approved")
			return
		}
		if _, err := o.applyPlan(human, contradiction); err == nil {
			exerciseErr = errors.New("contradictory trusted plan decision was accepted")
			return
		}
		if _, err := o.applyPlan(retired, decision); err == nil {
			exerciseErr = errors.New("retired human approved a plan")
			return
		}
		value, err = o.applyPlan(human, decision)
		if err != nil || value["gate"] != "granted" {
			exerciseErr = fmt.Errorf("human approval: %#v %v", value, err)
			return
		}
		if _, _, err := o.applyTaskCompletion(retired, task.ID); err == nil {
			exerciseErr = errors.New("retired human completed a task")
			return
		}
		completed, state, err := o.applyTaskCompletion(human, task.ID)
		if err != nil || state != "completed" || completed.State != TaskCompleted {
			exerciseErr = fmt.Errorf("human completion: %#v %q %v", completed, state, err)
			return
		}
		shutdown, _, _, err := store.requestShutdown(member.Ref, "ui-shutdown", "Please stop", 60000)
		if err != nil {
			exerciseErr = err
			return
		}
		views, err := o.shutdownSnapshot(human)
		if err != nil || len(views) != 1 || views[0].Request.ID != shutdown.ID || views[0].Request.State != "pending" {
			exerciseErr = fmt.Errorf("shutdown snapshot: %#v %v", views, err)
			return
		}
		if _, err := o.shutdownSnapshot(retired); err == nil {
			exerciseErr = errors.New("retired human read shutdowns")
			return
		}
		if _, err := o.applyShutdown(human, shutdownCommand{Action: "respond", RequestID: shutdown.ID, Decision: "approve"}); err == nil {
			exerciseErr = errors.New("human responded for the addressed teammate")
			return
		}
		_, path := planPaths(store.dir, member.Ref)
		if err := os.Remove(path); err != nil {
			exerciseErr = err
			return
		}
		if _, err := o.planSnapshot(human); err == nil {
			exerciseErr = errors.New("plan snapshot hid a member read error")
			return
		}
		if _, err := o.shutdownSnapshot(human); err == nil {
			exerciseErr = errors.New("shutdown snapshot hid a member read error")
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			var query struct{ Method string }
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getEntries" {
				return json.RawMessage(`[]`)
			}
			return json.RawMessage(`"fixture"`)
		}
		return json.RawMessage(`null`)
	})
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}
