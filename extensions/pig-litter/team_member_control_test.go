package piglitter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func withMemberControlServer(t *testing.T, held bool, exercise func(*memberControlQueue, *memberBinding, *http.Client)) {
	t.Helper()
	nativeCompletionFixture(t, func(o *owner, binding *memberBinding, _ *run, _ string, _ *runtimeHost) {
		queue, err := o.newMemberControlQueue(binding.record, binding.run, binding.ownership)
		if err != nil {
			t.Fatal(err)
		}
		defer queue.Close()
		o.tree.mu.Lock()
		binding.control = queue
		binding.record.native = nil
		binding.run.mu.Lock()
		binding.run.member = nil
		binding.run.mu.Unlock()
		if !held {
			err = queue.bind(binding)
			binding.record.native = binding
			binding.run.mu.Lock()
			binding.run.member = queue
			binding.run.mu.Unlock()
		}
		o.tree.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		directory, err := os.MkdirTemp("", "pig-member-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.RemoveAll(directory) })
		server, err := startMemberControlServer(context.Background(), directory, queue)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			if err := server.Close(); err != nil {
				t.Error(err)
			}
		}()
		transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, "unix", server.path)
		}}
		defer transport.CloseIdleConnections()
		exercise(queue, binding, &http.Client{Transport: transport, Timeout: 3 * time.Second})
	})
}

func postMemberControl(ctx context.Context, client *http.Client, path, token string, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://member"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Pig-Litter-Member", token)
	return client.Do(request)
}

func nextMemberControl(t *testing.T, client *http.Client, token string) memberControlCommand {
	t.Helper()
	response, err := postMemberControl(context.Background(), client, "/control/next", token, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var command memberControlCommand
	if response.StatusCode != http.StatusOK || json.NewDecoder(response.Body).Decode(&command) != nil {
		t.Fatalf("member control response = %s", response.Status)
	}
	return command
}

func replyMemberControl(t *testing.T, client *http.Client, token string, reply memberControlReply) {
	t.Helper()
	data, err := json.Marshal(reply)
	if err != nil {
		t.Fatal(err)
	}
	response, err := postMemberControl(context.Background(), client, "/control/result", token, data)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("member control result = %s: %s", response.Status, body)
	}
}

