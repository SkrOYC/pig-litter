package main

import (
	"fmt"
	"strings"
)

type collaborationProofSlot struct {
	Channel  string
	Platform string
}

type collaborationProofRule struct {
	Scenario          string
	ScenarioRevision  int
	Slots             []collaborationProofSlot
	ResearchScenarios []string
}

type collaborationProofRegistry map[string]collaborationProofRule

type collaborationReport struct {
	Target          collaborationTarget           `json:"target"`
	SelectionSHA256 string                        `json:"selection_sha256"`
	Parity          bool                          `json:"parity"`
	Requirements    []collaborationRequirementRow `json:"requirements"`
	Research        []collaborationResearchRow    `json:"research"`
}

type collaborationRequirementRow struct {
	ID       string                 `json:"id"`
	State    string                 `json:"state"`
	Reason   string                 `json:"reason,omitempty"`
	Evidence []string               `json:"evidence"`
	Slots    []collaborationSlotRow `json:"slots"`
}

type collaborationSlotRow struct {
	Channel  string   `json:"channel"`
	Platform string   `json:"platform"`
	State    string   `json:"state"`
	Evidence []string `json:"evidence"`
}

type collaborationResearchRow struct {
	ID       string `json:"id"`
	Outcome  string `json:"outcome"`
	Complete bool   `json:"complete"`
	Reason   string `json:"reason,omitempty"`
}

func collaborationReduceReport(catalog collaborationCatalog, evidence collaborationEvidenceSet, registry collaborationProofRegistry) (collaborationReport, error) {
	if err := collaborationValidateRegistry(catalog, registry); err != nil {
		return collaborationReport{}, err
	}
	report := collaborationReport{Target: evidence.Target, SelectionSHA256: evidence.SelectionSHA256, Parity: len(catalog.Requirements) > 0}
	for _, attempt := range evidence.Attempts {
		if attempt.Attempt.Purpose != "research" {
			continue
		}
		row := collaborationResearchRow{ID: attempt.Attempt.ID, Outcome: attempt.Attempt.Outcome, Reason: attempt.Attempt.Reason}
		row.Complete = collaborationResearchComplete(attempt)
		report.Research = append(report.Research, row)
	}
	for _, requirement := range catalog.Requirements {
		row := collaborationRequirementRow{ID: requirement.ID, State: "unverified", Reason: "no implemented proof rule", Evidence: []string{}, Slots: []collaborationSlotRow{}}
		rule, exists := registry[requirement.ID]
		if !exists {
			report.Parity = false
			report.Requirements = append(report.Requirements, row)
			continue
		}
		if collaborationScenarioImplemented(catalog, rule.Scenario) {
			row.Reason = ""
			for _, slot := range rule.Slots {
				slotRow := collaborationReduceSlot(requirement.ID, rule, slot, evidence.Attempts)
				row.Evidence = append(row.Evidence, slotRow.Evidence...)
				row.Slots = append(row.Slots, slotRow)
			}
			row.State, row.Reason = collaborationRequirementState(requirement, row.Slots)
		}
		for _, attempt := range evidence.Attempts {
			if attempt.Attempt.Purpose != "research" || attempt.Attempt.Outcome != "unsupported" || !collaborationResearchComplete(attempt) || !collaborationContains(rule.ResearchScenarios, attempt.Attempt.Scenario) || !collaborationContains(attempt.Attempt.Claims, requirement.ID) {
				continue
			}
			row.Evidence = append(row.Evidence, attempt.Attempt.ID)
			if row.State == "matched" || row.State == "deliberately_different" {
				row.State = "unverified"
				row.Reason = "product evidence conflicts with an unsupported host finding"
			} else {
				row.State = "blocked"
				row.Reason = attempt.Attempt.Prerequisite
			}
		}
		if row.State != "matched" {
			report.Parity = false
		}
		report.Requirements = append(report.Requirements, row)
	}
	return report, nil
}

func collaborationResearchComplete(attempt collaborationValidatedAttempt) bool {
	if !attempt.ResultsPassed || !collaborationArtifactsComplete("live", attempt.ArtifactKinds) {
		return false
	}
	return attempt.Attempt.Outcome == "passed" || (attempt.Attempt.Outcome == "unsupported" && strings.TrimSpace(attempt.Attempt.Prerequisite) != "")
}

