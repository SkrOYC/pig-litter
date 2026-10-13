package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

type acknowledgement struct {
	run       *run
	operation string
	done      chan struct{}
	once      sync.Once
}

type extensionState struct {
	mu             sync.Mutex
	initialization sync.Mutex
	epoch          uint64
	owner          *owner
	calls          *callScope
}

type owner struct {
	state                    *extensionState
	epoch                    uint64
	sessionID, cwd, agentDir string
	context                  sdk.Context
	config                   Config
	services                 *coding.Services
	tree                     *Tree
	ackMu                    sync.Mutex
	acks                     map[*callTicket]*acknowledgement
	outcomeAcks              map[*callTicket]*outcomeAcknowledgement
	afterOutcomeSnapshot     func()
	afterOutcomeReservation  func()
	rootMu                   sync.Mutex
	rootSignal               context.Context
	rootWatch                chan struct{}
}

func configuredAgentDir() string {
	if os.Getenv("PIG_USE_PI_DIRS") == "1" {
		return coding.DefaultAgentDir()
	}
	if directory := os.Getenv("PIG_CODING_AGENT_DIR"); directory != "" {
		if directory == "~" || strings.HasPrefix(directory, "~/") {
			home, _ := os.UserHomeDir()
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(directory, "~"), "/"))
		}
		return directory
	}
	return coding.DefaultAgentDir()
}

func (o *owner) current() error {
	o.state.mu.Lock()
	active := o.state.owner == o && o.state.epoch == o.epoch
	o.state.mu.Unlock()
	if !active {
		return reject("unavailable", "the root Session retired")
	}
	if err := o.context.Err(); err != nil {
		return reject("unavailable", err.Error())
	}
	id, err := o.context.SessionManager().GetSessionID()
	if err != nil {
		return err
	}
	if id != o.sessionID {
		return reject("unavailable", "the root Session changed")
	}
	return nil
}

func (s *extensionState) ensure(ctx sdk.Context) (*owner, error) {
	s.initialization.Lock()
	defer s.initialization.Unlock()
	id, err := ctx.SessionManager().GetSessionID()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	existing, epoch := s.owner, s.epoch
	s.mu.Unlock()
	if existing != nil && existing.sessionID == id {
		return existing, existing.current()
	}
	if existing != nil {
		s.retire()
		s.mu.Lock()
		epoch = s.epoch
		s.mu.Unlock()
	}
	trusted, err := ctx.IsProjectTrusted()
	if err != nil {
		return nil, err
	}
	if !trusted {
		return nil, reject("permission_denied", "Pig Litter requires a trusted project")
	}
	cwd := ctx.Cwd()
	if cwd == "" {
		return nil, reject("unavailable", "the project directory is unavailable")
	}
	config, err := LoadConfig(cwd)
	if err != nil {
		return nil, err
	}
	agentDir := configuredAgentDir()
	services, err := coding.NewServices(coding.ServicesOptions{CWD: cwd, AgentDir: agentDir, ProjectTrusted: &trusted})
	if err != nil {
		return nil, err
	}
	tree := newTree(config)
	tree.root.id = ChildID(id)
	o := &owner{state: s, epoch: epoch, sessionID: id, cwd: cwd, agentDir: agentDir, context: ctx, config: config, services: services, tree: tree, acks: map[*callTicket]*acknowledgement{}, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}}
	tree.execute, tree.completion, tree.changed = o.executeRun, o.sendCompletion, o.widget
	s.mu.Lock()
	if s.epoch != epoch {
		s.mu.Unlock()
		tree.close()
		services.Close()
		return nil, reject("unavailable", "the root Session changed during initialization")
	}
	s.owner = o
	s.mu.Unlock()
	if err := o.current(); err != nil {
		s.retire()
		return nil, err
	}
	return o, nil
}

func (s *extensionState) retire() {
	s.mu.Lock()
	s.epoch++
	previous := s.owner
	s.owner = nil
	s.calls = nil
	s.mu.Unlock()
	if previous == nil {
		return
	}
	previous.ackMu.Lock()
	acks := previous.acks
	previous.acks = map[*callTicket]*acknowledgement{}
	outcomeAcks := previous.outcomeAcks
	previous.outcomeAcks = map[*callTicket]*outcomeAcknowledgement{}
	previous.ackMu.Unlock()
	for _, ack := range acks {
		ack.once.Do(func() { close(ack.done); previous.tree.cancelPending(ack.run) })
	}
	for _, ack := range outcomeAcks {
		ack.once.Do(func() { close(ack.done) })
	}
	previous.tree.close()
	previous.services.Close()
	if previous.context.HasUI() {
		_ = previous.context.SetWidget("pig-litter", []string{})
	}
}

