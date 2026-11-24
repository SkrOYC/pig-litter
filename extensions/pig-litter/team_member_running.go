package piglitter

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"sync"
)

type memberWindowPublication struct {
	Window   string `json:"window"`
	Sequence uint64 `json:"sequence"`
}

type memberRunningInput struct {
	input      memberInputWire
	result     *memberStartingResult
	interim    *memberStartingResult
	notify     chan memberStartingResult
	done       chan struct{}
	cancelled  chan struct{}
	cancelOnce sync.Once
	stopCancel func() bool
}

func (queue *memberControlQueue) serveRunningInput(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/input/window":
		var request memberWindowPublication
		if decodeMemberControl(w, r, &request) != nil || queue.publishRunningWindow(request) != nil {
			http.Error(w, "native input window is unavailable", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case "/input/running-result":
		var request memberStartingResult
		if decodeMemberControl(w, r, &request) != nil || queue.finishRunningInput(request) != nil {
			http.Error(w, "native input result is unavailable", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case "/input/cancel-watch":
		var request memberStartingResult
		if decodeMemberControl(w, r, &request) != nil || request.Disposition != "" || request.Error != "" {
			http.Error(w, "invalid native input cancellation watch", http.StatusBadRequest)
			return
		}
		queue.mu.Lock()
		input := queue.running[request.Transfer]
		if input == nil || input.input.Window != request.Window {
			queue.mu.Unlock()
			http.Error(w, "native input cancellation watch is unavailable", http.StatusConflict)
			return
		}
		queue.mu.Unlock()
		select {
		case <-input.cancelled:
			_ = json.NewEncoder(w).Encode(map[string]bool{"cancel": true})
		case <-input.done:
			_ = json.NewEncoder(w).Encode(map[string]bool{"cancel": false})
		case <-queue.closed:
			_ = json.NewEncoder(w).Encode(map[string]bool{"cancel": true})
		case <-queue.cancelled:
			_ = json.NewEncoder(w).Encode(map[string]bool{"cancel": true})
		case <-r.Context().Done():
		}
	}
}

func (queue *memberControlQueue) publishRunningWindow(request memberWindowPublication) error {
	if request.Window == "" || len(request.Window) > 128 || request.Sequence < 2 {
		return reject("rejected", "native input window is invalid")
	}
	o := queue.owner
	o.tree.lifecycleMu.Lock()
	defer o.tree.lifecycleMu.Unlock()
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	queue.mu.Lock()
	defer queue.mu.Unlock()
	select {
	case <-queue.closed:
		return reject("unavailable", "native control is closed")
	case <-queue.cancelled:
		return reject("cancelled", "native control is cancelled")
	default:
	}
	if queue.binding == nil || !queue.binding.ready || !o.memberBindingCurrentLocked(queue.binding) || queue.run.ctx.Err() != nil || queue.record.paused {
		return reject("unavailable", "native execution is unavailable")
	}
	if sequence, exists := queue.windows[request.Window]; exists {
		if sequence == request.Sequence && queue.windowSeq == sequence && queue.window == request.Window {
			return nil
		}
		return reject("unavailable", "native input window ID was reused")
	}
	if request.Sequence <= queue.windowSeq {
		return reject("unavailable", "native input window is stale")
	}
	queue.windows[request.Window] = request.Sequence
	queue.window, queue.windowSeq = request.Window, request.Sequence
	return nil
}

func (queue *memberControlQueue) queueText(ctx context.Context, value memberTextInput) (memberInputDisposition, error) {
	input, err := queue.beginText(ctx, value)
	if err != nil {
		return memberInputUncertain, err
	}
	return queue.awaitText(ctx, input)
}

func (queue *memberControlQueue) beginText(ctx context.Context, value memberTextInput) (*memberRunningInput, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	queue.mu.Lock()
	if queue.window == "" {
		queue.mu.Unlock()
		return nil, reject("unavailable", "native input window is unavailable")
	}
	input := &memberRunningInput{input: memberInputWire{Window: queue.window, Transfer: rand.Text(), Mode: value.mode, Text: value.text}, done: make(chan struct{}), cancelled: make(chan struct{}), notify: make(chan memberStartingResult, 1)}
	if cause, ok := ctx.Value(memberCauseKey{}).(memberCause); ok && cause.queue == queue {
		input.input.Cause = cause.id
	}
	queue.running[input.input.Transfer] = input
	queue.mu.Unlock()
	input.stopCancel = context.AfterFunc(ctx, func() { input.cancelOnce.Do(func() { close(input.cancelled) }) })
	reply, dispatched, err := queue.requestCommand(ctx, memberControlCommand{Kind: "input", Input: &input.input}, true)
	if err != nil {
		input.stopCancel()
		if !dispatched || reply.Error != "" {
			queue.mu.Lock()
			delete(queue.running, input.input.Transfer)
			queue.mu.Unlock()
			return nil, reject("unavailable", "native teammate did not accept the input request")
		}
		input.cancelOnce.Do(func() { close(input.cancelled) })
		return nil, reject("unavailable", "native input result is uncertain")
	}
	return input, nil
}

func (queue *memberControlQueue) awaitText(ctx context.Context, input *memberRunningInput) (memberInputDisposition, error) {
	defer input.stopCancel()
	select {
	case result := <-input.notify:
		return memberRunningDisposition(&result)
	default:
	}
	select {
	case result := <-input.notify:
		return memberRunningDisposition(&result)
	case <-ctx.Done():
		input.cancelOnce.Do(func() { close(input.cancelled) })
		return memberInputUncertain, reject("cancelled", "native input result is uncertain")
	case <-queue.closed:
		return memberInputUncertain, reject("unavailable", "native input result is uncertain")
	case <-queue.cancelled:
		return memberInputUncertain, reject("cancelled", "native input result is uncertain")
	}
}

func memberRunningDisposition(result *memberStartingResult) (memberInputDisposition, error) {
	if result == nil {
		return memberInputUncertain, reject("unavailable", "native input result is unavailable")
	}
	switch result.Disposition {
	case "queued":
		return memberInputQueued, nil
	case "handled":
		return memberInputHandled, nil
	case "would_block":
		return memberInputUncertain, reject("unavailable", "native input remains pending because this callback cannot await it")
	default:
		return memberInputUncertain, reject("unavailable", "native teammate did not accept the message")
	}
}

func (queue *memberControlQueue) finishRunningInput(request memberStartingResult) error {
	if len(request.Error) > 1024 {
		return reject("rejected", "native input result is too large")
	}
	switch request.Disposition {
	case "queued", "handled", "cancelled", "stale", "unavailable", "failed", "would_block":
	default:
		return reject("rejected", "invalid native input result")
	}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	select {
	case <-queue.closed:
		return reject("unavailable", "native control is closed")
	case <-queue.cancelled:
		return reject("cancelled", "native control is cancelled")
	default:
	}
	input := queue.running[request.Transfer]
	if input == nil || input.input.Window != request.Window {
		return reject("unavailable", "native input identity changed")
	}
	if request.Disposition == "would_block" {
		if input.interim != nil {
			if *input.interim != request {
				return reject("unavailable", "native input interim result conflicts with its first reply")
			}
			return nil
		}
		result := request
		input.interim = &result
		input.notify <- result
		return nil
	}
	if input.result != nil {
		if *input.result != request {
			return reject("unavailable", "native input result conflicts with its first reply")
		}
		return nil
	}
	result := request
	input.result = &result
	select {
	case input.notify <- result:
	default:
	}
	close(input.done)
	return nil
}

func (queue *memberControlQueue) deliverInputs() {
	for {
		select {
		case request := <-queue.run.mailbox:
			requestCtx := request.ctx
			if requestCtx == nil {
				requestCtx = queue.run.ctx
			}
			ctx, cancel := context.WithCancel(requestCtx)
			stop := context.AfterFunc(queue.run.ctx, cancel)
			input, err := queue.beginText(ctx, memberTextInput{mode: request.mode, text: request.text})
			go func(request message, ctx context.Context, input *memberRunningInput, err error, stop func() bool, cancel context.CancelFunc) {
				defer stop()
				defer cancel()
				disposition := memberInputUncertain
				if err == nil {
					disposition, err = queue.awaitText(ctx, input)
				}
				if err == nil && disposition != memberInputQueued && disposition != memberInputHandled {
					err = reject("unavailable", "native input result is uncertain")
				}
				request.result <- err
				queue.run.messageMu.Lock()
				if queue.run.pendingMessages > 0 {
					queue.run.pendingMessages--
				}
				queue.run.messageMu.Unlock()
			}(request, ctx, input, err, stop, cancel)
		case <-queue.run.ctx.Done():
			return
		case <-queue.closed:
			return
		case <-queue.cancelled:
			return
		}
	}
}

func (queue *memberControlQueue) failQueuedInputs() {
	queue.owner.tree.mu.Lock()
	defer queue.owner.tree.mu.Unlock()
	queue.run.nativeInputErr = "native teammate is unavailable"
	queue.run.messageMu.Lock()
	defer queue.run.messageMu.Unlock()
	for {
		select {
		case request := <-queue.run.mailbox:
			if queue.run.pendingMessages > 0 {
				queue.run.pendingMessages--
			}
			request.result <- reject("unavailable", "native teammate stopped before message delivery")
		default:
			return
		}
	}
}
