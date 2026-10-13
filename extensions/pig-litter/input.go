package piglitter

import (
	"context"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/coding"
)

type initialInputKey struct{}

func (t *Tree) sendInput(session *coding.Session, selected *run) error {
	unsubscribe := session.Agent().Subscribe(func(ctx context.Context, event agent.AgentEvent) error {
		ended, ok := event.(agent.MessageEndEvent)
		if !ok || ended.Message.User == nil || ctx.Value(initialInputKey{}) != selected {
			return nil
		}
		t.mu.Lock()
		if node := t.records[selected.ref.ID]; node != nil && node.run == selected {
			node.pendingTask = ""
		}
		t.mu.Unlock()
		return nil
	})
	defer unsubscribe()
	_, err := session.Send(context.WithValue(selected.ctx, initialInputKey{}, selected), selected.prompt)
	return err
}
