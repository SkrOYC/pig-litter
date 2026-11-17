package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func collaborationTestReport(t *testing.T) (collaborationCatalog, collaborationEvidenceSet, collaborationProofRegistry) {
	t.Helper()
	catalog := collaborationTestCatalog(t)
	for index := range catalog.Scenarios {
		if catalog.Scenarios[index].ID == "team-roster" || catalog.Scenarios[index].ID == "host-tool-veto" {
			catalog.Scenarios[index].Status = "implemented"
		}
	}
	for index := range catalog.Requirements {
		if catalog.Requirements[index].ID == "TEAM-001" {
			catalog.Requirements[index].ReferenceRelationship = collaborationRelationship{State: "equivalent"}
		}
	}
	rule := collaborationProofRule{Scenario: "team-roster", ScenarioRevision: 1, Applicability: "core", Slots: []collaborationProofSlot{
		{Channel: "unit", Platform: "linux"},
		{Channel: "live", Platform: "linux"},
		{Channel: "performance", Platform: "linux"},
		{Channel: "live", Platform: "macos"},
	}, Research: []collaborationResearchDependency{{Scenario: "host-tool-veto", Revision: 1, Capability: "nested-hook", Scope: collaborationPlatformScope{Platform: "linux"}}}}
	registry := collaborationProofRegistry{"TEAM-001": rule, "TEAM-003": rule}
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true, "samples": true}
	var attempts []collaborationValidatedAttempt
	for _, channel := range []string{"unit", "live", "performance"} {
		attempts = append(attempts, collaborationValidatedAttempt{
			Attempt:       collaborationAttempt{ID: channel, Purpose: "product", Scenario: "team-roster", ScenarioRevision: 1, Channel: channel, Platform: "linux", Outcome: "passed", Claims: []string{"TEAM-001", "TEAM-003"}},
			ArtifactKinds: kinds, ResultsPassed: true,
		})
	}
	attempts = append(attempts, collaborationValidatedAttempt{
		Attempt:       collaborationAttempt{ID: "live-macos", Purpose: "product", Scenario: "team-roster", ScenarioRevision: 1, Channel: "live", Platform: "macos", Outcome: "passed", Claims: []string{"TEAM-001", "TEAM-003"}},
		ArtifactKinds: kinds, ResultsPassed: true,
	})
	return catalog, collaborationEvidenceSet{Attempts: attempts}, registry
}

func collaborationReportForTest(t *testing.T, catalog collaborationCatalog, evidence collaborationEvidenceSet, registry collaborationProofRegistry) (collaborationReport, error) {
	t.Helper()
	data, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	evidence.catalogData = data
	evidence.Target = collaborationTarget{Branch: "test/report", ProductCommit: strings.Repeat("a", 40), DriverCommit: strings.Repeat("b", 40), ProductBinarySHA256: strings.Repeat("c", 64), HostBinarySHA256: strings.Repeat("d", 64), CatalogSHA256: collaborationTestDigest(data)}
	evidence.expectedTarget = evidence.Target
	for index := range evidence.Attempts {
		evidence.Attempts[index].Attempt.Target = evidence.Target
	}
	return collaborationReduceReport(evidence, registry)
}

func collaborationReportRow(t *testing.T, report collaborationReport, id string) collaborationRequirementRow {
	t.Helper()
	for _, row := range report.Requirements {
		if row.ID == id {
			return row
		}
	}
	t.Fatalf("missing requirement %s", id)
	return collaborationRequirementRow{}
}

func TestCollaborationReportSeparatesReferenceRelationship(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	if got := collaborationReportRow(t, report, "TEAM-001").State; got != "matched" {
		t.Fatalf("equivalent requirement = %q, want matched", got)
	}
	if got := collaborationReportRow(t, report, "TEAM-003").State; got != "deliberately_different" {
		t.Fatalf("named-child requirement = %q, want deliberately_different", got)
	}
	if report.Parity {
		t.Fatal("unmeasured and different requirements produced aggregate parity")
	}
}

