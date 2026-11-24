package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const previewPlanRejection = "Please revise the rollout before writing."

type previewPlanRequest struct {
	ID     json.RawMessage `json:"requestId"`
	State  string          `json:"state"`
	Text   string          `json:"text"`
	Reason string          `json:"reason"`
}

type previewPlanResult struct {
	Operation string             `json:"operation"`
	State     string             `json:"state"`
	Request   previewPlanRequest `json:"request"`
}

func teamPlanTurn(request openAIRequest) (providerTurn, bool, error) {
	text := allMessageText(request.Messages)
	if request.Model == "held" && strings.Contains(text, "P07_PLAN_TASK_") {
		name := ""
		for _, candidate := range []string{"ALICE", "BOB"} {
			if strings.Contains(text, "P07_PLAN_TASK_"+candidate) {
				name = candidate
			}
		}
		if name == "" {
			return providerTurn{}, true, fmt.Errorf("unknown plan teammate")
		}
		messages := roleMessages(request.Messages, "tool")
		if len(messages) == 0 {
			return providerTurn{kind: "tools", calls: []toolCall{{name: "read", args: map[string]string{"path": "fixture-read.txt"}}}}, true, nil
		}
		var submitted, found *previewPlanResult
		for _, message := range messages {
			var result previewPlanResult
			if json.Unmarshal([]byte(contentText(message.Content)), &result) != nil || result.Operation != "team_plan" {
				continue
			}
			if result.State == "pending" {
				copy := result
				submitted = &copy
			} else if result.State == "found" {
				copy := result
				found = &copy
			} else {
				return providerTurn{}, true, fmt.Errorf("unexpected plan result: %+v", result)
			}
		}
		if submitted == nil {
			args := map[string]any{"action": "submit", "submissionId": "p07-plan-" + strings.ToLower(name), "text": "P07_PLAN_" + name, "timeoutMs": 60000}
			return providerTurn{kind: "tools", calls: []toolCall{{name: "team_plan", args: args}}}, true, nil
		}
		if found == nil {
			if len(submitted.Request.ID) == 0 {
				return providerTurn{}, true, fmt.Errorf("plan submission omitted its identity")
			}
			return providerTurn{kind: "hold", name: "p07-plan-" + strings.ToLower(name), calls: []toolCall{{name: "team_plan", args: map[string]any{"action": "get", "requestId": submitted.Request.ID}}}}, true, nil
		}
		expected := "approved"
		if name == "BOB" {
			expected = "rejected"
		}
		if !bytes.Equal(found.Request.ID, submitted.Request.ID) || found.Request.State != expected || name == "BOB" && found.Request.Reason != previewPlanRejection {
			return providerTurn{}, true, fmt.Errorf("teammate observed the wrong plan decision: %+v", found.Request)
		}
		return providerTurn{kind: "text", text: "P07_PLAN_OBSERVED_" + name}, true, nil
	}
	if request.Model != "parent" || latestRootPrompt(request.Messages).marker != "P07_PLAN_CONTROL" {
		return providerTurn{}, false, nil
	}
	members := map[string]bool{}
	for _, message := range roleMessages(request.Messages, "tool") {
		var result previewToolResult
		if json.Unmarshal([]byte(contentText(message.Content)), &result) == nil && result.Operation == "team_spawn" {
			if result.State != "starting" || result.Member.ID == "" || members[result.Name] {
				return providerTurn{}, true, fmt.Errorf("invalid plan teammate admission: %+v", result)
			}
			members[result.Name] = true
		}
	}
	for _, name := range []string{"Alice", "Bob"} {
		if !members[name] {
			args := map[string]any{"type": "scout", "name": name, "model": "fixture/held", "task": "P07_PLAN_TASK_" + strings.ToUpper(name), "planRequired": true, "workspace": map[string]any{"kind": "shared_checkout"}}
			return providerTurn{kind: "tools", calls: []toolCall{{name: "team_spawn", args: args}}}, true, nil
		}
	}
	return providerTurn{kind: "text", text: "P07_PLANS_READY"}, true, nil
}

func (p *panelProof) readPreviewPlan(member previewMember) (previewPlanRequest, []byte, error) {
	path := filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, fmt.Sprintf("plan-%s-%d.json", member.Ref.ID, member.Ref.Generation))
	data, err := os.ReadFile(path)
	if err != nil {
		return previewPlanRequest{}, nil, err
	}
	var ledger struct {
		Requests []previewPlanRequest `json:"requests"`
	}
	if err := json.Unmarshal(data, &ledger); err != nil || len(ledger.Requests) != 1 {
		return previewPlanRequest{}, nil, fmt.Errorf("invalid plan ledger for %s", member.Name)
	}
	return ledger.Requests[0], data, nil
}

