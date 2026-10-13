package piglitter

import (
	"context"
	"crypto/rand"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
)

type reservation struct {
	parent          ChildID
	nonce           string
	sent            bool
	outcomeObserved bool
	stored          bool
	continueTurn    bool
	content         string
	selected        *run
	auditOnce       sync.Once
	auditErr        error
}

type Tree struct {
	mu           sync.Mutex
	config       Config
	ctx          context.Context
	cancel       context.CancelFunc
	root         *record
	records      map[ChildID]*record
	order        []ChildID
	names        map[string]ChildID
	pending      map[*run]*reservation
	live         int
	closed       bool
	closedDone   chan struct{}
	completionWG sync.WaitGroup
	execute      func(*record, *run) Outcome
	completion   func(*record, *run, Outcome)
	changed      func()
}

type admission struct {
	parentRun                    *run
	kind, name, model, cwd, task string
	agent                        AgentDefinition
	history                      *coding.SessionManager
}

func newTree(config Config) *Tree {
	ctx, cancel := context.WithCancel(context.Background())
	return &Tree{config: config, root: &record{id: "root", name: "root"}, ctx: ctx, cancel: cancel, records: map[ChildID]*record{}, names: map[string]ChildID{}, pending: map[*run]*reservation{}, closedDone: make(chan struct{})}
}

func (t *Tree) currentLocked(selected *run) (*record, error) {
	if t.closed {
		return nil, reject("unavailable", "the owning Session is closed")
	}
	if selected == nil {
		return nil, reject("rejected", "unknown child ID")
	}
	r := t.records[selected.ref.ID]
	if r == nil {
		return nil, reject("rejected", "unknown child ID")
	}
	if r.run != selected {
		return nil, reject("rejected", "the child execution is no longer current")
	}
	return r, nil
}

func (t *Tree) selectRun(id ChildID) (*run, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return nil, reject("unavailable", "the owning Session is closed")
	}
	r := t.records[id]
	if r == nil {
		return nil, reject("rejected", "unknown child ID")
	}
	return r.run, nil
}

func (t *Tree) callerLocked(caller *run) (*record, error) {
	if t.closed {
		return nil, reject("unavailable", "the owning Session is closed")
	}
	if caller == nil {
		if t.root.paused {
			return nil, reject("rejected", "root is paused")
		}
		return t.root, nil
	}
	parent, err := t.currentLocked(caller)
	if err != nil {
		return nil, err
	}
	if parent.paused || caller.state != Starting && caller.state != Running {
		return nil, reject("rejected", "the caller is not active")
	}
	return parent, nil
}

func (t *Tree) belongsLocked(caller *record, r *record) bool {
	for parent := r.parent; parent != nil; parent = parent.parent {
		if parent == caller {
			return true
		}
	}
	return false
}

func (t *Tree) selectOwned(caller *run, id ChildID) (*run, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	parent, err := t.callerLocked(caller)
	if err != nil {
		return nil, err
	}
	r := t.records[id]
	if r == nil {
		return nil, reject("rejected", "unknown child ID")
	}
	if r.parent != parent {
		return nil, reject("permission_denied", "only the immediate parent can control this child")
	}
	return r.run, nil
}

func (t *Tree) controlLocked(caller, selected *run) (*record, error) {
	parent, err := t.callerLocked(caller)
	if err != nil {
		return nil, err
	}
	r, err := t.currentLocked(selected)
	if err != nil {
		return nil, err
	}
	if r.parent != parent {
		return nil, reject("permission_denied", "only the immediate parent can control this child")
	}
	return r, nil
}

func (r *record) parentID() ChildID {
	if r.parent == nil {
		return ""
	}
	return r.parent.id
}

func (t *Tree) rootID(id ChildID) bool {
	return id == "" || t.root != nil && id == t.root.id
}