func TestCollaborationReportRefusesIncompleteOrConflictingEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*collaborationCatalog, *collaborationEvidenceSet, collaborationProofRegistry)
		want string
	}{
		{"planned scenario", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			for i := range c.Scenarios {
				if c.Scenarios[i].ID == "team-roster" {
					c.Scenarios[i].Status = "planned"
				}
			}
		}, "unverified"},
		{"unreviewed reference", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			for i := range c.Requirements {
				if c.Requirements[i].ID == "TEAM-001" {
					c.Requirements[i].ReferenceRelationship.State = "unreviewed"
				}
			}
		}, "unverified"},
		{"missing performance", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			e.Attempts = e.Attempts[:2]
		}, "unverified"},
		{"missing live media", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			e.Attempts[1].ArtifactKinds = map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true}
		}, "unverified"},
		{"failed fixture", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			e.Attempts[0].ResultsPassed = false
		}, "unverified"},
		{"waiver", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			e.Attempts[0].Attempt.Outcome = "waived"
		}, "unverified"},
		{"conflicting runs", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			other := e.Attempts[0]
			other.Attempt.ID = "contradiction"
			other.Attempt.Outcome = "blocked"
			e.Attempts = append(e.Attempts, other)
		}, "unverified"},
		{"blocked platform", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			e.Attempts[1].Attempt.Outcome = "blocked"
		}, "blocked"},
		{"unsupported product", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			e.Attempts[1].Attempt.Outcome = "unsupported"
		}, "unsupported"},
		{"wrong revision", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			e.Attempts[0].Attempt.ScenarioRevision = 2
		}, "unverified"},
		{"required macOS evidence absent", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			e.Attempts = e.Attempts[:3]
		}, "unverified"},
		{"required macOS proof slot absent", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			rule := r["TEAM-001"]
			rule.Slots = rule.Slots[:3]
			r["TEAM-001"] = rule
		}, "unverified"},
		{"missing applicability", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			rule := r["TEAM-001"]
			rule.Applicability = ""
			r["TEAM-001"] = rule
		}, "unverified"},
		{"caller-narrowed applicability", func(c *collaborationCatalog, e *collaborationEvidenceSet, r collaborationProofRegistry) {
			rule := r["TEAM-001"]
			rule.Applicability = "linux-only"
			r["TEAM-001"] = rule
		}, "unverified"},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog, evidence, registry := collaborationTestReport(t)
			test.edit(&catalog, &evidence, registry)
			report, err := collaborationReportForTest(t, catalog, evidence, registry)
			if err != nil {
				t.Fatal(err)
			}
			if got := collaborationReportRow(t, report, "TEAM-001").State; got != test.want || report.Parity {
				t.Fatalf("state = %q, parity = %v, want %q and false", got, report.Parity, test.want)
			}
		})
	}
}

func TestCollaborationUnsupportedResearchDoesNotProveProductParity(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true}
	finding := collaborationValidatedAttempt{
		Attempt:       collaborationAttempt{ID: "host-probe", Purpose: "research", Channel: "research", Scenario: "host-tool-veto", ScenarioRevision: 1, Platform: "linux", Architecture: "amd64", Outcome: "unsupported", Finding: &collaborationCapabilityFinding{Capability: "nested-hook", Verdict: "unsupported", Detail: "nested call was not vetoed"}, Prerequisite: "an enforceable nested hook", Claims: []string{"TEAM-001"}},
		ArtifactKinds: kinds, ResultsPassed: true,
	}
	evidence.Attempts = append(evidence.Attempts[:0], finding)
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	row := collaborationReportRow(t, report, "TEAM-001")
	if row.State != "blocked" || row.Reason != "an enforceable nested hook" || report.Parity || len(report.Research) != 1 || !report.Research[0].Complete {
		t.Fatalf("unsupported research report = %#v, %#v", row, report.Research)
	}
	finding.Attempt.Outcome = "inconclusive"
	evidence.Attempts[0] = finding
	report, err = collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	if report.Research[0].Complete || collaborationReportRow(t, report, "TEAM-001").State != "unverified" {
		t.Fatalf("inconclusive research report = %#v", report)
	}
}

func TestCollaborationRejectsIncompleteProofPolicy(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	rule := registry["TEAM-001"]
	rule.Slots = rule.Slots[:2]
	registry["TEAM-001"] = rule
	if _, err := collaborationReportForTest(t, catalog, evidence, registry); err == nil || !strings.Contains(err.Error(), "performance") {
		t.Fatalf("incomplete proof policy error = %v", err)
	}
}

