package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func collaborationTestDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func collaborationTestEvidence(t *testing.T) (string, []byte, collaborationTarget, collaborationAttempt) {
	t.Helper()
	catalogData, err := os.ReadFile("../../testdata/collaboration/requirements.json")
	if err != nil {
		t.Fatal(err)
	}
	target := collaborationTarget{
		Branch: "test/example", ProductCommit: strings.Repeat("a", 40), DriverCommit: strings.Repeat("b", 40),
		ProductBinarySHA256: strings.Repeat("c", 64), HostBinarySHA256: strings.Repeat("d", 64), CatalogSHA256: collaborationTestDigest(catalogData),
	}
	attempt := collaborationAttempt{
		Version: 1, ID: "run-1", Scenario: "team-roster", ScenarioRevision: 1, Target: target,
		Purpose: "product", Channel: "unit", Platform: "linux", Architecture: "amd64", Host: "fixture-host",
		Command: []string{"fixture"}, Observation: "fixture completed", Outcome: "passed",
		Events: []collaborationEvent{{Name: "complete", OffsetNS: 1}}, Claims: []string{"TEAM-001"}, Artifacts: []collaborationArtifact{},
	}
	return t.TempDir(), catalogData, target, attempt
}

func collaborationWriteAttempt(t *testing.T, root, directory string, attempt collaborationAttempt) collaborationSelectedRecord {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, directory), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(attempt)
	if err != nil {
		t.Fatal(err)
	}
	name := filepath.Join(directory, "manifest.json")
	if err := os.WriteFile(filepath.Join(root, name), data, 0o600); err != nil {
		t.Fatal(err)
	}
	return collaborationSelectedRecord{Path: name, SHA256: collaborationTestDigest(data)}
}