func TestMemberControlBindsAndValidatesNativeRequests(t *testing.T) {
	withMemberControlServer(t, true, func(queue *memberControlQueue, binding *memberBinding, client *http.Client) {
		response, err := postMemberControl(context.Background(), client, "/control/next", "wrong", nil)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusForbidden {
			t.Fatalf("foreign capability status = %s", response.Status)
		}
		if _, err := queue.Queue(context.Background(), memberCompletionInput{}); err == nil {
			t.Fatal("completion was accepted before handle installation")
		}
		type snapshotResult struct {
			value memberSnapshot
			err   error
		}
		ready := make(chan snapshotResult, 1)
		go func() {
			value, err := queue.Snapshot(context.Background(), memberHistoryBound{entries: 2, bytes: 1024})
			ready <- snapshotResult{value, err}
		}()
		command := nextMemberControl(t, client, queue.token())
		if command.Kind != "snapshot" || command.History == nil || command.History.Entries != 2 || command.History.Bytes != 1024 {
			t.Fatalf("held snapshot command = %#v", command)
		}
		entry := json.RawMessage(`{"type":"message","id":"entry"}`)
		phase, pending := memberIdle, 0
		replyMemberControl(t, client, queue.token(), memberControlReply{ID: command.ID, Kind: "snapshot", Snapshot: &memberSnapshotWire{Session: binding.member.ID, Cwd: binding.record.cwd, Model: binding.record.model, Tools: []string{"read"}, Phase: &phase, Pending: &pending, Entries: []json.RawMessage{entry}}})
		result := <-ready
		if result.err != nil || result.value.session != binding.member.ID || len(result.value.entries) != 1 || string(result.value.entries[0]) != string(entry) {
			t.Fatalf("native snapshot = %#v, %v", result.value, result.err)
		}
		invalid := make(chan error, 1)
		go func() {
			_, err := queue.Snapshot(context.Background(), memberHistoryBound{})
			invalid <- err
		}()
		command = nextMemberControl(t, client, queue.token())
		replyMemberControl(t, client, queue.token(), memberControlReply{ID: command.ID, Kind: "snapshot", Snapshot: &memberSnapshotWire{Session: binding.member.ID, Phase: &phase}})
		if err := <-invalid; err == nil {
			t.Fatal("snapshot without an explicit pending count was accepted")
		}
		queue.owner.tree.mu.Lock()
		err = queue.bind(binding)
		binding.record.native = binding
		binding.run.mu.Lock()
		binding.run.member = queue
		binding.run.mu.Unlock()
		queue.owner.tree.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		if _, err := queue.Queue(context.Background(), memberTextInput{mode: "follow_up", text: "continue"}); err == nil || !strings.Contains(err.Error(), "input window") {
			t.Fatalf("native text admission = %v", err)
		}
		observerResult := make(chan error, 1)
		go func() {
			disposition, err := queue.Queue(context.Background(), memberObserverInput{content: "activity", activity: &observerActivityMarker{PairingID: "pair", Sequence: 1}})
			if err == nil && disposition != memberInputUncertain {
				err = errors.New("observer message did not retain its uncertain disposition")
			}
			observerResult <- err
		}()
		command = nextMemberControl(t, client, queue.token())
		if command.Kind != "observer" || command.Observer == nil || command.Observer.Content != "activity" || command.Observer.Activity == nil || command.Observer.Activity.PairingID != "pair" {
			t.Fatalf("native observer command = %#v", command)
		}
		replyMemberControl(t, client, queue.token(), memberControlReply{ID: command.ID, Kind: "observer", Disposition: memberInputUncertain})
		if err := <-observerResult; err != nil {
			t.Fatal(err)
		}
		queue.owner.tree.mu.Lock()
		binding.record.team.Ref.Generation++
		queue.owner.tree.mu.Unlock()
		response, err = postMemberControl(context.Background(), client, "/control/next", queue.token(), nil)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusGone {
			t.Fatalf("stale member capability status = %s", response.Status)
		}
		queue.owner.tree.mu.Lock()
		binding.record.team.Ref.Generation--
		binding.record.native = nil
		queue.owner.tree.mu.Unlock()
		response, err = postMemberControl(context.Background(), client, "/control/next", queue.token(), nil)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusGone {
			t.Fatalf("removed native handle status = %s", response.Status)
		}
		queue.owner.tree.mu.Lock()
		binding.record.native = binding
		queue.owner.tree.mu.Unlock()
	})
}

