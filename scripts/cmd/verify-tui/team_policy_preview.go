package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func installTeamPolicyFixture(agentDir, workspace string) error {
	command := `if test -f "$1"; then printf '%s' '{"permit":true}'; else printf '%s' '{"permit":false,"feedback":"Run the checks first."}'; fi`
	policy, err := json.Marshal(map[string]any{"taskCompletion": []string{"/bin/sh", "-c", command, "p07-policy", filepath.Join(workspace, "permit-task")}})
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(agentDir, "team-policy.json"), policy)
}

func (p *panelProof) teamPolicyProof(media *collaborationMedia) error {
	if err := p.sendText("P07_TASK_CONTROL"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	path := filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, "tasks.json")
	var before []byte
	var members []previewMember
	if err := waitFor(p.ctx, "task policy fixture", stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		data, err := os.ReadFile(path)
		if err != nil || !visibleContains(p.visible, "P07_TEAM_READY") {
			return false, nil
		}
		var graph struct {
			Tasks []previewTask `json:"tasks"`
		}
		if json.Unmarshal(data, &graph) != nil || len(graph.Tasks) != 2 || graph.Tasks[0].State != "in_progress" || graph.Tasks[0].Owner == nil {
			return false, nil
		}
		rosterData, err := os.ReadFile(filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, "team.json"))
		var roster struct {
			Members []previewMember `json:"members"`
		}
		root, rootErr := p.root()
		if err != nil || rootErr != nil || json.Unmarshal(rosterData, &roster) != nil || len(roster.Members) != 2 || p.state.modelRequests("quick") != 2 {
			return false, nil
		}
		for _, member := range roster.Members {
			child, ok := root.Children[member.Ref.ID]
			if !ok || child.State != "completed" || child.Latest == nil || member.State != "idle" {
				return false, nil
			}
		}
		members = roster.Members
		before = data
		return true, nil
	}); err != nil {
		return err
	}
	if err := p.awaitRootIdle("P07_TASK_CONTROL"); err != nil {
		return err
	}
	if err := p.captureTurnNotices(members); err != nil {
		return err
	}
	requests, memberRequests := p.state.totalRequests(), p.state.modelRequests("quick")
	resumeBefore, err := os.ReadFile(p.resumeTrace)
	if err != nil {
		return err
	}
	if err := p.checkpoint("p07-policy-before"); err != nil {
		return err
	}
	if err := p.openTeam("p07-policy-open"); err != nil {
		return err
	}
	if err := p.key("p07-policy-tasks", "k", func(text string) bool {
		return hasPanelView("team | tasks")(text) && strings.Contains(selectedPanelRow(text), "P07_ASSIGNED_TASK")
	}); err != nil {
		return err
	}
	if err := p.key("p07-policy-refuse", "c", func(text string) bool {
		return hasPanelView("team | task feedback")(text) && strings.Contains(text, "Run the checks first.") && strings.Contains(text, "Task #1 was not completed")
	}); err != nil {
		return err
	}
	refused, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, refused) {
		return fmt.Errorf("policy refusal changed the task graph: %v", err)
	}
	if err := writePrivate(filepath.Join(p.evidence, "p07-policy-refused.json"), refused); err != nil {
		return err
	}
	if media != nil {
		if err := media.markPanelObservation(p.ctx); err != nil {
			return err
		}
	}
	if err := writePrivate(filepath.Join(p.workspace, "permit-task"), []byte("allow")); err != nil {
		return err
	}
	if err := p.key("p07-policy-back", "Escape", hasPanelView("team | tasks")); err != nil {
		return err
	}
	if err := p.key("p07-policy-complete", "c", func(text string) bool { return strings.Contains(text, "Task #1 completed") }); err != nil {
		return err
	}
	var completed []byte
	if err := waitFor(p.ctx, "policy permitted completion", stageTimeout, func() (bool, error) {
		data, err := os.ReadFile(path)
		var graph struct {
			Tasks []previewTask `json:"tasks"`
		}
		if err != nil || json.Unmarshal(data, &graph) != nil || len(graph.Tasks) != 2 || graph.Tasks[0].State != "completed" || graph.Tasks[0].Owner == nil || graph.Tasks[1].State != "pending" {
			return false, nil
		}
		completed = data
		return true, nil
	}); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(p.evidence, "p07-policy-completed.json"), completed); err != nil {
		return err
	}
	if err := p.close("p07-policy-close"); err != nil {
		return err
	}
	resumeAfter, err := os.ReadFile(p.resumeTrace)
	requestsAfter, membersAfter := p.state.totalRequests(), p.state.modelRequests("quick")
	if err != nil || !bytes.Equal(resumeBefore, resumeAfter) || requests != requestsAfter || memberRequests != membersAfter {
		return fmt.Errorf("policy controls changed provider or resume activity: requests %d to %d, teammate requests %d to %d, resume trace equal %t: %v", requests, requestsAfter, memberRequests, membersAfter, bytes.Equal(resumeBefore, resumeAfter), err)
	}
	if err := p.checkpoint("p07-policy-after"); err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(map[string]any{"providerRequestsBefore": requests, "providerRequestsAfter": p.state.totalRequests(), "memberRequestsBefore": memberRequests, "memberRequestsAfter": p.state.modelRequests("quick"), "resumeTraceBefore": string(resumeBefore), "resumeTraceAfter": string(resumeAfter), "scope": "task policy refusal and retry"}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(p.evidence, "team-policy-preview.json"), receipt)
}
