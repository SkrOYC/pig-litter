package piglitter

import (
	"context"
	"strings"
	"unicode/utf8"
)

type memberSettlement struct {
	boundary  uint64
	candidate idlePolicyCandidate
	wake      uint64
}

type memberSettlementDecision struct {
	boundary    uint64
	allow       bool
	continueRun bool
	feedback    string
}

type memberSettlementCandidate struct {
	boundary   uint64
	report     string
	completed  bool
	continuing bool
}

func (o *owner) decideMemberSettlement(ctx context.Context, binding *memberBinding, input memberSettlementCandidate) (memberSettlementDecision, error) {
	boundary, report := input.boundary, input.report
	decision := memberSettlementDecision{boundary: boundary}
	if boundary == 0 || len(report) > 4096 || !utf8.ValidString(report) {
		return decision, reject("rejected", "invalid native settlement boundary")
	}
	if !input.completed || input.continuing {
		return decision, reject("unavailable", "native execution is not a completed settlement candidate")
	}
	binding.settleMu.Lock()
	if boundary <= binding.boundary || binding.evaluating || binding.settlement != nil {
		binding.settleMu.Unlock()
		return decision, reject("unavailable", "native settlement boundary is stale or already pending")
	}
	binding.boundary = boundary
	binding.evaluating = true
	binding.settleMu.Unlock()
	defer func() {
		binding.settleMu.Lock()
		binding.evaluating = false
		binding.settleMu.Unlock()
	}()
	o.tree.lifecycleMu.Lock()
	o.tree.mu.Lock()
	active := o.memberBindingCurrentLocked(binding)
	o.tree.mu.Unlock()
	if !active {
		o.tree.lifecycleMu.Unlock()
		return decision, reject("unavailable", "native member execution is no longer current")
	}
	candidate, wake, err := o.idleCandidate(binding.record, binding.run, report)
	o.tree.lifecycleMu.Unlock()
	if err != nil {
		return decision, err
	}
	permit, feedback := runTeamPolicy(ctx, o.policy.TeammateIdle, candidate)
	o.tree.lifecycleMu.Lock()
	defer o.tree.lifecycleMu.Unlock()
	o.tree.mu.Lock()
	active = o.memberBindingCurrentLocked(binding)
	o.tree.mu.Unlock()
	if !active {
		return decision, reject("unavailable", "native member execution changed during policy evaluation")
	}
	current, currentWake, err := o.idleCandidate(binding.record, binding.run, report)
	if err != nil {
		return decision, err
	}
	if current.Member != candidate.Member || current.Execution != candidate.Execution || current.Revision != candidate.Revision || currentWake != wake {
		permit = false
		feedback = "Team work changed while the idle policy ran. Review the current work before settling."
	}
	binding.settleMu.Lock()
	defer binding.settleMu.Unlock()
	if binding.boundary != boundary || binding.settlement != nil {
		return decision, reject("unavailable", "native settlement boundary changed during policy evaluation")
	}
	if !permit {
		decision.feedback = feedback
		o.tree.mu.Lock()
		binding.run.idleRefusal = feedback
		decision.continueRun = binding.run.progress.turns < o.config.MaxTurns
		o.tree.mu.Unlock()
		return decision, nil
	}
	binding.settlement = &memberSettlement{boundary: boundary, candidate: candidate, wake: wake}
	decision.allow = true
	return decision, nil
}

func (o *owner) finalizeMemberSettlement(binding *memberBinding, boundary uint64, aborted bool, commit func() error) (memberSettlementDecision, error) {
	decision := memberSettlementDecision{boundary: boundary}
	o.tree.lifecycleMu.Lock()
	defer o.tree.lifecycleMu.Unlock()
	binding.settleMu.Lock()
	defer binding.settleMu.Unlock()
	pending := binding.settlement
	if pending == nil || pending.boundary != boundary || binding.boundary != boundary {
		return decision, reject("unavailable", "native settled event does not match an approved boundary")
	}
	binding.settlement = nil
	if aborted {
		return decision, nil
	}
	o.tree.mu.Lock()
	active := o.memberBindingCurrentLocked(binding)
	o.tree.mu.Unlock()
	if !active {
		return decision, reject("unavailable", "native member execution is no longer current")
	}
	current, wake, err := o.idleCandidate(binding.record, binding.run, pending.candidate.Report)
	if err != nil {
		return decision, err
	}
	if current.Member != pending.candidate.Member || current.Execution != pending.candidate.Execution || current.Revision != pending.candidate.Revision || wake != pending.wake {
		decision.feedback = "Team work changed before the teammate settled. Review the current work before settling."
		o.tree.mu.Lock()
		binding.run.idleRefusal = decision.feedback
		decision.continueRun = binding.run.progress.turns < o.config.MaxTurns
		o.tree.mu.Unlock()
		return decision, nil
	}
	if commit == nil {
		return decision, reject("unavailable", "native settlement has no root commit")
	}
	if err := commit(); err != nil {
		return decision, err
	}
	o.tree.mu.Lock()
	binding.run.idleRefusal = ""
	o.tree.mu.Unlock()
	decision.allow = true
	return decision, nil
}

func memberSettlementFeedback(decision memberSettlementDecision) string {
	feedback := strings.TrimSpace(decision.feedback)
	if feedback == "" {
		feedback = "The current team work could not be checked."
	}
	return "The teammate idle policy requires further work. " + feedback
}
