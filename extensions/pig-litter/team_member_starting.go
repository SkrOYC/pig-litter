package piglitter

import (
	"crypto/rand"
	"encoding/json"
	"net/http"
	"slices"
)

type memberStartingTransfer struct {
	ID      string `json:"id"`
	Mode    string `json:"mode"`
	Text    string `json:"text"`
	request message
	result  *memberStartingResult
}

type memberStartingHandoff struct {
	window    string
	transfers []memberStartingTransfer
	installed bool
}

type memberStartingWindow struct {
	Window string `json:"window"`
}

type memberStartingInstallation struct {
	Window    string   `json:"window"`
	Transfers []string `json:"transfers"`
}

type memberStartingResult struct {
	Window      string `json:"window"`
	Transfer    string `json:"transfer"`
	Disposition string `json:"disposition"`
	Error       string `json:"error,omitempty"`
}

func (queue *memberControlQueue) serveStartingInput(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case "/input/start":
		var request memberStartingWindow
		if decodeMemberControl(w, r, &request) != nil {
			http.Error(w, "invalid starting window", http.StatusBadRequest)
			return
		}
		transfers, err := queue.beginStarting(request.Window)
		if err != nil {
			http.Error(w, "starting transfer is unavailable", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(transfers)
	case "/input/installed":
		var request memberStartingInstallation
		if decodeMemberControl(w, r, &request) != nil || queue.installStarting(request) != nil {
			http.Error(w, "starting installation is unavailable", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case "/input/ready":
		var request memberReadiness
		if err := decodeMemberControl(w, r, &request); err != nil {
			http.Error(w, "invalid native readiness", http.StatusBadRequest)
			return
		}
		if err := queue.acceptReadiness(r.Context(), request); err != nil {
			http.Error(w, err.Error(), http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case "/input/result":
		var request memberStartingResult
		if decodeMemberControl(w, r, &request) != nil || queue.finishStarting(request) != nil {
			http.Error(w, "starting result is unavailable", http.StatusConflict)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
	case "/input/window", "/input/running-result", "/input/cancel-watch":
		queue.serveRunningInput(w, r)
	}
}

func (queue *memberControlQueue) beginStarting(window string) ([]memberStartingTransfer, error) {
	if window == "" || len(window) > 128 {
		return nil, reject("rejected", "invalid native input window")
	}
	o := queue.owner
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.binding == nil || !o.memberBindingCurrentLocked(queue.binding) || queue.run.state != Starting || queue.run.ctx.Err() != nil || queue.record.paused {
		return nil, reject("unavailable", "native startup is no longer current")
	}
	select {
	case <-queue.closed:
		return nil, reject("unavailable", "native control is closed")
	case <-queue.cancelled:
		return nil, reject("cancelled", "native control is cancelled")
	case <-queue.run.ready:
		return nil, reject("unavailable", "native startup already activated")
	default:
	}
	if queue.starting != nil {
		if queue.starting.window != window {
			return nil, reject("unavailable", "native startup belongs to another window")
		}
		return slices.Clone(queue.starting.transfers), nil
	}
	queue.run.messageMu.Lock()
	defer queue.run.messageMu.Unlock()
	handoff := &memberStartingHandoff{window: window}
	for {
		select {
		case request := <-queue.run.mailbox:
			handoff.transfers = append(handoff.transfers, memberStartingTransfer{ID: rand.Text(), Mode: request.mode, Text: request.text, request: request})
		default:
			queue.run.nativeFrozen = true
			queue.run.nativeThaw = make(chan struct{})
			queue.starting = handoff
			queue.window, queue.windowSeq = window, 1
			queue.windows[window] = 1
			return slices.Clone(handoff.transfers), nil
		}
	}
}

func (queue *memberControlQueue) installStarting(request memberStartingInstallation) error {
	queue.owner.tree.mu.Lock()
	defer queue.owner.tree.mu.Unlock()
	queue.mu.Lock()
	select {
	case <-queue.closed:
		queue.mu.Unlock()
		return reject("unavailable", "native control is closed")
	case <-queue.cancelled:
		queue.mu.Unlock()
		return reject("cancelled", "native control is cancelled")
	default:
	}
	handoff := queue.starting
	if handoff == nil || handoff.window != request.Window || len(handoff.transfers) != len(request.Transfers) {
		queue.mu.Unlock()
		return reject("unavailable", "native starting transfer changed")
	}
	for index, transfer := range handoff.transfers {
		if transfer.ID != request.Transfers[index] {
			queue.mu.Unlock()
			return reject("unavailable", "native starting transfer IDs do not match")
		}
	}
	first := !handoff.installed
	handoff.installed = true
	activate := first && queue.binding != nil && queue.binding.ready && queue.run.peerWake != nil && queue.owner.memberBindingCurrentLocked(queue.binding) && !queue.record.paused
	queue.mu.Unlock()
	if activate {
		queue.activateReadyLocked()
	}
	return nil
}

func (queue *memberControlQueue) finishStarting(request memberStartingResult) error {
	if len(request.Error) > 1024 {
		return reject("rejected", "native input result is too large")
	}
	switch request.Disposition {
	case "queued", "handled", "cancelled", "stale", "unavailable", "failed":
	default:
		return reject("rejected", "invalid native input result")
	}
	o := queue.owner
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.binding == nil || !o.memberBindingCurrentLocked(queue.binding) {
		return reject("unavailable", "native execution changed")
	}
	select {
	case <-queue.closed:
		return reject("unavailable", "native control is closed")
	case <-queue.cancelled:
		return reject("cancelled", "native control is cancelled")
	default:
	}
	handoff := queue.starting
	if handoff == nil || !handoff.installed || handoff.window != request.Window {
		return reject("unavailable", "native starting window changed")
	}
	for index := range handoff.transfers {
		transfer := &handoff.transfers[index]
		if transfer.ID != request.Transfer {
			continue
		}
		if transfer.result != nil {
			if *transfer.result != request {
				return reject("unavailable", "native starting result conflicts with its first reply")
			}
			return nil
		}
		result := request
		transfer.result = &result
		var err error
		if request.Disposition != "queued" && request.Disposition != "handled" {
			err = reject("unavailable", "native teammate did not accept its starting message")
		}
		queue.finishStartingTransfer(transfer, err)
		return nil
	}
	return reject("unavailable", "native starting transfer is unknown")
}

func (queue *memberControlQueue) finishStartingTransfer(transfer *memberStartingTransfer, err error) {
	queue.run.messageMu.Lock()
	if queue.run.pendingMessages > 0 {
		queue.run.pendingMessages--
	}
	queue.run.messageMu.Unlock()
	transfer.request.result <- err
}

func (queue *memberControlQueue) failStarting() {
	queue.owner.tree.mu.Lock()
	defer queue.owner.tree.mu.Unlock()
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.starting == nil {
		return
	}
	if queue.run.nativeFrozen {
		queue.run.nativeInputErr = "native teammate stopped during starting transfer"
		queue.run.nativeFrozen = false
		close(queue.run.nativeThaw)
		queue.run.nativeThaw = nil
	}
	for index := range queue.starting.transfers {
		transfer := &queue.starting.transfers[index]
		if transfer.result != nil {
			continue
		}
		transfer.result = &memberStartingResult{Window: queue.starting.window, Transfer: transfer.ID, Disposition: "unavailable"}
		queue.finishStartingTransfer(transfer, reject("unavailable", "native teammate stopped during starting transfer"))
	}
}
