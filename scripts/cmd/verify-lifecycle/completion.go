package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func (f *fixture) releaseTask(task string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	gate := f.held[task]
	if gate == nil {
		return fmt.Errorf("provider stream %q is not held", task)
	}
	delete(f.held, task)
	close(gate)
	f.signalLocked()
	return nil
}

func completionInRequest(body modelRequest, run ref, task string) bool {
	for _, message := range body.Messages {
		if message.Role != "user" {
			continue
		}
		var completion struct {
			Origin           string `json:"origin"`
			Kind             string `json:"kind"`
			ReportProvenance string `json:"reportProvenance"`
			ID               string `json:"id"`
			State            string `json:"state"`
			Model            string `json:"model"`
			Report           string `json:"report"`
		}
		if json.Unmarshal([]byte(content(message.Content)), &completion) == nil && completion.Origin == "pig-litter" && completion.Kind == "litter_completion" && completion.ID == run.ID && completion.State == "completed" && completion.Model == "fixture/completion_child" && completion.ReportProvenance == "child_assistant" && strings.HasPrefix(completion.Report, "completed "+task+" ") {
			return true
		}
	}
	return false
}

func (f *fixture) parentCompletion(ctx context.Context, w http.ResponseWriter, body modelRequest) (string, []call, error) {
	tools, err := toolResults(body)
	if err != nil {
		return "", nil, err
	}
	latest := latestUserText(body)
	user := userText(body)
	runs := f.completionRunRefs()
	switch f.stage {
	case 0:
		f.stage = 1
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "busy-child", "model": "fixture/completion_child"}}}, nil
	case 1:
		if len(tools) != 1 || tools[0].Child.ID == "" {
			return "", nil, errors.New("busy completion child was not admitted")
		}
		f.mu.Lock()
		f.runs = append(f.runs, tools[0].Child.ref)
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return f.held["busy-child"] != nil }); err != nil {
			return "", nil, err
		}
		f.stage = 2
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := fmt.Fprint(w, ": busy parent stream held\n\n"); err != nil {
			return "", nil, err
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return "", nil, err
		}
		_ = f.hold(ctx, "busy-parent")
		return "BUSY_FIRST_ANSWER", nil, ctx.Err()
	case 2:
		if len(tools) != 1 || !completionInRequest(body, runs[0], "busy-child") {
			return "", nil, errors.New("busy parent model request did not contain its child's completion report")
		}
		f.mu.Lock()
		f.checks["busyCompletionInModelRequest"] = true
		f.mu.Unlock()
		f.stage = 3
		return "BUSY_COMPLETION_PROCESSED", nil, nil
	case 3:
		user := content(body.Messages[len(body.Messages)-1].Content)
		if user != "IDLE_BEGIN" || len(tools) != 1 {
			return "", nil, errors.New("unexpected prompt after busy completion")
		}
		f.stage = 4
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "idle-child", "model": "fixture/completion_child"}}}, nil
	case 4:
		if len(tools) != 2 || tools[1].Child.ID == "" {
			return "", nil, errors.New("idle completion child was not admitted")
		}
		f.mu.Lock()
		f.runs = append(f.runs, tools[1].Child.ref)
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return f.held["idle-child"] != nil }); err != nil {
			return "", nil, err
		}
		f.stage = 5
		return "IDLE_READY", nil, nil
	case 5:
		if latest != "CLEAR_BUSY_BEGIN" {
			return "", nil, fmt.Errorf("unexpected prompt in completion stage %d: %q", f.stage, latest)
		}
		f.stage = 6
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "clear-child", "name": "clear-child", "model": "fixture/completion_child"}}}, nil
	case 6:
		if len(tools) < 3 || tools[len(tools)-1].Child.ID == "" || tools[len(tools)-1].Child.Name != "clear-child" {
			return "", nil, errors.New("clear-queue completion child was not admitted")
		}
		f.mu.Lock()
		f.runs = append(f.runs, tools[len(tools)-1].Child.ref)
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return f.held["clear-child"] != nil }); err != nil {
			return "", nil, fmt.Errorf("clear-queue child provider request: %w", err)
		}
		f.stage = 7
		w.Header().Set("Content-Type", "text/event-stream")
		if _, err := fmt.Fprint(w, ": clear-queue parent stream held\n\n"); err != nil {
			return "", nil, err
		}
		if err := http.NewResponseController(w).Flush(); err != nil {
			return "", nil, err
		}
		_ = f.hold(ctx, "clear-parent")
		return "CLEAR_PARENT_ABORTED", nil, nil
	case 7:
		if !strings.Contains(user, "AFTER_CLEAR_ABORT") {
			return "", nil, fmt.Errorf("unexpected prompt after clear_queue and abort: %q", latest)
		}
		if len(runs) != 3 {
			return "", nil, errors.New("clear-queue child ID was not retained")
		}
		f.stage = 8
		return "", []call{
			{"litter_inspect", with(runs[2])},
			{"litter_spawn", map[string]any{"type": "scout", "task": "capacity-after-clear", "name": "capacity-after-clear", "model": "fixture/completion_child"}},
		}, nil
	case 8:
		results, err := trailingToolResults(body, 2)
		if err != nil {
			return "", nil, err
		}
		reportMatches := strings.HasPrefix(results[0].Outcome.Text, "completed clear-child ")
		if results[0].Child.ID != runs[2].ID || results[0].Child.State != "paused" || results[0].Outcome.Run != runs[2] || results[0].Outcome.State != "completed" || !reportMatches {
			return "", nil, fmt.Errorf("queue clear changed retained result: child=%s/%s outcome=%s/%s report_matches=%t", results[0].Child.ID, results[0].Child.State, results[0].Outcome.Run.ID, results[0].Outcome.State, reportMatches)
		}
		if results[1].Child.ID == "" || results[1].Child.Name != "capacity-after-clear" {
			return "", nil, fmt.Errorf("completion capacity stayed reserved after queue clear and abort: %#v", results[1])
		}
		f.mu.Lock()
		f.runs = append(f.runs, results[1].Child.ref)
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return f.held["capacity-after-clear"] != nil }); err != nil {
			return "", nil, fmt.Errorf("capacity retry provider request: %w", err)
		}
		f.stage = 9
		return "CLEAR_ABORT_RECOVERED", nil, nil
	case 9:
		if strings.Contains(user, "WAIT_AFTER_CLEAR_ABORT") && len(runs) == 4 {
			f.stage = 11
			_ = f.hold(ctx, "wait-suppression-parent")
			return "", []call{{"litter_wait", with(runs[3], "timeoutMs", 10000)}}, nil
		}
		if completionInRequest(body, runs[2], "clear-child") {
			f.markCheck("clearCompletionRetainedAfterAbort")
			f.stage = 10
			return "CLEAR_COMPLETION_RECOVERED", nil, nil
		}
		return "", nil, fmt.Errorf("unexpected completion stage %d prompt %q", f.stage, latest)
	case 10:
		if strings.Contains(user, "WAIT_AFTER_CLEAR_ABORT") && len(runs) == 4 {
			f.stage = 11
			_ = f.hold(ctx, "wait-suppression-parent")
			return "", []call{{"litter_wait", with(runs[3], "timeoutMs", 10000)}}, nil
		}
		return "", nil, errors.New("clear completion was delivered more than once after abort")
	case 11:
		waited, err := lastToolResult(body)
		if err != nil || waited.State != "completed" || waited.Outcome.Run != runs[3] || !strings.HasPrefix(waited.Outcome.Text, "completed capacity-after-clear ") {
			return "", nil, fmt.Errorf("post-abort capacity child did not complete under its ID: %#v, %v", waited, err)
		}
		if completionInRequest(body, runs[3], "capacity-after-clear") {
			return "", nil, errors.New("successful wait did not suppress the completion notice queued during the root turn")
		}
		f.markCheck("completionRecoveredAfterQueueClearAndAbort")
		f.markCheck("waitSuppressesQueuedCompletionInference")
		f.stage = 12
		return "WAIT_AFTER_CLEAR_ABORT_DONE", nil, nil
	case 12:
		if latest != "BOUNDARY_ARRIVAL_BEGIN" {
			return "", nil, fmt.Errorf("unexpected completion boundary prompt %q", latest)
		}
		f.stage = 13
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "boundary-child", "name": "boundary-child", "model": "fixture/completion_child"}}}, nil
	case 13:
		spawned, err := lastToolResult(body)
		if err != nil || spawned.Child.ID == "" || spawned.Child.Name != "boundary-child" {
			return "", nil, fmt.Errorf("boundary child was not admitted: name=%q id=%q err=%v", spawned.Child.Name, spawned.Child.ID, err)
		}
		f.mu.Lock()
		f.runs = append(f.runs, spawned.Child.ref)
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return f.held["boundary-child"] != nil }); err != nil {
			return "", nil, fmt.Errorf("boundary child provider request: %w", err)
		}
		f.stage = 14
		return "BOUNDARY_CHILD_READY", nil, nil
	case 14:
		if len(runs) != 5 || !completionInRequest(body, runs[4], "boundary-child") {
			return "", nil, errors.New("automatic boundary continuation did not contain the child completion")
		}
		f.markCheck("completionDuringAgentBeforeSettleContinued")
		f.stage = 15
		return "BOUNDARY_COMPLETION_PROCESSED", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected completion fixture provider request at stage %d", f.stage)
}

