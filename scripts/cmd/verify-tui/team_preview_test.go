package main

import "testing"

func TestPreviewTranscriptChecksOnlySelectedPanel(t *testing.T) {
	notice := "Teammate Bob | available\nReport\nP07_REPORT_BOB\n\n"
	panel := "Pig Litter | team | transcript\nuser | Task:\nP07_TRANSCRIPT_ALICE\nassistant | P07_REPORT_ALICE\n"
	ready, crossed := previewTranscriptState(notice+panel, "Alice")
	if !ready || crossed {
		t.Fatalf("a separate Bob notice changed Alice's transcript result: ready=%v crossed=%v", ready, crossed)
	}

	ready, crossed = previewTranscriptState(notice+panel+"assistant | P07_REPORT_BOB\n", "Alice")
	if !ready || !crossed {
		t.Fatalf("a Bob report inside Alice's transcript was missed: ready=%v crossed=%v", ready, crossed)
	}

	ready, _ = previewTranscriptState(notice+"Pig Litter | team | transcript\nLoading transcript\n", "Bob")
	if ready {
		t.Fatal("the root notice satisfied the transcript readiness check")
	}
}
