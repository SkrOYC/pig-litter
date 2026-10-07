package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}
type modelTool struct {
	Function struct {
		Name string `json:"name"`
	} `json:"function"`
}
type modelRequest struct {
	Model    string      `json:"model"`
	Messages []message   `json:"messages"`
	Tools    []modelTool `json:"tools"`
}
type ref struct {
	ID string `json:"id"`
}
type child struct {
	ref
	Parent       *string `json:"parent"`
	Depth        int     `json:"depth"`
	Name         string  `json:"name"`
	DisplayLabel string  `json:"displayLabel"`
	Type         string  `json:"type"`
	Model        string  `json:"model"`
	State        string  `json:"state"`
	CWD          string  `json:"cwd"`
}
type result struct {
	State               string  `json:"state"`
	Code                string  `json:"code"`
	Report              string  `json:"report"`
	Child               child   `json:"child"`
	Children            []child `json:"children"`
	AvailableAgentTypes []struct {
		Name        string   `json:"name"`
		Role        string   `json:"role"`
		Description string   `json:"description"`
		Tools       []string `json:"tools"`
		CanDelegate bool     `json:"canDelegate"`
	} `json:"availableAgentTypes"`
	TotalAgentTypes int     `json:"totalAgentTypes"`
	Outcome         outcome `json:"outcome"`
	StoppedCount    int     `json:"stoppedCount"`
}
type outcome struct {
	Run              ref    `json:"run"`
	State            string `json:"state"`
	Model            string `json:"model"`
	Text             string `json:"text"`
	Error            string `json:"error"`
	Truncated        bool   `json:"truncated"`
	ReportProvenance string `json:"reportProvenance"`
	Usage            struct {
		TotalTokens int `json:"totalTokens"`
	} `json:"usage"`
}
type call struct {
	name string
	args any
}
type fixture struct {
	mu                       sync.Mutex
	changed                  chan struct{}
	held                     map[string]chan struct{}
	requests                 []modelRequest
	failure                  error
	stage                    int
	runs                     []ref
	resumed, worker, stopped ref
	nestedParent, grandchild ref
	expectedCWD              string
	relativeCWD              string
	bashPath                 string
	depthRejected            bool
	done                     bool
	scenario                 string
	checks                   map[string]bool
	aborted                  map[string]bool
	sequence                 atomic.Uint64
}

func content(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct{ Type, Text string }
	if json.Unmarshal(raw, &blocks) == nil {
		for _, block := range blocks {
			if block.Type == "text" {
				text += block.Text
			}
		}
	}
	return text
}

func toolResults(body modelRequest) ([]result, error) {
	results := []result{}
	for _, message := range toolMessages(body) {
		var value result
		if err := json.Unmarshal([]byte(content(message.Content)), &value); err != nil {
			return nil, err
		}
		results = append(results, value)
	}
	return results, nil
}

func toolMessages(body modelRequest) []message {
	var messages []message
	for _, value := range body.Messages {
		if value.Role == "tool" {
			messages = append(messages, value)
		}
	}
	return messages
}

func systemPrompt(body modelRequest) string {
	for _, value := range body.Messages {
		if value.Role == "system" {
			return content(value.Content)
		}
	}
	return ""
}

func require(ok bool, message string) error {
	if ok {
		return nil
	}
	return errors.New(message)
}
func (f *fixture) signalLocked() { close(f.changed); f.changed = make(chan struct{}) }
func (f *fixture) wait(ctx context.Context, check func() bool) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for {
		f.mu.Lock()
		ready, changed := check(), f.changed
		f.mu.Unlock()
		if ready {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}
func (f *fixture) hold(ctx context.Context, task string) string {
	gate := make(chan struct{})
	f.mu.Lock()
	f.held[task] = gate
	f.signalLocked()
	f.mu.Unlock()
	select {
	case <-gate:
		return "completed " + task + " " + strings.Repeat("bounded report ", 900)
	case <-ctx.Done():
		f.mu.Lock()
		if f.aborted != nil {
			f.aborted[task] = true
		}
		if f.held[task] == gate {
			delete(f.held, task)
			f.signalLocked()
		}
		f.mu.Unlock()
		return "aborted child"
	}
}
func (f *fixture) release() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for task, gate := range f.held {
		delete(f.held, task)
		close(gate)
	}
	f.signalLocked()
}

func (f *fixture) answer(w http.ResponseWriter, model, text string, calls []call) {
	delta := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if len(calls) != 0 {
		values := []any{}
		for index, call := range calls {
			raw, _ := json.Marshal(call.args)
			values = append(values, map[string]any{"index": index, "id": fmt.Sprintf("call_%d", f.sequence.Add(1)), "type": "function", "function": map[string]any{"name": call.name, "arguments": string(raw)}})
		}
		delta = map[string]any{"role": "assistant", "tool_calls": values}
		finish = "tool_calls"
	}
	base := map[string]any{"id": "go-stock-fixture", "object": "chat.completion.chunk", "created": 1, "model": model}
	base["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}
	first, _ := json.Marshal(base)
	base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}
	base["usage"] = map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	last, _ := json.Marshal(base)
	w.Header().Set("Content-Type", "text/event-stream")
	_, _ = fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", first, last)
}

