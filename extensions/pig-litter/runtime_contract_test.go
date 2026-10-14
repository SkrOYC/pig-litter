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

func TestLongSoftTurnBudgetAndRemovedLimits(t *testing.T) {
	config, err := ParseConfig([]byte("depth: 3\nconcurrency: 6\nmax_turns: 1000000\nmax_result_bytes: 50000\n"))
	if err != nil || config.Depth != 3 || config.Concurrency != 6 || config.MaxTurns != 1000000 || config.MaxResultBytes != 50000 {
		t.Errorf("long soft budget rejected or settings lost: %#v %v", config, err)
	}
	for _, key := range []string{"max_records", "max_history_bytes"} {
		if _, err := ParseConfig([]byte(key + ": 1024\n")); err == nil {
			t.Errorf("accepted removed setting %s", key)
		}
	}
}
