package piglitter

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
)

type reservation struct {
	parent          ChildID
	generation      int
	sent            bool
	outcomeObserved bool
}

type Tree struct {
	mu         sync.Mutex
	config     Config
	ctx        context.Context
	cancel     context.CancelFunc
	identity   string
	sequence   int
	records    map[ChildID]*record
	order      []ChildID
	names      map[string]ChildID
	pending    map[Ref]*reservation
	live       int
	closed     bool
	closedDone chan struct{}
	execute    func(*record, *run) Outcome
	completion func(*record, Outcome)
	changed    func()
}

type admission struct {
	parent                       ChildID
	parentGeneration             int
	kind, name, model, cwd, task string
	agent                        AgentDefinition
	history                      *coding.SessionManager
	authority                    *authority
}

func newTree(config Config) (*Tree, error) {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Tree{config: config, identity: hex.EncodeToString(id[:]), ctx: ctx, cancel: cancel, records: map[ChildID]*record{}, names: map[string]ChildID{}, pending: map[Ref]*reservation{}, closedDone: make(chan struct{})}, nil
}

func (t *Tree) currentLocked(ref Ref) (*record, error) {
	if t.closed {
		return nil, reject("unavailable", "the owning Session is closed")
	}
	r := t.records[ref.ID]
	if r == nil {
		return nil, reject("rejected", "unknown child ID")
	}
	if r.run.ref.Generation != ref.Generation {
		return nil, reject("rejected", fmt.Sprintf("stale child generation %d; current generation is %d; use litter_inspect with id only to refresh", ref.Generation, r.run.ref.Generation))
	}
	return r, nil
}

func (t *Tree) belongsLocked(caller ChildID, r *record) bool {
	if caller == "" {
		return true
	}
	for parent := r.parent; parent != ""; {
		if parent == caller {
			return true
		}
		record := t.records[parent]
		if record == nil {
			break
		}
		parent = record.parent
	}
	return false
}

func (t *Tree) owned(caller ChildID, id ChildID) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return reject("unavailable", "the owning Session is closed")
	}
	r := t.records[id]
	if r == nil {
		return reject("rejected", "unknown child ID")
	}
	if !t.belongsLocked(caller, r) {
		return reject("permission_denied", "child is outside the caller subtree")
	}
	return nil
}

func (t *Tree) snapshotLocked(r *record) Snapshot {
	var parent *ChildID
	if r.parent != "" {
		value := r.parent
		parent = &value
	}
	return Snapshot{Ref: r.run.ref, Parent: parent, Depth: r.depth, Name: r.name, DisplayLabel: displayLabel(r.name, r.id), Type: r.kind, Model: r.model, State: r.run.state}
}

func displayLabel(name string, id ChildID) string {
	if name != string(id) {
		return name
	}
	sequence := string(id)
	if colon := strings.LastIndexByte(sequence, ':'); colon >= 0 {
		sequence = sequence[colon+1:]
	}
	return "#" + sequence
}

func (t *Tree) roomLocked(parent ChildID, writer bool, cwd string, except ChildID) error {
	if t.live >= t.config.Concurrency {
		return reject("rejected", "live child capacity reached")
	}
	reserved := 0
	for _, pending := range t.pending {
		if pending.parent == parent {
			reserved++
		}
	}
	if reserved >= t.config.MaxMailbox {
		if parent == "" {
			return reject("rejected", "root completion capacity reached")
		}
		return reject("rejected", "parent mailbox capacity reached")
	}
	if writer {
		for _, r := range t.records {
			if r.id != except && !r.readOnly && r.cwd == cwd && !r.run.state.terminal() {
				return reject("rejected", "writer conflict in the shared working directory")
			}
		}
	}
	return nil
}

func (t *Tree) newRunLocked(ref Ref, prompt string) *run {
	ctx, cancel := context.WithCancel(t.ctx)
	return &run{ref: ref, state: Starting, ctx: ctx, cancel: cancel, prompt: prompt, done: make(chan struct{}), ready: make(chan struct{}), mailbox: make(chan message, t.config.MaxMailbox), receipts: map[Ref]*completionReceipt{}}
}