func (f *fixture) parent(ctx context.Context, body modelRequest) (string, []call, error) {
	tools, err := toolResults(body)
	if err != nil {
		return "", nil, err
	}
	switch f.stage {
	case 0:
		f.stage++
		calls := []call{}
		for _, name := range []string{"A", "B", "C"} {
			calls = append(calls, call{"litter_spawn", map[string]any{"type": "scout", "task": "hold-" + name, "name": name, "model": "fixture/held"}})
		}
		return "", calls, require(len(tools) == 0, "initial parent has tool results")
	case 1:
		if err := require(len(tools) == 3, "parent expected three spawn results"); err != nil {
			return "", nil, err
		}
		rejected := 0
		for _, value := range tools {
			if value.Child.ID != "" {
				f.runs = append(f.runs, value.Child.ref)
			} else if value.State == "rejected" {
				rejected++
			}
		}
		if err := require(len(f.runs) == 2 && rejected == 1 && f.runs[0].ID != f.runs[1].ID, "project concurrency did not override the user limit of three"); err != nil {
			return "", nil, err
		}
		if err := f.wait(ctx, func() bool { return len(f.held) == 2 }); err != nil {
			return "", nil, fmt.Errorf("two independent native requests: %w", err)
		}
		f.stage++
		return "", []call{{"litter_list", map[string]any{}}, {"litter_wait", with(f.runs[0], "timeoutMs", 100)}}, nil
	case 2:
		if err := require(len(tools) == 5 && len(tools[3].Children) == 2 && tools[4].State == "timed_out", "wait timeout stopped a child or list missed the live children"); err != nil {
			return "", nil, err
		}
		if err := require(tools[3].TotalAgentTypes == 3 && len(tools[3].AvailableAgentTypes) == 3, "user agent discovery count changed"); err != nil {
			return "", nil, err
		}
		userAgent := tools[3].AvailableAgentTypes[2]
		if err := require(tools[3].AvailableAgentTypes[0].Name == "scout" && tools[3].AvailableAgentTypes[1].Name == "worker" && userAgent.Name == "zz_user_only" && userAgent.Role == "scout" && len(userAgent.Tools) == 1 && userAgent.Tools[0] == "read" && !userAgent.CanDelegate, "user agent discovery or project concurrency overlay changed"); err != nil {
			return "", nil, err
		}
		f.release()
		f.stage++
		return "", []call{{"litter_wait", with(f.runs[0])}, {"litter_wait", with(f.runs[1], "timeoutMs", 10000)}}, nil
	case 3:
		if err := require(len(tools) == 7, "parent expected two settled outcomes"); err != nil {
			return "", nil, err
		}
		for index, value := range tools[5:] {
			if err := require(value.State == "completed" && value.Outcome.Run == f.runs[index] && value.Outcome.State == "completed" && value.Outcome.Model == "fixture/held" && value.Outcome.Usage.TotalTokens == 30 && value.Outcome.Truncated && value.Outcome.ReportProvenance == "child_assistant" && len(value.Outcome.Text) <= 8192, "child outcome, ID correlation, exact model, usage, provenance, or report bound failed"); err != nil {
				return "", nil, err
			}
		}
		f.stage++
		return "", []call{{"litter_inspect", map[string]any{"id": f.runs[0].ID}}}, nil
	case 4:
		if err := require(len(tools) == 8 && tools[7].Child.ref == f.runs[0] && tools[7].Child.State == "completed" && tools[7].Outcome.Run == f.runs[0] && tools[7].Outcome.Text != "", "ID-only inspection omitted the settled child or its outcome"); err != nil {
			return "", nil, err
		}
		f.stage++
		return "", []call{{"litter_message", with(f.runs[0], "text", "resume marker", "resume", true)}}, nil
	case 5:
		if err := require(len(tools) == 9 && tools[8].Child.ID == f.runs[0].ID, "resume changed the stable child ID"); err != nil {
			return "", nil, err
		}
		f.resumed = tools[8].Child.ref
		f.stage++
		return "", []call{{"litter_wait", with(f.resumed, "timeoutMs", 10000)}}, nil
	case 6:
		if err := require(len(tools) == 10 && tools[9].State == "completed" && tools[9].Outcome.Run == f.runs[0] && tools[9].Outcome.Usage.TotalTokens == 15 && tools[9].Outcome.ReportProvenance == "child_assistant", "resumed child did not complete under its stable ID"); err != nil {
			return "", nil, err
		}
		f.stage++
		return "", []call{{"litter_message", with(f.runs[0], "text", "settled follow-up marker", "mode", "follow_up")}}, nil
	case 7:
		if err := require(len(tools) == 11 && tools[10].State == "rejected", "settled child accepted a follow-up without resume"); err != nil {
			return "", nil, err
		}
		f.stage++
		return "", []call{{"litter_spawn", map[string]any{"type": "worker", "task": "worker-write", "name": "writer", "model": "fixture/worker"}}}, nil
	case 8:
		if err := require(len(tools) == 12 && tools[11].Child.ID != "", "worker admission failed"); err != nil {
			return "", nil, err
		}
		f.worker = tools[11].Child.ref
		f.stage++
		return "", []call{{"litter_wait", with(f.worker, "timeoutMs", 10000)}}, nil
	case 9:
		if err := require(len(tools) == 13 && tools[12].State == "completed" && strings.Contains(tools[12].Outcome.Text, "worker readback verified"), "actual parent-mediated write/readback failed"); err != nil {
			return "", nil, err
		}
		f.stage++
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "stop-D", "name": "D", "model": "fixture/held"}}}, nil
	case 10:
		if err := require(len(tools) == 14 && tools[13].Child.ID != "", "stop target admission failed"); err != nil {
			return "", nil, err
		}
		f.stopped = tools[13].Child.ref
		f.stage++
		return "", []call{{"litter_stop", map[string]string{"id": f.stopped.ID}}}, nil
	case 11:
		if err := require(len(tools) == 15 && tools[14].StoppedCount == 1 && tools[14].Outcome.State == "stopped", "independent stop failed"); err != nil {
			return "", nil, err
		}
		f.done = true
		return "stock lifecycle complete", nil, nil
	}
	return "", nil, errors.New("unexpected core parent stage")
}

