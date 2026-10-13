package piglitter

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestOutcomeHandbackClippingPreservesRetainedReportAndWireFields(t *testing.T) {
	outcome := &Outcome{Run: Ref{ID: "child:1"}, State: Partial, Model: "fixture/child", Text: strings.Repeat("é", 5000), Error: "tool read failed", Usage: Usage{TotalTokens: 15}}
	for _, operation := range []string{"litter_wait", "litter_stop"} {
		encoded, failed := resultContent(operation, map[string]any{"state": outcome.State, "outcome": outcome}, nil)
		if failed {
			t.Fatalf("%s handback failed", operation)
		}
		var wire struct {
			Operation         string
			State             State
			Outcome           Outcome
			HandbackTruncated bool
		}
		if err := json.Unmarshal([]byte(encoded), &wire); err != nil {
			t.Fatal(err)
		}
		if wire.Operation != operation || wire.State != Partial || wire.Outcome.Run != (Ref{ID: "child:1"}) || wire.Outcome.Usage.TotalTokens != 15 || len(wire.Outcome.Text) != 8192 || !wire.HandbackTruncated {
			t.Fatalf("%s wire changed %#v", operation, wire)
		}
	}
	if len(outcome.Text) != 10000 {
		t.Fatal("wire clipping changed the retained report")
	}
}

func TestInspectHandbackHasOnlyCurrentStateChildAndOutcome(t *testing.T) {
	outcome := &Outcome{Run: Ref{ID: "child-1"}, State: Completed, Text: strings.Repeat("é", 5000)}
	child := Snapshot{Ref: Ref{ID: "child-1"}, State: Completed, Name: "reviewer"}
	encoded, failed := resultContent("litter_inspect", map[string]any{"state": child.State, "child": child, "outcome": outcome}, nil)
	if failed {
		t.Fatal("inspect handback failed")
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal([]byte(encoded), &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 3 {
		t.Fatalf("inspect returned extra top-level fields: %s", encoded)
	}
	var state State
	if err := json.Unmarshal(wire["state"], &state); err != nil {
		t.Fatal(err)
	}
	var gotChild Snapshot
	if err := json.Unmarshal(wire["child"], &gotChild); err != nil {
		t.Fatal(err)
	}
	var gotOutcome Outcome
	if err := json.Unmarshal(wire["outcome"], &gotOutcome); err != nil {
		t.Fatal(err)
	}
	if _, ok := wire["operation"]; ok {
		t.Fatalf("inspect included operation metadata: %s", encoded)
	}
	if _, ok := wire["handbackTruncated"]; ok {
		t.Fatalf("inspect included top-level truncation metadata: %s", encoded)
	}
	if state != Completed || gotChild.ID != child.ID || gotOutcome.Run.ID != outcome.Run.ID || len(gotOutcome.Text) != 8192 || !gotOutcome.HandbackTruncated {
		t.Fatalf("inspect handback changed %#v %#v %#v", state, gotChild, gotOutcome)
	}
}

func TestControlSchemasAcceptIDOnlyInputsAndRejectGeneration(t *testing.T) {
	accepted := map[string][]string{
		"litter_inspect": {`{"id":"child-1"}`},
		"litter_message": {`{"id":"child-1","text":"continue"}`, `{"id":"child-1","text":"continue","mode":"follow_up"}`, `{"id":"child-1","text":"continue","resume":true}`},
		"litter_wait":    {`{"id":"child-1"}`, `{"id":"child-1","timeoutMs":30000}`},
		"litter_stop":    {`{"id":"child-1"}`},
	}
	for operation, examples := range accepted {
		t.Run(operation, func(t *testing.T) {
			compiler := jsonschema.NewCompiler()
			location := fmt.Sprintf("https://schemas.test/%s.json", operation)
			schemaJSON, err := json.Marshal(schemas[operation])
			if err != nil {
				t.Fatal(err)
			}
			var schemaValue any
			if err := json.Unmarshal(schemaJSON, &schemaValue); err != nil {
				t.Fatal(err)
			}
			if err := compiler.AddResource(location, schemaValue); err != nil {
				t.Fatal(err)
			}
			schema, err := compiler.Compile(location)
			if err != nil {
				t.Fatal(err)
			}
			for _, example := range examples {
				var value map[string]any
				if err := json.Unmarshal([]byte(example), &value); err != nil {
					t.Fatal(err)
				}
				if err := schema.Validate(value); err != nil {
					t.Fatalf("rejected current arguments %s: %v", example, err)
				}
				value["generation"] = 1
				if err := schema.Validate(value); err == nil {
					t.Fatalf("accepted removed generation field in %s", example)
				}
			}
		})
	}
}

func TestMemorySettingsDisableRetryAndWarming(t *testing.T) {
	manager, err := memorySettings()
	if err != nil {
		t.Fatal(err)
	}
	settings := manager.Get()
	if settings.CacheWarming != "off" || settings.Retry == nil || settings.Retry.Enabled == nil || *settings.Retry.Enabled {
		t.Fatalf("unexpected child settings %#v", settings)
	}
}