func TestCollaborationFullParityRequiresAllPlatformProof(t *testing.T) {
	catalog := collaborationTestCatalog(t)
	for index := range catalog.Requirements {
		if catalog.Requirements[index].ID == "TEAM-001" || catalog.Requirements[index].ID == "BACKEND-003" {
			catalog.Requirements[index].ReferenceRelationship = collaborationRelationship{State: "equivalent"}
		}
	}
	for index := range catalog.Scenarios {
		catalog.Scenarios[index].Status = "implemented"
	}
	registry := collaborationProofRegistry{
		"TEAM-001":    {Scenario: "team-roster", ScenarioRevision: 1, Applicability: "core", Slots: []collaborationProofSlot{{Channel: "unit", Platform: "linux"}, {Channel: "live", Platform: "linux"}, {Channel: "live", Platform: "macos"}, {Channel: "performance", Platform: "linux"}}},
		"BACKEND-003": {Scenario: "team-backends", ScenarioRevision: 1, Applicability: "iterm2", Slots: []collaborationProofSlot{{Channel: "unit", Platform: "macos"}, {Channel: "live", Platform: "macos", Terminal: "iterm2"}, {Channel: "performance", Platform: "macos"}}},
	}
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true, "samples": true}
	var evidence collaborationEvidenceSet
	for id, rule := range registry {
		for _, slot := range rule.Slots {
			evidence.Attempts = append(evidence.Attempts, collaborationValidatedAttempt{
				Attempt:       collaborationAttempt{ID: id + "-" + slot.Channel + "-" + slot.Platform, Purpose: "product", Scenario: rule.Scenario, ScenarioRevision: 1, Channel: slot.Channel, Platform: slot.Platform, Architecture: "amd64", Terminal: slot.Terminal, Outcome: "passed", Claims: []string{id}},
				ArtifactKinds: kinds, ResultsPassed: true,
			})
		}
	}
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil || report.Parity || len(report.MissingRequirements) != 0 || collaborationReportRow(t, report, "TEAM-001").State != "matched" || collaborationReportRow(t, report, "BACKEND-003").State != "matched" {
		t.Fatalf("incomplete synthetic evidence = %#v, %v", report, err)
	}
	for index, attempt := range evidence.Attempts {
		if attempt.Attempt.Terminal == "iterm2" {
			evidence.Attempts = append(evidence.Attempts[:index], evidence.Attempts[index+1:]...)
			break
		}
	}
	report, err = collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil || report.Parity || collaborationReportRow(t, report, "BACKEND-003").State != "unverified" {
		t.Fatalf("missing iTerm2 evidence = %#v, %v", report, err)
	}
	for index := range catalog.Requirements {
		if catalog.Requirements[index].ID == "BACKEND-003" {
			catalog.Requirements = append(catalog.Requirements[:index], catalog.Requirements[index+1:]...)
			break
		}
	}
	delete(registry, "BACKEND-003")
	report, err = collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil || report.Parity {
		t.Fatalf("missing iTerm2 requirement = %#v, %v", report, err)
	}
}

func TestCollaborationTargetMatchesRemainSeparateFromFullParity(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	evidence.Attempts = evidence.Attempts[:3]
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	row := collaborationReportRow(t, report, "TEAM-001")
	if row.State != "unverified" || report.Parity || len(row.Targets) != 2 || row.Targets[0].Scope.Platform != "linux" || row.Targets[0].State != "matched" || row.Targets[1].Scope.Platform != "macos" || row.Targets[1].State != "unverified" {
		t.Fatalf("target results = %#v, full parity = %v", row, report.Parity)
	}
}

func TestCollaborationProgramTargetsRequireEveryApplicableRequirement(t *testing.T) {
	catalog := collaborationTestCatalog(t)
	var selected []collaborationRequirement
	for _, requirement := range catalog.Requirements {
		if requirement.ID == "TEAM-001" || requirement.ID == "TEAM-002" || requirement.ID == "BACKEND-003" {
			selected = append(selected, requirement)
		}
	}
	catalog.Requirements = selected
	rows := make([]collaborationRequirementRow, 0, len(selected))
	for _, requirement := range selected {
		_, scopes := collaborationApplicability(requirement)
		row := collaborationRequirementRow{ID: requirement.ID}
		for _, scope := range scopes {
			state := "matched"
			if requirement.ID == "TEAM-002" && scope.Platform == "linux" {
				state = "unverified"
			}
			row.Targets = append(row.Targets, collaborationTargetRow{Scope: scope, State: state, Evidence: []string{requirement.ID}})
		}
		rows = append(rows, row)
	}
	targets := collaborationReduceProgramTargets(catalog, rows, nil)
	if len(targets) != 3 || targets[0].State != "unverified" || targets[1].State != "matched" || targets[2].State != "matched" {
		t.Fatalf("partial program targets = %#v", targets)
	}
	for index := range rows {
		for target := range rows[index].Targets {
			rows[index].Targets[target].State = "matched"
		}
	}
	targets = collaborationReduceProgramTargets(catalog, rows, nil)
	for _, target := range targets {
		if target.State != "matched" {
			t.Fatalf("complete program targets = %#v", targets)
		}
	}
	if got := collaborationReduceProgramTargets(catalog, rows, []string{"TEAM-013"}); got[0].State != "unverified" || got[1].State != "unverified" || got[2].State != "unverified" {
		t.Fatalf("missing requirement reduced the required scope: %#v", got)
	}
	for _, requirement := range collaborationTestCatalog(t).Requirements {
		if requirement.ID == "TASK-011" {
			catalog.Requirements = append(catalog.Requirements, requirement)
			rows = append(rows, collaborationRequirementRow{ID: requirement.ID})
			break
		}
	}
	if got := collaborationReduceProgramTargets(catalog, rows, nil); got[0].State != "unverified" || got[1].State != "unverified" || got[2].State != "unverified" {
		t.Fatalf("unreviewed applicability reduced the required scope: %#v", got)
	}
}

