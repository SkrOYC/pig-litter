//go:build p08native

package piglitter

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding/extension"
)

func nativeMemberRoute(name string) string {
	switch name {
	case "observer_read":
		return "/member/observer/read"
	case "observer_ls":
		return "/member/observer/ls"
	case "observer_report":
		return "/member/observer/report"
	case "litter_spawn":
		return "/member/child/spawn"
	case "litter_list":
		return "/member/child/list"
	case "litter_inspect":
		return "/member/child/inspect"
	case "litter_message":
		return "/member/child/message"
	case "litter_stop":
		return "/member/child/stop"
	case "litter_wait":
		return "/member/child/wait"
	case "team_spawn":
		return "/member/team/spawn"
	case "team_list":
		return "/member/team/list"
	case "team_send":
		return "/member/team/send"
	case "team_plan":
		return "/member/team/plan"
	case "team_shutdown":
		return "/member/team/shutdown"
	case "team_task_create":
		return "/member/team/task/create"
	case "team_task_get":
		return "/member/team/task/get"
	case "team_task_list":
		return "/member/team/task/list"
	case "team_task_update":
		return "/member/team/task/update"
	default:
		return ""
	}
}

func (member *nativeMember) factory(api extension.API) error {
	member.fixtureHooks(api)
	if member.manifest.Observer {
		member.observerTrace = newObserverCallTrace()
		api.OnInput(func(_ context.Context, event extension.InputEvent) (extension.InputEventResult, error) {
			member.mu.Lock()
			initial := !member.observerInput && event.Text == member.manifest.Prompt && len(event.Images) == 0
			if initial {
				member.observerInput = true
			}
			member.mu.Unlock()
			if initial {
				return extension.InputEventResultContinue{}, nil
			}
			return extension.InputEventResultHandled{}, nil
		})
		api.OnMessageUpdate(func(_ context.Context, event extension.MessageUpdateEvent) error {
			if update, ok := event.AssistantMessageEvent.(ai.AssistantMessageEvent); ok {
				member.observerTrace.observe(update)
			}
			return nil
		})
	}
	if err := member.registerMCP(api); err != nil {
		return err
	}
	for _, name := range member.manifest.Tools {
		route := nativeMemberRoute(name)
		if route == "" {
			continue
		}
		parameters, err := json.Marshal(schemas[name])
		if err != nil {
			return err
		}
		api.RegisterTool(extension.ToolDefinition{Name: name, Label: name, Description: descriptions[name], PromptSnippet: descriptions[name], PromptGuidelines: promptGuidelines[name], Parameters: parameters, ExecutionMode: "parallel", Execute: func(ctx context.Context, callID string, args json.RawMessage, _ extension.AgentToolUpdateCallback) (extension.AgentToolResult, error) {
			ext := extension.FromContext(ctx)
			if ext == nil {
				return nativeToolError("native session is unavailable"), nil
			}
			manager, err := ext.SessionManager()
			if err != nil {
				return nativeToolError("native session is unavailable"), nil
			}
			leaf := ""
			if current := manager.GetLeafID(); current != nil {
				leaf = *current
			}
			request := memberOperationRequest{Turn: member.currentTurn(), CallID: callID, Leaf: leaf, Args: args}
			var reply memberOperationReply
			if err := member.call(ctx, route, request, &reply); err != nil {
				member.stop(ctx)
				return nativeToolError("native member authority is unavailable"), nil
			}
			member.mu.Lock()
			member.calls[callID] = struct{}{}
			member.mu.Unlock()
			return extension.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: reply.Content}}, IsError: reply.IsError}, nil
		}})
	}
	api.OnSessionStart(func(ctx context.Context, _ extension.SessionStartEvent) error {
		if err := member.sessionReady(ctx); err != nil {
			member.stop(ctx)
			return err
		}
		member.startControl(ctx, api)
		if member.manifest.PeerWake {
			if err := member.startPeerWake(ctx, api); err != nil {
				member.stop(ctx)
				return err
			}
		}
		return nil
	})
	api.OnBeforeAgentStart(func(context.Context, extension.BeforeAgentStartEvent) (extension.BeforeAgentStartEventResult, error) {
		member.mu.Lock()
		pending := member.pendingTools
		member.pendingTools = nil
		member.mu.Unlock()
		if len(pending) == 0 {
			return extension.BeforeAgentStartEventResult{}, nil
		}
		message := "<system-notice>\nSome allowed tools are still connecting: " + strings.Join(pending, ", ") + ". They can become available during this run.\n</system-notice>"
		return extension.BeforeAgentStartEventResult{Message: &extension.CustomMessageRef{CustomType: "pig_litter_pending_tools", Content: message}}, nil
	})
	api.OnTurnStart(func(ctx context.Context, _ extension.TurnStartEvent) error {
		if err := member.waitWindow(ctx); err != nil {
			member.stop(ctx)
			return err
		}
		member.mu.Lock()
		member.turn++
		if member.manifest.Observer {
			member.observerInput = true
		}
		clear(member.calls)
		if member.continuation != nil {
			close(member.continuation)
			member.continuation = nil
		}
		turn := member.turn
		member.mu.Unlock()
		var reply map[string]bool
		if !member.manifest.Observer {
			if err := member.call(ctx, "/member/peer/observe", struct{}{}, &reply); err != nil {
				member.stop(ctx)
				return err
			}
		}
		if err := member.call(ctx, "/member/turn/start", memberTurnRequest{ID: turn}, &reply); err != nil {
			member.stop(ctx)
			return err
		}
		return nil
	})
	api.OnTurnEnd(func(ctx context.Context, _ extension.TurnEndEvent) (extension.TurnEndEventResult, error) {
		var reply map[string]bool
		if !member.manifest.Observer {
			if err := member.call(ctx, "/member/peer/observe", struct{}{}, &reply); err != nil {
				member.stop(ctx)
				return extension.BoundaryResult{}, err
			}
		}
		if err := member.call(ctx, "/member/turn/end", memberTurnRequest{ID: member.currentTurn()}, &reply); err != nil {
			member.stop(ctx)
			return extension.BoundaryResult{}, err
		}
		return extension.BoundaryResult{}, nil
	})
	api.OnToolCall(member.beforeTool)
	api.OnToolExecutionEnd(member.afterTool)
	api.OnUserBash(func(context.Context, extension.UserBashEvent) (extension.UserBashEventResult, error) {
		return extension.UserBashEventResult{Result: map[string]any{"output": "Terminal shell commands are unavailable for this teammate.", "exitCode": 1, "cancelled": false, "truncated": false}}, nil
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
	api.OnAgentBeforeSettle(member.beforeSettle)
	api.OnAgentSettled(func(ctx context.Context, event extension.AgentSettledEvent) error {
		return member.settled(ctx, api, event)
	})
	return nil
}

func (member *nativeMember) startPeerWake(ctx context.Context, api extension.API) error {
	var reply memberPeerReply
	if err := member.call(ctx, "/member/peer/offer", struct{}{}, &reply); err != nil {
		return err
	}
	if reply.Peer == nil {
		return errors.New("native peer wake has no authenticated message")
	}
	member.mu.Lock()
	started := make(chan struct{})
	member.continuation = started
	member.expectWindow = member.windowSeq + 1
	member.mu.Unlock()
	go func() {
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		select {
		case <-started:
		case <-timer.C:
			member.stop(ctx)
		}
	}()
	trigger := true
	api.SendMessage(extension.SendMessagePayload{CustomType: "pig_litter_peer", Content: reply.Peer.Content, Display: true, Details: map[string]any{"envelope": reply.Peer.Envelope}}, &extension.SendMessageOptions{TriggerTurn: &trigger})
	return nil
}

func nativeToolError(message string) extension.AgentToolResult {
	return extension.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: message}}, IsError: true}
}

