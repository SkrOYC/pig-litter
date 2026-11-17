package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/coding/extension/builtin"
	"github.com/MichaelKinsy/PiG/coding/extension/factoryload"
	"github.com/MichaelKinsy/PiG/coding/mcpext"
)

const collaborationHookRefusal = "P02_HOOK_REFUSED"
const collaborationHookMarker = "P02_HOOK_FIXTURE"

type collaborationHookEvent struct {
	OffsetNanos int64  `json:"offset_nanos"`
	Tool        string `json:"tool"`
	CallID      string `json:"call_id"`
	ParentID    string `json:"parent_id,omitempty"`
	Path        string `json:"path,omitempty"`
	Refused     bool   `json:"refused"`
}

type collaborationHookPhase struct {
	Name          string                   `json:"name"`
	StartNanos    int64                    `json:"start_nanos"`
	DurationNanos int64                    `json:"duration_nanos"`
	Tool          string                   `json:"tool"`
	Nested        bool                     `json:"nested"`
	Deny          bool                     `json:"deny"`
	Target        string                   `json:"target"`
	CallID        string                   `json:"call_id,omitempty"`
	OuterCallID   string                   `json:"outer_call_id,omitempty"`
	Events        []collaborationHookEvent `json:"events"`
	ToolResult    json.RawMessage          `json:"tool_result,omitempty"`
	CallerText    string                   `json:"caller_text,omitempty"`
	FileExists    bool                     `json:"file_exists"`
	FileContent   string                   `json:"file_content,omitempty"`
	FileError     string                   `json:"file_error,omitempty"`
	SendError     string                   `json:"send_error,omitempty"`
	Verdict       string                   `json:"verdict"`
	Reason        string                   `json:"reason,omitempty"`
}

type collaborationHookEvidence struct {
	Schema        int                      `json:"schema"`
	Purpose       string                   `json:"purpose"`
	SDK           string                   `json:"sdk"`
	Status        string                   `json:"status"`
	Error         string                   `json:"error,omitempty"`
	MCPStartup    string                   `json:"mcp_startup,omitempty"`
	SessionPath   string                   `json:"session_path,omitempty"`
	ActiveTools   []string                 `json:"active_tools,omitempty"`
	ProviderCalls []json.RawMessage        `json:"provider_calls,omitempty"`
	MCPCalls      []json.RawMessage        `json:"mcp_calls,omitempty"`
	Events        []collaborationHookEvent `json:"events,omitempty"`
	Phases        []collaborationHookPhase `json:"phases"`
	ElapsedNanos  int64                    `json:"elapsed_nanos"`
}

type collaborationHookRecorder struct {
	started  time.Time
	mu       sync.Mutex
	events   []collaborationHookEvent
	provider []json.RawMessage
	mcp      []json.RawMessage
}

func (r *collaborationHookRecorder) recordEvent(event collaborationHookEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	event.OffsetNanos = time.Since(r.started).Nanoseconds()
	r.events = append(r.events, event)
}

func (r *collaborationHookRecorder) recordProvider(raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.provider = append(r.provider, append(json.RawMessage(nil), raw...))
}

func (r *collaborationHookRecorder) recordMCP(raw []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.mcp = append(r.mcp, append(json.RawMessage(nil), raw...))
}

func (r *collaborationHookRecorder) snapshot() ([]collaborationHookEvent, []json.RawMessage, []json.RawMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.events), slices.Clone(r.provider), slices.Clone(r.mcp)
}

func collaborationHookPhases(root string) []collaborationHookPhase {
	var phases []collaborationHookPhase
	for _, route := range []struct {
		name, tool string
		nested     bool
	}{
		{"direct-write", "write", false},
		{"codemode-write", "write", true},
		{"direct-mcp", "mcp__probe__write", false},
		{"codemode-mcp", "mcp__probe__write", true},
	} {
		for _, denied := range []bool{false, true} {
			suffix := "allow"
			if denied {
				suffix = "deny"
			}
			name := route.name + "-" + suffix
			phases = append(phases, collaborationHookPhase{Name: name, Tool: route.tool, Nested: route.nested, Deny: denied, Target: filepath.Join(root, "files", name+".txt"), Verdict: "inconclusive", Reason: "not attempted"})
		}
	}
	return phases
}

