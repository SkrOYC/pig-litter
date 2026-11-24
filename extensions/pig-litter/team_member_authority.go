package piglitter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
)

type memberAuthority struct {
	owner     *owner
	binding   *memberBinding
	gate      *teamAdmissionGate
	mu        sync.Mutex
	turn      uint64
	active    bool
	approved  map[string]memberAuthorizedCall
	responses map[string]string
	first     *agent.AssistantMessage
	turnCtx   context.Context
	cancel    context.CancelFunc
}

type memberAuthorizedCall struct {
	name     string
	digest   [32]byte
	decision memberToolDecision
	ready    bool
	consumed bool
	finished bool
	response string
}

type memberTurnRequest struct {
	ID uint64 `json:"id"`
}

type memberResponseRequest struct {
	Turn    uint64 `json:"turn"`
	EntryID string `json:"entryId"`
}

type memberToolRequest struct {
	Turn      uint64          `json:"turn"`
	ID        string          `json:"id"`
	Parent    string          `json:"parent,omitempty"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type memberToolDecision struct {
	Block     bool   `json:"block"`
	Reason    string `json:"reason,omitempty"`
	Terminate bool   `json:"terminate,omitempty"`
}

type memberOperationRequest struct {
	Turn   uint64          `json:"turn"`
	CallID string          `json:"callId"`
	Leaf   string          `json:"leaf,omitempty"`
	Args   json.RawMessage `json:"args"`
}

type memberOperationReply struct {
	Content string `json:"content"`
	IsError bool   `json:"isError"`
}

type memberResultBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type memberToolResult struct {
	Turn    uint64              `json:"turn"`
	CallID  string              `json:"callId"`
	Name    string              `json:"name"`
	IsError bool                `json:"isError"`
	Content []memberResultBlock `json:"content"`
}

type memberSettleRequest struct {
	Boundary   uint64 `json:"boundary"`
	Report     string `json:"report"`
	Completed  bool   `json:"completed"`
	Continuing bool   `json:"continuing"`
}

type memberSettleReply struct {
	Boundary uint64 `json:"boundary"`
	Allow    bool   `json:"allow"`
	Continue bool   `json:"continue"`
	Feedback string `json:"feedback,omitempty"`
}

type memberFinalizationRequest struct {
	Boundary uint64 `json:"boundary"`
	Aborted  bool   `json:"aborted"`
}

func (queue *memberControlQueue) attachAuthority(binding *memberBinding) error {
	queue.mu.Lock()
	current := queue.binding
	queue.mu.Unlock()
	if !queue.bound() || binding != current {
		return reject("unavailable", "native member handle is not current")
	}
	store, err := queue.owner.teamStoreForRoot()
	if err != nil {
		return err
	}
	team, roster, pending, err := store.SnapshotWithReservations()
	if err != nil || team != binding.member.Team {
		return reject("unavailable", "native member roster is unavailable")
	}
	committed, reserved := false, false
	for _, member := range roster {
		if member.Ref == binding.member && member.AdmissionID == binding.admission {
			committed = true
		}
	}
	for _, member := range pending {
		if member.Ref == binding.member && member.AdmissionID == binding.admission {
			reserved = true
		}
	}
	if committed == reserved {
		return reject("unavailable", "native member admission is unavailable or ambiguous")
	}
	if !queue.bound() {
		return reject("unavailable", "native member handle changed during admission")
	}
	authority := &memberAuthority{owner: queue.owner, binding: binding, gate: &teamAdmissionGate{owner: queue.owner, record: binding.record, run: binding.run, committed: committed}}
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.authority != nil || queue.binding != binding {
		return reject("unavailable", "native member authority is already bound")
	}
	queue.authority = authority
	return nil
}

func (queue *memberControlQueue) serveAuthority(w http.ResponseWriter, r *http.Request) {
	queue.mu.Lock()
	authority := queue.authority
	queue.mu.Unlock()
	if authority == nil || !queue.ready() {
		http.Error(w, "native member authority is unavailable", http.StatusGone)
		return
	}
	write := func(value any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}
	fail := func() { http.Error(w, "invalid native member request", http.StatusBadRequest) }
	switch r.URL.Path {
	case "/member/turn/start", "/member/turn/end":
		var request memberTurnRequest
		if decodeMemberControl(w, r, &request) != nil {
			fail()
			return
		}
		var err error
		if r.URL.Path == "/member/turn/start" {
			err = authority.startTurn(request.ID)
		} else {
			err = authority.endTurn(request.ID)
		}
		if err != nil {
			http.Error(w, "native turn is unavailable", http.StatusConflict)
			return
		}
		write(map[string]bool{"ok": true})
	case "/member/response":
		var request memberResponseRequest
		if decodeMemberControl(w, r, &request) != nil || authority.observeResponse(r.Context(), request) != nil {
			fail()
			return
		}
		write(map[string]bool{"ok": true})
	case "/member/tool":
		var request memberToolRequest
		if decodeMemberControl(w, r, &request) != nil {
			fail()
			return
		}
		write(authority.authorize(r.Context(), request))
	case "/member/result":
		var request memberToolResult
		if decodeMemberControl(w, r, &request) != nil || authority.finishCall(request) != nil {
			fail()
			return
		}
		write(map[string]bool{"ok": true})
	case "/member/settlement/decide":
		var request memberSettleRequest
		if decodeMemberControl(w, r, &request) != nil {
			fail()
			return
		}
		snapshot, err := authority.owner.memberLogSnapshot(r.Context(), authority.binding)
		if err != nil {
			http.Error(w, "native settlement history is unavailable", http.StatusConflict)
			return
		}
		_, terminal, err := memberAssistantMessages(snapshot, authority.binding.log.baselineCount)
		if err != nil || request.Report != idleAssistantReport(terminal) {
			http.Error(w, "native settlement report does not match its history", http.StatusConflict)
			return
		}
		decision, err := authority.owner.decideMemberSettlement(r.Context(), authority.binding, memberSettlementCandidate{boundary: request.Boundary, report: request.Report, completed: request.Completed, continuing: request.Continuing})
		if err != nil {
			write(memberSettleReply{Boundary: request.Boundary, Feedback: "The current team work could not be checked."})
			return
		}
		write(memberSettleReply{Boundary: decision.boundary, Allow: decision.allow, Continue: decision.continueRun, Feedback: decision.feedback})
	case "/member/settlement/finalize":
		var request memberFinalizationRequest
		if decodeMemberControl(w, r, &request) != nil {
			fail()
			return
		}
		decision, err := authority.finalizeSettlement(r.Context(), request)
		if err != nil {
			http.Error(w, "native settlement could not be committed", http.StatusConflict)
			return
		}
		write(memberSettleReply{Boundary: decision.boundary, Allow: decision.allow, Continue: decision.continueRun, Feedback: decision.feedback})
	default:
		operation := memberOperationRoute(r.URL.Path)
		if operation == "" {
			http.NotFound(w, r)
			return
		}
		var request memberOperationRequest
		if decodeMemberControl(w, r, &request) != nil {
			fail()
			return
		}
		reply, err := authority.operation(r.Context(), operation, request)
		if err != nil {
			fail()
			return
		}
		write(reply)
	}
}

func (authority *memberAuthority) finalizeSettlement(ctx context.Context, request memberFinalizationRequest) (memberSettlementDecision, error) {
	binding, o := authority.binding, authority.owner
	if request.Aborted {
		return o.finalizeMemberSettlement(binding, request.Boundary, true, nil)
	}
	evidence, err := o.memberOutcome(ctx, binding)
	if err != nil {
		return memberSettlementDecision{}, err
	}
	if evidence.outcome.State != Completed && evidence.outcome.State != Partial {
		return memberSettlementDecision{}, reject("unavailable", "native assistant outcome cannot settle")
	}
	binding.settleMu.Lock()
	pending := binding.settlement
	matches := pending != nil && pending.boundary == request.Boundary && pending.candidate.Report == evidence.report
	binding.settleMu.Unlock()
	if !matches {
		return memberSettlementDecision{}, reject("unavailable", "native outcome does not match its approved settlement")
	}
	authority.gate.observe(evidence.first)
	outcome := evidence.outcome
	store, err := authority.gate.prepareFinish(ctx, outcome)
	if err != nil {
		return memberSettlementDecision{}, err
	}
	begun := false
	decision, err := o.finalizeMemberSettlement(binding, request.Boundary, false, func() error {
		snapshot, err := readMemberLog(ctx, binding.log)
		if err != nil || len(snapshot.entries) != evidence.count || memberEntriesDigest(snapshot.entries) != evidence.digest {
			return reject("unavailable", "native history changed before settlement commit")
		}
		authority.gate.finishPrepared(&outcome, store)
		authority.gate.mu.Lock()
		committed := authority.gate.committed
		authority.gate.mu.Unlock()
		if !committed {
			outcome.State, outcome.Error = Unavailable, "teammate admission was not committed"
		}
		if err := o.tree.beginFinishLocked(binding.record, binding.run, outcome); err != nil {
			return err
		}
		begun = true
		return nil
	})
	if begun {
		after, settled, finishErr := o.tree.finishBegun(binding.record, binding.run, false)
		if after != nil {
			after()
		}
		if finishErr != nil {
			decision.allow = false
			return decision, finishErr
		}
		if settled.State != Completed && settled.State != Partial {
			decision.allow = false
			return decision, reject("unavailable", "native outcome did not settle")
		}
	}
	if err == nil && decision.allow {
		o.tree.mu.Lock()
		teamErr := binding.run.teamFinishErr
		active := o.memberBindingCurrentLocked(binding)
		o.tree.mu.Unlock()
		if !active {
			decision.allow = false
			return decision, reject("unavailable", "native owner retired during settlement")
		}
		if teamErr != nil {
			decision.allow = false
			return decision, fmt.Errorf("native team settlement did not persist: %w", teamErr)
		}
	}
	return decision, err
}

func memberOperationRoute(path string) string {
	switch path {
	case "/member/child/spawn":
		return "litter_spawn"
	case "/member/child/list":
		return "litter_list"
	case "/member/child/inspect":
		return "litter_inspect"
	case "/member/child/message":
		return "litter_message"
	case "/member/child/stop":
		return "litter_stop"
	case "/member/child/wait":
		return "litter_wait"
	case "/member/team/list":
		return "team_list"
	case "/member/team/send":
		return "team_send"
	case "/member/team/plan":
		return "team_plan"
	case "/member/team/shutdown":
		return "team_shutdown"
	case "/member/team/task/create":
		return "team_task_create"
	case "/member/team/task/get":
		return "team_task_get"
	case "/member/team/task/list":
		return "team_task_list"
	case "/member/team/task/update":
		return "team_task_update"
	default:
		return ""
	}
}

func (authority *memberAuthority) startTurn(id uint64) error {
	authority.mu.Lock()
	if id == 0 || id <= authority.turn || authority.active {
		authority.mu.Unlock()
		return reject("unavailable", "native turn is stale")
	}
	o, binding := authority.owner, authority.binding
	o.tree.mu.Lock()
	if !o.memberBindingCurrentLocked(binding) || binding.run.ctx.Err() != nil || binding.record.paused {
		o.tree.mu.Unlock()
		authority.mu.Unlock()
		return reject("unavailable", "native execution is unavailable")
	}
	binding.run.mu.Lock()
	previous := binding.run.calls
	binding.run.calls = newCallScope()
	binding.run.planDenials = nil
	binding.run.mu.Unlock()
	binding.run.progress.turns++
	if binding.run.state == Starting {
		binding.run.state = Running
	}
	o.tree.mu.Unlock()
	authority.turn, authority.active = id, true
	authority.approved = map[string]memberAuthorizedCall{}
	authority.responses = map[string]string{}
	authority.turnCtx, authority.cancel = context.WithCancel(binding.run.ctx)
	authority.mu.Unlock()
	o.retireScope(previous)
	o.widget()
	return nil
}

func (authority *memberAuthority) endTurn(id uint64) error {
	authority.mu.Lock()
	if id == 0 || id != authority.turn || !authority.active {
		authority.mu.Unlock()
		return reject("unavailable", "native turn is stale")
	}
	authority.active = false
	authority.approved = nil
	authority.responses = nil
	cancel := authority.cancel
	scope := authority.owner.callScope(authority.binding.run)
	if scope != nil {
		scope.mu.Lock()
		scope.closed = true
		scope.mu.Unlock()
	}
	authority.cancel = nil
	authority.turnCtx = nil
	authority.mu.Unlock()
	cancel()
	o, binding := authority.owner, authority.binding
	o.tree.mu.Lock()
	current := o.memberBindingCurrentLocked(binding)
	o.tree.mu.Unlock()
	if !current {
		return reject("unavailable", "native execution is unavailable")
	}
	o.retireScope(scope)
	o.tree.mu.Lock()
	if o.memberBindingCurrentLocked(binding) {
		o.tree.teamWG.Add(1)
		go func() {
			defer o.tree.teamWG.Done()
			o.confirmCompletions(binding.record)
		}()
	}
	o.tree.mu.Unlock()
	return nil
}

func (authority *memberAuthority) observeResponse(ctx context.Context, request memberResponseRequest) error {
	authority.mu.Lock()
	active := authority.active && request.Turn == authority.turn
	authority.mu.Unlock()
	if !active {
		return reject("unavailable", "native turn is not active")
	}
	if request.EntryID == "" || len(request.EntryID) > 128 {
		return reject("rejected", "invalid native response entry")
	}
	entries, err := authority.owner.memberLogEntries(ctx, authority.binding)
	if err != nil {
		return err
	}
	var first *agent.AssistantMessage
	var observed *agent.AssistantMessage
	found := false
	for _, raw := range entries[authority.binding.log.baselineCount:] {
		var entry struct {
			ID      string `json:"id"`
			Type    string `json:"type"`
			Message struct {
				Role       string `json:"role"`
				StopReason string `json:"stopReason"`
				Content    []struct {
					Type string `json:"type"`
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Type != "message" || entry.Message.Role != "assistant" {
			continue
		}
		if len(entry.Message.Content) > 1024 {
			return reject("unavailable", "native response entry is invalid")
		}
		message := &agent.AssistantMessage{StopReason: ai.StopReason(entry.Message.StopReason)}
		for _, block := range entry.Message.Content {
			if block.Type == "toolCall" {
				if block.ID == "" || len(block.ID) > 256 || block.Name == "" || len(block.Name) > 256 {
					return reject("unavailable", "native response tool call is invalid")
				}
				message.Content = append(message.Content, ai.ToolCall{ID: block.ID, Name: block.Name})
			}
		}
		if first == nil {
			first = message
		}
		if entry.ID == request.EntryID {
			observed = message
			found = true
			break
		}
	}
	if !found {
		return reject("unavailable", "native response entry was not observed in this execution")
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if !authority.active || authority.turn != request.Turn {
		return reject("unavailable", "native turn changed during response observation")
	}
	if authority.first == nil {
		authority.first = first
	}
	for _, block := range observed.Content {
		if call, ok := block.(ai.ToolCall); ok {
			authority.responses[call.ID] = request.EntryID
		}
	}
	return nil
}

func (authority *memberAuthority) authorize(ctx context.Context, request memberToolRequest) memberToolDecision {
	denied := func(reason string) memberToolDecision { return memberToolDecision{Block: true, Reason: reason} }
	if request.ID == "" || len(request.ID) > 256 || request.Name == "" || len(request.Name) > 256 || len(request.Parent) > 256 || request.Parent != "" && !strings.HasPrefix(request.ID, request.Parent+"/") || len(request.Arguments) > 1024*1024 || !json.Valid(request.Arguments) {
		return denied("invalid native tool request")
	}
	if !slices.Contains(authority.binding.owned.Tools, request.Name) {
		return denied("tool is outside the teammate ceiling")
	}
	digest, err := memberArgumentsDigest(request.Arguments)
	if err != nil {
		return denied("invalid native tool arguments")
	}
	authority.mu.Lock()
	if !authority.active || request.Turn != authority.turn {
		authority.mu.Unlock()
		return denied("native turn is not active")
	}
	turn := authority.turn
	if previous, ok := authority.approved[request.ID]; ok {
		authority.mu.Unlock()
		if previous.name == request.Name && previous.digest == digest && previous.ready && !previous.consumed {
			return previous.decision
		}
		return denied("native tool call identity was reused or is still being checked")
	}
	if len(authority.approved) >= 1024 {
		authority.mu.Unlock()
		return denied("native tool call bound reached")
	}
	response := authority.responses[request.ID]
	if request.Parent != "" {
		parent, ok := authority.approved[request.Parent]
		if !ok || !parent.ready || parent.decision.Block {
			authority.mu.Unlock()
			return denied("native parent tool call is unavailable")
		}
		response = parent.response
	}
	authority.approved[request.ID] = memberAuthorizedCall{name: request.Name, digest: digest, response: response}
	first := authority.first
	authority.mu.Unlock()
	decision := memberToolDecision{}
	planDenied := false
	authority.gate.observe(first)
	if result := authority.gate.before(ctx, request.ID, request.Name, request.Arguments); result.Block {
		decision = memberToolDecision{Block: true, Reason: result.Reason, Terminate: result.Terminate}
	} else if result := authority.owner.beforePlanTool(authority.binding.record, authority.binding.run, request.ID, request.Name, request.Arguments); result.Block {
		decision = memberToolDecision{Block: true, Reason: result.Reason, Terminate: result.Terminate}
		authority.binding.run.mu.Lock()
		planDenied = authority.binding.run.planDenials[planDenial{ID: request.ID, Name: request.Name}]
		authority.binding.run.mu.Unlock()
	} else if result := authority.owner.shutdownHook(authority.binding.record, authority.binding.run)(ctx, request.ID, request.Name, request.Arguments); result.Block {
		decision = memberToolDecision{Block: true, Reason: result.Reason, Terminate: result.Terminate}
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if !authority.active || authority.turn != turn {
		return denied("native turn changed during authorization")
	}
	if planDenied && response != "" {
		authority.binding.denialMu.Lock()
		if authority.binding.denials == nil {
			authority.binding.denials = map[memberPlanDenial]string{}
		}
		authority.binding.denials[memberPlanDenial{response: response, call: planDenial{ID: request.ID, Name: request.Name}}] = decision.Reason
		authority.binding.denialMu.Unlock()
	}
	authority.approved[request.ID] = memberAuthorizedCall{name: request.Name, digest: digest, decision: decision, ready: true, response: response}
	return decision
}

func memberArgumentsDigest(raw json.RawMessage) ([32]byte, error) {
	var digest [32]byte
	if checkUniqueJSON(raw) != nil {
		return digest, errors.New("invalid operation arguments")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return digest, err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return digest, err
	}
	return sha256.Sum256(data), nil
}

func (authority *memberAuthority) operation(ctx context.Context, operation string, request memberOperationRequest) (memberOperationReply, error) {
	var reply memberOperationReply
	if request.CallID == "" || len(request.CallID) > 256 || len(request.Leaf) > 128 || checkUniqueJSON(request.Args) != nil {
		return reply, reject("rejected", "invalid native member operation")
	}
	schema, err := json.Marshal(schemas[operation])
	if err != nil {
		return reply, err
	}
	args, err := ai.ValidateToolArgumentsJSON(operation, schema, request.Args)
	if err != nil {
		return reply, reject("rejected", "invalid native member operation arguments")
	}
	digest, err := memberArgumentsDigest(request.Args)
	if err != nil {
		return reply, err
	}
	authority.mu.Lock()
	approved, ok := authority.approved[request.CallID]
	if !authority.active || request.Turn != authority.turn || !ok || !approved.ready || approved.decision.Block || approved.consumed || approved.name != operation || approved.digest != digest {
		authority.mu.Unlock()
		return reply, reject("permission_denied", "native operation has no matching tool authorization")
	}
	approved.consumed = true
	authority.approved[request.CallID] = approved
	turnCtx := authority.turnCtx
	scope := authority.owner.callScope(authority.binding.run)
	authority.mu.Unlock()
	ticket, err := authority.owner.memberCallTicket(authority.binding, scope, request.CallID, operation, request.Leaf)
	if err != nil {
		return reply, err
	}
	if operation == "litter_spawn" {
		ctx = turnCtx
	} else {
		merged, cancel := context.WithCancel(ctx)
		stop := context.AfterFunc(turnCtx, cancel)
		defer func() { stop(); cancel() }()
		ctx = merged
	}
	value, operationErr := authority.owner.operation(ctx, args, ticket, nil)
	reply.Content, reply.IsError = resultContent(operation, value, operationErr)
	return reply, nil
}

func (authority *memberAuthority) finishCall(request memberToolResult) error {
	if request.CallID == "" || len(request.CallID) > 256 || request.Name == "" || len(request.Name) > 256 || len(request.Content) > 128 {
		return reject("rejected", "invalid native tool result")
	}
	result := agent.AgentToolResult{IsError: request.IsError}
	total := 0
	for _, block := range request.Content {
		total += len(block.Text)
		if block.Type != "text" || total > 1024*1024 {
			return errors.New("native tool result content is invalid")
		}
		result.Content = append(result.Content, ai.TextContent{Text: block.Text})
	}
	authority.mu.Lock()
	approved, ok := authority.approved[request.CallID]
	if !authority.active || request.Turn != authority.turn || !ok || approved.name != request.Name || !approved.consumed || approved.finished {
		authority.mu.Unlock()
		return reject("unavailable", "native tool result is not current")
	}
	authority.owner.tree.mu.Lock()
	current := authority.owner.memberBindingCurrentLocked(authority.binding)
	authority.owner.tree.mu.Unlock()
	if !current {
		authority.mu.Unlock()
		return reject("unavailable", "native execution is unavailable")
	}
	scope := authority.owner.callScope(authority.binding.run)
	if scope == nil {
		authority.mu.Unlock()
		return reject("unavailable", "native tool call has no active turn")
	}
	scope.mu.Lock()
	ticket := scope.calls[request.CallID]
	if scope.closed || ticket == nil || ticket.operation != request.Name {
		scope.mu.Unlock()
		authority.mu.Unlock()
		return reject("unavailable", "native tool call is not current")
	}
	delete(scope.calls, request.CallID)
	scope.mu.Unlock()
	approved.finished = true
	authority.approved[request.CallID] = approved
	authority.mu.Unlock()
	authority.owner.finishCall(ticket, result, request.IsError)
	return nil
}
