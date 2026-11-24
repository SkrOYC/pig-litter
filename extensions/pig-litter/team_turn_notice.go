package piglitter

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

type turnNotice struct {
	Member        MemberRef `json:"member"`
	Execution     string    `json:"execution"`
	Kind          string    `json:"kind"`
	Report        string    `json:"report,omitempty"`
	Error         string    `json:"error,omitempty"`
	Delivery      string    `json:"delivery"`
	NativeEntryID string    `json:"nativeEntryId,omitempty"`
}

const turnNoticeLimit = 16

func validateTurnNotices(notices []turnNotice, ref MemberRef) error {
	if len(notices) > turnNoticeLimit {
		return errors.New("too many teammate turn notices")
	}
	seen := map[string]bool{}
	for _, notice := range notices {
		if notice.Member != ref || notice.Execution == "" || len(notice.Execution) > 128 || seen[notice.Execution] || !utf8.ValidString(notice.Execution) || notice.Kind != "available" && notice.Kind != "interrupted" && notice.Kind != "failed" || len(notice.Report) > 4096 || len(notice.Error) > 1024 || !utf8.ValidString(notice.Report) || !utf8.ValidString(notice.Error) {
			return errors.New("invalid teammate turn notice")
		}
		switch notice.Delivery {
		case "pending", "attempted", "ambiguous":
			if notice.NativeEntryID != "" {
				return errors.New("unobserved turn notice has an entry ID")
			}
		case "native_observed":
			if notice.NativeEntryID == "" {
				return errors.New("observed turn notice lacks an entry ID")
			}
		default:
			return errors.New("invalid turn notice disposition")
		}
		seen[notice.Execution] = true
	}
	return nil
}

func (s *teamStore) recordTurnNotice(ref MemberRef, execution string, outcome Outcome) (turnNotice, bool, error) {
	kind := "failed"
	if outcome.State == Completed {
		kind = "available"
	} else if outcome.State == Stopped || outcome.State == Paused {
		kind = "interrupted"
	}
	report, _ := bounded(strings.ToValidUTF8(outcome.Text, "\uFFFD"), 4096)
	failure, _ := bounded(strings.ToValidUTF8(outcome.Error, "\uFFFD"), 1024)
	notice := turnNotice{Member: ref, Execution: execution, Kind: kind, Report: report, Error: failure, Delivery: "pending"}
	created := false
	err := s.lifecycleTransaction(ref, func(lifecycle *memberLifecycle) (bool, error) {
		for _, current := range lifecycle.Turns {
			if current.Execution != execution {
				continue
			}
			prior := current
			prior.Delivery, prior.NativeEntryID = "pending", ""
			if prior != notice {
				return false, errors.New("teammate turn notice identity changed")
			}
			notice = current
			return false, nil
		}
		if len(lifecycle.Turns) == turnNoticeLimit {
			removed := false
			for index, current := range lifecycle.Turns {
				if current.Delivery == "native_observed" {
					lifecycle.Turns = append(lifecycle.Turns[:index], lifecycle.Turns[index+1:]...)
					removed = true
					break
				}
			}
			if !removed {
				return false, reject("capacity", "teammate turn notices are full")
			}
		}
		lifecycle.Turns = append(lifecycle.Turns, notice)
		created = true
		return true, nil
	})
	return notice, created, err
}

func (s *teamStore) setTurnNotice(expected turnNotice, from, to, entryID string) (bool, error) {
	changed := false
	err := s.lifecycleTransaction(expected.Member, func(lifecycle *memberLifecycle) (bool, error) {
		for index := range lifecycle.Turns {
			current := &lifecycle.Turns[index]
			if current.Execution != expected.Execution {
				continue
			}
			prior := *current
			prior.Delivery, prior.NativeEntryID = expected.Delivery, expected.NativeEntryID
			if prior != expected {
				return false, errors.New("teammate turn notice identity changed")
			}
			if current.Delivery != from {
				return false, nil
			}
			current.Delivery, current.NativeEntryID = to, entryID
			changed = true
			return true, nil
		}
		return false, errors.New("teammate turn notice is unavailable")
	})
	return changed, err
}

func (o *owner) routeTurnNotice(notice turnNotice) {
	o.controlMu.Lock()
	if o.turnAuth == nil {
		o.turnAuth = map[string]turnNotice{}
	}
	o.turnAuth[notice.Execution] = notice
	o.controlMu.Unlock()
	o.tree.mu.Lock()
	if o.tree.closed {
		o.tree.mu.Unlock()
		return
	}
	o.tree.teamWG.Add(1)
	o.tree.mu.Unlock()
	go func() {
		defer o.tree.teamWG.Done()
		if o.current() != nil {
			return
		}
		store, err := o.teamStoreForRoot()
		if err != nil {
			return
		}
		changed, err := store.setTurnNotice(notice, "pending", "attempted", "")
		if err != nil || !changed {
			return
		}
		payload, err := json.Marshal(notice)
		if err != nil {
			return
		}
		trigger := false
		err = o.context.SendCustomMessage(sdk.CustomMessage{CustomType: "pig_litter_team_turn", Content: string(payload), Display: true, Details: map[string]any{"notice": notice}}, sdk.SendMessageOptions{TriggerTurn: &trigger})
		if err != nil {
			_, _ = store.setTurnNotice(notice, "attempted", "ambiguous", "")
			return
		}
		o.observeTurnNotice(notice)
	}()
}

func (o *owner) observeTurnNotice(notice turnNotice) {
	o.controlMu.Lock()
	trusted, ok := o.turnAuth[notice.Execution]
	o.controlMu.Unlock()
	if !ok || trusted != notice {
		return
	}
	entries, err := o.completionEntries(o.tree.root.id)
	if err != nil {
		return
	}
	payload, err := json.Marshal(notice)
	if err != nil {
		return
	}
	var observed string
	for _, raw := range entries {
		var entry struct {
			Type       string          `json:"type"`
			ID         string          `json:"id"`
			CustomType string          `json:"customType"`
			Content    string          `json:"content"`
			Details    json.RawMessage `json:"details"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Type != "custom_message" || entry.CustomType != "pig_litter_team_turn" {
			continue
		}
		var details struct {
			Notice turnNotice `json:"notice"`
		}
		if json.Unmarshal(entry.Details, &details) != nil || details.Notice != notice || entry.Content != string(payload) {
			continue
		}
		if entry.ID == "" || observed != "" {
			return
		}
		observed = entry.ID
	}
	if observed == "" {
		return
	}
	store, err := o.teamStoreForRoot()
	if err != nil {
		return
	}
	changed, err := store.setTurnNotice(notice, "attempted", "native_observed", observed)
	if err == nil && changed {
		o.controlMu.Lock()
		delete(o.turnAuth, notice.Execution)
		o.controlMu.Unlock()
	}
}

func (o *owner) observeTurnNotices() {
	o.controlMu.Lock()
	var notices []turnNotice
	for _, notice := range o.turnAuth {
		notices = append(notices, notice)
	}
	o.controlMu.Unlock()
	for _, notice := range notices {
		o.observeTurnNotice(notice)
	}
}
