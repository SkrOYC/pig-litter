package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

type childCheck struct{ name, kind, model, task, state, errorText string }

var childChecks = []childCheck{
	{"deniedRead", "worker", "denied", "deny-read", "partial", "tool read failed"},
	{"deniedWrite", "worker", "denied", "deny-write", "partial", "tool write failed"},
	{"providerFailure", "scout", "provider_error", "provider-failure", "failed", "intentional provider failure"},
	{"toolFailure", "scout", "missing_file", "missing-file", "partial", "tool read failed"},
	{"readonlyDelegation", "scout", "roles", "role-ceiling", "partial", "tool litter_spawn failed"},
	{"historyRetired", "scout", "history", "oversized-history", "partial", "retained child history exceeded"},
}

func validScenario(name string) bool {
	switch name {
	case "core", "nested", "checks", "messages", "lifecycle", "shadow":
		return true
	}
	return false
}

func (f *fixture) parentShadow(body modelRequest) (string, []call, error) {
	tools, err := toolResults(body)
	if err != nil {
		return "", nil, err
	}
	if len(tools) == 0 {
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "shadow must not reproduce", "model": "fixture/held"}}}, nil
	}
	if len(tools) != 1 || tools[0].State != "unavailable" || !strings.Contains(tools[0].Report, "extension-registered parent provider") {
		return "", nil, errors.New("extension provider shadow was reproduced in an independent child")
	}
	f.checks["parentProviderShadowRejected"] = true
	f.done = true
	return "provider shadow rejected", nil, nil
}

func jsonResult(message message, value *result) error {
	return json.Unmarshal([]byte(content(message.Content)), value)
}

func (f *fixture) parentChecks(body modelRequest) (string, []call, error) {
	tools, err := toolResults(body)
	if err != nil {
		return "", nil, err
	}
	if f.stage == 0 {
		f.stage = 1
		return "", []call{
			{"litter_spawn", map[string]any{"type": "scout", "task": "missing model", "model": "fixture/missing"}},
			{"litter_spawn", map[string]any{"type": "fixed", "task": "fixed model", "model": "fixture/worker"}},
		}, nil
	}
	if f.stage == 1 {
		if len(tools) != 2 || tools[0].State != "unavailable" || tools[1].State != "rejected" || tools[1].Code != "permission_denied" {
			return "", nil, errors.New("missing model or fixed model was not rejected truthfully")
		}
		f.checks["missingModel"], f.checks["fixedModel"] = true, true
		f.stage = 2
	}
	index := len(f.runs)
	if f.stage == 2 {
		if index == len(childChecks) {
			f.done = true
			return "failure and authority checks complete", nil, nil
		}
		check := childChecks[index]
		f.stage = 3
		return "", []call{{"litter_spawn", map[string]any{"type": check.kind, "task": check.task, "name": check.name, "model": "fixture/" + check.model}}}, nil
	}
	if f.stage == 3 {
		latest := tools[len(tools)-1]
		if latest.Child.ID == "" {
			return "", nil, fmt.Errorf("%s admission failed: %s %s", childChecks[index].name, latest.State, latest.Report)
		}
		f.worker = latest.Child.ref
		f.stage = 4
		return "", []call{{"litter_wait", with(f.worker, "timeoutMs", 10000)}}, nil
	}
	if f.stage == 4 {
		check := childChecks[index]
		latest := tools[len(tools)-1]
		if latest.State != check.state || !strings.Contains(latest.Outcome.Error, check.errorText) {
			return "", nil, fmt.Errorf("%s outcome is %s with %q, want %s and %q", check.name, latest.State, latest.Outcome.Error, check.state, check.errorText)
		}
		f.checks[check.name] = true
		if check.model == "history" {
			f.stage = 5
			return "", []call{{"litter_inspect", map[string]any{"id": f.worker.ID, "transcript": true}}}, nil
		}
		f.runs = append(f.runs, f.worker)
		f.stage = 2
		return f.parentChecks(body)
	}
	if f.stage == 5 {
		latest := tools[len(tools)-1]
		if latest.HistoryAvailable || len(latest.Entries) != 0 {
			return "", nil, errors.New("over-budget manager remained available after cleanup")
		}
		f.stage = 6
		return "", []call{{"litter_message", with(f.worker, "resume", true, "text", "must not recreate history")}}, nil
	}
	if f.stage == 6 {
		if tools[len(tools)-1].State != "unavailable" {
			return "", nil, errors.New("over-budget history resumed")
		}
		f.checks["historyResumeUnavailable"] = true
		f.runs = append(f.runs, f.worker)
		f.stage = 2
		return f.parentChecks(body)
	}
	return "", nil, errors.New("unexpected check stage")
}

