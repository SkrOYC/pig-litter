package main

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"
)

type collaborationProofSlot struct {
	Channel      string
	Platform     string
	Architecture string
	Terminal     string
}

type collaborationProofRule struct {
	Scenario         string
	ScenarioRevision int
	Applicability    string
	Slots            []collaborationProofSlot
	Research         []collaborationResearchDependency
}

type collaborationProofRegistry map[string]collaborationProofRule

type collaborationReport struct {
	Target              collaborationTarget            `json:"target"`
	SelectionSHA256     string                         `json:"selection_sha256"`
	Parity              bool                           `json:"parity"`
	MissingRequirements []string                       `json:"missing_requirements"`
	ProgramTargets      []collaborationTargetRow       `json:"program_targets"`
	Requirements        []collaborationRequirementRow  `json:"requirements"`
	Research            []collaborationResearchRow     `json:"research"`
	Verification        []collaborationVerificationRow `json:"verification"`
}

type collaborationRequirementRow struct {
	ID            string                   `json:"id"`
	State         string                   `json:"state"`
	Reason        string                   `json:"reason,omitempty"`
	Prerequisites []string                 `json:"prerequisites,omitempty"`
	Evidence      []string                 `json:"evidence"`
	Slots         []collaborationSlotRow   `json:"slots"`
	Targets       []collaborationTargetRow `json:"targets"`
}

type collaborationSlotRow struct {
	Channel      string   `json:"channel"`
	Platform     string   `json:"platform"`
	Architecture string   `json:"architecture,omitempty"`
	Terminal     string   `json:"terminal,omitempty"`
	State        string   `json:"state"`
	Evidence     []string `json:"evidence"`
}

type collaborationPlatformScope struct {
	Platform     string `json:"platform"`
	Architecture string `json:"architecture,omitempty"`
	Terminal     string `json:"terminal,omitempty"`
}

type collaborationResearchDependency struct {
	Scenario   string
	Revision   int
	Capability string
	Scope      collaborationPlatformScope
}

type collaborationTargetRow struct {
	Scope         collaborationPlatformScope `json:"scope"`
	State         string                     `json:"state"`
	Reason        string                     `json:"reason,omitempty"`
	Prerequisites []string                   `json:"prerequisites,omitempty"`
	Evidence      []string                   `json:"evidence"`
}

type collaborationResearchRow struct {
	ID           string                          `json:"id"`
	Scenario     string                          `json:"scenario"`
	Revision     int                             `json:"revision"`
	Scope        collaborationPlatformScope      `json:"scope"`
	Observation  string                          `json:"observation"`
	Outcome      string                          `json:"outcome"`
	Finding      *collaborationCapabilityFinding `json:"finding,omitempty"`
	Complete     bool                            `json:"complete"`
	Conflict     bool                            `json:"conflict"`
	Reason       string                          `json:"reason,omitempty"`
	Prerequisite string                          `json:"prerequisite,omitempty"`
}

type collaborationVerificationRow struct {
	ID       string                     `json:"id"`
	Scenario string                     `json:"scenario"`
	Channel  string                     `json:"channel"`
	Scope    collaborationPlatformScope `json:"scope"`
	Outcome  string                     `json:"outcome"`
	Complete bool                       `json:"complete"`
	Reason   string                     `json:"reason,omitempty"`
}

var collaborationApplicabilityProfiles = map[string][]collaborationPlatformScope{
	"core":   {{Platform: "linux"}, {Platform: "macos"}},
	"iterm2": {{Platform: "macos", Terminal: "iterm2"}},
}