func TestCollaborationResearchIsRegisteredIndependentlyOfProductProof(t *testing.T) {
	catalog, evidence, _ := collaborationTestReport(t)
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true}
	finding := collaborationValidatedAttempt{
		Attempt:       collaborationAttempt{ID: "host-observation", Purpose: "research", Channel: "research", Scenario: "host-tool-veto", ScenarioRevision: 1, Platform: "linux", Architecture: "amd64", Observation: "the host lacks this capability", Outcome: "passed", Finding: &collaborationCapabilityFinding{Capability: "nested-hook", Verdict: "unsupported", Detail: "nested call was not vetoed"}, Prerequisite: "a host capability", Claims: []string{}},
		ArtifactKinds: kinds, ResultsPassed: true,
	}
	evidence.Attempts = []collaborationValidatedAttempt{finding}
	report, err := collaborationReportForTest(t, catalog, evidence, collaborationProofRegistry{})
	if err != nil || len(report.Research) != 1 || !report.Research[0].Complete || report.Research[0].Observation != "the host lacks this capability" || report.Research[0].Finding.Verdict != "unsupported" || report.Research[0].Prerequisite != "a host capability" || report.Parity || collaborationReportRow(t, report, "TEAM-001").State != "unverified" {
		t.Fatalf("independent research result = %#v, %v", report, err)
	}
	for _, edit := range []func(*collaborationAttempt){
		func(attempt *collaborationAttempt) { attempt.ScenarioRevision = 2 },
		func(attempt *collaborationAttempt) { attempt.Channel = "live" },
	} {
		changed := finding
		edit(&changed.Attempt)
		evidence.Attempts[0] = changed
		report, err = collaborationReportForTest(t, catalog, evidence, collaborationProofRegistry{})
		if err != nil || report.Research[0].Complete || report.Parity {
			t.Fatalf("unregistered research result = %#v, %v", report.Research, err)
		}
	}
}

func TestCollaborationResearchConflictsUseExplicitScopedFindings(t *testing.T) {
	catalog, evidence, _ := collaborationTestReport(t)
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true}
	supported := collaborationAttempt{ID: "supported", Purpose: "research", Channel: "research", Scenario: "host-tool-veto", ScenarioRevision: 1, Platform: "linux", Architecture: "amd64", Outcome: "passed", Finding: &collaborationCapabilityFinding{Capability: "nested-hook", Verdict: "supported", Detail: "first observation"}}
	unsupported := supported
	unsupported.ID = "unsupported"
	unsupported.Finding = &collaborationCapabilityFinding{Capability: "nested-hook", Verdict: "unsupported", Detail: "second observation"}
	unsupported.Prerequisite = "an enforceable nested hook"
	evidence.Attempts = []collaborationValidatedAttempt{{Attempt: supported, ArtifactKinds: kinds, ResultsPassed: true}, {Attempt: unsupported, ArtifactKinds: kinds, ResultsPassed: true}}
	report, err := collaborationReportForTest(t, catalog, evidence, collaborationProofRegistry{})
	if err != nil || len(report.Research) != 2 || !report.Research[0].Complete || !report.Research[1].Complete || !report.Research[0].Conflict || !report.Research[1].Conflict || report.Parity {
		t.Fatalf("contradictory host findings = %#v, %v", report.Research, err)
	}
	evidence.Attempts[1].Attempt.Architecture = "arm64"
	report, err = collaborationReportForTest(t, catalog, evidence, collaborationProofRegistry{})
	if err != nil || report.Research[0].Conflict || report.Research[1].Conflict {
		t.Fatalf("different architectures were conflated: %#v, %v", report.Research, err)
	}
	evidence.Attempts[1].Attempt.Architecture = "amd64"
	evidence.Attempts[1].Attempt.Finding = &collaborationCapabilityFinding{Capability: "direct-hook", Verdict: "unsupported", Detail: "different capability"}
	report, err = collaborationReportForTest(t, catalog, evidence, collaborationProofRegistry{})
	if err != nil || report.Research[0].Conflict || report.Research[1].Conflict {
		t.Fatalf("different capabilities were conflated: %#v, %v", report.Research, err)
	}
	evidence.Attempts[1].Attempt.Finding = unsupported.Finding
	evidence.Attempts[0].Attempt.Finding = nil
	report, err = collaborationReportForTest(t, catalog, evidence, collaborationProofRegistry{})
	if err != nil || report.Research[0].Conflict || report.Research[1].Conflict {
		t.Fatalf("scenario outcome was treated as a capability finding: %#v, %v", report.Research, err)
	}
}

