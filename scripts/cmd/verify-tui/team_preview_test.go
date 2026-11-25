package main

import "testing"

func TestPreviewTranscriptChecksOnlySelectedPanel(t *testing.T) {
	notice := "Teammate Bob | available\nReport\nTEAM_REPORT_BOB\n\n"
	panel := "Pig Litter | team | transcript\nuser | Task:\nTEAM_TRANSCRIPT_ALICE\nassistant | TEAM_REPORT_ALICE\n"
	ready, crossed := previewTranscriptState(notice+panel, "Alice")
	if !ready || crossed {
		t.Fatalf("a separate Bob notice changed Alice's transcript result: ready=%v crossed=%v", ready, crossed)
	}

	ready, crossed = previewTranscriptState(notice+panel+"assistant | TEAM_REPORT_BOB\n", "Alice")
	if !ready || !crossed {
		t.Fatalf("a Bob report inside Alice's transcript was missed: ready=%v crossed=%v", ready, crossed)
	}

	ready, _ = previewTranscriptState(notice+"Pig Litter | team | transcript\nLoading transcript\n", "Bob")
	if ready {
		t.Fatal("the root notice satisfied the transcript readiness check")
	}
}