func (o *owner) modelFor(ctx context.Context, name string, runtime *coding.ModelRuntime) (*ai.Model, error) {
	if err := o.current(); err != nil {
		return nil, err
	}
	if !exactModel.MatchString(name) {
		return nil, reject("unavailable", "an exact provider/model is required")
	}
	provider, modelID, _ := strings.Cut(name, "/")
	registered, err := o.context.ModelRegistry().GetRegisteredProviderIDs()
	if err != nil {
		return nil, err
	}
	if slices.Contains(registered, provider) {
		return nil, reject("unavailable", "an extension-registered parent provider cannot be reproduced in the independent child")
	}
	available, err := o.context.ModelRegistry().GetAvailable()
	if err != nil {
		return nil, err
	}
	parentAvailable := false
	for _, model := range available {
		if model["provider"] == provider && model["id"] == modelID {
			parentAvailable = true
		}
	}
	scope, err := o.context.ScopedModels()
	if err != nil {
		return nil, err
	}
	permitted := len(scope) == 0
	for _, item := range scope {
		if item.Model["provider"] == provider && item.Model["id"] == modelID {
			permitted = true
		}
	}
	model := runtime.GetModel(provider, modelID)
	if !parentAvailable || !permitted || model == nil || !runtime.HasConfiguredAuth(provider) {
		return nil, reject("unavailable", "exact model is unavailable to the root and stock SDK")
	}
	independent, err := runtime.GetAvailable(ctx, provider)
	if err != nil {
		return nil, err
	}
	for _, candidate := range independent {
		if candidate.ID == modelID {
			return model, nil
		}
	}
	return nil, reject("unavailable", "exact model is unavailable to the stock SDK")
}

func (o *owner) deferActivation(key *callTicket, selected *run, done <-chan struct{}, cancelled func() bool, operation string) {
	ack := &acknowledgement{run: selected, operation: operation, done: make(chan struct{})}
	o.ackMu.Lock()
	o.acks[key] = ack
	o.ackMu.Unlock()
	go func() {
		select {
		case <-done:
			if cancelled() {
				o.settleAck(key, true)
			}
		case <-ack.done:
		case <-o.tree.ctx.Done():
			o.settleAck(key, true)
		}
	}()
}

func (o *owner) watchRequest(key *callTicket, ctx sdk.Context) {
	o.ackMu.Lock()
	ack := o.acks[key]
	o.ackMu.Unlock()
	if ack == nil {
		return
	}
	done := ctx.Done()
	go func() {
		select {
		case <-done:
			if ctx.Err() != nil {
				o.settleAck(key, true)
			}
		case <-ack.done:
		case <-o.tree.ctx.Done():
		}
	}()
}

func (o *owner) settleAck(key *callTicket, failed bool) {
	o.ackMu.Lock()
	ack := o.acks[key]
	delete(o.acks, key)
	o.ackMu.Unlock()
	if ack == nil {
		return
	}
	ack.once.Do(func() {
		close(ack.done)
		if failed {
			o.tree.cancelPending(ack.run)
			return
		}
		if err := o.tree.activate(ack.run); err != nil {
			o.tree.cancelPending(ack.run)
		}
	})
}

type nativeTool struct {
	name    string
	schema  map[string]any
	execute func(context.Context, string, json.RawMessage) (agent.AgentToolResult, error)
}

func (t *nativeTool) Name() string  { return t.name }
func (t *nativeTool) Label() string { return t.name }
func (t *nativeTool) Schema() ai.ToolSchema {
	return ai.ToolSchema{Name: t.name, Description: descriptions[t.name], Parameters: t.schema, PromptGuidelines: promptGuidelines[t.name]}
}
func (t *nativeTool) ExecutionMode() agent.ToolExecutionMode { return agent.ToolModeParallel }
func (t *nativeTool) Execute(ctx context.Context, id string, params json.RawMessage, _ agent.ToolUpdateCallback) (agent.AgentToolResult, error) {
	return t.execute(ctx, id, params)
}

