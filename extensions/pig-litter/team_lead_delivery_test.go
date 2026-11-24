package piglitter

import (
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/coding/extension/host/subprocess"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestLeadMessageWaitsForNativePromotionAndObservesOnce(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	sender, _ := peerTestMembers(t, store, workspace)
	manager := nativeTestManager(t, t.TempDir(), string(root))
	file := *manager.GetSessionFile()
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	senderRecord := &record{id: sender.Ref.ID, team: &sender, parent: tree.root, admitted: true}
	selected := tree.newRunLocked(Ref{ID: sender.Ref.ID}, "")
	selected.owner, senderRecord.run = senderRecord, selected
	tree.records[senderRecord.id] = senderRecord
	var mu sync.Mutex
	var sent []struct {
		Message sdk.CustomMessage
		Options sdk.SendMessageOptions
	}
	withCompletionMode(t, tree, "tui", func(o *owner) {
		o.sessionID, o.teamStore = string(root), store
		ticket := &callTicket{caller: selected, operation: "team_send"}
		raw := json.RawMessage(`{"recipient":"team-lead","messageId":"root-promotion","text":"hello lead","summary":"greeting"}`)
		type outcome struct {
			value map[string]any
			err   error
		}
		outcomes := make(chan outcome, 2)
		for range 2 {
			go func() {
				value, err := o.operation(context.Background(), raw, ticket, nil)
				outcomes <- outcome{value, err}
			}()
		}
		for range 2 {
			result := <-outcomes
			if result.err != nil || result.value["state"] != "accepted" {
				t.Errorf("lead send = %#v, %v", result.value, result.err)
				return
			}
		}
		mu.Lock()
		queued := append([]struct {
			Message sdk.CustomMessage
			Options sdk.SendMessageOptions
		}(nil), sent...)
		mu.Unlock()
		if len(queued) != 1 || queued[0].Options.TriggerTurn == nil || *queued[0].Options.TriggerTurn || queued[0].Options.DeliverAs != "nextTurn" {
			t.Errorf("root promotion = %#v", queued)
			return
		}
		var content struct {
			Sender     MemberRef `json:"sender"`
			SenderName string    `json:"senderName"`
			Provenance string    `json:"provenance"`
			Body       peerBody  `json:"body"`
		}
		text, ok := queued[0].Message.Content.(string)
		if !ok || json.Unmarshal([]byte(text), &content) != nil || content.Sender != sender.Ref || content.SenderName != "alice" || content.Provenance != "verified" || content.Body.Text != "hello lead" {
			t.Fatalf("root message content = %#v", queued[0].Message.Content)
		}
		request, _ := parsePeerRequest(raw)
		receipt, _, err := store.acceptPeer(sender.Ref, request)
		if err != nil || receipt.State != peerAttempted {
			t.Fatalf("unpromoted receipt = %#v, %v", receipt, err)
		}
		entry, err := manager.AppendCustomMessageEntry("pig_litter_peer", text, true, queued[0].Message.Details)
		if err != nil {
			t.Fatal(err)
		}
		lead := MemberRef{Team: sender.Ref.Team, ID: root, Generation: 1}
		if err := store.observePeers(lead, file); err != nil {
			t.Fatal(err)
		}
		observed, _, err := store.acceptPeer(sender.Ref, request)
		if err != nil || observed.State != peerObserved || observed.NativeEntryID != entry || observed.SessionID != root {
			t.Fatalf("promoted receipt = %#v, %v", observed, err)
		}
		result, err := o.operation(context.Background(), raw, ticket, nil)
		mu.Lock()
		count := len(sent)
		mu.Unlock()
		if err != nil || result["state"] != "accepted" || result["delivery"] != "unverified" || count != 1 {
			t.Errorf("observed retry = %#v, sends=%d, %v", result, count, err)
		}
	}, func(method string, raw json.RawMessage) json.RawMessage {
		switch method {
		case "sessionRead":
			var query struct{ Method string }
			_ = json.Unmarshal(raw, &query)
			if query.Method == "getSessionFile" {
				value, _ := json.Marshal(file)
				return value
			}
			value, _ := json.Marshal(root)
			return value
		case "host.getProcessIdentity":
			return json.RawMessage(`{"pid":12345,"birth":"linux:1","scope":"linux:fixture"}`)
		case "sendMessage":
			var call struct {
				Message sdk.CustomMessage
				Options sdk.SendMessageOptions
			}
			if err := json.Unmarshal(raw, &call); err != nil {
				t.Fatal(err)
			}
			mu.Lock()
			sent = append(sent, call)
			mu.Unlock()
			return json.RawMessage(`{}`)
		default:
			t.Fatalf("unexpected host action %s", method)
			return nil
		}
	})
}

