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
	retiring       <-chan struct{}
	calls          *callScope
	sessionContext *sdk.Context
}

type owner struct {
	state                    *extensionState
	epoch                    uint64
	sessionID, cwd, agentDir string
	context                  sdk.Context
	config                   Config
	services                 *coding.Services
	tree                     *Tree
	rootCalls                *callScope
	ackMu                    sync.Mutex
	ackWG                    sync.WaitGroup
	acks                     map[*callTicket]*acknowledgement
	outcomeAcks              map[*callTicket]*outcomeAcknowledgement
	afterOutcomeSnapshot     func()
	afterOutcomeReservation  func()
	rootMu                   sync.Mutex
	root                     *rootGeneration
	rootClosed               bool
	rootWG                   sync.WaitGroup
	recoveryMu               sync.Mutex
	recovered                []recoveredCompletion
	recoveryParents          map[ChildID]bool
	actionMu                 sync.Mutex
	actionWG                 sync.WaitGroup
	actionContext            context.Context
	actionCancel             context.CancelFunc
	actionsClosed            bool
	persistMu                sync.Mutex
	widgetMu                 sync.Mutex
	widgetStopped            bool
	widgetUnsubscribe        func()
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
	return s.acquire(ctx, true)
}

func (s *extensionState) acquire(ctx sdk.Context, recover bool) (*owner, error) {
	s.initialization.Lock()
	defer s.initialization.Unlock()
	id, err := ctx.SessionManager().GetSessionID()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	existing, epoch, retiring, sessionContext := s.owner, s.epoch, s.retiring, s.sessionContext
	s.mu.Unlock()
	if retiring != nil {
		<-retiring
		s.mu.Lock()
		existing, epoch = s.owner, s.epoch
		s.mu.Unlock()
	}
	if existing != nil && existing.sessionID == id {
		if err := existing.current(); err != nil {
			return nil, err
		}
		if recover {
			existing.startRecovery(nil)
		}
		return existing, nil
	}
	if existing != nil {
		s.retire()
		s.mu.Lock()
		epoch = s.epoch
		s.mu.Unlock()
	}
	if sessionContext != nil {
		stableID, err := sessionContext.SessionManager().GetSessionID()
		if err != nil || stableID != id {
			return nil, reject("unavailable", "the root Session changed during initialization")
		}
		ctx = *sessionContext
	}
	trusted := true
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
	tree.root.paused = !recover
	o := &owner{state: s, epoch: epoch, sessionID: id, cwd: cwd, agentDir: agentDir, context: ctx, config: config, services: services, tree: tree, acks: map[*callTicket]*acknowledgement{}, outcomeAcks: map[*callTicket]*outcomeAcknowledgement{}}
	tree.execute, tree.completion, tree.changed = o.executeRun, o.sendCompletion, o.widget
	if err := o.restoreChildren(); err != nil {
		tree.close()
		services.Close()
		return nil, err
	}
	o.recovered = o.recoveryBatch()
	tree.persist = o.saveChild
	s.mu.Lock()
	if s.epoch != epoch {
		s.mu.Unlock()
		tree.close()
		services.Close()
		return nil, reject("unavailable", "the root Session changed during initialization")
	}
	s.owner = o
	if s.calls == nil {
		s.calls = newCallScope()
	}
	o.rootCalls = s.calls
	s.mu.Unlock()
	if err := o.current(); err != nil {
		s.retire()
		return nil, err
	}
	if err := o.startWidget(); err != nil {
		s.retire()
		return nil, err
	}
	o.widget()
	if recover {
		o.startRecovery(nil)
	}
	return o, nil
}

func (o *owner) startRecovery(caller *run) {
	o.recoveryMu.Lock()
	defer o.recoveryMu.Unlock()
	o.tree.mu.Lock()
	defer o.tree.mu.Unlock()
	if o.tree.closed {
		return
	}
	var parent *record
	if caller != nil {
		var err error
		parent, err = o.tree.callerLocked(caller)
		if err != nil || caller.admission != admissionReady || caller.ctx.Err() != nil {
			return
		}
		o.state.mu.Lock()
		current := o.state.owner == o && o.state.epoch == o.epoch
		o.state.mu.Unlock()
		if !current {
			return
		}
	}
	if o.recoveryParents == nil {
		o.recoveryParents = map[ChildID]bool{}
	}
	var pending []recoveredCompletion
	parents := map[ChildID]bool{}
	for _, recovered := range o.recovered {
		id := recovered.selected.owner.parentID()
		if o.context.Mode() == "tui" && o.tree.rootID(id) {
			continue
		}
		if !o.recoveryParents[id] && (parent == nil || id == parent.id) {
			pending = append(pending, recovered)
			parents[id] = true
		}
	}
	for id := range parents {
		o.recoveryParents[id] = true
	}
	if len(pending) == 0 {
		return
	}
	o.tree.completionWG.Add(1)
	go func() {
		defer o.tree.completionWG.Done()
		o.recoverCompletions(pending)
	}()
}

