package piglitter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestOutcomeHandbackClippingPreservesRetainedReportAndWireFields(t *testing.T) {
	outcome := &Outcome{Run: Ref{"child:1", 2}, State: Partial, Model: "fixture/child", Text: strings.Repeat("é", 5000), Error: "tool read failed", Usage: Usage{TotalTokens: 15}}
	for _, operation := range []string{"litter_wait", "litter_stop", "litter_inspect"} {
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
		if wire.Operation != operation || wire.State != Partial || wire.Outcome.Run != (Ref{"child:1", 2}) || wire.Outcome.Usage.TotalTokens != 15 || len(wire.Outcome.Text) != 8192 || !wire.HandbackTruncated {
			t.Fatalf("%s wire changed %#v", operation, wire)
		}
	}
	if len(outcome.Text) != 10000 {
		t.Fatal("wire clipping changed the retained report")
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