func with(run ref, fields ...any) map[string]any {
	values := map[string]any{"id": run.ID}
	for i := 0; i < len(fields); i += 2 {
		values[fields[i].(string)] = fields[i+1]
	}
	return values
}

func (f *fixture) parentNested(ctx context.Context, body modelRequest) (string, []call, error) {
	tools, err := toolResults(body)
	if err != nil {
		return "", nil, err
	}
	switch f.stage {
	case 0:
		f.stage++
		return "", []call{{"litter_spawn", map[string]any{"type": "coordinator", "task": "Nested parent task.", "name": "nested-parent", "model": "fixture/nester"}}}, nil
	case 1:
		if len(tools) != 1 || tools[0].Child.ID == "" || tools[0].Child.Type != "coordinator" || tools[0].Child.CWD != f.expectedCWD {
			return "", nil, errors.New("nested parent admission failed")
		}
		f.nestedParent = tools[0].Child.ref
		if err := f.wait(ctx, func() bool { return f.grandchild.ID != "" && f.depthRejected && f.held["nested-grandchild"] != nil }); err != nil {
			return "", nil, fmt.Errorf("model-called grandchild and depth rejection: %w", err)
		}
		f.stage++
		return "", []call{{"litter_wait", with(f.nestedParent, "timeoutMs", 10000)}}, nil
	case 2:
		if len(tools) != 2 || tools[1].State != "completed" || tools[1].Outcome.Run != f.nestedParent {
			return "", nil, errors.New("nested parent did not complete independently")
		}
		f.stage++
		return "", []call{{"litter_list", map[string]any{}}}, nil
	case 3:
		if len(tools) != 3 || len(tools[2].Children) != 2 {
			return "", nil, errors.New("root list omitted nested descendant")
		}
		found := false
		for _, child := range tools[2].Children {
			if child.ID == f.grandchild.ID && child.Parent != nil && *child.Parent == f.nestedParent.ID && child.Depth == 2 && child.Type == "coordinator" && child.CWD == f.expectedCWD {
				found = true
			}
		}
		if !found {
			return "", nil, errors.New("nested child ancestry changed")
		}
		f.stage++
		return "", []call{{"litter_stop", map[string]string{"id": f.nestedParent.ID}}}, nil
	case 4:
		if len(tools) != 4 || tools[3].StoppedCount != 1 {
			return "", nil, errors.New("settled parent subtree stop did not stop its live descendant")
		}
		f.stage++
		return "", []call{{"litter_wait", with(f.grandchild, "timeoutMs", 10000)}}, nil
	case 5:
		if len(tools) != 5 || tools[4].State != "stopped" {
			return "", nil, errors.New("grandchild did not stop")
		}
		f.done = true
		return "nested stock lifecycle complete", nil, nil
	}
	return "", nil, errors.New("unexpected nested parent stage")
}

func toolNames(body modelRequest) []string {
	names := []string{}
	for _, tool := range body.Tools {
		names = append(names, tool.Function.Name)
	}
	sort.Strings(names)
	return names
}

