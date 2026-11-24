package piglitter

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
)

type viewHealth struct {
	Status string
	Error  string
}

type teamExecutionView struct {
	Child     Snapshot
	Admission string
	Progress  runProgress
	Tools     []string
	Latest    *Outcome
}

type teamMemberView struct {
	Member    teamMember
	Execution *teamExecutionView
}

type transcriptEntry struct {
	Role string
	Text string
}

type teamTranscript struct {
	Member    MemberRef
	Admission string
	Request   uint64
	Health    viewHealth
	Entries   []transcriptEntry
	Clipped   bool
}

type historyIdentity struct {
	record *record
	run    *run
	log    *coding.SessionManager
	native *memberBinding
}

type teamViewFrame struct {
	Team             *TeamID
	Members          []teamMemberView
	Reservations     []teamMember
	Tasks            []teamTaskView
	Plans            []planView
	Shutdowns        []shutdownView
	Assigned         map[MemberRef][]int
	RosterHealth     viewHealth
	TaskHealth       viewHealth
	PlanHealth       viewHealth
	ShutdownHealth   viewHealth
	PreferenceHealth viewHealth
	Preference       *MemberRef
	PreferenceLoaded bool
	Transcript       teamTranscript
}

type teamView struct {
	owner        *owner
	ctx          context.Context
	cancel       context.CancelFunc
	invalidate   func()
	mu           sync.Mutex
	frame        *teamViewFrame
	selected     *MemberRef
	request      uint64
	inspect      MemberRef
	wake         chan struct{}
	history      chan struct{}
	controls     chan struct{}
	watching     teamControlSection
	controlEpoch uint64
	joined       chan struct{}
	callbacks    sync.WaitGroup
	closed       bool
}

func newTeamView(ctx context.Context, owner *owner, invalidate func()) *teamView {
	ctx, cancel := context.WithCancel(ctx)
	v := &teamView{owner: owner, ctx: ctx, cancel: cancel, invalidate: invalidate, frame: &teamViewFrame{RosterHealth: viewHealth{Status: "loading"}, TaskHealth: viewHealth{Status: "loading"}, PlanHealth: viewHealth{Status: "loading"}, ShutdownHealth: viewHealth{Status: "loading"}}, wake: make(chan struct{}, 1), history: make(chan struct{}, 1), controls: make(chan struct{}, 1), joined: make(chan struct{})}
	go v.collect()
	go v.inspectLoop()
	go v.controlLoop()
	return v
}

func (v *teamView) Snapshot() *teamViewFrame {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.frame
}

func (v *teamView) Select(ref MemberRef) {
	v.mu.Lock()
	if !v.closed {
		v.selected = &ref
	}
	v.mu.Unlock()
	select {
	case v.wake <- struct{}{}:
	default:
	}
}

func (v *teamView) Inspect(ref MemberRef) uint64 {
	v.mu.Lock()
	v.request++
	request := v.request
	v.inspect = ref
	frame := *v.frame
	frame.Transcript = teamTranscript{Member: ref, Request: request, Health: viewHealth{Status: "loading"}}
	v.frame = &frame
	v.mu.Unlock()
	select {
	case v.history <- struct{}{}:
	default:
	}
	return request
}

func (v *teamView) Close() {
	v.mu.Lock()
	v.closed = true
	v.mu.Unlock()
	v.cancel()
	<-v.joined
	v.callbacks.Wait()
}

func (v *teamView) beginAction() (ownerAction, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.closed || v.ctx.Err() != nil {
		return ownerAction{}, reject("unavailable", "the team view closed")
	}
	return v.owner.beginAction(v.ctx)
}

func (v *teamView) notify() {
	v.mu.Lock()
	if v.closed || v.ctx.Err() != nil {
		v.mu.Unlock()
		return
	}
	v.callbacks.Add(1)
	v.mu.Unlock()
	defer v.callbacks.Done()
	v.invalidate()
}

