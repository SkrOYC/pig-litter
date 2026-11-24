package piglitter

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

func teamPlanMember(frame *teamViewFrame, ref MemberRef) string {
	if index := slices.IndexFunc(frame.Members, func(member teamMemberView) bool { return member.Member.Ref == ref }); index >= 0 {
		return frame.Members[index].Member.Name
	}
	return string(ref.ID)
}

func teamPlanSummary(frame *teamViewFrame, plan planView) string {
	return fmt.Sprintf("#%d %s | %s | %s", plan.Request.ID.Sequence, plan.Request.State, teamPlanMember(frame, plan.Request.ID.Member), plan.Request.Text)
}

func teamPlanDetail(frame *teamViewFrame, plan planView) []string {
	request := plan.Request
	lines := []string{
		fmt.Sprintf("Plan #%d | state %s | gate %s", request.ID.Sequence, request.State, plan.Gate),
		"Member " + teamPlanMember(frame, request.ID.Member),
		fmt.Sprintf("Member UUID %s | generation %d", request.ID.Member.ID, request.ID.Member.Generation),
		"Deadline " + request.Deadline.UTC().Format(time.RFC3339),
		"Plan " + request.Text,
	}
	if request.Reason != "" {
		lines = append(lines, "Feedback "+request.Reason)
	}
	return lines
}

func (v *teamView) WatchPlans() {
	v.mu.Lock()
	if !v.closed {
		v.watchPlans = true
		v.planEpoch++
		frame := *v.frame
		frame.PlanHealth = viewHealth{Status: "loading"}
		v.frame = &frame
	}
	v.mu.Unlock()
	v.queuePlans()
}

func (v *teamView) StopWatchingPlans() {
	v.mu.Lock()
	v.watchPlans = false
	v.planEpoch++
	v.mu.Unlock()
}

func (v *teamView) queuePlans() {
	v.mu.Lock()
	watching := v.watchPlans && !v.closed
	v.mu.Unlock()
	if watching {
		select {
		case v.plans <- struct{}{}:
		default:
		}
	}
}

func (v *teamView) planLoop() {
	for {
		select {
		case <-v.ctx.Done():
			return
		case <-v.plans:
		}
		v.mu.Lock()
		frame, epoch, watching := v.frame, v.planEpoch, v.watchPlans
		v.mu.Unlock()
		if !watching || frame.Team == nil {
			continue
		}
		team := *frame.Team
		var plans []planView
		var err error
		if frame.RosterHealth.Status != "fresh" {
			err = errors.New("roster unavailable")
		} else if slices.ContainsFunc(frame.Members, func(member teamMemberView) bool { return member.Member.PlanRequired }) {
			action, actionErr := v.owner.beginAction(v.ctx)
			if actionErr != nil {
				err = actionErr
			} else {
				plans, err = v.owner.planSnapshot(v.owner.humanTeamAuthority(action.ctx))
				action.finish()
			}
		}
		v.mu.Lock()
		v.owner.state.mu.Lock()
		active := v.owner.state.owner == v.owner && v.owner.state.epoch == v.owner.epoch
		current := v.frame.Team != nil && *v.frame.Team == team
		sameRoster := slices.EqualFunc(frame.Members, v.frame.Members, func(a, b teamMemberView) bool {
			return a.Member.Ref == b.Member.Ref && a.Member.AdmissionID == b.Member.AdmissionID && a.Member.PlanRequired == b.Member.PlanRequired
		})
		if !active || !current || !sameRoster || !v.watchPlans || v.planEpoch != epoch || v.closed || v.ctx.Err() != nil {
			v.owner.state.mu.Unlock()
			v.mu.Unlock()
			continue
		}
		updated := *v.frame
		if err != nil {
			updated.PlanHealth = viewFailure(updated.PlanHealth, err)
		} else {
			updated.Plans = plans
			updated.PlanHealth = viewHealth{Status: "fresh"}
		}
		changed := updated.PlanHealth != v.frame.PlanHealth || !slices.Equal(updated.Plans, v.frame.Plans)
		v.frame = &updated
		v.owner.state.mu.Unlock()
		v.mu.Unlock()
		if changed {
			v.notify()
		}
	}
}