func contains(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func (f *fixture) childRequest(ctx context.Context, body modelRequest) (string, []call, error) {
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
	if strings.Contains(user, "resume marker") {
		history, _ := json.Marshal(body.Messages)
		if (!strings.Contains(user, "hold-A") && !strings.Contains(user, "hold-B")) || !strings.Contains(string(history), "stock fixture read marker") {
			return "", nil, errors.New("resumed Session lost original user history")
		}
		return "resume completed with retained history", nil, nil
	}
	task := ""
	for _, name := range []string{"hold-A", "hold-B", "hold-C", "stop-D"} {
		if strings.Contains(user, name) {
			task = name
			break
		}
	}
	if task == "" {
		return "", nil, errors.New("unknown held task")
	}
	if task != "stop-D" && len(tools) == 0 {
		return "", []call{{"read", map[string]string{"path": "input.txt"}}}, nil
	}
	if task != "stop-D" && (len(tools) != 1 || !strings.Contains(content(tools[0].Content), "stock fixture read marker")) {
		return "", nil, errors.New("parent-mediated read failed")
	}
	return f.hold(ctx, task), nil, nil
}

func (f *fixture) workerRequest(body modelRequest) (string, []call, error) {
	tools := []message{}
	for _, message := range body.Messages {
		if message.Role == "tool" {
			tools = append(tools, message)
		}
	}
	if len(tools) == 0 {
		return "", []call{{"write", map[string]string{"path": "output.txt", "content": "stock worker write marker\n"}}}, nil
	}
	if len(tools) == 1 {
		return "", []call{{"read", map[string]string{"path": "output.txt"}}}, nil
	}
	if len(tools) != 2 || !strings.Contains(content(tools[1].Content), "stock worker write marker") {
		return "", nil, errors.New("worker readback failed")
	}
	return "worker readback verified", nil, nil
}

func (f *fixture) parentCWD(body modelRequest) (string, []call, error) {
	tools, err := toolResults(body)
	if err != nil {
		return "", nil, err
	}
	switch f.stage {
	case 0:
		f.stage++
		return "", []call{{"litter_spawn", map[string]any{"type": "worker", "task": "cwd-worker", "name": "cwd-worker", "model": "fixture/cwd_worker", "cwd": f.relativeCWD}}}, nil
	case 1:
		if len(tools) != 1 || tools[0].Child.ID == "" || tools[0].Child.Type != "worker" || tools[0].Child.CWD != f.expectedCWD {
			return "", nil, fmt.Errorf("explicit relative cwd was not resolved in the returned child snapshot: %#v", tools)
		}
		f.worker = tools[0].Child.ref
		f.stage++
		return "", []call{{"litter_wait", with(f.worker)}}, nil
	case 2:
		if len(tools) != 2 || tools[1].State != "completed" || tools[1].Outcome.Run != f.worker || tools[1].Outcome.Model != "fixture/cwd_worker" {
			return "", nil, errors.New("cwd worker did not complete with its stable ID and exact model")
		}
		f.done = true
		return "cwd operations completed", nil, nil
	}
	return "", nil, errors.New("unexpected cwd parent stage")
}

func (f *fixture) parentUnbounded(body modelRequest) (string, []call, error) {
	var tools []result
	if f.stage < 3 {
		var err error
		tools, err = toolResults(body)
		if err != nil {
			return "", nil, err
		}
	}
	switch f.stage {
	case 0:
		f.stage++
		return "", []call{{"litter_spawn", map[string]any{"type": "worker", "task": "unbounded-child", "name": "unbounded-child", "model": "fixture/unbounded_child"}}}, nil
	case 1:
		if len(tools) != 1 || tools[0].Child.ID == "" || tools[0].Child.Type != "worker" {
			return "", nil, errors.New("unbounded worker was not admitted")
		}
		f.worker = tools[0].Child.ref
		f.stage++
		return "", []call{{"litter_wait", with(f.worker, "timeoutMs", 170000)}}, nil
	case 2:
		if len(tools) != 2 || tools[1].State != "completed" || tools[1].Outcome.Run != f.worker || tools[1].Outcome.Model != "fixture/unbounded_child" || tools[1].Outcome.Text != "unbounded child marker recorded" || tools[1].Outcome.ReportProvenance != "child_assistant" {
			return "", nil, errors.New("worker did not survive beyond the former run deadline")
		}
		f.stage++
		return "", []call{{"read", map[string]string{"path": "unbounded-marker.txt"}}}, nil
	case 3:
		messages := toolMessages(body)
		if len(messages) != 3 || !strings.Contains(content(messages[2].Content), "unbounded marker") {
			return "", nil, errors.New("parent could not independently read the marker written after the provider hold")
		}
		f.done = true
		return "unbounded child completed", nil, nil
	}
	return "", nil, errors.New("unexpected unbounded parent stage")
}

func (f *fixture) unboundedChildRequest(ctx context.Context, body modelRequest) (string, []call, error) {
	tools := []message{}
	for _, value := range body.Messages {
		if value.Role == "tool" {
			tools = append(tools, value)
		}
	}
	if len(tools) == 0 {
		timer := time.NewTimer(125 * time.Second)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return "", nil, fmt.Errorf("provider request ended before the 125-second hold: %w", ctx.Err())
		case <-timer.C:
		}
		return "", []call{{"write", map[string]string{"path": "unbounded-marker.txt", "content": "unbounded marker\n"}}}, nil
	}
	if len(tools) != 1 || content(tools[0].Content) != "Successfully wrote to "+filepath.Join(f.expectedCWD, "unbounded-marker.txt") {
		return "", nil, errors.New("provider hold completed but the child marker write was not observed")
	}
	return "unbounded child marker recorded", nil, nil
}

