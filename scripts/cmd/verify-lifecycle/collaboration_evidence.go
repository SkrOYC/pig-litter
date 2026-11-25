package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

type collaborationTarget struct {
	Branch              string `json:"branch"`
	ProductCommit       string `json:"product_commit"`
	DriverCommit        string `json:"driver_commit"`
	ProductBinarySHA256 string `json:"product_binary_sha256"`
	HostBinarySHA256    string `json:"host_binary_sha256"`
	CatalogSHA256       string `json:"catalog_sha256"`
}

type collaborationSelection struct {
	Version  int                           `json:"version"`
	Target   collaborationTarget           `json:"target"`
	Attempts []collaborationSelectedRecord `json:"attempts"`
}

type collaborationSelectedRecord struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

type collaborationAttempt struct {
	Version          int                             `json:"version"`
	ID               string                          `json:"id"`
	Scenario         string                          `json:"scenario"`
	ScenarioRevision int                             `json:"scenario_revision"`
	Target           collaborationTarget             `json:"target"`
	Purpose          string                          `json:"purpose"`
	Channel          string                          `json:"channel"`
	Platform         string                          `json:"platform"`
	Architecture     string                          `json:"architecture"`
	Terminal         string                          `json:"terminal,omitempty"`
	Host             string                          `json:"host"`
	Command          []string                        `json:"command"`
	Observation      string                          `json:"observation"`
	Outcome          string                          `json:"outcome"`
	Finding          *collaborationCapabilityFinding `json:"finding,omitempty"`
	Reason           string                          `json:"reason,omitempty"`
	Prerequisite     string                          `json:"prerequisite,omitempty"`
	Events           []collaborationEvent            `json:"events"`
	Claims           []string                        `json:"claims"`
	Artifacts        []collaborationArtifact         `json:"artifacts"`
}

type collaborationCapabilityFinding struct {
	Capability string `json:"capability"`
	Verdict    string `json:"verdict"`
	Detail     string `json:"detail"`
}

type collaborationEvent struct {
	Name     string `json:"name"`
	OffsetNS int64  `json:"offset_ns"`
}

type collaborationValidatedAttempt struct {
	Attempt       collaborationAttempt
	ArtifactKinds map[string]bool
	ResultsPassed bool
}

type collaborationEvidenceSet struct {
	Target          collaborationTarget
	SelectionSHA256 string
	Attempts        []collaborationValidatedAttempt
	catalogData     []byte
	expectedTarget  collaborationTarget
}

