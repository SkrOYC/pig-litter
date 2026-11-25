package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const previewShutdownReason = "Please stop after your current work."

type previewShutdownRequest struct {
	ID       json.RawMessage `json:"requestId"`
	State    string          `json:"state"`
	Reason   string          `json:"reason"`
	Response string          `json:"response"`
}

type previewShutdownLifecycle struct {
	Phase    string                   `json:"phase"`
	Requests []previewShutdownRequest `json:"requests"`
}

func teamShutdownTurn(request openAIRequest) (providerTurn, bool, error) {
	text := allMessageText(request.Messages)
	if request.Model == "held" && strings.Contains(text, "TEAM_SHUTDOWN_TASK_") {
		messages := roleMessages(request.Messages, "tool")
		if len(messages) == 0 {
			return providerTurn{kind: "tools", calls: []toolCall{{name: "read", args: map[string]string{"path": "fixture-read.txt"}}}}, true, nil
		}
		if strings.Contains(text, "TEAM_SHUTDOWN_TASK_ALICE") {
			return providerTurn{kind: "hold", name: "team-shutdown-alice", text: "TEAM_ALICE_INITIAL"}, true, nil
		}
		if !strings.Contains(text, "TEAM_SHUTDOWN_TASK_BOB") {
			return providerTurn{}, true, fmt.Errorf("unknown shutdown teammate")
		}
		var listed *struct {
			Operation string `json:"operation"`
			State     string `json:"state"`
			Shutdowns []struct {
				Request previewShutdownRequest `json:"request"`
			} `json:"shutdowns"`
		}
		for _, message := range messages {
			var result struct {
				Operation string `json:"operation"`
				State     string `json:"state"`
				Shutdowns []struct {
					Request previewShutdownRequest `json:"request"`
				} `json:"shutdowns"`
			}
			if json.Unmarshal([]byte(contentText(message.Content)), &result) != nil || result.Operation != "team_shutdown" {
				continue
			}
			if result.State != "listed" || listed != nil {
				return providerTurn{}, true, fmt.Errorf("teammate continued after shutdown response: %+v", result)
			}
			copy := result
			listed = &copy
		}
		if listed == nil {
			return providerTurn{kind: "hold", name: "team-shutdown-bob", calls: []toolCall{{name: "team_shutdown", args: map[string]any{"action": "list"}}}}, true, nil
		}
		if len(listed.Shutdowns) != 1 || listed.Shutdowns[0].Request.State != "pending" || listed.Shutdowns[0].Request.Reason != previewShutdownReason || len(listed.Shutdowns[0].Request.ID) == 0 {
			return providerTurn{}, true, fmt.Errorf("teammate did not receive the pending shutdown request: %+v", listed)
		}
		args := map[string]any{"action": "respond", "requestId": listed.Shutdowns[0].Request.ID, "decision": "approve"}
		return providerTurn{kind: "tools", calls: []toolCall{{name: "team_shutdown", args: args}}}, true, nil
	}
	if request.Model != "parent" || latestRootPrompt(request.Messages).marker != "TEAM_SHUTDOWN_CONTROL" {
		return providerTurn{}, false, nil
	}
	members := map[string]bool{}
	for _, message := range roleMessages(request.Messages, "tool") {
		var result previewToolResult
		if json.Unmarshal([]byte(contentText(message.Content)), &result) == nil && result.Operation == "team_spawn" {
			if result.State != "starting" || result.Member.ID == "" || members[result.Name] {
				return providerTurn{}, true, fmt.Errorf("invalid shutdown teammate admission: %+v", result)
			}
			members[result.Name] = true
		}
	}
	for _, name := range []string{"Alice", "Bob"} {
		if !members[name] {
			args := map[string]any{"type": "scout", "name": name, "model": "fixture/held", "task": "TEAM_SHUTDOWN_TASK_" + strings.ToUpper(name), "workspace": map[string]any{"kind": "shared_checkout"}}
			return providerTurn{kind: "tools", calls: []toolCall{{name: "team_spawn", args: args}}}, true, nil
		}
	}
	return providerTurn{kind: "text", text: "TEAM_SHUTDOWN_READY"}, true, nil
}

