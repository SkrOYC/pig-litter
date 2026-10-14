package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestOutcomeHandbackPreservesRetainedReportAndWireFields(t *testing.T) {
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
		if wire.Operation != operation || wire.State != Partial || wire.Outcome.Run != (Ref{ID: "child:1"}) || wire.Outcome.Usage.TotalTokens != 15 || wire.Outcome.Text != strings.Repeat("é", 5000) || wire.HandbackTruncated {
			t.Fatalf("%s wire changed %#v", operation, wire)
		}
	}
	if len(outcome.Text) != 10000 {
		t.Fatal("wire clipping changed the retained report")
	}
}

func TestInspectHandbackIncludesCommonResultMetadata(t *testing.T) {
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
	if len(wire) != 5 {
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
	if string(wire["operation"]) != `"litter_inspect"` {
		t.Fatalf("inspect lost operation metadata: %s", encoded)
	}
	if string(wire["handbackTruncated"]) != "false" {
		t.Fatalf("inspect lost top-level truncation metadata: %s", encoded)
	}
	if state != Completed || gotChild.ID != child.ID || gotOutcome.Run.ID != outcome.Run.ID || gotOutcome.Text != strings.Repeat("é", 5000) || gotOutcome.HandbackTruncated {
		t.Fatalf("inspect handback changed %#v %#v %#v", state, gotChild, gotOutcome)
	}
}

func TestStopReportsAnAlreadySettledOutcome(t *testing.T) {
	for _, state := range []State{Completed, Failed, Partial, Stopped} {
		t.Run(string(state), func(t *testing.T) {
			tree := testTree(t, defaultConfig())
			child := testAdmission(t, tree, Ref{}, "scout", "settled")
			selected := testRun(t, tree, child.Ref)
			tree.finish(selected.owner, selected, Outcome{State: state, Text: "retained report"})
			withCompletionHost(t, tree, func(o *owner) {
				ticket, err := o.callScope(nil).begin(nil, "stop", "litter_stop")
				if err != nil {
					t.Fatal(err)
				}
				raw, _ := json.Marshal(map[string]any{"id": child.ID})
				value, err := o.operation(context.Background(), raw, ticket, nil)
				if err != nil || value["state"] != state || value["outcome"].(*Outcome).Text != "retained report" || value["stoppedCount"] != 0 {
					t.Fatalf("stop lost the settled outcome: %#v %v", value, err)
				}
			}, func(string, json.RawMessage) json.RawMessage { return json.RawMessage(`"fixture"`) })
		})
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
