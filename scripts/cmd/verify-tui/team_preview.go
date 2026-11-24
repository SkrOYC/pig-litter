package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/tui/widthx"
)

type previewTeamID struct {
	Root  string `json:"root"`
	Epoch uint64 `json:"epoch"`
}

type previewMemberRef struct {
	Team       previewTeamID `json:"team"`
	ID         string        `json:"id"`
	Generation uint64        `json:"generation"`
}

type previewMember struct {
	Ref         previewMemberRef `json:"ref"`
	Name        string           `json:"name"`
	AdmissionID string           `json:"admissionId"`
	State       string           `json:"state"`
}

type previewTask struct {
	ID        json.RawMessage   `json:"id"`
	Subject   string            `json:"subject"`
	Owner     *previewMemberRef `json:"owner"`
	State     string            `json:"state"`
	BlockedBy []json.RawMessage `json:"blockedBy"`
}

type previewToolResult struct {
	Operation string           `json:"operation"`
	State     string           `json:"state"`
	Report    string           `json:"report"`
	Name      string           `json:"name"`
	Member    previewMemberRef `json:"member"`
	Admission string           `json:"admission"`
	Task      previewTask      `json:"task"`
}

func previewResumeProbe(dir string) error {
	trusted := true
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: dir, AgentDir: dir, ProjectTrusted: &trusted})
	if err != nil {
		return err
	}
	defer services.Close()
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		return err
	}
	defer runtime.Close()
	if _, err := runtime.Resume("p07-missing-session", coding.SessionStartOptions{}); err == nil {
		return fmt.Errorf("resume probe unexpectedly found its missing session")
	}
	return nil
}

func teamPreviewTurn(request openAIRequest) (providerTurn, bool, error) {
	text := allMessageText(request.Messages)
	if request.Model == "quick" {
		if strings.Contains(text, "P07_TRANSCRIPT_ALICE") {
			return providerTurn{kind: "text", text: "P07_REPORT_ALICE"}, true, nil
		}
		if strings.Contains(text, "P07_TRANSCRIPT_BOB") {
			return providerTurn{kind: "text", text: "P07_REPORT_BOB"}, true, nil
		}
	}
	marker := latestRootPrompt(request.Messages).marker
	if request.Model != "parent" || marker != "P07_TEAM_PREVIEW" && marker != "P07_TASK_CONTROL" {
		return providerTurn{}, false, nil
	}
	var results []previewToolResult
	for _, message := range roleMessages(request.Messages, "tool") {
		var result previewToolResult
		if err := json.Unmarshal([]byte(contentText(message.Content)), &result); err != nil {
			return providerTurn{}, true, err
		}
		if result.Operation == "team_spawn" || result.Operation == "team_task_create" || result.Operation == "team_task_update" {
			if result.State == "failed" || result.State == "rejected" || result.State == "unavailable" {
				return providerTurn{}, true, fmt.Errorf("%s failed: %s", result.Operation, result.Report)
			}
			results = append(results, result)
		}
	}
	var alice, bob *previewToolResult
	var tasks []previewTask
	assigned := false
	for i := range results {
		result := &results[i]
		switch result.Operation {
		case "team_spawn":
			if result.Name == "Alice" {
				alice = result
			} else if result.Name == "Bob" {
				bob = result
			}
		case "team_task_create":
			tasks = append(tasks, result.Task)
		case "team_task_update":
			assigned = result.State == "assigned"
		}
	}
	spawn := func(name, marker string) providerTurn {
		return providerTurn{kind: "tools", calls: []toolCall{{name: "team_spawn", args: map[string]any{"type": "scout", "name": name, "model": "fixture/quick", "task": marker, "workspace": map[string]any{"kind": "shared_checkout"}}}}}
	}
	if alice == nil {
		return spawn("Alice", "P07_TRANSCRIPT_ALICE"), true, nil
	}
	if bob == nil {
		return spawn("Bob", "P07_TRANSCRIPT_BOB"), true, nil
	}
	if len(tasks) == 0 {
		return providerTurn{kind: "tools", calls: []toolCall{{name: "team_task_create", args: map[string]any{"subject": "P07_ASSIGNED_TASK", "description": "Alice owns this task"}}}}, true, nil
	}
	if len(tasks) == 1 {
		args := map[string]any{"subject": "P07_UNASSIGNED_TASK"}
		if marker == "P07_TASK_CONTROL" {
			args["blockedBy"] = []json.RawMessage{tasks[0].ID}
		}
		return providerTurn{kind: "tools", calls: []toolCall{{name: "team_task_create", args: args}}}, true, nil
	}
	if !assigned {
		return providerTurn{kind: "tools", calls: []toolCall{{name: "team_task_update", args: map[string]any{"taskId": tasks[0].ID, "action": "assign", "assignee": "Alice"}}}}, true, nil
	}
	return providerTurn{kind: "text", text: "P07_TEAM_READY"}, true, nil
}

