package piglitter

import (
	"errors"
	"math"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

type ShutdownRequestID struct {
	Member   MemberRef `json:"member"`
	Sequence uint64    `json:"sequence"`
	Nonce    string    `json:"nonce"`
}

type shutdownRequest struct {
	ID           ShutdownRequestID `json:"requestId"`
	SubmissionID string            `json:"submissionId"`
	Reason       string            `json:"reason"`
	TimeoutMs    int               `json:"timeoutMs"`
	CreatedAt    time.Time         `json:"createdAt"`
	Deadline     time.Time         `json:"deadline"`
	State        string            `json:"state"`
	Response     string            `json:"response,omitempty"`
	Execution    string            `json:"execution,omitempty"`
}

type controlNotice struct {
	ID            string            `json:"id"`
	Request       ShutdownRequestID `json:"requestId"`
	Recipient     string            `json:"recipient"`
	Kind          string            `json:"kind"`
	Text          string            `json:"text"`
	Delivery      string            `json:"delivery"`
	NativeEntryID string            `json:"nativeEntryId,omitempty"`
}

type memberLifecycle struct {
	HighWater uint64             `json:"highWater"`
	Clock     time.Time          `json:"clock"`
	Phase     string             `json:"phase"`
	Active    *ShutdownRequestID `json:"active,omitempty"`
	Requests  []shutdownRequest  `json:"requests"`
	Notices   []controlNotice    `json:"notices"`
	Turns     []turnNotice       `json:"turns,omitempty"`
}

const shutdownRequestLimit = 24

func validateMemberLifecycle(lifecycle memberLifecycle, ref MemberRef) error {
	if lifecycle.Requests == nil || lifecycle.Notices == nil || len(lifecycle.Requests) > shutdownRequestLimit || len(lifecycle.Notices) > shutdownRequestLimit*3 || lifecycle.HighWater != uint64(len(lifecycle.Requests)) {
		return errors.New("invalid teammate lifecycle metadata")
	}
	switch lifecycle.Phase {
	case "open":
		if lifecycle.Active != nil {
			return errors.New("open teammate has an active shutdown")
		}
	case "closing", "terminated", "recovery_pending":
		if lifecycle.Active == nil || lifecycle.Active.Member != ref {
			return errors.New("teammate lifecycle lacks its shutdown identity")
		}
	default:
		return errors.New("invalid teammate lifecycle phase")
	}
	seen := map[string]bool{}
	for index, request := range lifecycle.Requests {
		if request.ID.Member != ref || request.ID.Sequence != uint64(index+1) || !validPlanNonce(request.ID.Nonce) || !peerMessageIDPattern.MatchString(request.SubmissionID) || seen[request.SubmissionID] || strings.TrimSpace(request.Reason) == "" || len(request.Reason) > 4096 || !utf8.ValidString(request.Reason) || request.TimeoutMs < 1000 || request.TimeoutMs > 3600000 || request.CreatedAt.IsZero() || !request.Deadline.Equal(request.CreatedAt.Add(time.Duration(request.TimeoutMs)*time.Millisecond)) {
			return errors.New("invalid shutdown request")
		}
		switch request.State {
		case "pending", "cancelled", "expired":
			if request.Response != "" || request.Execution != "" {
				return errors.New("unexpected shutdown response")
			}
		case "rejected":
			if strings.TrimSpace(request.Response) == "" || len(request.Response) > 4096 || !utf8.ValidString(request.Response) || request.Execution == "" {
				return errors.New("invalid shutdown refusal")
			}
		case "approved", "terminated", "interrupted":
			if request.Response != "" || request.Execution == "" {
				return errors.New("invalid shutdown approval")
			}
		default:
			return errors.New("invalid shutdown request state")
		}
		seen[request.SubmissionID] = true
	}
	if lifecycle.Active != nil {
		id := *lifecycle.Active
		if id.Sequence == 0 || id.Sequence > uint64(len(lifecycle.Requests)) || lifecycle.Requests[id.Sequence-1].ID != id {
			return errors.New("active shutdown does not identify a retained request")
		}
		state := lifecycle.Requests[id.Sequence-1].State
		if lifecycle.Phase == "closing" && state != "approved" || lifecycle.Phase == "terminated" && state != "terminated" || lifecycle.Phase == "recovery_pending" && state != "interrupted" {
			return errors.New("shutdown phase conflicts with request state")
		}
	}
	notices := map[string]bool{}
	for _, notice := range lifecycle.Notices {
		if notice.ID == "" || notices[notice.ID] || notice.Request.Member != ref || notice.Request.Sequence == 0 || notice.Request.Sequence > uint64(len(lifecycle.Requests)) || lifecycle.Requests[notice.Request.Sequence-1].ID != notice.Request || notice.Text == "" || len(notice.Text) > 8192 || !utf8.ValidString(notice.Text) || notice.Recipient != "lead" && notice.Recipient != "member" {
			return errors.New("invalid teammate control notice")
		}
		switch notice.Delivery {
		case "pending", "attempted", "ambiguous":
			if notice.NativeEntryID != "" {
				return errors.New("unobserved control notice has an entry ID")
			}
		case "native_observed":
			if notice.NativeEntryID == "" {
				return errors.New("observed control notice lacks an entry ID")
			}
		default:
			return errors.New("invalid control notice disposition")
		}
		notices[notice.ID] = true
	}
	for _, request := range lifecycle.Requests {
		if !notices[request.ID.Nonce+"-shutdown_request"] {
			return errors.New("shutdown request has no retained notice")
		}
	}
	if err := validateTurnNotices(lifecycle.Turns, ref); err != nil {
		return err
	}
	return nil
}

func (s *teamStore) lifecycleTransaction(ref MemberRef, change func(*memberLifecycle) (bool, error)) error {
	return s.memberControlTransaction(ref, false, func(ledger *planLedger) (bool, *PlanRequestID, error) {
		initial := ledger.Lifecycle == nil
		if initial {
			ledger.Lifecycle = &memberLifecycle{Phase: "open", Requests: []shutdownRequest{}, Notices: []controlNotice{}}
		}
		changed, err := change(ledger.Lifecycle)
		return initial || changed, nil, err
	})
}

func shutdownNotice(request shutdownRequest, recipient, kind, text string) controlNotice {
	return controlNotice{ID: request.ID.Nonce + "-" + kind, Request: request.ID, Recipient: recipient, Kind: kind, Text: text, Delivery: "pending"}
}

func (s *teamStore) requestShutdown(ref MemberRef, submission, reason string, timeoutMs int) (shutdownRequest, controlNotice, bool, error) {
	var result shutdownRequest
	var notice controlNotice
	if !peerMessageIDPattern.MatchString(submission) || strings.TrimSpace(reason) == "" || len(reason) > 4096 || !utf8.ValidString(reason) || timeoutMs < 1000 || timeoutMs > 3600000 {
		return result, notice, false, reject("rejected", "invalid shutdown request")
	}
	created := false
	var timerDeadline time.Time
	err := s.lifecycleTransaction(ref, func(lifecycle *memberLifecycle) (bool, error) {
		current := time.Now()
		changed := expireShutdowns(lifecycle, current.UTC())
		if lifecycle.Phase != "open" {
			return changed, reject("unavailable", "teammate is not open for shutdown requests")
		}
		for _, request := range lifecycle.Requests {
			if request.SubmissionID == submission {
				if request.Reason != reason || request.TimeoutMs != timeoutMs {
					return changed, reject("rejected", "shutdown submission ID conflicts with an existing request")
				}
				result = request
				for _, candidate := range lifecycle.Notices {
					if candidate.Request == request.ID && candidate.Kind == "shutdown_request" {
						notice = candidate
					}
				}
				return changed, nil
			}
			if request.State == "pending" {
				return changed, reject("rejected", "a shutdown request is already pending")
			}
		}
		if len(lifecycle.Requests) >= shutdownRequestLimit || lifecycle.HighWater == math.MaxUint64 {
			return changed, reject("capacity", "shutdown history is full")
		}
		nonce, err := newControlNonce()
		if err != nil {
			return changed, reject("unavailable", "cannot allocate shutdown request identity")
		}
		now := current.UTC()
		lifecycle.HighWater++
		result = shutdownRequest{ID: ShutdownRequestID{Member: ref, Sequence: lifecycle.HighWater, Nonce: nonce}, SubmissionID: submission, Reason: reason, TimeoutMs: timeoutMs, CreatedAt: now, Deadline: now.Add(time.Duration(timeoutMs) * time.Millisecond), State: "pending"}
		notice = shutdownNotice(result, "member", "shutdown_request", reason)
		lifecycle.Requests = append(lifecycle.Requests, result)
		lifecycle.Notices = append(lifecycle.Notices, notice)
		timerDeadline, created = current.Add(time.Duration(timeoutMs)*time.Millisecond), true
		return true, nil
	})
	if err == nil && created {
		time.AfterFunc(time.Until(timerDeadline), func() { _ = s.expireShutdown(result.ID) })
	}
	return result, notice, created, err
}

func expireShutdowns(lifecycle *memberLifecycle, now time.Time) bool {
	changed := false
	backward := !lifecycle.Clock.IsZero() && now.Before(lifecycle.Clock)
	for index := range lifecycle.Requests {
		request := &lifecycle.Requests[index]
		if request.State == "pending" && (backward || !now.Before(request.Deadline)) {
			request.State = "expired"
			changed = true
		}
	}
	if now.After(lifecycle.Clock) {
		lifecycle.Clock = now
		changed = true
	}
	return changed
}

func (s *teamStore) expireShutdown(id ShutdownRequestID) error {
	return s.lifecycleTransaction(id.Member, func(lifecycle *memberLifecycle) (bool, error) {
		if id.Sequence == 0 || id.Sequence > uint64(len(lifecycle.Requests)) {
			return false, nil
		}
		request := &lifecycle.Requests[id.Sequence-1]
		if request.ID != id || request.State != "pending" {
			return false, nil
		}
		request.State = "expired"
		return true, nil
	})
}

func (s *teamStore) respondShutdown(id ShutdownRequestID, execution, decision, reason string) (shutdownRequest, controlNotice, error) {
	var result shutdownRequest
	var notice controlNotice
	if id.Sequence == 0 || !validPlanNonce(id.Nonce) || execution == "" || decision != "approve" && decision != "reject" || decision == "approve" && reason != "" || decision == "reject" && (strings.TrimSpace(reason) == "" || len(reason) > 4096 || !utf8.ValidString(reason)) {
		return result, notice, reject("rejected", "invalid shutdown response")
	}
	err := s.lifecycleTransaction(id.Member, func(lifecycle *memberLifecycle) (bool, error) {
		changed := expireShutdowns(lifecycle, time.Now().UTC())
		if id.Sequence > uint64(len(lifecycle.Requests)) {
			return changed, reject("rejected", "shutdown request is unavailable")
		}
		request := &lifecycle.Requests[id.Sequence-1]
		result = *request
		if request.ID != id || request.State != "pending" || lifecycle.Phase != "open" {
			return changed, reject("rejected", "shutdown response is stale or replayed")
		}
		if !time.Now().Before(request.Deadline) {
			request.State = "expired"
			result = *request
			return true, reject("rejected", "shutdown request expired")
		}
		request.Execution = execution
		if decision == "reject" {
			request.State, request.Response = "rejected", reason
			notice = shutdownNotice(*request, "lead", "shutdown_rejected", reason)
		} else {
			request.State = "approved"
			lifecycle.Phase, lifecycle.Active = "closing", &id
			notice = shutdownNotice(*request, "lead", "shutdown_approved", "The teammate accepted shutdown after its current tool batch.")
		}
		result = *request
		lifecycle.Notices = append(lifecycle.Notices, notice)
		return true, nil
	})
	return result, notice, err
}

func (s *teamStore) cancelShutdown(id ShutdownRequestID) (shutdownRequest, controlNotice, error) {
	var result shutdownRequest
	var notice controlNotice
	err := s.lifecycleTransaction(id.Member, func(lifecycle *memberLifecycle) (bool, error) {
		changed := expireShutdowns(lifecycle, time.Now().UTC())
		if id.Sequence == 0 || id.Sequence > uint64(len(lifecycle.Requests)) {
			return changed, reject("rejected", "shutdown request is unavailable")
		}
		request := &lifecycle.Requests[id.Sequence-1]
		result = *request
		if request.ID != id || request.State != "pending" {
			return changed, reject("rejected", "shutdown request is no longer pending")
		}
		request.State = "cancelled"
		result = *request
		notice = shutdownNotice(*request, "member", "shutdown_cancelled", "The lead cancelled the shutdown request.")
		lifecycle.Notices = append(lifecycle.Notices, notice)
		return true, nil
	})
	return result, notice, err
}

func (s *teamStore) readLifecycle(ref MemberRef) (memberLifecycle, error) {
	var result memberLifecycle
	err := s.lifecycleTransaction(ref, func(lifecycle *memberLifecycle) (bool, error) {
		changed := expireShutdowns(lifecycle, time.Now().UTC())
		result = *lifecycle
		if lifecycle.Active != nil {
			copy := *lifecycle.Active
			result.Active = &copy
		}
		result.Requests = append([]shutdownRequest(nil), lifecycle.Requests...)
		result.Notices = append([]controlNotice(nil), lifecycle.Notices...)
		return changed, nil
	})
	return result, err
}

func (s *teamStore) snapshotLifecycle(ref MemberRef) (memberLifecycle, error) {
	result := memberLifecycle{Phase: "open", Requests: []shutdownRequest{}, Notices: []controlNotice{}}
	err := s.transact(func(roster *teamRoster) (bool, error) {
		found := false
		for _, member := range roster.Members {
			if member.Ref == ref {
				found = true
				break
			}
		}
		if !found || s.planUncertain[ref] != nil {
			return false, reject("unavailable", "teammate control state is unavailable")
		}
		lockPath, path := planPaths(s.dir, ref)
		if _, err := os.Lstat(lockPath); errors.Is(err, os.ErrNotExist) {
			if _, exists := s.planDigests[ref]; exists {
				return false, errors.New("plan metadata disappeared")
			}
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				return false, errors.New("plan lock is missing for existing metadata")
			}
			return false, nil
		} else if err != nil {
			return false, err
		}
		lock, err := lockTeamFile(lockPath, false)
		if err != nil {
			return false, err
		}
		err = func() error {
			ledger, err := s.readMemberControlLocked(ref, path)
			if err != nil {
				return err
			}
			if ledger.Lifecycle != nil {
				result = *ledger.Lifecycle
			}
			return nil
		}()
		return false, errors.Join(err, lock.Close())
	})
	if err == nil {
		expireShutdowns(&result, time.Now().UTC())
	}
	return result, err
}