func (f *fixture) cwdWorkerRequest(body modelRequest) (string, []call, error) {
	if got := systemPrompt(body); got != "USER_ONLY_WORKER_INSTRUCTIONS" {
		return "", nil, fmt.Errorf("user-only worker system prompt = %q", got)
	}
	tools := []message{}
	for _, value := range body.Messages {
		if value.Role == "tool" {
			tools = append(tools, value)
		}
	}
	switch len(tools) {
	case 0:
		return "", []call{{"read", map[string]string{"path": "cwd-input.txt"}}}, nil
	case 1:
		if !strings.Contains(content(tools[0].Content), "cwd original marker") {
			return "", nil, errors.New("cwd worker did not read the sibling input file")
		}
		return "", []call{{"edit", map[string]any{"path": "cwd-input.txt", "edits": []any{map[string]string{"oldText": "cwd original marker", "newText": "cwd edited marker"}}}}}, nil
	case 2:
		return "", []call{{"read", map[string]string{"path": "cwd-input.txt"}}}, nil
	case 3:
		if !strings.Contains(content(tools[2].Content), "cwd edited marker") {
			return "", nil, errors.New("edit did not change the sibling input file")
		}
		return "", []call{{"write", map[string]string{"path": "cwd-output.txt", "content": "cwd output marker\n"}}}, nil
	case 4:
		return "", []call{{"bash", map[string]string{"command": "pwd > cwd-bash.txt"}}}, nil
	case 5:
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "cwd-nested", "name": "cwd-nested", "model": "fixture/cwd_nested"}}}, nil
	case 6:
		var value result
		if err := json.Unmarshal([]byte(content(tools[5].Content)), &value); err != nil || value.Child.ID == "" || value.Child.Type != "scout" || value.Child.CWD != f.expectedCWD {
			return "", nil, fmt.Errorf("relative nested child did not inherit its parent's cwd: %#v, %v", value.Child, err)
		}
		f.grandchild = value.Child.ref
		return "", []call{{"litter_wait", with(f.grandchild)}}, nil
	case 7:
		var value result
		if err := json.Unmarshal([]byte(content(tools[6].Content)), &value); err != nil || value.State != "completed" || value.Outcome.Run != f.grandchild || value.Outcome.Model != "fixture/cwd_nested" || value.Outcome.Text != "nested cwd read marker verified" {
			return "", nil, fmt.Errorf("nested cwd child did not complete with its exact ID and model: %#v, %v", value, err)
		}
		return "cwd worker and nested child completed", nil, nil
	}
	return "", nil, errors.New("unexpected cwd worker stage")
}

func (f *fixture) cwdNestedRequest(body modelRequest) (string, []call, error) {
	if got := systemPrompt(body); got != "USER_ONLY_SCOUT_INSTRUCTIONS" {
		return "", nil, fmt.Errorf("user-only scout system prompt = %q", got)
	}
	tools := toolMessages(body)
	switch len(tools) {
	case 0:
		return "", []call{{"read", map[string]string{"path": "cwd-output.txt"}}}, nil
	case 1:
		if !strings.Contains(content(tools[0].Content), "cwd output marker") {
			return "", nil, errors.New("nested scout did not read the parent's output from its inherited cwd")
		}
		return "nested cwd read marker verified", nil, nil
	}
	return "", nil, errors.New("unexpected nested cwd worker stage")
}

