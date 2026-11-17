package main

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

type collaborationCatalog struct {
	SchemaVersion    int                        `json:"schema_version"`
	ReferenceVersion string                     `json:"reference_version"`
	Description      string                     `json:"description"`
	EvidenceStates   []string                   `json:"evidence_states"`
	SourceKinds      []string                   `json:"source_kinds"`
	Requirements     []collaborationRequirement `json:"requirements"`
	Scenarios        []collaborationScenario    `json:"scenarios"`
}

type collaborationRequirement struct {
	ID                    string                    `json:"id"`
	Title                 string                    `json:"title"`
	Reference             collaborationReference    `json:"reference"`
	ProposedBehavior      string                    `json:"proposed_behavior"`
	ReferenceRelationship collaborationRelationship `json:"reference_relationship"`
	Owner                 string                    `json:"owner"`
	AcceptanceScenario    string                    `json:"acceptance_scenario"`
	AcceptanceAssertion   string                    `json:"acceptance_assertion"`
	ProductEvidence       collaborationCatalogState `json:"product_evidence"`
	Frame                 *collaborationFrame       `json:"frame,omitempty"`
}

type collaborationReference struct {
	Kind        string `json:"kind"`
	Path        string `json:"path"`
	Section     string `json:"section"`
	Confidence  string `json:"confidence"`
	Observation string `json:"observation"`
}

type collaborationRelationship struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

type collaborationCatalogState struct {
	State   string            `json:"state"`
	Records []json.RawMessage `json:"records"`
}

type collaborationFrame struct {
	Variant string `json:"variant"`
	Role    string `json:"role"`
}

type collaborationScenario struct {
	ID       string `json:"id"`
	Owner    string `json:"owner"`
	Status   string `json:"status"`
	Expected string `json:"expected"`
}

var (
	collaborationRequirementID = regexp.MustCompile(`^[A-Z][A-Z0-9]*-[0-9]{3}$`)
	collaborationScenarioID    = regexp.MustCompile(`^[a-z][a-z0-9]*(?:-[a-z0-9]+)*$`)
	collaborationOwner         = regexp.MustCompile(`^P(?:0[2-9]|10)$`)
)

func collaborationReadCatalog(data []byte) (collaborationCatalog, error) {
	var catalog collaborationCatalog
	if err := collaborationDecodeJSON(data, &catalog); err != nil {
		return catalog, fmt.Errorf("decode collaboration catalog: %w", err)
	}
	if err := catalog.validate(); err != nil {
		return collaborationCatalog{}, err
	}
	return catalog, nil
}

