package piglitter

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/gofrs/flock"
)

type PlanRequestID struct {
	Member   MemberRef `json:"member"`
	Sequence uint64    `json:"sequence"`
}

type planRequest struct {
	ID           PlanRequestID `json:"requestId"`
	SubmissionID string        `json:"submissionId"`
	Text         string        `json:"text"`
	Execution    string        `json:"execution"`
	TimeoutMs    int           `json:"timeoutMs"`
	CreatedAt    time.Time     `json:"createdAt"`
	Deadline     time.Time     `json:"deadline"`
	State        string        `json:"state"`
	Reason       string        `json:"reason,omitempty"`
}

type planLedger struct {
	Version   int           `json:"version"`
	Member    MemberRef     `json:"member"`
	HighWater uint64        `json:"highWater"`
	Clock     time.Time     `json:"clock"`
	Requests  []planRequest `json:"requests"`
}

type uncertainPlanWrite struct {
	lock   *flock.Flock
	digest [32]byte
}

const planRequestLimit = 24

func planPaths(dir string, ref MemberRef) (string, string) {
	name := fmt.Sprintf("plan-%s-%d", ref.ID, ref.Generation)
	return filepath.Join(dir, name+".lock"), filepath.Join(dir, name+".json")
}

func (s *teamStore) reconcilePlanLocked(ref MemberRef, pending *uncertainPlanWrite) error {
	if err := checkLockedFile(pending.lock); err != nil {
		return err
	}
	_, path := planPaths(s.dir, ref)
	data, err := readPeerFile(path)
	if err != nil {
		return err
	}
	if sha256.Sum256(data) != pending.digest {
		return errors.New("uncertain plan record identity changed")
	}
	return s.syncDir(s.dir)
}

func (s *teamStore) planTransaction(ref MemberRef, change func(*planLedger) (bool, *PlanRequestID, error)) error {
	var grantToInstall *PlanRequestID
	err := s.transact(func(roster *teamRoster) (bool, error) {
		found := false
		for _, member := range roster.Members {
			if member.Ref == ref && member.PlanRequired {
				found = true
				break
			}
		}
		if !found {
			return false, reject("unavailable", "plan-required teammate is unavailable")
		}
		lockPath, path := planPaths(s.dir, ref)
		var lock *flock.Flock
		initial := false
		if pending := s.planUncertain[ref]; pending != nil {
			if err := s.reconcilePlanLocked(ref, pending); err != nil {
				return false, err
			}
			if s.planDigests == nil {
				s.planDigests = map[MemberRef][32]byte{}
			}
			s.planDigests[ref] = pending.digest
			lock = pending.lock
			delete(s.planUncertain, ref)
		} else {
			_, err := os.Lstat(lockPath)
			initial = errors.Is(err, os.ErrNotExist)
			if err != nil && !initial {
				return false, err
			}
			if initial {
				if _, exists := s.planDigests[ref]; exists {
					return false, errors.New("plan metadata disappeared")
				}
				if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
					return false, errors.New("plan lock is missing for existing metadata")
				}
			}
			lock, err = lockTeamFile(lockPath, true)
			if err != nil {
				return false, err
			}
		}
		keep := false
		finish := func(err error) (bool, error) {
			if !keep {
				err = errors.Join(err, lock.Close())
			}
			return false, err
		}
		ledger := planLedger{Version: 1, Member: ref, Requests: []planRequest{}}
		if !initial {
			data, err := readPeerFile(path)
			if err != nil {
				return finish(err)
			}
			digest := sha256.Sum256(data)
			if expected, exists := s.planDigests[ref]; exists && expected != digest {
				return finish(errors.New("plan metadata changed outside its owner"))
			}
			if err := checkUniqueJSON(data); err != nil {
				return finish(err)
			}
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err := decoder.Decode(&ledger); err != nil {
				return finish(err)
			}
			if err := validatePlanLedger(ledger, ref); err != nil {
				return finish(err)
			}
			if s.planDigests == nil {
				s.planDigests = map[MemberRef][32]byte{}
			}
			s.planDigests[ref] = digest
		}
		changed, grant, actionErr := change(&ledger)
		if initial || changed {
			if err := validatePlanLedger(ledger, ref); err != nil {
				return finish(err)
			}
			data, err := json.Marshal(ledger)
			if err != nil || len(data) > teamJSONLimit {
				return finish(errors.New("plan metadata exceeds size limit"))
			}
			renamed, err := savePlanRecord(path, data, s.syncDir)
			if err != nil {
				if renamed {
					if s.planUncertain == nil {
						s.planUncertain = map[MemberRef]*uncertainPlanWrite{}
					}
					s.planUncertain[ref] = &uncertainPlanWrite{lock: lock, digest: sha256.Sum256(data)}
					keep = true
				}
				return finish(err)
			}
			if s.planDigests == nil {
				s.planDigests = map[MemberRef][32]byte{}
			}
			s.planDigests[ref] = sha256.Sum256(data)
		}
		if grant != nil && actionErr == nil {
			copy := *grant
			grantToInstall = &copy
		}
		return finish(actionErr)
	})
	if err != nil || grantToInstall == nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("team store is closed")
	}
	if err := checkLockedFile(s.lead); err != nil {
		return err
	}
	if s.planGrants == nil {
		s.planGrants = map[MemberRef]PlanRequestID{}
	}
	s.planGrants[ref] = *grantToInstall
	return nil
}