func (f *fixture) checkedChild(ctx context.Context, body modelRequest) (string, []call, error) {
	user := ""
	tools := []message{}
	for _, message := range body.Messages {
		if message.Role == "user" {
			user += content(message.Content) + "\n"
		}
		if message.Role == "tool" {
			tools = append(tools, message)
		}
	}
	switch body.Model {
	case "denied":
		name, path := "read", "denied-read.txt"
		if strings.Contains(user, "deny-write") {
			name, path = "write", "denied-write.txt"
		}
		if len(tools) == 0 {
			args := map[string]string{"path": path}
			if name == "write" {
				args["content"] = "must not be written"
			}
			return "", []call{{name, args}}, nil
		}
		if len(tools) != 1 || !strings.Contains(content(tools[0].Content), "PARENT_PERMISSION_DENIED_"+name) {
			return "", nil, errors.New("child did not receive the parent's denied tool result")
		}
		return "assistant finished after denied operation", nil, nil
	case "missing_file":
		if len(tools) == 0 {
			return "", []call{{"read", map[string]string{"path": "missing-file.txt"}}}, nil
		}
		return "assistant finished after failed read", nil, nil
	case "roles":
		if len(tools) == 0 {
			return "", []call{{"litter_spawn", map[string]any{"type": "worker", "task": "forbidden writer", "model": "fixture/worker"}}}, nil
		}
		var value result
		if len(tools) != 1 || jsonResult(tools[0], &value) != nil || value.State != "rejected" || value.Code != "permission_denied" {
			return "", nil, errors.New("readonly child delegated a writer")
		}
		return "assistant finished after refused writer delegation", nil, nil
	case "history":
		return strings.Repeat("large retained history ", 4000), nil, nil
	case "message_child":
		if strings.Contains(user, "THIRD_OVERFLOW") {
			return "", nil, errors.New("SDK queued mailbox accepted an overflow message")
		}
		if strings.Contains(user, "SECOND_FOLLOW") {
			if !strings.Contains(user, "FIRST_STEER") || strings.Index(user, "FIRST_STEER") > strings.Index(user, "SECOND_FOLLOW") {
				return "", nil, errors.New("steering and follow-up messages were reordered")
			}
			f.mu.Lock()
			f.checks["messageOrder"] = true
			f.mu.Unlock()
			return "MESSAGE_ORDER_DONE", nil, nil
		}
		if strings.Contains(user, "FIRST_STEER") {
			return "STEER_APPLIED", nil, nil
		}
		return f.hold(ctx, "messages"), nil, nil
	}
	return "", nil, errors.New("unexpected checked child")
}

func (f *fixture) parentMessages(ctx context.Context, body modelRequest) (string, []call, error) {
	tools, err := toolResults(body)
	if err != nil {
		return "", nil, err
	}
	switch f.stage {
	case 0:
		f.stage++
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "message-task", "name": "ordered", "model": "fixture/message_child"}}}, nil
	case 1:
		if len(tools) != 1 || tools[0].Child.ID == "" {
			return "", nil, errors.New("message child admission failed")
		}
		f.worker = tools[0].Child.ref
		if err := f.wait(ctx, func() bool { return f.held["messages"] != nil }); err != nil {
			return "", nil, err
		}
		f.stage++
		return "", []call{{"litter_message", with(f.worker, "text", "FIRST_STEER", "mode", "steer")}}, nil
	case 2:
		if len(tools) != 2 || tools[1].State != "running" {
			return "", nil, errors.New("steering did not queue to a live child")
		}
		f.stage++
		return "", []call{{"litter_message", with(f.worker, "text", "SECOND_FOLLOW", "mode", "follow_up")}}, nil
	case 3:
		if len(tools) != 3 || tools[2].State != "running" {
			return "", nil, errors.New("follow-up did not queue to a live child")
		}
		f.stage++
		return "", []call{{"litter_message", with(f.worker, "text", "THIRD_OVERFLOW", "mode", "steer")}}, nil
	case 4:
		if len(tools) != 4 || tools[3].State != "rejected" || !strings.Contains(tools[3].Report, "message bound") {
			return "", nil, errors.New("SDK queued steering did not count against the mailbox bound")
		}
		f.mu.Lock()
		f.checks["sdkMailboxBound"] = true
		f.mu.Unlock()
		f.release()
		f.stage++
		return "", []call{{"litter_wait", with(f.worker, "timeoutMs", 10000)}}, nil
	case 5:
		if len(tools) != 5 || tools[4].State != "completed" || tools[4].Outcome.Text != "MESSAGE_ORDER_DONE" || tools[4].Outcome.Usage.TotalTokens != 45 {
			return "", nil, errors.New("ordered messages did not finish in the owning generation")
		}
		f.done = true
		return "message ordering complete", nil, nil
	}
	return "", nil, errors.New("unexpected message stage")
}
