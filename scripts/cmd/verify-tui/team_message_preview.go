package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const previewHumanMessage = "P07 human message: exact text 123"
const previewCancelledMessage = "P07 cancelled draft"

func teamMessageTurn(request openAIRequest) (providerTurn, bool, error) {
	text := allMessageText(request.Messages)
	if request.Model == "held" && strings.Contains(text, "P07_MESSAGE_TASK_") {
		if len(roleMessages(request.Messages, "tool")) == 0 {
			return providerTurn{kind: "tools", calls: []toolCall{{name: "read", args: map[string]string{"path": "fixture-read.txt"}}}}, true, nil
		}
		if strings.Contains(text, "P07_MESSAGE_TASK_ALICE") {
			if strings.Contains(text, previewHumanMessage) || strings.Contains(text, previewCancelledMessage) {
				return providerTurn{}, true, fmt.Errorf("the unselected teammate received human input")
			}
			return providerTurn{kind: "hold", name: "p07-alice", text: "P07_ALICE_INITIAL"}, true, nil
		}
		if strings.Contains(text, "P07_MESSAGE_TASK_BOB") {
			count := 0
			for _, message := range request.Messages {
				if strings.Contains(contentText(message.Content), previewHumanMessage) {
					if message.Role != "user" || contentText(message.Content) != previewHumanMessage {
						return providerTurn{}, true, fmt.Errorf("human input lost its exact text or user role")
					}
					count++
				}
				if strings.Contains(contentText(message.Content), previewCancelledMessage) {
					return providerTurn{}, true, fmt.Errorf("a cancelled draft reached the provider")
				}
			}
			if count == 1 {
				return providerTurn{kind: "text", text: "P07_BOB_MESSAGE_ACK"}, true, nil
			}
			if count != 0 {
				return providerTurn{}, true, fmt.Errorf("human input reached the provider more than once")
			}
			return providerTurn{kind: "hold", name: "p07-bob", text: "P07_BOB_INITIAL"}, true, nil
		}
		return providerTurn{}, true, fmt.Errorf("unknown teammate task")
	}
	if request.Model != "parent" || latestRootPrompt(request.Messages).marker != "P07_TEAM_MESSAGE_SETUP" {
		return providerTurn{}, false, nil
	}
	members := map[string]bool{}
	for _, message := range roleMessages(request.Messages, "tool") {
		var result previewToolResult
		if err := json.Unmarshal([]byte(contentText(message.Content)), &result); err != nil {
			return providerTurn{}, true, err
		}
		if result.Operation != "team_spawn" {
			continue
		}
		if result.State != "starting" || result.Member.ID == "" || result.Admission == "" || members[result.Name] {
			return providerTurn{}, true, fmt.Errorf("invalid teammate admission: %+v", result)
		}
		members[result.Name] = true
	}
	for _, name := range []string{"Alice", "Bob"} {
		if !members[name] {
			args := map[string]any{"type": "scout", "name": name, "model": "fixture/held", "task": "P07_MESSAGE_TASK_" + strings.ToUpper(name), "workspace": map[string]any{"kind": "shared_checkout"}}
			return providerTurn{kind: "tools", calls: []toolCall{{name: "team_spawn", args: args}}}, true, nil
		}
	}
	return providerTurn{kind: "text", text: "P07_TEAM_MESSAGE_READY"}, true, nil
}

