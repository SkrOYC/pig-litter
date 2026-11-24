package piglitter

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"
)

const memberControlLimit = 8 << 20
const memberControlPendingLimit = 32

type memberControlCommand struct {
	ID         string                `json:"id"`
	Kind       string                `json:"kind"`
	Completion *memberCompletionWire `json:"completion,omitempty"`
	Observer   *memberObserverWire   `json:"observer,omitempty"`
	History    *memberHistoryWire    `json:"history,omitempty"`
	Audit      *completionAudit      `json:"audit,omitempty"`
	Input      *memberInputWire      `json:"input,omitempty"`
}

type memberInputWire struct {
	Window   string `json:"window"`
	Transfer string `json:"transfer"`
	Mode     string `json:"mode"`
	Text     string `json:"text"`
	Cause    string `json:"cause,omitempty"`
}

type memberCompletionWire struct {
	Details completionDetails `json:"details"`
	Content string            `json:"content"`
	Mode    string            `json:"mode,omitempty"`
	Trigger bool              `json:"trigger"`
}

type memberObserverWire struct {
	Content  string                  `json:"content"`
	Activity *observerActivityMarker `json:"activity,omitempty"`
	Report   *observerMarker         `json:"report,omitempty"`
}

type memberHistoryWire struct {
	Entries int `json:"entries"`
	Bytes   int `json:"bytes"`
}

type memberSnapshotWire struct {
	Session ChildID           `json:"session"`
	Cwd     string            `json:"cwd"`
	Model   string            `json:"model"`
	Tools   []string          `json:"tools"`
	Phase   *memberPhase      `json:"phase"`
	Pending *int              `json:"pending"`
	Entries []json.RawMessage `json:"entries,omitempty"`
	Leaf    *string           `json:"leaf,omitempty"`
	Clipped bool              `json:"clipped,omitempty"`
}

type memberControlReply struct {
	ID          string                 `json:"id"`
	Kind        string                 `json:"kind"`
	Disposition memberInputDisposition `json:"disposition,omitempty"`
	Snapshot    *memberSnapshotWire    `json:"snapshot,omitempty"`
	Error       string                 `json:"error,omitempty"`
}

type memberControlCall struct {
	command    memberControlCommand
	ctx        context.Context
	reply      chan memberControlReply
	dispatched bool
}

type memberCauseKey struct{}

type memberCause struct {
	queue *memberControlQueue
	id    string
}

type memberControlQueue struct {
	capability [32]byte
	owner      *owner
	record     *record
	run        *run
	ownership  *memberOwnership
	owned      *memberOwnership
	binding    *memberBinding
	authority  *memberAuthority
	starting   *memberStartingHandoff
	window     string
	windowSeq  uint64
	windows    map[string]uint64
	running    map[string]*memberRunningInput
	inputAcks  map[string]memberControlReply
	mcpOnce    sync.Once
	mcpConfig  memberMCPConfig
	mcpErr     error
	mu         sync.Mutex
	pending    map[string]*memberControlCall
	commands   []*memberControlCall
	wake       chan struct{}
	turn       chan struct{}
	closed     chan struct{}
	cancelled  chan struct{}
	closeOnce  sync.Once
	cancelOnce sync.Once
}

func (o *owner) newMemberControlQueue(r *record, selected *run, ownership *memberOwnership) (*memberControlQueue, error) {
	var capability [32]byte
	if _, err := rand.Read(capability[:]); err != nil {
		return nil, err
	}
	return o.newMemberControlQueueWithCapability(r, selected, ownership, capability)
}

func (o *owner) newMemberControlQueueWithCapability(r *record, selected *run, ownership *memberOwnership, capability [32]byte) (*memberControlQueue, error) {
	if capability == ([32]byte{}) {
		return nil, reject("unavailable", "native control capability is invalid")
	}
	o.tree.mu.Lock()
	current, err := o.tree.currentLocked(selected)
	if err != nil || current != r || selected.ctx.Err() != nil || r.team == nil || ownership == nil || r.nativeOwnership != ownership || ownership.Pane == nil || !validMemberOwnership(ownership, r.team, r.cwd, r.model, selected.noticeID) || ownership.OwnerEpoch != o.epoch {
		o.tree.mu.Unlock()
		return nil, reject("unavailable", "native control has no matching launch")
	}
	queue := &memberControlQueue{capability: capability, owner: o, record: r, run: selected, ownership: ownership, owned: cloneMemberOwnership(ownership), pending: map[string]*memberControlCall{}, windows: map[string]uint64{}, running: map[string]*memberRunningInput{}, inputAcks: map[string]memberControlReply{}, wake: make(chan struct{}, 1), turn: make(chan struct{}, 1), closed: make(chan struct{}), cancelled: make(chan struct{})}
	queue.turn <- struct{}{}
	o.tree.mu.Unlock()
	context.AfterFunc(selected.ctx, queue.Close)
	return queue, nil
}

