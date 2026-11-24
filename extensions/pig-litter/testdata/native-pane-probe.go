package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/cli"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

var logMu sync.Mutex
var boundaryMu sync.Mutex
var boundaryCounter uint64
var approvedBoundary uint64

func record(name string, data any) {
	logMu.Lock()
	defer logMu.Unlock()
	file, err := os.OpenFile(filepath.Join(os.Getenv("PROBE_ROOT"), name+".jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		panic(err)
	}
	defer file.Close()
	if err := json.NewEncoder(file).Encode(data); err != nil {
		panic(err)
	}
}

type decision struct {
	Kind     string `json:"kind"`
	Revision string `json:"revision"`
	Boundary uint64 `json:"boundary"`
	Result   string `json:"result"`
	Feedback string `json:"feedback"`
	Continue bool   `json:"continue"`
}

func rootDecision(kind string, detail any) decision {
	conn, err := net.DialTimeout("unix", filepath.Join(os.Getenv("PROBE_ROOT"), "root.sock"), 2*time.Second)
	if err != nil {
		return decision{Kind: "unavailable"}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if json.NewEncoder(conn).Encode(map[string]any{"kind": kind, "detail": detail, "token": os.Getenv("PROBE_TOKEN")}) != nil {
		return decision{Kind: "unavailable"}
	}
	var result decision
	if json.NewDecoder(conn).Decode(&result) != nil {
		return decision{Kind: "unavailable"}
	}
	return result
}

func watchRoot(ctx context.Context, api extension.API) {
	lost := func() {
		record("events", map[string]any{"event": "root_lost"})
		if ext := extension.FromContext(ctx); ext != nil {
			_ = ext.Abort()
			_ = ext.Shutdown()
		}
	}
	conn, err := net.DialTimeout("unix", filepath.Join(os.Getenv("PROBE_ROOT"), "root.sock"), 2*time.Second)
	if err != nil {
		lost()
		return
	}
	if json.NewEncoder(conn).Encode(map[string]any{"kind": "monitor", "token": os.Getenv("PROBE_TOKEN")}) != nil {
		conn.Close()
		lost()
		return
	}
	decoder := json.NewDecoder(conn)
	for {
		var control struct {
			ID      string          `json:"id"`
			Kind    string          `json:"kind"`
			Content string          `json:"content"`
			Details json.RawMessage `json:"details"`
			Audit   json.RawMessage `json:"audit"`
		}
		if decoder.Decode(&control) != nil {
			break
		}
		switch control.Kind {
		case "completion":
			trigger := false
			api.SendMessage(extension.SendMessagePayload{CustomType: "litter_completion", Content: control.Content, Details: control.Details}, &extension.SendMessageOptions{TriggerTurn: &trigger})
		case "audit":
			api.AppendEntry("litter_completion", control.Audit)
		case "queue":
			ext := extension.FromContext(ctx)
			idle, _ := ext.IsIdle()
			before, _ := ext.HasPendingMessages()
			api.SendUserMessage(control.Content, &extension.SendUserMessageOptions{DeliverAs: extension.DeliverAsFollowUp})
			after, _ := ext.HasPendingMessages()
			record("events", map[string]any{"event": "control_applied", "id": control.ID, "kind": control.Kind, "idle": idle, "before": before, "after": after})
			continue
		case "inspect":
			ext := extension.FromContext(ctx)
			idle, _ := ext.IsIdle()
			pending, _ := ext.HasPendingMessages()
			record("events", map[string]any{"event": "control_applied", "id": control.ID, "kind": control.Kind, "idle": idle, "pending": pending})
			continue
		case "queue-hold":
			go func() {
				ext := extension.FromContext(ctx)
				idle, _ := ext.IsIdle()
				pending, _ := ext.HasPendingMessages()
				record("events", map[string]any{"event": "queue_checked", "id": control.ID, "idle": idle, "pending": pending})
				for rootDecision("queue-release", nil).Kind != "allow" {
					time.Sleep(10 * time.Millisecond)
				}
				api.SendUserMessage(control.Content, &extension.SendUserMessageOptions{DeliverAs: extension.DeliverAsFollowUp})
				record("events", map[string]any{"event": "control_applied", "id": control.ID, "kind": control.Kind})
			}()
			continue
		default:
			continue
		}
		record("events", map[string]any{"event": "control_applied", "id": control.ID, "kind": control.Kind})
	}
	conn.Close()
	lost()
}

func identity(ctx context.Context) map[string]any {
	ext := extension.FromContext(ctx)
	cwd, _ := ext.CWD()
	model, _ := ext.Model()
	manager, _ := ext.SessionManager()
	result := map[string]any{"cwd": cwd, "model": extension.ModelValue{Model: model}, "tools": ext.GetActiveTools()}
	if manager != nil {
		result["uuid"] = manager.GetSessionId()
		result["sessionFile"] = manager.GetSessionFile()
		result["leaf"] = manager.GetLeafID()
	}
	return result
}

func factory(api extension.API) error {
	api.OnSessionStart(func(ctx context.Context, event extension.SessionStartEvent) error {
		record("events", map[string]any{"event": "session_start", "reason": event.Reason, "identity": identity(ctx)})
		go watchRoot(ctx, api)
		return nil
	})
	api.OnInput(func(ctx context.Context, event extension.InputEvent) (extension.InputEventResult, error) {
		decision := rootDecision("decision", nil)
		record("events", map[string]any{"event": "input", "text": event.Text, "decision": decision})
		if decision.Kind != "allow" {
			return extension.InputEventResultHandled{}, nil
		}
		if event.Text == "QUEUED_ONE" {
			for rootDecision("queue-input", nil).Kind != "allow" {
				select {
				case <-ctx.Done():
					return extension.InputEventResultHandled{}, nil
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		return extension.InputEventResultContinue{}, nil
	})
	api.OnToolCall(func(ctx context.Context, event extension.ToolCallEvent) (extension.ToolCallEventResult, error) {
		call := event.(extension.CustomToolCallEvent)
		path, _ := call.Input["path"].(string)
		decision := rootDecision("tool", map[string]any{"name": call.ToolName, "call": call.ToolCallID, "parent": call.ParentToolCallID, "path": path})
		denied := decision.Kind != "allow"
		record("events", map[string]any{"event": "tool_call", "tool": call.ToolName, "id": call.ToolCallID, "parent": call.ParentToolCallID, "input": call.Input, "decision": decision, "denied": denied})
		if denied {
			return extension.ToolCallEventResult{Block: true, Reason: "PROBE_POLICY_DENIED"}, nil
		}
		return extension.ToolCallEventResult{}, nil
	})
	for _, name := range []string{"litter_spawn", "litter_wait"} {
		api.RegisterTool(extension.ToolDefinition{
			Name: name, Label: name, Description: name,
			Parameters: json.RawMessage(`{"type":"object","properties":{"type":{"type":"string"},"task":{"type":"string"},"id":{"type":"string"},"timeoutMs":{"type":"integer"}},"additionalProperties":false}`),
			Execute: func(ctx context.Context, callID string, params json.RawMessage, _ extension.AgentToolUpdateCallback) (extension.AgentToolResult, error) {
				manager, err := extension.FromContext(ctx).SessionManager()
				if err != nil {
					return extension.AgentToolResult{}, err
				}
				leaf := ""
				if id := manager.GetLeafID(); id != nil {
					leaf = *id
				}
				decision := rootDecision(name, map[string]any{"call": callID, "leaf": leaf, "args": params})
				return extension.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: decision.Result}}, IsError: decision.Kind != "allow"}, nil
			},
		})
	}
	api.OnToolExecutionEnd(func(ctx context.Context, event extension.ToolExecutionEndEvent) error {
		if event.ToolName == "litter_spawn" || event.ToolName == "litter_wait" {
			rootDecision("tool-end", map[string]any{"call": event.ToolCallID, "name": event.ToolName, "result": event.Result, "isError": event.IsError})
		}
		return nil
	})
	api.OnUserBash(func(ctx context.Context, event extension.UserBashEvent) (extension.UserBashEventResult, error) {
		record("events", map[string]any{"event": "user_bash", "command": event.Command})
		return extension.UserBashEventResult{Result: map[string]any{"output": "PROBE_BASH_DENIED", "exitCode": 1, "cancelled": false, "truncated": false}}, nil
	})
	api.OnSessionBeforeSwitch(func(context.Context, extension.SessionBeforeSwitchEvent) (extension.SessionBeforeSwitchResult, error) {
		return extension.SessionBeforeSwitchResult{Cancel: true}, nil
	})
	api.OnSessionBeforeFork(func(context.Context, extension.SessionBeforeForkEvent) (extension.SessionBeforeForkResult, error) {
		return extension.SessionBeforeForkResult{Cancel: true}, nil
	})
	api.OnSessionBeforeTree(func(context.Context, extension.SessionBeforeTreeEvent) (extension.SessionBeforeTreeResult, error) {
		return extension.SessionBeforeTreeResult{Cancel: true}, nil
	})
	api.OnAgentBeforeSettle(func(ctx context.Context, event *extension.AgentBeforeSettleEvent) (extension.AgentBeforeSettleEventResult, error) {
		if event.Outcome != extension.AgentActivityCompleted || event.Continue {
			return extension.BoundaryResult{}, nil
		}
		boundaryMu.Lock()
		boundaryCounter++
		boundary := boundaryCounter
		boundaryMu.Unlock()
		report := ""
		for index := len(event.Context.ContextMessages) - 1; index >= 0; index-- {
			message := event.Context.ContextMessages[index].Assistant
			if message == nil {
				continue
			}
			for _, block := range message.Content {
				if text, ok := block.(ai.TextContent); ok {
					report += text.Text
				}
			}
			break
		}
		decision := rootDecision("settle", map[string]any{"boundary": boundary, "outcome": event.Outcome, "continuing": event.Continue, "canContinue": event.Context.CanContinue, "report": report})
		record("events", map[string]any{"event": "before_settle", "decision": decision, "outcome": event.Outcome, "canContinue": event.Context.CanContinue})
		if decision.Kind != "allow" || decision.Boundary != boundary {
			if decision.Kind == "allow" && decision.Boundary != boundary {
				go func() { _ = extension.FromContext(ctx).Abort(); _ = extension.FromContext(ctx).Shutdown() }()
				return extension.BoundaryResult{}, nil
			}
			if !decision.Continue {
				return extension.BoundaryResult{}, nil
			}
			entries := append([]extension.SessionBoundaryDraft{}, event.Entries...)
			entries = append(entries, extension.SessionBoundaryDraft{Type: extension.SessionBoundaryDraftCustomMessage, CustomType: "probe-policy", Content: "POLICY_REFUSED: " + decision.Feedback + " Continue the work.", Display: true, Details: map[string]any{"decision": decision.Kind, "revision": decision.Revision}})
			cont := true
			return extension.BoundaryResult{Entries: &entries, Continue: &cont}, nil
		}
		boundaryMu.Lock()
		approvedBoundary = boundary
		boundaryMu.Unlock()
		record("events", map[string]any{"event": "root_settle_candidate", "revision": decision.Revision, "boundary": boundary})
		return extension.BoundaryResult{}, nil
	})
	api.OnAgentSettled(func(ctx context.Context, event extension.AgentSettledEvent) error {
		boundaryMu.Lock()
		boundary := approvedBoundary
		approvedBoundary = 0
		boundaryMu.Unlock()
		decision := decision{Kind: "observed"}
		if boundary != 0 {
			decision = rootDecision("settled", map[string]any{"boundary": boundary, "aborted": event.Aborted})
		}
		record("events", map[string]any{"event": "native_settled", "aborted": event.Aborted, "boundary": boundary, "decision": decision, "identity": identity(ctx)})
		if decision.Kind == "allow" && boundary != 0 {
			record("events", map[string]any{"event": "root_settle_approved", "boundary": boundary})
		}
		if decision.Kind == "deny" && decision.Continue && decision.Feedback != "" {
			api.SendUserMessage("POLICY_REFUSED: "+decision.Feedback, nil)
			record("events", map[string]any{"event": "stale_continued", "boundary": boundary})
		}
		return nil
	})
	return nil
}

type request struct {
	Model    string `json:"model"`
	Messages []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"messages"`
}

func response(w http.ResponseWriter, model, text, tool string, args any) {
	delta := map[string]any{"role": "assistant", "content": text}
	finish := "stop"
	if tool != "" {
		raw, _ := json.Marshal(args)
		delta = map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"index": 0, "id": "probe_call", "type": "function", "function": map[string]any{"name": tool, "arguments": string(raw)}}}}
		finish = "tool_calls"
	}
	base := map[string]any{"id": "probe", "object": "chat.completion.chunk", "created": 1, "model": model}
	base["choices"] = []any{map[string]any{"index": 0, "delta": delta, "finish_reason": nil}}
	first, _ := json.Marshal(base)
	base["choices"] = []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": finish}}
	base["usage"] = map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15}
	last, _ := json.Marshal(base)
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: %s\n\ndata: %s\n\ndata: [DONE]\n\n", first, last)
}

func fixture() {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		var request request
		if err != nil || json.Unmarshal(raw, &request) != nil || len(request.Messages) == 0 {
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		record("provider", map[string]any{"event": "request", "pid": os.Getpid(), "body": json.RawMessage(raw)})
		last := request.Messages[len(request.Messages)-1]
		if strings.Contains(string(last.Content), "POLICY_REFUSED") {
			response(w, request.Model, "PROBE_CONTINUED", "", nil)
			return
		}
		if last.Role == "tool" {
			response(w, request.Model, "PROBE_TOOL_DONE", "", nil)
			return
		}
		var text string
		if json.Unmarshal(last.Content, &text) != nil {
			text = string(last.Content)
		}
		if strings.Contains(text, "CHILD_WAIT_GATE") {
			for rootDecision("child-gate", nil).Kind != "allow" {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			response(w, request.Model, "CHILD_COMPLETED", "", nil)
			return
		}
		if strings.Contains(text, "QUEUE_GATE") {
			record("provider", map[string]any{"event": "queue_gate_wait"})
			for rootDecision("queue-provider", nil).Kind != "allow" {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(10 * time.Millisecond):
				}
			}
			response(w, request.Model, "QUEUE_GATE_DONE", "", nil)
			return
		}
		if strings.Contains(text, "case spawn-child") {
			response(w, request.Model, "", "litter_spawn", map[string]any{"type": "scout", "task": "CHILD_WAIT_GATE"})
			return
		}
		if strings.Contains(text, "case spawn-notice") {
			response(w, request.Model, "", "litter_spawn", map[string]any{"type": "scout", "task": "NOTICE_CHILD"})
			return
		}
		if strings.Contains(text, "case wait-child") {
			child := rootDecision("child-id", nil).Result
			response(w, request.Model, "", "litter_wait", map[string]any{"id": child, "timeoutMs": 10000})
			return
		}
		name := ""
		for _, candidate := range []string{"direct-write-allow", "direct-write-deny", "codemode-write-allow", "codemode-write-deny", "direct-write-lost"} {
			if strings.Contains(text, candidate) {
				name = candidate
				break
			}
		}
		if name == "" {
			response(w, request.Model, "PROBE_DONE", "", nil)
			return
		}
		args := map[string]any{"path": filepath.Join(os.Getenv("PROBE_ROOT"), "files", name+".txt"), "content": "PROBE_WRITTEN"}
		tool := "write"
		if strings.HasPrefix(name, "codemode") {
			rawArgs, _ := json.Marshal(args)
			tool = "codemode"
			args = map[string]any{"code": fmt.Sprintf("try { const value = await tools.write(%s); return {ok:true,value}; } catch (error) { return {ok:false,error:String(error.message)}; }", rawArgs)}
		}
		response(w, request.Model, "", tool, args)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	record("provider", map[string]any{"event": "listening", "pid": os.Getpid(), "address": listener.Addr().String()})
	if err := http.Serve(listener, mux); err != nil {
		panic(err)
	}
}

func main() {
	if len(os.Args) == 3 && os.Args[1] == "--pig-litter-member" {
		data, err := os.ReadFile(os.Args[2])
		if err != nil {
			panic(err)
		}
		var manifest struct {
			Cwd  string
			Env  map[string]string
			Args []string
		}
		if json.Unmarshal(data, &manifest) != nil {
			panic("invalid manifest")
		}
		cwd, err := os.Getwd()
		if err != nil || cwd != manifest.Cwd {
			panic("unexpected cwd")
		}
		os.Clearenv()
		for key, value := range manifest.Env {
			_ = os.Setenv(key, value)
		}
		os.Args = append([]string{os.Args[0]}, manifest.Args...)
		for {
			decision := rootDecision("launch", nil)
			if decision.Kind == "allow" {
				break
			}
			if decision.Kind == "unavailable" {
				os.Exit(1)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	if len(os.Args) > 1 && os.Args[1] == "--fixture" {
		fixture()
		return
	}
	cli.Main(os.Args[1:], &cli.MainOptions{HostPolicy: &cli.HostPolicy{FixedModel: coding.FixedModel{Provider: "fixture", ID: "fixed"}, RequiredFactory: factory}})
}
