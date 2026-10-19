package piglitter

import "github.com/MichaelKinsy/PiG/agent"

const activeToolLimit = 64

func (t *Tree) observeProgress(selected *run, event agent.AgentEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	r := t.records[selected.ref.ID]
	if t.closed || r == nil || r.run != selected || selected.progress.activated.IsZero() || selected.state.terminal() {
		return
	}
	progress := &selected.progress
	switch event := event.(type) {
	case agent.TurnStartEvent, agent.TurnEndEvent:
		progress.activeTools = nil
		progress.toolsClipped = false
	case agent.ToolExecutionStartEvent:
		progress.toolStarts++
		if progress.activeTools == nil {
			progress.activeTools = map[string]string{}
		}
		if len(progress.activeTools) >= activeToolLimit || len(event.ToolCallID) > 1024 {
			progress.toolsClipped = true
			return
		}
		name, _ := bounded(event.ToolName, 256)
		progress.activeTools[event.ToolCallID] = name
	case agent.ToolExecutionEndEvent:
		delete(progress.activeTools, event.ToolCallID)
	}
}
