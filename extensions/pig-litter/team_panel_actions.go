package piglitter

import (
	"context"
	"fmt"
)

func (o *owner) humanTeamAction(ctx context.Context, apply func(controlAuthority) humanResult) <-chan humanResult {
	result := make(chan humanResult, 1)
	admitted, err := o.beginAction(ctx)
	if err != nil {
		result <- humanResult{err: err}
		return result
	}
	go func() {
		defer admitted.finish()
		result <- apply(o.humanTeamAuthority(admitted.ctx))
	}()
	return result
}

func (o *owner) humanTeamAuthority(ctx context.Context) controlAuthority {
	return controlAuthority{human: func() error {
		if err := ctx.Err(); err != nil {
			return err
		}
		o.state.mu.Lock()
		defer o.state.mu.Unlock()
		if o.state.owner != o || o.state.epoch != o.epoch {
			return reject("unavailable", "the root Session retired")
		}
		return nil
	}}
}

func (p *childPanel) completeSelectedTask(frame *teamViewFrame) {
	index := p.taskSelection(frame)
	if frame.TaskHealth.Status != "fresh" || index < 0 {
		p.feedback = "Selected task is unavailable"
		return
	}
	id := frame.Tasks[index].ID
	ctx, cancel := context.WithCancel(p.ctx)
	p.pendingCancel = cancel
	p.pending = p.owner.humanTeamAction(ctx, func(authority controlAuthority) humanResult {
		_, state, err := p.owner.applyTaskCompletion(authority, id)
		feedback := fmt.Sprintf("Task #%d %s", id.Number, state)
		switch state {
		case "completed":
			feedback = fmt.Sprintf("Task #%d completed", id.Number)
		case "already_completed":
			feedback = fmt.Sprintf("Task #%d is already complete", id.Number)
		case "blocked":
			feedback = fmt.Sprintf("Task #%d is blocked by dependencies", id.Number)
		}
		return humanResult{feedback: feedback, err: err}
	})
}

func (p *childPanel) decideSelectedPlan(frame *teamViewFrame, decision, reason string) {
	index := p.planSelection(frame)
	if frame.RosterHealth.Status != "fresh" || frame.PlanHealth.Status != "fresh" || index < 0 || frame.Plans[index].Request.State != "pending" {
		p.feedback = "Selected plan is unavailable or no longer pending"
		return
	}
	id := frame.Plans[index].Request.ID
	ctx, cancel := context.WithCancel(p.ctx)
	p.pendingCancel = cancel
	p.pending = p.owner.humanTeamAction(ctx, func(authority controlAuthority) humanResult {
		result, err := p.owner.applyPlan(authority, planCommand{Action: "decide", RequestID: id, Decision: decision, Reason: reason})
		return humanResult{feedback: fmt.Sprintf("Plan #%d %v", id.Sequence, result["state"]), err: err}
	})
}