func (v *teamView) publish(frame *teamViewFrame) {
	v.mu.Lock()
	v.owner.state.mu.Lock()
	active := v.owner.state.owner == v.owner && v.owner.state.epoch == v.owner.epoch
	if !active || v.closed || v.ctx.Err() != nil {
		v.owner.state.mu.Unlock()
		v.mu.Unlock()
		return
	}
	frame.Transcript = v.frame.Transcript
	if frame.Team != nil && v.frame.Team != nil && *frame.Team == *v.frame.Team {
		frame.Plans, frame.PlanHealth = v.frame.Plans, v.frame.PlanHealth
		frame.Shutdowns, frame.ShutdownHealth = v.frame.Shutdowns, v.frame.ShutdownHealth
	}
	v.frame = frame
	v.owner.state.mu.Unlock()
	v.mu.Unlock()
	v.notify()
}

func viewFailure(old viewHealth, err error) viewHealth {
	status := "unavailable"
	if old.Status == "fresh" || old.Status == "stale" {
		status = "stale"
	}
	return viewHealth{Status: status, Error: err.Error()}
}

func (v *teamView) collect() {
	defer close(v.joined)
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	var lastSaved *MemberRef
	for {
		v.refresh(&lastSaved)
		v.queueControls()
		select {
		case <-v.ctx.Done():
			v.saveSelection(&lastSaved)
			return
		case <-v.wake:
		case <-ticker.C:
		}
	}
}

func (v *teamView) refresh(lastSaved **MemberRef) {
	action, err := v.owner.beginAction(v.ctx)
	if err != nil {
		return
	}
	defer action.finish()
	if action.ctx.Err() != nil {
		return
	}
	v.owner.teamMu.Lock()
	store := v.owner.teamStore
	v.owner.teamMu.Unlock()
	previous := v.Snapshot()
	frame := *previous
	if store == nil {
		frame = teamViewFrame{RosterHealth: viewHealth{Status: "fresh"}, TaskHealth: viewHealth{Status: "fresh"}, PreferenceLoaded: true}
		v.publish(&frame)
		return
	}
	team, members, pending, err := store.SnapshotWithReservations()
	if err != nil {
		frame.RosterHealth = viewFailure(frame.RosterHealth, err)
		frame.TaskHealth = viewFailure(frame.TaskHealth, fmt.Errorf("roster unavailable: %w", err))
		v.publish(&frame)
		return
	}
	if frame.Team == nil || *frame.Team != team {
		frame = teamViewFrame{Team: &team, TaskHealth: viewHealth{Status: "loading"}, PlanHealth: viewHealth{Status: "loading"}, ShutdownHealth: viewHealth{Status: "loading"}}
		*lastSaved = nil
	}
	frame.Team = &team
	frame.RosterHealth = viewHealth{Status: "fresh"}
	frame.Reservations = pending
	frame.Members = make([]teamMemberView, len(members))
	v.owner.tree.mu.Lock()
	for i, member := range members {
		view := teamMemberView{Member: member}
		r := v.owner.tree.records[member.Ref.ID]
		if r != nil && r.team != nil && r.team.Ref == member.Ref && r.team.AdmissionID == member.AdmissionID && r.run != nil {
			execution := &teamExecutionView{Child: v.owner.tree.snapshotLocked(r), Admission: "ready", Progress: r.run.progress}
			if r.run.admission != admissionReady {
				execution.Admission = "pending"
			}
			for _, tool := range r.run.progress.activeTools {
				execution.Tools = append(execution.Tools, tool)
			}
			slices.Sort(execution.Tools)
			execution.Progress.activeTools = nil
			if r.latest != nil && !r.run.finishing {
				outcome := *r.latest
				execution.Latest = &outcome
			}
			view.Execution = execution
		}
		frame.Members[i] = view
	}
	v.owner.tree.mu.Unlock()
	tasks, err := store.listTasks()
	if err != nil {
		frame.TaskHealth = viewFailure(frame.TaskHealth, err)
	} else {
		frame.Tasks = tasks
		frame.Assigned = make(map[MemberRef][]int)
		for i, task := range tasks {
			if task.Owner != nil {
				frame.Assigned[*task.Owner] = append(frame.Assigned[*task.Owner], i)
			}
		}
		frame.TaskHealth = viewHealth{Status: "fresh"}
	}
	if !frame.PreferenceLoaded {
		preference, err := readTeamSelection(store.dir, team, members)
		frame.Preference, frame.PreferenceLoaded = preference, true
		if err != nil {
			frame.PreferenceHealth = viewHealth{Status: "unavailable", Error: err.Error()}
		} else {
			frame.PreferenceHealth = viewHealth{Status: "fresh"}
			if preference != nil {
				copy := *preference
				*lastSaved = &copy
			}
		}
	}
	v.publish(&frame)
	v.saveSelection(lastSaved)
}