var collaborationApplicabilityGroups = []struct {
	Scenario string
	Profile  string
	IDs      []string
}{
	{"team-roster", "core", []string{
		"TEAM-001", "TEAM-002", "TEAM-003", "TEAM-004", "TEAM-005", "TEAM-006", "TEAM-007",
		"TEAM-008", "TEAM-009", "TEAM-010", "TEAM-011", "TEAM-012", "TEAM-013",
	}},
	{"team-messages", "core", []string{
		"MSG-001", "MSG-002", "MSG-003", "MSG-004", "MSG-005", "MSG-006", "MSG-010", "MSG-011", "MSG-012",
	}},
	{"team-forgery", "core", []string{"MSG-007", "MSG-008", "MSG-009"}},
	{"team-tasks", "core", []string{
		"TASK-001", "TASK-002", "TASK-003", "TASK-004", "TASK-005", "TASK-006", "TASK-007", "TASK-008", "FRAME-007", "FRAME-008",
	}},
	{"team-hooks", "core", []string{"TASK-009", "GATE-008"}},
	{"team-shutdown", "core", []string{"TASK-010", "GATE-005", "FRAME-003", "FRAME-004", "FRAME-005", "FRAME-006", "FRAME-009"}},
	{"team-plan", "core", []string{
		"GATE-001", "GATE-002", "GATE-003", "GATE-004", "GATE-006", "FRAME-001", "FRAME-002",
		"FRAME-011", "FRAME-012", "FRAME-013", "FRAME-014", "FRAME-015", "FRAME-016",
	}},
	{"team-idle", "core", []string{"GATE-007", "FRAME-010"}},
	{"team-local-peers", "core", []string{
		"PEER-001", "PEER-002", "PEER-003", "PEER-004", "PEER-005", "PEER-006", "PEER-007", "PEER-008", "PEER-009", "UI-007",
	}},
	{"team-observers", "core", []string{"OBS-001", "OBS-002", "OBS-003", "OBS-004", "OBS-005"}},
	{"team-view", "core", []string{"UI-001", "UI-002", "UI-003", "UI-004", "UI-005", "UI-006", "UI-008", "UI-009"}},
	{"team-backends", "core", []string{"BACKEND-001", "BACKEND-002", "BACKEND-004", "BACKEND-005", "BACKEND-006", "BACKEND-007"}},
	{"team-backends", "iterm2", []string{"BACKEND-003"}},
	{"team-hooks", "unreviewed", []string{"TASK-011"}},
	{"team-local-peers", "unreviewed", []string{"PEER-010"}},
	{"team-observers", "unreviewed", []string{"OBS-006"}},
	{"team-parity-report", "unreviewed", []string{"FILE-001"}},
}

func collaborationApplicability(requirement collaborationRequirement) (string, []collaborationPlatformScope) {
	for _, group := range collaborationApplicabilityGroups {
		if group.Scenario == requirement.AcceptanceScenario && collaborationContains(group.IDs, requirement.ID) {
			return group.Profile, collaborationApplicabilityProfiles[group.Profile]
		}
	}
	return "", nil
}