func collaborationHookCall(phase collaborationHookPhase) call {
	args := map[string]any{"path": phase.Target, "content": collaborationHookMarker}
	if !phase.Nested {
		return call{phase.Tool, args}
	}
	encoded, _ := json.Marshal(args)
	code := fmt.Sprintf("try { const value = await tools.%s(%s); return {ok: true, value}; } catch (error) { return {ok: false, error: String(error.message)}; }", phase.Tool, encoded)
	return call{"codemode", map[string]any{"code": code}}
}

func collaborationHookModelServer(recorder *collaborationHookRecorder, phases []collaborationHookPhase) *httptest.Server {
	var mu sync.Mutex
	requestCount := 0
	fixture := &fixture{}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		raw, err := io.ReadAll(io.LimitReader(req.Body, 16<<20))
		if err != nil || !json.Valid(raw) {
			http.Error(w, "invalid fixture provider request", http.StatusBadRequest)
			return
		}
		recorder.recordProvider(raw)
		mu.Lock()
		n := requestCount
		requestCount++
		mu.Unlock()
		if n/2 >= len(phases) {
			http.Error(w, "unexpected fixture provider request", http.StatusBadRequest)
			return
		}
		if n%2 == 0 {
			fixture.answer(w, "hook-fixture", "", []call{collaborationHookCall(phases[n/2])})
			return
		}
		fixture.answer(w, "hook-fixture", "P02_FIXTURE_DONE", nil)
	}))
}

func collaborationHookMCPServer(recorder *collaborationHookRecorder, phases []collaborationHookPhase) *httptest.Server {
	allowedTargets := make(map[string]bool)
	for _, phase := range phases {
		if strings.HasPrefix(phase.Tool, "mcp__") {
			allowedTargets[phase.Target] = true
		}
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if req.Method != http.MethodPost {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		raw, err := io.ReadAll(io.LimitReader(req.Body, 1<<20))
		if err != nil || !json.Valid(raw) {
			http.Error(w, "invalid MCP request", http.StatusBadRequest)
			return
		}
		recorder.recordMCP(raw)
		var rpc struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				ProtocolVersion string `json:"protocolVersion"`
				Name            string `json:"name"`
				Arguments       struct {
					Path    string `json:"path"`
					Content string `json:"content"`
				} `json:"arguments"`
			} `json:"params"`
		}
		if err := json.Unmarshal(raw, &rpc); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(rpc.ID) == 0 {
			w.WriteHeader(http.StatusAccepted)
			return
		}
		var result any
		switch rpc.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "p02-hook-fixture")
			result = map[string]any{"protocolVersion": rpc.Params.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "p02-hook-fixture", "version": "1"}}
		case "tools/list":
			result = map[string]any{"tools": []any{map[string]any{"name": "write", "description": "Write the controlled fixture file", "inputSchema": map[string]any{"type": "object", "properties": map[string]any{"path": map[string]any{"type": "string"}, "content": map[string]any{"type": "string"}}, "required": []string{"path", "content"}}}}}
		case "tools/call":
			if rpc.Params.Name != "write" || !allowedTargets[rpc.Params.Arguments.Path] || rpc.Params.Arguments.Content != collaborationHookMarker {
				http.Error(w, "unexpected fixture write", http.StatusBadRequest)
				return
			}
			if err := os.WriteFile(rpc.Params.Arguments.Path, []byte(rpc.Params.Arguments.Content), 0o600); err != nil {
				http.Error(w, err.Error(), http.StatusInternalServerError)
				return
			}
			result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "MCP_FIXTURE_WROTE"}}}
		default:
			http.Error(w, "unexpected MCP method", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": rpc.ID, "result": result})
	}))
}