func (catalog collaborationCatalog) validate() error {
	if catalog.SchemaVersion != 1 || catalog.ReferenceVersion != "Claude Code 2.1.322" || strings.TrimSpace(catalog.Description) == "" {
		return fmt.Errorf("invalid catalog version, reference, or description")
	}
	if err := collaborationSet(catalog.EvidenceStates, "matched", "deliberately_different", "blocked", "unsupported", "unverified"); err != nil {
		return fmt.Errorf("invalid evidence states: %w", err)
	}
	if err := collaborationSet(catalog.SourceKinds, "static_implementation", "captured_runtime", "reconstructed_screen", "proposal"); err != nil {
		return fmt.Errorf("invalid source kinds: %w", err)
	}
	if len(catalog.Requirements) == 0 || len(catalog.Scenarios) == 0 {
		return fmt.Errorf("catalog requires requirements and scenarios")
	}
	scenarios := make(map[string]collaborationScenario, len(catalog.Scenarios))
	for _, scenario := range catalog.Scenarios {
		if !collaborationScenarioID.MatchString(scenario.ID) || !collaborationOwner.MatchString(scenario.Owner) || scenario.Status != "planned" || strings.TrimSpace(scenario.Expected) == "" {
			return fmt.Errorf("invalid scenario %q", scenario.ID)
		}
		if _, exists := scenarios[scenario.ID]; exists {
			return fmt.Errorf("duplicate scenario %q", scenario.ID)
		}
		scenarios[scenario.ID] = scenario
	}
	frames := make(map[string]bool)
	ids := make(map[string]bool, len(catalog.Requirements))
	for _, requirement := range catalog.Requirements {
		if !collaborationRequirementID.MatchString(requirement.ID) || ids[requirement.ID] {
			return fmt.Errorf("invalid or duplicate requirement %q", requirement.ID)
		}
		ids[requirement.ID] = true
		if err := requirement.validate(scenarios); err != nil {
			return fmt.Errorf("requirement %s: %w", requirement.ID, err)
		}
		if requirement.Frame != nil {
			if frames[requirement.Frame.Variant] {
				return fmt.Errorf("duplicate frame variant %q", requirement.Frame.Variant)
			}
			frames[requirement.Frame.Variant] = true
		}
	}
	wantFrames := []string{
		"plan_approval_request", "plan_approval_response", "shutdown_request", "shutdown_response",
		"shutdown_approved", "shutdown_rejected", "task_assignment", "task_completed",
		"teammate_terminated", "idle_notification", "permission_request", "permission_response",
		"sandbox_permission_request", "sandbox_permission_response", "team_permission_update", "mode_set_request",
	}
	if len(frames) != len(wantFrames) {
		return fmt.Errorf("frame census has %d variants, want %d", len(frames), len(wantFrames))
	}
	for _, name := range wantFrames {
		if !frames[name] {
			return fmt.Errorf("missing frame variant %q", name)
		}
	}
	return nil
}

func (requirement collaborationRequirement) validate(scenarios map[string]collaborationScenario) error {
	ref := requirement.Reference
	if !collaborationOneOf(ref.Kind, "static_implementation", "captured_runtime", "reconstructed_screen", "proposal") ||
		!collaborationOneOf(ref.Confidence, "high", "medium", "low") ||
		!collaborationNonempty(ref.Path, ref.Section, ref.Observation, requirement.Title, requirement.ProposedBehavior, requirement.AcceptanceAssertion) {
		return fmt.Errorf("incomplete reference or behavior")
	}
	relation := requirement.ReferenceRelationship
	if !collaborationOneOf(relation.State, "unreviewed", "equivalent", "deliberately_different") ||
		(relation.State == "deliberately_different" && strings.TrimSpace(relation.Reason) == "") {
		return fmt.Errorf("invalid reference relationship")
	}
	if requirement.ProductEvidence.State != "unverified" || requirement.ProductEvidence.Records == nil || len(requirement.ProductEvidence.Records) != 0 {
		return fmt.Errorf("catalog cannot assert runtime evidence")
	}
	scenario, exists := scenarios[requirement.AcceptanceScenario]
	if !exists || scenario.Owner != requirement.Owner {
		return fmt.Errorf("acceptance scenario is missing or has a different owner")
	}
	if strings.HasPrefix(requirement.ID, "FRAME-") != (requirement.Frame != nil) {
		return fmt.Errorf("frame requirement and frame metadata disagree")
	}
	if requirement.Frame != nil && (requirement.Frame.Variant != requirement.Title || strings.TrimSpace(requirement.Frame.Role) == "") {
		return fmt.Errorf("invalid frame metadata")
	}
	return nil
}

func collaborationSet(values []string, expected ...string) error {
	if len(values) != len(expected) {
		return fmt.Errorf("wrong number of values")
	}
	remaining := make(map[string]bool, len(expected))
	for _, value := range expected {
		remaining[value] = true
	}
	for _, value := range values {
		if !remaining[value] {
			return fmt.Errorf("unknown or duplicate value %q", value)
		}
		delete(remaining, value)
	}
	return nil
}

func collaborationOneOf(value string, options ...string) bool {
	for _, option := range options {
		if value == option {
			return true
		}
	}
	return false
}

func collaborationNonempty(values ...string) bool {
	for _, value := range values {
		if strings.TrimSpace(value) == "" {
			return false
		}
	}
	return true
}