func (t *Tree) admit(request admission) (Snapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return Snapshot{}, reject("unavailable", "the owning Session is closed")
	}
	depth := 1
	var parent *record
	if request.parent != "" {
		var err error
		parent, err = t.currentLocked(Ref{request.parent, request.parentGeneration})
		if err != nil {
			return Snapshot{}, err
		}
		if !parent.canDelegate || parent.run.state != Starting && parent.run.state != Running {
			return Snapshot{}, reject("permission_denied", "the parent cannot delegate")
		}
		depth = parent.depth + 1
		if parent.readOnly && request.agent.Role != "scout" {
			return Snapshot{}, reject("permission_denied", "read-only parent cannot delegate a worker")
		}
		for _, name := range request.agent.Tools {
			if !slices.Contains(parent.tools, name) {
				return Snapshot{}, reject("permission_denied", "child tools exceed the caller ceiling")
			}
		}
	}
	if depth > t.config.Depth {
		return Snapshot{}, reject("rejected", "child depth limit reached")
	}
	if len(t.records) >= t.config.MaxRecords {
		return Snapshot{}, reject("rejected", "retained child limit reached")
	}
	readOnly := request.agent.Role == "scout" || !slices.Contains(request.agent.Tools, "write") && !slices.Contains(request.agent.Tools, "edit")
	if err := t.roomLocked(request.parent, !readOnly, request.cwd, ""); err != nil {
		return Snapshot{}, err
	}
	t.sequence++
	id := ChildID(t.identity + ":" + strconv.Itoa(t.sequence))
	name := request.name
	if name == "" {
		name = string(id)
	}
	nameKey := string(request.parent) + "\x00" + name
	if _, exists := t.names[nameKey]; exists {
		return Snapshot{}, reject("rejected", "child name already belongs to this owner")
	}
	r := &record{id: id, parent: request.parent, parentGeneration: request.parentGeneration, depth: depth, name: name, kind: request.kind, model: request.model, instructions: request.agent.Instructions, task: request.task, cwd: request.cwd, tools: slices.Clone(request.agent.Tools), canDelegate: request.agent.CanDelegate, readOnly: readOnly, history: request.history, authority: request.authority}
	if parent != nil {
		r.parentRun = parent.run
	}
	r.run = t.newRunLocked(Ref{id, 1}, "Task:\n"+request.task)
	t.records[id], t.names[nameKey] = r, id
	t.order = append(t.order, id)
	t.pending[r.run.ref] = &reservation{parent: r.parent, generation: r.parentGeneration}
	t.live++
	return t.snapshotLocked(r), nil
}

func (t *Tree) activate(ref Ref) error {
	t.mu.Lock()
	r, err := t.currentLocked(ref)
	if err != nil {
		t.mu.Unlock()
		return err
	}
	run := r.run
	if run.started || run.state != Starting {
		t.mu.Unlock()
		return reject("rejected", "child activation is no longer pending")
	}
	run.started = true
	r.previous = nil
	t.mu.Unlock()
	t.notify()
	go func() {
		timer := time.AfterFunc(time.Duration(t.config.MaxRunMillis)*time.Millisecond, func() { t.mu.Lock(); run.deadline = true; t.mu.Unlock(); run.cancel(); run.abort() })
		outcome := t.execute(r, run)
		timer.Stop()
		t.finish(r, run, outcome)
	}()
	return nil
}

func (r *run) abort() {
	r.mu.Lock()
	session := r.session
	r.mu.Unlock()
	if session != nil {
		session.RequestAbort()
	}
}