func (f *fixture) completionChild(ctx context.Context, body modelRequest) (string, []call, error) {
	for _, message := range body.Messages {
		if message.Role != "user" {
			continue
		}
		for _, task := range []string{"busy-child", "idle-child", "clear-child", "capacity-after-clear", "boundary-child"} {
			if strings.Contains(content(message.Content), task) {
				return f.hold(ctx, task), nil, ctx.Err()
			}
		}
	}
	return "", nil, errors.New("unknown completion child task")
}

func waitCompletionDelivery(ctx context.Context, directory string, run ref) (completionDelivery, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		var delivery completionDelivery
		data, err := os.ReadFile(filepath.Join(directory, completionDeliveryFile(run)))
		if err == nil {
			if err := json.Unmarshal(data, &delivery); err != nil {
				return delivery, err
			}
			if len(delivery.Error) != 0 {
				return delivery, fmt.Errorf("host rejected completion delivery: %s", delivery.Error)
			}
			if delivery.Message.CustomType != "litter_completion" || delivery.Message.Display || delivery.Message.Details.ChildID != run.ID {
				return delivery, errors.New("completion lost its hidden display or child ID details")
			}
			return delivery, nil
		}
		if !os.IsNotExist(err) {
			return delivery, err
		}
		select {
		case <-ctx.Done():
			return delivery, fmt.Errorf("completion sendMessage acknowledgement: %w", ctx.Err())
		case <-tick.C:
		}
	}
}