func (o *owner) executeRun(r *record, run *run) (outcome Outcome) {
	outcome.State = Failed
	if err := o.current(); err != nil {
		outcome.Error = err.Error()
		return
	}
	trusted, err := o.context.IsProjectTrusted()
	if err != nil || !trusted {
		outcome.Error = "project trust was revoked"
		if err != nil {
			outcome.Error = err.Error()
		}
		return
	}
	o.tree.mu.Lock()
	manager := r.history
	lost := r.historyLost
	o.tree.mu.Unlock()
	if manager == nil || lost {
		outcome.State = Unavailable
		outcome.Error = "retained child history was retired before construction"
		return
	}
	services, err := coding.NewServices(coding.ServicesOptions{CWD: r.cwd, AgentDir: o.agentDir, ProjectTrusted: &trusted})
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	defer services.Close()
	model, err := o.modelFor(run.ctx, r.model, services.ModelRuntime())
	if err != nil {
		outcome.State = Unavailable
		outcome.Error = err.Error()
		return
	}
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, AbortContext: run.ctx, NewExtensions: []extension.Extension{o.completionHooks(r)}})
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	defer func() {
		if err := runtime.Close(); err != nil {
			outcome.State, outcome.Error = Failed, "child cleanup failed: "+err.Error()
		}
	}()
	tools := []agent.AgentTool{}
	allowed := map[string]struct{}{}
	for _, name := range r.tools {
		allowed[name] = struct{}{}
	}
	if r.canDelegate {
		for _, name := range controlNames {
			tools = append(tools, &nativeTool{name: name, schema: schemas[name], execute: func(ctx context.Context, callID string, raw json.RawMessage) (agent.AgentToolResult, error) {
				ticket, err := o.callScope(run).begin(run, callID, name)
				if err != nil {
					return nativeResult(name, nil, err), nil
				}
				value, err := o.operation(ctx, raw, ticket, nil)
				return nativeResult(name, value, err), nil
			}})
			allowed[name] = struct{}{}
		}
	}
	loader := coding.NewDefaultResourceLoader(coding.DefaultResourceLoaderOptions{CWD: r.cwd, AgentDir: o.agentDir, SettingsManager: services.SettingsManager(), AppendSystemPromptOverride: func(found []string) []string {
		return append(found, r.instructions)
	}})
	if err := loader.Reload(); err != nil {
		outcome.Error = err.Error()
		return
	}
	active := services.SettingsManager().ResolvedDefaultTools()
	if r.canDelegate {
		active = append(active, controlNames...)
	}
	r.deliveryMu.Lock()
	session, err := runtime.New(coding.SessionStartOptions{SessionManager: manager, Model: model, ResourceLoader: loader, ExtraTools: tools, AllowedTools: allowed})
	if err != nil {
		r.deliveryMu.Unlock()
		outcome.Error = err.Error()
		return
	}
	session.SetActiveToolsByName(active)
	eventDrain := make(chan struct{})
	go func() {
		defer close(eventDrain)
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	var observedMu sync.Mutex
	var terminal *agent.AssistantMessage
	var failure string
	var turns int
	var limitHit bool
	unsubscribe := session.Subscribe(func(event agent.AgentEvent) {
		switch event := event.(type) {
		case agent.TurnStartEvent:
			run.mu.Lock()
			previous := run.calls
			run.calls = newCallScope()
			run.mu.Unlock()
			o.retireScope(previous)
			observedMu.Lock()
			turns++
			exceeded := turns > o.config.MaxTurns
			if exceeded {
				failure = "child turn limit reached"
				limitHit = true
			}
			observedMu.Unlock()
			o.tree.mu.Lock()
			if run.state == Starting {
				run.state = Running
			}
			o.tree.mu.Unlock()
			o.widget()
			if exceeded {
				session.RequestAbort()
			}
		case agent.TurnEndEvent:
			o.retireScope(o.callScope(run))
		case agent.ToolExecutionEndEvent:
			o.observeCall(run, event.ToolName, event.ToolCallID, event.Result, event.IsError)
			if event.IsError || event.Result.IsError {
				observedMu.Lock()
				if failure == "" {
					failure = toolFailure(event.ToolName, event.Result)
				}
				observedMu.Unlock()
			}
		case agent.MessageEndEvent:
			if event.Message.Assistant != nil {
				observedMu.Lock()
				terminal = event.Message.Assistant
				observedMu.Unlock()
			}
			if event.Message.Custom != nil {
				o.observeCompletion(run, event.Message.Custom)
			}
			o.tree.mu.Lock()
			retired := r.history != manager || r.historyLost
			o.tree.mu.Unlock()
			if retired || historyBytes(manager) > o.config.MaxHistoryBytes {
				observedMu.Lock()
				failure = "child history limit reached"
				limitHit = true
				observedMu.Unlock()
				session.RequestAbort()
			}
		}
	})
	run.mu.Lock()
	run.session = session
	close(run.ready)
	run.mu.Unlock()
	r.deliveryMu.Unlock()
	deliveryContext, stopDelivery := context.WithCancel(run.ctx)
	deliveryDone := make(chan struct{})
	go func() {
		defer close(deliveryDone)
		for {
			select {
			case request := <-run.mailbox:
				var err error
				if deliveryContext.Err() != nil {
					err = reject("cancelled", "child stopped before message delivery")
				} else if request.mode == "steer" {
					_, err = session.Steer(deliveryContext, request.text, nil, nil)
				} else {
					_, err = session.FollowUp(deliveryContext, request.text, nil, nil)
				}
				request.result <- err
				run.messageMu.Lock()
				run.pendingMessages--
				run.messageMu.Unlock()
			case <-deliveryContext.Done():
				return
			}
		}
	}()
	baseline := session.GetSessionStats()
	_, sendErr := session.Send(run.ctx, run.prompt)
	stopDelivery()
	<-deliveryDone
	session.ClearQueue()
	r.deliveryMu.Lock()
	run.mu.Lock()
	run.disposing = true
	run.session = nil
	run.mu.Unlock()
	if err := session.Close(); err != nil {
		observedMu.Lock()
		failure = "child cleanup failed: " + err.Error()
		observedMu.Unlock()
	}
	<-eventDrain
	r.deliveryMu.Unlock()
	unsubscribe()
	o.retireScope(o.callScope(run))
	o.retireReceipts(r)
	observedMu.Lock()
	if sendErr != nil && failure == "" {
		failure = sendErr.Error()
	}
	outcome = outcomeFrom(terminal, baseline, session.GetSessionStats(), failure, run.ctx.Err() != nil, limitHit)
	observedMu.Unlock()
	if size := historyBytes(manager); size > o.config.MaxHistoryBytes {
		o.tree.retireHistory(r)
		outcome.State = Partial
		outcome.Error = fmt.Sprintf("retained child history exceeded the configured limit (%d > %d bytes); resume unavailable", size, o.config.MaxHistoryBytes)
	}
	return outcome
}