func (t *Tree) snapshotLocked(r *record) Snapshot {
	var parent *ChildID
	if r.parent != nil {
		value := r.parent.id
		parent = &value
	}
	state := r.run.state
	if state.terminal() && r.latest != nil {
		state = r.latest.State
	}
	if r.paused && state.terminal() {
		state = Paused
	}
	return Snapshot{Ref: r.run.ref, Parent: parent, Depth: r.depth, Name: r.name, DisplayLabel: displayLabel(r.name, r.id), Type: r.kind, Model: r.model, Cwd: r.cwd, State: state}
}

func displayLabel(name string, id ChildID) string {
	if name != string(id) {
		return name
	}
	return "#" + string(id)[max(0, len(id)-8):]
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
		if t.rootID(parent) {
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
	return &run{ref: ref, noticeID: rand.Text(), state: Starting, ctx: ctx, cancel: cancel, prompt: prompt, done: make(chan struct{}), ready: make(chan struct{}), mailbox: make(chan message, t.config.MaxMailbox)}
}

func (t *Tree) admit(request admission) (Snapshot, *run, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return Snapshot{}, nil, reject("unavailable", "the owning Session is closed")
	}
	parent, err := t.callerLocked(request.parentRun)
	if err != nil {
		return Snapshot{}, nil, err
	}
	depth := parent.depth + 1
	if parent != t.root {
		if !parent.canDelegate {
			return Snapshot{}, nil, reject("permission_denied", "the parent cannot delegate")
		}
		for _, name := range request.agent.Tools {
			if !slices.Contains(parent.tools, name) {
				return Snapshot{}, nil, reject("permission_denied", "child tools exceed the caller ceiling")
			}
		}
	}
	if depth > t.config.Depth {
		return Snapshot{}, nil, reject("rejected", "child depth limit reached")
	}
	if len(t.records) >= t.config.MaxRecords {
		return Snapshot{}, nil, reject("rejected", "retained child limit reached")
	}
	readOnly := request.agent.Role == "scout" || !slices.Contains(request.agent.Tools, "write") && !slices.Contains(request.agent.Tools, "edit") && !slices.Contains(request.agent.Tools, "bash")
	if err := t.roomLocked(parent.id, !readOnly, request.cwd, ""); err != nil {
		return Snapshot{}, nil, err
	}
	if request.history == nil {
		request.history, err = coding.NewInMemorySessionManager(request.cwd)
		if err != nil {
			return Snapshot{}, nil, err
		}
	}
	id := ChildID(request.history.GetSessionId())
	if _, exists := t.records[id]; exists {
		return Snapshot{}, nil, reject("rejected", "session is already retained")
	}
	name := request.name
	if name == "" {
		name = string(id)
	}
	nameKey := string(parent.id) + "\x00" + name
	if _, exists := t.names[nameKey]; exists {
		return Snapshot{}, nil, reject("rejected", "child name already belongs to this owner")
	}
	r := &record{id: id, parent: parent, depth: depth, name: name, kind: request.kind, model: request.model, instructions: request.agent.Instructions, task: request.task, cwd: request.cwd, tools: slices.Clone(request.agent.Tools), canDelegate: request.agent.CanDelegate && request.agent.Role != "scout", readOnly: readOnly, history: request.history}
	r.run = t.newRunLocked(Ref{id}, "Task:\n"+request.task)
	r.run.owner = r
	t.records[id], t.names[nameKey] = r, id
	t.order = append(t.order, id)
	r.run.reservation = &reservation{parent: r.parentID(), nonce: r.run.noticeID, selected: r.run}
	t.pending[r.run] = r.run.reservation
	t.live++
	return t.snapshotLocked(r), r.run, nil
}

