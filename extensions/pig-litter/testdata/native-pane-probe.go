package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/cli"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/SkrOYC/pig-litter/extensions/pig-litter/memberinput"
)

var logMu sync.Mutex
var boundaryMu sync.Mutex
var boundaryCounter uint64
var approvedBoundary uint64
var domainMu sync.Mutex
var domain *nativeDomain
var domainTurn uint64
var inputAdmission *coding.InputAdmission
var inputBridge *memberinput.Bridge
var inputPhase atomic.Uint64
var startupDomain *nativeDomain
var startupWindow memberinput.WindowID
var startupTransport *http.Transport

type nativeDomain struct {
	client *http.Client
	token  string
}

func (domain *nativeDomain) call(ctx context.Context, path string, request, result any) error {
	data, err := json.Marshal(request)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://member"+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("X-Pig-Litter-Member", domain.token)
	response, err := domain.client.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))
		return fmt.Errorf("native domain route %s: %s: %s", path, response.Status, strings.TrimSpace(string(body)))
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(result); err != nil {
		return err
	}
	record("events", map[string]any{"event": "domain_call", "route": path})
	return nil
}

func currentDomain() *nativeDomain {
	domainMu.Lock()
	defer domainMu.Unlock()
	return domain
}

func currentDomainTurn() uint64 {
	domainMu.Lock()
	defer domainMu.Unlock()
	return domainTurn
}

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

type rootDecisionResult struct {
	Kind     string `json:"kind"`
	Revision string `json:"revision"`
	Boundary uint64 `json:"boundary"`
	Result   string `json:"result"`
	Feedback string `json:"feedback"`
	Continue bool   `json:"continue"`
}

func rootDecision(kind string, detail any) rootDecisionResult {
	conn, err := net.DialTimeout("unix", filepath.Join(os.Getenv("PROBE_ROOT"), "root.sock"), 2*time.Second)
	if err != nil {
		return rootDecisionResult{Kind: "unavailable"}
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if json.NewEncoder(conn).Encode(map[string]any{"kind": kind, "detail": detail, "token": os.Getenv("PROBE_TOKEN")}) != nil {
		return rootDecisionResult{Kind: "unavailable"}
	}
	var result rootDecisionResult
	if json.NewDecoder(conn).Decode(&result) != nil {
		return rootDecisionResult{Kind: "unavailable"}
	}
	return result
}

func watchRoot(ctx context.Context, api extension.API) {
	controlCtx, stopControl := context.WithCancel(context.Background())
	defer stopControl()
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
			Socket  string          `json:"socket"`
			Token   string          `json:"token"`
		}
		if decoder.Decode(&control) != nil {
			break
		}
		switch control.Kind {
		case "transport":
			transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", control.Socket)
			}}
			client := &nativeDomain{client: &http.Client{Transport: transport}, token: control.Token}
			domainMu.Lock()
			domain = client
			domainMu.Unlock()
			go serveControl(controlCtx, ctx, api, client, transport)
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

type nativeControlCommand struct {
	ID         string `json:"id"`
	Kind       string `json:"kind"`
	Completion *struct {
		Details json.RawMessage `json:"details"`
		Content string          `json:"content"`
		Mode    string          `json:"mode"`
		Trigger bool            `json:"trigger"`
	} `json:"completion"`
	History *struct {
		Entries int `json:"entries"`
		Bytes   int `json:"bytes"`
	} `json:"history"`
	Audit json.RawMessage `json:"audit"`
}

func serveControl(ctx, nativeCtx context.Context, api extension.API, domain *nativeDomain, transport *http.Transport) {
	defer transport.CloseIdleConnections()
	request := func(path string, body io.Reader) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://member"+path, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("X-Pig-Litter-Member", domain.token)
		return domain.client.Do(req)
	}
	go func() {
		response, err := request("/control/watch", nil)
		if response != nil {
			response.Body.Close()
		}
		if ctx.Err() == nil && (err != nil || response.StatusCode != http.StatusOK) {
			_ = extension.FromContext(nativeCtx).Abort()
			_ = extension.FromContext(nativeCtx).Shutdown()
			return
		}
		if err == nil && response.StatusCode == http.StatusOK {
			_ = extension.FromContext(nativeCtx).Abort()
			_ = extension.FromContext(nativeCtx).Shutdown()
		}
	}()
	for {
		response, err := request("/control/next", nil)
		if err != nil || response.StatusCode != http.StatusOK {
			if response != nil {
				response.Body.Close()
			}
			if ctx.Err() == nil {
				_ = extension.FromContext(nativeCtx).Abort()
				_ = extension.FromContext(nativeCtx).Shutdown()
			}
			return
		}
		var command nativeControlCommand
		err = json.NewDecoder(io.LimitReader(response.Body, 8<<20)).Decode(&command)
		response.Body.Close()
		if err != nil {
			return
		}
		if command.Kind == "cancel" {
			_ = extension.FromContext(nativeCtx).Abort()
			_ = extension.FromContext(nativeCtx).Shutdown()
			return
		}
		reply := executeNativeControl(nativeCtx, api, command)
		record("events", map[string]any{"event": "transport_control", "id": command.ID, "kind": command.Kind, "disposition": reply["disposition"], "error": reply["error"]})
		data, err := json.Marshal(reply)
		if err != nil {
			return
		}
		response, err = request("/control/result", strings.NewReader(string(data)))
		if err != nil {
			return
		}
		response.Body.Close()
		if response.StatusCode != http.StatusNoContent {
			return
		}
	}
}

