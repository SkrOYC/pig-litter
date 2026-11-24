package piglitter

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
)

func TestNativeStartingTransferReplaysAndPreservesMailbox(t *testing.T) {
	withMemberControlServer(t, false, func(queue *memberControlQueue, binding *memberBinding, client *http.Client) {
		tree, selected := queue.owner.tree, binding.run
		tree.config.MaxMailbox = 2
		for _, input := range []message{{mode: "steer", text: "first"}, {mode: "follow_up", text: "second"}} {
			if _, _, err := tree.message(context.Background(), nil, selected, input.mode, input.text); err != nil {
				t.Fatal(err)
			}
		}
		token := queue.token()
		if status, _ := postAuthority(t, client, "wrong", "/input/start", memberStartingWindow{Window: "window"}); status != http.StatusForbidden {
			t.Fatalf("unauthenticated transfer status = %d", status)
		}
		start := func(window string) (int, []memberStartingTransfer) {
			t.Helper()
			status, body := postAuthority(t, client, token, "/input/start", memberStartingWindow{Window: window})
			var transfers []memberStartingTransfer
			if status == http.StatusOK && json.Unmarshal(body, &transfers) != nil {
				t.Fatalf("invalid starting response: %s", body)
			}
			return status, transfers
		}
		status, first := start("window")
		if status != http.StatusOK || len(first) != 2 || first[0].Mode != "steer" || first[0].Text != "first" || first[1].Mode != "follow_up" || first[1].Text != "second" || first[0].ID == "" || first[0].ID == first[1].ID {
			t.Fatalf("starting transfer = %d %#v", status, first)
		}
		status, replay := start("window")
		if status != http.StatusOK || len(replay) != 2 || replay[0].ID != first[0].ID || replay[1].ID != first[1].ID {
			t.Fatalf("lost reply replay = %d %#v", status, replay)
		}
		if status, _ := start("other-window"); status == http.StatusOK {
			t.Fatal("startup was transferred to a different window")
		}
		waiting, cancel := context.WithCancel(context.Background())
		started, finished := make(chan struct{}), make(chan error, 1)
		go func() {
			close(started)
			_, _, err := tree.message(waiting, nil, selected, "steer", "third")
			finished <- err
		}()
		<-started
		cancel()
		if err := <-finished; err == nil {
			t.Fatal("cancelled message crossed the frozen startup")
		}
		ids := []string{first[0].ID, first[1].ID}
		if status, _ := postAuthority(t, client, token, "/input/installed", memberStartingInstallation{Window: "window", Transfers: ids[:1]}); status == http.StatusOK {
			t.Fatal("partial starting installation was accepted")
		}
		result := memberStartingResult{Window: "window", Transfer: ids[0], Disposition: "queued"}
		if status, _ := postAuthority(t, client, token, "/input/result", result); status == http.StatusOK {
			t.Fatal("starting result arrived before installation")
		}
		installation := memberStartingInstallation{Window: "window", Transfers: ids}
		for range 2 {
			if status, body := postAuthority(t, client, token, "/input/installed", installation); status != http.StatusOK {
				t.Fatalf("starting installation = %d %s", status, body)
			}
		}
		for range 2 {
			if status, body := postAuthority(t, client, token, "/input/result", result); status != http.StatusOK {
				t.Fatalf("starting result replay = %d %s", status, body)
			}
		}
		result.Disposition = "handled"
		if status, _ := postAuthority(t, client, token, "/input/result", result); status == http.StatusOK {
			t.Fatal("conflicting result replay was accepted")
		}
		result.Transfer = ids[1]
		if status, body := postAuthority(t, client, token, "/input/result", result); status != http.StatusOK {
			t.Fatalf("second starting result = %d %s", status, body)
		}
		selected.messageMu.Lock()
		pending := selected.pendingMessages
		selected.messageMu.Unlock()
		if pending != 0 {
			t.Fatalf("starting messages retained %d root claims", pending)
		}
		queue.Close()
		if status, _ := postAuthority(t, client, token, "/input/start", memberStartingWindow{Window: "window"}); status != http.StatusGone {
			t.Fatalf("closed authority status = %d", status)
		}
	})
}

func TestNativeStartingTransferFailsOnOwnerClose(t *testing.T) {
	withMemberControlServer(t, false, func(queue *memberControlQueue, binding *memberBinding, client *http.Client) {
		if _, _, err := queue.owner.tree.message(context.Background(), nil, binding.run, "steer", "held"); err != nil {
			t.Fatal(err)
		}
		if status, _ := postAuthority(t, client, queue.token(), "/input/start", memberStartingWindow{Window: "window"}); status != http.StatusOK {
			t.Fatalf("starting transfer status = %d", status)
		}
		queue.mu.Lock()
		result := queue.starting.transfers[0].request.result
		queue.mu.Unlock()
		queue.Close()
		if err := <-result; err == nil {
			t.Fatal("owner close reported a successful starting transfer")
		}
		binding.run.messageMu.Lock()
		pending := binding.run.pendingMessages
		binding.run.messageMu.Unlock()
		if pending != 0 {
			t.Fatalf("owner close retained %d root claims", pending)
		}
	})
}