func (t *Tree) activate(selected *run) error {
	t.mu.Lock()
	r, err := t.currentLocked(selected)
	if err != nil {
		t.mu.Unlock()
		return err
	}
	run := r.run
	if run.started || run.state != Starting || r.paused {
		t.mu.Unlock()
		return reject("rejected", "child activation is no longer pending")
	}
	run.started = true
	r.previous = nil
	t.mu.Unlock()
	t.notify()
	go func() {
		outcome := t.execute(r, run)
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

func (t *Tree) cancelPending(selected *run) {
	t.mu.Lock()
	r := t.records[selected.ref.ID]
	if r == nil || r.run != selected || r.run.started {
		t.mu.Unlock()
		return
	}
	run := r.run
	run.cancel()
	if run.outcome == nil {
		run.state = Stopped
		run.outcome = &Outcome{Run: selected.ref, State: Stopped, Model: r.model, Error: "child admission ended before final tool success"}
		close(run.done)
		t.live--
	}
	if t.pending[selected] != nil {
		delete(t.pending, selected)
	}
	if r.previous != nil {
		r.run = r.previous
		r.previous = nil
		r.paused = r.paused || selected.resumePaused
		r.latest = r.run.outcome
	} else {
		delete(t.records, r.id)
		delete(t.names, string(r.parentID())+"\x00"+r.name)
		t.order = slices.DeleteFunc(t.order, func(id ChildID) bool { return id == r.id })
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
	if run.deliveryError != "" {
		raw.Error = strings.TrimSpace(raw.Error + " " + run.deliveryError)
	}
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
	if r.paused {
		raw.State = Paused
	} else if run.ctx.Err() != nil {
		raw.State = Stopped
	}
	run.state, run.outcome = raw.State, &raw
	if r.run == run {
		r.latest = &raw
	}
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
	run.messageMu.Lock()
	run.pendingMessages = 0
	run.messageMu.Unlock()
	t.live--
	close(run.done)
	deliver := run.started && !t.closed && t.completion != nil
	if deliver {
		t.completionWG.Add(1)
	}
	t.mu.Unlock()
	t.notify()
	if deliver {
		go func() {
			defer t.completionWG.Done()
			t.completion(r, run, raw)
		}()
	}
}

func (t *Tree) notify() {
	if t.changed != nil {
		t.changed()
	}
}

func (t *Tree) snapshotsLocked(parent *record, activeOnly bool) []Snapshot {
	list := []Snapshot{}
	for _, id := range t.order {
		r := t.records[id]
		if r.parent == parent && (!activeOnly || !r.paused && !r.run.state.terminal()) {
			list = append(list, t.snapshotLocked(r))
		}
	}
	return list
}

func (t *Tree) list(caller *run) ([]Snapshot, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	parent, err := t.callerLocked(caller)
	if err != nil {
		return nil, err
	}
	return t.snapshotsLocked(parent, true), nil
}

func (t *Tree) retained() []Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotsLocked(t.root, false)
}

func (t *Tree) inspect(id ChildID) (Inspection, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.records[id]
	if r == nil || t.closed {
		return Inspection{}, reject("unavailable", "child is unavailable")
	}
	var outcome *Outcome
	if r.latest != nil {
		copy := *r.latest
		outcome = &copy
	}
	return Inspection{Child: t.snapshotLocked(r), Outcome: outcome}, nil
}

func (t *Tree) wait(ctx context.Context, selected *run, timeout int) (Outcome, error) {
	if timeout < 1 || timeout > 300000 {
		return Outcome{}, reject("rejected", "invalid wait timeout")
	}
	if ctx.Err() != nil {
		return Outcome{}, reject("cancelled", "wait cancelled")
	}
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return Outcome{}, reject("unavailable", "the owning Session is closed")
	}
	if selected.outcome != nil {
		outcome := selected.outcome
		if current := t.records[selected.ref.ID]; current != nil && current.run == selected && current.latest != nil {
			outcome = current.latest
		}
		copy := *outcome
		t.mu.Unlock()
		return copy, nil
	}
	t.mu.Unlock()
	timer := time.NewTimer(time.Duration(timeout) * time.Millisecond)
	defer timer.Stop()
	select {
	case <-selected.done:
		return *selected.outcome, nil
	case <-timer.C:
		return Outcome{}, reject("wait_timeout", "wait timed out")
	case <-ctx.Done():
		return Outcome{}, reject("cancelled", "wait cancelled")
	}
}

func (t *Tree) resume(caller, selected *run, text string) (Snapshot, *run, error) {
	t.mu.Lock()
	r, err := t.controlLocked(caller, selected)
	if err != nil {
		t.mu.Unlock()
		return Snapshot{}, nil, err
	}
	if !r.run.state.terminal() {
		t.mu.Unlock()
		return Snapshot{}, nil, reject("rejected", "resume needs a settled child and text")
	}
	manager := r.history
	if manager == nil || r.historyLost {
		t.mu.Unlock()
		return Snapshot{}, nil, reject("unavailable", "retained history is unavailable for resume")
	}
	t.mu.Unlock()
	if historyBytes(manager)+len(text)+512 > t.config.MaxHistoryBytes {
		return Snapshot{}, nil, reject("rejected", "resume would exceed the retained history limit")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	r, err = t.controlLocked(caller, selected)
	if err != nil {
		return Snapshot{}, nil, err
	}
	if r.history != manager || r.historyLost || !r.run.state.terminal() {
		return Snapshot{}, nil, reject("unavailable", "retained history or execution changed before resume")
	}
	if err := t.roomLocked(r.parentID(), !r.readOnly, r.cwd, r.id); err != nil {
		return Snapshot{}, nil, err
	}
	for parent := r.parent; parent != nil; parent = parent.parent {
		if parent.paused {
			return Snapshot{}, nil, reject("rejected", "the parent is paused")
		}
	}
	r.previous = r.run
	r.run = t.newRunLocked(Ref{r.id}, text)
	r.run.owner = r
	r.run.resumePaused = r.paused
	r.paused = false
	t.pending[r.run] = &reservation{parent: r.parentID(), nonce: r.run.noticeID}
	t.live++
	return t.snapshotLocked(r), r.run, nil
}

func (t *Tree) message(ctx context.Context, caller, selected *run, mode, text string) (Snapshot, *run, error) {
	if len(text) > 16384 || len(strings.TrimSpace(text)) == 0 {
		return Snapshot{}, nil, reject("rejected", "invalid child message or message byte bound reached")
	}
	if mode == "resume" {
		return t.resume(caller, selected, text)
	}
	if mode != "steer" && mode != "follow_up" {
		return Snapshot{}, nil, reject("rejected", "invalid child message")
	}
	t.mu.Lock()
	r, err := t.controlLocked(caller, selected)
	if err != nil {
		t.mu.Unlock()
		return Snapshot{}, nil, err
	}
	run := r.run
	if run.state != Starting && run.state != Running {
		t.mu.Unlock()
		return Snapshot{}, nil, reject("rejected", "child is not live")
	}
	run.mu.Lock()
	session, disposing := run.session, run.disposing
	run.mu.Unlock()
	state := run.state
	t.mu.Unlock()
	if disposing || session != nil && !session.IsStreaming() && state == Running {
		return Snapshot{}, nil, reject("rejected", "child is settling and cannot accept a message")
	}
	t.mu.Lock()
	if _, err := t.controlLocked(caller, selected); err != nil || r.paused || run.state != Starting && run.state != Running {
		t.mu.Unlock()
		return Snapshot{}, nil, reject("rejected", "child is no longer active")
	}
	run.messageMu.Lock()
	queued := run.pendingMessages
	if session != nil {
		queued += session.PendingMessageCount()
	}
	if queued >= t.config.MaxMailbox {
		run.messageMu.Unlock()
		t.mu.Unlock()
		return Snapshot{}, nil, reject("rejected", "child message bound reached")
	}
	request := message{mode: mode, text: text, result: make(chan error, 1)}
	select {
	case run.mailbox <- request:
		run.pendingMessages++
	default:
		run.messageMu.Unlock()
		t.mu.Unlock()
		return Snapshot{}, nil, reject("rejected", "child message bound reached")
	}
	run.messageMu.Unlock()
	t.mu.Unlock()
	select {
	case <-run.ready:
		select {
		case err := <-request.result:
			if err != nil {
				return Snapshot{}, nil, err
			}
		case <-run.ctx.Done():
			return Snapshot{}, nil, reject("cancelled", "child stopped before message delivery")
		case <-ctx.Done():
			return Snapshot{}, nil, reject("cancelled", "message caller cancelled")
		}
	default:
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.snapshotLocked(r), run, nil
}

func (t *Tree) stop(caller, selected *run) (*Outcome, int, error) {
	t.mu.Lock()
	target, err := t.controlLocked(caller, selected)
	if err != nil {
		t.mu.Unlock()
		return nil, 0, err
	}
	var settled *Outcome
	if selected.outcome != nil && target.latest != nil {
		copy := *target.latest
		settled = &copy
	}
	pending, fresh := t.pauseLocked(target)
	t.mu.Unlock()
	t.joinPaused(pending, fresh)
	if settled != nil {
		return settled, len(fresh), nil
	}
	return selected.outcome, len(fresh), nil
}

func (t *Tree) pauseLocked(target *record) ([]*run, []*run) {
	target.paused = true
	var pending, fresh []*run
	for _, id := range t.order {
		r := t.records[id]
		if r != target && !t.belongsLocked(target, r) {
			continue
		}
		r.paused = true
		run := r.run
		if run.state.terminal() {
			continue
		}
		pending = append(pending, run)
		if run.state != Pausing {
			run.state = Pausing
			fresh = append(fresh, run)
		}
	}
	return pending, fresh
}

func (t *Tree) joinPaused(pending, fresh []*run) {
	for _, run := range fresh {
		run.cancel()
		run.abort()
		t.mu.Lock()
		started := run.started
		t.mu.Unlock()
		if !started {
			t.finish(run.owner, run, Outcome{State: Paused})
		}
	}
	for _, run := range pending {
		<-run.done
	}
	t.notify()
}

func (t *Tree) pauseRoot() {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	pending, fresh := t.pauseLocked(t.root)
	t.mu.Unlock()
	t.joinPaused(pending, fresh)
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
		t.cancelPending(run)
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
	t.completionWG.Wait()
	t.mu.Lock()
	for selected := range t.pending {
		delete(t.pending, selected)
	}
	t.mu.Unlock()
	close(t.closedDone)
}

func (t *Tree) acknowledge(selected *run) {
	t.mu.Lock()
	if pending := t.pending[selected]; pending != nil {
		pending.stored = true
		selected.reservation = pending
		delete(t.pending, selected)
	}
	t.mu.Unlock()
	t.notify()
}

func (t *Tree) startCompletion(selected *run, content string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	reservation := t.pending[selected]
	if t.closed || reservation == nil || reservation.sent {
		return false
	}
	reservation.content = content
	reservation.sent = true
	reservation.selected = selected
	selected.reservation = reservation
	return true
}

func (t *Tree) deliveryFailed(selected *run, failure error) {
	if failure == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	child := t.records[selected.ref.ID]
	if child == nil {
		return
	}
	target := child.parent
	if target == nil || target == t.root {
		target = child
	}
	description, _ := bounded("child completion delivery failed: "+failure.Error(), 1024)
	if !target.run.state.terminal() {
		target.run.deliveryError = description
		return
	}
	if target.latest != nil {
		outcome := *target.latest
		outcome.Error, _ = bounded(strings.TrimSpace(outcome.Error+" "+description), 1024)
		if outcome.State == Completed {
			outcome.State = Partial
		}
		target.latest = &outcome
	}
}