func collaborationReduceReport(evidence collaborationEvidenceSet, registry collaborationProofRegistry) (collaborationReport, error) {
	if evidence.Target != evidence.expectedTarget {
		return collaborationReport{}, fmt.Errorf("selected target changed")
	}
	if err := evidence.expectedTarget.validate(); err != nil {
		return collaborationReport{}, fmt.Errorf("selected target: %w", err)
	}
	for _, attempt := range evidence.Attempts {
		if attempt.Attempt.Target != evidence.expectedTarget {
			return collaborationReport{}, fmt.Errorf("attempt %q target changed", attempt.Attempt.ID)
		}
	}
	digest := sha256.Sum256(evidence.catalogData)
	if hex.EncodeToString(digest[:]) != evidence.Target.CatalogSHA256 {
		return collaborationReport{}, fmt.Errorf("selected catalog digest changed")
	}
	catalog, err := collaborationReadCatalog(evidence.catalogData)
	if err != nil {
		return collaborationReport{}, err
	}
	if err := collaborationValidateRegistry(catalog, registry); err != nil {
		return collaborationReport{}, err
	}
	missing := collaborationMissingRequirements(catalog)
	report := collaborationReport{Target: evidence.Target, SelectionSHA256: evidence.SelectionSHA256, Parity: len(catalog.Requirements) > 0 && len(missing) == 0, MissingRequirements: missing}
	for _, attempt := range evidence.Attempts {
		scope := collaborationPlatformScope{Platform: attempt.Attempt.Platform, Architecture: attempt.Attempt.Architecture, Terminal: attempt.Attempt.Terminal}
		switch attempt.Attempt.Purpose {
		case "research":
			row := collaborationResearchRow{ID: attempt.Attempt.ID, Scenario: attempt.Attempt.Scenario, Revision: attempt.Attempt.ScenarioRevision, Scope: scope, Observation: attempt.Attempt.Observation, Outcome: attempt.Attempt.Outcome, Finding: attempt.Attempt.Finding, Reason: attempt.Attempt.Reason, Prerequisite: attempt.Attempt.Prerequisite}
			row.Complete = collaborationResearchComplete(attempt) && collaborationBaselineScenarioRegistered(catalog, attempt.Attempt)
			if !row.Complete {
				if row.Reason == "" {
					row.Reason = "research scenario, scope, or evidence is incomplete"
				}
				report.Parity = false
			}
			report.Research = append(report.Research, row)
		case "verification":
			row := collaborationVerificationRow{ID: attempt.Attempt.ID, Scenario: attempt.Attempt.Scenario, Channel: attempt.Attempt.Channel, Scope: scope, Outcome: attempt.Attempt.Outcome, Reason: attempt.Attempt.Reason}
			row.Complete = collaborationBaselineScenarioRegistered(catalog, attempt.Attempt) && attempt.Attempt.Outcome == "passed" && attempt.ResultsPassed && collaborationArtifactsComplete(attempt.Attempt.Channel, attempt.ArtifactKinds)
			if !row.Complete {
				if row.Reason == "" {
					row.Reason = "fixture result or required artifacts are incomplete"
				}
				report.Parity = false
			}
			report.Verification = append(report.Verification, row)
		}
	}
	if collaborationMarkResearchConflicts(report.Research) {
		report.Parity = false
	}
	for _, requirement := range catalog.Requirements {
		row := collaborationRequirementRow{ID: requirement.ID, State: "unverified", Reason: "no implemented proof rule", Evidence: []string{}, Slots: []collaborationSlotRow{}, Targets: []collaborationTargetRow{}}
		profile, scopes := collaborationApplicability(requirement)
		row.Targets = collaborationReduceTargets(requirement, scopes, nil)
		rule, exists := registry[requirement.ID]
		if !exists {
			report.Parity = false
			report.Requirements = append(report.Requirements, row)
			continue
		}
		if profile == "" || rule.Applicability != profile || len(scopes) == 0 {
			row.Reason = "missing or mismatched trusted applicability profile"
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
			row.Targets = collaborationReduceTargets(requirement, scopes, row.Slots)
			for _, target := range row.Targets {
				if (row.State == "matched" || row.State == "deliberately_different") && target.State != row.State {
					row.State = "unverified"
					row.Reason = "required target proof is missing or incomplete"
				}
			}
		}
		collaborationApplyResearch(&row, requirement.ID, rule.Research, evidence.Attempts, catalog)
		if row.State != "matched" {
			report.Parity = false
		}
		report.Requirements = append(report.Requirements, row)
	}
	report.ProgramTargets = collaborationReduceProgramTargets(catalog, report.Requirements, missing)
	for _, target := range report.ProgramTargets {
		if target.State != "matched" {
			report.Parity = false
		}
	}
	return report, nil
}

func collaborationMarkResearchConflicts(rows []collaborationResearchRow) bool {
	conflict := false
	for index := range rows {
		for other := index + 1; other < len(rows); other++ {
			a, b := &rows[index], &rows[other]
			if !a.Complete || !b.Complete || a.Finding == nil || b.Finding == nil || a.Scenario != b.Scenario || a.Revision != b.Revision || a.Scope != b.Scope || a.Finding.Capability != b.Finding.Capability {
				continue
			}
			if a.Finding.Verdict != b.Finding.Verdict {
				a.Conflict, b.Conflict, conflict = true, true, true
			}
		}
	}
	return conflict
}