func (t *Tree) cancelPending(ref Ref) {
	t.mu.Lock()
	r := t.records[ref.ID]
	if r == nil || r.run.ref != ref || r.run.started || r.run.state != Starting {
		t.mu.Unlock()
		return
	}
	run := r.run
	run.cancel()
	run.state = Stopped
	run.outcome = &Outcome{Run: ref, State: Stopped, Model: r.model, Error: "spawn admission ended before the tool result was acknowledged"}
	close(run.done)
	t.live--
	delete(t.pending, ref)
	if r.previous != nil {
		r.run = r.previous
		r.previous = nil
	}
	t.mu.Unlock()
	t.notify()
}

func (t *Tree) finish(r *record, run *run, raw Outcome) {
	t.mu.Lock()
	if run.outcome != nil {
		t.mu.Unlock()
		return
	}
	raw.Run, raw.Model = run.ref, r.model
	if raw.Text != "" {
		raw.ReportProvenance = "child_assistant"
	}
	if !raw.State.terminal() {
		raw.State = Failed
	}
	if raw.State == Completed && raw.Error != "" {
		raw.State = Partial
	}
	raw.Text, raw.Truncated = bounded(raw.Text, t.config.MaxResultBytes)
	raw.Error, _ = bounded(raw.Error, 1024)
	if run.deadline {
		raw.State = Failed
		if raw.Text != "" {
			raw.State = Partial
		}
		raw.Error = "child run time bound reached"
	} else if run.ctx.Err() != nil {
		raw.State = Stopped
	}
	run.state, run.outcome = raw.State, &raw
	run.cancel()
	for {
		select {
		case request := <-run.mailbox:
			request.result <- reject("cancelled", "child stopped before message delivery")
		default:
			goto mailboxDrained
		}
	}
mailboxDrained:
	t.live--
	close(run.done)
	closed := t.closed
	t.mu.Unlock()
	t.notify()
	if !closed && t.completion != nil {
		go t.completion(r, raw)
	}
}

func (t *Tree) notify() {
	if t.changed != nil {
		t.changed()
	}
}

func (t *Tree) list(caller ChildID) ([]Snapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, reject("unavailable", "the owning Session is closed")
	}
	list := []Snapshot{}
	for _, id := range t.order {
		r := t.records[id]
		if t.belongsLocked(caller, r) {
			list = append(list, t.snapshotLocked(r))
		}
	}
	return list, nil
}

func (t *Tree) inspect(target InspectTarget, transcript bool, offset, limit int) (Inspection, error) {
	if offset < 0 || offset > 1000000 || limit < 1 || limit > 50 {
		return Inspection{}, reject("rejected", "invalid transcript page")
	}
	t.mu.Lock()
	r := t.records[target.ID]
	if r == nil || t.closed {
		t.mu.Unlock()
		return Inspection{}, reject("unavailable", "child history is unavailable")
	}
	if target.Generation != nil && *target.Generation < 1 {
		t.mu.Unlock()
		return Inspection{}, reject("rejected", "inspection generation must be a positive integer")
	}
	if target.Generation != nil {
		if _, err := t.currentLocked(Ref{target.ID, *target.Generation}); err != nil {
			t.mu.Unlock()
			return Inspection{}, err
		}
	}
	result := Inspection{Child: t.snapshotLocked(r), Outcome: previewOutcome(r.run.outcome, 2048), HistoryAvailable: r.history != nil && !r.historyLost, TranscriptRequested: transcript, Entries: []json.RawMessage{}, Mailbox: []Outcome{}}
	var frozenEvidence *ToolEvidence
	if r.run.outcome != nil {
		frozenEvidence = r.run.outcome.Evidence
	}
	manager := r.history
	var entries []json.RawMessage
	if manager != nil {
		entries = historyEntries(manager)
	}
	for _, childID := range t.order {
		child := t.records[childID]
		if child.parent == target.ID && child.run.outcome != nil {
			result.MailboxTotal++
			result.Mailbox = append(result.Mailbox, *previewOutcome(child.run.outcome, 512))
		}
	}
	if len(result.Mailbox) > 8 {
		result.Mailbox = result.Mailbox[len(result.Mailbox)-8:]
	}
	result.MailboxMore = result.MailboxTotal > 8
	t.mu.Unlock()
	if manager != nil {
		result.Evidence = projectEvidence(result.Child.Ref, entries)
	} else if frozenEvidence != nil {
		result.Evidence = *frozenEvidence
	} else {
		result.Evidence = ToolEvidence{Source: "retained_tool_results", TextFormat: "concatenated_text_blocks", Unavailable: "retained history and frozen evidence are unavailable"}
	}
	if transcript && manager != nil {
		start := min(offset, len(entries))
		end := min(start+limit, len(entries))
		result.Entries = append(result.Entries, entries[start:end]...)
		data, err := json.Marshal(result.Entries)
		if err != nil {
			return Inspection{}, err
		}
		if len(data) > min(t.config.MaxHistoryBytes, 32768) {
			return Inspection{}, reject("rejected", "transcript page exceeds 32768 bytes")
		}
		result.NextOffset, result.More = offset+len(result.Entries), end < len(entries)
	}
	return result, nil
}

