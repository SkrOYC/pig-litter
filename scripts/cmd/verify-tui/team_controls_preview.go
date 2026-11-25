package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

func (p *panelProof) teamControlsProof(media *collaborationMedia) error {
	if err := p.sendText("TEAM_PREVIEW"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "team controls fixture", stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		data, err := os.ReadFile(filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, "tasks.json"))
		if err != nil || !visibleContains(p.visible, "TEAM_READY") {
			return false, nil
		}
		var graph struct {
			Tasks []previewTask `json:"tasks"`
		}
		return json.Unmarshal(data, &graph) == nil && len(graph.Tasks) == 2 && graph.Tasks[0].Owner != nil, nil
	}); err != nil {
		return err
	}
	if err := p.awaitRootIdle("TEAM_PREVIEW"); err != nil {
		return err
	}
	bindings := []byte(`{"tui.select.up":"ctrl+p","tui.select.down":"ctrl+n","tui.select.confirm":"ctrl+g","tui.select.cancel":"ctrl+q"}`)
	if err := writePrivate(filepath.Join(p.agentDir, "keybindings.json"), bindings); err != nil {
		return err
	}
	if err := p.sendText("/reload"); err != nil {
		return err
	}
	if err := p.key("team-controls-reload", "Enter", func(text string) bool {
		return strings.Contains(text, "Reloaded keybindings, extensions")
	}); err != nil {
		return err
	}
	before, err := p.quiescentLogs()
	if err != nil {
		return err
	}
	requests, memberRequests := p.state.totalRequests(), p.state.modelRequests("quick")
	resumeBefore, err := os.ReadFile(p.resumeTrace)
	if err != nil {
		return err
	}
	if err := p.checkpoint("team-controls-before-native"); err != nil {
		return err
	}
	for _, width := range []int{80, 120, 150} {
		if err := p.resize(width, 32); err != nil {
			return err
		}
		if err := waitFor(p.ctx, "team controls resize", panelBudget, func() (bool, error) {
			geometry, err := p.geometry()
			return err == nil && geometry.width == width && geometry.height == 32, nil
		}); err != nil {
			return err
		}
		prefix := fmt.Sprintf("team-controls-%d", width)
		if err := p.openTeam(prefix + "-open"); err != nil {
			return err
		}
		if err := p.key(prefix+"-first-member", "Home", func(text string) bool { return strings.Contains(selectedPanelRow(text), "Alice") }); err != nil {
			return err
		}
		if err := p.key(prefix+"-next-member", "C-n", func(text string) bool {
			return strings.Contains(selectedPanelRow(text), "Bob") && strings.Contains(text, "Ctrl+P/Ctrl+N")
		}); err != nil {
			return err
		}
		if err := p.key(prefix+"-member-detail", "C-g", func(text string) bool {
			return hasPanelView("team | detail")(text) && strings.Contains(text, "Name Bob")
		}); err != nil {
			return err
		}
		if err := p.key(prefix+"-member-back", "C-q", hasPanelView("team")); err != nil {
			return err
		}
		if err := p.key(prefix+"-tasks", "k", func(text string) bool {
			return hasPanelView("team | tasks")(text) && strings.Contains(selectedPanelRow(text), "TEAM_ASSIGNED_TASK") && strings.Contains(text, "TEAM_UNASSIGNED_TASK")
		}); err != nil {
			return err
		}
		if err := p.key(prefix+"-next-task", "C-n", func(text string) bool { return strings.Contains(selectedPanelRow(text), "TEAM_UNASSIGNED_TASK") }); err != nil {
			return err
		}
		if err := p.key(prefix+"-task-detail", "C-g", func(text string) bool {
			return hasPanelView("team | task detail")(text) && strings.Contains(text, "TEAM_UNASSIGNED_TASK")
		}); err != nil {
			return err
		}
		text, err := p.visible()
		if err != nil {
			return err
		}
		if err := checkPreviewWidth(text, width); err != nil {
			return err
		}
		if media != nil && width == 80 {
			if err := media.markObservation(); err != nil {
				return err
			}
		}
		if err := p.key(prefix+"-task-back", "C-q", hasPanelView("team | tasks")); err != nil {
			return err
		}
		if err := p.key(prefix+"-previous-task", "C-p", func(text string) bool { return strings.Contains(selectedPanelRow(text), "TEAM_ASSIGNED_TASK") }); err != nil {
			return err
		}
		if err := p.key(prefix+"-tasks-back", "C-q", hasPanelView("team")); err != nil {
			return err
		}
		if text, err := p.visible(); err != nil || !strings.Contains(selectedPanelRow(text), "Bob") {
			return fmt.Errorf("task navigation changed selected teammate: %s %v", text, err)
		}
		if err := p.key(prefix+"-close", "C-q", func(text string) bool { return panelView(text) == "" }); err != nil {
			return err
		}
	}
	if err := p.logsUnchanged(before); err != nil {
		return err
	}
	resumeAfter, err := os.ReadFile(p.resumeTrace)
	if err != nil || !reflect.DeepEqual(resumeBefore, resumeAfter) || requests != p.state.totalRequests() || memberRequests != p.state.modelRequests("quick") {
		return fmt.Errorf("task navigation changed provider or resume activity: %v", err)
	}
	if err := p.checkpoint("team-controls-after-native"); err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(map[string]any{"widths": []int{80, 120, 150}, "keybindings": json.RawMessage(bindings), "providerRequestsBefore": requests, "providerRequestsAfter": p.state.totalRequests(), "memberRequestsBefore": memberRequests, "memberRequestsAfter": p.state.modelRequests("quick"), "resumeTraceBefore": string(resumeBefore), "resumeTraceAfter": string(resumeAfter), "historyBefore": before, "scope": "task selection and configured navigation"}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(p.evidence, "team-controls-preview.json"), receipt)
}