var (
	collaborationAttemptID = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,127}$`)
	collaborationCommit    = regexp.MustCompile(`^[a-f0-9]{40}$`)
)

func collaborationSelectEvidence(ctx context.Context, evidenceDirectory string, selectionData, catalogData []byte, expected collaborationTarget, media collaborationMediaTools) (collaborationCatalog, collaborationEvidenceSet, error) {
	catalog, err := collaborationReadCatalog(catalogData)
	if err != nil {
		return catalog, collaborationEvidenceSet{}, err
	}
	if err := expected.validate(); err != nil {
		return catalog, collaborationEvidenceSet{}, fmt.Errorf("target: %w", err)
	}
	catalogHash := sha256.Sum256(catalogData)
	if hex.EncodeToString(catalogHash[:]) != expected.CatalogSHA256 {
		return catalog, collaborationEvidenceSet{}, fmt.Errorf("target catalog digest does not match catalog")
	}
	var selection collaborationSelection
	if err := collaborationDecodeJSON(selectionData, &selection); err != nil {
		return catalog, collaborationEvidenceSet{}, fmt.Errorf("decode evidence selection: %w", err)
	}
	if selection.Version != 1 || selection.Target != expected || selection.Attempts == nil || len(selection.Attempts) > 1000 {
		return catalog, collaborationEvidenceSet{}, fmt.Errorf("invalid evidence selection or target")
	}
	root, err := os.OpenRoot(evidenceDirectory)
	if err != nil {
		return catalog, collaborationEvidenceSet{}, err
	}
	defer root.Close()
	selectionHash := sha256.Sum256(selectionData)
	result := collaborationEvidenceSet{Target: expected, SelectionSHA256: hex.EncodeToString(selectionHash[:]), catalogData: bytes.Clone(catalogData), expectedTarget: expected}
	ids := make(map[string]bool)
	paths := make(map[string]bool)
	for _, selected := range selection.Attempts {
		if paths[selected.Path] || !collaborationSHA256(selected.SHA256) || !filepath.IsLocal(selected.Path) || filepath.Clean(selected.Path) != selected.Path || filepath.Dir(selected.Path) == "." {
			return catalog, collaborationEvidenceSet{}, fmt.Errorf("invalid or duplicate selected attempt %q", selected.Path)
		}
		paths[selected.Path] = true
		attemptRoot, err := collaborationOpenAttemptDirectory(root, filepath.Dir(selected.Path))
		if err != nil {
			return catalog, collaborationEvidenceSet{}, fmt.Errorf("attempt %q: %w", selected.Path, err)
		}
		data, err := collaborationReadSelectedFile(attemptRoot, filepath.Base(selected.Path), selected.SHA256)
		if err != nil {
			attemptRoot.Close()
			return catalog, collaborationEvidenceSet{}, fmt.Errorf("attempt %q: %w", selected.Path, err)
		}
		var attempt collaborationAttempt
		if err := collaborationDecodeJSON(data, &attempt); err != nil {
			attemptRoot.Close()
			return catalog, collaborationEvidenceSet{}, fmt.Errorf("attempt %q: %w", selected.Path, err)
		}
		if err := attempt.validate(catalog, expected); err != nil {
			attemptRoot.Close()
			return catalog, collaborationEvidenceSet{}, fmt.Errorf("attempt %q: %w", selected.Path, err)
		}
		if ids[attempt.ID] {
			attemptRoot.Close()
			return catalog, collaborationEvidenceSet{}, fmt.Errorf("duplicate attempt ID %q", attempt.ID)
		}
		ids[attempt.ID] = true
		kinds, resultsPassed, artifactErr := collaborationValidateArtifacts(ctx, attemptRoot, attempt.Artifacts, media)
		closeErr := attemptRoot.Close()
		if artifactErr != nil {
			return catalog, collaborationEvidenceSet{}, fmt.Errorf("attempt %q: %w", selected.Path, artifactErr)
		}
		if closeErr != nil {
			return catalog, collaborationEvidenceSet{}, closeErr
		}
		result.Attempts = append(result.Attempts, collaborationValidatedAttempt{Attempt: attempt, ArtifactKinds: kinds, ResultsPassed: resultsPassed})
	}
	return catalog, result, nil
}

func collaborationOpenAttemptDirectory(root *os.Root, name string) (*os.Root, error) {
	if !filepath.IsLocal(name) || filepath.Clean(name) != name || name == "." {
		return nil, fmt.Errorf("invalid relative path %q", name)
	}
	parts := strings.Split(name, string(filepath.Separator))
	var final os.FileInfo
	for index := range parts {
		info, err := root.Lstat(filepath.Join(parts[:index+1]...))
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf("symbolic link or non-directory in %q", name)
		}
		final = info
	}
	attemptRoot, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, err := attemptRoot.Stat(".")
	if err != nil || !os.SameFile(final, actual) {
		attemptRoot.Close()
		return nil, fmt.Errorf("attempt directory %q changed during selection", name)
	}
	return attemptRoot, nil
}

func collaborationReadSelectedFile(root *os.Root, name, expected string) ([]byte, error) {
	file, err := collaborationOpenFile(root, name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	digest, _, err := collaborationFileDigest(file, collaborationMaxJSONBytes)
	if err != nil || digest != expected {
		return nil, fmt.Errorf("manifest digest or size is invalid")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, collaborationMaxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	actual := sha256.Sum256(data)
	if hex.EncodeToString(actual[:]) != expected {
		return nil, fmt.Errorf("manifest changed during reading")
	}
	return data, nil
}

func (target collaborationTarget) validate() error {
	if strings.TrimSpace(target.Branch) == "" || !collaborationCommit.MatchString(target.ProductCommit) || !collaborationCommit.MatchString(target.DriverCommit) || !collaborationSHA256(target.ProductBinarySHA256) || !collaborationSHA256(target.HostBinarySHA256) || !collaborationSHA256(target.CatalogSHA256) {
		return fmt.Errorf("missing or invalid branch, commit, or digest")
	}
	return nil
}

func (attempt collaborationAttempt) validate(catalog collaborationCatalog, expected collaborationTarget) error {
	if attempt.Version != 1 || !collaborationAttemptID.MatchString(attempt.ID) || attempt.Target != expected || attempt.ScenarioRevision <= 0 || !collaborationNonempty(attempt.Platform, attempt.Architecture, attempt.Host, attempt.Observation) || len(attempt.Command) == 0 || strings.TrimSpace(attempt.Command[0]) == "" {
		return fmt.Errorf("invalid attempt identity or target")
	}
	if !collaborationOneOf(attempt.Purpose, "product", "research", "verification") || !collaborationOneOf(attempt.Channel, "unit", "live", "performance", "research") || (attempt.Purpose == "research") != (attempt.Channel == "research") {
		return fmt.Errorf("invalid attempt purpose or channel")
	}
	if !collaborationOneOf(attempt.Outcome, "passed", "failed", "inconclusive", "blocked", "unsupported", "waived") || (attempt.Outcome != "passed" && strings.TrimSpace(attempt.Reason) == "") {
		return fmt.Errorf("invalid attempt outcome or reason")
	}
	if attempt.Purpose == "research" && attempt.Outcome == "unsupported" && strings.TrimSpace(attempt.Prerequisite) == "" {
		return fmt.Errorf("unsupported research requires a capability prerequisite")
	}
	if finding := attempt.Finding; finding != nil {
		rule, registered := collaborationBaselineScenarios[attempt.Scenario]
		if attempt.Purpose != "research" || !registered || !collaborationContains(rule.Capabilities, finding.Capability) || !collaborationOneOf(finding.Verdict, "supported", "unsupported") || strings.TrimSpace(finding.Detail) == "" || (finding.Verdict == "unsupported" && strings.TrimSpace(attempt.Prerequisite) == "") || (attempt.Outcome == "unsupported" && finding.Verdict != "unsupported") {
			return fmt.Errorf("invalid scoped capability finding")
		}
	}
	if attempt.Outcome == "unsupported" && len(attempt.Artifacts) == 0 {
		return fmt.Errorf("unsupported conclusion requires retained evidence")
	}
	if attempt.Events == nil || attempt.Claims == nil || attempt.Artifacts == nil || len(attempt.Claims) > len(catalog.Requirements) || len(attempt.Artifacts) > 128 {
		return fmt.Errorf("missing or oversized attempt records")
	}
	var previous int64 = -1
	for _, event := range attempt.Events {
		if strings.TrimSpace(event.Name) == "" || event.OffsetNS < 0 || event.OffsetNS < previous {
			return fmt.Errorf("invalid monotonic event sequence")
		}
		previous = event.OffsetNS
	}
	if attempt.Outcome == "passed" && len(attempt.Events) == 0 {
		return fmt.Errorf("passing attempt requires timing evidence")
	}
	var scenario *collaborationScenario
	for index := range catalog.Scenarios {
		if catalog.Scenarios[index].ID == attempt.Scenario {
			scenario = &catalog.Scenarios[index]
			break
		}
	}
	if scenario == nil || (attempt.Purpose != "product" && scenario.Owner != "baseline") {
		return fmt.Errorf("unknown or incompatible scenario %q", attempt.Scenario)
	}
	seen := make(map[string]bool)
	for _, claim := range attempt.Claims {
		if seen[claim] {
			return fmt.Errorf("duplicate requirement claim %q", claim)
		}
		seen[claim] = true
		found := false
		for _, requirement := range catalog.Requirements {
			if requirement.ID == claim {
				if attempt.Purpose == "product" && requirement.AcceptanceScenario != attempt.Scenario {
					return fmt.Errorf("requirement %q belongs to another scenario", claim)
				}
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("unknown requirement claim %q", claim)
		}
	}
	if attempt.Purpose == "product" && len(attempt.Claims) == 0 {
		return fmt.Errorf("product attempt requires requirement claims")
	}
	return nil
}