func (p *panelProof) teamPreviewProof() error {
	if err := p.sendText("P07_TEAM_PREVIEW"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	var members []previewMember
	var tasks []previewTask
	if err := waitFor(p.ctx, "settled P07 members and tasks", stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		root, err := p.root()
		if err != nil || !visibleContains(p.visible, "P07_TEAM_READY") {
			return false, nil
		}
		var roster struct {
			Members []previewMember `json:"members"`
		}
		var graph struct {
			Tasks []previewTask `json:"tasks"`
		}
		dir := filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID)
		rosterData, rosterErr := os.ReadFile(filepath.Join(dir, "team.json"))
		taskData, taskErr := os.ReadFile(filepath.Join(dir, "tasks.json"))
		if rosterErr != nil || taskErr != nil || json.Unmarshal(rosterData, &roster) != nil || json.Unmarshal(taskData, &graph) != nil || len(roster.Members) != 2 || len(graph.Tasks) != 2 {
			return false, nil
		}
		for _, member := range roster.Members {
			child, ok := root.Children[member.Ref.ID]
			if !ok || child.Name != member.Name || child.State != "completed" || child.Latest == nil || child.Latest.Text != "P07_REPORT_"+strings.ToUpper(member.Name) || member.State != "idle" {
				return false, nil
			}
		}
		members, tasks = roster.Members, graph.Tasks
		return true, nil
	}); err != nil {
		return err
	}
	byName := map[string]previewMember{}
	for _, member := range members {
		if member.Ref.Team.Root != p.rootID || member.Ref.Team.Epoch == 0 || member.Ref.Generation == 0 || member.Ref.ID == "" || member.AdmissionID == "" {
			return fmt.Errorf("incomplete teammate identity: %+v", member)
		}
		byName[member.Name] = member
	}
	alice, aliceOK := byName["Alice"]
	bob, bobOK := byName["Bob"]
	if !aliceOK || !bobOK || alice.Ref == bob.Ref || tasks[0].Subject != "P07_ASSIGNED_TASK" || tasks[0].Owner == nil || *tasks[0].Owner != alice.Ref || tasks[1].Subject != "P07_UNASSIGNED_TASK" || tasks[1].Owner != nil {
		return fmt.Errorf("unexpected teammate identities or task ownership: %+v %+v", members, tasks)
	}
	if err := p.awaitRootIdle("P07_TEAM_PREVIEW"); err != nil {
		return err
	}
	before, err := p.quiescentLogs()
	if err != nil {
		return err
	}
	requests := p.state.totalRequests()
	childRequests := p.state.modelRequests("quick")
	resumeBefore, err := os.ReadFile(p.resumeTrace)
	if err != nil {
		return err
	}
	if childRequests != 2 {
		return fmt.Errorf("expected exactly two settled teammate requests, got %d", childRequests)
	}
	if err := p.checkpoint("p07-settled-before-view"); err != nil {
		return err
	}
	for _, width := range []int{80, 120, 150} {
		if err := p.resize(width, 32); err != nil {
			return err
		}
		if err := waitFor(p.ctx, "P07 terminal resize", panelBudget, func() (bool, error) {
			geometry, err := p.geometry()
			return err == nil && geometry.width == width && geometry.height == 32, nil
		}); err != nil {
			return err
		}
		prefix := fmt.Sprintf("p07-%d", width)
		if err := p.openTeam(prefix + "-open"); err != nil {
			return err
		}
		if err := p.key(prefix+"-alice", "Home", func(text string) bool { return strings.Contains(selectedPanelRow(text), "Alice") }); err != nil {
			return err
		}
		for _, member := range []previewMember{alice, bob} {
			name := strings.ToLower(member.Name)
			if member.Name == "Bob" {
				if err := p.key(prefix+"-bob", "Down", func(text string) bool { return strings.Contains(selectedPanelRow(text), "Bob") }); err != nil {
					return err
				}
			}
			if err := p.key(prefix+"-"+name+"-detail", "Enter", func(text string) bool {
				return hasPanelView("team | detail")(text) && strings.Contains(text, "Name "+member.Name) && strings.Contains(text, "UUID "+member.Ref.ID)
			}); err != nil {
				return err
			}
			text, err := p.visible()
			if err != nil {
				return err
			}
			if member.Name == "Alice" && !strings.Contains(text, "P07_ASSIGNED_TASK") || member.Name == "Bob" && strings.Contains(text, "P07_ASSIGNED_TASK") {
				return fmt.Errorf("task ownership displayed for wrong member: %s", text)
			}
			if err := checkPreviewWidth(text, width); err != nil {
				return err
			}
			report := "P07_REPORT_" + strings.ToUpper(member.Name)
			if err := p.key(prefix+"-"+name+"-report", "v", func(text string) bool { return hasPanelView("team | report")(text) && strings.Contains(text, report) }); err != nil {
				return err
			}
			if err := p.key(prefix+"-"+name+"-report-back", "Escape", hasPanelView("team | detail")); err != nil {
				return err
			}
			marker := "P07_TRANSCRIPT_" + strings.ToUpper(member.Name)
			if err := p.key(prefix+"-"+name+"-transcript", "t", func(text string) bool {
				return hasPanelView("team | transcript")(text) && strings.Contains(text, marker) && strings.Contains(text, report)
			}); err != nil {
				return err
			}
			text, err = p.visible()
			if err != nil {
				return err
			}
			other := "ALICE"
			if member.Name == "Alice" {
				other = "BOB"
			}
			if strings.Contains(text, "P07_TRANSCRIPT_"+other) || strings.Contains(text, "P07_REPORT_"+other) {
				return fmt.Errorf("transcript crossed member identities: %s", text)
			}
			if err := checkPreviewWidth(text, width); err != nil {
				return err
			}
			if err := p.key(prefix+"-"+name+"-transcript-back", "Escape", hasPanelView("team | detail")); err != nil {
				return err
			}
			if err := p.key(prefix+"-"+name+"-list", "Escape", hasPanelView("team")); err != nil {
				return err
			}
		}
		if err := p.key(prefix+"-all-tasks", "k", func(text string) bool {
			return hasPanelView("team | tasks")(text) && strings.Contains(text, "P07_ASSIGNED_TASK") && strings.Contains(text, "P07_UNASSIGNED_TASK")
		}); err != nil {
			return err
		}
		if err := p.key(prefix+"-tasks-back", "Escape", hasPanelView("team")); err != nil {
			return err
		}
		if err := p.close(prefix + "-close"); err != nil {
			return err
		}
		if err := p.openTeam(prefix + "-reopen"); err != nil {
			return err
		}
		if text, err := p.visible(); err != nil || !strings.Contains(selectedPanelRow(text), "Bob") {
			return fmt.Errorf("selection was not retained after reopening: %s %v", text, err)
		}
		if err := p.close(prefix + "-reopen-close"); err != nil {
			return err
		}
	}
	if err := p.open("p07-direct-open"); err != nil {
		return err
	}
	if err := p.key("p07-direct-editor", "m", hasPanelView("message")); err != nil {
		return err
	}
	if err := p.action("p07-direct-editor-text", func() error { return p.sendText("t") }, func(text string) bool {
		return hasPanelView("message")(text) && strings.Contains(text, "Draft t|")
	}); err != nil {
		return err
	}
	if err := p.key("p07-direct-editor-cancel", "Escape", hasPanelView("list")); err != nil {
		return err
	}
	if err := p.close("p07-direct-close"); err != nil {
		return err
	}
	if err := p.logsUnchanged(before); err != nil {
		return err
	}
	if p.state.totalRequests() != requests || p.state.modelRequests("quick") != childRequests {
		return fmt.Errorf("viewing caused a provider request")
	}
	if err := p.restartHost(p.rootPath); err != nil {
		return err
	}
	if err := p.awaitRootIdle("P07_RELOADED"); err != nil {
		return err
	}
	if err := p.openTeam("p07-reload-open"); err != nil {
		return err
	}
	if text, err := p.visible(); err != nil || !strings.Contains(selectedPanelRow(text), "Bob") {
		return fmt.Errorf("selection was not retained after root reload: %s %v", text, err)
	}
	if err := p.key("p07-reload-bob-transcript", "t", func(text string) bool {
		return hasPanelView("team | transcript")(text) && strings.Contains(text, "P07_TRANSCRIPT_BOB") && strings.Contains(text, "P07_REPORT_BOB")
	}); err != nil {
		return err
	}
	if err := p.close("p07-reload-close"); err != nil {
		return err
	}
	if p.state.totalRequests() != requests || p.state.modelRequests("quick") != childRequests {
		return fmt.Errorf("reload or viewing caused a provider request")
	}
	resumeAfter, err := os.ReadFile(p.resumeTrace)
	if err != nil || string(resumeAfter) != string(resumeBefore) {
		return fmt.Errorf("reload or viewing invoked child Runtime.Resume: %q, %v", resumeAfter, err)
	}
	after, err := nativeDigests(p.rootSessions, filepath.Join(p.agentDir, "sessions"))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("viewing or reload changed native history bytes")
	}
	if err := p.checkpoint("p07-reloaded-after-view"); err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(map[string]any{"members": members, "tasks": tasks, "historyBefore": before, "historyAfter": after, "providerRequestsBefore": requests, "providerRequestsAfter": p.state.totalRequests(), "memberRequestsBefore": childRequests, "memberRequestsAfter": p.state.modelRequests("quick"), "resumeTraceBefore": string(resumeBefore), "resumeTraceAfter": string(resumeAfter), "widths": []int{80, 120, 150}, "scope": "first passive P07 preview"}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(p.evidence, "team-preview.json"), receipt)
}

func (p *panelProof) openTeam(label string) error {
	if err := p.sendText("/pig-litter team"); err != nil {
		return err
	}
	return p.key(label, "Enter", func(text string) bool {
		return hasPanelView("team")(text) && strings.Contains(text, "Alice") && strings.Contains(text, "Bob")
	})
}

func checkPreviewWidth(text string, width int) error {
	for _, line := range strings.Split(text, "\n") {
		if widthx.VisibleWidth(line) > width {
			return fmt.Errorf("terminal row exceeded %d columns: %q", width, line)
		}
	}
	return nil
}
