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
			ref
			State, Model, Report string
		}
		if json.Unmarshal([]byte(content(message.Content)), &completion) == nil && completion.ref == run && completion.State == "completed" && completion.Model == "fixture/completion_child" && strings.HasPrefix(completion.Report, "completed "+task+" ") {
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
		if len(tools) != 1 || !completionInRequest(body, f.runs[0], "busy-child") {
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
	}
	return "", nil, errors.New("idle completion unexpectedly triggered inference")
}

func (f *fixture) completionChild(ctx context.Context, body modelRequest) (string, []call, error) {
	for _, message := range body.Messages {
		if message.Role != "user" {
			continue
		}
		for _, task := range []string{"busy-child", "idle-child"} {
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
			if delivery.Message.CustomType != "litter_completion" || delivery.Message.Display || delivery.Message.Details.ChildID != run.ID || delivery.Message.Details.Generation != run.Generation {
				return delivery, errors.New("completion lost its hidden display or child generation details")
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
	return delivery.Message.CustomType == "litter_completion" && !delivery.Message.Display && delivery.Message.Details.ChildID == run.ID && delivery.Message.Details.Generation == run.Generation
}

func (f *fixture) runCompletion(ctx context.Context, directory string, send func(any) error, wait func(string, string) error, events <-chan map[string]any) error {
	if err := send(map[string]any{"id": "busy", "type": "prompt", "message": "BUSY_BEGIN. PARENT_PRIVATE_MARKER"}); err != nil {
		return err
	}
	if err := f.wait(ctx, func() bool { return len(f.runs) == 1 && f.held["busy-parent"] != nil }); err != nil {
		return fmt.Errorf("busy parent provider stream: %w", err)
	}
	if err := f.releaseTask("busy-child"); err != nil {
		return err
	}
	busy, err := waitCompletionDelivery(ctx, directory, f.runs[0])
	if err != nil {
		return err
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
			hidden = hidden || completionEvent(event, f.runs[0])
			if event["type"] == "response" && event["success"] == false {
				return fmt.Errorf("host rejected completion command: %v", event)
			}
			if event["type"] == "message_end" {
				raw, _ := json.Marshal(event["message"])
				processed = processed || strings.Contains(string(raw), "BUSY_COMPLETION_PROCESSED")
			}
			settled = processed && event["type"] == "agent_end"
		case <-answerCtx.Done():
			return fmt.Errorf("busy completion did not produce another answer without a user prompt: %w", answerCtx.Err())
		}
	}
	if !hidden || busy.Options.TriggerTurn != nil || busy.Options.DeliverAs != "followUp" {
		return errors.New("busy completion was not delivered as a hidden follow-up")
	}
	f.mu.Lock()
	f.checks["busyCompletionProducedAnswerWithoutPrompt"] = true
	f.mu.Unlock()
	if err := send(map[string]any{"id": "idle", "type": "prompt", "message": "IDLE_BEGIN"}); err != nil {
		return err
	}
	if err := wait("IDLE_READY", ""); err != nil {
		return err
	}
	f.mu.Lock()
	requestCount := len(f.requests)
	f.mu.Unlock()
	if err := f.releaseTask("idle-child"); err != nil {
		return err
	}
	idle, err := waitCompletionDelivery(ctx, directory, f.runs[1])
	if err != nil {
		return err
	}
	if idle.Options.TriggerTurn == nil || *idle.Options.TriggerTurn || idle.Options.DeliverAs != "" {
		return errors.New("idle completion did not request append-only delivery")
	}
	if err := send(map[string]any{"id": "idle-state", "type": "get_state"}); err != nil {
		return err
	}
	stateCtx, stateCancel := context.WithTimeout(ctx, 10*time.Second)
	defer stateCancel()
	hidden = false
	verifiedIdle := false
	for {
		select {
		case event, open := <-events:
			if !open {
				return errors.New("host exited before idle completion verification")
			}
			hidden = hidden || completionEvent(event, f.runs[1])
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
			defer f.mu.Unlock()
			if f.failure != nil {
				return f.failure
			}
			if len(f.requests) != requestCount {
				return errors.New("idle completion caused an extra provider request")
			}
			f.checks["idleCompletionAppendedWithoutInference"] = true
			return nil
		case <-stateCtx.Done():
			return fmt.Errorf("idle completion state: %w", stateCtx.Err())
		}
	}
}