func TestMemberControlSerializesAndFencesUncertainResults(t *testing.T) {
	withMemberControlServer(t, false, func(queue *memberControlQueue, _ *memberBinding, client *http.Client) {
		auditDone := make(chan error, 1)
		go func() { auditDone <- queue.RecordCompletion(context.Background(), completionAudit{Content: "audit"}) }()
		first := nextMemberControl(t, client, queue.token())
		if first.Kind != "audit" || first.Audit == nil || first.Audit.Content != "audit" {
			t.Fatalf("audit command = %#v", first)
		}
		type queuedResult struct {
			disposition memberInputDisposition
			err         error
		}
		completionDone := make(chan queuedResult, 1)
		go func() {
			disposition, err := queue.Queue(context.Background(), memberCompletionInput{content: "notice", details: completionDetails{ChildID: "child", Receipt: "receipt"}})
			completionDone <- queuedResult{disposition, err}
		}()
		blocked, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		_, err := postMemberControl(blocked, client, "/control/next", queue.token(), nil)
		cancel()
		if err == nil {
			t.Fatal("a second command was delivered before the first result")
		}
		bad := []byte(`{"id":"` + first.ID + `","kind":"audit","kind":"audit"}`)
		response, err := postMemberControl(context.Background(), client, "/control/result", queue.token(), bad)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Fatalf("duplicate result fields status = %s", response.Status)
		}
		replyMemberControl(t, client, queue.token(), memberControlReply{ID: first.ID, Kind: "audit"})
		if err := <-auditDone; err != nil {
			t.Fatal(err)
		}
		second := nextMemberControl(t, client, queue.token())
		if second.Kind != "completion" || second.Completion == nil || second.Completion.Content != "notice" || second.Completion.Details.Receipt != "receipt" {
			t.Fatalf("completion command = %#v", second)
		}
		replyMemberControl(t, client, queue.token(), memberControlReply{ID: second.ID, Kind: "completion", Disposition: memberInputHandled})
		if result := <-completionDone; result.err != nil || result.disposition != memberInputHandled {
			t.Fatalf("completion result = %#v", result)
		}
		waiting, stop := context.WithCancel(context.Background())
		uncertain := make(chan queuedResult, 1)
		go func() {
			disposition, err := queue.Queue(waiting, memberCompletionInput{content: "uncertain"})
			uncertain <- queuedResult{disposition, err}
		}()
		third := nextMemberControl(t, client, queue.token())
		stop()
		if result := <-uncertain; result.err == nil || result.disposition != memberInputUncertain {
			t.Fatalf("lost member result = %#v", result)
		}
		data, _ := json.Marshal(memberControlReply{ID: third.ID, Kind: "completion", Disposition: memberInputHandled})
		response, err = postMemberControl(context.Background(), client, "/control/result", queue.token(), data)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != http.StatusGone {
			t.Fatalf("late result status = %s", response.Status)
		}
		if _, err := queue.Snapshot(context.Background(), memberHistoryBound{}); err == nil {
			t.Fatal("uncertain dispatched command left the control queue open")
		}
	})
}

func TestMemberControlReclaimsCancelledRequestsAndSignalsStop(t *testing.T) {
	withMemberControlServer(t, false, func(queue *memberControlQueue, binding *memberBinding, client *http.Client) {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, memberControlPendingLimit)
		for range memberControlPendingLimit {
			go func() {
				_, err := queue.Snapshot(ctx, memberHistoryBound{})
				done <- err
			}()
		}
		deadline, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		for {
			queue.mu.Lock()
			count := len(queue.pending)
			queue.mu.Unlock()
			if count == memberControlPendingLimit {
				break
			}
			select {
			case <-deadline.Done():
				t.Fatalf("member control admitted %d pending requests", count)
			case <-time.After(10 * time.Millisecond):
			}
		}
		if _, err := queue.Snapshot(context.Background(), memberHistoryBound{}); err == nil || !strings.Contains(err.Error(), "bound reached") {
			t.Fatalf("saturated control queue = %v", err)
		}
		cancel()
		for range memberControlPendingLimit {
			if err := <-done; err == nil {
				t.Fatal("cancelled request reported success")
			}
		}
		ready := make(chan error, 1)
		go func() {
			_, err := queue.Snapshot(context.Background(), memberHistoryBound{})
			ready <- err
		}()
		command := nextMemberControl(t, client, queue.token())
		if command.Kind != "snapshot" {
			t.Fatalf("cancelled request was retained: %#v", command)
		}
		phase, pending := memberIdle, 0
		replyMemberControl(t, client, queue.token(), memberControlReply{ID: command.ID, Kind: "snapshot", Snapshot: &memberSnapshotWire{Session: binding.member.ID, Phase: &phase, Pending: &pending}})
		if err := <-ready; err != nil {
			t.Fatalf("control did not recover capacity: %v", err)
		}
		queue.Cancel()
		queue.Cancel()
		command = nextMemberControl(t, client, queue.token())
		if command.Kind != "cancel" {
			t.Fatalf("native cancellation command = %#v", command)
		}
		if _, err := queue.Snapshot(context.Background(), memberHistoryBound{}); err == nil {
			t.Fatal("cancelled control accepted another request")
		}
	})
}