func (f *fixture) serve(w http.ResponseWriter, request *http.Request) {
	defer request.Body.Close()
	var body modelRequest
	err := json.NewDecoder(io.LimitReader(request.Body, 8<<20)).Decode(&body)
	if err != nil {
		f.fail(w, err)
		return
	}
	f.mu.Lock()
	f.requests = append(f.requests, body)
	f.signalLocked()
	f.mu.Unlock()
	if err := requireFixtureModelTools(body); err != nil {
		f.fail(w, err)
		return
	}
	if !strings.HasPrefix(body.Model, "parent") {
		raw, _ := json.Marshal(body.Messages)
		for _, private := range []string{"PARENT_PRIVATE_MARKER", "PRIVATE_PROJECT_APPEND_MARKER", "PRIVATE_AGENT_APPEND_MARKER"} {
			if strings.Contains(string(raw), private) {
				f.fail(w, errors.New("parent context leaked into a child"))
				return
			}
		}
		if body.Model == "held" || body.Model == "lifecycle_child" || body.Model == "denied" || body.Model == "missing_file" || body.Model == "roles" || body.Model == "message_child" || body.Model == "completion_child" || body.Model == "cwd_nested" || body.Model == "nester" || body.Model == "grandchild" {
			prompt := ""
			for _, message := range body.Messages {
				if message.Role == "system" {
					prompt = content(message.Content)
					break
				}
			}
			if !strings.HasPrefix(prompt, "input.txt") || strings.Contains(prompt, "stock fixture read marker") {
				f.fail(w, errors.New("named instruction text was interpreted as a file path"))
				return
			}
		}
	}
	var text string
	var calls []call
	switch body.Model {
	case "parent":
		text, calls, err = f.parent(request.Context(), body)
	case "parent_cwd":
		text, calls, err = f.parentCWD(body)
	case "parent_unbounded":
		text, calls, err = f.parentUnbounded(body)
	case "parent_nested":
		text, calls, err = f.parentNested(request.Context(), body)
	case "parent_checks":
		text, calls, err = f.parentChecks(body)
	case "parent_shadow":
		text, calls, err = f.parentShadow(body)
	case "parent_messages":
		text, calls, err = f.parentMessages(request.Context(), body)
	case "parent_lifecycle":
		text, calls, err = f.parentLifecycle(request.Context(), body)
	case "parent_completion":
		text, calls, err = f.parentCompletion(request.Context(), w, body)
	case "completion_child":
		text, calls, err = f.completionChild(request.Context(), body)
	case "lifecycle_child":
		text, calls, err = f.lifecycleChild(request.Context(), body)
	case "denied", "missing_file", "roles", "message_child":
		text, calls, err = f.checkedChild(request.Context(), body)
	case "provider_error":
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": "intentional provider failure", "type": "fixture_provider_error"}})
		return
	case "held":
		text, calls, err = f.childRequest(request.Context(), body)
	case "worker":
		text, calls, err = f.workerRequest(body)
	case "cwd_worker":
		text, calls, err = f.cwdWorkerRequest(body)
	case "cwd_nested":
		text, calls, err = f.cwdNestedRequest(body)
	case "unbounded_child":
		text, calls, err = f.unboundedChildRequest(request.Context(), body)
	case "nester":
		tools, parseErr := toolResults(body)
		err = parseErr
		if err == nil && len(tools) == 0 {
			calls = []call{{"litter_spawn", map[string]any{"type": "coordinator", "task": "Nested grandchild task.", "name": "nested-grandchild", "model": "fixture/grandchild"}}}
		} else if err == nil {
			if len(tools) != 1 || tools[0].Child.ID == "" || tools[0].State != "starting" {
				err = errors.New("nested spawn did not return its frozen admission")
			} else {
				f.mu.Lock()
				f.grandchild = tools[0].Child.ref
				f.signalLocked()
				f.mu.Unlock()
				text = "nested parent complete"
			}
		}
	case "grandchild":
		tools := toolMessages(body)
		if len(tools) == 0 {
			calls = []call{{"read", map[string]string{"path": "input.txt"}}}
		} else if len(tools) == 1 {
			if !strings.Contains(content(tools[0].Content), "stock fixture read marker") {
				err = errors.New("depth-limited coordinator did not read its workspace file")
			} else {
				calls = []call{{"litter_spawn", map[string]any{"type": "coordinator", "task": "Forbidden depth three.", "name": "too-deep", "model": "fixture/nester"}}}
			}
		} else {
			var value result
			err = json.Unmarshal([]byte(content(tools[len(tools)-1].Content)), &value)
			if err == nil && value.State != "rejected" {
				err = errors.New("depth-three admission was not rejected")
			} else if err == nil {
				f.mu.Lock()
				f.depthRejected = true
				f.signalLocked()
				f.mu.Unlock()
				text = f.hold(request.Context(), "nested-grandchild")
			}
		}
	default:
		err = fmt.Errorf("unexpected model %q", body.Model)
	}
	if err != nil {
		f.fail(w, err)
		return
	}
	f.answer(w, body.Model, text, calls)
}

func (f *fixture) fail(w http.ResponseWriter, err error) {
	f.mu.Lock()
	if f.failure == nil {
		f.failure = err
	}
	f.signalLocked()
	f.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(500)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": err.Error(), "type": "fixture_error"}})
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path+".tmp", raw, 0o600); err != nil {
		return err
	}
	return os.Rename(path+".tmp", path)
}

func fixtureConfigFiles(workspace, agentDir, scenario string) map[string]string {
	files := map[string]string{}
	if scenario != "cwd" {
		files[filepath.Join(workspace, "litter.yaml")] = "concurrency: 2\ndepth: 2\nmax_result_bytes: 8192\nagents:\n  scout:\n    instructions: input.txt\n"
	}
	if scenario == "core" {
		files[filepath.Join(agentDir, "litter.yaml")] = "concurrency: 3\nagents:\n  zz_user_only:\n    role: scout\n    instructions: USER_ONLY_SCOUT_INSTRUCTIONS\n    tools: [read]\n    can_delegate: false\n"
	} else if scenario == "cwd" {
		files[filepath.Join(agentDir, "litter.yaml")] = "agents:\n  scout:\n    instructions: USER_ONLY_SCOUT_INSTRUCTIONS\n  worker:\n    instructions: USER_ONLY_WORKER_INSTRUCTIONS\n"
	}
	return files
}

