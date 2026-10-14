package piglitter

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEveryResultOperationReturnsEnvelopeAndFiftyKBReport(t *testing.T) {
	report := strings.Repeat("é", 25000)
	for _, operation := range []string{"litter_inspect", "litter_wait", "litter_stop"} {
		outcome := &Outcome{Run: Ref{ID: "child"}, State: Partial, Text: report}
		encoded, failed := resultContent(operation, map[string]any{"state": Partial, "outcome": outcome}, nil)
		var envelope struct {
			Operation         string  `json:"operation"`
			HandbackTruncated *bool   `json:"handbackTruncated"`
			Outcome           Outcome `json:"outcome"`
		}
		if err := json.Unmarshal([]byte(encoded), &envelope); err != nil {
			t.Fatal(err)
		}
		if failed || envelope.Operation != operation || envelope.HandbackTruncated == nil || *envelope.HandbackTruncated || envelope.Outcome.Text != report {
			t.Errorf("%s lost its envelope or report: operation=%q truncation=%v bytes=%d failed=%t", operation, envelope.Operation, envelope.HandbackTruncated, len(envelope.Outcome.Text), failed)
		}
	}
}