func (t *Tree) wait(ctx context.Context, ref Ref, timeout int) (Outcome, error) {
	if timeout < 1 || timeout > 300000 {
		return Outcome{}, reject("rejected", "invalid wait timeout")
	}
	t.mu.Lock()
	r, err := t.currentLocked(ref)
	var run *run
	if err == nil {
		run = r.run
	}
	t.mu.Unlock()
	if err != nil {
		return Outcome{}, err
	}
	if ctx.Err() != nil {
		return Outcome{}, reject("cancelled", "wait cancelled")
	}
	timer := time.NewTimer(time.Duration(timeout) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-run.done:
		return *run.outcome, nil
	case <-timer.C:
		return Outcome{}, reject("wait_timeout", "wait timed out")
	case <-ctx.Done():
		return Outcome{}, reject("cancelled", "wait cancelled")
	}
}

func (t *Tree) resume(ref Ref, text string) (Snapshot, error) {
	t.mu.Lock()
	r, err := t.currentLocked(ref)
	if err != nil {
		t.mu.Unlock()
		return Snapshot{}, err
	}
	if !r.run.state.terminal() {
		t.mu.Unlock()
		return Snapshot{}, reject("rejected", "resume needs a settled child and text")
	}
	manager := r.history
	if manager == nil || r.historyLost {
		t.mu.Unlock()
		return Snapshot{}, reject("unavailable", "retained history is unavailable for resume")
	}
	t.mu.Unlock()
	if historyBytes(manager)+len(text)+512 > t.config.MaxHistoryBytes {
		return Snapshot{}, reject("rejected", "resume would exceed the retained history limit")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r, err = t.currentLocked(ref)
	if err != nil {
		return Snapshot{}, err
	}
	if r.history != manager || r.historyLost || !r.run.state.terminal() {
		return Snapshot{}, reject("unavailable", "retained history or generation changed before resume")
	}
	if err := t.roomLocked(r.parent, !r.readOnly, r.cwd, r.id); err != nil {
		return Snapshot{}, err
	}
	r.previous = r.run
	r.run = t.newRunLocked(Ref{r.id, ref.Generation + 1}, text)
	t.pending[r.run.ref] = &reservation{parent: r.parent, generation: r.parentGeneration}
	t.live++
	return t.snapshotLocked(r), nil
}

func (t *Tree) message(ctx context.Context, ref Ref, mode, text string) (Snapshot, error) {
	if len(text) > 16384 || len(strings.TrimSpace(text)) == 0 {
		return Snapshot{}, reject("rejected", "invalid child message or message byte bound reached")
	}
	if mode == "resume" {
		return t.resume(ref, text)
	}
	if mode != "steer" && mode != "follow_up" {
		return Snapshot{}, reject("rejected", "invalid child message")
	}
	t.mu.Lock()
	r, err := t.currentLocked(ref)
	if err != nil {
		t.mu.Unlock()
		return Snapshot{}, err
	}
	run := r.run
	if run.state != Starting && run.state != Running {
		t.mu.Unlock()
		return Snapshot{}, reject("rejected", "child is not live")
	}
	run.mu.Lock()
	session, disposing := run.session, run.disposing
	run.mu.Unlock()
	state := run.state
	t.mu.Unlock()
	if disposing || session != nil && !session.IsStreaming() && state == Running {
		return Snapshot{}, reject("rejected", "child is settling and cannot accept a message")
	}
	run.messageMu.Lock()
	queued := run.pendingMessages
	if session != nil {
		queued += session.PendingMessageCount()
	}
	if queued >= t.config.MaxMailbox {
		run.messageMu.Unlock()
		return Snapshot{}, reject("rejected", "child message bound reached")
	}
	request := message{mode: mode, text: text, result: make(chan error, 1)}
	select {
	case run.mailbox <- request:
		run.pendingMessages++
	default:
		run.messageMu.Unlock()
		return Snapshot{}, reject("rejected", "child message bound reached")
	}
	run.messageMu.Unlock()
	select {
	case <-run.ready:
		select {
		case err := <-request.result:
			if err != nil {
				return Snapshot{}, err
			}
		case <-run.ctx.Done():
			return Snapshot{}, reject("cancelled", "child stopped before message delivery")
		case <-ctx.Done():
			return Snapshot{}, reject("cancelled", "message caller cancelled")
		}
	default:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(r), nil
}

func (t *Tree) stop(ref Ref) (*Outcome, int, error) {
	t.mu.Lock()
	target, err := t.currentLocked(ref)
	if err != nil {
		t.mu.Unlock()
		return nil, 0, err
	}
	targetRun := target.run
	var pending []*run
	var fresh []*run
	for _, id := range t.order {
		r := t.records[id]
		if r.id != target.id && !t.belongsLocked(target.id, r) {
			continue
		}
		run := r.run
		if run.state.terminal() {
			continue
		}
		pending = append(pending, run)
		if run.state != Stopping {
			run.state = Stopping
			fresh = append(fresh, run)
		}
	}
	t.mu.Unlock()
	for _, run := range fresh {
		run.cancel()
	}
	for _, run := range fresh {
		run.abort()
		if !run.started {
			t.mu.Lock()
			r := t.records[run.ref.ID]
			t.mu.Unlock()
			t.finish(r, run, Outcome{State: Stopped})
		}
	}
	for _, run := range pending {
		<-run.done
	}
	t.notify()
	return targetRun.outcome, len(fresh), nil
}

func (t *Tree) retireHistory(r *record) {
	t.mu.Lock()
	r.history = nil
	r.historyLost = true
	run := r.run
	pending := !run.started && run.state == Starting
	live := !run.state.terminal()
	t.mu.Unlock()
	if pending {
		t.cancelPending(run.ref)
	} else if live {
		run.abort()
	}
}

func (t *Tree) close() {
	t.mu.Lock()
	if t.closed {
		done := t.closedDone
		t.mu.Unlock()
		<-done
		return
	}
	t.closed = true
	var runs []*run
	for _, id := range t.order {
		run := t.records[id].run
		if !run.state.terminal() {
			run.state = Stopping
			runs = append(runs, run)
		}
	}
	t.mu.Unlock()
	t.cancel()
	for _, run := range runs {
		run.cancel()
	}
	for _, run := range runs {
		run.abort()
		if !run.started {
			t.mu.Lock()
			r := t.records[run.ref.ID]
			t.mu.Unlock()
			t.finish(r, run, Outcome{State: Stopped})
		}
	}
	for _, run := range runs {
		<-run.done
	}
	close(t.closedDone)
}

func (t *Tree) acknowledge(ref Ref) { t.mu.Lock(); delete(t.pending, ref); t.mu.Unlock(); t.notify() }

func (t *Tree) startCompletion(ref Ref) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	reservation := t.pending[ref]
	if t.closed || reservation == nil || reservation.sent {
		return false
	}
	reservation.sent = true
	return true
}