func (queue *memberControlQueue) valid() error {
	select {
	case <-queue.closed:
		return reject("unavailable", "native control is closed")
	case <-queue.cancelled:
		return reject("cancelled", "native control is cancelled")
	default:
	}
	if err := queue.owner.current(); err != nil {
		return err
	}
	queue.owner.tree.mu.Lock()
	defer queue.owner.tree.mu.Unlock()
	r := queue.record
	if queue.owner.tree.closed || queue.run.ctx.Err() != nil || queue.owner.tree.records[r.id] != r || r.run != queue.run || r.nativeOwnership != queue.ownership || !sameMemberOwnership(r.nativeOwnership, queue.owned) || r.team == nil || r.team.Ref != queue.owned.Member || r.team.AdmissionID != queue.owned.Admission || queue.run.noticeID != queue.owned.Execution || queue.owner.epoch != queue.owned.OwnerEpoch {
		return reject("unavailable", "native control execution changed")
	}
	queue.mu.Lock()
	binding := queue.binding
	queue.mu.Unlock()
	if binding == nil && r.native != nil || binding != nil && (r.native != binding || binding.control != queue || !queue.owner.memberBindingCurrentLocked(binding)) {
		return reject("unavailable", "native control handle changed")
	}
	return nil
}

func (queue *memberControlQueue) bound() bool {
	return queue.memberBound(false)
}

func (queue *memberControlQueue) ready() bool {
	return queue.memberBound(true)
}

func (queue *memberControlQueue) memberBound(requireReady bool) bool {
	queue.owner.tree.mu.Lock()
	defer queue.owner.tree.mu.Unlock()
	queue.mu.Lock()
	binding := queue.binding
	queue.mu.Unlock()
	return binding != nil && (!requireReady || binding.ready) && queue.record.native == binding && binding.control == queue && queue.owner.memberBindingCurrentLocked(binding)
}

func (queue *memberControlQueue) bind(binding *memberBinding) error {
	queue.mu.Lock()
	defer queue.mu.Unlock()
	if queue.binding != nil || binding.record != queue.record || binding.run != queue.run || binding.ownership != queue.ownership || binding.control != queue {
		return reject("unavailable", "native control binding changed")
	}
	select {
	case <-queue.closed:
		return reject("unavailable", "native control is closed")
	case <-queue.cancelled:
		return reject("cancelled", "native control is cancelled")
	default:
	}
	queue.binding = binding
	return nil
}

func (queue *memberControlQueue) token() string { return hex.EncodeToString(queue.capability[:]) }

func (queue *memberControlQueue) authorize(r *http.Request) bool {
	value, err := hex.DecodeString(r.Header.Get("X-Pig-Litter-Member"))
	return err == nil && len(value) == len(queue.capability) && subtle.ConstantTimeCompare(value, queue.capability[:]) == 1
}