func TestMemberControlListenerPreservesAReplacedSocket(t *testing.T) {
	nativeCompletionFixture(t, func(o *owner, binding *memberBinding, _ *run, _ string, _ *runtimeHost) {
		queue, err := o.newMemberControlQueue(binding.record, binding.run, binding.ownership)
		if err != nil {
			t.Fatal(err)
		}
		o.tree.mu.Lock()
		binding.control = queue
		if err := queue.bind(binding); err != nil {
			o.tree.mu.Unlock()
			t.Fatal(err)
		}
		o.tree.mu.Unlock()
		occupied := t.TempDir()
		if err := os.Chmod(occupied, 0o700); err != nil {
			t.Fatal(err)
		}
		occupiedPath := filepath.Join(occupied, "member-control.sock")
		if err := os.WriteFile(occupiedPath, []byte("unrelated"), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := startMemberControlServer(context.Background(), occupied, queue); err == nil {
			t.Fatal("accepted an occupied control socket path")
		}
		if data, err := os.ReadFile(occupiedPath); err != nil || string(data) != "unrelated" {
			t.Fatalf("occupied path changed: %q, %v", data, err)
		}
		directory := t.TempDir()
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatal(err)
		}
		server, err := startMemberControlServer(context.Background(), directory, queue)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = server.Close() }()
		info, err := os.Lstat(server.path)
		if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode().Perm() != 0o600 {
			t.Fatalf("control socket permissions = %v, %v", info, err)
		}
		if err := os.Rename(server.path, server.path+".original"); err != nil {
			t.Fatal(err)
		}
		other, err := net.ListenUnix("unix", &net.UnixAddr{Name: server.path, Net: "unix"})
		if err != nil {
			t.Fatal(err)
		}
		other.SetUnlinkOnClose(false)
		defer other.Close()
		otherInfo, err := os.Lstat(server.path)
		if err != nil {
			t.Fatal(err)
		}
		if err := server.Close(); err == nil || !strings.Contains(err.Error(), "identity changed") {
			t.Fatalf("replaced socket cleanup = %v", err)
		}
		after, err := os.Lstat(server.path)
		if err != nil || !os.SameFile(otherInfo, after) {
			t.Fatalf("cleanup removed the other socket: %v", err)
		}
	})
}

func TestMemberControlCancellationBypassesHeldCommand(t *testing.T) {
	withMemberControlServer(t, false, func(queue *memberControlQueue, _ *memberBinding, client *http.Client) {
		done := make(chan error, 1)
		go func() { done <- queue.RecordCompletion(context.Background(), completionAudit{Content: "held audit"}) }()
		command := nextMemberControl(t, client, queue.token())
		if command.Kind != "audit" {
			t.Fatalf("held command = %#v", command)
		}
		type watchResult struct {
			response *http.Response
			err      error
		}
		watched := make(chan watchResult, 1)
		go func() {
			response, err := postMemberControl(context.Background(), client, "/control/watch", queue.token(), nil)
			watched <- watchResult{response, err}
		}()
		queue.Cancel()
		result := <-watched
		if result.err != nil {
			t.Fatal(result.err)
		}
		defer result.response.Body.Close()
		var cancelCommand memberControlCommand
		if result.response.StatusCode != http.StatusOK || json.NewDecoder(result.response.Body).Decode(&cancelCommand) != nil || cancelCommand.Kind != "cancel" {
			t.Fatalf("cancellation watch = %s %#v", result.response.Status, cancelCommand)
		}
		if err := <-done; err == nil {
			t.Fatal("held command reported success after cancellation")
		}
	})
}