func collaborationHookExtensions(root, mcpURL string, recorder *collaborationHookRecorder) ([]extension.Extension, *extension.ExtensionRuntime, error) {
	registrations := extension.CreateExtensionRuntime()
	config, err := json.Marshal(map[string]any{"url": mcpURL, "exposure": "direct"})
	if err != nil {
		return nil, nil, err
	}
	if err := registrations.RegisterMcpServer("p02-hook-fixture", "probe", config); err != nil {
		return nil, nil, err
	}
	guard := extension.Extension{Path: "p02-hook-guard", Handlers: map[string][]extension.HandlerFn{"tool_call": {func(args ...any) (any, error) {
		if len(args) == 0 {
			return nil, errors.New("missing tool call event")
		}
		event, ok := args[0].(extension.CustomToolCallEvent)
		if !ok {
			return nil, fmt.Errorf("unexpected tool call event %T", args[0])
		}
		path, _ := event.Input["path"].(string)
		refuse := (event.ToolName == "write" || event.ToolName == "mcp__probe__write") && strings.HasSuffix(path, "-deny.txt")
		recorder.recordEvent(collaborationHookEvent{Tool: event.ToolName, CallID: event.ToolCallID, ParentID: event.ParentToolCallID, Path: path, Refused: refuse})
		if refuse {
			return &extension.ToolCallEventResult{Block: true, Reason: collaborationHookRefusal}, nil
		}
		return nil, nil
	}}}}
	options := builtin.Options{Mcp: mcpext.Options{AgentDir: filepath.Join(root, "agent"), LogPath: filepath.Join(root, "mcp.log"), LoadConfig: func(context.Context) mcpext.LoadedMcpConfig { return mcpext.LoadedMcpConfig{} }}}
	options.ConfigureCodemode(filepath.Join(root, "codemode-cache"))
	loaded := []extension.Extension{guard}
	for _, name := range []string{"codemode", "mcp"} {
		entry, err := builtin.Resolve("builtin:"+name, options)
		if err != nil {
			return nil, nil, err
		}
		ext, err := factoryload.LoadExtensionFromFactory(entry.Factory, root, extension.CreateEventBus(), registrations, entry.Path())
		if err != nil {
			return nil, nil, err
		}
		ext.Name = entry.Name
		loaded = append(loaded, ext)
	}
	return loaded, registrations, nil
}