func (queue *memberControlQueue) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !queue.authorize(r) {
		http.Error(w, "member authorization failed", http.StatusForbidden)
		return
	}
	if cause := r.Header.Get("X-Pig-Litter-Cause"); cause != "" {
		if len(cause) > 128 {
			http.Error(w, "invalid member callback identity", http.StatusBadRequest)
			return
		}
		r = r.WithContext(context.WithValue(r.Context(), memberCauseKey{}, memberCause{queue: queue, id: cause}))
	}
	if (r.URL.Path == "/control/watch" || r.URL.Path == "/control/next") && r.ContentLength == 0 {
		select {
		case <-queue.cancelled:
			if r.URL.Path == "/control/watch" {
				queue.watch(w, r)
			} else {
				queue.next(w, r)
			}
			return
		case <-queue.closed:
			if r.URL.Path == "/control/watch" {
				queue.watch(w, r)
			} else {
				queue.next(w, r)
			}
			return
		default:
		}
	}
	if err := queue.valid(); err != nil {
		http.Error(w, "member execution is unavailable", http.StatusGone)
		return
	}
	switch r.URL.Path {
	case "/control/next", "/control/watch":
		if r.ContentLength != 0 {
			http.Error(w, "unexpected member control body", http.StatusBadRequest)
			return
		}
		if r.URL.Path == "/control/watch" {
			queue.watch(w, r)
		} else {
			queue.next(w, r)
		}
	case "/control/result":
		queue.result(w, r)
	case "/member/mcp/config":
		queue.serveMCPConfig(w, r)
	case "/input/start", "/input/installed", "/input/ready", "/input/result", "/input/window", "/input/running-result", "/input/cancel-watch":
		queue.serveStartingInput(w, r)
	default:
		if strings.HasPrefix(r.URL.Path, "/member/") {
			queue.serveAuthority(w, r)
		} else {
			http.NotFound(w, r)
		}
	}
}

func (queue *memberControlQueue) watch(w http.ResponseWriter, r *http.Request) {
	select {
	case <-queue.cancelled:
		_ = json.NewEncoder(w).Encode(memberControlCommand{Kind: "cancel"})
	case <-queue.closed:
		http.Error(w, "member control closed", http.StatusGone)
	case <-r.Context().Done():
	}
}

func (queue *memberControlQueue) next(w http.ResponseWriter, r *http.Request) {
	for {
		select {
		case <-queue.closed:
			http.Error(w, "member control closed", http.StatusGone)
			return
		case <-queue.cancelled:
			_ = json.NewEncoder(w).Encode(memberControlCommand{Kind: "cancel"})
			return
		case <-r.Context().Done():
			return
		case <-queue.turn:
		}
		for {
			queue.mu.Lock()
			if r.Context().Err() != nil {
				queue.mu.Unlock()
				queue.turn <- struct{}{}
				return
			}
			stopped := false
			select {
			case <-queue.closed:
				stopped = true
			case <-queue.cancelled:
				stopped = true
			default:
			}
			var call *memberControlCall
			for !stopped && len(queue.commands) != 0 {
				candidate := queue.commands[0]
				queue.commands[0] = nil
				queue.commands = queue.commands[1:]
				if queue.pending[candidate.command.ID] == candidate && candidate.ctx.Err() == nil {
					call = candidate
					call.dispatched = true
					break
				}
				if queue.pending[candidate.command.ID] == candidate {
					delete(queue.pending, candidate.command.ID)
				}
			}
			queue.mu.Unlock()
			if call != nil {
				if err := queue.valid(); err != nil {
					queue.Close()
					http.Error(w, "member execution is unavailable", http.StatusGone)
					return
				}
				_ = json.NewEncoder(w).Encode(call.command)
				return
			}
			if stopped {
				queue.turn <- struct{}{}
				select {
				case <-queue.cancelled:
					_ = json.NewEncoder(w).Encode(memberControlCommand{Kind: "cancel"})
				default:
					http.Error(w, "member control closed", http.StatusGone)
				}
				return
			}
			select {
			case <-queue.closed:
			case <-queue.cancelled:
			case <-queue.wake:
			case <-r.Context().Done():
				queue.turn <- struct{}{}
				return
			}
		}
	}
}

func decodeMemberControl(w http.ResponseWriter, r *http.Request, value any) error {
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(5 * time.Second))
	defer func() { _ = controller.SetReadDeadline(time.Time{}) }()
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, memberControlLimit))
	if err != nil || checkUniqueJSON(data) != nil {
		return errors.New("invalid member control request")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return errors.New("member control request has trailing data")
	}
	return nil
}