func (p *panelProof) teamMessageProof() error {
	if err := p.sendText("P07_TEAM_MESSAGE_SETUP"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	var members []previewMember
	if err := waitFor(p.ctx, "two active teammates", stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		if !p.state.holding("p07-alice", "p07-bob") || !visibleContains(p.visible, "P07_TEAM_MESSAGE_READY") {
			return false, nil
		}
		root, err := p.root()
		if err != nil {
			return false, nil
		}
		path := filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, "team.json")
		data, err := os.ReadFile(path)
		var roster struct {
			Members []previewMember `json:"members"`
		}
		if err != nil || json.Unmarshal(data, &roster) != nil || len(roster.Members) != 2 {
			return false, nil
		}
		for _, member := range roster.Members {
			child, ok := root.Children[member.Ref.ID]
			if !ok || child.Name != member.Name || child.State != "running" || member.State != "running" {
				return false, nil
			}
		}
		members = roster.Members
		return true, nil
	}); err != nil {
		return err
	}
	byName := map[string]previewMember{}
	for _, member := range members {
		if member.Ref.Team.Root != p.rootID || member.Ref.Team.Epoch == 0 || member.Ref.Generation == 0 || member.AdmissionID == "" {
			return fmt.Errorf("invalid teammate identity: %+v", member)
		}
		byName[member.Name] = member
	}
	alice, aliceOK := byName["Alice"]
	bob, bobOK := byName["Bob"]
	if !aliceOK || !bobOK || alice.Ref == bob.Ref {
		return fmt.Errorf("active teammate identities are ambiguous: %+v", members)
	}
	if err := p.awaitRootIdle("P07_TEAM_MESSAGE_SETUP"); err != nil {
		return err
	}
	before, err := p.quiescentLogs()
	if err != nil {
		return err
	}
	aliceBefore, err := p.childNative(alice.Ref.ID)
	if err != nil {
		return err
	}
	bobBefore, err := p.childNative(bob.Ref.ID)
	if err != nil {
		return err
	}
	requestsBefore := p.state.totalRequests()
	memberRequestsBefore := p.state.modelRequests("held")
	rootRequestsBefore := p.state.modelRequests("parent")
	resumesBefore, err := os.ReadFile(p.resumeTrace)
	if err != nil {
		return err
	}
	if memberRequestsBefore != 4 {
		return fmt.Errorf("expected four setup teammate requests, got %d", memberRequestsBefore)
	}
	if err := p.checkpoint("p07-message-before-view"); err != nil {
		return err
	}
	if err := p.openTeam("p07-message-passive-open"); err != nil {
		return err
	}
	if err := p.key("p07-message-select-bob", "End", func(text string) bool { return strings.Contains(selectedPanelRow(text), "Bob") }); err != nil {
		return err
	}
	if err := p.key("p07-message-passive-detail", "Enter", func(text string) bool {
		return hasPanelView("team | detail")(text) && strings.Contains(text, bob.Ref.ID)
	}); err != nil {
		return err
	}
	if err := p.key("p07-message-passive-transcript", "t", func(text string) bool {
		return hasPanelView("team | transcript")(text) && strings.Contains(text, "P07_MESSAGE_TASK_BOB")
	}); err != nil {
		return err
	}
	if err := p.close("p07-message-passive-close"); err != nil {
		return err
	}
	if err := p.logsUnchanged(before); err != nil {
		return err
	}
	passiveResumes, err := os.ReadFile(p.resumeTrace)
	if err != nil || string(passiveResumes) != string(resumesBefore) || p.state.totalRequests() != requestsBefore {
		return fmt.Errorf("passive viewing changed provider or resume activity: %v", err)
	}
	if err := p.openTeam("p07-message-open"); err != nil {
		return err
	}
	if text, err := p.visible(); err != nil || !strings.Contains(selectedPanelRow(text), "Bob") {
		return fmt.Errorf("selected teammate changed before messaging: %s %v", text, err)
	}
	if err := p.key("p07-message-cancel-editor", "m", func(text string) bool {
		return hasPanelView("team | message")(text) && strings.Contains(text, bob.Ref.ID)
	}); err != nil {
		return err
	}
	if err := p.action("p07-message-cancel-draft", func() error { return p.sendText(previewCancelledMessage) }, func(text string) bool { return strings.Contains(text, previewCancelledMessage) }); err != nil {
		return err
	}
	if err := p.key("p07-message-cancel", "Escape", hasPanelView("team")); err != nil {
		return err
	}
	if err := p.key("p07-message-editor", "m", hasPanelView("team | message")); err != nil {
		return err
	}
	if err := p.key("p07-message-follow-up", "Tab", func(text string) bool { return strings.Contains(text, "Message mode follow_up") }); err != nil {
		return err
	}
	if err := p.action("p07-message-draft", func() error { return p.sendText(previewHumanMessage) }, func(text string) bool { return strings.Contains(text, previewHumanMessage) }); err != nil {
		return err
	}
	if err := p.key("p07-message-submit", "Enter", func(text string) bool { return hasPanelView("team")(text) && strings.Contains(text, "Message queued") }); err != nil {
		return err
	}
	if err := p.close("p07-message-close"); err != nil {
		return err
	}
	if p.state.totalRequests() != requestsBefore || !p.state.holding("p07-alice", "p07-bob") {
		return fmt.Errorf("a provider request occurred before the held continuation was released")
	}
	if err := p.state.releaseHold("p07-bob"); err != nil {
		return err
	}
	if err := p.waitNative("selected teammate message continuation", func(root nativeSession) bool {
		child := root.Children[bob.Ref.ID]
		return child.State == "completed" && child.Latest != nil && child.Latest.Text == "P07_BOB_MESSAGE_ACK" && p.state.holding("p07-alice")
	}); err != nil {
		return err
	}
	aliceAfter, err := p.childNative(alice.Ref.ID)
	if err != nil {
		return err
	}
	bobAfter, err := p.childNative(bob.Ref.ID)
	if err != nil {
		return err
	}
	rootAfter, err := p.root()
	if err != nil {
		return err
	}
	if string(aliceBefore.Raw) != string(aliceAfter.Raw) || exactNativeUserCount(bobAfter, previewHumanMessage) != 1 || nativeTextCount(bobAfter, previewHumanMessage) != 1 || nativeTextCount(aliceAfter, previewHumanMessage) != 0 || nativeTextCount(rootAfter, previewHumanMessage) != 0 || nativeTextCount(bobAfter, previewCancelledMessage) != 0 || nativeTextCount(aliceAfter, previewCancelledMessage) != 0 || nativeTextCount(rootAfter, previewCancelledMessage) != 0 {
		return fmt.Errorf("native history did not isolate exactly one user message to Bob")
	}
	if exactNativeUserCount(bobBefore, previewHumanMessage) != 0 || p.state.totalRequests() != requestsBefore+1 || p.state.modelRequests("held") != memberRequestsBefore+1 || p.state.modelRequests("parent") != rootRequestsBefore {
		return fmt.Errorf("message continuation changed an unexpected provider count")
	}
	resumesAfter, err := os.ReadFile(p.resumeTrace)
	if err != nil || string(resumesAfter) != string(resumesBefore) {
		return fmt.Errorf("message or viewing invoked Runtime.Resume: %v", err)
	}
	if err := p.checkpoint("p07-message-after-continuation"); err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(map[string]any{"scope": "active teammate human message preview", "members": members, "selected": bob.Ref, "message": previewHumanMessage, "mode": "follow_up", "nativeUserMatches": exactNativeUserCount(bobAfter, previewHumanMessage), "unselectedHistoryUnchanged": string(aliceBefore.Raw) == string(aliceAfter.Raw), "passiveHistoryBefore": before, "providerRequestsBefore": requestsBefore, "providerRequestsAfter": p.state.totalRequests(), "memberRequestsBefore": memberRequestsBefore, "memberRequestsAfter": p.state.modelRequests("held"), "rootRequestsBefore": rootRequestsBefore, "rootRequestsAfter": p.state.modelRequests("parent"), "resumeTraceBefore": string(resumesBefore), "resumeTraceAfter": string(resumesAfter), "continuation": "one selected teammate provider request after explicit message and barrier release"}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(p.evidence, "team-message-preview.json"), receipt)
}

func nativeTextCount(session nativeSession, text string) int {
	count := 0
	for _, raw := range session.Messages {
		var message openAIMessage
		if json.Unmarshal(raw, &message) == nil && strings.Contains(contentText(message.Content), text) {
			count++
		}
	}
	return count
}

func exactNativeUserCount(session nativeSession, text string) int {
	count := 0
	for _, raw := range session.Messages {
		var message openAIMessage
		if json.Unmarshal(raw, &message) == nil && message.Role == "user" && contentText(message.Content) == text {
			count++
		}
	}
	return count
}
