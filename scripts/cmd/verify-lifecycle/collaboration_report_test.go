package main

import (
	"strings"
	"testing"
)

func collaborationTestReport(t *testing.T) (collaborationCatalog, collaborationEvidenceSet, collaborationProofRegistry) {
	t.Helper()
	catalog := collaborationTestCatalog(t)
	for index := range catalog.Scenarios {
		if catalog.Scenarios[index].ID == "team-roster" {
			catalog.Scenarios[index].Status = "implemented"
		}
	}
	for index := range catalog.Requirements {
		if catalog.Requirements[index].ID == "TEAM-001" {
			catalog.Requirements[index].ReferenceRelationship = collaborationRelationship{State: "equivalent"}
		}
	}
	rule := collaborationProofRule{Scenario: "team-roster", ScenarioRevision: 1, Slots: []collaborationProofSlot{
		{Channel: "unit", Platform: "linux"},
		{Channel: "live", Platform: "linux"},
		{Channel: "performance", Platform: "linux"},
		{Channel: "live", Platform: "macos"},
	}, ResearchScenarios: []string{"host-tool-veto"}}
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
	report, err := collaborationReduceReport(catalog, evidence, registry)
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
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog, evidence, registry := collaborationTestReport(t)
			test.edit(&catalog, &evidence, registry)
			report, err := collaborationReduceReport(catalog, evidence, registry)
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
		Attempt:       collaborationAttempt{ID: "host-probe", Purpose: "research", Scenario: "host-tool-veto", Outcome: "unsupported", Prerequisite: "an enforceable nested hook", Claims: []string{"TEAM-001"}},
		ArtifactKinds: kinds, ResultsPassed: true,
	}
	evidence.Attempts = append(evidence.Attempts[:0], finding)
	report, err := collaborationReduceReport(catalog, evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	row := collaborationReportRow(t, report, "TEAM-001")
	if row.State != "blocked" || row.Reason != "an enforceable nested hook" || report.Parity || len(report.Research) != 1 || !report.Research[0].Complete {
		t.Fatalf("unsupported research report = %#v, %#v", row, report.Research)
	}
	finding.Attempt.Outcome = "inconclusive"
	evidence.Attempts[0] = finding
	report, err = collaborationReduceReport(catalog, evidence, registry)
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
	if _, err := collaborationReduceReport(catalog, evidence, registry); err == nil || !strings.Contains(err.Error(), "performance") {
		t.Fatalf("incomplete proof policy error = %v", err)
	}
}

func TestCollaborationFullParityRequiresAllPlatformProof(t *testing.T) {
	catalog := collaborationTestCatalog(t)
	var requirements []collaborationRequirement
	for _, requirement := range catalog.Requirements {
		if requirement.ID == "TEAM-001" || requirement.ID == "BACKEND-003" {
			requirement.ReferenceRelationship = collaborationRelationship{State: "equivalent"}
			requirements = append(requirements, requirement)
		}
	}
	catalog.Requirements = requirements
	for index := range catalog.Scenarios {
		catalog.Scenarios[index].Status = "implemented"
	}
	registry := collaborationProofRegistry{
		"TEAM-001":    {Scenario: "team-roster", ScenarioRevision: 1, Slots: []collaborationProofSlot{{Channel: "unit", Platform: "linux"}, {Channel: "live", Platform: "linux"}, {Channel: "live", Platform: "macos"}, {Channel: "performance", Platform: "linux"}}},
		"BACKEND-003": {Scenario: "team-backends", ScenarioRevision: 1, Slots: []collaborationProofSlot{{Channel: "unit", Platform: "macos"}, {Channel: "live", Platform: "iterm2"}, {Channel: "performance", Platform: "macos"}}},
	}
	kinds := map[string]bool{"log": true, "result": true, "ansi": true, "pty": true, "timing": true, "screenshot": true, "video": true, "samples": true}
	var evidence collaborationEvidenceSet
	for id, rule := range registry {
		for _, slot := range rule.Slots {
			evidence.Attempts = append(evidence.Attempts, collaborationValidatedAttempt{
				Attempt:       collaborationAttempt{ID: id + "-" + slot.Channel + "-" + slot.Platform, Purpose: "product", Scenario: rule.Scenario, ScenarioRevision: 1, Channel: slot.Channel, Platform: slot.Platform, Outcome: "passed", Claims: []string{id}},
				ArtifactKinds: kinds, ResultsPassed: true,
			})
		}
	}
	report, err := collaborationReduceReport(catalog, evidence, registry)
	if err != nil || !report.Parity {
		t.Fatalf("complete synthetic platform evidence = %#v, %v", report, err)
	}
	for index, attempt := range evidence.Attempts {
		if attempt.Attempt.Platform == "iterm2" {
			evidence.Attempts = append(evidence.Attempts[:index], evidence.Attempts[index+1:]...)
			break
		}
	}
	report, err = collaborationReduceReport(catalog, evidence, registry)
	if err != nil || report.Parity || collaborationReportRow(t, report, "BACKEND-003").State != "unverified" {
		t.Fatalf("missing iTerm2 evidence = %#v, %v", report, err)
	}
	catalog.Requirements = requirements[:1]
	delete(registry, "BACKEND-003")
	report, err = collaborationReduceReport(catalog, evidence, registry)
	if err != nil || report.Parity {
		t.Fatalf("missing iTerm2 requirement = %#v, %v", report, err)
	}
}