func run(directory, pig, extension, scenario string) error {
	workspace, agentDir := filepath.Join(directory, "workspace"), filepath.Join(directory, "agent")
	for _, dir := range []string{filepath.Join(workspace, ".pig"), agentDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		return fmt.Errorf("resolve bash for isolated worker tools: %w", err)
	}
	bashPath, err = filepath.Abs(bashPath)
	if err != nil {
		return fmt.Errorf("resolve bash path: %w", err)
	}
	expectedCWD, err := filepath.Abs(workspace)
	if err != nil {
		return err
	}
	files := map[string]string{filepath.Join(workspace, "input.txt"): "stock fixture read marker\n", filepath.Join(workspace, ".pig", "APPEND_SYSTEM.md"): "PRIVATE_PROJECT_APPEND_MARKER\n", filepath.Join(agentDir, "APPEND_SYSTEM.md"): "PRIVATE_AGENT_APPEND_MARKER\n"}
	for path, content := range fixtureConfigFiles(workspace, agentDir, scenario) {
		files[path] = content
	}
	relativeCWD := ""
	if scenario == "cwd" {
		expectedCWD = filepath.Join(directory, "sibling dir's")
		if err := os.MkdirAll(expectedCWD, 0o700); err != nil {
			return err
		}
		relativeCWD, err = filepath.Rel(workspace, expectedCWD)
		if err != nil {
			return err
		}
		files[filepath.Join(expectedCWD, "cwd-input.txt")] = "cwd original marker\n"
	}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	f := &fixture{scenario: scenario, held: map[string]chan struct{}{}, changed: make(chan struct{}), checks: map[string]bool{}, aborted: map[string]bool{}, expectedCWD: expectedCWD, relativeCWD: relativeCWD, bashPath: bashPath}
	server := &http.Server{Handler: http.HandlerFunc(f.serve), ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = server.Serve(listener) }()
	defer func() {
		f.release()
		_ = server.Close()
		f.mu.Lock()
		defer f.mu.Unlock()
		_ = writeJSON(filepath.Join(directory, "requests.json"), f.requests)
	}()
	models := []map[string]any{}
	for _, id := range configuredFixtureModelIDs() {
		models = append(models, map[string]any{"id": id, "name": id, "contextWindow": 100000, "maxTokens": 20000})
	}
	if err := writeJSON(filepath.Join(agentDir, "models.json"), map[string]any{"providers": map[string]any{"fixture": map[string]any{"api": "openai-completions", "baseUrl": "http://" + listener.Addr().String() + "/v1", "apiKey": "local-fixture-only", "models": models}}}); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(agentDir, "auth.json"), map[string]any{"fixture": map[string]string{"type": "api_key", "key": "local-fixture-only"}}); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(agentDir, "settings.json"), map[string]any{"cacheWarming": "off", "retry": map[string]bool{"enabled": false}, "compaction": map[string]bool{"enabled": false}}); err != nil {
		return err
	}
	if err := writeJSON(filepath.Join(agentDir, "trust.json"), map[string]bool{workspace: true}); err != nil {
		return err
	}
	stdout, err := os.Create(filepath.Join(directory, "stdout.jsonl"))
	if err != nil {
		return err
	}
	defer stdout.Close()
	stderr, err := os.Create(filepath.Join(directory, "stderr.log"))
	if err != nil {
		return err
	}
	defer stderr.Close()
	timeout := 60 * time.Second
	if scenario == "unbounded" {
		timeout = 180 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if scenario == "lifecycle" || scenario == "completion" {
		return f.runLifecycle(ctx, directory, workspace, agentDir, pig, extension, stdout, stderr)
	}
	model := "fixture/parent"
	if scenario == "unbounded" {
		model = "fixture/parent_unbounded"
	} else if scenario == "cwd" {
		model = "fixture/parent_cwd"
	} else if scenario == "nested" {
		model = "fixture/parent_nested"
		configuration := "concurrency: 2\ndepth: 2\nagents:\n  scout:\n    instructions: input.txt\n  coordinator:\n    role: worker\n    instructions: input.txt\n    tools: [read]\n    can_delegate: true\n"
		if err := os.WriteFile(filepath.Join(workspace, "litter.yaml"), []byte(configuration), 0o600); err != nil {
			return err
		}
	} else if scenario == "checks" {
		model = "fixture/parent_checks"
		configuration := "concurrency: 2\ndepth: 2\nmax_result_bytes: 256\nagents:\n  scout:\n    instructions: input.txt\n  worker:\n    instructions: input.txt\n  fixed:\n    role: scout\n    instructions: input.txt\n    tools: [read]\n    can_delegate: false\n    model: fixture/held\n  custom_scout:\n    role: scout\n    instructions: input.txt\n    tools: [read]\n    can_delegate: false\n"
		if err := os.WriteFile(filepath.Join(workspace, "litter.yaml"), []byte(configuration), 0o600); err != nil {
			return err
		}
	} else if scenario == "messages" {
		model = "fixture/parent_messages"
		if err := os.WriteFile(filepath.Join(workspace, "litter.yaml"), []byte("concurrency: 2\nmax_mailbox: 2\nagents:\n  scout:\n    instructions: input.txt\n"), 0o600); err != nil {
			return err
		}
	} else if scenario == "shadow" {
		model = "fixture/parent_shadow"
	}
	arguments := []string{"--no-extensions", "-e", extension, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--no-session", "--approve", "--offline", "--model", model, "--mode", "json", "--print", "Run the fixture. PARENT_PRIVATE_MARKER"}
	if scenario == "checks" || scenario == "shadow" {
		arguments = append([]string{"-e", filepath.Join(filepath.Dir(extension), "litter-fixture-guard")}, arguments...)
	}
	command := exec.CommandContext(ctx, pig, arguments...)
	prepareProcess(command)
	defer stopProcess(command)
	command.Dir = workspace
	command.Env = fixtureEnvironment(pig, bashPath, directory, agentDir)
	if scenario == "shadow" {
		command.Env = append(command.Env, "LITTER_FIXTURE_SHADOW_BASE=http://"+listener.Addr().String()+"/v1")
	}
	command.Stdout, command.Stderr = stdout, stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("released host exit: %w", err)
	}
	f.mu.Lock()
	failure := f.failure
	f.mu.Unlock()
	if failure != nil {
		return failure
	}
	if !f.done {
		return errors.New("parent fixture did not complete")
	}
	if scenario == "core" {
		output, err := os.ReadFile(filepath.Join(workspace, "output.txt"))
		if err != nil || string(output) != "stock worker write marker\n" {
			return errors.New("actual output file differs from the verified readback")
		}
	}
	if scenario == "cwd" {
		for path, expected := range map[string]string{
			filepath.Join(expectedCWD, "cwd-input.txt"):  "cwd edited marker\n",
			filepath.Join(expectedCWD, "cwd-output.txt"): "cwd output marker\n",
			filepath.Join(expectedCWD, "cwd-bash.txt"):   expectedCWD + "\n",
		} {
			got, err := os.ReadFile(path)
			if err != nil || string(got) != expected {
				return fmt.Errorf("cwd tool effect at %s = %q, %v; want %q", filepath.Base(path), got, err, expected)
			}
		}
	}
	if scenario == "unbounded" {
		marker, err := os.ReadFile(filepath.Join(workspace, "unbounded-marker.txt"))
		if err != nil || string(marker) != "unbounded marker\n" {
			return fmt.Errorf("marker written after the provider hold = %q, %v", marker, err)
		}
	}
	if scenario == "checks" {
		data, err := os.ReadFile(filepath.Join(directory, "permission.jsonl"))
		if err != nil {
			return err
		}
		lines := strings.Split(strings.TrimSpace(string(data)), "\n")
		if len(lines) != 2 {
			return fmt.Errorf("parent permission hook recorded %d calls, want 2", len(lines))
		}
		for _, line := range lines {
			var receipt map[string]any
			if json.Unmarshal([]byte(line), &receipt) != nil || receipt["parentToolCallId"] == nil || receipt["parentToolCallId"] == "" {
				return errors.New("permission denial lost original parent call provenance")
			}
		}
		if _, err := os.Stat(filepath.Join(workspace, "denied-write.txt")); !os.IsNotExist(err) {
			return errors.New("parent-denied write reached disk")
		}
	}
	return writeJSON(filepath.Join(directory, "result.json"), map[string]any{"passed": true, "scenario": scenario, "runs": f.runs, "resumed": f.resumed, "stopped": f.stopped, "nestedParent": f.nestedParent, "grandchild": f.grandchild, "depthRejected": f.depthRejected, "checks": f.checks, "exitCode": command.ProcessState.ExitCode(), "isolatedEnvironment": true})
}