func (p *panelProof) readPreviewShutdown(member previewMember) (previewShutdownLifecycle, []byte, error) {
	path := filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, fmt.Sprintf("plan-%s-%d.json", member.Ref.ID, member.Ref.Generation))
	data, err := os.ReadFile(path)
	if err != nil {
		return previewShutdownLifecycle{}, nil, err
	}
	var ledger struct {
		Lifecycle previewShutdownLifecycle `json:"lifecycle"`
	}
	if err := json.Unmarshal(data, &ledger); err != nil || len(ledger.Lifecycle.Requests) != 1 {
		return previewShutdownLifecycle{}, nil, fmt.Errorf("invalid shutdown ledger for %s", member.Name)
	}
	return ledger.Lifecycle, data, nil
}

func (p *panelProof) teamShutdownProof(media *collaborationMedia) error {
	if err := p.sendText("TEAM_SHUTDOWN_CONTROL"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	path := filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, "team.json")
	var alice, bob previewMember
	if err := waitFor(p.ctx, "active teammates for shutdown", stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		if !p.state.holding("team-shutdown-alice", "team-shutdown-bob") || !visibleContains(p.visible, "TEAM_SHUTDOWN_READY") {
			return false, nil
		}
		data, err := os.ReadFile(path)
		var roster struct {
			Members []previewMember `json:"members"`
		}
		if err != nil || json.Unmarshal(data, &roster) != nil || len(roster.Members) != 2 {
			return false, nil
		}
		for _, member := range roster.Members {
			if member.Name == "Alice" {
				alice = member
			} else if member.Name == "Bob" {
				bob = member
			}
		}
		return alice.Ref.ID != "" && bob.Ref.ID != "" && alice.Ref != bob.Ref && alice.State == "running" && bob.State == "running", nil
	}); err != nil {
		return err
	}
	if err := p.awaitRootIdle("TEAM_SHUTDOWN_CONTROL"); err != nil {
		return err
	}
	aliceBefore, err := p.childNative(alice.Ref.ID)
	if err != nil {
		return err
	}
	resumeBefore, err := os.ReadFile(p.resumeTrace)
	if err != nil {
		return err
	}
	requestsBefore, rootRequests := p.state.totalRequests(), p.state.modelRequests("parent")
	if err := p.checkpoint("team-shutdown-before"); err != nil {
		return err
	}
	if err := p.openTeam("team-shutdown-open"); err != nil {
		return err
	}
	if err := p.key("team-shutdown-select-bob", "End", func(text string) bool { return strings.Contains(selectedPanelRow(text), "Bob") }); err != nil {
		return err
	}
	if err := p.key("team-shutdown-request", "s", func(text string) bool {
		return hasPanelView("team | request shutdown")(text) && strings.Contains(text, bob.Ref.ID)
	}); err != nil {
		return err
	}
	if err := p.action("team-shutdown-reason", func() error { return p.sendText(previewShutdownReason) }, func(text string) bool { return strings.Contains(text, previewShutdownReason) }); err != nil {
		return err
	}
	if err := p.key("team-shutdown-submit", "Enter", func(text string) bool {
		return hasPanelView("team")(text) && strings.Contains(text, "Shutdown request pending")
	}); err != nil {
		return err
	}
	pending, pendingData, err := p.readPreviewShutdown(bob)
	if err != nil || pending.Phase != "open" || pending.Requests[0].State != "pending" || pending.Requests[0].Reason != previewShutdownReason {
		return fmt.Errorf("shutdown request was not pending: %+v, %v", pending, err)
	}
	var requestID struct {
		Member previewMemberRef `json:"member"`
	}
	if json.Unmarshal(pending.Requests[0].ID, &requestID) != nil || requestID.Member != bob.Ref {
		return fmt.Errorf("shutdown request targeted another member: %s", pending.Requests[0].ID)
	}
	if err := writePrivate(filepath.Join(p.evidence, "team-shutdown-pending.json"), pendingData); err != nil {
		return err
	}
	if err := p.key("team-shutdown-history", "h", func(text string) bool {
		return hasPanelView("team | shutdowns")(text) && strings.Contains(selectedPanelRow(text), "pending | Bob")
	}); err != nil {
		return err
	}
	if err := p.key("team-shutdown-detail", "Enter", func(text string) bool {
		return hasPanelView("team | shutdown detail")(text) && strings.Contains(text, "request pending | phase open") && strings.Contains(text, previewShutdownReason)
	}); err != nil {
		return err
	}
	if media != nil {
		if err := media.markPanelObservation(p.ctx); err != nil {
			return err
		}
	}
	if err := p.key("team-shutdown-history-back", "Escape", hasPanelView("team | shutdowns")); err != nil {
		return err
	}
	if err := p.key("team-shutdown-roster-back", "Escape", hasPanelView("team")); err != nil {
		return err
	}
	if err := p.state.releaseHold("team-shutdown-bob"); err != nil {
		return err
	}
	var terminatedData []byte
	if err := waitFor(p.ctx, "durable graceful shutdown", stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		lifecycle, data, err := p.readPreviewShutdown(bob)
		if err != nil || lifecycle.Phase != "terminated" || lifecycle.Requests[0].State != "terminated" || !bytes.Equal(lifecycle.Requests[0].ID, pending.Requests[0].ID) {
			return false, nil
		}
		root, err := p.root()
		child, ok := root.Children[bob.Ref.ID]
		if err != nil || !ok || child.State != "terminated" || child.Paused {
			return false, nil
		}
		terminatedData = data
		return true, nil
	}); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(p.evidence, "team-shutdown-terminated.json"), terminatedData); err != nil {
		return err
	}
	if err := p.close("team-shutdown-notices-close"); err != nil {
		return err
	}
	if err := p.captureShutdownNotices(bob); err != nil {
		return err
	}
	if err := p.openTeam("team-shutdown-notices-reopen"); err != nil {
		return err
	}
	aliceAfter, err := p.childNative(alice.Ref.ID)
	if err != nil || !bytes.Equal(aliceBefore.Raw, aliceAfter.Raw) || !p.state.holding("team-shutdown-alice") {
		return fmt.Errorf("shutdown changed the other active teammate: %v", err)
	}
	if err := p.key("team-shutdown-select-alice", "Home", func(text string) bool { return strings.Contains(selectedPanelRow(text), "Alice") }); err != nil {
		return err
	}
	if err := p.key("team-shutdown-pause-alice", "p", func(text string) bool {
		return strings.Contains(text, "Paused subtree") && strings.Contains(selectedPanelRow(text), "Alice | member paused")
	}); err != nil {
		return err
	}
	if err := p.key("team-shutdown-final-history", "h", func(text string) bool {
		return hasPanelView("team | shutdowns")(text) && strings.Contains(text, "terminated | Bob")
	}); err != nil {
		return err
	}
	if err := p.key("team-shutdown-final-detail", "Enter", func(text string) bool {
		return hasPanelView("team | shutdown detail")(text) && strings.Contains(text, "request terminated | phase terminated")
	}); err != nil {
		return err
	}
	if err := p.close("team-shutdown-close"); err != nil {
		return err
	}
	resumeAfter, err := os.ReadFile(p.resumeTrace)
	if err != nil || !bytes.Equal(resumeBefore, resumeAfter) || rootRequests != p.state.modelRequests("parent") {
		return fmt.Errorf("shutdown or pause resumed work or started root inference: %v", err)
	}
	if err := p.checkpoint("team-shutdown-after"); err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(map[string]any{"shutdownMember": bob, "pausedMember": alice, "shutdownRequest": pending.Requests[0], "providerRequestsBefore": requestsBefore, "providerRequestsAfter": p.state.totalRequests(), "rootRequestsBefore": rootRequests, "rootRequestsAfter": p.state.modelRequests("parent"), "resumeTraceBefore": string(resumeBefore), "resumeTraceAfter": string(resumeAfter), "scope": "graceful shutdown and distinct force-pause"}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(p.evidence, "team-shutdown-preview.json"), receipt)
}