func (v *teamView) saveSelection(lastSaved **MemberRef) {
	v.mu.Lock()
	selected := v.selected
	frame := v.frame
	v.mu.Unlock()
	if selected == nil || frame.Team == nil || selected.Team != *frame.Team || (*lastSaved != nil && **lastSaved == *selected) {
		return
	}
	action, err := v.owner.beginAction(context.Background())
	if err != nil {
		return
	}
	defer action.finish()
	v.owner.teamMu.Lock()
	store := v.owner.teamStore
	v.owner.teamMu.Unlock()
	if store == nil {
		return
	}
	err = writeTeamSelection(store.dir, *selected)
	v.mu.Lock()
	v.owner.state.mu.Lock()
	if v.owner.state.owner == v.owner && v.owner.state.epoch == v.owner.epoch && !v.closed {
		updated := *v.frame
		if err != nil {
			updated.PreferenceHealth = viewHealth{Status: "unavailable", Error: err.Error()}
		} else {
			updated.PreferenceHealth = viewHealth{Status: "fresh"}
		}
		v.frame = &updated
	}
	v.owner.state.mu.Unlock()
	v.mu.Unlock()
	if err == nil {
		copy := *selected
		*lastSaved = &copy
	}
}

func readTeamSelection(dir string, team TeamID, members []teamMember) (*MemberRef, error) {
	path := filepath.Join(dir, "view-selection.json")
	if err := checkMetadataFile(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(before, opened) {
		return nil, errors.New("team selection preference was replaced")
	}
	data, err := io.ReadAll(io.LimitReader(file, 1025))
	if err != nil {
		return nil, err
	}
	var ref MemberRef
	if len(data) > 1024 || json.Unmarshal(data, &ref) != nil || ref.Team != team {
		return nil, errors.New("invalid team selection preference")
	}
	if !slices.ContainsFunc(members, func(member teamMember) bool { return member.Ref == ref }) {
		return nil, nil
	}
	return &ref, nil
}

func writeTeamSelection(dir string, ref MemberRef) error {
	data, err := json.Marshal(ref)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".view-selection-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if _, err := file.Write(data); err != nil {
		return errors.Join(err, file.Close())
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, "view-selection.json")); err != nil {
		return err
	}
	return syncDir(dir)
}

func (v *teamView) inspectLoop() {
	for {
		select {
		case <-v.ctx.Done():
			return
		case <-v.history:
		}
		v.mu.Lock()
		ref, request := v.inspect, v.request
		v.mu.Unlock()
		result, identity := v.readTranscript(ref, request)
		v.owner.tree.mu.Lock()
		v.mu.Lock()
		v.owner.state.mu.Lock()
		active := v.owner.state.owner == v.owner && v.owner.state.epoch == v.owner.epoch
		present := slices.ContainsFunc(v.frame.Members, func(member teamMemberView) bool {
			return member.Member.Ref == ref && (result.Admission == "" || member.Member.AdmissionID == result.Admission)
		})
		if identity.record != nil && (v.owner.tree.records[ref.ID] != identity.record || identity.record.team == nil || identity.record.team.Ref != ref || identity.record.team.AdmissionID != result.Admission || identity.record.run != identity.run || identity.record.activeLog != identity.log || identity.record.native != identity.native || identity.native != nil && (identity.record.nativeOwnership != identity.native.ownership || !sameMemberOwnership(identity.native.ownership, identity.native.owned))) {
			result.Entries = nil
			result.Clipped = false
			result.Health = viewHealth{Status: "unavailable", Error: "member execution changed during inspection"}
		}
		publish := active && !v.closed && v.ctx.Err() == nil && v.request == request && v.inspect == ref && present
		if publish {
			frame := *v.frame
			frame.Transcript = result
			v.frame = &frame
		}
		v.owner.state.mu.Unlock()
		v.mu.Unlock()
		v.owner.tree.mu.Unlock()
		if publish {
			v.notify()
		}
	}
}