func collaborationHookWriteEvidence(root string, evidence collaborationHookEvidence) error {
	data, err := json.MarshalIndent(evidence, "", "  ")
	if err != nil {
		return err
	}
	file, err := os.OpenFile(filepath.Join(root, "result.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, writeErr := file.Write(append(data, '\n'))
	return errors.Join(writeErr, file.Close())
}

func collaborationHookSDKVersion() string {
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dep := range info.Deps {
			if dep.Path == "github.com/MichaelKinsy/PiG" {
				return dep.Version
			}
		}
	}
	return "unknown"
}

func collaborationRunHostHookProbe(ctx context.Context, root string) (evidence collaborationHookEvidence, resultErr error) {
	started := time.Now()
	evidence = collaborationHookEvidence{Schema: 1, Purpose: "host research only; product evidence remains unverified", SDK: collaborationHookSDKVersion(), Status: "inconclusive"}
	if !filepath.IsAbs(root) {
		return evidence, errors.New("probe root must be absolute")
	}
	if _, err := os.Lstat(filepath.Join(root, "result.json")); err == nil {
		return evidence, errors.New("probe result already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return evidence, fmt.Errorf("inspect probe result: %w", err)
	}
	recorder := &collaborationHookRecorder{started: started}
	defer func() {
		evidence.ElapsedNanos = time.Since(started).Nanoseconds()
		evidence.Events, evidence.ProviderCalls, evidence.MCPCalls = recorder.snapshot()
		if resultErr != nil {
			if evidence.Status != "provisional_bypass" {
				evidence.Status = "inconclusive"
			}
			evidence.Error = resultErr.Error()
		}
		if err := collaborationHookWriteEvidence(root, evidence); err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("write evidence: %w", err))
		}
	}()
	for _, path := range []string{"files", "agent", "sessions", "codemode-cache"} {
		if err := os.MkdirAll(filepath.Join(root, path), 0o700); err != nil {
			return evidence, err
		}
	}
	evidence.Phases = collaborationHookPhases(root)
	for _, phase := range evidence.Phases {
		if _, err := os.Lstat(phase.Target); err == nil || !errors.Is(err, os.ErrNotExist) {
			return evidence, fmt.Errorf("fixture target is not absent: %s (%v)", phase.Target, err)
		}
	}
	model := collaborationHookModelServer(recorder, evidence.Phases)
	defer model.Close()
	mcp := collaborationHookMCPServer(recorder, evidence.Phases)
	defer mcp.Close()
	modelConfig := fmt.Sprintf(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":%q,"apiKey":"fixture-only","models":[{"id":"hook-fixture","name":"hook-fixture","contextWindow":32000,"maxTokens":1024}]}}}`, model.URL+"/v1")
	if err := os.WriteFile(filepath.Join(root, "agent", "models.json"), []byte(modelConfig), 0o600); err != nil {
		return evidence, err
	}
	trusted := true
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: root, AgentDir: filepath.Join(root, "agent"), ProjectTrusted: &trusted})
	if err != nil {
		return evidence, err
	}
	defer services.Close()
	extensions, registrations, err := collaborationHookExtensions(root, mcp.URL, recorder)
	if err != nil {
		return evidence, err
	}
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, NewExtensions: extensions, ExtensionRuntime: registrations, AbortContext: ctx})
	if err != nil {
		return evidence, err
	}
	defer func() { resultErr = errors.Join(resultErr, runtime.Close()) }()
	allowed := map[string]struct{}{"write": {}, "codemode": {}, "mcp__probe__write": {}}
	session, err := runtime.New(coding.SessionStartOptions{Model: services.ModelRuntime().GetModel("fixture", "hook-fixture"), ResourceLoader: coding.NoResources, AllowedTools: allowed, InitialActiveToolNames: []string{"write", "codemode", "mcp__probe__write"}, SessionDir: filepath.Join(root, "sessions")})
	if err != nil {
		return evidence, err
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	defer func() {
		resultErr = errors.Join(resultErr, session.Close())
		<-drained
	}()
	if err := session.BindExtensions(ctx, coding.ExtensionBindings{}); err != nil {
		return evidence, err
	}
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
waitForMCP:
	for !slices.Contains(session.ActiveToolNames(), "mcp__probe__write") {
		select {
		case <-ctx.Done():
			return evidence, ctx.Err()
		case <-timer.C:
			evidence.MCPStartup = "MCP tool did not become active before the startup deadline"
			break waitForMCP
		case <-ticker.C:
		}
	}
	evidence.ActiveTools = session.ActiveToolNames()
	evidence.SessionPath = session.Path()
	for index := range evidence.Phases {
		phase := &evidence.Phases[index]
		phaseStart := time.Now()
		phase.StartNanos = phaseStart.Sub(started).Nanoseconds()
		before, _, _ := recorder.snapshot()
		messages, sendErr := session.Send(ctx, "Execute the controlled fixture call for "+phase.Name)
		if sendErr != nil {
			phase.SendError = sendErr.Error()
		}
		for _, message := range messages {
			if message.ToolResult != nil {
				phase.OuterCallID = message.ToolResult.ToolCallID
				phase.CallerText = message.ToolResult.Text()
				phase.ToolResult, _ = json.Marshal(message.ToolResult)
			}
		}
		after, _, _ := recorder.snapshot()
		if len(after) >= len(before) {
			phase.Events = after[len(before):]
		}
		data, readErr := os.ReadFile(phase.Target)
		if readErr == nil {
			phase.FileExists = true
			phase.FileContent = string(data)
		} else if !errors.Is(readErr, os.ErrNotExist) {
			phase.FileError = readErr.Error()
		}
		phase.DurationNanos = time.Since(phaseStart).Nanoseconds()
		collaborationAssessHookPhase(phase)
		if ctx.Err() != nil {
			break
		}
	}
	for index := 1; index < len(evidence.Phases); index += 2 {
		control, denied := &evidence.Phases[index-1], &evidence.Phases[index]
		if control.Verdict != "observed" && denied.Verdict == "observed" {
			denied.Verdict = "inconclusive"
			denied.Reason = "the matching allowed control did not prove the route was reachable"
		}
	}
	var issues []string
	for _, phase := range evidence.Phases {
		if phase.Verdict != "observed" {
			issues = append(issues, phase.Name+": "+phase.Verdict+" ("+phase.Reason+")")
		}
		if phase.Verdict == "provisional_bypass" {
			evidence.Status = "provisional_bypass"
		}
	}
	if len(issues) > 0 {
		return evidence, errors.New(strings.Join(issues, "; "))
	}
	evidence.Status = "observed"
	return evidence, nil
}

