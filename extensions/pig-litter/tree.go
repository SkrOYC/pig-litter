package piglitter

import (
	"context"
	"crypto/rand"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
)

type reservation struct {
	parent          ChildID
	nonce           string
	sent            bool
	outcomeObserved bool
	handback        *outcomeHandback
	stored          bool
	continueTurn    bool
	content         string
	selected        *run
	auditMu         sync.Mutex
	auditStored     bool
	auditErr        error
	auditAttempted  bool
	nativeAttempted bool
}

type Tree struct {
	mu           sync.Mutex
	lifecycleMu  sync.Mutex
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
	executeWG    sync.WaitGroup
	teamWG       sync.WaitGroup
	peerWakeWG   sync.WaitGroup
	roomChanged  chan struct{}
	execute      func(*record, *run) Outcome
	completion   func(*record, *run, Outcome)
	changed      func()
	persist      func(*record) error
	teamFinished func(*record, *run, Outcome) error
	teamSettled  func(*record, *run)
	teamCanceled func(*record)
	teamPaused   func(*record, *run) error
	teamResuming func(*record, *run) error
	dirty        map[ChildID]*record
}

type admission struct {
	parentRun                    *run
	kind, name, model, cwd, task string
	agent                        AgentDefinition
	available                    []string
	member                       *teamMember
	backend                      backendRequest
}

type controlAuthority struct {
	caller *run
	human  func() error
}

func newTree(config Config) *Tree {
	ctx, cancel := context.WithCancel(context.Background())
	return &Tree{config: config, root: &record{id: "root", name: "root"}, ctx: ctx, cancel: cancel, records: map[ChildID]*record{}, names: map[string]ChildID{}, pending: map[*run]*reservation{}, dirty: map[ChildID]*record{}, closedDone: make(chan struct{}), roomChanged: make(chan struct{})}
}

func (t *Tree) currentLocked(selected *run) (*record, error) {
	if t.closed {
		return nil, reject("unavailable", "the owning Session is closed")
	}
	if selected == nil {
		return nil, reject("unavailable", "child UUID is unavailable")
	}
	r := t.records[selected.ref.ID]
	if r == nil {
		return nil, reject("unavailable", "child UUID is unavailable")
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
		return nil, reject("unavailable", "child UUID is unavailable")
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
		return nil, reject("unavailable", "child UUID is unavailable")
	}
	if r.parent != parent {
		return nil, reject("permission_denied", "only the immediate parent can control this child")
	}
	return r.run, nil
}