func main() {
	if target := os.Getenv("LITTER_FIXTURE_PROXY_TARGET"); target != "" {
		if err := proxyCompletionExtension(target, os.Getenv("LITTER_FIXTURE_EVIDENCE")); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		return
	}
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pig := flag.String("pig-bin", "", "Released PiG binary")
	extension := flag.String("extension", filepath.Join(filepath.Dir(self), "pig-litter"), "Built pig-litter standalone")
	scenario := flag.String("scenario", "core", "core, cwd, unbounded, nested, checks, messages, lifecycle, completion, or shadow")
	evidenceRoot := flag.String("evidence-root", filepath.Join(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(self)))), ".pstack", "evidence", "go-lifecycle"), "Evidence directory")
	flag.Parse()
	if *pig == "" {
		*pig, err = exec.LookPath("pig")
	}
	if err != nil || *pig == "" || !validScenario(*scenario) {
		fmt.Fprintln(os.Stderr, "pass --pig-bin PATH and a supported --scenario")
		os.Exit(1)
	}
	*pig, err = filepath.Abs(*pig)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	if err := os.MkdirAll(*evidenceRoot, 0o700); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	directory, err := os.MkdirTemp(*evidenceRoot, *scenario+"-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	err = run(directory, *pig, *extension, *scenario)
	result := map[string]any{"directory": directory, "passed": err == nil, "scenario": *scenario}
	if err != nil {
		result["error"] = err.Error()
		_ = os.WriteFile(filepath.Join(directory, "failure.txt"), []byte(err.Error()), 0o600)
	}
	_ = json.NewEncoder(os.Stdout).Encode(result)
	if err != nil {
		os.Exit(1)
	}
}
