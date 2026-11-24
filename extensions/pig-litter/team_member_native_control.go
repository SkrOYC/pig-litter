//go:build p08native

package piglitter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/SkrOYC/pig-litter/extensions/pig-litter/memberinput"
)

func (member *nativeMember) controlRequest(ctx context.Context, path string, body []byte) (*http.Response, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://member"+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("X-Pig-Litter-Member", member.manifest.Capability)
	return member.client.Do(request)
}

func (member *nativeMember) startControl(nativeCtx context.Context, api extension.API) {
	member.mu.Lock()
	if member.control != nil {
		member.control()
	}
	ctx, cancel := context.WithCancel(context.Background())
	member.control = cancel
	member.mu.Unlock()
	go func() {
		response, err := member.controlRequest(ctx, "/control/watch", nil)
		if response != nil {
			response.Body.Close()
		}
		if ctx.Err() == nil && (err != nil || response.StatusCode != http.StatusOK) {
			member.stop(nativeCtx)
			return
		}
		if ctx.Err() == nil {
			member.stop(nativeCtx)
		}
	}()
	go func() {
		for {
			response, err := member.controlRequest(ctx, "/control/next", nil)
			if err != nil || response.StatusCode != http.StatusOK {
				if response != nil {
					response.Body.Close()
				}
				if ctx.Err() == nil {
					member.stop(nativeCtx)
				}
				return
			}
			var command memberControlCommand
			err = json.NewDecoder(io.LimitReader(response.Body, memberControlLimit)).Decode(&command)
			response.Body.Close()
			if err != nil || command.Kind == "cancel" {
				member.stop(nativeCtx)
				return
			}
			reply := member.applyControl(nativeCtx, api, command)
			data, err := json.Marshal(reply)
			if err != nil {
				member.stop(nativeCtx)
				return
			}
			for {
				response, err = member.controlRequest(ctx, "/control/result", data)
				if response != nil {
					response.Body.Close()
				}
				if err == nil && response.StatusCode == http.StatusNoContent {
					break
				}
				if command.Kind != "input" || err == nil || ctx.Err() != nil {
					if ctx.Err() == nil {
						member.stop(nativeCtx)
					}
					return
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(20 * time.Millisecond):
				}
			}
		}
	}()
}

func (member *nativeMember) applyControl(ctx context.Context, api extension.API, command memberControlCommand) (reply memberControlReply) {
	reply.ID, reply.Kind = command.ID, command.Kind
	defer func() {
		if recover() != nil {
			reply = memberControlReply{ID: command.ID, Kind: command.Kind, Error: "native control failed"}
		}
	}()
	switch command.Kind {
	case "snapshot":
		if command.History == nil {
			reply.Error = "native snapshot request is invalid"
			return reply
		}
		snapshot, err := member.snapshot(ctx, *command.History)
		if err != nil {
			reply.Error = err.Error()
		} else {
			reply.Snapshot = &snapshot
		}
	case "completion":
		if command.Completion == nil {
			reply.Error = "native completion request is invalid"
			return reply
		}
		value := command.Completion
		api.SendMessage(extension.SendMessagePayload{CustomType: "litter_completion", Content: value.Content, Details: value.Details}, &extension.SendMessageOptions{TriggerTurn: &value.Trigger, DeliverAs: extension.DeliverAs(value.Mode)})
		reply.Disposition = memberInputUncertain
	case "audit":
		if command.Audit == nil {
			reply.Error = "native audit request is invalid"
			return reply
		}
		api.AppendEntry("litter_completion", command.Audit)
	case "input":
		if command.Input == nil {
			reply.Error = "native input request is unavailable"
			return reply
		}
		value := command.Input
		window := memberinput.WindowID(value.Window)
		member.mu.Lock()
		windowCtx := member.windows[window]
		causeCtx := member.causes[value.Cause]
		member.mu.Unlock()
		if windowCtx == nil || value.Cause != "" && causeCtx == nil {
			reply.Error = "native input window is unavailable"
			return reply
		}
		inputCtx := windowCtx
		release := func() {}
		if value.Cause != "" {
			inputCtx, release = dependentMemberInputContext(windowCtx, causeCtx)
		}
		receipt, err := member.bridge.Admit(inputCtx, window, memberinput.TransferID(value.Transfer), memberinput.Input{Mode: memberinput.Mode(value.Mode), Text: value.Text})
		if err != nil {
			release()
			switch {
			case errors.Is(err, coding.ErrInputFull):
				reply.Error = "native input is full"
			case errors.Is(err, coding.ErrInputStale):
				reply.Error = "native input window is stale"
			default:
				reply.Error = "native input is unavailable"
			}
			return reply
		}
		go member.completeRunning(inputCtx, *value, receipt, release)
		go member.watchInputCancellation(windowCtx, ctx, *value, receipt)
	default:
		reply.Error = "native control operation is unavailable"
	}
	return reply
}