func TestLeadNativeObservationRequiresCurrentRootEpoch(t *testing.T) {
	store, _, root, workspace := storeTestOpen(t)
	sender, _ := peerTestMembers(t, store, workspace)
	manager := nativeTestManager(t, t.TempDir(), string(root))
	file := *manager.GetSessionFile()
	lead := MemberRef{Team: sender.Ref.Team, ID: root, Generation: 1}
	request := peerRequest{Recipient: "team-lead", MessageID: "root-observation", Text: "hello lead", Summary: "greeting"}
	if _, _, err := store.acceptPeer(sender.Ref, request); err != nil {
		t.Fatal(err)
	}
	attempt := peerDeliveryAttempt{Host: peerHostIdentity{PID: 12345, Birth: "linux:1", Scope: "linux:fixture"}, SessionFile: file}
	receipt, content, found, err := store.offerPeerAttempt(lead, &attempt)
	if err != nil || !found {
		t.Fatalf("lead offer = %#v, found=%v, %v", receipt, found, err)
	}
	entryID, err := manager.AppendCustomMessageEntry("pig_litter_peer", content, true, map[string]any{"envelope": receipt.Envelope})
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, defaultConfig())
	tree.root.id = root
	state := &extensionState{}
	ext := newExtension(state)
	ext.OnEvent("probe_setup", func(ctx sdk.Context, _ map[string]any) (any, error) {
		state.mu.Lock()
		defer state.mu.Unlock()
		state.calls = newCallScope()
		o := &owner{state: state, epoch: state.epoch, sessionID: string(root), context: ctx, tree: tree, teamStore: store, rootCalls: state.calls}
		state.owner = o
		return nil, nil
	})
	host, handlers, finish := startExtensionFixture(t, ext, &subprocess.ReadyPayload{Cwd: workspace.Cwd, Mode: "tui", Width: 80, State: &subprocess.StatePayload{}})
	defer finish()
	requestEvent := func(event string) {
		t.Helper()
		writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgRequest, ID: event, Request: &subprocess.RequestPayload{Method: "event", Event: event, HandlerID: handlers[event], Args: json.RawMessage(`{}`)}})
		for {
			frame := readCompletionEnvelope(t, host)
			if frame.Type == subprocess.MsgResponse && frame.ID == event {
				if frame.Response.Error != nil {
					t.Fatal(frame.Response.Error)
				}
				return
			}
			if frame.Type != subprocess.MsgCall {
				continue
			}
			var query struct{ Method string }
			_ = json.Unmarshal(frame.Call.Args, &query)
			var value any = string(root)
			if query.Method == "getSessionFile" {
				value = file
			}
			data, _ := json.Marshal(value)
			writeCompletionEnvelope(t, host, subprocess.Envelope{Type: subprocess.MsgCallResult, ID: frame.ID, CallResult: &subprocess.CallResultPayload{Result: data}})
		}
	}
	requestEvent("probe_setup")
	state.mu.Lock()
	state.epoch++
	state.mu.Unlock()
	requestEvent("turn_start")
	key, _ := peerKey(receipt.Envelope)
	_, path := peerPaths(store.dir, key)
	if got, err := readPeerReceipt(path); err != nil || got.State != peerAttempted {
		t.Fatalf("stale root observed receipt = %#v, %v", got, err)
	}
	state.mu.Lock()
	state.epoch--
	state.mu.Unlock()
	requestEvent("turn_start")
	if got, err := readPeerReceipt(path); err != nil || got.State != peerObserved || got.NativeEntryID != entryID || got.SessionID != root {
		t.Fatalf("current root observation = %#v, %v", got, err)
	}
	state.mu.Lock()
	state.owner = nil
	state.mu.Unlock()
}
