package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"slices"
	"strings"
	"sync"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

type authority struct {
	context   sdk.Context
	sessionID string
	epoch     uint64
}
type canonicalTool struct {
	source      sdk.SourceInfo
	parameters  map[string]any
	description string
}
type ackKey struct {
	caller Ref
	callID string
}
type acknowledgement struct {
	ref  Ref
	done chan struct{}
	once sync.Once
}

type extensionState struct {
	mu             sync.Mutex
	initialization sync.Mutex
	epoch          uint64
	owner          *owner
}

type owner struct {
	state                    *extensionState
	epoch                    uint64
	sessionID, cwd, agentDir string
	context                  sdk.Context
	config                   Config
	services                 *coding.Services
	tree                     *Tree
	canonical                map[string]canonicalTool
	ackMu                    sync.Mutex
	acks                     map[ackKey]*acknowledgement
	outcomeAcks              map[ackKey]*outcomeAcknowledgement
	afterOutcomeSnapshot     func()
}

func memorySettings() (*coding.SettingsManager, error) {
	manager := coding.NewInMemorySettingsManager(coding.Settings{CacheWarming: "off"})
	if err := manager.SetRetryEnabled(false); err != nil {
		return nil, err
	}
	return manager, nil
}

func configuredAgentDir() string {
	if directory := os.Getenv("PIG_CODING_AGENT_DIR"); directory != "" {
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
	settings, err := memorySettings()
	if err != nil {
		return nil, err
	}
	services, err := coding.NewServices(coding.ServicesOptions{CWD: cwd, AgentDir: agentDir, ProjectTrusted: &trusted, SettingsManager: settings})
	if err != nil {
		return nil, err
	}
	tree, err := newTree(config)
	if err != nil {
		services.Close()
		return nil, err
	}
	o := &owner{state: s, epoch: epoch, sessionID: id, cwd: cwd, agentDir: agentDir, context: ctx, config: config, services: services, tree: tree, canonical: map[string]canonicalTool{}, acks: map[ackKey]*acknowledgement{}, outcomeAcks: map[ackKey]*outcomeAcknowledgement{}}
	tools, err := ctx.GetAllTools()
	if err != nil {
		tree.close()
		services.Close()
		return nil, err
	}
	for _, tool := range tools {
		if !slices.Contains(fileNames, tool.Name) || tool.SourceInfo.Source != "builtin" || tool.SourceInfo.Path != "builtin:"+tool.Name {
			continue
		}
		var parameters map[string]any
		if err := json.Unmarshal(tool.Parameters, &parameters); err != nil {
			tree.close()
			services.Close()
			return nil, err
		}
		o.canonical[tool.Name] = canonicalTool{source: tool.SourceInfo, parameters: parameters, description: tool.Description}
	}
	tree.execute, tree.completion, tree.changed = o.executeGeneration, o.sendCompletion, o.widget
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
	s.mu.Unlock()
	if previous == nil {
		return
	}
	previous.ackMu.Lock()
	acks := previous.acks
	previous.acks = map[ackKey]*acknowledgement{}
	outcomeAcks := previous.outcomeAcks
	previous.outcomeAcks = map[ackKey]*outcomeAcknowledgement{}
	previous.ackMu.Unlock()
	for _, ack := range acks {
		ack.once.Do(func() { close(ack.done); previous.tree.cancelPending(ack.ref) })
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

func (o *owner) canonicalTool(ctx sdk.Context, name string) (canonicalTool, error) {
	if err := o.current(); err != nil {
		return canonicalTool{}, err
	}
	callable, err := ctx.Tools()
	if err != nil {
		return canonicalTool{}, err
	}
	registered, err := ctx.GetAllTools()
	if err != nil {
		return canonicalTool{}, err
	}
	original, exists := o.canonical[name]
	if !exists {
		return canonicalTool{}, reject("permission_denied", "canonical "+name+" is unavailable")
	}
	var current *sdk.AgentTool
	for _, tool := range callable {
		if tool.Name == name {
			copy := tool
			current = &copy
			break
		}
	}
	var source sdk.SourceInfo
	for _, tool := range registered {
		if tool.Name == name {
			source = tool.SourceInfo
			break
		}
	}
	if current == nil || source.Source != "builtin" || source.Path != "builtin:"+name || source != original.source {
		return canonicalTool{}, reject("permission_denied", "canonical "+name+" is unavailable or changed")
	}
	var parameters map[string]any
	if err := json.Unmarshal(current.Parameters, &parameters); err != nil {
		return canonicalTool{}, err
	}
	if !reflect.DeepEqual(parameters, original.parameters) {
		return canonicalTool{}, reject("permission_denied", "canonical "+name+" changed")
	}
	return original, nil
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

func (o *owner) deferActivation(key ackKey, ref Ref, done <-chan struct{}, cancelled func() bool) {
	ack := &acknowledgement{ref: ref, done: make(chan struct{})}
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

func (o *owner) watchRequest(key ackKey, ctx sdk.Context) {
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

func (o *owner) settleAck(key ackKey, failed bool) {
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
			o.tree.cancelPending(ack.ref)
			return
		}
		if err := o.tree.activate(ack.ref); err != nil {
			o.tree.cancelPending(ack.ref)
		}
	})
}

type nativeTool struct {
	name        string
	schema      map[string]any
	description string
	execute     func(context.Context, string, json.RawMessage) (agent.AgentToolResult, error)
}

func (t *nativeTool) Name() string  { return t.name }
func (t *nativeTool) Label() string { return t.name }
func (t *nativeTool) Schema() ai.ToolSchema {
	description := t.description
	if description == "" {
		description = descriptions[t.name]
	}
	return ai.ToolSchema{Name: t.name, Description: description, Parameters: t.schema, PromptGuidelines: promptGuidelines[t.name]}
}
func (t *nativeTool) ExecutionMode() agent.ToolExecutionMode { return agent.ToolModeParallel }
func (t *nativeTool) Execute(ctx context.Context, id string, params json.RawMessage, _ agent.ToolUpdateCallback) (agent.AgentToolResult, error) {
	return t.execute(ctx, id, params)
}

func (o *owner) fileTool(r *record, name string) (agent.AgentTool, error) {
	metadata, err := o.canonicalTool(r.authority.context, name)
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(metadata.parameters)
	if err != nil {
		return nil, err
	}
	var parameters map[string]any
	if err := json.Unmarshal(encoded, &parameters); err != nil {
		return nil, err
	}
	return &nativeTool{name: name, schema: parameters, description: metadata.description + " Base reports on the returned tool result. Quote exact content from that result and label paraphrases as summaries. Report failed calls even when later calls succeed.", execute: func(ctx context.Context, _ string, raw json.RawMessage) (agent.AgentToolResult, error) {
		if r.authority.sessionID != o.sessionID || r.authority.epoch != o.epoch {
			return agent.AgentToolResult{}, reject("unavailable", "child authority retired")
		}
		if err := o.current(); err != nil {
			return agent.AgentToolResult{}, err
		}
		trusted, err := r.authority.context.IsProjectTrusted()
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		if !trusted {
			return agent.AgentToolResult{}, reject("permission_denied", "project trust was revoked")
		}
		if !slices.Contains(r.tools, name) || ctx.Err() != nil {
			return agent.AgentToolResult{}, reject("permission_denied", name+" is unavailable to this child")
		}
		if _, err := o.canonicalTool(r.authority.context, name); err != nil {
			return agent.AgentToolResult{}, err
		}
		var args map[string]any
		if err := json.Unmarshal(raw, &args); err != nil {
			return agent.AgentToolResult{}, err
		}
		outcome, err := r.authority.context.ExecuteTool(name, args, &sdk.ExecuteToolOptions{Signal: ctx})
		if err != nil {
			return agent.AgentToolResult{}, err
		}
		blocks := []ai.ToolResultMessageContent{}
		for _, block := range outcome.Result.Content {
			switch block.Type {
			case "text":
				blocks = append(blocks, ai.TextContent{Text: block.Text})
			case "image":
				blocks = append(blocks, ai.ImageContent{Data: block.Data, MimeType: block.MimeType})
			}
		}
		result := agent.AgentToolResult{Content: blocks, Details: outcome.Result.Details, IsError: outcome.IsError || outcome.Result.IsError, Terminate: outcome.Result.Terminate}
		if outcome.Result.StructuredContent != nil {
			encoded, err := json.Marshal(outcome.Result.StructuredContent)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			result.StructuredContent = encoded
		}
		if outcome.Result.Usage != nil {
			encoded, err := json.Marshal(outcome.Result.Usage)
			if err != nil {
				return agent.AgentToolResult{}, err
			}
			if err := json.Unmarshal(encoded, &result.Usage); err != nil {
				return agent.AgentToolResult{}, err
			}
		}
		return result, nil
	}}, nil
}

func (o *owner) executeGeneration(r *record, run *run) (outcome Outcome) {
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
	if err := appendGeneration(manager, run.ref, o.config.MaxHistoryBytes); err != nil {
		o.tree.retireHistory(r)
		outcome.State = Unavailable
		outcome.Error = err.Error()
		return
	}
	settings, err := memorySettings()
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	services, err := coding.NewServices(coding.ServicesOptions{CWD: o.cwd, AgentDir: o.agentDir, ProjectTrusted: &trusted, SettingsManager: settings})
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
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, AbortContext: run.ctx, NewExtensions: nil})
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
		tool, err := o.fileTool(r, name)
		if err != nil {
			outcome.Error = err.Error()
			return
		}
		tools = append(tools, tool)
		allowed[name] = struct{}{}
	}
	if r.canDelegate {
		for _, name := range controlNames {
			tools = append(tools, &nativeTool{name: name, schema: schemas[name], execute: func(ctx context.Context, callID string, raw json.RawMessage) (agent.AgentToolResult, error) {
				value, err := o.operation(ctx, name, raw, run.ref, callID, nil)
				return nativeResult(name, value, err), nil
			}})
			allowed[name] = struct{}{}
		}
	}
	session, err := runtime.New(coding.SessionStartOptions{SessionManager: manager, Model: model, SystemPrompt: r.instructions, ResourceLoader: coding.NoResources, SkipBuiltinTools: true, ExtraTools: tools, AllowedTools: allowed})
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	eventDrain := make(chan struct{})
	go func() {
		defer close(eventDrain)
		for range session.Events() {
		}
	}()
	var observedMu sync.Mutex
	var terminal *agent.AssistantMessage
	var failure string
	var turns int
	var limitHit bool
	ended := make(chan struct{})
	var endedOnce sync.Once
	unsubscribe := session.Subscribe(func(event agent.AgentEvent) {
		switch event := event.(type) {
		case agent.TurnStartEvent:
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
		case agent.ToolExecutionEndEvent:
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
			if event.Message.ToolResult != nil {
				result := event.Message.ToolResult
				o.settleAck(ackKey{run.ref, result.ToolCallID}, result.IsError)
				o.observeOutcome(run.ref, result.ToolName, result.ToolCallID, result.Text(), result.IsError)
			}
			if event.Message.Custom != nil {
				o.observeCompletion(run.ref, event.Message.Custom)
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
		case agent.AgentEndEvent:
			endedOnce.Do(func() { close(ended) })
		}
	})
	run.mu.Lock()
	run.session = session
	close(run.ready)
	run.mu.Unlock()
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
	if sendErr == nil {
		select {
		case <-ended:
		case <-run.ctx.Done():
		}
	}
	stopDelivery()
	<-deliveryDone
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
	unsubscribe()
	o.retireAcks(run.ref)
	o.retireReceipts(run)
	observedMu.Lock()
	if sendErr != nil && failure == "" {
		failure = sendErr.Error()
	}
	run.mu.Lock()
	completionError := run.completionSendError
	run.mu.Unlock()
	if completionError != "" && failure == "" {
		failure = "child completion enqueue failed: " + completionError
	}
	outcome = outcomeFrom(terminal, baseline, session.GetSessionStats(), failure, run.ctx.Err() != nil, limitHit)
	observedMu.Unlock()
	evidence := projectEvidence(run.ref, historyEntries(manager))
	outcome.Evidence = &evidence
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

func (o *owner) retireAcks(caller Ref) {
	o.ackMu.Lock()
	keys := []ackKey{}
	for key := range o.acks {
		if key.caller == caller {
			keys = append(keys, key)
		}
	}
	o.ackMu.Unlock()
	for _, key := range keys {
		o.settleAck(key, true)
	}
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
	children, err := o.tree.list("")
	if err != nil {
		return
	}
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
			if signal == nil {
				signal = o.tree.ctx
			}
			value, err := o.operation(signal, name, raw, Ref{}, ctx.ToolCallID(), &ctx)
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
		if message["role"] == "toolResult" {
			id, _ := message["toolCallId"].(string)
			failed, _ := message["isError"].(bool)
			o.settleAck(ackKey{Ref{}, id}, failed)
			raw, err := json.Marshal(message)
			var result agent.AgentMessage
			if err == nil {
				err = json.Unmarshal(raw, &result)
			}
			if err == nil && result.ToolResult != nil {
				tool := result.ToolResult
				o.observeOutcome(Ref{}, tool.ToolName, tool.ToolCallID, tool.Text(), tool.IsError)
			} else {
				o.settleOutcomeAck(ackKey{Ref{}, id}, false)
			}
		}
		if message["role"] == "custom" {
			o.observeCompletion(Ref{}, message)
		}
		return nil, nil
	})
	ext.OnSessionStart(func(_ sdk.Context, _ map[string]any) (any, error) { state.retire(); return nil, nil })
	ext.OnSessionShutdown(func(_ sdk.Context, _ map[string]any) (any, error) { state.retire(); return nil, nil })
	ext.Command("pig-litter", "Show Pig Litter child controls", func(ctx sdk.Context, _ string) error {
		ctx.Notify("Use litter_list to discover named agents and children. Use litter_spawn to start a background child, then inspect, message, stop, or wait by ID and generation.", "info")
		return nil
	})
	return ext
}