func outcomeFrom(terminal *agent.AssistantMessage, before, after coding.SessionStats, failure string, stopped, limit bool) Outcome {
	outcome := Outcome{State: Failed, Error: failure}
	if stopped {
		outcome.State = Stopped
	} else if terminal != nil {
		switch terminal.StopReason {
		case ai.StopReason("stop"):
			outcome.State = Completed
		case ai.StopReason("length"):
			outcome.State = Partial
		}
	}
	if terminal != nil {
		for _, block := range terminal.Content {
			if text, ok := block.(ai.TextContent); ok {
				outcome.Text += text.Text
			}
		}
		if outcome.Error == "" {
			outcome.Error = terminal.ErrorMessage
		}
	}
	if failure != "" && outcome.State == Completed {
		outcome.State = Partial
	}
	if limit && !stopped {
		if terminal == nil {
			outcome.State = Failed
		} else {
			outcome.State = Partial
		}
	}
	outcome.Usage = Usage{Input: max(0, after.Tokens.Input-before.Tokens.Input), Output: max(0, after.Tokens.Output-before.Tokens.Output), CacheRead: max(0, after.Tokens.CacheRead-before.Tokens.CacheRead), CacheWrite: max(0, after.Tokens.CacheWrite-before.Tokens.CacheWrite), TotalTokens: max(0, after.Tokens.Total-before.Tokens.Total), Cost: max(0, after.Cost-before.Cost)}
	return outcome
}