func collaborationSelectionData(t *testing.T, target collaborationTarget, selected ...collaborationSelectedRecord) []byte {
	t.Helper()
	if selected == nil {
		selected = []collaborationSelectedRecord{}
	}
	data, err := json.Marshal(collaborationSelection{Version: 1, Target: target, Attempts: selected})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestCollaborationSelectsOnlyExplicitImmutableAttempts(t *testing.T) {
	root, catalog, target, attempt := collaborationTestEvidence(t)
	first := collaborationWriteAttempt(t, root, "one", attempt)
	attempt.ID = "run-2"
	collaborationWriteAttempt(t, root, "unselected", attempt)
	selection := collaborationSelectionData(t, target, first)
	_, evidence, err := collaborationSelectEvidence(context.Background(), root, selection, catalog, target, collaborationMediaTools{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Attempts) != 1 || evidence.Attempts[0].Attempt.ID != "run-1" || evidence.SelectionSHA256 != collaborationTestDigest(selection) {
		t.Fatalf("selected evidence = %#v", evidence)
	}
	if err := os.WriteFile(filepath.Join(root, first.Path), []byte(`{"changed":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := collaborationSelectEvidence(context.Background(), root, selection, catalog, target, collaborationMediaTools{}); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("changed selected manifest error = %v", err)
	}
}

func TestCollaborationRejectsAmbiguousOrStaleEvidence(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*testing.T, string, *collaborationTarget, *collaborationAttempt, *collaborationSelectedRecord) []collaborationSelectedRecord
		want string
	}{
		{"duplicate path", func(t *testing.T, root string, target *collaborationTarget, attempt *collaborationAttempt, selected *collaborationSelectedRecord) []collaborationSelectedRecord {
			return []collaborationSelectedRecord{*selected, *selected}
		}, "duplicate selected attempt"},
		{"duplicate ID", func(t *testing.T, root string, target *collaborationTarget, attempt *collaborationAttempt, selected *collaborationSelectedRecord) []collaborationSelectedRecord {
			return []collaborationSelectedRecord{*selected, collaborationWriteAttempt(t, root, "two", *attempt)}
		}, "duplicate attempt ID"},
		{"escaping path", func(t *testing.T, root string, target *collaborationTarget, attempt *collaborationAttempt, selected *collaborationSelectedRecord) []collaborationSelectedRecord {
			selected.Path = "../outside/manifest.json"
			return []collaborationSelectedRecord{*selected}
		}, "invalid or duplicate selected attempt"},
		{"symlink path", func(t *testing.T, root string, target *collaborationTarget, attempt *collaborationAttempt, selected *collaborationSelectedRecord) []collaborationSelectedRecord {
			if err := os.Symlink(filepath.Join(root, "one"), filepath.Join(root, "alias")); err != nil {
				t.Fatal(err)
			}
			selected.Path = filepath.Join("alias", "manifest.json")
			return []collaborationSelectedRecord{*selected}
		}, "symbolic link"},
		{"wrong attempt target", func(t *testing.T, root string, target *collaborationTarget, attempt *collaborationAttempt, selected *collaborationSelectedRecord) []collaborationSelectedRecord {
			attempt.Target.ProductCommit = strings.Repeat("d", 40)
			*selected = collaborationWriteAttempt(t, root, "one", *attempt)
			return []collaborationSelectedRecord{*selected}
		}, "invalid attempt identity or target"},
		{"wrong host binary", func(t *testing.T, root string, target *collaborationTarget, attempt *collaborationAttempt, selected *collaborationSelectedRecord) []collaborationSelectedRecord {
			attempt.Target.HostBinarySHA256 = strings.Repeat("e", 64)
			*selected = collaborationWriteAttempt(t, root, "one", *attempt)
			return []collaborationSelectedRecord{*selected}
		}, "invalid attempt identity or target"},
		{"missing architecture", func(t *testing.T, root string, target *collaborationTarget, attempt *collaborationAttempt, selected *collaborationSelectedRecord) []collaborationSelectedRecord {
			attempt.Architecture = ""
			*selected = collaborationWriteAttempt(t, root, "one", *attempt)
			return []collaborationSelectedRecord{*selected}
		}, "invalid attempt identity or target"},
		{"duplicate claim", func(t *testing.T, root string, target *collaborationTarget, attempt *collaborationAttempt, selected *collaborationSelectedRecord) []collaborationSelectedRecord {
			attempt.Claims = append(attempt.Claims, "TEAM-001")
			*selected = collaborationWriteAttempt(t, root, "one", *attempt)
			return []collaborationSelectedRecord{*selected}
		}, "duplicate requirement claim"},
		{"wrong scenario", func(t *testing.T, root string, target *collaborationTarget, attempt *collaborationAttempt, selected *collaborationSelectedRecord) []collaborationSelectedRecord {
			attempt.Scenario = "team-messages"
			*selected = collaborationWriteAttempt(t, root, "one", *attempt)
			return []collaborationSelectedRecord{*selected}
		}, "belongs to another scenario"},
		{"backward timing", func(t *testing.T, root string, target *collaborationTarget, attempt *collaborationAttempt, selected *collaborationSelectedRecord) []collaborationSelectedRecord {
			attempt.Events = []collaborationEvent{{Name: "first", OffsetNS: 20}, {Name: "second", OffsetNS: 10}}
			*selected = collaborationWriteAttempt(t, root, "one", *attempt)
			return []collaborationSelectedRecord{*selected}
		}, "monotonic"},
	} {
		t.Run(test.name, func(t *testing.T) {
			root, catalog, target, attempt := collaborationTestEvidence(t)
			selected := collaborationWriteAttempt(t, root, "one", attempt)
			items := test.edit(t, root, &target, &attempt, &selected)
			selection := collaborationSelectionData(t, target, items...)
			_, _, err := collaborationSelectEvidence(context.Background(), root, selection, catalog, target, collaborationMediaTools{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("selection error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCollaborationRetainsFailedFixtureResult(t *testing.T) {
	root, catalog, target, attempt := collaborationTestEvidence(t)
	directory := filepath.Join(root, "one")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	result := []byte(`{"ok":false}`)
	if err := os.WriteFile(filepath.Join(directory, "result.json"), result, 0o600); err != nil {
		t.Fatal(err)
	}
	attempt.Artifacts = []collaborationArtifact{{Path: "result.json", Kind: "result", SHA256: collaborationTestDigest(result)}}
	selected := collaborationWriteAttempt(t, root, "one", attempt)
	_, evidence, err := collaborationSelectEvidence(context.Background(), root, collaborationSelectionData(t, target, selected), catalog, target, collaborationMediaTools{})
	if err != nil {
		t.Fatal(err)
	}
	if len(evidence.Attempts) != 1 || evidence.Attempts[0].ResultsPassed {
		t.Fatalf("failed fixture was treated as successful: %#v", evidence)
	}
}

func TestCollaborationSelectionOwnsCatalogSnapshot(t *testing.T) {
	root, catalogData, target, _ := collaborationTestEvidence(t)
	catalog, evidence, err := collaborationSelectEvidence(context.Background(), root, collaborationSelectionData(t, target), catalogData, target, collaborationMediaTools{})
	if err != nil {
		t.Fatal(err)
	}
	for index := range catalog.Requirements {
		if catalog.Requirements[index].ID == "TEAM-001" {
			catalog.Requirements[index].ReferenceRelationship = collaborationRelationship{State: "equivalent"}
		}
	}
	for index := range catalog.Scenarios {
		if catalog.Scenarios[index].ID == "team-roster" {
			catalog.Scenarios[index].Status = "implemented"
		}
	}
	catalogData[0] = 'X'
	_, synthetic, registry := collaborationTestReport(t)
	evidence.Attempts = synthetic.Attempts
	for index := range evidence.Attempts {
		evidence.Attempts[index].Attempt.Target = target
	}
	report, err := collaborationReduceReport(evidence, registry)
	if err != nil {
		t.Fatal(err)
	}
	if got := collaborationReportRow(t, report, "TEAM-001").State; got != "unverified" {
		t.Fatalf("mutating the returned catalog changed the selected report state to %q", got)
	}
	evidence.Target.ProductCommit = strings.Repeat("e", 40)
	if _, err := collaborationReduceReport(evidence, registry); err == nil || !strings.Contains(err.Error(), "selected target changed") {
		t.Fatalf("mutated selected target error = %v", err)
	}
	evidence.Target = target
	evidence.Attempts[0].Attempt.Target.HostBinarySHA256 = strings.Repeat("f", 64)
	if _, err := collaborationReduceReport(evidence, registry); err == nil || !strings.Contains(err.Error(), "target changed") {
		t.Fatalf("mutated attempt target error = %v", err)
	}
	evidence.Attempts[0].Attempt.Target = target
	evidence.catalogData[0] = 'X'
	if _, err := collaborationReduceReport(evidence, registry); err == nil || !strings.Contains(err.Error(), "catalog digest changed") {
		t.Fatalf("mutated selected snapshot error = %v", err)
	}
}
