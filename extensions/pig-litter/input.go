package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/coding"
)

type initialInputKey struct{}

func inputStored(entries []json.RawMessage, input pendingInput) bool {
	if input.After == nil {
		return false
	}
	after := *input.After == ""
	for _, raw := range entries {
		var entry struct {
			ID      string `json:"id"`
			Message struct {
				Role    string          `json:"role"`
				Content json.RawMessage `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(raw, &entry) != nil {
			continue
		}
		if entry.ID == *input.After {
			after = true
			continue
		}
		if !after || entry.Message.Role != "user" {
			continue
		}
		var text string
		if json.Unmarshal(entry.Message.Content, &text) != nil {
			var blocks []struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(entry.Message.Content, &blocks) != nil {
				continue
			}
			for _, block := range blocks {
				text += block.Text
			}
		}
		if text == input.Text {
			return true
		}
	}
	return false
}

func (t *Tree) prepareInput(session *coding.Session, selected *run) error {
	entries, err := rawEntries(session.SessionManager().Entries())
	if err != nil {
		return err
	}
	after := ""
	if leaf := session.SessionManager().LeafID(); leaf != nil {
		after = *leaf
	}
	t.mu.Lock()
	node := t.records[selected.ref.ID]
	if selected.resume {
		pending := []pendingInput{}
		for _, input := range node.pendingInputs {
			if !inputStored(entries, input) {
				pending = append(pending, input)
			}
		}
		parts := []string{}
		if node.pendingTask != "" && (len(pending) == 0 || pending[0].After == nil) {
			parts = append(parts, "Task:\n"+node.pendingTask)
		}
		for _, input := range pending {
			parts = append(parts, input.Text)
		}
		if len(pending) == 0 {
			parts = append(parts, selected.continuation)
		}
		selected.prompt = strings.Join(parts, "\n\nResume instructions:\n")
	}
	node.pendingInputs = []pendingInput{{Text: selected.prompt, After: &after}}
	t.dirty[node.id] = node
	t.mu.Unlock()
	if err := t.notify(); err != nil {
		return fmt.Errorf("child input storage failed: %w", err)
	}
	return nil
}

func (t *Tree) sendInput(session *coding.Session, selected *run) error {
	if err := t.prepareInput(session, selected); err != nil {
		return err
	}
	cleared := false
	unsubscribe := session.Agent().Subscribe(func(ctx context.Context, event agent.AgentEvent) error {
		if _, started := event.(agent.TurnStartEvent); !started || cleared || ctx.Value(initialInputKey{}) != selected {
			return nil
		}
		t.mu.Lock()
		if node := t.records[selected.ref.ID]; node != nil && node.run == selected {
			node.pendingTask = ""
			node.pendingInputs = nil
			t.dirty[node.id] = node
		}
		t.mu.Unlock()
		cleared = true
		return t.notify()
	})
	defer unsubscribe()
	_, err := session.Send(context.WithValue(selected.ctx, initialInputKey{}, selected), selected.prompt)
	return err
}