func completionEvent(event map[string]any, run ref) bool {
	if event["type"] != "message_end" {
		return false
	}
	raw, _ := json.Marshal(event["message"])
	var delivery completionDelivery
	if json.Unmarshal(raw, &delivery.Message) != nil {
		return false
	}
	return delivery.Message.CustomType == "litter_completion" && !delivery.Message.Display && delivery.Message.Details.ChildID == run.ID
}

func waitCompletionTurn(ctx context.Context, f *fixture, events <-chan map[string]any, marker string) error {
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	seenMarker, seenAgentEnd := false, false
	for {
		select {
		case event, open := <-events:
			if !open {
				return fmt.Errorf("host exited before completion turn %q settled", marker)
			}
			if event["type"] == "response" && event["success"] == false {
				return fmt.Errorf("host rejected a completion command: %v", event)
			}
			if event["type"] == "message_end" {
				raw, _ := json.Marshal(event["message"])
				seenMarker = seenMarker || strings.Contains(string(raw), marker)
			}
			if seenMarker && event["type"] == "agent_end" {
				seenAgentEnd = true
			}
			if seenAgentEnd && event["type"] == "agent_settled" {
				return nil
			}
		case <-ticker.C:
			f.mu.Lock()
			failure := f.failure
			f.mu.Unlock()
			if failure != nil {
				return failure
			}
		case <-ctx.Done():
			return fmt.Errorf("completion turn %q did not settle: %w", marker, ctx.Err())
		}
	}
}

func (f *fixture) completionRunRefs() []ref {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]ref(nil), f.runs...)
}

func (f *fixture) countCompletionRequests(run ref, task string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	count := 0
	for _, request := range f.requests {
		if request.Model == "parent_completion" && completionInRequest(request, run, task) {
			count++
		}
	}
	return count
}