func collaborationAssessHookPhase(phase *collaborationHookPhase) {
	phase.Verdict = "inconclusive"
	phase.Reason = ""
	if phase.Deny && phase.FileExists {
		phase.Verdict = "provisional_bypass"
		phase.Reason = "the denied fixture file was created; inspect the hook trace and repeat on the final P01 host"
		return
	}
	if phase.SendError != "" || phase.FileError != "" || len(phase.ToolResult) == 0 {
		phase.Reason = "missing caller result or filesystem observation"
		return
	}
	var selected *collaborationHookEvent
	matches := 0
	for index := range phase.Events {
		event := &phase.Events[index]
		if event.Tool == phase.Tool && event.Path == phase.Target && (event.ParentID != "") == phase.Nested {
			selected = event
			matches++
		}
	}
	if matches != 1 {
		phase.Reason = "the intended tool call was not observed exactly once by the hook"
		return
	}
	phase.CallID = selected.CallID
	if selected.CallID == "" || phase.OuterCallID == "" || phase.Nested && selected.ParentID != phase.OuterCallID || !phase.Nested && selected.CallID != phase.OuterCallID {
		phase.Reason = "the hook event and caller result did not identify the same call"
		return
	}
	var result struct {
		ToolName    string `json:"toolName"`
		ToolCallID  string `json:"toolCallId"`
		IsError     bool   `json:"isError"`
		NestedCalls *struct {
			Complete bool `json:"complete"`
			Calls    []struct {
				ID     string `json:"id"`
				Name   string `json:"name"`
				Status string `json:"status"`
				Error  string `json:"error"`
			} `json:"calls"`
		} `json:"nestedCalls"`
	}
	if err := json.Unmarshal(phase.ToolResult, &result); err != nil || result.ToolCallID != phase.OuterCallID {
		phase.Reason = "the caller result could not be attributed to this call"
		return
	}
	if phase.Nested {
		status := "ok"
		if phase.Deny {
			status = "error"
		}
		if result.ToolName != "codemode" || result.IsError || result.NestedCalls == nil || !result.NestedCalls.Complete || len(result.NestedCalls.Calls) != 1 || result.NestedCalls.Calls[0].ID != selected.CallID || result.NestedCalls.Calls[0].Name != phase.Tool || result.NestedCalls.Calls[0].Status != status {
			phase.Reason = "the caller result did not record the expected nested call and status"
			return
		}
		if phase.Deny && !strings.Contains(result.NestedCalls.Calls[0].Error, collaborationHookRefusal) {
			phase.Reason = "the nested call record did not contain the refusal"
			return
		}
	} else if result.ToolName != phase.Tool || result.IsError != phase.Deny {
		phase.Reason = "the direct caller result did not identify the expected tool and outcome"
		return
	}
	if phase.Deny {
		if !selected.Refused || phase.FileExists || !strings.Contains(phase.CallerText, collaborationHookRefusal) {
			phase.Reason = "refusal, caller result, and absence of the side effect were not all observed"
			return
		}
	} else if selected.Refused || !phase.FileExists || phase.FileContent != collaborationHookMarker {
		phase.Reason = "the allowed control did not create the expected fixture file"
		return
	}
	phase.Verdict = "observed"
	phase.Reason = ""
}