func (member *nativeMember) sessionReady(ctx context.Context) error {
	ext := extension.FromContext(ctx)
	if ext == nil {
		return errors.New("native readiness context is unavailable")
	}
	manager, err := ext.SessionManager()
	if err != nil || manager.GetSessionFile() == nil {
		return errors.New("native readiness session is unavailable")
	}
	cwd, err := ext.CWD()
	if err != nil {
		return err
	}
	model, err := ext.Model()
	if err != nil {
		return err
	}
	if ChildID(manager.GetSessionId()) != member.manifest.Session || cwd != member.manifest.Cwd || nativeModelName(model) != member.manifest.Model {
		return errors.New("native readiness does not match its launch")
	}
	tools, pending, err := member.waitTools(ctx, ext)
	if err != nil {
		return err
	}
	member.mu.Lock()
	ready, window := member.ready, member.window
	member.mu.Unlock()
	if ready {
		return nil
	}
	entries, err := rawEntries(manager.GetEntries())
	if err != nil {
		return err
	}
	request := memberReadiness{Window: string(window), Session: member.manifest.Session, Cwd: cwd, Model: nativeModelName(model), Tools: tools, PendingTools: pending, SessionFile: *manager.GetSessionFile(), Leaf: manager.GetLeafID(), Entries: entries}
	if member.manifest.ResumeFile != "" {
		count, digest := len(entries), memberEntriesDigest(entries)
		request.Entries = nil
		request.HistoryCount = &count
		request.HistoryDigest = hex.EncodeToString(digest[:])
	}
	var reply map[string]bool
	readyCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if err := member.retry(readyCtx, "/input/ready", request, &reply); err != nil {
		return err
	}
	member.mu.Lock()
	member.ready = true
	member.pendingTools = pending
	member.mu.Unlock()
	return nil
}

