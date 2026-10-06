package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
)

func framedJSON(raw string) []byte {
	var header [4]byte
	binary.BigEndian.PutUint32(header[:], uint32(len(raw)))
	return append(header[:], []byte(raw)...)
}

func TestCompletionProxyPreservesExactFramesAndObservesTaggedCall(t *testing.T) {
	raw := ` { "type" : "call", "id":"call-1", "call": {"method":"sendMessage", "args":{"message":{"customType":"litter_completion","content":"report é","display":false,"details":{"childId":"child:1","generation":2}},"options":{"deliverAs":"followUp"}}}} `
	input := append(framedJSON(raw), framedJSON(`{"type":"call_result","id":"call-1","call_result":{"result":{}}}`)...)
	var forwarded bytes.Buffer
	seen := []string{}
	err := forwardFrames(&forwarded, bytes.NewReader(input), func(envelope subprocess.Envelope) error {
		seen = append(seen, envelope.Type)
		if envelope.Type == subprocess.MsgCall {
			var delivery completionDelivery
			if err := json.Unmarshal(envelope.Call.Args, &delivery); err != nil {
				return err
			}
			if delivery.Message.Content != "report é" || delivery.Message.Details.ChildID != "child:1" || delivery.Message.Details.Generation != 2 || delivery.Options.DeliverAs != "followUp" || delivery.Options.TriggerTurn != nil {
				t.Fatalf("tagged completion call decoded incorrectly: %#v", delivery)
			}
		}
		return nil
	})
	if !errors.Is(err, io.EOF) || !bytes.Equal(forwarded.Bytes(), input) || len(seen) != 2 || seen[0] != subprocess.MsgCall || seen[1] != subprocess.MsgCallResult {
		t.Fatalf("proxy changed frames or lost envelope order: error=%v observed=%v", err, seen)
	}
}

func TestCompletionProxyRejectsMalformedFramesBeforeForwarding(t *testing.T) {
	var oversized [4]byte
	binary.BigEndian.PutUint32(oversized[:], subprocess.MaxFrameSize+1)
	for _, test := range []struct {
		name  string
		input []byte
	}{
		{"oversized", oversized[:]},
		{"empty", make([]byte, 4)},
		{"partial header", []byte{0}},
		{"partial body", framedJSON(`{"type":"call"}`)[:8]},
		{"invalid JSON", framedJSON(`{"type":`)},
	} {
		t.Run(test.name, func(t *testing.T) {
			var forwarded bytes.Buffer
			err := forwardFrames(&forwarded, bytes.NewReader(test.input), func(subprocess.Envelope) error { return nil })
			if err == nil || forwarded.Len() != 0 {
				t.Fatalf("malformed frame forwarded: error=%v bytes=%d", err, forwarded.Len())
			}
		})
	}
}

func TestBusyCompletionRequiresReportInModelRequest(t *testing.T) {
	run := ref{"child:1", 2}
	completion := `{"id":"child:1","generation":2,"state":"completed","model":"fixture/completion_child","report":"completed busy-child result"}`
	for _, test := range []struct {
		name, role, completion string
		processed              bool
	}{
		{"real completion", "user", completion, true},
		{"missing completion", "user", "BUSY_BEGIN", false},
		{"tool result only", "tool", completion, false},
		{"stale generation", "user", `{"id":"child:1","generation":1,"state":"completed","model":"fixture/completion_child","report":"completed busy-child result"}`, false},
		{"other child", "user", `{"id":"child:2","generation":2,"state":"completed","model":"fixture/completion_child","report":"completed busy-child result"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			raw, _ := json.Marshal(test.completion)
			f := &fixture{stage: 2, runs: []ref{run}, checks: map[string]bool{}}
			body := modelRequest{Messages: []message{{Role: "tool", Content: json.RawMessage(`"{}"`)}, {Role: test.role, Content: raw}}}
			answer, calls, err := f.parentCompletion(context.Background(), httptest.NewRecorder(), body)
			if test.processed {
				if err != nil || answer != "BUSY_COMPLETION_PROCESSED" || len(calls) != 0 || !f.checks["busyCompletionInModelRequest"] {
					t.Fatalf("completion was not processed: %q %#v %v", answer, calls, err)
				}
			} else if err == nil || f.checks["busyCompletionInModelRequest"] {
				t.Fatal("fixture accepted a request without the correct child's completion")
			}
		})
	}
}
