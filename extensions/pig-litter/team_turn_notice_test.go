package piglitter

import (
	"encoding/json"
	"fmt"
	"testing"
)

func TestTeammateTurnNoticesRetainReasonAndRetryIdentity(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	if member.Ref.Team.Root != root {
		t.Fatal("teammate belongs to a different root")
	}
	outcomes := []struct {
		execution string
		outcome   Outcome
		kind      string
	}{
		{"run-available", Outcome{State: Completed, Text: "Finished the review."}, "available"},
		{"run-interrupted", Outcome{State: Paused, Error: "Paused by the lead."}, "interrupted"},
		{"run-failed", Outcome{State: Failed, Error: "Provider failed."}, "failed"},
	}
	for _, item := range outcomes {
		notice, created, err := store.recordTurnNotice(member.Ref, item.execution, item.outcome)
		if err != nil || !created || notice.Kind != item.kind || notice.Report != item.outcome.Text || notice.Error != item.outcome.Error || notice.Delivery != "pending" {
			t.Fatalf("notice: %#v %v %v", notice, created, err)
		}
		retry, created, err := store.recordTurnNotice(member.Ref, item.execution, item.outcome)
		if err != nil || created || retry != notice {
			t.Fatalf("notice retry: %#v %v %v", retry, created, err)
		}
	}
	if _, _, err := store.recordTurnNotice(member.Ref, "run-available", Outcome{State: Completed, Text: "Different report."}); err == nil {
		t.Fatal("accepted a conflicting turn notice retry")
	}
	lifecycle, err := store.readLifecycle(member.Ref)
	if err != nil || len(lifecycle.Turns) != 3 {
		t.Fatalf("retained notices: %#v %v", lifecycle.Turns, err)
	}
}

func TestTeammateTurnNoticeReachesLeadOnceAndRecordsObservation(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	member, err := store.Reserve(storeTestUUID(2), string(storeTestUUID(3)), "worker", workspace, false)
	if err != nil {
		t.Fatal(err)
	}
	member, err = store.Commit(member.AdmissionID)
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	selected.owner.admitted = true
	outcome := Outcome{Run: selected.ref, State: Completed, Text: "Finished the review."}
	var entries []map[string]any
	var sends int
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		if err := o.finishTeamMember(selected.owner, selected, outcome); err != nil {
			exerciseErr = err
			return
		}
		o.tree.teamWG.Wait()
		if err := o.finishTeamMember(selected.owner, selected, outcome); err != nil {
			exerciseErr = err
			return
		}
		o.tree.teamWG.Wait()
		lifecycle, err := store.readLifecycle(member.Ref)
		if err != nil || len(lifecycle.Turns) != 1 || lifecycle.Turns[0].Kind != "available" || lifecycle.Turns[0].Report != "Finished the review." || lifecycle.Turns[0].Delivery != "native_observed" || lifecycle.Turns[0].NativeEntryID != "entry-1" || sends != 1 {
			exerciseErr = fmt.Errorf("lead notice: %#v sends=%d err=%v", lifecycle.Turns, sends, err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		switch method {
		case "sessionRead":
			var query struct{ Method string }
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getEntries" {
				data, _ := json.Marshal(entries)
				return data
			}
			return json.RawMessage(`"fixture"`)
		case "sendMessage":
			var args struct {
				Message map[string]any
				Options struct{ TriggerTurn *bool }
			}
			_ = json.Unmarshal(raw, &args)
			sends++
			if args.Options.TriggerTurn == nil || *args.Options.TriggerTurn {
				exerciseErr = fmt.Errorf("turn notice unexpectedly triggered the lead")
			}
			args.Message["type"], args.Message["id"] = "custom_message", "entry-1"
			entries = append(entries, args.Message)
			return json.RawMessage(`{}`)
		default:
			return json.RawMessage(`null`)
		}
	})
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}
