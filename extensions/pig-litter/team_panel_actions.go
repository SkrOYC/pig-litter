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
		authority := controlAuthority{human: func() error {
			if err := admitted.ctx.Err(); err != nil {
				return err
			}
			o.state.mu.Lock()
			defer o.state.mu.Unlock()
			if o.state.owner != o || o.state.epoch != o.epoch {
				return reject("unavailable", "the root Session retired")
			}
			return nil
		}}
		result <- apply(authority)
	}()
	return result
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