func (o *owner) widget() {
	if !o.context.HasUI() {
		return
	}
	o.state.mu.Lock()
	active := o.state.owner == o
	o.state.mu.Unlock()
	if !active {
		return
	}
	children := o.tree.retained()
	_ = o.context.SetWidget("pig-litter", widgetLines(children, o.context.Width()))
}

func Extension() *sdk.Extension {
	return newExtension(&extensionState{})
}

func Run() error {
	state := &extensionState{}
	defer state.retire()
	return newExtension(state).Run()
}

func newExtension(state *extensionState) *sdk.Extension {
	ext := sdk.New("pig-litter")
	for _, name := range controlNames {
		ext.RegisterTool(sdk.ToolDefinition{Name: name, Label: name, Description: descriptions[name], PromptSnippet: descriptions[name], PromptGuidelines: promptGuidelines[name], Parameters: schemas[name], ExecutionMode: "parallel", Execute: func(ctx sdk.Context, params map[string]any) (any, error) {
			o, err := state.ensure(ctx)
			if err != nil {
				return sdkResult(name, nil, err), nil
			}
			raw, err := json.Marshal(params)
			if err != nil {
				return sdkResult(name, nil, err), nil
			}
			signal := ctx.Signal()
			o.watchRoot(signal)
			if signal == nil {
				signal = o.tree.ctx
			}
			ticket, err := o.callScope(nil).begin(nil, ctx.ToolCallID(), name)
			if err != nil {
				return sdkResult(name, nil, err), nil
			}
			value, err := o.operation(signal, raw, ticket, &ctx)
			return sdkResult(name, value, err), nil
		}})
	}
	ext.OnEvent("message_end", func(_ sdk.Context, data map[string]any) (any, error) {
		state.mu.Lock()
		o := state.owner
		state.mu.Unlock()
		if o == nil {
			return nil, nil
		}
		message, _ := data["message"].(map[string]any)
		if message["role"] == "custom" {
			o.observeCompletion(nil, message)
		}
		return nil, nil
	})
	ext.OnEvent("turn_start", func(_ sdk.Context, _ map[string]any) (any, error) {
		state.mu.Lock()
		previous, o := state.calls, state.owner
		state.calls = newCallScope()
		state.mu.Unlock()
		if o != nil {
			o.retireScope(previous)
		}
		return nil, nil
	})
	ext.OnEvent("turn_end", func(_ sdk.Context, _ map[string]any) (any, error) {
		state.mu.Lock()
		scope, o := state.calls, state.owner
		state.mu.Unlock()
		if o != nil {
			o.retireScope(scope)
		}
		return nil, nil
	})
	ext.OnEvent("tool_execution_end", func(_ sdk.Context, data map[string]any) (any, error) {
		state.mu.Lock()
		o := state.owner
		state.mu.Unlock()
		if o != nil {
			o.observeToolEvent(data)
		}
		return nil, nil
	})
	ext.OnEvent("context", func(_ sdk.Context, data map[string]any) (any, error) {
		state.mu.Lock()
		o := state.owner
		state.mu.Unlock()
		if o == nil {
			return nil, nil
		}
		messages, ok := data["messages"].([]any)
		if !ok {
			return nil, nil
		}
		return map[string]any{"messages": o.completionContext(o.tree.root, messages)}, nil
	})
	ext.OnEvent("agent_before_settle", func(_ sdk.Context, _ map[string]any) (any, error) {
		state.mu.Lock()
		o := state.owner
		state.mu.Unlock()
		if o != nil && o.continueForCompletions(o.tree.root) {
			return map[string]any{"continue": true}, nil
		}
		return nil, nil
	})
	ext.OnEvent("agent_start", func(ctx sdk.Context, _ map[string]any) (any, error) {
		state.mu.Lock()
		o := state.owner
		state.mu.Unlock()
		if o != nil {
			o.watchRoot(ctx.Signal())
		}
		return nil, nil
	})
	ext.OnEvent("agent_settled", func(_ sdk.Context, _ map[string]any) (any, error) {
		state.mu.Lock()
		o := state.owner
		state.mu.Unlock()
		if o != nil {
			o.settleRoot()
			o.retireScope(o.callScope(nil))
			o.retireReceipts(o.tree.root)
		}
		return nil, nil
	})
	ext.OnSessionStart(func(_ sdk.Context, _ map[string]any) (any, error) { state.retire(); return nil, nil })
	ext.OnSessionShutdown(func(_ sdk.Context, _ map[string]any) (any, error) { state.retire(); return nil, nil })
	ext.Command("pig-litter", "Show Pig Litter child controls", func(ctx sdk.Context, _ string) error {
		ctx.Notify("Use litter_list to discover named agents and children. Use litter_spawn to start a background child, then inspect, message, stop, or wait by ID.", "info")
		return nil
	})
	return ext
}