func (member *nativeMember) beforeTool(ctx context.Context, event extension.ToolCallEvent) (extension.ToolCallEventResult, error) {
	call, ok := event.(extension.CustomToolCallEvent)
	if !ok {
		return extension.ToolCallEventResult{Block: true, Reason: "native tool event is unavailable"}, nil
	}
	ext := extension.FromContext(ctx)
	if ext == nil {
		return extension.ToolCallEventResult{Block: true, Reason: "native session is unavailable"}, nil
	}
	manager, err := ext.SessionManager()
	if err != nil {
		return extension.ToolCallEventResult{Block: true, Reason: "native session is unavailable"}, nil
	}
	id, err := nativeAssistantEntry(manager, call.ToolCallID)
	if err != nil {
		return extension.ToolCallEventResult{Block: true, Reason: err.Error()}, nil
	}
	turn := member.currentTurn()
	var observed map[string]bool
	if err := member.call(ctx, "/member/response", memberResponseRequest{Turn: turn, EntryID: id}, &observed); err != nil {
		member.stop(ctx)
		return extension.ToolCallEventResult{Block: true, Reason: "native member authority is unavailable"}, nil
	}
	arguments, err := json.Marshal(call.Input)
	if err != nil {
		return extension.ToolCallEventResult{Block: true, Reason: "native tool input is invalid"}, nil
	}
	if call.WireInput != nil {
		arguments = bytes.Clone(*call.WireInput)
	}
	if member.manifest.Observer && call.ToolName == "observer_report" {
		original, ok := member.observerTrace.take(call.ToolCallID)
		id, body, originalErr := parseObserverReport(original)
		validatedID, validatedBody, validatedErr := parseObserverReport(arguments)
		if !ok || originalErr != nil || validatedErr != nil || id != validatedID || body != validatedBody {
			return extension.ToolCallEventResult{Block: true, Reason: "observer report arguments are invalid or unavailable"}, nil
		}
	}
	var decision memberToolDecision
	if err := member.call(ctx, "/member/tool", memberToolRequest{Turn: turn, ID: call.ToolCallID, Parent: call.ParentToolCallID, Name: call.ToolName, Arguments: arguments}, &decision); err != nil {
		member.stop(ctx)
		return extension.ToolCallEventResult{Block: true, Reason: "native member authority is unavailable"}, nil
	}
	return extension.ToolCallEventResult{Block: decision.Block, Reason: decision.Reason, Terminate: decision.Terminate}, nil
}

func nativeAssistantEntry(manager extension.ReadonlySessionManager, callID string) (string, error) {
	entries, err := rawEntries(manager.GetEntries())
	if err != nil {
		return "", err
	}
	for index := len(entries) - 1; index >= 0; index-- {
		var entry struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(entries[index], &entry) != nil || entry.Type != "message" {
			continue
		}
		var message agent.AgentMessage
		if json.Unmarshal(entry.Message, &message) != nil || message.Assistant == nil {
			continue
		}
		for _, block := range message.Assistant.Content {
			var id string
			switch tool := block.(type) {
			case ai.ToolCall:
				id = tool.ID
			case *ai.ToolCall:
				id = tool.ID
			}
			if id != "" && (callID == id || strings.HasPrefix(callID, id+"/")) {
				return entry.ID, nil
			}
		}
	}
	return "", errors.New("native tool response was not observed")
}