func (v *teamView) readTranscript(ref MemberRef, request uint64) (teamTranscript, historyIdentity) {
	result := teamTranscript{Member: ref, Request: request, Health: viewHealth{Status: "unavailable"}}
	var identity historyIdentity
	action, err := v.beginAction()
	if err != nil {
		result.Health.Error = err.Error()
		return result, identity
	}
	defer action.finish()
	if err := action.ctx.Err(); err != nil {
		result.Health.Error = err.Error()
		return result, identity
	}
	frame := v.Snapshot()
	index := slices.IndexFunc(frame.Members, func(member teamMemberView) bool { return member.Member.Ref == ref })
	if index < 0 {
		result.Health.Error = "member is unavailable"
		return result, identity
	}
	member := frame.Members[index].Member
	result.Admission = member.AdmissionID
	tree := v.owner.tree
	tree.mu.Lock()
	r := tree.records[ref.ID]
	if r == nil || r.team == nil || r.team.Ref != ref || r.team.AdmissionID != member.AdmissionID || r.run == nil {
		tree.mu.Unlock()
		result.Health.Error = "matching retained execution is unavailable"
		return result, identity
	}
	identity = historyIdentity{record: r, run: r.run, log: r.activeLog, native: r.native}
	tree.mu.Unlock()
	var entries []transcriptEntry
	var clipped bool
	if identity.native != nil {
		var snapshot memberSnapshot
		snapshot, err = identity.native.control.Snapshot(action.ctx, memberHistoryBound{entries: 200, bytes: 64 * 1024})
		if err == nil && (snapshot.session != ref.ID || len(snapshot.entries) > 200) {
			err = errors.New("native transcript identity or entry bound is invalid")
		}
		if err == nil {
			bytes := 0
			for _, entry := range snapshot.entries {
				bytes += len(entry)
			}
			if bytes > 128*1024 {
				err = errors.New("native transcript exceeds its response bound")
			} else {
				entries, clipped = transcriptEntries(snapshot.entries)
				clipped = clipped || snapshot.clipped
			}
		}
	} else if identity.log == nil {
		entries, clipped, err = v.privateHistory(action.ctx, r, member)
	} else {
		entries, clipped = transcriptEntries(identity.log.GetBranch())
	}
	if err != nil {
		result.Health.Error = err.Error()
		return result, identity
	}
	result.Entries, result.Clipped = entries, clipped
	result.Health = viewHealth{Status: "fresh"}
	return result, identity
}

func (v *teamView) privateHistory(ctx context.Context, r *record, member teamMember) ([]transcriptEntry, bool, error) {
	for !r.deliveryMu.TryLock() {
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, false, ctx.Err()
		case <-timer.C:
		}
	}
	defer r.deliveryMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	root := filepath.Join(v.owner.agentDir, "sessions")
	var candidates []string
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if cancelErr := ctx.Err(); cancelErr != nil {
			return cancelErr
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() && strings.HasSuffix(entry.Name(), "_"+string(member.Ref.ID)+".jsonl") {
			candidates = append(candidates, path)
		}
		return nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("find retained history: %w", err)
	}
	if len(candidates) != 1 {
		return nil, false, fmt.Errorf("retained history has %d matching files", len(candidates))
	}
	dir, err := os.MkdirTemp("", "pig-team-history-")
	if err != nil {
		return nil, false, err
	}
	defer os.RemoveAll(dir)
	copyPath := filepath.Join(dir, "history.jsonl")
	if err := copyHistory(ctx, candidates[0], copyPath, member); err != nil {
		return nil, false, err
	}
	trusted := true
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: member.Workspace.Cwd, AgentDir: v.owner.agentDir, ProjectTrusted: &trusted})
	if err != nil {
		return nil, false, err
	}
	defer services.Close()
	log, err := coding.SessionManagerFor(services, coding.SessionStartOptions{ResumePath: copyPath, SessionDir: dir})
	if err != nil {
		return nil, false, err
	}
	if log.ID() != string(member.Ref.ID) || log.GetCwd() != member.Workspace.Cwd {
		return nil, false, errors.New("retained history identity mismatch")
	}
	entries, clipped := transcriptEntries(log.GetBranch())
	return entries, clipped, nil
}

