package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestCodemodeChildUsesLatestResumeInstruction(t *testing.T) {
	oldPrompt, err := json.Marshal("Task codemode-resume-target CODEMODE_RESUME_BEFORE_THROW")
	if err != nil {
		t.Fatal(err)
	}
	resumePrompt, err := json.Marshal("CODEMODE_RESUME_NOW")
	if err != nil {
		t.Fatal(err)
	}
	body := modelRequest{Messages: []message{
		{Role: "user", Content: oldPrompt},
		{Role: "user", Content: resumePrompt},
	}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &fixture{held: map[string]chan struct{}{}, aborted: map[string]bool{}, changed: make(chan struct{})}

	text, calls, err := f.codemodeChild(ctx, body)
	if err != nil || text != "CODEMODE_RESUMED_RESULT" || len(calls) != 0 {
		t.Fatalf("resume after the prior hold prompt returned %q, %#v, %v", text, calls, err)
	}
}