func TestCollaborationP02ScenarioTableCoversCatalog(t *testing.T) {
	catalog := collaborationTestCatalog(t)
	seen := make(map[string]bool)
	for _, scenario := range catalog.Scenarios {
		if scenario.Owner != "P02" {
			continue
		}
		rule, ok := collaborationP02Scenarios[scenario.ID]
		if !ok || rule.Revision != 1 || !collaborationOneOf(rule.Purpose, "research", "verification") || !collaborationOneOf(rule.Channel, "research", "live", "performance") || (rule.Purpose == "research") != (rule.Channel == "research") || (rule.Purpose == "research") != (len(rule.Capabilities) > 0) {
			t.Fatalf("P02 scenario has no trusted rule: %#v", scenario)
		}
		seen[scenario.ID] = true
	}
	if len(seen) != len(collaborationP02Scenarios) {
		t.Fatalf("trusted P02 rules do not match catalog: %d vs %d", len(seen), len(collaborationP02Scenarios))
	}
}

func TestCollaborationArchitectureConstraintsAreExplicit(t *testing.T) {
	requirement := collaborationRequirement{ReferenceRelationship: collaborationRelationship{State: "equivalent"}}
	scopes := []collaborationPlatformScope{{Platform: "linux", Architecture: "amd64"}, {Platform: "linux", Architecture: "arm64"}}
	slots := []collaborationSlotRow{{Channel: "live", Platform: "linux", Architecture: "amd64", State: "matched", Evidence: []string{"linux-amd64"}}}
	targets := collaborationReduceTargets(requirement, scopes, slots)
	if len(targets) != 2 || targets[0].State != "matched" || targets[1].State != "unverified" {
		t.Fatalf("architecture targets = %#v", targets)
	}
	if collaborationScopeMatches(scopes[1], "linux", "", "") {
		t.Fatal("an unspecified architecture satisfied an arm64 obligation")
	}
	rule := collaborationProofRule{Scenario: "team-roster", ScenarioRevision: 1}
	slot := collaborationProofSlot{Channel: "live", Platform: "linux", Architecture: "arm64"}
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true}
	attempt := collaborationValidatedAttempt{Attempt: collaborationAttempt{ID: "x86", Purpose: "product", Scenario: "team-roster", ScenarioRevision: 1, Channel: "live", Platform: "linux", Architecture: "amd64", Outcome: "passed", Claims: []string{"TEAM-001"}}, ArtifactKinds: kinds, ResultsPassed: true}
	if row := collaborationReduceSlot("TEAM-001", rule, slot, []collaborationValidatedAttempt{attempt}); row.State != "unverified" {
		t.Fatalf("amd64 evidence satisfied arm64 slot: %#v", row)
	}
	attempt.Attempt.Architecture = "arm64"
	if row := collaborationReduceSlot("TEAM-001", rule, slot, []collaborationValidatedAttempt{attempt}); row.State != "matched" {
		t.Fatalf("arm64 evidence did not satisfy arm64 slot: %#v", row)
	}
}

