package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func (f *fixture) parentNested(ctx context.Context, body modelRequest) (string, []call, error) {
	tools, err := toolResults(body)
	if err != nil {
		return "", nil, err
	}
	switch f.stage {
	case 0:
		f.stage = 1
		return "", []call{
			{"litter_spawn", map[string]any{"type": "coordinator", "task": "Nested parent task.", "name": "nested-parent", "model": "fixture/nester"}},
			{"litter_spawn", map[string]any{"type": "scout", "task": "Nested sibling task.", "name": "nested-sibling", "model": "fixture/nested_sibling"}},
		}, nil
	case 1:
		if len(tools) != 2 || tools[0].Child.ID == "" || tools[1].Child.ID == "" || tools[0].Child.ID == tools[1].Child.ID || tools[0].Child.Type != "coordinator" || tools[1].Child.Type != "scout" || tools[0].Child.CWD != f.expectedCWD || tools[1].Child.CWD != f.expectedCWD || tools[0].Child.Parent == nil || tools[1].Child.Parent == nil || *tools[0].Child.Parent != *tools[1].Child.Parent {
			return "", nil, fmt.Errorf("nested direct children were not admitted under one host parent: %#v", tools)
		}
		f.mu.Lock()
		f.nestedParent, f.sibling = tools[0].Child.ref, tools[1].Child.ref
		f.hostSessionID = *tools[0].Child.Parent
		f.signalLocked()
		f.mu.Unlock()
		if err := f.wait(ctx, func() bool { return f.held["nested-parent-ready"] != nil && f.held["nested-sibling"] != nil }); err != nil {
			return "", nil, fmt.Errorf("direct children did not start: %w", err)
		}
		f.stage = 2
		return "", []call{{"litter_message", with(f.nestedParent, "text", "OWNERSHIP_IDS root="+f.hostSessionID+" self="+f.nestedParent.ID+" sibling="+f.sibling.ID, "mode", "steer")}}, nil
	case 2:
		if len(tools) != 3 || tools[2].Child.ID != f.nestedParent.ID {
			return "", nil, errors.New("ownership message did not target the direct child")
		}
		if err := f.releaseTask("nested-parent-ready"); err != nil {
			return "", nil, fmt.Errorf("release parent after steering acknowledgement: %w", err)
		}
		if err := f.wait(ctx, func() bool {
			return f.grandchild.ID != "" && f.depthRejected && f.held["nested-parent"] != nil && f.held["nested-grandchild"] != nil &&
				f.checks["nesterAncestorDenied"] && f.checks["nesterSiblingDenied"] && f.checks["nesterDirectList"] &&
				f.checks["grandchildAncestorDenied"] && f.checks["grandchildSiblingDenied"] && f.checks["grandchildDirectList"]
		}); err != nil {
			return "", nil, fmt.Errorf("nested ownership checks did not settle: %w", err)
		}
		f.stage = 3
		return "", []call{{"litter_list", map[string]any{}}}, nil
	case 3:
		if len(tools) != 4 || len(tools[3].Children) != 2 {
			return "", nil, errors.New("root active-child list was not limited to its two direct children")
		}
		found := map[string]bool{}
		for _, child := range tools[3].Children {
			if child.Parent != nil && *child.Parent == f.hostSessionID && child.Depth == 1 && child.CWD == f.expectedCWD {
				found[child.ID] = true
			}
		}
		if !found[f.nestedParent.ID] || !found[f.sibling.ID] || found[f.grandchild.ID] {
			return "", nil, errors.New("root active-child list included a descendant or lost a direct child")
		}
		f.markCheck("rootDirectActiveChildren")
		f.stage = 4
		return "", []call{{"litter_wait", with(f.grandchild)}}, nil
	case 4:
		if len(tools) != 5 || tools[4].State != "rejected" || tools[4].Code != "permission_denied" {
			return "", nil, errors.New("root controlled a grandchild")
		}
		f.markCheck("rootGrandchildControlDenied")
		f.stage = 5
		return "", []call{{"litter_stop", map[string]string{"id": f.nestedParent.ID}}}, nil
	case 5:
		if len(tools) != 6 || tools[5].State != "paused" || tools[5].StoppedCount != 2 || tools[5].Outcome.Run.ID != f.nestedParent.ID {
			return "", nil, fmt.Errorf("parent stop did not pause its active subtree: %#v", tools)
		}
		if err := f.wait(ctx, func() bool {
			return f.aborted["nested-parent"] && f.aborted["nested-grandchild"] && f.held["nested-parent"] == nil && f.held["nested-grandchild"] == nil
		}); err != nil {
			return "", nil, fmt.Errorf("parent pause did not cancel both active provider streams: %w", err)
		}
		f.markCheck("parentPauseCascade")
		f.stage = 6
		return "", []call{{"litter_list", map[string]any{}}}, nil
	case 6:
		if len(tools) != 7 || len(tools[6].Children) != 1 || tools[6].Children[0].ID != f.sibling.ID {
			return "", nil, errors.New("active-child list resumed or retained a paused descendant")
		}
		f.markCheck("pausedNodesExcludedFromActiveList")
		f.stage = 7
		return "", []call{{"litter_inspect", with(f.nestedParent)}}, nil
	case 7:
		if len(tools) != 8 || tools[7].Child.ID != f.nestedParent.ID || tools[7].Child.State != "paused" {
			return "", nil, fmt.Errorf("inspection lost the paused parent node: %#v", tools)
		}
		f.markCheck("pausedNodeRetained")
		f.stage = 8
		return "", []call{{"litter_message", with(f.nestedParent, "text", "EXPLICIT_RESUME_A", "resume", true)}}, nil
	case 8:
		if len(tools) != 9 || tools[8].Child.ID != f.nestedParent.ID || tools[8].Delivery != "resumed" {
			return "", nil, fmt.Errorf("explicit resume changed the selected parent identity: %#v", tools)
		}
		f.markCheck("parentResumedWithStableUUID")
		f.stage = 9
		return "", []call{{"litter_wait", with(f.nestedParent, "timeoutMs", 10000)}}, nil
	case 9:
		if len(tools) != 10 || tools[9].State != "completed" || tools[9].Outcome.Run != f.nestedParent || tools[9].Outcome.Text != "NESTED_PARENT_RESUMED_RESULT" {
			return "", nil, fmt.Errorf("selected parent did not finish its explicit resume: %#v", tools)
		}
		f.mu.Lock()
		resumeChecks := f.checks["nestedDescendantNotAutoResumed"] && f.checks["nestedDescendantPaused"] && f.checks["nestedDescendantExplicitlyResumed"]
		f.mu.Unlock()
		if !resumeChecks {
			return "", nil, errors.New("resumed parent did not explicitly select its paused child")
		}
		if err := f.releaseTask("nested-sibling"); err != nil {
			return "", nil, err
		}
		f.stage = 10
		return "", []call{{"litter_wait", with(f.sibling, "timeoutMs", 10000)}}, nil
	case 10:
		if len(tools) != 11 || tools[10].State != "completed" || tools[10].Outcome.Run != f.sibling || !strings.Contains(tools[10].Outcome.Text, "completed nested-sibling") {
			return "", nil, fmt.Errorf("unpaused sibling did not complete with its stable ID: %#v", tools)
		}
		f.stage = 11
		return "", []call{{"litter_list", map[string]any{}}}, nil
	case 11:
		if len(tools) != 12 || len(tools[11].Children) != 0 {
			return "", nil, errors.New("completed direct children remained in the active-child list")
		}
		f.markCheck("completedNodesExcludedFromActiveList")
		f.done = true
		return "nested ownership and pause lifecycle complete", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected nested parent stage %d", f.stage)
}

func ownershipIDs(f *fixture) (root, parent, sibling, grandchild string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hostSessionID, f.nestedParent.ID, f.sibling.ID, f.grandchild.ID
}

func userText(body modelRequest) string {
	var text strings.Builder
	for _, message := range body.Messages {
		if message.Role == "user" {
			text.WriteString(content(message.Content))
			text.WriteByte('\n')
		}
	}
	return text.String()
}

func lastToolResult(body modelRequest) (result, error) {
	messages := toolMessages(body)
	if len(messages) == 0 {
		return result{}, errors.New("provider request has no tool result")
	}
	var value result
	if err := json.Unmarshal([]byte(content(messages[len(messages)-1].Content)), &value); err != nil {
		return result{}, err
	}
	return value, nil
}

func (f *fixture) nesterRequest(ctx context.Context, body modelRequest) (string, []call, error) {
	user := userText(body)
	switch f.nesterStage {
	case 0:
		if !strings.Contains(user, "OWNERSHIP_IDS") {
			return f.hold(ctx, "nested-parent-ready"), nil, nil
		}
		root, parent, sibling, _ := ownershipIDs(f)
		if !strings.Contains(user, "root="+root) || !strings.Contains(user, "self="+parent) || !strings.Contains(user, "sibling="+sibling) {
			return "", nil, errors.New("root did not give the parent actor its known ancestor and sibling IDs")
		}
		f.nesterStage = 1
		return "", []call{{"litter_inspect", map[string]string{"id": root}}}, nil
	case 1:
		value, err := lastToolResult(body)
		if err != nil || value.State != "rejected" {
			return "", nil, fmt.Errorf("child controlled its ancestor: %#v, %v", value, err)
		}
		f.markCheck("nesterAncestorDenied")
		_, _, sibling, _ := ownershipIDs(f)
		f.nesterStage = 2
		return "", []call{{"litter_inspect", map[string]string{"id": sibling}}}, nil
	case 2:
		value, err := lastToolResult(body)
		if err != nil || value.State != "rejected" || value.Code != "permission_denied" {
			return "", nil, fmt.Errorf("child controlled its sibling: %#v, %v", value, err)
		}
		f.markCheck("nesterSiblingDenied")
		_, parent, sibling, _ := ownershipIDs(f)
		task := "Nested grandchild task. ANCESTOR_ID=" + parent + " SIBLING_ID=" + sibling
		f.nesterStage = 3
		return "", []call{{"litter_spawn", map[string]any{"type": "coordinator", "task": task, "name": "nested-grandchild", "model": "fixture/grandchild"}}}, nil
	case 3:
		value, err := lastToolResult(body)
		if err != nil || value.Child.ID == "" || value.Child.Type != "coordinator" || value.Child.CWD != f.expectedCWD {
			return "", nil, fmt.Errorf("grandchild admission failed: %#v, %v", value, err)
		}
		f.mu.Lock()
		f.grandchild = value.Child.ref
		f.signalLocked()
		f.mu.Unlock()
		f.nesterStage = 4
		return "", []call{{"litter_list", map[string]any{}}}, nil
	case 4:
		value, err := lastToolResult(body)
		_, parent, _, grandchild := ownershipIDs(f)
		if err != nil || len(value.Children) != 1 || value.Children[0].ID != grandchild || value.Children[0].Parent == nil || *value.Children[0].Parent != parent || value.Children[0].Depth != 2 {
			return "", nil, fmt.Errorf("child active-list was not limited to its direct grandchild: %#v, %v", value, err)
		}
		f.markCheck("nesterDirectList")
		f.nesterStage = 5
		return f.hold(ctx, "nested-parent"), nil, nil
	case 5:
		if !strings.Contains(user, "EXPLICIT_RESUME_A") {
			return "", nil, errors.New("paused parent resumed without an explicit selected-node message")
		}
		f.nesterStage = 6
		return "", []call{{"litter_list", map[string]any{}}}, nil
	case 6:
		value, err := lastToolResult(body)
		if err != nil || len(value.Children) != 0 {
			return "", nil, fmt.Errorf("paused grandchild resumed automatically with its parent: %#v, %v", value, err)
		}
		f.markCheck("nestedDescendantNotAutoResumed")
		_, _, _, grandchild := ownershipIDs(f)
		f.nesterStage = 7
		return "", []call{{"litter_inspect", map[string]string{"id": grandchild}}}, nil
	case 7:
		value, err := lastToolResult(body)
		_, _, _, grandchild := ownershipIDs(f)
		if err != nil || value.Child.ID != grandchild || value.Child.State != "paused" {
			return "", nil, fmt.Errorf("descendant did not retain its paused session: %#v, %v", value, err)
		}
		f.markCheck("nestedDescendantPaused")
		f.nesterStage = 8
		return "", []call{{"litter_message", map[string]any{"id": grandchild, "text": "EXPLICIT_RESUME_C", "resume": true}}}, nil
	case 8:
		value, err := lastToolResult(body)
		_, _, _, grandchild := ownershipIDs(f)
		if err != nil || value.Child.ID != grandchild || value.Delivery != "resumed" {
			return "", nil, fmt.Errorf("child resume changed its selected session ID: %#v, %v", value, err)
		}
		f.nesterStage = 9
		return "", []call{{"litter_wait", map[string]any{"id": grandchild, "timeoutMs": 10000}}}, nil
	case 9:
		value, err := lastToolResult(body)
		_, _, _, grandchild := ownershipIDs(f)
		if err != nil || value.State != "completed" || value.Outcome.Run.ID != grandchild || value.Outcome.Text != "GRANDCHILD_EXPLICIT_RESUMED_RESULT" {
			return "", nil, fmt.Errorf("explicit descendant resume changed its ID or result: %#v, %v", value, err)
		}
		f.markCheck("nestedDescendantExplicitlyResumed")
		f.nesterStage = 10
		return "NESTED_PARENT_RESUMED_RESULT", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected nested parent actor stage %d", f.nesterStage)
}

func (f *fixture) grandchildRequest(ctx context.Context, body modelRequest) (string, []call, error) {
	user := userText(body)
	if strings.Contains(user, "EXPLICIT_RESUME_C") {
		f.markCheck("grandchildExplicitResumeObserved")
		return "GRANDCHILD_EXPLICIT_RESUMED_RESULT", nil, nil
	}
	switch f.grandchildStage {
	case 0:
		_, parent, sibling, _ := ownershipIDs(f)
		if !strings.Contains(user, "ANCESTOR_ID="+parent) || !strings.Contains(user, "SIBLING_ID="+sibling) {
			return "", nil, errors.New("grandchild task omitted its known ancestor and sibling IDs")
		}
		f.grandchildStage = 1
		return "", []call{{"litter_inspect", map[string]string{"id": parent}}}, nil
	case 1:
		value, err := lastToolResult(body)
		if err != nil || value.State != "rejected" || value.Code != "permission_denied" {
			return "", nil, fmt.Errorf("grandchild controlled its ancestor: %#v, %v", value, err)
		}
		f.markCheck("grandchildAncestorDenied")
		_, _, sibling, _ := ownershipIDs(f)
		f.grandchildStage = 2
		return "", []call{{"litter_inspect", map[string]string{"id": sibling}}}, nil
	case 2:
		value, err := lastToolResult(body)
		if err != nil || value.State != "rejected" || value.Code != "permission_denied" {
			return "", nil, fmt.Errorf("grandchild controlled its parent's sibling: %#v, %v", value, err)
		}
		f.markCheck("grandchildSiblingDenied")
		f.grandchildStage = 3
		return "", []call{{"litter_list", map[string]any{}}}, nil
	case 3:
		value, err := lastToolResult(body)
		if err != nil || len(value.Children) != 0 {
			return "", nil, fmt.Errorf("grandchild list included a non-direct child: %#v, %v", value, err)
		}
		f.markCheck("grandchildDirectList")
		f.grandchildStage = 4
		return "", []call{{"read", map[string]string{"path": "input.txt"}}}, nil
	case 4:
		if !strings.Contains(content(toolMessages(body)[len(toolMessages(body))-1].Content), "stock fixture read marker") {
			return "", nil, errors.New("depth-limited coordinator could not read its cwd file")
		}
		f.grandchildStage = 5
		return "", []call{{"litter_spawn", map[string]any{"type": "coordinator", "task": "Forbidden depth three.", "name": "too-deep", "model": "fixture/nester"}}}, nil
	case 5:
		value, err := lastToolResult(body)
		if err != nil || value.State != "rejected" {
			return "", nil, fmt.Errorf("depth-three admission was not rejected: %#v, %v", value, err)
		}
		f.mu.Lock()
		f.depthRejected = true
		f.signalLocked()
		f.mu.Unlock()
		f.grandchildStage = 6
		return f.hold(ctx, "nested-grandchild"), nil, nil
	}
	return "", nil, fmt.Errorf("unexpected grandchild actor stage %d", f.grandchildStage)
}

func (f *fixture) nestedSiblingRequest(ctx context.Context, _ modelRequest) (string, []call, error) {
	return f.hold(ctx, "nested-sibling"), nil, nil
}