func collaborationValidateRegistry(catalog collaborationCatalog, registry collaborationProofRegistry) error {
	requirements := make(map[string]collaborationRequirement, len(catalog.Requirements))
	scenarios := make(map[string]collaborationScenario, len(catalog.Scenarios))
	for _, requirement := range catalog.Requirements {
		requirements[requirement.ID] = requirement
	}
	for _, scenario := range catalog.Scenarios {
		scenarios[scenario.ID] = scenario
	}
	for id, rule := range registry {
		requirement, exists := requirements[id]
		if !exists || rule.Scenario != requirement.AcceptanceScenario || rule.ScenarioRevision <= 0 || len(rule.Slots) == 0 {
			return fmt.Errorf("invalid proof rule for %q", id)
		}
		channels := make(map[string]bool)
		slots := make(map[collaborationProofSlot]bool)
		for _, slot := range rule.Slots {
			if !collaborationOneOf(slot.Channel, "unit", "live", "performance") || strings.TrimSpace(slot.Platform) == "" || slots[slot] {
				return fmt.Errorf("invalid or duplicate proof slot for %q", id)
			}
			slots[slot] = true
			channels[slot.Channel] = true
		}
		if !channels["unit"] || !channels["live"] || !channels["performance"] {
			return fmt.Errorf("proof rule for %q lacks unit, live, or performance coverage", id)
		}
		seenResearch := make(map[string]bool)
		for _, scenarioID := range rule.ResearchScenarios {
			scenario, found := scenarios[scenarioID]
			if !found || scenario.Owner != "P02" || seenResearch[scenarioID] {
				return fmt.Errorf("invalid research scenario for %q", id)
			}
			seenResearch[scenarioID] = true
		}
	}
	return nil
}

func collaborationScenarioImplemented(catalog collaborationCatalog, id string) bool {
	for _, scenario := range catalog.Scenarios {
		if scenario.ID == id {
			return scenario.Status == "implemented"
		}
	}
	return false
}

func collaborationReduceSlot(requirementID string, rule collaborationProofRule, slot collaborationProofSlot, attempts []collaborationValidatedAttempt) collaborationSlotRow {
	row := collaborationSlotRow{Channel: slot.Channel, Platform: slot.Platform, State: "unverified", Evidence: []string{}}
	outcomes := make(map[string]bool)
	for _, selected := range attempts {
		attempt := selected.Attempt
		if attempt.Purpose != "product" || attempt.Scenario != rule.Scenario || attempt.ScenarioRevision != rule.ScenarioRevision || attempt.Channel != slot.Channel || attempt.Platform != slot.Platform || !collaborationContains(attempt.Claims, requirementID) {
			continue
		}
		state := "unverified"
		switch attempt.Outcome {
		case "passed":
			if selected.ResultsPassed && collaborationArtifactsComplete(slot.Channel, selected.ArtifactKinds) {
				state = "matched"
			}
		case "blocked":
			state = "blocked"
		case "unsupported":
			if selected.ArtifactKinds["log"] && selected.ArtifactKinds["result"] {
				state = "unsupported"
			}
		}
		outcomes[state] = true
		row.Evidence = append(row.Evidence, attempt.ID)
	}
	if len(outcomes) == 1 {
		for state := range outcomes {
			row.State = state
		}
	}
	return row
}

func collaborationArtifactsComplete(channel string, kinds map[string]bool) bool {
	required := []string{"log", "result"}
	switch channel {
	case "live":
		required = append(required, "ansi", "pty", "timing", "screenshot", "video")
	case "performance":
		required = append(required, "timing", "samples")
	}
	for _, kind := range required {
		if !kinds[kind] {
			return false
		}
	}
	return true
}

func collaborationRequirementState(requirement collaborationRequirement, slots []collaborationSlotRow) (string, string) {
	allMatched := true
	var blocked, unsupported bool
	for _, slot := range slots {
		allMatched = allMatched && slot.State == "matched"
		blocked = blocked || slot.State == "blocked"
		unsupported = unsupported || slot.State == "unsupported"
	}
	if !allMatched {
		if blocked {
			return "blocked", "a required proof slot is blocked"
		}
		if unsupported {
			return "unsupported", "a required product capability is unsupported"
		}
		return "unverified", "required proof is missing, inconclusive, or conflicting"
	}
	switch requirement.ReferenceRelationship.State {
	case "equivalent":
		return "matched", ""
	case "deliberately_different":
		return "deliberately_different", requirement.ReferenceRelationship.Reason
	default:
		return "unverified", "reference relationship is unreviewed"
	}
}

func collaborationContains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
