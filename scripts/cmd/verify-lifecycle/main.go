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
	"reflect"
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
type modelRequest struct {
	Model    string    `json:"model"`
	Messages []message `json:"messages"`
	Tools    []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}
type ref struct {
	ID         string `json:"id"`
	Generation int    `json:"generation"`
}
type child struct {
	ref
	Parent *string `json:"parent"`
	Depth  int     `json:"depth"`
	Model  string  `json:"model"`
	State  string  `json:"state"`
}
type result struct {
	State               string  `json:"state"`
	Code                string  `json:"code"`
	Report              string  `json:"report"`
	Child               child   `json:"child"`
	Children            []child `json:"children"`
	AvailableAgentTypes []struct {
		Name, Description string
		Tools             []string
	} `json:"availableAgentTypes"`
	TotalAgentTypes int `json:"totalAgentTypes"`
	Outcome         struct {
		State, Text, Model, Error string
		Truncated                 bool
		Usage                     struct{ TotalTokens int }
	} `json:"outcome"`
	Entries          []json.RawMessage `json:"entries"`
	Mailbox          []json.RawMessage `json:"mailbox"`
	StoppedCount     int               `json:"stoppedCount"`
	HistoryAvailable bool              `json:"historyAvailable"`
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
	for _, message := range body.Messages {
		if message.Role == "tool" {
			var value result
			if err := json.Unmarshal([]byte(content(message.Content)), &value); err != nil {
				return nil, err
			}
			results = append(results, value)
		}
	}
	return results, nil
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
		if err := require(len(f.runs) == 2 && rejected == 1 && f.runs[0].ID != f.runs[1].ID, "capacity did not admit two independent children and reject the third"); err != nil {
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
		if err := require(tools[3].TotalAgentTypes == 2 && len(tools[3].AvailableAgentTypes) == 2 && tools[3].AvailableAgentTypes[0].Name == "scout" && tools[3].AvailableAgentTypes[1].Name == "worker", "named agent listing changed"); err != nil {
			return "", nil, err
		}
		f.release()
		f.stage++
		return "", []call{{"litter_wait", with(f.runs[0], "timeoutMs", 10000)}, {"litter_wait", with(f.runs[1], "timeoutMs", 10000)}}, nil
	case 3:
		if err := require(len(tools) == 7, "parent expected two settled outcomes"); err != nil {
			return "", nil, err
		}
		for _, value := range tools[5:] {
			if err := require(value.State == "completed" && value.Outcome.Model == "fixture/held" && value.Outcome.Usage.TotalTokens == 30 && value.Outcome.Truncated && len(value.Outcome.Text) <= 8192, "child outcome, exact model, generation usage, or report bound failed"); err != nil {
				return "", nil, err
			}
		}
		f.stage++
		return "", []call{{"litter_inspect", map[string]any{"id": f.runs[0].ID, "transcript": true, "offset": 0, "limit": 10}}}, nil
	case 4:
		if err := require(len(tools) == 8 && len(tools[7].Entries) > 0 && len(tools[7].Entries) <= 10 && len(tools[7].Mailbox) == 0, "bounded transcript inspection failed"); err != nil {
			return "", nil, err
		}
		f.stage++
		return "", []call{{"litter_message", with(f.runs[0], "text", "resume marker", "resume", true)}}, nil
	case 5:
		if err := require(len(tools) == 9 && tools[8].Child.ID == f.runs[0].ID && tools[8].Child.Generation == 2, "resume lost retained ID or generation"); err != nil {
			return "", nil, err
		}
		f.resumed = tools[8].Child.ref
		f.stage++
		return "", []call{{"litter_message", with(f.runs[0], "text", "stale marker", "mode", "follow_up")}, {"litter_wait", with(f.resumed, "timeoutMs", 10000)}}, nil
	case 6:
		if err := require(len(tools) == 11 && tools[9].State == "rejected" && tools[10].State == "completed" && tools[10].Outcome.Usage.TotalTokens == 15, "stale generation or resumed usage failed"); err != nil {
			return "", nil, err
		}
		f.stage++
		return "", []call{{"litter_spawn", map[string]any{"type": "worker", "task": "worker-write", "name": "writer", "model": "fixture/worker"}}}, nil
	case 7:
		if err := require(len(tools) == 12 && tools[11].Child.ID != "", "worker admission failed"); err != nil {
			return "", nil, err
		}
		f.worker = tools[11].Child.ref
		f.stage++
		return "", []call{{"litter_wait", with(f.worker, "timeoutMs", 10000)}}, nil
	case 8:
		if err := require(len(tools) == 13 && tools[12].State == "completed" && strings.Contains(tools[12].Outcome.Text, "worker readback verified"), "actual parent-mediated write/readback failed"); err != nil {
			return "", nil, err
		}
		f.stage++
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "stop-D", "name": "D", "model": "fixture/held"}}}, nil
	case 9:
		if err := require(len(tools) == 14 && tools[13].Child.ID != "", "stop target admission failed"); err != nil {
			return "", nil, err
		}
		f.stopped = tools[13].Child.ref
		f.stage++
		return "", []call{{"litter_stop", f.stopped}}, nil
	case 10:
		if err := require(len(tools) == 15 && tools[14].StoppedCount == 1 && tools[14].Outcome.State == "stopped", "independent stop failed"); err != nil {
			return "", nil, err
		}
		f.done = true
		return "stock lifecycle complete", nil, nil
	}
	return "", nil, errors.New("unexpected core parent stage")
}

