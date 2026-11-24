package piglitter

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

func TestTeamPolicyLoadsOnlyOwnerConfiguration(t *testing.T) {
	agentDir, projectDir := t.TempDir(), t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, "team-policy.json"), []byte(`{"taskCompletion":["/bin/false"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	policy, err := loadTeamPolicy(agentDir)
	if err != nil || len(policy.TaskCompletion) != 0 {
		t.Fatalf("default policy = %#v, %v", policy, err)
	}
	path := filepath.Join(agentDir, "team-policy.json")
	if err := os.WriteFile(path, []byte(`{"taskCompletion":["/bin/sh","-c","printf '{\"permit\":true}'"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	policy, err = loadTeamPolicy(agentDir)
	if err != nil || len(policy.TaskCompletion) != 3 {
		t.Fatalf("owner policy = %#v, %v", policy, err)
	}
	permit, feedback := runTeamPolicy(t.Context(), policy.TaskCompletion, map[string]string{"kind": "task_completion"})
	if !permit || feedback != "" {
		t.Fatalf("configured command = %v, %q", permit, feedback)
	}
	if err := os.WriteFile(path, []byte(`{"taskCompletion":["relative-command"]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTeamPolicy(agentDir); err == nil {
		t.Fatal("accepted a relative executable")
	}
	if err := os.WriteFile(path, []byte(`{"taskCompletion":[],"taskCompletion":[]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadTeamPolicy(agentDir); err == nil {
		t.Fatal("accepted duplicate policy settings")
	}
}

func TestIdlePolicySuppliesContinuationAndRetainsOwnedWork(t *testing.T) {
	store, _, member := planTestMember(t)
	config := defaultConfig()
	config.MaxTurns = 3
	tree := testTree(t, config)
	tree.root.id = member.Ref.Team.Root
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: member.Workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	selected.progress.turns = 1
	task, err := store.createTask("Finish the migration", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.claimTask(task.ID, member.Ref, false); err != nil {
		t.Fatal(err)
	}
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		dir := t.TempDir()
		input := filepath.Join(dir, "candidate")
		o.policy.TeammateIdle = []string{"/bin/sh", "-c", `cat > "$1"; printf '{"permit":false,"feedback":"Finish the migration first."}'`, "policy", input}
		event := &extension.AgentBeforeSettleEvent{BoundaryState: extension.BoundaryState{
			Outcome: extension.AgentActivityCompleted,
			Entries: []extension.SessionBoundaryDraft{{Type: extension.SessionBoundaryDraftCustomMessage, CustomType: "existing", Content: "preserve me"}},
			Context: extension.BoundaryContextPreview{ContextMessages: []agent.AgentMessage{{Assistant: &agent.AssistantMessage{Content: []ai.AssistantContentBlock{ai.TextContent{Text: "Report preview"}}}}}},
		}}
		handler := o.idlePolicyHooks(selected.owner, selected).Handlers["agent_before_settle"][0]
		value, err := handler(event)
		result, ok := value.(extension.BoundaryResult)
		if err != nil || !ok || result.Continue == nil || !*result.Continue || result.Entries == nil || len(*result.Entries) != 2 || (*result.Entries)[0].CustomType != "existing" || (*result.Entries)[1].Content != "The teammate idle policy requires further work. Finish the migration first." {
			exerciseErr = fmt.Errorf("idle refusal: %#v %v", value, err)
			return
		}
		candidate, err := os.ReadFile(input)
		if err != nil || !strings.Contains(string(candidate), `"report":"Report preview"`) || !strings.Contains(string(candidate), `"subject":"Finish the migration"`) {
			exerciseErr = fmt.Errorf("idle candidate: %s %v", candidate, err)
			return
		}
		stored, err := store.getTask(task.ID)
		if err != nil || stored.State != TaskInProgress || stored.Owner == nil || *stored.Owner != member.Ref {
			exerciseErr = fmt.Errorf("idle refusal lost work: %#v %v", stored, err)
			return
		}
		o.policy.TeammateIdle = []string{"/bin/sh", "-c", `printf '{"permit":true}'`}
		value, err = handler(event)
		if err != nil || value != nil || selected.idleRefusal != "" {
			exerciseErr = fmt.Errorf("idle permit: %#v %v", value, err)
			return
		}
		ready, release := filepath.Join(dir, "idle-ready"), filepath.Join(dir, "idle-release")
		o.policy.TeammateIdle = []string{"/bin/sh", "-c", `: > "$1"; while [ ! -f "$2" ]; do sleep 0.02; done; printf '{"permit":true}'`, "policy", ready, release}
		type boundaryResponse struct {
			value any
			err   error
		}
		done := make(chan boundaryResponse, 1)
		go func() {
			value, err := handler(event)
			done <- boundaryResponse{value, err}
		}()
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				exerciseErr = errors.New("idle policy command did not start")
				_ = os.WriteFile(release, nil, 0600)
				<-done
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := store.createTask("Change the graph during idle", "", nil, nil); err != nil {
			exerciseErr = err
		}
		if err := os.WriteFile(release, nil, 0600); err != nil {
			exerciseErr = err
		}
		answer := <-done
		stale, ok := answer.value.(extension.BoundaryResult)
		if answer.err != nil || !ok || stale.Continue == nil || !*stale.Continue || stale.Entries == nil || len(*stale.Entries) != 2 || !strings.Contains((*stale.Entries)[1].Content.(string), "Team work changed") {
			exerciseErr = fmt.Errorf("stale idle permit: %#v %v", answer.value, answer.err)
			return
		}
		selected.progress.turns = config.MaxTurns
		o.policy.TeammateIdle = []string{"/bin/sh", "-c", `printf '{"permit":false,"feedback":"More work remains."}'`}
		value, err = handler(event)
		if err != nil || value != nil || selected.idleRefusal != "More work remains." {
			exerciseErr = fmt.Errorf("turn limit refusal: %#v %v", value, err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			return json.RawMessage(`"fixture"`)
		}
		return json.RawMessage(`null`)
	})
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func TestTaskCompletionPolicyRetainsWorkAndRejectsStaleSnapshot(t *testing.T) {
	store, _, member := planTestMember(t)
	tree := testTree(t, defaultConfig())
	tree.root.id = member.Ref.Team.Root
	_, selected, err := tree.admit(admission{kind: "worker", name: member.Name, cwd: member.Workspace.Cwd, task: "work", agent: AgentDefinition{Role: "worker", Tools: []string{"read"}}, member: &member})
	if err != nil {
		t.Fatal(err)
	}
	task, err := store.createTask("Keep the claim", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.claimTask(task.ID, member.Ref, false); err != nil {
		t.Fatal(err)
	}
	var exerciseErr error
	withCompletionHost(t, tree, func(o *owner) {
		o.teamStore = store
		authority := controlAuthority{caller: selected}
		o.policy.TaskCompletion = []string{"/bin/sh", "-c", `printf '{"permit":false,"feedback":"Run the checks first."}'`}
		result, state, err := o.applyTaskCompletion(authority, task.ID)
		if err != nil || state != "policy_refused" || result.State != TaskInProgress || result.Owner == nil || *result.Owner != member.Ref || result.Feedback != "Run the checks first." {
			exerciseErr = fmt.Errorf("policy refusal: %#v %q %v", result, state, err)
			return
		}
		stored, err := store.getTask(task.ID)
		if err != nil || stored.State != TaskInProgress || stored.Owner == nil || *stored.Owner != member.Ref {
			exerciseErr = fmt.Errorf("retained task: %#v %v", stored, err)
			return
		}
		dir := t.TempDir()
		input, ready, release := filepath.Join(dir, "input"), filepath.Join(dir, "ready"), filepath.Join(dir, "release")
		script := `cat > "$1"; : > "$2"; while [ ! -f "$3" ]; do sleep 0.02; done; printf '{"permit":true}'`
		o.policy.TaskCompletion = []string{"/bin/sh", "-c", script, "policy", input, ready, release}
		type response struct {
			task  teamTaskView
			state string
			err   error
		}
		done := make(chan response, 1)
		go func() {
			result, state, err := o.applyTaskCompletion(authority, task.ID)
			done <- response{result, state, err}
		}()
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(ready); err == nil {
				break
			}
			if time.Now().After(deadline) {
				exerciseErr = errors.New("policy command did not start")
				_ = os.WriteFile(release, nil, 0600)
				<-done
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		candidate, err := os.ReadFile(input)
		if err != nil || !strings.Contains(string(candidate), `"subject":"Keep the claim"`) {
			exerciseErr = fmt.Errorf("policy candidate: %s %v", candidate, err)
		}
		if _, err := store.createTask("Change the task graph", "", nil, nil); err != nil {
			exerciseErr = err
		}
		if err := os.WriteFile(release, nil, 0600); err != nil {
			exerciseErr = err
		}
		answer := <-done
		if answer.err == nil {
			exerciseErr = fmt.Errorf("stale policy permit completed task: %#v", answer)
			return
		}
		stored, err = store.getTask(task.ID)
		if err != nil || stored.State != TaskInProgress || stored.Owner == nil || *stored.Owner != member.Ref {
			exerciseErr = fmt.Errorf("stale permit changed task: %#v %v", stored, err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		if method == "sessionRead" {
			return json.RawMessage(`"fixture"`)
		}
		return json.RawMessage(`null`)
	})
	if exerciseErr != nil {
		t.Fatal(exerciseErr)
	}
}

func TestTaskEditInvalidatesCompletionPolicySnapshot(t *testing.T) {
	store, _, member := planTestMember(t)
	task, err := store.createTask("Original", "", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.claimTask(task.ID, member.Ref, false); err != nil {
		t.Fatal(err)
	}
	_, state, revision, err := store.taskCompletionCandidate(task.ID, &member.Ref)
	if err != nil || state != "ready" {
		t.Fatalf("completion candidate = %q, %v", state, err)
	}
	subject := "Edited"
	if _, state, err := store.editTask(task.ID, teamTaskEdit{Subject: &subject}, &member.Ref); err != nil || state != "updated" {
		t.Fatalf("task edit = %q, %v", state, err)
	}
	if _, _, err := store.completeTask(task.ID, &member.Ref, &revision); err == nil {
		t.Fatal("a completion policy snapshot survived a task edit")
	}
	current, err := store.getTask(task.ID)
	if err != nil || current.Subject != subject || current.State != TaskInProgress {
		t.Fatalf("task after stale completion = %#v, %v", current, err)
	}
}