func TestCollaborationITerm2RequiresMacOSTerminalEvidence(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	var backend collaborationRequirement
	for _, requirement := range catalog.Requirements {
		if requirement.ID == "BACKEND-003" {
			backend = requirement
			break
		}
	}
	backend.ReferenceRelationship = collaborationRelationship{State: "equivalent"}
	for index := range catalog.Requirements {
		if catalog.Requirements[index].ID == backend.ID {
			catalog.Requirements[index] = backend
		}
	}
	for index := range catalog.Scenarios {
		if catalog.Scenarios[index].ID == "team-backends" {
			catalog.Scenarios[index].Status = "implemented"
		}
	}
	rule := collaborationProofRule{Scenario: "team-backends", ScenarioRevision: 1, Applicability: "iterm2", Slots: []collaborationProofSlot{{Channel: "unit", Platform: "macos"}, {Channel: "live", Platform: "macos"}, {Channel: "performance", Platform: "macos"}}}
	registry[backend.ID] = rule
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true, "samples": true}
	for _, slot := range rule.Slots {
		evidence.Attempts = append(evidence.Attempts, collaborationValidatedAttempt{Attempt: collaborationAttempt{ID: "backend-" + slot.Channel, Purpose: "product", Scenario: rule.Scenario, ScenarioRevision: 1, Channel: slot.Channel, Platform: "macos", Architecture: "arm64", Outcome: "passed", Claims: []string{backend.ID}}, ArtifactKinds: kinds, ResultsPassed: true})
	}
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	row := collaborationReportRow(t, report, backend.ID)
	if row.State != "unverified" || len(row.Targets) != 1 || row.Targets[0].Scope.Platform != "macos" || row.Targets[0].Scope.Terminal != "iterm2" || row.Targets[0].State != "unverified" {
		t.Fatalf("iTerm2 target without terminal evidence = %#v", row)
	}
}

func TestCollaborationApplicabilityCoversOnlyReviewedRequirements(t *testing.T) {
	catalog := collaborationTestCatalog(t)
	seen := make(map[string]bool)
	for _, group := range collaborationApplicabilityGroups {
		for _, id := range group.IDs {
			if seen[id] {
				t.Fatalf("duplicate applicability for %s", id)
			}
			seen[id] = true
		}
	}
	for _, requirement := range catalog.Requirements {
		profile, scopes := collaborationApplicability(requirement)
		if profile == "" || (profile != "unreviewed" && len(scopes) == 0) {
			t.Fatalf("requirement %s has no reviewed or explicit unreviewed profile", requirement.ID)
		}
		delete(seen, requirement.ID)
	}
	if len(seen) != 0 {
		t.Fatalf("applicability refers to missing requirements: %v", seen)
	}
	unknown := collaborationRequirement{ID: "NEW-001", AcceptanceScenario: "team-roster"}
	if profile, scopes := collaborationApplicability(unknown); profile != "" || len(scopes) != 0 {
		t.Fatalf("new requirement silently inherited %q: %#v", profile, scopes)
	}
}

func TestCollaborationMissingProfileCannotClaimTargetMatches(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	rule := registry["TEAM-001"]
	rule.Applicability = "linux-only"
	registry["TEAM-001"] = rule
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	row := collaborationReportRow(t, report, "TEAM-001")
	if row.State != "unverified" || len(row.Targets) != 2 || row.Targets[0].State != "unverified" || row.Targets[1].State != "unverified" || report.Parity {
		t.Fatalf("narrowed profile produced target matches: %#v", row)
	}
}

func TestCollaborationReportIsDeterministic(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	var first string
	for index := 0; index < 20; index++ {
		report, err := collaborationReportForTest(t, catalog, evidence, registry)
		if err != nil {
			t.Fatal(err)
		}
		data, err := json.Marshal(report)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			first = string(data)
		} else if string(data) != first {
			t.Fatal("the same evidence selection produced a different report")
		}
	}
}

func TestCollaborationMissingCatalogRequirementBlocksFullParity(t *testing.T) {
	catalog := collaborationTestCatalog(t)
	catalog.Requirements = catalog.Requirements[1:]
	report, err := collaborationReportForTest(t, catalog, collaborationEvidenceSet{}, nil)
	if err != nil || report.Parity || !collaborationContains(report.MissingRequirements, "TEAM-001") {
		t.Fatalf("missing catalog requirement = %#v, %v", report, err)
	}
}

