package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func (p *panelProof) teamPauseProof(media *collaborationMedia) error {
	if err := p.sendText("TEAM_MESSAGE_SETUP"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	path := filepath.Join(p.agentDir, "pig-litter", "roots", p.rootID, "team.json")
	var members []previewMember
	if err := waitFor(p.ctx, "active teammates for pause", stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		if !p.state.holding("team-alice", "team-bob") || !visibleContains(p.visible, "TEAM_MESSAGE_READY") {
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
			if member.State != "running" {
				return false, nil
			}
		}
		members = roster.Members
		return true, nil
	}); err != nil {
		return err
	}
	byName := make(map[string]previewMember, len(members))
	for _, member := range members {
		byName[member.Name] = member
	}
	alice, aliceOK := byName["Alice"]
	bob, bobOK := byName["Bob"]
	if !aliceOK || !bobOK || alice.Ref == bob.Ref || bob.Ref.ID == "" || bob.AdmissionID == "" {
		return fmt.Errorf("invalid pause fixture identities: %+v", members)
	}
	if err := p.awaitRootIdle("TEAM_MESSAGE_SETUP"); err != nil {
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
	requests, memberRequests := p.state.totalRequests(), p.state.modelRequests("held")
	if err := p.checkpoint("team-pause-before"); err != nil {
		return err
	}
	if err := p.openTeam("team-pause-open"); err != nil {
		return err
	}
	if err := p.key("team-pause-select-bob", "End", func(text string) bool { return strings.Contains(selectedPanelRow(text), "Bob") }); err != nil {
		return err
	}
	if err := p.key("team-pause-selected", "p", func(text string) bool {
		return strings.Contains(text, "Paused subtree") && strings.Contains(selectedPanelRow(text), "Bob | member paused")
	}); err != nil {
		return err
	}
	var after []byte
	if err := waitFor(p.ctx, "durable paused teammate", stageTimeout, func() (bool, error) {
		data, err := os.ReadFile(path)
		var roster struct {
			Members []previewMember `json:"members"`
		}
		if err != nil || json.Unmarshal(data, &roster) != nil || len(roster.Members) != 2 {
			return false, nil
		}
		foundBob, foundAlice := false, false
		for _, member := range roster.Members {
			foundBob = foundBob || member.Ref == bob.Ref && member.State == "paused"
			foundAlice = foundAlice || member.Ref == alice.Ref && member.State == "running"
		}
		root, err := p.root()
		child, present := root.Children[bob.Ref.ID]
		if err != nil || !foundBob || !foundAlice || !present || child.ID != bob.Ref.ID || !child.Paused || child.State != "paused" {
			return false, nil
		}
		after = data
		return true, nil
	}); err != nil {
		return err
	}
	if media != nil {
		if err := media.markObservation(); err != nil {
			return err
		}
	}
	if err := writePrivate(filepath.Join(p.evidence, "team-pause-roster.json"), after); err != nil {
		return err
	}
	if err := p.close("team-pause-close"); err != nil {
		return err
	}
	if err := p.openTeam("team-pause-reopen"); err != nil {
		return err
	}
	if text, err := p.visible(); err != nil || !strings.Contains(selectedPanelRow(text), "Bob | member paused") {
		return fmt.Errorf("paused selection was not retained: %s %v", text, err)
	}
	if err := p.close("team-pause-reopen-close"); err != nil {
		return err
	}
	aliceAfter, err := p.childNative(alice.Ref.ID)
	if err != nil || !bytes.Equal(aliceBefore.Raw, aliceAfter.Raw) || !p.state.holding("team-alice") {
		return fmt.Errorf("pause changed the other teammate: %v", err)
	}
	bobAfter, err := p.childNative(bob.Ref.ID)
	if err != nil || bobAfter.ID != bob.Ref.ID {
		return fmt.Errorf("pause lost the selected teammate session: %v", err)
	}
	resumeAfter, err := os.ReadFile(p.resumeTrace)
	if err != nil || !bytes.Equal(resumeBefore, resumeAfter) || requests != p.state.totalRequests() || memberRequests != p.state.modelRequests("held") {
		return fmt.Errorf("pause added provider or resume activity: %v", err)
	}
	if err := p.checkpoint("team-pause-after"); err != nil {
		return err
	}
	receipt, err := json.MarshalIndent(map[string]any{"paused": bob, "untouched": alice, "providerRequestsBefore": requests, "providerRequestsAfter": p.state.totalRequests(), "memberRequestsBefore": memberRequests, "memberRequestsAfter": p.state.modelRequests("held"), "resumeTraceBefore": string(resumeBefore), "resumeTraceAfter": string(resumeAfter), "scope": "selected teammate force-pause"}, "", "  ")
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(p.evidence, "team-pause-preview.json"), receipt)
}
