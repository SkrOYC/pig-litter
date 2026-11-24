package piglitter

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestNativeRunningInputRetainsWindowAndTicket(t *testing.T) {
	withMemberControlServer(t, false, func(queue *memberControlQueue, _ *memberBinding, client *http.Client) {
		if _, err := queue.beginStarting("window-one"); err != nil {
			t.Fatal(err)
		}
		type outcome struct {
			disposition memberInputDisposition
			err         error
		}
		results := make(chan outcome, 1)
		go func() {
			disposition, err := queue.Queue(context.Background(), memberTextInput{mode: "follow_up", text: "first"})
			results <- outcome{disposition, err}
		}()
		command := nextMemberControl(t, client, queue.token())
		if command.Kind != "input" || command.Input == nil || command.Input.Window != "window-one" || command.Input.Text != "first" {
			t.Fatalf("native input command = %#v", command)
		}
		watch := make(chan bool, 1)
		go func() {
			status, body := postAuthority(t, client, queue.token(), "/input/cancel-watch", memberStartingResult{Window: command.Input.Window, Transfer: command.Input.Transfer})
			var result map[string]bool
			watch <- status == http.StatusOK && json.Unmarshal(body, &result) == nil && result["cancel"]
		}()
		ack := memberControlReply{ID: command.ID, Kind: "input"}
		replyMemberControl(t, client, queue.token(), ack)
		replyMemberControl(t, client, queue.token(), ack)
		result := memberStartingResult{Window: command.Input.Window, Transfer: command.Input.Transfer, Disposition: "queued"}
		for range 2 {
			if status, body := postAuthority(t, client, queue.token(), "/input/running-result", result); status != http.StatusOK {
				t.Fatalf("native input result = %d %s", status, body)
			}
		}
		if <-watch {
			t.Fatal("completed input was cancelled")
		}
		if got := <-results; got.err != nil || got.disposition != memberInputQueued {
			t.Fatalf("native input disposition = %#v", got)
		}
		result.Disposition = "handled"
		if status, _ := postAuthority(t, client, queue.token(), "/input/running-result", result); status == http.StatusOK {
			t.Fatal("conflicting input result replay was accepted")
		}
		publication := memberWindowPublication{Window: "window-two", Sequence: 2}
		for range 2 {
			if status, body := postAuthority(t, client, queue.token(), "/input/window", publication); status != http.StatusOK {
				t.Fatalf("window publication = %d %s", status, body)
			}
		}
		if status, _ := postAuthority(t, client, queue.token(), "/input/window", memberWindowPublication{Window: "window-one", Sequence: 3}); status == http.StatusOK {
			t.Fatal("an old opaque window ID was reused")
		}
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			disposition, err := queue.Queue(ctx, memberTextInput{mode: "steer", text: "second"})
			results <- outcome{disposition, err}
		}()
		command = nextMemberControl(t, client, queue.token())
		if command.Input == nil || command.Input.Window != "window-two" {
			t.Fatalf("new input used the wrong phase: %#v", command)
		}
		watch = make(chan bool, 1)
		go func() {
			status, body := postAuthority(t, client, queue.token(), "/input/cancel-watch", memberStartingResult{Window: command.Input.Window, Transfer: command.Input.Transfer})
			var result map[string]bool
			watch <- status == http.StatusOK && json.Unmarshal(body, &result) == nil && result["cancel"]
		}()
		cancel()
		if got := <-results; got.err == nil || got.disposition != memberInputUncertain {
			t.Fatalf("cancelled input = %#v", got)
		}
		if !<-watch {
			t.Fatal("native input did not receive cancellation")
		}
		ack = memberControlReply{ID: command.ID, Kind: "input"}
		replyMemberControl(t, client, queue.token(), ack)
		replyMemberControl(t, client, queue.token(), ack)
		result = memberStartingResult{Window: command.Input.Window, Transfer: command.Input.Transfer, Disposition: "cancelled"}
		if status, body := postAuthority(t, client, queue.token(), "/input/running-result", result); status != http.StatusOK {
			t.Fatalf("cancelled native result = %d %s", status, body)
		}
	})
}

func TestNativeRunningInputKeepsCallbackCauseWithoutCancellingTicket(t *testing.T) {
	withMemberControlServer(t, false, func(queue *memberControlQueue, _ *memberBinding, client *http.Client) {
		if _, err := queue.beginStarting("window"); err != nil {
			t.Fatal(err)
		}
		ctx := context.WithValue(context.Background(), memberCauseKey{}, memberCause{queue: queue, id: "callback"})
		finished := make(chan error, 1)
		go func() {
			_, err := queue.Queue(ctx, memberTextInput{mode: "follow_up", text: "dependent"})
			finished <- err
		}()
		command := nextMemberControl(t, client, queue.token())
		if command.Input == nil || command.Input.Cause != "callback" {
			t.Fatalf("callback cause was not retained: %#v", command)
		}
		replyMemberControl(t, client, queue.token(), memberControlReply{ID: command.ID, Kind: "input"})
		result := memberStartingResult{Window: command.Input.Window, Transfer: command.Input.Transfer, Disposition: "would_block"}
		for range 2 {
			if status, body := postAuthority(t, client, queue.token(), "/input/running-result", result); status != http.StatusOK {
				t.Fatalf("dependent wait result = %d %s", status, body)
			}
		}
		if err := <-finished; err == nil {
			t.Fatal("dependent input wait did not report that it remains pending")
		}
		watch := make(chan bool, 1)
		go func() {
			status, body := postAuthority(t, client, queue.token(), "/input/cancel-watch", memberStartingResult{Window: command.Input.Window, Transfer: command.Input.Transfer})
			var reply map[string]bool
			watch <- status == http.StatusOK && json.Unmarshal(body, &reply) == nil && reply["cancel"]
		}()
		result.Disposition = "queued"
		if status, body := postAuthority(t, client, queue.token(), "/input/running-result", result); status != http.StatusOK {
			t.Fatalf("eventual native input result = %d %s", status, body)
		}
		if <-watch {
			t.Fatal("dependent wait cancelled the retained native ticket")
		}
		queue.mu.Lock()
		retained := queue.running[result.Transfer]
		ok := retained != nil && retained.interim != nil && retained.result != nil && retained.result.Disposition == "queued"
		queue.mu.Unlock()
		if !ok {
			t.Fatal("native input did not retain the eventual result")
		}
	})
}