func (member *nativeMember) afterTool(ctx context.Context, event extension.ToolExecutionEndEvent) error {
	member.mu.Lock()
	_, executed := member.calls[event.ToolCallID]
	delete(member.calls, event.ToolCallID)
	member.mu.Unlock()
	data, err := json.Marshal(event.Result)
	if err != nil {
		return err
	}
	var result struct {
		Content []memberResultBlock `json:"content"`
		IsError bool                `json:"isError"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		return err
	}
	content := make([]memberResultBlock, 0, len(result.Content))
	for _, block := range result.Content {
		if block.Type == "text" {
			content = append(content, block)
		}
	}
	observation := memberToolResult{Turn: member.currentTurn(), CallID: event.ToolCallID, Name: event.ToolName, IsError: event.IsError || result.IsError, Content: content}
	var reply map[string]bool
	if !executed && nativeMissingTool(event, result.Content) {
		ext := extension.FromContext(ctx)
		if ext == nil {
			member.stop(ctx)
			return errors.New("native missing tool context is unavailable")
		}
		manager, err := ext.SessionManager()
		if err == nil {
			var entry string
			entry, err = nativeAssistantEntry(manager, event.ToolCallID)
			if err == nil {
				err = member.call(ctx, "/member/tool/missing", memberMissingTool{Turn: observation.Turn, CallID: event.ToolCallID, Name: event.ToolName, EntryID: entry}, &reply)
			}
		}
		if err != nil {
			member.stop(ctx)
		}
		return err
	}
	if executed {
		if err := member.call(ctx, "/member/result", observation, &reply); err != nil {
			member.stop(ctx)
			return err
		}
	}
	if err := member.call(ctx, "/member/tool/end", observation, &reply); err != nil {
		member.stop(ctx)
		return err
	}
	return nil
}

func nativeMissingTool(event extension.ToolExecutionEndEvent, content []memberResultBlock) bool {
	return event.IsError && event.DurationMs == nil && event.ParentToolCallID == "" && len(content) == 1 && content[0].Type == "text" && content[0].Text == "Tool "+event.ToolName+" not found"
}

func (member *nativeMember) beforeSettle(ctx context.Context, event *extension.AgentBeforeSettleEvent) (extension.AgentBeforeSettleEventResult, error) {
	if event.Continue {
		member.mu.Lock()
		member.expectWindow = member.windowSeq + 1
		member.mu.Unlock()
		return extension.BoundaryResult{}, nil
	}
	if event.Outcome != extension.AgentActivityCompleted {
		return extension.BoundaryResult{}, nil
	}
	member.mu.Lock()
	member.boundary++
	boundary := member.boundary
	member.mu.Unlock()
	report := ""
	if !member.manifest.Observer {
		report = idleReport(event)
	}
	request := memberSettleRequest{Boundary: boundary, Report: report, Completed: true}
	var decision memberSettleReply
	if err := member.call(ctx, "/member/settlement/decide", request, &decision); err != nil || decision.Boundary != boundary {
		member.stop(ctx)
		return extension.BoundaryResult{}, nil
	}
	if decision.Peer != nil && decision.Continue {
		member.mu.Lock()
		member.expectWindow = member.windowSeq + 1
		member.mu.Unlock()
		entries := slices.Clone(event.Entries)
		entries = append(entries, extension.SessionBoundaryDraft{Type: extension.SessionBoundaryDraftCustomMessage, CustomType: "pig_litter_peer", Content: decision.Peer.Content, Display: true, Details: map[string]any{"envelope": decision.Peer.Envelope}})
		cont := true
		return extension.BoundaryResult{Entries: &entries, Continue: &cont}, nil
	}
	if !decision.Allow {
		if !decision.Continue {
			member.stop(ctx)
			return extension.BoundaryResult{}, nil
		}
		member.mu.Lock()
		member.expectWindow = member.windowSeq + 1
		member.mu.Unlock()
		entries := slices.Clone(event.Entries)
		entries = append(entries, extension.SessionBoundaryDraft{Type: extension.SessionBoundaryDraftCustomMessage, CustomType: "pig_litter_idle_policy", Content: memberSettlementFeedback(memberSettlementDecision{feedback: decision.Feedback}), Display: true})
		cont := true
		return extension.BoundaryResult{Entries: &entries, Continue: &cont}, nil
	}
	member.mu.Lock()
	member.approved = boundary
	member.mu.Unlock()
	return extension.BoundaryResult{}, nil
}

func (member *nativeMember) settled(ctx context.Context, api extension.API, event extension.AgentSettledEvent) error {
	member.mu.Lock()
	boundary := member.approved
	member.approved = 0
	member.mu.Unlock()
	if boundary == 0 {
		member.stop(ctx)
		return nil
	}
	var decision memberSettleReply
	if err := member.call(ctx, "/member/settlement/finalize", memberFinalizationRequest{Boundary: boundary, Aborted: event.Aborted}, &decision); err != nil {
		member.stop(ctx)
		return nil
	}
	if !decision.Allow {
		if !event.Aborted && decision.Continue {
			member.continueAfterSettlement(ctx, api, decision.Feedback)
			return nil
		}
		member.stop(ctx)
		return nil
	}
	member.stop(ctx)
	return nil
}

func (member *nativeMember) continueAfterSettlement(ctx context.Context, api extension.API, feedback string) {
	member.mu.Lock()
	if member.continuation != nil {
		member.mu.Unlock()
		member.stop(ctx)
		return
	}
	started := make(chan struct{})
	member.continuation = started
	member.expectWindow = member.windowSeq + 1
	member.mu.Unlock()
	go func() {
		timer := time.NewTimer(15 * time.Second)
		defer timer.Stop()
		select {
		case <-started:
		case <-timer.C:
			member.stop(ctx)
		}
	}()
	defer func() {
		if recover() != nil {
			member.stop(ctx)
		}
	}()
	api.SendUserMessage(memberSettlementFeedback(memberSettlementDecision{feedback: feedback}), nil)
}