func (queue *memberControlQueue) result(w http.ResponseWriter, r *http.Request) {
	var reply memberControlReply
	if err := decodeMemberControl(w, r, &reply); err != nil || len(reply.Error) > 4096 {
		http.Error(w, "invalid member control result", http.StatusBadRequest)
		return
	}
	queue.mu.Lock()
	call := queue.pending[reply.ID]
	stopped := false
	select {
	case <-queue.closed:
		stopped = true
	case <-queue.cancelled:
		stopped = true
	default:
	}
	if !stopped && call == nil && reply.Kind == "input" {
		previous, ok := queue.inputAcks[reply.ID]
		if ok && previous.Kind == reply.Kind && previous.Error == reply.Error && previous.Disposition == reply.Disposition && previous.Snapshot == nil && reply.Snapshot == nil {
			queue.mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	if stopped || call == nil || !call.dispatched || call.command.Kind != reply.Kind {
		queue.mu.Unlock()
		http.Error(w, "stale member control result", http.StatusConflict)
		return
	}
	valid := reply.Error != "" && reply.Disposition == 0 && reply.Snapshot == nil
	if reply.Error == "" {
		switch reply.Kind {
		case "completion", "observer":
			valid = reply.Snapshot == nil && (reply.Disposition == memberInputQueued || reply.Disposition == memberInputHandled || reply.Disposition == memberInputUncertain)
		case "snapshot":
			valid = reply.Snapshot != nil && reply.Disposition == 0
		case "audit":
			valid = reply.Snapshot == nil && reply.Disposition == 0
		case "input":
			valid = reply.Snapshot == nil && reply.Disposition == 0
		}
	}
	if !valid {
		queue.mu.Unlock()
		http.Error(w, "invalid member control result", http.StatusBadRequest)
		return
	}
	delete(queue.pending, reply.ID)
	if reply.Kind == "input" {
		queue.inputAcks[reply.ID] = reply
	}
	queue.turn <- struct{}{}
	queue.mu.Unlock()
	call.reply <- reply
	w.WriteHeader(http.StatusNoContent)
}

func (queue *memberControlQueue) request(ctx context.Context, command memberControlCommand) (memberControlReply, error) {
	reply, _, err := queue.requestCommand(ctx, command, false)
	return reply, err
}

func (queue *memberControlQueue) requestCommand(ctx context.Context, command memberControlCommand, retain bool) (memberControlReply, bool, error) {
	var reply memberControlReply
	if err := queue.valid(); err != nil {
		return reply, false, err
	}
	if err := ctx.Err(); err != nil {
		return reply, false, err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return reply, false, err
	}
	command.ID = hex.EncodeToString(id[:])
	data, err := json.Marshal(command)
	if err != nil || len(data) > memberControlLimit {
		return reply, false, reject("rejected", "member control request exceeds its bound")
	}
	call := &memberControlCall{command: command, ctx: ctx, reply: make(chan memberControlReply, 1)}
	queue.mu.Lock()
	select {
	case <-queue.closed:
		queue.mu.Unlock()
		return reply, false, reject("unavailable", "member control is closed")
	case <-queue.cancelled:
		queue.mu.Unlock()
		return reply, false, reject("cancelled", "member control is cancelled")
	default:
	}
	if len(queue.pending) >= memberControlPendingLimit {
		queue.mu.Unlock()
		return reply, false, reject("unavailable", "member control request bound reached")
	}
	queue.pending[command.ID] = call
	queue.commands = append(queue.commands, call)
	select {
	case queue.wake <- struct{}{}:
	default:
	}
	queue.mu.Unlock()
	select {
	case reply = <-call.reply:
		if err := queue.valid(); err != nil {
			return reply, true, err
		}
		if reply.Error != "" {
			return reply, true, reject("unavailable", "native member rejected the control request")
		}
		return reply, true, nil
	case <-ctx.Done():
	case <-queue.closed:
	case <-queue.cancelled:
	}
	queue.mu.Lock()
	uncertain := call.dispatched
	if queue.pending[command.ID] == call {
		if !call.dispatched || !retain {
			delete(queue.pending, command.ID)
		}
		if !call.dispatched {
			queue.commands = slices.DeleteFunc(queue.commands, func(candidate *memberControlCall) bool { return candidate == call })
		}
	}
	queue.mu.Unlock()
	if uncertain && !retain {
		select {
		case <-queue.cancelled:
		default:
			queue.Close()
		}
	}
	return reply, uncertain, reject("unavailable", "member control result is uncertain")
}

func (queue *memberControlQueue) Queue(ctx context.Context, input memberInput) (memberInputDisposition, error) {
	if !queue.ready() {
		return memberInputUncertain, reject("unavailable", "native control handle is not installed")
	}
	if text, ok := input.(memberTextInput); ok {
		return queue.queueText(ctx, text)
	}
	var command memberControlCommand
	switch value := input.(type) {
	case memberCompletionInput:
		command = memberControlCommand{Kind: "completion", Completion: &memberCompletionWire{Details: value.details, Content: value.content, Mode: value.mode, Trigger: value.trigger}}
	case memberObserverInput:
		command = memberControlCommand{Kind: "observer", Observer: &memberObserverWire{Content: value.content, Activity: value.activity, Report: value.report}}
	default:
		return memberInputUncertain, reject("unavailable", "native input is unavailable")
	}
	reply, err := queue.request(ctx, command)
	if err != nil {
		return memberInputUncertain, err
	}
	if reply.Disposition != memberInputQueued && reply.Disposition != memberInputHandled && reply.Disposition != memberInputUncertain {
		return memberInputUncertain, reject("unavailable", "member returned an invalid input disposition")
	}
	return reply.Disposition, nil
}

func (queue *memberControlQueue) Snapshot(ctx context.Context, bound memberHistoryBound) (memberSnapshot, error) {
	if bound.entries < 0 || bound.entries > 200 || bound.bytes < 0 || bound.bytes > 64*1024 || (bound.entries == 0) != (bound.bytes == 0) {
		return memberSnapshot{}, reject("rejected", "invalid member history bound")
	}
	reply, err := queue.request(ctx, memberControlCommand{Kind: "snapshot", History: &memberHistoryWire{Entries: bound.entries, Bytes: bound.bytes}})
	if err != nil {
		return memberSnapshot{}, err
	}
	if reply.Snapshot == nil || reply.Snapshot.Phase == nil || *reply.Snapshot.Phase < memberStarting || *reply.Snapshot.Phase > memberClosed || reply.Snapshot.Pending == nil || *reply.Snapshot.Pending < 0 || *reply.Snapshot.Pending > 1024 || len(reply.Snapshot.Entries) > bound.entries {
		return memberSnapshot{}, reject("unavailable", "member returned an invalid snapshot")
	}
	bytes := 0
	for _, entry := range reply.Snapshot.Entries {
		bytes += len(entry)
		if bytes > 128*1024 {
			return memberSnapshot{}, reject("unavailable", "member snapshot exceeds its response bound")
		}
	}
	value := reply.Snapshot
	return memberSnapshot{session: value.Session, cwd: value.Cwd, model: value.Model, tools: value.Tools, phase: *value.Phase, pending: *value.Pending, entries: value.Entries, leaf: value.Leaf, clipped: value.Clipped}, nil
}

func (queue *memberControlQueue) RecordCompletion(ctx context.Context, audit completionAudit) error {
	if !queue.ready() {
		return reject("unavailable", "native control handle is not installed")
	}
	command := memberControlCommand{Kind: "audit", Audit: &audit}
	_, err := queue.request(ctx, command)
	return err
}

func (queue *memberControlQueue) Cancel() {
	queue.cancelOnce.Do(func() {
		queue.mu.Lock()
		close(queue.cancelled)
		queue.mu.Unlock()
		queue.failStarting()
		queue.failQueuedInputs()
	})
}

func (queue *memberControlQueue) Focus(ctx context.Context) error {
	if err := queue.valid(); err != nil {
		return err
	}
	if !queue.bound() {
		return reject("unavailable", "native control handle is not installed")
	}
	if queue.owned.Backend == backendITerm {
		server := itermServerFromIdentity(queue.owned.Server, queue.owned.Executable).iterm
		if err := server.verifyPane(ctx, *queue.owned.Pane); err != nil {
			return err
		}
		_, err := itermCall(ctx, itermSelect, server.window, queue.owned.Pane.ID, queue.owned.Pane.TTY)
		return err
	}
	server, err := tmuxFromEnvironment(ctx, fmt.Sprintf("%s,%d,0", queue.owned.Server.Socket, queue.owned.Server.PID), queue.owned.Server.Target)
	if err != nil || server.birth != queue.owned.Server.Birth {
		return reject("unavailable", "native pane server is unavailable")
	}
	if err := verifyMemberPane(ctx, server, *queue.owned.Pane); err != nil {
		return err
	}
	_, err = server.call(ctx, "select-pane", "-t", queue.owned.Pane.ID)
	return err
}

func (queue *memberControlQueue) Close() {
	queue.closeOnce.Do(func() {
		queue.mu.Lock()
		close(queue.closed)
		queue.mu.Unlock()
		queue.failStarting()
		queue.failQueuedInputs()
	})
}