func toolFailure(tool string, result agent.AgentToolResult) string {
	var text strings.Builder
	for _, block := range result.Content {
		if block, ok := block.(ai.TextContent); ok {
			text.WriteString(block.Text)
		}
	}
	preview, clipped := bounded(text.String(), 512)
	message := fmt.Sprintf("tool %s failed", tool)
	if preview != "" {
		message += ": " + preview
	}
	if clipped {
		message += " [truncated]"
	}
	return message
}

func (o *owner) observeCall(caller *run, operation, callID string, result agent.AgentToolResult, failed bool) {
	ticket := o.callScope(caller).capture(callID, operation)
	o.finishCall(ticket, result, failed)
}

func (o *owner) finishCall(ticket *callTicket, result agent.AgentToolResult, failed bool) {
	if ticket == nil {
		return
	}
	var content strings.Builder
	for _, block := range result.Content {
		if block, ok := block.(ai.TextContent); ok {
			content.WriteString(block.Text)
		}
	}
	o.settleAck(ticket, failed)
	o.observeOutcome(ticket, ticket.operation, content.String(), failed)
	ticket.scope.forget(ticket)
}

func (o *owner) observeToolEvent(data map[string]any) {
	raw, err := json.Marshal(data)
	var event struct {
		ToolCallID string `json:"toolCallId"`
		ToolName   string `json:"toolName"`
		IsError    bool   `json:"isError"`
		Result     struct {
			Content []struct{ Type, Text string } `json:"content"`
			Details any                           `json:"details"`
			IsError bool                          `json:"isError"`
		} `json:"result"`
	}
	if err != nil || json.Unmarshal(raw, &event) != nil {
		return
	}
	result := agent.AgentToolResult{Details: event.Result.Details}
	for _, block := range event.Result.Content {
		if block.Type == "text" {
			result.Content = append(result.Content, ai.TextContent{Text: block.Text})
		}
	}
	o.observeCall(nil, event.ToolName, event.ToolCallID, result, event.IsError)
}

func (o *owner) watchRoot(signal context.Context) {
	if signal == nil {
		return
	}
	o.rootMu.Lock()
	if o.rootSignal == signal {
		o.rootMu.Unlock()
		return
	}
	if o.rootWatch != nil {
		close(o.rootWatch)
	}
	done := make(chan struct{})
	o.rootWatch, o.rootSignal = done, signal
	o.tree.mu.Lock()
	if signal.Err() == nil {
		o.tree.root.paused = false
	}
	o.tree.mu.Unlock()
	o.rootMu.Unlock()
	go func() {
		select {
		case <-signal.Done():
			o.rootMu.Lock()
			if o.rootSignal != signal {
				o.rootMu.Unlock()
				return
			}
			o.tree.mu.Lock()
			pending, fresh := o.tree.pauseLocked(o.tree.root)
			o.tree.mu.Unlock()
			o.rootMu.Unlock()
			o.tree.joinPaused(pending, fresh)
		case <-done:
		case <-o.tree.ctx.Done():
		}
	}()
}

func (o *owner) settleRoot() {
	o.rootMu.Lock()
	var pending, fresh []*run
	if o.rootSignal != nil && o.rootSignal.Err() != nil {
		o.tree.mu.Lock()
		pending, fresh = o.tree.pauseLocked(o.tree.root)
		o.tree.mu.Unlock()
	}
	if o.rootWatch != nil {
		close(o.rootWatch)
	}
	o.rootWatch, o.rootSignal = nil, nil
	o.rootMu.Unlock()
	o.tree.joinPaused(pending, fresh)
}