func executeNativeControl(ctx context.Context, api extension.API, command nativeControlCommand) (reply map[string]any) {
	reply = map[string]any{"id": command.ID, "kind": command.Kind}
	defer func() {
		if recover() != nil {
			reply = map[string]any{"id": command.ID, "kind": command.Kind, "error": "native control failed"}
		}
	}()
	switch command.Kind {
	case "snapshot":
		ext := extension.FromContext(ctx)
		manager, err := ext.SessionManager()
		if err != nil {
			reply["error"] = "native session is unavailable"
			return reply
		}
		cwd, _ := ext.CWD()
		model, _ := ext.Model()
		idle, _ := ext.IsIdle()
		pending, _ := ext.HasPendingMessages()
		phase, count := 2, 0
		if idle {
			phase = 4
		}
		if pending {
			count = 1
		}
		var entries []json.RawMessage
		clipped, used := false, 0
		if command.History != nil && command.History.Entries > 0 {
			for _, entry := range manager.GetBranch() {
				data, err := json.Marshal(entry)
				if err != nil || len(entries) == command.History.Entries || used+len(data) > command.History.Bytes {
					clipped = true
					break
				}
				entries = append(entries, data)
				used += len(data)
			}
		}
		modelName := ""
		if model != nil {
			modelName = model.ProviderMeta.ProviderID + "/" + model.ID
		}
		reply["snapshot"] = map[string]any{"session": manager.GetSessionId(), "cwd": cwd, "model": modelName, "tools": ext.GetActiveTools(), "phase": phase, "pending": count, "entries": entries, "leaf": manager.GetLeafID(), "clipped": clipped}
	case "completion":
		if command.Completion == nil {
			reply["error"] = "missing completion"
			break
		}
		api.SendMessage(extension.SendMessagePayload{CustomType: "litter_completion", Content: command.Completion.Content, Details: command.Completion.Details}, &extension.SendMessageOptions{TriggerTurn: &command.Completion.Trigger, DeliverAs: extension.DeliverAs(command.Completion.Mode)})
		reply["disposition"] = 3
	case "audit":
		api.AppendEntry("litter_completion", command.Audit)
	default:
		reply["error"] = "unsupported native control"
	}
	return reply
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
		if os.Getenv("P08_INPUT_TEST") != "" {
			manager, err := extension.FromContext(ctx).SessionManager()
			if err != nil {
				_ = inputAdmission.Close()
				return err
			}
			baseline, err := json.Marshal(manager.GetEntries())
			if err != nil || os.WriteFile(filepath.Join(os.Getenv("PROBE_ROOT"), "baseline.json"), baseline, 0o600) != nil {
				_ = inputAdmission.Close()
				return errors.New("fixture could not retain the starting session baseline")
			}
			for {
				if _, err := os.Stat(filepath.Join(os.Getenv("PROBE_ROOT"), "release-ready")); err == nil {
					break
				}
				select {
				case <-ctx.Done():
					_ = inputAdmission.Close()
					return ctx.Err()
				case <-time.After(10 * time.Millisecond):
				}
			}
		}
		if os.Getenv("P08_INPUT_TEST") == "fail" {
			_ = inputAdmission.Close()
			record("events", map[string]any{"event": "readiness_refused", "closed": true})
			go func() { _ = extension.FromContext(ctx).Shutdown() }()
			return errors.New("fixture readiness refused")
		}
		if startupDomain != nil {
			ext := extension.FromContext(ctx)
			manager, err := ext.SessionManager()
			cwd, cwdErr := ext.CWD()
			model, modelErr := ext.Model()
			if err != nil || cwdErr != nil || modelErr != nil || model == nil || manager.GetSessionFile() == nil {
				_ = inputAdmission.Close()
				return errors.New("native readiness context is incomplete")
			}
			request := map[string]any{"window": startupWindow, "session": manager.GetSessionId(), "cwd": cwd, "model": model.ProviderID() + "/" + model.ID, "tools": ext.GetActiveTools(), "sessionFile": *manager.GetSessionFile(), "leaf": manager.GetLeafID(), "entries": manager.GetEntries()}
			if os.Getenv("P08_INPUT_TEST") == "bad-ready" {
				request["model"] = "fixture/wrong"
			}
			for range 2 {
				var reply map[string]any
				if err := startupDomain.call(ctx, "/input/ready", request, &reply); err != nil {
					_ = inputAdmission.Close()
					record("events", map[string]any{"event": "readiness_refused", "closed": true, "error": err.Error()})
					go func() { _ = ext.Shutdown() }()
					return err
				}
			}
		}
		if startupDomain != nil && os.Getenv("P08_COMMIT_ROOT") == "1" {
			domainMu.Lock()
			domain = startupDomain
			domainMu.Unlock()
			go serveControl(context.Background(), ctx, api, startupDomain, startupTransport)
		}
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
	api.OnTurnStart(func(ctx context.Context, event extension.TurnStartEvent) error {
		if client := currentDomain(); client != nil {
			domainMu.Lock()
			domainTurn++
			turn := domainTurn
			domainMu.Unlock()
			var reply map[string]any
			if err := client.call(ctx, "/member/turn/start", map[string]any{"id": turn}, &reply); err != nil {
				record("events", map[string]any{"event": "domain_error", "route": "turn-start", "error": err.Error()})
				go func() { _ = extension.FromContext(ctx).Abort(); _ = extension.FromContext(ctx).Shutdown() }()
			}
		}
		return nil
	})
	api.OnTurnEnd(func(ctx context.Context, event extension.TurnEndEvent) (extension.TurnEndEventResult, error) {
		if client := currentDomain(); client != nil {
			domainMu.Lock()
			turn := domainTurn
			domainMu.Unlock()
			var reply map[string]any
			if err := client.call(ctx, "/member/turn/end", map[string]any{"id": turn}, &reply); err != nil {
				record("events", map[string]any{"event": "domain_error", "route": "turn-end", "error": err.Error()})
				go func() { _ = extension.FromContext(ctx).Abort(); _ = extension.FromContext(ctx).Shutdown() }()
			}
		}
		return extension.BoundaryResult{}, nil
	})
	api.OnToolCall(func(ctx context.Context, event extension.ToolCallEvent) (extension.ToolCallEventResult, error) {
		call := event.(extension.CustomToolCallEvent)
		path, _ := call.Input["path"].(string)
		if client := currentDomain(); client != nil && !strings.HasSuffix(path, "direct-write-lost.txt") {
			turn := currentDomainTurn()
			manager, err := extension.FromContext(ctx).SessionManager()
			if err != nil {
				return extension.ToolCallEventResult{Block: true, Reason: "native session unavailable"}, nil
			}
			if leaf := manager.GetLeafID(); leaf != nil {
				var observed map[string]any
				if err := client.call(ctx, "/member/response", map[string]any{"turn": turn, "entryId": *leaf}, &observed); err != nil {
					record("events", map[string]any{"event": "domain_error", "route": "response", "error": err.Error()})
					return extension.ToolCallEventResult{Block: true, Reason: "native response was not observed"}, nil
				}
			}
			var decision extension.ToolCallEventResult
			err = client.call(ctx, "/member/tool", map[string]any{"turn": turn, "id": call.ToolCallID, "parent": call.ParentToolCallID, "name": call.ToolName, "arguments": call.Input}, &decision)
			if err != nil {
				record("events", map[string]any{"event": "domain_error", "route": "tool", "error": err.Error()})
				return extension.ToolCallEventResult{Block: true, Reason: "native tool authority unavailable"}, nil
			}
			record("events", map[string]any{"event": "tool_call", "tool": call.ToolName, "id": call.ToolCallID, "parent": call.ParentToolCallID, "input": call.Input, "decision": decision, "denied": decision.Block})
			return decision, nil
		}
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
				if client := currentDomain(); client != nil {
					route := "/member/child/spawn"
					if name == "litter_wait" {
						route = "/member/child/wait"
					}
					var reply struct {
						Content string `json:"content"`
						IsError bool   `json:"isError"`
					}
					if err := client.call(ctx, route, map[string]any{"turn": currentDomainTurn(), "callId": callID, "leaf": leaf, "args": params}, &reply); err != nil {
						record("events", map[string]any{"event": "domain_error", "route": route, "error": err.Error()})
						return extension.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: "native operation unavailable"}}, IsError: true}, nil
					}
					return extension.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: reply.Content}}, IsError: reply.IsError}, nil
				}
				decision := rootDecision(name, map[string]any{"call": callID, "leaf": leaf, "args": params})
				return extension.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: decision.Result}}, IsError: decision.Kind != "allow"}, nil
			},
		})
	}
	api.OnToolExecutionEnd(func(ctx context.Context, event extension.ToolExecutionEndEvent) error {
		if event.ToolName == "litter_spawn" || event.ToolName == "litter_wait" {
			if client := currentDomain(); client != nil {
				data, _ := json.Marshal(event.Result)
				var result struct {
					Content []struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"content"`
					IsError bool `json:"isError"`
				}
				_ = json.Unmarshal(data, &result)
				var reply map[string]any
				if err := client.call(ctx, "/member/result", map[string]any{"turn": currentDomainTurn(), "callId": event.ToolCallID, "name": event.ToolName, "isError": event.IsError || result.IsError, "content": result.Content}, &reply); err != nil {
					record("events", map[string]any{"event": "domain_error", "route": "result", "error": err.Error()})
				}
				return nil
			}
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
		var decision rootDecisionResult
		if client := currentDomain(); client != nil {
			var reply struct {
				Boundary uint64 `json:"boundary"`
				Allow    bool   `json:"allow"`
				Continue bool   `json:"continue"`
				Feedback string `json:"feedback"`
			}
			err := client.call(ctx, "/member/settlement/decide", map[string]any{"boundary": boundary, "report": report, "completed": true, "continuing": false}, &reply)
			if err != nil {
				record("events", map[string]any{"event": "domain_error", "route": "settlement", "error": err.Error()})
				go func() { _ = extension.FromContext(ctx).Abort(); _ = extension.FromContext(ctx).Shutdown() }()
				return extension.BoundaryResult{}, nil
			}
			decision = rootDecisionResult{Kind: "deny", Boundary: reply.Boundary, Continue: reply.Continue, Feedback: reply.Feedback}
			if reply.Allow {
				decision.Kind = "allow"
				rootDecision("after-decision", map[string]any{"boundary": boundary})
			}
		} else {
			decision = rootDecision("settle", map[string]any{"boundary": boundary, "outcome": event.Outcome, "continuing": event.Continue, "canContinue": event.Context.CanContinue, "report": report})
		}
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
		decision := rootDecisionResult{Kind: "observed"}
		if boundary != 0 {
			if startupDomain != nil && os.Getenv("P08_COMMIT_ROOT") == "1" {
				var reply struct {
					Allow    bool   `json:"allow"`
					Continue bool   `json:"continue"`
					Feedback string `json:"feedback"`
				}
				if err := startupDomain.call(ctx, "/member/settlement/finalize", map[string]any{"boundary": boundary, "aborted": event.Aborted}, &reply); err != nil {
					record("events", map[string]any{"event": "finalize_uncertain", "boundary": boundary, "error": err.Error()})
					return nil
				}
				decision = rootDecisionResult{Kind: "deny", Boundary: boundary, Continue: reply.Continue, Feedback: reply.Feedback}
				if reply.Allow {
					decision.Kind = "allow"
				}
			} else {
				decision = rootDecision("settled", map[string]any{"boundary": boundary, "aborted": event.Aborted})
			}
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

func publishInputWindow(ctx context.Context, window *coding.InputWindow) error {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return err
	}
	id := memberinput.WindowID(hex.EncodeToString(nonce[:]))
	if err := inputBridge.Open(ctx, id, memberinput.NativeWindow{Value: window}); err != nil {
		return err
	}
	phase := inputPhase.Add(1)
	record("events", map[string]any{"event": "input_window", "id": id, "phase": phase})
	if phase != 1 {
		return nil
	}
	startupWindow = id
	type transferInput struct {
		ID   memberinput.TransferID `json:"id"`
		Mode memberinput.Mode       `json:"mode"`
		Text string                 `json:"text"`
	}
	inputs := []transferInput{{ID: "starting-0", Mode: memberinput.FollowUp, Text: "START_ONE"}, {ID: "starting-1", Mode: memberinput.FollowUp, Text: "START_TWO"}}
	if startupDomain != nil {
		request := map[string]any{"window": id}
		for {
			if err := startupDomain.call(ctx, "/input/start", request, &inputs); err == nil {
				break
			} else {
				var transport *net.OpError
				if !errors.As(err, &transport) {
					return err
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(10 * time.Millisecond):
			}
		}
		var replay []transferInput
		if err := startupDomain.call(ctx, "/input/start", request, &replay); err != nil || len(replay) != len(inputs) {
			return errors.New("starting transfer replay was unavailable")
		}
		for index := range replay {
			if replay[index] != inputs[index] {
				return errors.New("starting transfer replay changed its content")
			}
		}
	}
	ids := make([]memberinput.TransferID, 0, len(inputs))
	for _, entry := range inputs {
		transfer := entry.ID
		input := memberinput.Input{Mode: entry.Mode, Text: entry.Text}
		receipt, err := inputBridge.Admit(ctx, id, transfer, input)
		if err != nil {
			return err
		}
		replayed, err := inputBridge.Admit(ctx, id, transfer, input)
		if err != nil || replayed != receipt {
			return errors.New("starting transfer replay changed its ticket")
		}
		if _, err := receipt.Await(ctx); !errors.Is(err, coding.ErrInputWouldBlock) {
			return fmt.Errorf("starting ticket callback wait = %v", err)
		}
		go func() {
			result, err := receipt.Await(context.Background())
			detail := ""
			if err != nil {
				detail = err.Error()
			}
			record("events", map[string]any{"event": "input_result", "transfer": transfer, "result": result, "error": detail})
			if startupDomain != nil && err == nil {
				request := map[string]any{"window": id, "transfer": transfer, "disposition": result.Disposition, "error": result.Error}
				for ctx.Err() == nil {
					var reply map[string]any
					if startupDomain.call(ctx, "/input/result", request, &reply) == nil {
						break
					}
					time.Sleep(10 * time.Millisecond)
				}
			}
			receipt.Close()
		}()
		ids = append(ids, transfer)
	}
	if startupDomain != nil {
		for range 2 {
			var reply map[string]any
			if err := startupDomain.call(ctx, "/input/installed", map[string]any{"window": id, "transfers": ids}, &reply); err != nil {
				return err
			}
		}
	}
	_, err := inputBridge.Admit(ctx, id, "starting-overflow", memberinput.Input{Mode: "follow_up", Text: "START_OVERFLOW"})
	if !errors.Is(err, coding.ErrInputFull) {
		return fmt.Errorf("starting input limit = %v", err)
	}
	record("events", map[string]any{"event": "input_staged", "id": id, "count": len(inputs), "replay": true, "callbackWouldBlock": true, "overflow": "full"})
	for {
		if _, err := os.Stat(filepath.Join(os.Getenv("PROBE_ROOT"), "release-window")); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
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
		for _, candidate := range []string{"direct-write-allow", "direct-write-deny", "direct-write-fail", "codemode-write-allow", "codemode-write-deny", "direct-write-lost"} {
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
		if name == "direct-write-fail" {
			args["path"] = filepath.Join(os.Getenv("PROBE_ROOT"), "files")
		}
		tool := "write"
		if strings.HasPrefix(name, "codemode") {
			rawArgs, _ := json.Marshal(args)
			tool = "codemode"
			prefix := ""
			gate := filepath.Join(os.Getenv("PROBE_ROOT"), "files", "nested-gate")
			if name == "codemode-write-deny" {
				if _, err := os.Stat(gate); err == nil {
					readArgs, _ := json.Marshal(map[string]any{"path": gate})
					prefix = fmt.Sprintf("await tools.read(%s); ", readArgs)
				}
			}
			args = map[string]any{"code": prefix + fmt.Sprintf("try { const value = await tools.write(%s); return {ok:true,value}; } catch (error) { return {ok:false,error:String(error.message)}; }", rawArgs)}
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
	if os.Getenv("P08_INPUT_TEST") != "" {
		inputBridge = memberinput.New()
		if socket := os.Getenv("P08_CONTROL_SOCKET"); socket != "" {
			transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", socket)
			}}
			defer transport.CloseIdleConnections()
			startupTransport = transport
			startupDomain = &nativeDomain{client: &http.Client{Transport: transport}, token: os.Getenv("P08_CONTROL_TOKEN")}
		}
		var err error
		inputAdmission, err = coding.NewInputAdmission(context.Background(), coding.InputAdmissionOptions{SessionID: "018f0000-0000-7000-8000-000000000002", PendingLimit: 2, OnWindow: publishInputWindow})
		if err != nil {
			panic(err)
		}
		defer inputBridge.Close()
		defer inputAdmission.Close()
	}
	cli.Main(os.Args[1:], &cli.MainOptions{InputAdmission: inputAdmission, HostPolicy: &cli.HostPolicy{FixedModel: coding.FixedModel{Provider: "fixture", ID: "fixed"}, RequiredFactory: factory}})
}