func with(run ref, fields ...any) map[string]any {
	values := map[string]any{"id": run.ID, "generation": run.Generation}
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
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "Nested parent task.", "name": "nested-parent", "model": "fixture/nester"}}}, nil
	case 1:
		if len(tools) != 1 || tools[0].Child.ID == "" {
			return "", nil, errors.New("nested parent admission failed")
		}
		f.nestedParent = tools[0].Child.ref
		if err := f.wait(ctx, func() bool { return f.grandchild.ID != "" && f.depthRejected && f.held["nested-grandchild"] != nil }); err != nil {
			return "", nil, fmt.Errorf("model-called grandchild and depth rejection: %w", err)
		}
		f.stage++
		return "", []call{{"litter_wait", with(f.nestedParent, "timeoutMs", 10000)}}, nil
	case 2:
		if len(tools) != 2 || tools[1].State != "completed" {
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
			if child.ID == f.grandchild.ID && child.Parent != nil && *child.Parent == f.nestedParent.ID && child.Depth == 2 {
				found = true
			}
		}
		if !found {
			return "", nil, errors.New("nested child ancestry changed")
		}
		f.stage++
		return "", []call{{"litter_stop", f.nestedParent}}, nil
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

func (f *fixture) childRequest(ctx context.Context, body modelRequest) (string, []call, error) {
	expected := []string{"read", "litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}
	sort.Strings(expected)
	if !reflect.DeepEqual(toolNames(body), expected) {
		return "", nil, fmt.Errorf("readonly tool ceiling changed: %v", toolNames(body))
	}
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
		if !strings.Contains(user, "hold-A") && !strings.Contains(user, "hold-B") {
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
	expected := []string{"read", "write", "edit", "litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}
	sort.Strings(expected)
	if !reflect.DeepEqual(toolNames(body), expected) {
		return "", nil, fmt.Errorf("worker tool ceiling changed: %v", toolNames(body))
	}
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
	if !strings.HasPrefix(body.Model, "parent") {
		raw, _ := json.Marshal(body.Messages)
		for _, private := range []string{"PARENT_PRIVATE_MARKER", "PRIVATE_PROJECT_APPEND_MARKER", "PRIVATE_AGENT_APPEND_MARKER"} {
			if strings.Contains(string(raw), private) {
				f.fail(w, errors.New("parent context leaked into a child"))
				return
			}
		}
		if body.Model != "worker" {
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
	case "lifecycle_child":
		text, calls, err = f.lifecycleChild(request.Context(), body)
	case "denied", "missing_file", "roles", "history", "message_child":
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
	case "nester":
		tools, parseErr := toolResults(body)
		err = parseErr
		if err == nil && len(tools) == 0 {
			calls = []call{{"litter_spawn", map[string]any{"type": "scout", "task": "Nested grandchild task.", "name": "nested-grandchild", "model": "fixture/grandchild"}}}
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
		tools, parseErr := toolResults(body)
		err = parseErr
		if err == nil && len(tools) == 0 {
			calls = []call{{"litter_spawn", map[string]any{"type": "scout", "task": "Forbidden depth three.", "name": "too-deep", "model": "fixture/held"}}}
		} else if err == nil {
			if len(tools) != 1 || tools[0].State != "rejected" {
				err = errors.New("depth-three admission was not rejected")
			} else {
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

func run(directory, pig, extension, scenario string) error {
	workspace, agentDir := filepath.Join(directory, "workspace"), filepath.Join(directory, "agent")
	for _, dir := range []string{filepath.Join(workspace, ".pig"), agentDir} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	files := map[string]string{filepath.Join(workspace, "litter.yaml"): "concurrency: 2\ndepth: 2\nmax_result_bytes: 8192\nagents:\n  scout:\n    instructions: input.txt\n", filepath.Join(workspace, "input.txt"): "stock fixture read marker\n", filepath.Join(workspace, ".pig", "APPEND_SYSTEM.md"): "PRIVATE_PROJECT_APPEND_MARKER\n", filepath.Join(agentDir, "APPEND_SYSTEM.md"): "PRIVATE_AGENT_APPEND_MARKER\n"}
	for path, content := range files {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return err
		}
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	f := &fixture{scenario: scenario, held: map[string]chan struct{}{}, changed: make(chan struct{}), checks: map[string]bool{}, aborted: map[string]bool{}}
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
	for _, id := range []string{"parent", "held", "worker", "parent_nested", "nester", "grandchild", "parent_checks", "denied", "provider_error", "missing_file", "roles", "history", "parent_messages", "message_child", "parent_lifecycle", "lifecycle_child"} {
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if scenario == "lifecycle" {
		return f.runLifecycle(ctx, directory, workspace, agentDir, pig, extension, stdout, stderr)
	}
	model := "fixture/parent"
	if scenario == "nested" {
		model = "fixture/parent_nested"
	} else if scenario == "checks" {
		model = "fixture/parent_checks"
		configuration := "concurrency: 2\ndepth: 2\nmax_history_bytes: 65536\nmax_result_bytes: 256\nagents:\n  scout:\n    instructions: input.txt\n  worker:\n    instructions: input.txt\n  fixed:\n    role: scout\n    instructions: input.txt\n    tools: [read]\n    can_delegate: false\n    model: fixture/held\n"
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
	command.Env = fixtureEnvironment(pig, directory, agentDir)
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
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	pig := flag.String("pig-bin", "", "Released PiG binary")
	extension := flag.String("extension", filepath.Join(filepath.Dir(self), "pig-litter"), "Built pig-litter standalone")
	scenario := flag.String("scenario", "core", "core, nested, checks, messages, lifecycle, or shadow")
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
