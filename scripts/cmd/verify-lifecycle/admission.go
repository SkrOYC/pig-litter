package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

const finalSpawnFailurePrefix = "FIXTURE_FINAL_SPAWN_FAILURE\n"

func (f *fixture) parentAdmission(_ context.Context, body modelRequest) (string, []call, error) {
	messages := toolMessages(body)
	switch f.stage {
	case 0:
		if len(messages) != 0 {
			return "", nil, errors.New("admission fixture started with prior tool results")
		}
		f.stage = 1
		return "", []call{{"litter_spawn", map[string]any{
			"type": "scout", "task": "RESULT_HOOK_FAILURE_MUST_NOT_RUN", "name": "result-hook-retry", "model": "fixture/lifecycle_child",
		}}}, nil
	case 1:
		if len(messages) == 0 {
			return "", nil, errors.New("failed spawn returned no tool result")
		}
		text := content(messages[len(messages)-1].Content)
		original, ok := strings.CutPrefix(text, finalSpawnFailurePrefix)
		if !ok {
			return "", nil, fmt.Errorf("spawn result hook did not inject its failure: %q", text)
		}
		var provisional result
		if err := json.Unmarshal([]byte(original), &provisional); err != nil || provisional.State != "starting" || provisional.Child.ID == "" || provisional.Child.Name != "result-hook-retry" {
			return "", nil, fmt.Errorf("tool-result hook did not follow provisional spawn success: %#v, %v", provisional, err)
		}
		f.admissionProvisional = provisional.Child.ref
		f.stage = 2
		return "", []call{{"litter_inspect", with(f.admissionProvisional)}}, nil
	case 2:
		inspected, err := lastToolResult(body)
		if err != nil || inspected.State != "unavailable" || inspected.Code != "unavailable" || !strings.Contains(inspected.Report, "UUID is unavailable") {
			return "", nil, fmt.Errorf("failed spawn ID remained controllable: %#v, %v", inspected, err)
		}
		f.markCheck("toolResultFailureRolledBackID")
		f.stage = 3
		return "", []call{{"litter_list", map[string]any{}}}, nil
	case 3:
		listed, err := lastToolResult(body)
		if err != nil || len(listed.Children) != 0 {
			return "", nil, fmt.Errorf("rolled-back spawn remained in the active child list: %#v, %v", listed, err)
		}
		f.stage = 4
		return "", []call{{"litter_spawn", map[string]any{
			"type": "scout", "task": "RESULT_HOOK_SUCCESS", "name": "result-hook-retry", "model": "fixture/lifecycle_child",
		}}}, nil
	case 4:
		spawned, err := lastToolResult(body)
		if err != nil || spawned.Child.ID == "" || spawned.Child.Name != "result-hook-retry" || spawned.Child.ID == f.admissionProvisional.ID {
			return "", nil, fmt.Errorf("same-name spawn was not admitted after rollback: %#v, %v", spawned, err)
		}
		f.resumed = spawned.Child.ref
		f.stage = 5
		return "", []call{{"litter_wait", with(f.resumed, "timeoutMs", 10000)}}, nil
	case 5:
		waited, err := lastToolResult(body)
		if err != nil || waited.State != "completed" || waited.Outcome.Run != f.resumed || waited.Outcome.Text != "RESULT_HOOK_SUCCESS" {
			return "", nil, fmt.Errorf("replacement child did not complete under its admitted ID: %#v, %v", waited, err)
		}
		if f.countRequests("lifecycle_child", "RESULT_HOOK_FAILURE_MUST_NOT_RUN") != 0 {
			return "", nil, errors.New("rolled-back child reached the provider")
		}
		f.markCheck("toolResultFailureProviderNotStarted")
		f.markCheck("toolResultFailureCapacityReused")
		f.runs = append(f.runs, f.resumed)
		f.done = true
		return "tool-result failure rolled back provisional admission", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected admission stage %d", f.stage)
}