func (t *Tree) controlAsLocked(authority controlAuthority, selected *run) (*record, error) {
	parent := t.root
	var err error
	if authority.human != nil {
		err = authority.human()
	} else {
		parent, err = t.callerLocked(authority.caller)
	}
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
	if authority.human != nil && selected.admission != admissionReady {
		return nil, reject("rejected", "child admission is pending its model tool acknowledgement or storage")
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
	if r.run.finishing {
		state = Starting
		if r.run.started {
			state = Running
		}
	} else {
		if state.terminal() && r.latest != nil {
			state = r.latest.State
		}
		if r.paused && state.terminal() {
			state = Paused
		}
	}
	return Snapshot{Ref: r.run.ref, Parent: parent, Depth: r.depth, Name: r.name, DisplayLabel: displayLabel(r.name, r.id), Type: r.kind, Model: r.model, Cwd: r.cwd, State: state}
}

func displayLabel(name string, id ChildID) string {
	if name != string(id) {
		return name
	}
	return "#" + string(id)[max(0, len(id)-8):]
}

func (t *Tree) roomLocked(parent ChildID, writer bool, cwd string, except ChildID, member *teamMember) error {
	if t.live >= t.config.Concurrency {
		return reject("rejected", "live child capacity reached")
	}
	reserved := 0
	for _, pending := range t.pending {
		if pending.parent == parent {
			reserved++
		}
	}
	if member == nil && reserved >= t.config.MaxMailbox {
		if t.rootID(parent) {
			return reject("rejected", "root completion capacity reached")
		}
		return reject("rejected", "parent mailbox capacity reached")
	}
	if writer {
		for _, r := range t.records {
			if r.id != except && !r.readOnly && r.cwd == cwd && !r.run.state.terminal() {
				if member != nil && r.team != nil && member.Ref.Team == r.team.Ref.Team && member.Workspace.Kind == "shared_checkout" && r.team.Workspace.Kind == "shared_checkout" && member.Workspace.Cwd == cwd && r.team.Workspace.Cwd == cwd {
					continue
				}
				return reject("rejected", "writer conflict in the shared working directory")
			}
		}
	}
	return nil
}

func (t *Tree) signalRoomChangedLocked() {
	close(t.roomChanged)
	t.roomChanged = make(chan struct{})
}

func (t *Tree) newRunLocked(ref Ref, prompt string) *run {
	ctx, cancel := context.WithCancel(t.ctx)
	return &run{ref: ref, noticeID: rand.Text(), state: Starting, ctx: ctx, cancel: cancel, prompt: prompt, done: make(chan struct{}), ready: make(chan struct{}), teamFinishDone: make(chan struct{}), mailbox: make(chan message, t.config.MaxMailbox)}
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
	for ancestor := parent; ancestor != nil && ancestor != t.root; ancestor = ancestor.parent {
		if ancestor.controlPhase == "closing" || ancestor.controlPhase == "terminated" || ancestor.controlPhase == "recovery_pending" {
			return Snapshot{}, nil, reject("unavailable", "teammate shutdown prevents child admission")
		}
	}
	depth := parent.depth + 1
	available := request.available
	if available == nil {
		available = t.root.tools
	}
	if parent != t.root {
		if !parent.canDelegate {
			return Snapshot{}, nil, reject("permission_denied", "the parent cannot delegate")
		}
		for _, name := range expandTools(request.agent.Tools, available) {
			if !withinParent(parent, t.root, name) {
				return Snapshot{}, nil, reject("permission_denied", "child tools exceed the caller ceiling")
			}
		}
	}
	if depth > t.config.Depth {
		return Snapshot{}, nil, reject("rejected", "child depth limit reached")
	}
	readOnly := request.agent.Role == "scout" || !matchesTool(request.agent.Tools, "write") && !matchesTool(request.agent.Tools, "edit") && !matchesTool(request.agent.Tools, "bash")
	if err := t.roomLocked(parent.id, !readOnly, request.cwd, "", request.member); err != nil {
		return Snapshot{}, nil, err
	}
	var id ChildID
	if request.member != nil {
		workspace := request.member.Workspace
		validWorkspace := workspace.Kind == "shared_checkout" || workspace.Kind == "isolated_worktree" && workspace.Phase == "intent"
		if parent != t.root || request.member.Ref.Team.Root != t.root.id || !validWorkspace || workspace.Cwd != request.cwd {
			return Snapshot{}, nil, reject("permission_denied", "invalid team workspace admission")
		}
		id = request.member.Ref.ID
	} else {
		uuid, err := ai.UUIDv7(nil)
		if err != nil {
			return Snapshot{}, nil, err
		}
		id = ChildID(uuid)
	}
	if t.records[id] != nil {
		return Snapshot{}, nil, reject("rejected", "child UUID is already owned")
	}
	name := request.name
	if name == "" {
		name = string(id)
	}
	nameKey := string(parent.id) + "\x00" + name
	if _, exists := t.names[nameKey]; exists && request.member == nil {
		return Snapshot{}, nil, reject("rejected", "child name already belongs to this owner")
	}
	r := &record{id: id, team: request.member, backend: request.backend, parent: parent, depth: depth, name: name, kind: request.kind, model: request.model, instructions: request.agent.Instructions, pendingTask: request.task, cwd: request.cwd, tools: slices.Clone(request.agent.Tools), canDelegate: request.agent.CanDelegate && request.agent.Role != "scout", readOnly: readOnly}
	r.run = t.newRunLocked(Ref{id}, "Task:\n"+request.task)
	r.run.owner = r
	t.records[id] = r
	if request.member == nil {
		t.names[nameKey] = id
	}
	t.order = append(t.order, id)
	if request.member == nil {
		r.run.reservation = &reservation{parent: r.parentID(), nonce: r.run.noticeID, selected: r.run}
		t.pending[r.run] = r.run.reservation
	}
	t.live++
	return t.snapshotLocked(r), r.run, nil
}

func (t *Tree) activate(selected *run) error {
	return t.activateChecked(selected, nil)
}

func (t *Tree) activateChecked(selected *run, accept func() error) error {
	t.lifecycleMu.Lock()
	t.mu.Lock()
	r, err := t.currentLocked(selected)
	if err != nil {
		t.mu.Unlock()
		t.lifecycleMu.Unlock()
		return err
	}
	run := r.run
	if run.started || run.state != Starting || r.paused {
		t.mu.Unlock()
		t.lifecycleMu.Unlock()
		return reject("rejected", "child activation is no longer pending")
	}
	run.started = true
	run.admission = admissionStoring
	r.admitted = true
	r.previous = nil
	previousInputs := slices.Clone(r.pendingInputs)
	if run.resume && run.peerWake == nil {
		r.pendingInputs = append(r.pendingInputs, pendingInput{Text: run.continuation})
	}
	t.mu.Unlock()
	err = t.store(r)
	activated := time.Now()
	if err == nil && accept != nil {
		err = accept()
	}
	if err == nil && r.team != nil && run.resume && run.peerWake == nil {
		if t.teamResuming == nil {
			err = reject("unavailable", "teammate resume is unavailable")
		} else {
			err = t.teamResuming(r, run)
		}
	}
	t.mu.Lock()
	if err != nil {
		r.pendingInputs = previousInputs
		if r.team != nil && run.resume && run.peerWake == nil && run.resumePaused {
			r.paused = true
		}
	} else {
		run.admission = admissionReady
		run.progress.activated = activated
		run.executorDone = make(chan struct{})
		t.executeWG.Add(1)
	}
	t.mu.Unlock()
	t.lifecycleMu.Unlock()
	t.notify()
	if err != nil {
		t.finish(r, run, Outcome{State: Unavailable, Error: "child activation storage failed: " + err.Error()})
		return err
	}
	go func() {
		defer t.executeWG.Done()
		defer close(run.executorDone)
		outcome := t.execute(r, run)
		t.finish(r, run, outcome)
	}()
	return nil
}

func (r *run) abort() {
	r.mu.Lock()
	session := r.session
	member := r.member
	r.mu.Unlock()
	if session != nil {
		session.RequestAbort()
	}
	if member != nil {
		member.Cancel()
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
		if r.team != nil && run.teamFinishDone != nil {
			close(run.teamFinishDone)
		}
		t.live--
		t.signalRoomChangedLocked()
	}
	if t.pending[selected] != nil {
		delete(t.pending, selected)
	}
	if r.previous != nil {
		r.run = r.previous
		r.previous = nil
		r.paused = r.paused || selected.resumePaused
		t.dirty[r.id] = r
	} else {
		delete(t.records, r.id)
		nameKey := string(r.parentID()) + "\x00" + r.name
		if r.team == nil && t.names[nameKey] == r.id {
			delete(t.names, nameKey)
		}
		t.order = slices.DeleteFunc(t.order, func(id ChildID) bool { return id == r.id })
	}
	t.mu.Unlock()
	if r.team != nil && !selected.resume && t.teamCanceled != nil {
		t.teamCanceled(r)
	}
	t.notify()
}

func (t *Tree) finish(r *record, run *run, raw Outcome) {
	t.lifecycleMu.Lock()
	after, _, _ := t.finishLocked(r, run, raw)
	t.lifecycleMu.Unlock()
	if after != nil {
		after()
	}
}

func (t *Tree) finishLocked(r *record, run *run, raw Outcome) (func(), Outcome, error) {
	if err := t.beginFinishLocked(r, run, raw); err != nil {
		return nil, Outcome{}, err
	}
	return t.finishBegun(r, run, true)
}

func (t *Tree) beginFinishLocked(r *record, run *run, raw Outcome) error {
	t.mu.Lock()
	if run.outcome != nil || run.finishing {
		t.mu.Unlock()
		return reject("unavailable", "child execution has already finished")
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
	run.finishing = true
	if r.run == run && r.previous == nil {
		r.latest = &raw
	}
	t.mu.Unlock()
	return nil
}

func (t *Tree) finishBegun(r *record, run *run, lifecycleHeld bool) (func(), Outcome, error) {
	storeErr := t.store(r)
	if !lifecycleHeld {
		t.lifecycleMu.Lock()
	}
	t.mu.Lock()
	raw := *run.outcome
	run.state, run.finishing = raw.State, false
	if run.started {
		run.admission = admissionReady
	}
	if !run.progress.activated.IsZero() {
		run.progress.settled = time.Now()
	}
	run.progress.activeTools = nil
	run.progress.toolsClipped = false
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
	t.signalRoomChangedLocked()
	if r.team != nil && t.teamFinished != nil {
		t.teamWG.Add(1)
	}
	close(run.done)
	deliver := r.team == nil && run.started && !t.closed && t.completion != nil
	if deliver {
		t.completionWG.Add(1)
	}
	t.mu.Unlock()
	if !lifecycleHeld {
		t.lifecycleMu.Unlock()
	}
	return func() {
		if r.team != nil && t.teamFinished != nil {
			err := t.teamFinished(r, run, raw)
			t.mu.Lock()
			run.teamFinishErr = err
			if run.teamFinishDone != nil {
				close(run.teamFinishDone)
			}
			t.mu.Unlock()
			if t.teamSettled != nil {
				t.teamSettled(r, run)
			}
			t.teamWG.Done()
		}
		t.notify()
		if deliver {
			go func() {
				defer t.completionWG.Done()
				t.completion(r, run, raw)
			}()
		}
	}, raw, storeErr
}

func (t *Tree) notify() error {
	t.mu.Lock()
	dirty := t.dirty
	t.dirty = map[ChildID]*record{}
	t.mu.Unlock()
	var failure error
	for _, r := range dirty {
		if err := t.store(r); err != nil && failure == nil {
			failure = err
		}
	}
	if t.changed != nil {
		t.changed()
	}
	return failure
}

func (t *Tree) store(r *record) error {
	if t.persist == nil {
		return nil
	}
	err := t.persist(r)
	if err == nil {
		return nil
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	description, _ := bounded("child metadata storage failed: "+err.Error(), 1024)
	r.run.deliveryError = description
	if r.latest != nil {
		previous := r.latest
		copy := *previous
		copy.Error = description
		if copy.State == Completed {
			copy.State = Partial
		}
		r.latest = &copy
		if r.run.outcome == previous {
			r.run.outcome = r.latest
		}
	}
	return err
}

func (t *Tree) snapshotsLocked(parent *record, activeOnly bool) []Snapshot {
	list := []Snapshot{}
	for _, id := range t.order {
		r := t.records[id]
		if r.team == nil && r.parent == parent && (!activeOnly || !r.paused && !r.run.state.terminal()) {
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
	if r.latest != nil && !r.run.finishing {
		copy := *r.latest
		outcome = &copy
	}
	var backend *BackendStatus
	if r.backend != "" || r.nativeOwnership != nil || r.nativeCleanupError != "" {
		state := "unallocated"
		if r.run.state.terminal() {
			state = "released"
		}
		if r.nativeOwnership != nil {
			state = "owned"
		}
		if r.nativeReleasing {
			state = "releasing"
		}
		if r.nativeCleanupError != "" {
			state = "cleanup_failed"
		}
		kind := r.backend
		if kind == "" && r.nativeOwnership != nil {
			kind = r.nativeOwnership.Backend
		}
		backend = &BackendStatus{Kind: string(kind), Ownership: state, Error: r.nativeCleanupError}
	}
	return Inspection{Child: t.snapshotLocked(r), Outcome: outcome, Backend: backend}, nil
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
	if selected.outcome != nil && !selected.finishing {
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
		t.mu.Lock()
		latest := selected.outcome
		if current := t.records[selected.ref.ID]; current != nil && current.run == selected && current.latest != nil {
			latest = current.latest
		}
		outcome := *latest
		t.mu.Unlock()
		return outcome, nil
	case <-timer.C:
		return Outcome{}, reject("wait_timeout", "wait timed out")
	case <-ctx.Done():
		return Outcome{}, reject("cancelled", "wait cancelled")
	}
}

func (t *Tree) resume(caller, selected *run, text string) (Snapshot, *run, error) {
	return t.resumeAs(controlAuthority{caller: caller}, selected, text)
}

func (t *Tree) resumeAs(authority controlAuthority, selected *run, text string) (Snapshot, *run, error) {
	t.mu.Lock()
	r, err := t.controlAsLocked(authority, selected)
	if err != nil {
		t.mu.Unlock()
		return Snapshot{}, nil, err
	}
	if r.nativeReservationConflict || r.nativeOwnership != nil || r.nativeReleasing {
		t.mu.Unlock()
		return Snapshot{}, nil, reject("unavailable", "native pane ownership requires reconciliation before resume")
	}
	for ancestor := r.parent; ancestor != nil && ancestor != t.root; ancestor = ancestor.parent {
		if ancestor.controlPhase == "closing" || ancestor.controlPhase == "terminated" || ancestor.controlPhase == "recovery_pending" {
			t.mu.Unlock()
			return Snapshot{}, nil, reject("unavailable", "teammate shutdown prevents child resume")
		}
	}
	if !r.run.state.terminal() || r.run.finishing {
		t.mu.Unlock()
		return Snapshot{}, nil, reject("rejected", "resume needs a settled child and text")
	}
	if r.team != nil {
		if r.controlPhase == "closing" || r.controlPhase == "terminated" || r.controlPhase == "recovery_pending" {
			t.mu.Unlock()
			return Snapshot{}, nil, reject("unavailable", "teammate shutdown requires recovery")
		}
		select {
		case <-r.run.teamFinishDone:
			if r.run.teamFinishErr != nil {
				t.mu.Unlock()
				return Snapshot{}, nil, reject("unavailable", "teammate settlement did not persist")
			}
		default:
			t.mu.Unlock()
			return Snapshot{}, nil, reject("unavailable", "teammate settlement is still pending")
		}
	}
	prompt := text
	if r.pendingTask != "" && r.backend != backendTmux {
		prompt = "Task:\n" + r.pendingTask + "\n\nResume instructions:\n" + text
	}
	defer t.mu.Unlock()
	if err := t.roomLocked(r.parentID(), !r.readOnly, r.cwd, r.id, r.team); err != nil {
		return Snapshot{}, nil, err
	}
	r.previous = r.run
	r.run = t.newRunLocked(Ref{r.id}, prompt)
	r.run.owner = r
	r.run.resumePaused = r.paused
	r.run.resume = true
	r.run.continuation = text
	r.paused = false
	if r.team == nil {
		t.pending[r.run] = &reservation{parent: r.parentID(), nonce: r.run.noticeID}
	}
	t.live++
	return t.snapshotLocked(r), r.run, nil
}

func (t *Tree) message(ctx context.Context, caller, selected *run, mode, text string) (Snapshot, *run, error) {
	return t.messageAs(ctx, controlAuthority{caller: caller}, selected, mode, text)
}

func (t *Tree) messageAs(ctx context.Context, authority controlAuthority, selected *run, mode, text string) (Snapshot, *run, error) {
	if ctx.Err() != nil {
		return Snapshot{}, nil, reject("cancelled", "message caller cancelled")
	}
	if len(text) > 16384 || len(strings.TrimSpace(text)) == 0 {
		return Snapshot{}, nil, reject("rejected", "invalid child message or message byte bound reached")
	}
	if mode == "resume" {
		return t.resumeAs(authority, selected, text)
	}
	if mode != "steer" && mode != "follow_up" {
		return Snapshot{}, nil, reject("rejected", "invalid child message")
	}
retry:
	t.mu.Lock()
	r, err := t.controlAsLocked(authority, selected)
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
	if _, err := t.controlAsLocked(authority, selected); err != nil || ctx.Err() != nil || r.paused || run.state != Starting && run.state != Running {
		t.mu.Unlock()
		return Snapshot{}, nil, reject("rejected", "child is no longer active")
	}
	if run.nativeFrozen {
		thaw := run.nativeThaw
		t.mu.Unlock()
		select {
		case <-thaw:
			goto retry
		case <-run.ctx.Done():
			return Snapshot{}, nil, reject("cancelled", "child stopped before message delivery")
		case <-ctx.Done():
			return Snapshot{}, nil, reject("cancelled", "message caller cancelled")
		}
	}
	if run.nativeInputErr != "" {
		err := reject("unavailable", run.nativeInputErr)
		t.mu.Unlock()
		return Snapshot{}, nil, err
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
	request := message{mode: mode, text: text, result: make(chan error, 1), ctx: ctx}
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
	return t.stopAs(controlAuthority{caller: caller}, selected)
}

func (t *Tree) stopAs(authority controlAuthority, selected *run) (*Outcome, int, error) {
	t.lifecycleMu.Lock()
	t.mu.Lock()
	target, err := t.controlAsLocked(authority, selected)
	if err != nil {
		t.mu.Unlock()
		t.lifecycleMu.Unlock()
		return nil, 0, err
	}
	if target.controlPhase == "terminated" {
		t.mu.Unlock()
		t.lifecycleMu.Unlock()
		return nil, 0, reject("unavailable", "teammate is terminated")
	}
	var settled *Outcome
	if selected.outcome != nil && target.latest != nil {
		copy := *target.latest
		settled = &copy
	}
	pending, fresh := t.pauseLocked(target)
	t.mu.Unlock()
	t.lifecycleMu.Unlock()
	if err := t.joinPaused(pending, fresh); err != nil {
		return nil, len(fresh), err
	}
	if settled != nil {
		return settled, len(fresh), nil
	}
	return selected.outcome, len(fresh), nil
}

func (t *Tree) pauseLocked(target *record) ([]*run, []*run) {
	target.paused = true
	t.signalRoomChangedLocked()
	var pending, fresh []*run
	for _, id := range t.order {
		r := t.records[id]
		if r != target && !t.belongsLocked(target, r) {
			continue
		}
		if r.team != nil && r.controlPhase == "terminated" {
			continue
		}
		r.paused = true
		t.dirty[r.id] = r
		run := r.run
		if run.state.terminal() {
			if r.team != nil {
				pending = append(pending, run)
			}
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

func (t *Tree) joinPaused(pending, fresh []*run) error {
	var failure error
	hasTeam := false
	for _, run := range pending {
		t.mu.Lock()
		isTeam := run.owner.team != nil
		t.mu.Unlock()
		if isTeam && t.teamPaused != nil {
			hasTeam = true
			if err := t.teamPaused(run.owner, run); err != nil && failure == nil {
				failure = err
			}
		}
	}
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
		t.mu.Lock()
		executorDone := run.executorDone
		t.mu.Unlock()
		if executorDone != nil {
			<-executorDone
		}
	}
	if err := t.notify(); err != nil && hasTeam && failure == nil {
		failure = err
	}
	return failure
}

func (t *Tree) pauseRoot() {
	t.lifecycleMu.Lock()
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		t.lifecycleMu.Unlock()
		return
	}
	pending, fresh := t.pauseLocked(t.root)
	t.mu.Unlock()
	t.lifecycleMu.Unlock()
	_ = t.joinPaused(pending, fresh)
}

func (t *Tree) close() {
	t.lifecycleMu.Lock()
	t.mu.Lock()
	if t.closed {
		done := t.closedDone
		t.mu.Unlock()
		t.lifecycleMu.Unlock()
		<-done
		return
	}
	t.closed = true
	var runs []*run
	for _, id := range t.order {
		run := t.records[id].run
		if !run.state.terminal() || run.finishing {
			if !run.state.terminal() {
				run.state = Stopping
			}
			runs = append(runs, run)
		}
	}
	t.mu.Unlock()
	t.lifecycleMu.Unlock()
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
	t.executeWG.Wait()
	t.completionWG.Wait()
	t.teamWG.Wait()
	t.peerWakeWG.Wait()
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
	if r := t.records[selected.ref.ID]; r != nil {
		t.dirty[r.id] = r
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
	if selected.reservation != nil && selected.reservation.stored {
		return
	}
	child := t.records[selected.ref.ID]
	if child == nil {
		return
	}
	target := child.parent
	if target == nil || target == t.root {
		target = child
	}
	description, _ := bounded("child completion delivery failed: "+failure.Error(), 1024)
	if !target.run.state.terminal() && target.previous == nil {
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