func (f *fixture) runCompletion(ctx context.Context, directory string, send func(any) error, wait func(string, string) error, waitToolExecution func(string, string) error, events <-chan map[string]any, boundaryGateReady <-chan boundaryGateConnection) error {
	if err := send(map[string]any{"id": "busy", "type": "prompt", "message": "BUSY_BEGIN. PARENT_PRIVATE_MARKER"}); err != nil {
		return err
	}
	if err := f.wait(ctx, func() bool { return len(f.runs) == 1 && f.held["busy-parent"] != nil }); err != nil {
		return fmt.Errorf("busy parent provider stream: %w", err)
	}
	runs := f.completionRunRefs()
	if err := f.releaseTask("busy-child"); err != nil {
		return err
	}
	busy, err := waitCompletionDelivery(ctx, directory, runs[0])
	if err != nil {
		return err
	}
	if !hiddenCompletionOptions(busy.Options) {
		return fmt.Errorf("busy completion options = %#v, want hidden native delivery without a turn trigger", busy.Options)
	}
	if err := f.releaseTask("busy-parent"); err != nil {
		return err
	}
	answerCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	processed, hidden, settled := false, false, false
	for !settled {
		select {
		case event, open := <-events:
			if !open {
				return errors.New("host exited before busy completion produced its next answer")
			}
			hidden = hidden || completionEvent(event, runs[0])
			if event["type"] == "response" && event["success"] == false {
				return fmt.Errorf("host rejected completion command: %v", event)
			}
			if event["type"] == "message_end" {
				raw, _ := json.Marshal(event["message"])
				processed = processed || strings.Contains(string(raw), "BUSY_COMPLETION_PROCESSED")
			}
			settled = processed && event["type"] == "agent_settled"
		case <-answerCtx.Done():
			return fmt.Errorf("busy completion did not produce another answer without a user prompt: %w", answerCtx.Err())
		}
	}
	if !hidden {
		return errors.New("busy parent did not receive its unconsumed hidden completion without a new prompt")
	}
	f.mu.Lock()
	f.checks["busyCompletionProducedAnswerWithoutPrompt"] = true
	f.mu.Unlock()
	if err := send(map[string]any{"id": "idle", "type": "prompt", "message": "IDLE_BEGIN"}); err != nil {
		return err
	}
	if err := waitCompletionTurn(ctx, f, events, "IDLE_READY"); err != nil {
		return err
	}
	f.mu.Lock()
	requestCount := len(f.requests)
	f.mu.Unlock()
	if err := f.releaseTask("idle-child"); err != nil {
		return err
	}
	runs = f.completionRunRefs()
	idle, err := waitCompletionDelivery(ctx, directory, runs[1])
	if err != nil {
		return err
	}
	if !hiddenCompletionOptions(idle.Options) {
		return fmt.Errorf("idle completion options = %#v, want hidden native delivery without a turn trigger", idle.Options)
	}
	if err := send(map[string]any{"id": "idle-state", "type": "get_state"}); err != nil {
		return err
	}
	stateCtx, stateCancel := context.WithTimeout(ctx, 10*time.Second)
	defer stateCancel()
	hidden = false
	verifiedIdle := false
	idleVerified := false
	for !idleVerified {
		select {
		case event, open := <-events:
			if !open {
				return errors.New("host exited before idle completion verification")
			}
			hidden = hidden || completionEvent(event, runs[1])
			if event["type"] == "response" && event["id"] == "idle-state" {
				data, _ := event["data"].(map[string]any)
				if event["success"] != true || data["isStreaming"] != false || data["pendingMessageCount"] != float64(0) {
					return errors.New("idle completion was not appended while the parent stayed idle")
				}
				verifiedIdle = true
			}
			if !hidden || !verifiedIdle {
				continue
			}
			f.mu.Lock()
			failure, currentRequestCount := f.failure, len(f.requests)
			if failure == nil && currentRequestCount == requestCount {
				f.checks["idleCompletionAppendedWithoutInference"] = true
			}
			f.mu.Unlock()
			if failure != nil {
				return failure
			}
			if currentRequestCount != requestCount {
				return errors.New("idle completion caused an extra provider request")
			}
			idleVerified = true
		case <-stateCtx.Done():
			return fmt.Errorf("idle completion state: %w", stateCtx.Err())
		}
	}

	if err := send(map[string]any{"id": "clear-busy", "type": "prompt", "message": "CLEAR_BUSY_BEGIN"}); err != nil {
		return err
	}
	if err := f.wait(ctx, func() bool {
		return f.held["clear-child"] != nil && f.held["clear-parent"] != nil
	}); err != nil {
		return fmt.Errorf("clear-queue parent stream: %w", err)
	}
	runs = f.completionRunRefs()
	if len(runs) != 3 {
		return errors.New("clear-queue child did not retain its native session ID")
	}
	if err := f.releaseTask("clear-child"); err != nil {
		return err
	}
	cleared, err := waitCompletionDelivery(ctx, directory, runs[2])
	if err != nil {
		return err
	}
	if !hiddenCompletionOptions(cleared.Options) {
		return fmt.Errorf("clear-queue completion options = %#v, want hidden native delivery without a turn trigger", cleared.Options)
	}
	if err := send(map[string]any{"id": "clear-queued-completion", "type": "clear_queue"}); err != nil {
		return err
	}
	if err := wait("", "clear-queued-completion"); err != nil {
		return err
	}
	if err := send(map[string]any{"id": "abort-clear-completion", "type": "abort"}); err != nil {
		return err
	}
	if err := wait("", "abort-clear-completion"); err != nil {
		return err
	}
	if err := f.wait(ctx, func() bool { return f.aborted["clear-parent"] && f.held["clear-parent"] == nil }); err != nil {
		return fmt.Errorf("clear-queue root abort did not cancel the active parent request: %w", err)
	}
	if err := send(map[string]any{"id": "after-clear-abort", "type": "prompt", "message": "AFTER_CLEAR_ABORT"}); err != nil {
		return err
	}
	if err := waitCompletionTurn(ctx, f, events, "CLEAR_ABORT_RECOVERED"); err != nil {
		return err
	}
	runs = f.completionRunRefs()
	if len(runs) != 4 {
		return errors.New("capacity retry did not retain its child ID after queue clear and abort")
	}
	f.mu.Lock()
	requestCount = len(f.requests)
	f.mu.Unlock()
	if err := send(map[string]any{"id": "wait-after-clear-abort", "type": "prompt", "message": "WAIT_AFTER_CLEAR_ABORT"}); err != nil {
		return err
	}
	if err := f.wait(ctx, func() bool { return f.held["wait-suppression-parent"] != nil }); err != nil {
		return fmt.Errorf("wait-suppression provider stream: %w", err)
	}
	if err := f.releaseTask("capacity-after-clear"); err != nil {
		return err
	}
	capacityDelivery, err := waitCompletionDelivery(ctx, directory, runs[3])
	if err != nil {
		return err
	}
	if !hiddenCompletionOptions(capacityDelivery.Options) {
		return fmt.Errorf("capacity retry completion options = %#v, want hidden native delivery without a turn trigger", capacityDelivery.Options)
	}
	if err := f.releaseTask("wait-suppression-parent"); err != nil {
		return err
	}
	if err := waitToolExecution("litter_wait", runs[3].ID); err != nil {
		return fmt.Errorf("root did not wait after the completion notice was queued: %w", err)
	}
	if err := waitCompletionTurn(ctx, f, events, "WAIT_AFTER_CLEAR_ABORT_DONE"); err != nil {
		return err
	}
	if err := send(map[string]any{"id": "completion-final-state", "type": "get_state"}); err != nil {
		return err
	}
	if err := waitCompletionState(ctx, events, "completion-final-state"); err != nil {
		return err
	}
	f.mu.Lock()
	failure, finalRequestCount := f.failure, len(f.requests)
	if failure == nil && finalRequestCount == requestCount+2 {
		f.checks["completionQueuedWaitSuppressedDuplicateInference"] = true
	}
	f.mu.Unlock()
	if failure != nil {
		return failure
	}
	if finalRequestCount != requestCount+2 {
		return fmt.Errorf("successful completion wait caused extra provider requests: before=%d after=%d", requestCount, finalRequestCount)
	}
	boundaryArm := filepath.Join(directory, "boundary-gate-arm")
	if err := os.WriteFile(boundaryArm, []byte("armed"), 0o600); err != nil {
		return err
	}
	if err := send(map[string]any{"id": "boundary-completion", "type": "prompt", "message": "BOUNDARY_ARRIVAL_BEGIN"}); err != nil {
		return err
	}
	if err := f.wait(ctx, func() bool { return f.held["boundary-child"] != nil }); err != nil {
		return fmt.Errorf("boundary child provider request: %w", err)
	}
	var gate boundaryGateConnection
	select {
	case gate = <-boundaryGateReady:
		if gate.err != nil {
			return fmt.Errorf("agent_before_settle gate: %w", gate.err)
		}
		if gate.conn == nil {
			return errors.New("agent_before_settle gate returned no connection")
		}
	case <-ctx.Done():
		return fmt.Errorf("agent_before_settle gate was not reached: %w", ctx.Err())
	}
	runs = f.completionRunRefs()
	if len(runs) != 5 {
		return errors.New("boundary child did not retain its native session ID")
	}
	var boundaryEvidence struct {
		Event   string `json:"event"`
		Outcome string `json:"outcome"`
	}
	data, err := os.ReadFile(filepath.Join(directory, "boundary-gate-entry.json"))
	if err != nil {
		return fmt.Errorf("read boundary gate evidence: %w", err)
	}
	if err := json.Unmarshal(data, &boundaryEvidence); err != nil || boundaryEvidence.Event != "agent_before_settle" || boundaryEvidence.Outcome != "completed" {
		return fmt.Errorf("boundary gate evidence = %s/%s, err=%v", boundaryEvidence.Event, boundaryEvidence.Outcome, err)
	}
	if err := f.releaseTask("boundary-child"); err != nil {
		return err
	}
	boundaryDelivery, err := waitCompletionDelivery(ctx, directory, runs[4])
	if err != nil {
		return fmt.Errorf("boundary child completion acknowledgement: %w", err)
	}
	if !hiddenCompletionOptions(boundaryDelivery.Options) {
		return fmt.Errorf("boundary completion options = %#v, want hidden native delivery without a turn trigger", boundaryDelivery.Options)
	}
	if _, err := fmt.Fprintln(gate.conn, "release"); err != nil {
		_ = gate.conn.Close()
		return fmt.Errorf("release agent_before_settle gate: %w", err)
	}
	if err := gate.conn.Close(); err != nil {
		return err
	}
	boundaryCtx, boundaryCancel := context.WithTimeout(ctx, 10*time.Second)
	defer boundaryCancel()
	if err := waitCompletionTurn(boundaryCtx, f, events, "BOUNDARY_COMPLETION_PROCESSED"); err != nil {
		return err
	}
	if count := f.countRequests("parent_completion", "BOUNDARY_ARRIVAL_BEGIN"); count != 3 {
		return fmt.Errorf("boundary prompt produced %d parent requests, want the initial call, spawn result, and one continuation", count)
	}
	if count := f.countCompletionRequests(runs[4], "boundary-child"); count != 1 {
		return fmt.Errorf("boundary child report appeared in %d parent requests, want exactly one", count)
	}
	f.markCheck("completionDuringAgentBeforeSettleContinuedWithoutPrompt")
	if err := send(map[string]any{"id": "completion-boundary-final-state", "type": "get_state"}); err != nil {
		return err
	}
	return waitCompletionState(ctx, events, "completion-boundary-final-state")
}

func hiddenCompletionOptions(options sdk.SendMessageOptions) bool {
	return options.TriggerTurn != nil && !*options.TriggerTurn && options.DeliverAs == ""
}

func waitCompletionState(ctx context.Context, events <-chan map[string]any, id string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		select {
		case event, open := <-events:
			if !open {
				return fmt.Errorf("host exited before state response %q", id)
			}
			if event["type"] != "response" || event["id"] != id {
				continue
			}
			data, _ := event["data"].(map[string]any)
			if event["success"] != true || data["isStreaming"] != false || data["pendingMessageCount"] != float64(0) {
				return fmt.Errorf("completion root did not settle cleanly: %v", event)
			}
			return nil
		case <-ctx.Done():
			return fmt.Errorf("state response %q: %w", id, ctx.Err())
		}
	}
}