func TestCollaborationReportRetainsVerificationFailures(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	failure := collaborationValidatedAttempt{
		Attempt:       collaborationAttempt{ID: "fixture-interrupted", Purpose: "verification", Scenario: "fixture-interrupt", Channel: "live", Platform: "linux", Outcome: "inconclusive", Reason: "interrupted before cleanup"},
		ArtifactKinds: map[string]bool{"log": true},
	}
	evidence.Attempts = append(evidence.Attempts, failure)
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	if report.Parity || len(report.Verification) != 1 || report.Verification[0].ID != "fixture-interrupted" || report.Verification[0].Complete || report.Verification[0].Reason != "interrupted before cleanup" {
		t.Fatalf("interrupted verification result = %#v", report.Verification)
	}
	failure.Attempt.ID = "failed-fixture"
	failure.Attempt.Outcome = "passed"
	failure.Attempt.Reason = ""
	failure.ArtifactKinds = map[string]bool{"log": true, "result": true}
	failure.ResultsPassed = false
	evidence.Attempts[len(evidence.Attempts)-1] = failure
	report, err = collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil || report.Parity || report.Verification[0].Complete || report.Verification[0].Reason != "fixture result or required artifacts are incomplete" {
		t.Fatalf("failed fixture result = %#v, %v", report.Verification, err)
	}
}

func TestCollaborationResearchDependencyMatchesRevisionAndScope(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*collaborationAttempt, *collaborationProofRule)
	}{
		{"different revision", func(attempt *collaborationAttempt, rule *collaborationProofRule) { attempt.ScenarioRevision = 2 }},
		{"different platform", func(attempt *collaborationAttempt, rule *collaborationProofRule) { attempt.Platform = "macos" }},
		{"different architecture", func(attempt *collaborationAttempt, rule *collaborationProofRule) {
			rule.Research[0].Scope.Architecture = "arm64"
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog, evidence, registry := collaborationTestReport(t)
			finding := collaborationValidatedAttempt{
				Attempt:       collaborationAttempt{ID: "unrelated", Purpose: "research", Channel: "research", Scenario: "host-tool-veto", ScenarioRevision: 1, Platform: "linux", Architecture: "amd64", Outcome: "unsupported", Finding: &collaborationCapabilityFinding{Capability: "nested-hook", Verdict: "unsupported", Detail: "fixture finding"}, Prerequisite: "different scope", Claims: []string{"TEAM-001"}},
				ArtifactKinds: map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true}, ResultsPassed: true,
			}
			rule := registry["TEAM-001"]
			test.edit(&finding.Attempt, &rule)
			registry["TEAM-001"] = rule
			evidence.Attempts = append(evidence.Attempts, finding)
			report, err := collaborationReportForTest(t, catalog, evidence, registry)
			if err != nil {
				t.Fatal(err)
			}
			if row := collaborationReportRow(t, report, "TEAM-001"); row.State != "matched" {
				t.Fatalf("unrelated research changed the product result: %#v", row)
			}
		})
	}
}

func TestCollaborationRepeatedUnsupportedFindingsPreserveConflict(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true}
	for index, prerequisite := range []string{"first prerequisite", "second prerequisite"} {
		evidence.Attempts = append(evidence.Attempts, collaborationValidatedAttempt{
			Attempt:       collaborationAttempt{ID: fmt.Sprintf("unsupported-%d", index), Purpose: "research", Channel: "research", Scenario: "host-tool-veto", ScenarioRevision: 1, Platform: "linux", Architecture: "amd64", Outcome: "unsupported", Finding: &collaborationCapabilityFinding{Capability: "nested-hook", Verdict: "unsupported", Detail: "fixture finding"}, Prerequisite: prerequisite, Claims: []string{"TEAM-001"}},
			ArtifactKinds: kinds, ResultsPassed: true,
		})
	}
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	row := collaborationReportRow(t, report, "TEAM-001")
	if row.State != "unverified" || !strings.Contains(row.Reason, "conflicts") || len(row.Prerequisites) != 2 || row.Targets[0].State != "unverified" || row.Targets[1].State != "matched" {
		t.Fatalf("repeated unsupported findings = %#v", row)
	}
}

func TestCollaborationPlannedResearchCannotComplete(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	for index := range catalog.Scenarios {
		if catalog.Scenarios[index].ID == "host-tool-veto" {
			catalog.Scenarios[index].Status = "planned"
		}
	}
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true}
	evidence.Attempts = append(evidence.Attempts, collaborationValidatedAttempt{
		Attempt:       collaborationAttempt{ID: "unimplemented-research", Purpose: "research", Channel: "research", Scenario: "host-tool-veto", ScenarioRevision: 1, Platform: "linux", Architecture: "amd64", Outcome: "passed", Claims: []string{"TEAM-001"}},
		ArtifactKinds: kinds, ResultsPassed: true,
	})
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil || report.Parity || len(report.Research) != 1 || report.Research[0].Complete {
		t.Fatalf("planned research result = %#v, %v", report.Research, err)
	}
}