func collaborationReduceProgramTargets(catalog collaborationCatalog, requirements []collaborationRequirementRow, missing []string) []collaborationTargetRow {
	var targets []collaborationTargetRow
	for _, profile := range []string{"core", "iterm2"} {
		for _, scope := range collaborationApplicabilityProfiles[profile] {
			row := collaborationTargetRow{Scope: scope, State: "unverified", Reason: "one or more applicable requirements lack matched target evidence", Evidence: []string{}}
			matched := len(missing) == 0 && len(requirements) == len(catalog.Requirements)
			applicable := 0
			for index, requirement := range catalog.Requirements {
				_, scopes := collaborationApplicability(requirement)
				if len(scopes) == 0 {
					matched = false
					continue
				}
				for _, required := range scopes {
					if !collaborationProgramScopeIncludes(scope, required) {
						continue
					}
					applicable++
					if index >= len(requirements) || requirements[index].ID != requirement.ID {
						matched = false
						continue
					}
					found := false
					for _, target := range requirements[index].Targets {
						if target.Scope == required {
							found = target.State == "matched"
							row.Evidence = append(row.Evidence, target.Evidence...)
							break
						}
					}
					matched = matched && found
				}
			}
			if matched && applicable > 0 {
				row.State, row.Reason = "matched", ""
				slices.Sort(row.Evidence)
				row.Evidence = slices.Compact(row.Evidence)
			}
			targets = append(targets, row)
		}
	}
	return targets
}

func collaborationProgramScopeIncludes(target, required collaborationPlatformScope) bool {
	return target.Platform == required.Platform && (target.Architecture == "" || required.Architecture == "" || target.Architecture == required.Architecture) && (required.Terminal == "" || target.Terminal == required.Terminal)
}

func collaborationApplyResearch(row *collaborationRequirementRow, requirementID string, dependencies []collaborationResearchDependency, attempts []collaborationValidatedAttempt, catalog collaborationCatalog) {
	var prerequisites []string
	conflict := false
	for index := range row.Targets {
		target := &row.Targets[index]
		var targetPrerequisites []string
		var supportedIDs []string
		for _, dependency := range dependencies {
			var dependencySupported, dependencyUnsupported []collaborationAttempt
			for _, selected := range attempts {
				attempt := selected.Attempt
				if !collaborationBaselineScenarioRegistered(catalog, attempt) || !collaborationResearchComplete(selected) || attempt.Finding == nil || attempt.Finding.Capability != dependency.Capability || !collaborationContains(attempt.Claims, requirementID) || !collaborationScopeMatches(target.Scope, attempt.Platform, attempt.Architecture, attempt.Terminal) || dependency.Scenario != attempt.Scenario || dependency.Revision != attempt.ScenarioRevision || !collaborationScopeMatches(dependency.Scope, attempt.Platform, attempt.Architecture, attempt.Terminal) {
					continue
				}
				if attempt.Finding.Verdict == "supported" {
					dependencySupported = append(dependencySupported, attempt)
				} else if attempt.Finding.Verdict == "unsupported" {
					dependencyUnsupported = append(dependencyUnsupported, attempt)
				}
			}
			for _, unsupported := range dependencyUnsupported {
				target.Evidence = append(target.Evidence, unsupported.ID)
				row.Evidence = append(row.Evidence, unsupported.ID)
				targetPrerequisites = append(targetPrerequisites, unsupported.Prerequisite)
				for _, supported := range dependencySupported {
					if supported.Platform == unsupported.Platform && supported.Architecture == unsupported.Architecture && supported.Terminal == unsupported.Terminal {
						supportedIDs = append(supportedIDs, supported.ID)
					}
				}
			}
		}
		if len(targetPrerequisites) == 0 {
			continue
		}
		slices.Sort(targetPrerequisites)
		targetPrerequisites = slices.Compact(targetPrerequisites)
		target.Prerequisites = targetPrerequisites
		prerequisites = append(prerequisites, targetPrerequisites...)
		if len(supportedIDs) > 0 || target.State == "matched" || target.State == "deliberately_different" {
			target.Evidence = append(target.Evidence, supportedIDs...)
			row.Evidence = append(row.Evidence, supportedIDs...)
			conflict = true
			target.State = "unverified"
			target.Reason = "observed evidence conflicts with an unsupported host finding"
		} else {
			target.State = "blocked"
			target.Reason = strings.Join(targetPrerequisites, ", ")
		}
	}
	if len(prerequisites) == 0 {
		return
	}
	slices.Sort(prerequisites)
	row.Prerequisites = slices.Compact(prerequisites)
	if conflict {
		row.State = "unverified"
		row.Reason = "observed evidence conflicts with an unsupported host finding"
		return
	}
	row.State = "blocked"
	row.Reason = strings.Join(row.Prerequisites, ", ")
}

