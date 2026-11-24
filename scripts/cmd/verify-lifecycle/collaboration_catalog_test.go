package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func collaborationTestCatalog(t *testing.T) collaborationCatalog {
	t.Helper()
	data, err := os.ReadFile("../../testdata/collaboration/requirements.json")
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := collaborationReadCatalog(data)
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestCollaborationCatalogAcceptsAttributedInventory(t *testing.T) {
	catalog := collaborationTestCatalog(t)
	var selected *collaborationRequirement
	for index := range catalog.Requirements {
		if catalog.Requirements[index].ID == "TEAM-003" {
			selected = &catalog.Requirements[index]
		}
	}
	if selected == nil || selected.ReferenceRelationship.State != "deliberately_different" || selected.ProductEvidence.State != "unverified" || selected.AcceptanceScenario != "team-roster" {
		t.Fatalf("named-child requirement = %#v", selected)
	}
}

func TestCollaborationCatalogRejectsIncompleteOrForgedClaims(t *testing.T) {
	for _, test := range []struct {
		name string
		edit func(*collaborationCatalog)
		want string
	}{
		{"duplicate requirement", func(c *collaborationCatalog) { c.Requirements = append(c.Requirements, c.Requirements[0]) }, "duplicate requirement"},
		{"duplicate scenario", func(c *collaborationCatalog) { c.Scenarios = append(c.Scenarios, c.Scenarios[0]) }, "duplicate scenario"},
		{"missing source", func(c *collaborationCatalog) { c.Requirements[0].Reference.Section = "" }, "incomplete reference"},
		{"unknown source kind", func(c *collaborationCatalog) { c.Requirements[0].Reference.Kind = "captured_guess" }, "incomplete reference"},
		{"dangling scenario", func(c *collaborationCatalog) { c.Requirements[0].AcceptanceScenario = "missing" }, "acceptance scenario"},
		{"wrong owner", func(c *collaborationCatalog) { c.Requirements[0].Owner = "P04" }, "different owner"},
		{"missing difference reason", func(c *collaborationCatalog) {
			c.Requirements[0].ReferenceRelationship = collaborationRelationship{State: "deliberately_different"}
		}, "reference relationship"},
		{"claimed match", func(c *collaborationCatalog) { c.Requirements[0].ProductEvidence.State = "matched" }, "cannot assert runtime evidence"},
		{"embedded record", func(c *collaborationCatalog) {
			c.Requirements[0].ProductEvidence.Records = []json.RawMessage{json.RawMessage(`{}`)}
		}, "cannot assert runtime evidence"},
		{"missing frame", func(c *collaborationCatalog) {
			for i, req := range c.Requirements {
				if req.Frame != nil {
					c.Requirements = append(c.Requirements[:i], c.Requirements[i+1:]...)
					return
				}
			}
		}, "frame census"},
		{"duplicate frame", func(c *collaborationCatalog) {
			var first *collaborationFrame
			for i := range c.Requirements {
				if c.Requirements[i].Frame == nil {
					continue
				}
				if first == nil {
					first = c.Requirements[i].Frame
					continue
				}
				c.Requirements[i].Frame.Variant = first.Variant
				c.Requirements[i].Title = first.Variant
				return
			}
		}, "duplicate frame"},
	} {
		t.Run(test.name, func(t *testing.T) {
			catalog := collaborationTestCatalog(t)
			test.edit(&catalog)
			data, err := json.Marshal(catalog)
			if err != nil {
				t.Fatal(err)
			}
			_, err = collaborationReadCatalog(data)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestCollaborationJSONRejectsAmbiguousDocuments(t *testing.T) {
	var value struct {
		State string `json:"state"`
	}
	for _, input := range []string{
		`{"state":"unverified","state":"matched"}`,
		`{"state":"unverified","unknown":true}`,
		`{"state":"unverified"} {"state":"matched"}`,
		strings.Repeat("[", 130) + "0" + strings.Repeat("]", 130),
	} {
		if err := collaborationDecodeJSON([]byte(input), &value); err == nil {
			t.Fatalf("accepted %s", input)
		}
	}
	if err := collaborationDecodeJSON([]byte(`{"state":"unverified"}`), &value); err != nil || value.State != "unverified" {
		t.Fatalf("valid JSON = %#v, %v", value, err)
	}
}