func copyHistory(ctx context.Context, source, destination string, member teamMember) error {
	before, err := os.Lstat(source)
	if err != nil || !before.Mode().IsRegular() {
		return errors.New("retained history is not a regular file")
	}
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	opened, err := in.Stat()
	if err != nil || !os.SameFile(before, opened) {
		return errors.New("retained history identity changed")
	}
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	hash := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, hash), contextReader{ctx: ctx, r: in})
	if err := errors.Join(copyErr, out.Close()); err != nil {
		return err
	}
	after, err := os.Lstat(source)
	if err != nil || !os.SameFile(before, after) || before.Size() != after.Size() || !before.ModTime().Equal(after.ModTime()) {
		return errors.New("retained history changed during copying")
	}
	if _, err := in.Seek(0, io.SeekStart); err != nil {
		return err
	}
	check := sha256.New()
	if _, err := io.Copy(check, contextReader{ctx: ctx, r: in}); err != nil || !bytes.Equal(hash.Sum(nil), check.Sum(nil)) {
		return errors.New("retained history changed during verification")
	}
	verified, err := os.Lstat(source)
	if err != nil || !os.SameFile(before, verified) || before.Size() != verified.Size() || !before.ModTime().Equal(verified.ModTime()) {
		return errors.New("retained history changed during verification")
	}
	file, err := os.Open(destination)
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(bufio.NewReader(contextReader{ctx: ctx, r: file}))
	var header struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Cwd  string `json:"cwd"`
	}
	if err := decoder.Decode(&header); err != nil || header.Type != "session" || header.ID != string(member.Ref.ID) || header.Cwd != member.Workspace.Cwd {
		return errors.New("retained history header does not match the member")
	}
	for {
		var entry json.RawMessage
		if err := decoder.Decode(&entry); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return fmt.Errorf("retained history is incomplete: %w", err)
		}
	}
}

func transcriptEntries[T any](entries []T) ([]transcriptEntry, bool) {
	const maxEntries, maxBytes = 200, 64 * 1024
	result := make([]transcriptEntry, 0, min(len(entries), maxEntries))
	clipped, used := false, 0
	for _, entry := range entries {
		data, err := json.Marshal(entry)
		if err != nil {
			continue
		}
		var item struct {
			Type    string `json:"type"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(data, &item) != nil || item.Type != "message" || (item.Message.Role != "user" && item.Message.Role != "assistant") {
			continue
		}
		var text string
		if json.Unmarshal(item.Message.Content, &text) != nil {
			var parts []struct{ Type, Text string }
			if json.Unmarshal(item.Message.Content, &parts) != nil {
				continue
			}
			var texts []string
			for _, part := range parts {
				if part.Type == "text" {
					texts = append(texts, part.Text)
				}
			}
			text = strings.Join(texts, "\n")
		}
		if text == "" {
			continue
		}
		if len(result) >= maxEntries || used >= maxBytes {
			clipped = true
			break
		}
		text, truncated := bounded(text, maxBytes-used)
		clipped = clipped || truncated
		used += len(text)
		result = append(result, transcriptEntry{Role: item.Message.Role, Text: text})
	}
	return result, clipped
}