func dependentMemberInputContext(window, cause context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(cause))
	stop := context.AfterFunc(window, cancel)
	return ctx, func() { stop(); cancel() }
}

func (member *nativeMember) completeRunning(ctx context.Context, input memberInputWire, receipt *memberinput.Receipt, release func()) {
	defer release()
	defer member.fixtureReceiptBoundary(input, "/fixture/outer-closed")
	defer receipt.Close()
	defer member.fixtureReceiptBoundary(input, "/fixture/outer-closing")
	result, err := receipt.Await(ctx)
	if errors.Is(err, coding.ErrInputWouldBlock) {
		request := memberStartingResult{Window: input.Window, Transfer: input.Transfer, Disposition: "would_block"}
		resultCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		var reply map[string]bool
		_ = member.retry(resultCtx, "/input/running-result", request, &reply)
		cancel()
	}
	if err != nil {
		result, err = receipt.Await(context.Background())
	}
	if err != nil {
		return
	}
	request := memberStartingResult{Window: input.Window, Transfer: input.Transfer, Disposition: string(result.Disposition), Error: result.Error}
	resultCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var reply map[string]bool
	_ = member.retry(resultCtx, "/input/running-result", request, &reply)
}

func (member *nativeMember) watchInputCancellation(ctx, nativeCtx context.Context, input memberInputWire, receipt *memberinput.Receipt) {
	var result struct {
		Cancel bool `json:"cancel"`
	}
	err := member.call(ctx, "/input/cancel-watch", memberStartingResult{Window: input.Window, Transfer: input.Transfer}, &result)
	if err != nil && ctx.Err() == nil {
		receipt.Close()
		member.stop(nativeCtx)
		return
	}
	if result.Cancel {
		receipt.Close()
	}
}

func (member *nativeMember) snapshot(ctx context.Context, bound memberHistoryWire) (memberSnapshotWire, error) {
	var snapshot memberSnapshotWire
	if bound.Entries < 0 || bound.Entries > 200 || bound.Bytes < 0 || bound.Bytes > 64*1024 || (bound.Entries == 0) != (bound.Bytes == 0) {
		return snapshot, errors.New("native history bound is invalid")
	}
	ext := extension.FromContext(ctx)
	if ext == nil {
		return snapshot, errors.New("native context is unavailable")
	}
	manager, err := ext.SessionManager()
	if err != nil {
		return snapshot, err
	}
	cwd, err := ext.CWD()
	if err != nil {
		return snapshot, err
	}
	model, err := ext.Model()
	if err != nil {
		return snapshot, err
	}
	idle, err := ext.IsIdle()
	if err != nil {
		return snapshot, err
	}
	pending, err := ext.HasPendingMessages()
	if err != nil {
		return snapshot, err
	}
	phase, count := memberStreaming, 0
	if idle {
		phase = memberIdle
	}
	if pending {
		count = 1
	}
	snapshot = memberSnapshotWire{Session: ChildID(manager.GetSessionId()), Cwd: cwd, Model: nativeModelName(model), Tools: ext.GetActiveTools(), Phase: &phase, Pending: &count, Leaf: manager.GetLeafID()}
	if bound.Entries == 0 {
		return snapshot, nil
	}
	entries, err := rawEntries(manager.GetEntries())
	if err != nil {
		return snapshot, err
	}
	used := 0
	for index := len(entries) - 1; index >= 0; index-- {
		if len(snapshot.Entries) == bound.Entries || used+len(entries[index]) > bound.Bytes {
			snapshot.Clipped = true
			break
		}
		used += len(entries[index])
		snapshot.Entries = append(snapshot.Entries, entries[index])
	}
	for left, right := 0, len(snapshot.Entries)-1; left < right; left, right = left+1, right-1 {
		snapshot.Entries[left], snapshot.Entries[right] = snapshot.Entries[right], snapshot.Entries[left]
	}
	return snapshot, nil
}
