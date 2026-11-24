package piglitter

import (
	"errors"
	"fmt"
	"slices"
	"time"
)

type teamControlSection uint8

const (
	teamControlNone teamControlSection = iota
	teamControlPlans
	teamControlShutdowns
)

func teamShutdownSummary(frame *teamViewFrame, view shutdownView) string {
	return fmt.Sprintf("#%d %s | %s | phase %s | %s", view.Request.ID.Sequence, view.Request.State, teamMemberName(frame, view.Request.ID.Member), view.Phase, view.Request.Reason)
}

func teamShutdownDetail(frame *teamViewFrame, view shutdownView) []string {
	request := view.Request
	lines := []string{
		fmt.Sprintf("Shutdown #%d | request %s | phase %s", request.ID.Sequence, request.State, view.Phase),
		"Member " + teamMemberName(frame, request.ID.Member),
		fmt.Sprintf("Member UUID %s | generation %d", request.ID.Member.ID, request.ID.Member.Generation),
		"Deadline " + request.Deadline.UTC().Format(time.RFC3339),
		"Reason " + request.Reason,
	}
	if request.Response != "" {
		lines = append(lines, "Response "+request.Response)
	}
	for _, notice := range view.Notices {
		lines = append(lines, "Notice "+notice.Kind+" | delivery "+notice.Delivery)
		if notice.Text != "" {
			lines = append(lines, "  "+notice.Text)
		}
	}
	return lines
}

func (v *teamView) watchControl(section teamControlSection) {
	v.mu.Lock()
	if !v.closed {
		v.watching = section
		v.controlEpoch++
		frame := *v.frame
		if section == teamControlPlans {
			frame.PlanHealth = viewHealth{Status: "loading"}
		} else if section == teamControlShutdowns {
			frame.ShutdownHealth = viewHealth{Status: "loading"}
		}
		v.frame = &frame
	}
	v.mu.Unlock()
	v.queueControls()
}

func (v *teamView) queueControls() {
	v.mu.Lock()
	watching := v.watching != teamControlNone && !v.closed
	v.mu.Unlock()
	if watching {
		select {
		case v.controls <- struct{}{}:
		default:
		}
	}
}

func (v *teamView) controlLoop() {
	for {
		select {
		case <-v.ctx.Done():
			return
		case <-v.controls:
		}
		v.mu.Lock()
		frame, epoch, section := v.frame, v.controlEpoch, v.watching
		v.mu.Unlock()
		if section == teamControlNone || frame.Team == nil {
			continue
		}
		team := *frame.Team
		var plans []planView
		var shutdowns []shutdownView
		var err error
		needsRead := section == teamControlShutdowns || slices.ContainsFunc(frame.Members, func(member teamMemberView) bool { return member.Member.PlanRequired })
		if frame.RosterHealth.Status != "fresh" {
			err = errors.New("roster unavailable")
		} else if needsRead {
			action, actionErr := v.beginAction()
			if actionErr != nil {
				err = actionErr
			} else {
				authority := v.owner.humanTeamAuthority(action.ctx)
				if section == teamControlPlans {
					plans, err = v.owner.planSnapshot(authority)
				} else {
					shutdowns, err = v.owner.shutdownSnapshot(authority)
				}
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
		if !active || !current || !sameRoster || v.watching != section || v.controlEpoch != epoch || v.closed || v.ctx.Err() != nil {
			v.owner.state.mu.Unlock()
			v.mu.Unlock()
			continue
		}
		updated := *v.frame
		changed := false
		if section == teamControlPlans {
			if err != nil {
				updated.PlanHealth = viewFailure(updated.PlanHealth, err)
			} else {
				updated.Plans = plans
				updated.PlanHealth = viewHealth{Status: "fresh"}
			}
			changed = updated.PlanHealth != v.frame.PlanHealth || !slices.Equal(updated.Plans, v.frame.Plans)
		} else {
			if err != nil {
				updated.ShutdownHealth = viewFailure(updated.ShutdownHealth, err)
			} else {
				updated.Shutdowns = shutdowns
				updated.ShutdownHealth = viewHealth{Status: "fresh"}
			}
			changed = updated.ShutdownHealth != v.frame.ShutdownHealth || !slices.EqualFunc(updated.Shutdowns, v.frame.Shutdowns, func(a, b shutdownView) bool {
				return a.Request == b.Request && a.Phase == b.Phase && slices.Equal(a.Notices, b.Notices)
			})
		}
		v.frame = &updated
		v.owner.state.mu.Unlock()
		v.mu.Unlock()
		if changed {
			v.notify()
		}
	}
}