func (s *extensionState) retire() {
	s.mu.Lock()
	s.epoch++
	previous := s.owner
	var retirement chan struct{}
	if previous != nil {
		retirement = make(chan struct{})
		s.retiring = retirement
	}
	s.owner = nil
	s.calls = nil
	retiring := s.retiring
	s.mu.Unlock()
	if previous == nil {
		if retiring != nil {
			<-retiring
		}
		return
	}
	previous.stopWidget()
	previous.actionMu.Lock()
	previous.actionsClosed = true
	if previous.actionCancel != nil {
		previous.actionCancel()
	}
	previous.actionMu.Unlock()
	previous.actionWG.Wait()
	previous.retireScope(previous.rootCalls)
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
	previous.rootMu.Lock()
	previous.rootClosed = true
	root := previous.root
	if root != nil && root.watch != nil {
		close(root.watch)
		root.watch = nil
	}
	previous.rootMu.Unlock()
	previous.tree.close()
	previous.rootWG.Wait()
	previous.ackWG.Wait()
	previous.services.Close()
	close(retirement)
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
	o.ackWG.Add(1)
	go func() {
		defer o.ackWG.Done()
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
	o.ackWG.Add(1)
	go func() {
		defer o.ackWG.Done()
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
	trusted := true
	if err := o.saveChild(r); err != nil {
		outcome.State = Unavailable
		outcome.Error = err.Error()
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
	available, err := o.rootToolNames()
	if err != nil {
		outcome.State, outcome.Error = Unavailable, err.Error()
		return
	}
	tools := []agent.AgentTool{}
	allowed := map[string]struct{}{}
	for _, name := range expandTools(r.tools, available) {
		if withinParent(r.parent, o.tree.root, name) {
			allowed[name] = struct{}{}
		}
	}
	if r.canDelegate {
		for _, name := range controlNames {
			if !slices.Contains(available, name) {
				continue
			}
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
	extensions, registrations, err := o.childExtensions(r, allowed)
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services, AbortContext: run.ctx, NewExtensions: extensions, ExtensionRuntime: registrations})
	if err != nil {
		outcome.Error = err.Error()
		return
	}
	defer func() {
		if err := runtime.Close(); err != nil {
			outcome.State, outcome.Error = Failed, "child cleanup failed: "+err.Error()
		}
	}()
	loader := coding.NewDefaultResourceLoader(coding.DefaultResourceLoaderOptions{CWD: r.cwd, AgentDir: o.agentDir, SettingsManager: services.SettingsManager(), AppendSystemPromptOverride: func(found []string) []string {
		return append(found, r.instructions)
	}})
	if err := loader.Reload(); err != nil {
		outcome.Error = err.Error()
		return
	}
	r.deliveryMu.Lock()
	options := coding.SessionStartOptions{Model: model, ResourceLoader: loader, ExtraTools: tools, AllowedTools: allowed}
	var session *coding.Session
	if run.resume {
		session, err = runtime.Resume(string(r.id), options)
		if err == nil {
			run.prompt = run.continuation
			o.tree.mu.Lock()
			r.pendingTask = ""
			o.tree.mu.Unlock()
		}
		if err != nil && r.pendingTask != "" && (len(r.pendingInputs) == 0 || r.pendingInputs[0].After == nil) {
			options.SessionID = string(r.id)
			session, err = runtime.New(options)
		}
	} else {
		options.SessionID = string(r.id)
		session, err = runtime.New(options)
	}
	if err != nil {
		r.deliveryMu.Unlock()
		outcome.State = Unavailable
		outcome.Error = err.Error()
		return
	}
	eventDrain := make(chan struct{})
	go func() {
		defer close(eventDrain)
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	bindErr := session.BindExtensions(run.ctx)
	var pendingTools []string
	if bindErr == nil {
		pendingTools = waitForChildToolStartup(run.ctx, session, allowed)
		bindErr = run.ctx.Err()
	}
	if bindErr != nil {
		session.EmitSessionShutdown("quit")
		_ = session.Close()
		<-eventDrain
		r.deliveryMu.Unlock()
		outcome.Error = bindErr.Error()
		return
	}
	active := session.ActiveToolNames()
	for _, name := range append([]string{"codemode", "tool_search"}, controlNames...) {
		if _, exists := allowed[name]; exists {
			active = append(active, name)
		}
	}
	session.SetActiveToolsByName(active)
	o.tree.mu.Lock()
	r.activeLog = session.SessionManager()
	o.tree.mu.Unlock()
	var observedMu sync.Mutex
	var terminal *agent.AssistantMessage
	var failure string
	var turns int
	var limitHit bool
	unsubscribe := session.Subscribe(func(event agent.AgentEvent) {
		o.tree.observeProgress(run, event)
		switch event := event.(type) {
		case agent.TurnStartEvent:
			o.startRecovery(run)
			o.confirmCompletions(r)
			run.mu.Lock()
			previous := run.calls
			run.calls = newCallScope()
			run.mu.Unlock()
			o.retireScope(previous)
			observedMu.Lock()
			turns++
			warn := turns == o.config.MaxTurns
			if warn {
				limitHit = true
			}
			observedMu.Unlock()
			o.tree.mu.Lock()
			if run.state == Starting {
				run.state = Running
			}
			o.tree.mu.Unlock()
			o.widget()
			if warn {
				session.QueueSteer("<system-notice>\nYou have reached your turn allowance. Return a final response with your partial progress, remaining work, and any request for more turns. Your parent can resume this session to grant a fresh allowance.\n</system-notice>", nil)
			}
		case agent.TurnEndEvent:
			o.retireScope(o.callScope(run))
			o.confirmCompletions(r)
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
	if len(pendingTools) != 0 {
		session.QueueSteer("<system-notice>\nSome allowed tools are still connecting: "+strings.Join(pendingTools, ", ")+". They can become available during this run.\n</system-notice>", nil)
	}
	sendErr := o.tree.sendInput(session, run)
	stopDelivery()
	<-deliveryDone
	session.ClearQueue()
	r.deliveryMu.Lock()
	run.mu.Lock()
	run.disposing = true
	run.session = nil
	run.mu.Unlock()
	session.EmitSessionShutdown("quit")
	if err := session.Close(); err != nil {
		observedMu.Lock()
		failure = "child cleanup failed: " + err.Error()
		observedMu.Unlock()
	}
	<-eventDrain
	o.tree.mu.Lock()
	r.activeLog = nil
	o.tree.mu.Unlock()
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
		outcome.State = Partial
		if outcome.Error == "" {
			outcome.Error = "soft turn allowance reached; resume to continue with a fresh allowance"
		}
	}
	outcome.Usage = Usage{Input: max(0, after.Tokens.Input-before.Tokens.Input), Output: max(0, after.Tokens.Output-before.Tokens.Output), CacheRead: max(0, after.Tokens.CacheRead-before.Tokens.CacheRead), CacheWrite: max(0, after.Tokens.CacheWrite-before.Tokens.CacheWrite), TotalTokens: max(0, after.Tokens.Total-before.Tokens.Total), Cost: max(0, after.Cost-before.Cost)}
	return outcome
}

func (o *owner) startWidget() error {
	if !o.context.HasUI() {
		return nil
	}
	o.widgetMu.Lock()
	defer o.widgetMu.Unlock()
	if o.widgetStopped {
		return reject("unavailable", "the root Session retired")
	}
	unsubscribe, err := o.context.OnWidthChange(func(_ sdk.Context, _ int) { o.widget() })
	if err != nil {
		return err
	}
	o.widgetUnsubscribe = unsubscribe
	return nil
}

func (o *owner) stopWidget() {
	o.widgetMu.Lock()
	defer o.widgetMu.Unlock()
	o.widgetStopped = true
	if o.widgetUnsubscribe != nil {
		o.widgetUnsubscribe()
		o.widgetUnsubscribe = nil
	}
	if o.context.HasUI() {
		_ = o.context.SetWidget("pig-litter", []string{})
	}
}

func (o *owner) widget() {
	o.widgetMu.Lock()
	defer o.widgetMu.Unlock()
	if o.widgetStopped {
		return
	}
	if !o.context.HasUI() {
		return
	}
	o.state.mu.Lock()
	active := o.state.owner == o && o.state.epoch == o.epoch
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
		ext.RegisterTool(sdk.ToolDefinition{Name: name, Label: name, Description: descriptions[name], PromptSnippet: descriptions[name], PromptGuidelines: promptGuidelines[name], Parameters: schemas[name], RenderShell: sdk.ToolRenderShellDefault, RenderCall: toolCallRenderer(name), RenderResult: toolResultRenderer(name), ExecutionMode: "parallel", Execute: func(ctx sdk.Context, params map[string]any) (any, error) {
			o, err := state.ensure(ctx)
			if err != nil {
				return sdkResult(name, nil, err), nil
			}
			signal := ctx.Signal()
			if signal == nil {
				signal = o.tree.ctx
			}
			action, err := o.beginAction(signal)
			if err != nil {
				return sdkResult(name, nil, err), nil
			}
			defer action.finish()
			raw, err := json.Marshal(params)
			if err != nil {
				return sdkResult(name, nil, err), nil
			}
			o.watchRoot(signal)
			ticket, err := action.scope.begin(nil, ctx.ToolCallID(), name)
			if err != nil {
				return sdkResult(name, nil, err), nil
			}
			value, err := o.operation(action.ctx, raw, ticket, &ctx)
			return sdkResult(name, value, err), nil
		}})
	}
	ext.OnEvent("message_end", func(ctx sdk.Context, data map[string]any) (any, error) {
		return state.observe(ctx, func(o *owner) (any, error) {
			message, _ := data["message"].(map[string]any)
			if message["role"] == "custom" {
				o.observeCompletion(nil, message)
			}
			return nil, nil
		})
	})
	ext.OnEvent("turn_start", func(ctx sdk.Context, _ map[string]any) (any, error) {
		return state.observe(ctx, func(o *owner) (any, error) {
			state.mu.Lock()
			if state.owner != o || state.epoch != o.epoch {
				state.mu.Unlock()
				return nil, nil
			}
			previous := o.rootCalls
			state.calls = newCallScope()
			o.rootCalls = state.calls
			state.mu.Unlock()
			o.confirmCompletions(o.tree.root)
			o.tree.root.markAgentEnded(false)
			o.retireScope(previous)
			return nil, nil
		})
	})
	ext.OnEvent("turn_end", func(ctx sdk.Context, _ map[string]any) (any, error) {
		return state.observe(ctx, func(o *owner) (any, error) {
			o.retireScope(o.callScope(nil))
			o.confirmCompletions(o.tree.root)
			return nil, nil
		})
	})
	ext.OnEvent("tool_execution_end", func(ctx sdk.Context, data map[string]any) (any, error) {
		return state.observe(ctx, func(o *owner) (any, error) {
			o.observeToolEvent(data)
			return nil, nil
		})
	})
	ext.OnEvent("context", state.rootContext)
	ext.OnEvent("agent_end", func(ctx sdk.Context, _ map[string]any) (any, error) {
		return state.observe(ctx, func(o *owner) (any, error) {
			o.tree.root.markAgentEnded(true)
			return nil, nil
		})
	})
	ext.OnEvent("agent_before_settle", func(ctx sdk.Context, data map[string]any) (any, error) {
		return state.observe(ctx, func(o *owner) (any, error) {
			preview, _ := data["context"].(map[string]any)
			pending, _ := preview["pendingMessages"].([]any)
			if o.continueForCompletions(o.tree.root, pending) {
				return map[string]any{"continue": true}, nil
			}
			return nil, nil
		})
	})
	ext.OnEvent("agent_start", func(ctx sdk.Context, _ map[string]any) (any, error) {
		return state.observe(ctx, func(o *owner) (any, error) {
			o.watchRoot(ctx.Signal())
			o.startRecovery(nil)
			return nil, nil
		})
	})
	ext.OnEvent("agent_settled", func(ctx sdk.Context, _ map[string]any) (any, error) {
		return state.observe(ctx, func(o *owner) (any, error) {
			o.tree.root.markAgentEnded(false)
			o.settleRoot()
			o.retireScope(o.callScope(nil))
			o.retireReceipts(o.tree.root)
			return nil, nil
		})
	})
	ext.OnSessionStart(func(ctx sdk.Context, _ map[string]any) (any, error) {
		state.retire()
		state.mu.Lock()
		state.sessionContext = &ctx
		state.mu.Unlock()
		return nil, nil
	})
	ext.OnSessionShutdown(func(_ sdk.Context, _ map[string]any) (any, error) { state.retire(); return nil, nil })
	ext.Command("pig-litter", "Show Pig Litter child controls", state.openPanel)
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