func collaborationMissingRequirements(catalog collaborationCatalog) []string {
	byID := make(map[string]string, len(catalog.Requirements))
	for _, requirement := range catalog.Requirements {
		byID[requirement.ID] = requirement.AcceptanceScenario
	}
	missing := []string{}
	for _, group := range collaborationApplicabilityGroups {
		for _, id := range group.IDs {
			if byID[id] != group.Scenario {
				missing = append(missing, id)
			}
		}
	}
	return missing
}

func collaborationReduceTargets(requirement collaborationRequirement, scopes []collaborationPlatformScope, slots []collaborationSlotRow) []collaborationTargetRow {
	targets := make([]collaborationTargetRow, 0, len(scopes))
	for _, scope := range scopes {
		row := collaborationTargetRow{Scope: scope, State: "unverified", Reason: "no live evidence for target", Evidence: []string{}}
		var matching []collaborationSlotRow
		for _, slot := range slots {
			if slot.Channel == "live" && collaborationScopeMatches(scope, slot.Platform, slot.Architecture, slot.Terminal) {
				matching = append(matching, slot)
				row.Evidence = append(row.Evidence, slot.Evidence...)
			}
		}
		if len(matching) > 0 {
			row.State, row.Reason = collaborationRequirementState(requirement, matching)
		}
		targets = append(targets, row)
	}
	return targets
}

func collaborationScopeMatches(scope collaborationPlatformScope, platform, architecture, terminal string) bool {
	return scope.Platform == platform && (scope.Architecture == "" || scope.Architecture == architecture) && (scope.Terminal == "" || scope.Terminal == terminal)
}

func collaborationResearchComplete(attempt collaborationValidatedAttempt) bool {
	if !attempt.ResultsPassed || !collaborationArtifactsComplete("live", attempt.ArtifactKinds) {
		return false
	}
	return attempt.Attempt.Outcome == "passed" || (attempt.Attempt.Outcome == "unsupported" && attempt.Attempt.Finding != nil && attempt.Attempt.Finding.Verdict == "unsupported" && strings.TrimSpace(attempt.Attempt.Prerequisite) != "")
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
			if !collaborationOneOf(slot.Channel, "unit", "live", "performance") || !collaborationOneOf(slot.Platform, "linux", "macos") || (slot.Terminal == "iterm2" && slot.Platform != "macos") || slots[slot] {
				return fmt.Errorf("invalid or duplicate proof slot for %q", id)
			}
			slots[slot] = true
			channels[slot.Channel] = true
		}
		if !channels["unit"] || !channels["live"] || !channels["performance"] {
			return fmt.Errorf("proof rule for %q lacks unit, live, or performance coverage", id)
		}
		seenResearch := make(map[collaborationResearchDependency]bool)
		for _, dependency := range rule.Research {
			scenario, found := scenarios[dependency.Scenario]
			trusted, registered := collaborationBaselineScenarios[dependency.Scenario]
			if !found || scenario.Owner != "baseline" || !registered || trusted.Purpose != "research" || dependency.Revision != trusted.Revision || !collaborationContains(trusted.Capabilities, dependency.Capability) || !collaborationOneOf(dependency.Scope.Platform, "linux", "macos") || (dependency.Scope.Terminal == "iterm2" && dependency.Scope.Platform != "macos") || seenResearch[dependency] {
				return fmt.Errorf("invalid research scenario for %q", id)
			}
			seenResearch[dependency] = true
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
	row := collaborationSlotRow{Channel: slot.Channel, Platform: slot.Platform, Architecture: slot.Architecture, Terminal: slot.Terminal, State: "unverified", Evidence: []string{}}
	outcomes := make(map[string]bool)
	for _, selected := range attempts {
		attempt := selected.Attempt
		if attempt.Purpose != "product" || attempt.Scenario != rule.Scenario || attempt.ScenarioRevision != rule.ScenarioRevision || attempt.Channel != slot.Channel || attempt.Platform != slot.Platform || (slot.Architecture != "" && attempt.Architecture != slot.Architecture) || (slot.Terminal != "" && attempt.Terminal != slot.Terminal) || !collaborationContains(attempt.Claims, requirementID) {
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