func (s *teamStore) setControlNotice(expected controlNotice, from, to, entryID string) (bool, error) {
	changed := false
	err := s.lifecycleTransaction(expected.Request.Member, func(lifecycle *memberLifecycle) (bool, error) {
		for index := range lifecycle.Notices {
			notice := &lifecycle.Notices[index]
			if notice.ID != expected.ID {
				continue
			}
			if notice.Request != expected.Request || notice.Recipient != expected.Recipient || notice.Kind != expected.Kind || notice.Text != expected.Text {
				return false, errors.New("control notice identity changed")
			}
			if notice.Delivery != from {
				return false, nil
			}
			notice.Delivery, notice.NativeEntryID = to, entryID
			changed = true
			return true, nil
		}
		return false, errors.New("control notice is unavailable")
	})
	return changed, err
}

func (s *teamStore) finishShutdown(id ShutdownRequestID, execution string, terminated bool) (controlNotice, error) {
	var notice controlNotice
	err := s.lifecycleTransaction(id.Member, func(lifecycle *memberLifecycle) (bool, error) {
		if lifecycle.Phase != "closing" || lifecycle.Active == nil || *lifecycle.Active != id || id.Sequence == 0 || id.Sequence > uint64(len(lifecycle.Requests)) {
			return false, reject("unavailable", "shutdown is no longer current")
		}
		request := &lifecycle.Requests[id.Sequence-1]
		if request.ID != id || request.State != "approved" || request.Execution != execution {
			return false, reject("unavailable", "shutdown execution changed")
		}
		if terminated {
			request.State, lifecycle.Phase = "terminated", "terminated"
			notice = shutdownNotice(*request, "lead", "shutdown_terminated", "The teammate finished its current batch and cleanup and has terminated.")
			lifecycle.Notices = append(lifecycle.Notices, notice)
		} else {
			request.State, lifecycle.Phase = "interrupted", "recovery_pending"
		}
		return true, nil
	})
	return notice, err
}

func (s *teamStore) recoverLifecycle(ref MemberRef, rosterState string) (string, error) {
	phase := "open"
	err := s.lifecycleTransaction(ref, func(lifecycle *memberLifecycle) (bool, error) {
		changed := false
		for index := range lifecycle.Requests {
			if lifecycle.Requests[index].State == "pending" {
				lifecycle.Requests[index].State = "cancelled"
				changed = true
			}
		}
		switch lifecycle.Phase {
		case "closing":
			lifecycle.Requests[lifecycle.Active.Sequence-1].State = "interrupted"
			lifecycle.Phase = "recovery_pending"
			changed = true
		case "open":
			if rosterState == "closing" || rosterState == "terminated" {
				return changed, reject("unavailable", "shutdown history is missing")
			}
		case "terminated":
			if rosterState != "terminated" {
				return changed, reject("unavailable", "terminal shutdown records conflict")
			}
		}
		phase = lifecycle.Phase
		return changed, nil
	})
	return phase, err
}