func savePlanRecord(path string, data []byte, syncDirectory func(string) error) (bool, error) {
	file, err := os.CreateTemp(filepath.Dir(path), ".plan-*.tmp")
	if err != nil {
		return false, err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err := file.Write(data); err != nil {
		return false, errors.Join(err, file.Close())
	}
	if err := errors.Join(file.Sync(), file.Close()); err != nil {
		return false, err
	}
	if err := os.Rename(name, path); err != nil {
		return false, err
	}
	return true, syncDirectory(filepath.Dir(path))
}

func validatePlanLedger(ledger planLedger, ref MemberRef) error {
	if ledger.Version != 1 || ledger.Member != ref || ledger.Requests == nil || len(ledger.Requests) > planRequestLimit {
		return errors.New("invalid plan metadata")
	}
	seen := map[string]bool{}
	for index, request := range ledger.Requests {
		if request.ID.Member != ref || request.ID.Sequence != uint64(index+1) || request.ID.Sequence > ledger.HighWater || seen[request.SubmissionID] || !peerMessageIDPattern.MatchString(request.SubmissionID) || request.Execution == "" || strings.TrimSpace(request.Text) == "" || len(request.Text) > 4096 || !utf8.ValidString(request.Text) || request.TimeoutMs < 1000 || request.TimeoutMs > 3600000 || request.CreatedAt.IsZero() || !request.Deadline.Equal(request.CreatedAt.Add(time.Duration(request.TimeoutMs)*time.Millisecond)) {
			return errors.New("invalid plan request")
		}
		switch request.State {
		case "pending", "approved", "expired", "cancelled":
			if request.Reason != "" {
				return errors.New("unexpected plan decision reason")
			}
		case "rejected":
			if strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 4096 || !utf8.ValidString(request.Reason) {
				return errors.New("invalid plan rejection")
			}
		default:
			return errors.New("invalid plan state")
		}
		seen[request.SubmissionID] = true
	}
	if ledger.HighWater != uint64(len(ledger.Requests)) {
		return errors.New("invalid plan allocation watermark")
	}
	return nil
}

func expirePlans(ledger *planLedger, now time.Time) bool {
	changed := false
	backward := !ledger.Clock.IsZero() && now.Before(ledger.Clock)
	for index := range ledger.Requests {
		request := &ledger.Requests[index]
		if request.State == "pending" && (backward || !now.Before(request.Deadline)) {
			request.State = "expired"
			changed = true
		}
	}
	if now.After(ledger.Clock) {
		ledger.Clock = now
		changed = true
	}
	return changed
}

func (s *teamStore) submitPlan(ref MemberRef, execution, submission, text string, timeoutMs int) (planRequest, error) {
	var result planRequest
	if !peerMessageIDPattern.MatchString(submission) || strings.TrimSpace(text) == "" || len(text) > 4096 || !utf8.ValidString(text) || timeoutMs < 1000 || timeoutMs > 3600000 {
		return result, reject("rejected", "invalid plan submission")
	}
	created := false
	var timerDeadline time.Time
	err := s.planTransaction(ref, func(ledger *planLedger) (bool, *PlanRequestID, error) {
		current := time.Now()
		now := current.UTC()
		changed := expirePlans(ledger, now)
		for _, request := range ledger.Requests {
			if request.SubmissionID == submission {
				if request.Text != text || request.TimeoutMs != timeoutMs || request.Execution != execution {
					return changed, nil, reject("rejected", "plan submission ID conflicts with an existing request")
				}
				result = request
				return changed, nil, nil
			}
			if request.State == "pending" {
				return changed, nil, reject("rejected", "a plan request is already pending")
			}
		}
		if _, granted := s.planGrants[ref]; granted {
			return changed, nil, reject("rejected", "the current owner already approved this teammate")
		}
		if len(ledger.Requests) == planRequestLimit || ledger.HighWater == math.MaxUint64 {
			return changed, nil, reject("capacity", "plan history is full")
		}
		ledger.HighWater++
		result = planRequest{ID: PlanRequestID{Member: ref, Sequence: ledger.HighWater}, SubmissionID: submission, Text: text, Execution: execution, TimeoutMs: timeoutMs, CreatedAt: now, Deadline: now.Add(time.Duration(timeoutMs) * time.Millisecond), State: "pending"}
		ledger.Requests = append(ledger.Requests, result)
		timerDeadline = current.Add(time.Duration(timeoutMs) * time.Millisecond)
		created = true
		return true, nil, nil
	})
	if err == nil && created {
		time.AfterFunc(time.Until(timerDeadline), func() { _ = s.expirePlan(result.ID) })
	}
	return result, err
}

func (s *teamStore) expirePlan(id PlanRequestID) error {
	return s.planTransaction(id.Member, func(ledger *planLedger) (bool, *PlanRequestID, error) {
		if id.Sequence == 0 || id.Sequence > uint64(len(ledger.Requests)) {
			return false, nil, nil
		}
		request := &ledger.Requests[id.Sequence-1]
		if request.ID != id || request.State != "pending" {
			return false, nil, nil
		}
		request.State = "expired"
		return true, nil, nil
	})
}

func (s *teamStore) readPlans(ref MemberRef) ([]planRequest, error) {
	var result []planRequest
	err := s.planTransaction(ref, func(ledger *planLedger) (bool, *PlanRequestID, error) {
		changed := expirePlans(ledger, time.Now().UTC())
		result = append([]planRequest(nil), ledger.Requests...)
		return changed, nil, nil
	})
	return result, err
}

func (s *teamStore) decidePlan(id PlanRequestID, execution, decision, reason string) (planRequest, error) {
	var result planRequest
	if id.Sequence == 0 || decision != "approve" && decision != "reject" || decision == "approve" && reason != "" || decision == "reject" && (strings.TrimSpace(reason) == "" || len(reason) > 4096 || !utf8.ValidString(reason)) {
		return result, reject("rejected", "invalid plan decision")
	}
	err := s.planTransaction(id.Member, func(ledger *planLedger) (bool, *PlanRequestID, error) {
		changed := expirePlans(ledger, time.Now().UTC())
		if id.Sequence > uint64(len(ledger.Requests)) {
			return changed, nil, reject("rejected", "plan request is unavailable")
		}
		request := &ledger.Requests[id.Sequence-1]
		result = *request
		if request.ID != id || request.State != "pending" || request.Execution != execution {
			return changed, nil, reject("rejected", "plan decision is stale or replayed")
		}
		if decision == "approve" {
			request.State = "approved"
			result = *request
			return true, &id, nil
		}
		request.State, request.Reason = "rejected", reason
		result = *request
		return true, nil, nil
	})
	return result, err
}

func (s *teamStore) cancelPlan(id PlanRequestID) (planRequest, error) {
	var result planRequest
	err := s.planTransaction(id.Member, func(ledger *planLedger) (bool, *PlanRequestID, error) {
		changed := expirePlans(ledger, time.Now().UTC())
		if id.Sequence == 0 || id.Sequence > uint64(len(ledger.Requests)) {
			return changed, nil, reject("rejected", "plan request is unavailable")
		}
		request := &ledger.Requests[id.Sequence-1]
		result = *request
		if request.ID != id || request.State != "pending" {
			return changed, nil, reject("rejected", "plan request is no longer pending")
		}
		request.State = "cancelled"
		result = *request
		return true, nil, nil
	})
	return result, err
}

func (s *teamStore) recoverPlans(ref MemberRef) error {
	return s.planTransaction(ref, func(ledger *planLedger) (bool, *PlanRequestID, error) {
		changed := false
		for index := range ledger.Requests {
			if ledger.Requests[index].State == "pending" {
				ledger.Requests[index].State = "cancelled"
				changed = true
			}
		}
		delete(s.planGrants, ref)
		return changed, nil, nil
	})
}

func (s *teamStore) planGranted(ref MemberRef) (bool, error) {
	granted := false
	err := s.planTransaction(ref, func(*planLedger) (bool, *PlanRequestID, error) {
		_, granted = s.planGrants[ref]
		return false, nil, nil
	})
	return granted, err
}

func (o *owner) planHook(r *record, selected *run) agent.BeforeToolCallHook {
	return func(_ context.Context, id, name string, raw json.RawMessage) agent.ToolCallHookResult {
		return o.beforePlanTool(r, selected, id, name, raw)
	}
}

func (o *owner) beforePlanTool(r *record, selected *run, id, name string, raw json.RawMessage) agent.ToolCallHookResult {
	if err := o.current(); err != nil {
		return agent.ToolCallHookResult{Block: true, Reason: err.Error()}
	}
	o.tree.mu.Lock()
	current, err := o.tree.currentLocked(selected)
	if err != nil || current != r || r.paused {
		o.tree.mu.Unlock()
		return agent.ToolCallHookResult{Block: true, Reason: "the child execution is no longer current"}
	}
	var member *teamMember
	for ancestor := r; ancestor != nil && ancestor != o.tree.root; ancestor = ancestor.parent {
		if o.tree.records[ancestor.id] != ancestor {
			o.tree.mu.Unlock()
			return agent.ToolCallHookResult{Block: true, Reason: "teammate ancestry is unavailable"}
		}
		if ancestor.team != nil {
			copy := *ancestor.team
			member = &copy
			break
		}
	}
	o.tree.mu.Unlock()
	if member == nil || !member.PlanRequired {
		return agent.ToolCallHookResult{}
	}
	o.teamMu.Lock()
	store := o.teamStore
	o.teamMu.Unlock()
	if store == nil {
		return agent.ToolCallHookResult{Block: true, Reason: "the teammate plan store is unavailable"}
	}
	granted, err := store.planGranted(member.Ref)
	if err != nil {
		return agent.ToolCallHookResult{Block: true, Reason: "the teammate plan gate is unavailable: " + err.Error()}
	}
	if granted || planReadTool(name, raw) {
		return agent.ToolCallHookResult{}
	}
	selected.mu.Lock()
	if selected.planDenials == nil {
		selected.planDenials = map[planDenial]bool{}
	}
	selected.planDenials[planDenial{ID: id, Name: name}] = true
	selected.mu.Unlock()
	return agent.ToolCallHookResult{Block: true, Reason: "plan approval is required before this tool can run"}
}

func planReadTool(name string, raw json.RawMessage) bool {
	switch name {
	case "read", "ls", "team_list", "team_task_get", "team_task_list", "litter_list", "litter_inspect":
		return true
	case "team_plan":
		command, err := parsePlanCommand(raw)
		return err == nil && command.Action != "decide"
	default:
		return false
	}
}

func consumePlanDenial(selected *run, id, name string) bool {
	selected.mu.Lock()
	defer selected.mu.Unlock()
	key := planDenial{ID: id, Name: name}
	denied := selected.planDenials[key]
	delete(selected.planDenials, key)
	return denied
}