func (p *panelProof) teamPlansProof(media *collaborationMedia) error {
	if err := p.sendText("P07_PLAN_CONTROL"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	var alice, bob previewMember
	var first, second previewPlanRequest
	if err := waitFor(p.ctx, "pending plans for two active teammates", stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		if !p.state.holding("p07-plan-alice", "p07-plan-bob") || !visibleContains(p.visible, "P07_PLANS_READY") {
			return false, nil
		}
		data, err := os.ReadFile(filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, "team.json"))
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
		if alice.Ref.ID == "" || bob.Ref.ID == "" || alice.Ref == bob.Ref {
			return false, nil
		}
		first, _, err = p.readPreviewPlan(alice)
		if err != nil {
			return false, nil
		}
		second, _, err = p.readPreviewPlan(bob)
		return err == nil && first.State == "pending" && second.State == "pending" && !bytes.Equal(first.ID, second.ID), nil
	}); err != nil {
		return err
	}
	if err := p.awaitRootIdle("P07_PLAN_CONTROL"); err != nil {
		return err
	}
	requests, memberRequests := p.state.totalRequests(), p.state.modelRequests("held")
	resumeBefore, err := os.ReadFile(p.resumeTrace)
	if err != nil {
		return err
	}
	if err := p.checkpoint("p07-plans-before"); err != nil {
		return err
	}
	if err := p.openTeam("p07-plans-open"); err != nil {
		return err
	}
	if err := p.key("p07-plans-list", "g", func(text string) bool {
		return hasPanelView("team | plans")(text) && strings.Contains(selectedPanelRow(text), "Alice") && strings.Contains(text, "P07_PLAN_BOB")
	}); err != nil {
		return err
	}
	if err := p.key("p07-plans-detail", "Enter", func(text string) bool {
		return hasPanelView("team | plan detail")(text) && strings.Contains(text, "P07_PLAN_ALICE")
	}); err != nil {
		return err
	}
	if err := p.key("p07-plans-approve", "a", func(text string) bool { return strings.Contains(text, "Plan #1 approved") }); err != nil {
		return err
	}
	approved, approvedData, err := p.readPreviewPlan(alice)
	if err != nil || approved.State != "approved" || !bytes.Equal(approved.ID, first.ID) {
		return fmt.Errorf("selected plan was not approved: %+v, %v", approved, err)
	}
	stillPending, _, err := p.readPreviewPlan(bob)
	if err != nil || stillPending.State != "pending" || !bytes.Equal(stillPending.ID, second.ID) {
		return fmt.Errorf("approval changed the other request: %+v, %v", stillPending, err)
	}
	if err := p.key("p07-plans-list-again", "Escape", hasPanelView("team | plans")); err != nil {
		return err
	}
	if err := p.key("p07-plans-select-bob", "Down", func(text string) bool { return strings.Contains(selectedPanelRow(text), "Bob") }); err != nil {
		return err
	}
	if err := p.key("p07-plans-reject", "r", hasPanelView("team | reject plan")); err != nil {
		return err
	}
	if err := p.action("p07-plans-rejection-reason", func() error { return p.sendText(previewPlanRejection) }, func(text string) bool { return strings.Contains(text, previewPlanRejection) }); err != nil {
		return err
	}
	if err := p.key("p07-plans-reject-submit", "Enter", func(text string) bool {
		return hasPanelView("team | plans")(text) && strings.Contains(text, "Plan #1 rejected")
	}); err != nil {
		return err
	}
	rejected, rejectedData, err := p.readPreviewPlan(bob)
	if err != nil || rejected.State != "rejected" || rejected.Reason != previewPlanRejection || !bytes.Equal(rejected.ID, second.ID) {
		return fmt.Errorf("selected rejection was not stored: %+v, %v", rejected, err)
	}
	if err := writePrivate(filepath.Join(p.evidence, "p07-plan-alice.json"), approvedData); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(p.evidence, "p07-plan-bob.json"), rejectedData); err != nil {
		return err
	}
	if media != nil {
		if err := media.markObservation(); err != nil {
			return err
		}
	}
	if err := p.close("p07-plans-close"); err != nil {
		return err
	}
	resumeAfter, err := os.ReadFile(p.resumeTrace)
	requestsBeforeRelease, membersBeforeRelease := p.state.totalRequests(), p.state.modelRequests("held")
	if err != nil || !bytes.Equal(resumeBefore, resumeAfter) || requests != requestsBeforeRelease || memberRequests != membersBeforeRelease {
		return fmt.Errorf("plan decisions added provider or resume activity before release: %v", err)
	}
	if err := p.state.releaseHold("p07-plan-alice"); err != nil {
		return err
	}
	if err := p.state.releaseHold("p07-plan-bob"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "teammates observed plan decisions", stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		aliceNative, aliceErr := p.childNative(alice.Ref.ID)
		bobNative, bobErr := p.childNative(bob.Ref.ID)
		return aliceErr == nil && bobErr == nil && bytes.Contains(aliceNative.Raw, []byte("P07_PLAN_OBSERVED_ALICE")) && bytes.Contains(bobNative.Raw, []byte("P07_PLAN_OBSERVED_BOB")) && bytes.Contains(bobNative.Raw, []byte(previewPlanRejection)), nil
	}); err != nil {
		return err
	}
	if err := p.checkpoint("p07-plans-after"); err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(map[string]any{"approved": approved, "rejected": rejected, "providerRequestsBefore": requests, "providerRequestsBeforeRelease": requestsBeforeRelease, "memberRequestsBefore": memberRequests, "memberRequestsBeforeRelease": membersBeforeRelease, "providerRequestsAfterRelease": p.state.totalRequests(), "memberRequestsAfterRelease": p.state.modelRequests("held"), "resumeTraceBefore": string(resumeBefore), "resumeTraceBeforeRelease": string(resumeAfter), "scope": "selected plan decisions and teammate readback"}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(p.evidence, "team-plans-preview.json"), receipt)
}
