package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (p *panelProof) teamTaskActionsProof(media *collaborationMedia) error {
	if err := p.sendText("P07_TASK_CONTROL"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	path := filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, "tasks.json")
	var before []byte
	if err := waitFor(p.ctx, "task action fixture", stageTimeout, func() (bool, error) {
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
		if json.Unmarshal(data, &graph) != nil || len(graph.Tasks) != 2 || graph.Tasks[0].State != "in_progress" || graph.Tasks[1].State != "pending" || len(graph.Tasks[1].BlockedBy) != 1 || !bytes.Equal(graph.Tasks[0].ID, graph.Tasks[1].BlockedBy[0]) {
			return false, nil
		}
		before = data
		return true, nil
	}); err != nil {
		return err
	}
	if err := p.awaitRootIdle("P07_TASK_CONTROL"); err != nil {
		return err
	}
	history, err := p.quiescentLogs()
	if err != nil {
		return err
	}
	requests, memberRequests := p.state.totalRequests(), p.state.modelRequests("quick")
	resumeBefore, err := os.ReadFile(p.resumeTrace)
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(p.evidence, "p07-tasks-before.json"), before); err != nil {
		return err
	}
	if err := p.checkpoint("p07-tasks-before"); err != nil {
		return err
	}
	if err := p.openTeam("p07-tasks-open"); err != nil {
		return err
	}
	if err := p.key("p07-tasks-list", "k", func(text string) bool {
		return hasPanelView("team | tasks")(text) && strings.Contains(selectedPanelRow(text), "P07_ASSIGNED_TASK") && strings.Contains(text, "blocked")
	}); err != nil {
		return err
	}
	if err := p.key("p07-tasks-select-blocked", "Down", func(text string) bool { return strings.Contains(selectedPanelRow(text), "P07_UNASSIGNED_TASK") }); err != nil {
		return err
	}
	if err := p.key("p07-tasks-refuse-blocked", "c", func(text string) bool { return strings.Contains(text, "Task #2 is blocked by dependencies") }); err != nil {
		return err
	}
	blocked, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, blocked) {
		return fmt.Errorf("blocked completion changed the task graph: %v", err)
	}
	if err := writePrivate(filepath.Join(p.evidence, "p07-tasks-after-blocked.json"), blocked); err != nil {
		return err
	}
	if err := p.key("p07-tasks-select-prerequisite", "Up", func(text string) bool { return strings.Contains(selectedPanelRow(text), "P07_ASSIGNED_TASK") }); err != nil {
		return err
	}
	if err := p.key("p07-tasks-complete-prerequisite", "c", func(text string) bool { return strings.Contains(text, "Task #1 completed") }); err != nil {
		return err
	}
	var after []byte
	if err := waitFor(p.ctx, "completed prerequisite and available dependent", stageTimeout, func() (bool, error) {
		data, err := os.ReadFile(path)
		if err != nil {
			return false, nil
		}
		var graph struct {
			Tasks []previewTask `json:"tasks"`
		}
		text, err := p.visible()
		if err != nil || json.Unmarshal(data, &graph) != nil || len(graph.Tasks) != 2 || graph.Tasks[0].State != "completed" || graph.Tasks[1].State != "pending" || strings.Contains(text, "blocked") {
			return false, nil
		}
		after = data
		return true, nil
	}); err != nil {
		return err
	}
	if _, err := p.frame("p07-tasks-available"); err != nil {
		return err
	}
	if media != nil {
		if err := media.markObservation(); err != nil {
			return err
		}
	}
	if err := writePrivate(filepath.Join(p.evidence, "p07-tasks-after.json"), after); err != nil {
		return err
	}
	if err := p.close("p07-tasks-close"); err != nil {
		return err
	}
	if err := p.logsUnchanged(history); err != nil {
		return err
	}
	resumeAfter, err := os.ReadFile(p.resumeTrace)
	if err != nil || !bytes.Equal(resumeBefore, resumeAfter) || requests != p.state.totalRequests() || memberRequests != p.state.modelRequests("quick") {
		return fmt.Errorf("task controls changed provider or resume activity: %v", err)
	}
	if err := p.checkpoint("p07-tasks-after"); err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(map[string]any{"providerRequestsBefore": requests, "providerRequestsAfter": p.state.totalRequests(), "memberRequestsBefore": memberRequests, "memberRequestsAfter": p.state.modelRequests("quick"), "resumeTraceBefore": string(resumeBefore), "resumeTraceAfter": string(resumeAfter), "historyBefore": history, "scope": "blocked and completed task controls"}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(p.evidence, "team-task-actions.json"), receipt)
}