func TestCollaborationDifferentResearchScenariosDoNotConflict(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	for index := range catalog.Scenarios {
		if catalog.Scenarios[index].ID == "host-busy-message" {
			catalog.Scenarios[index].Status = "implemented"
		}
	}
	rule := registry["TEAM-001"]
	rule.Research = append(rule.Research, collaborationResearchDependency{Scenario: "host-busy-message", Revision: 1, Capability: "peer-delivery", Scope: collaborationPlatformScope{Platform: "linux"}})
	registry["TEAM-001"] = rule
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true}
	evidence.Attempts = []collaborationValidatedAttempt{
		{Attempt: collaborationAttempt{ID: "delivery-supported", Purpose: "research", Channel: "research", Scenario: "host-busy-message", ScenarioRevision: 1, Platform: "linux", Architecture: "amd64", Outcome: "passed", Finding: &collaborationCapabilityFinding{Capability: "peer-delivery", Verdict: "supported", Detail: "fixture finding"}, Claims: []string{"TEAM-001"}}, ArtifactKinds: kinds, ResultsPassed: true},
		{Attempt: collaborationAttempt{ID: "veto-unsupported", Purpose: "research", Channel: "research", Scenario: "host-tool-veto", ScenarioRevision: 1, Platform: "linux", Architecture: "amd64", Outcome: "unsupported", Finding: &collaborationCapabilityFinding{Capability: "nested-hook", Verdict: "unsupported", Detail: "fixture finding"}, Prerequisite: "enforceable hook", Claims: []string{"TEAM-001"}}, ArtifactKinds: kinds, ResultsPassed: true},
	}
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	if row := collaborationReportRow(t, report, "TEAM-001"); row.State != "blocked" || row.Reason != "enforceable hook" {
		t.Fatalf("different research scenarios were treated as conflicting: %#v", row)
	}
}

func TestCollaborationResearchConflictRequiresSameArchitecture(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true}
	evidence.Attempts = []collaborationValidatedAttempt{
		{Attempt: collaborationAttempt{ID: "amd64-supported", Purpose: "research", Channel: "research", Scenario: "host-tool-veto", ScenarioRevision: 1, Platform: "linux", Architecture: "amd64", Outcome: "passed", Finding: &collaborationCapabilityFinding{Capability: "nested-hook", Verdict: "supported", Detail: "fixture finding"}, Claims: []string{"TEAM-001"}}, ArtifactKinds: kinds, ResultsPassed: true},
		{Attempt: collaborationAttempt{ID: "arm64-unsupported", Purpose: "research", Channel: "research", Scenario: "host-tool-veto", ScenarioRevision: 1, Platform: "linux", Architecture: "arm64", Outcome: "unsupported", Finding: &collaborationCapabilityFinding{Capability: "nested-hook", Verdict: "unsupported", Detail: "fixture finding"}, Prerequisite: "arm64 hook", Claims: []string{"TEAM-001"}}, ArtifactKinds: kinds, ResultsPassed: true},
	}
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	if row := collaborationReportRow(t, report, "TEAM-001"); row.State != "blocked" || row.Reason != "arm64 hook" {
		t.Fatalf("different architectures were treated as conflicting: %#v", row)
	}
}

func TestCollaborationPlannedVerificationCannotComplete(t *testing.T) {
	catalog, evidence, registry := collaborationTestReport(t)
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true}
	evidence.Attempts = append(evidence.Attempts, collaborationValidatedAttempt{
		Attempt:       collaborationAttempt{ID: "planned-verification", Purpose: "verification", Scenario: "fixture-interrupt", ScenarioRevision: 1, Channel: "live", Platform: "linux", Outcome: "passed"},
		ArtifactKinds: kinds, ResultsPassed: true,
	})
	report, err := collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil || len(report.Verification) != 1 || report.Verification[0].Complete || report.Parity {
		t.Fatalf("planned verification = %#v, %v", report.Verification, err)
	}
	for index := range catalog.Scenarios {
		if catalog.Scenarios[index].ID == "fixture-interrupt" {
			catalog.Scenarios[index].Status = "implemented"
		}
	}
	report, err = collaborationReportForTest(t, catalog, evidence, registry)
	if err != nil || !report.Verification[0].Complete || report.Parity {
		t.Fatalf("implemented verification = %#v, %v", report.Verification, err)
	}
}
