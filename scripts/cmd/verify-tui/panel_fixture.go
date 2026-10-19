package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type messageDrainStage uint8

const (
	drainKitty messageDrainStage = iota
	drainSteer
	drainFollowUp
	drainComplete
)

var panelMarkers = []string{"HUMAN_RESUME_MARKER", "HUMAN_MESSAGE_RUN", "KITTY_INPUT_MARKER", "KITTY_CANCELLED_MARKER", "STEER_MARKER", "FOLLOWUP_MARKER", "HUMAN_CHAIN_RESUME", "NESTED_RESUME_MARKER", "TUI_READ_MARKER", "WORKER_PRIVATE_BOUNDARY", "GRAND_PRIVATE_BOUNDARY", "LIVE_QUICK_RUN"}

func fixtureMarkers(messages []openAIMessage) []string {
	text := allMessageText(messages)
	var markers []string
	for _, marker := range panelMarkers {
		if strings.Contains(text, marker) {
			markers = append(markers, marker)
		}
	}
	return markers
}

func allMessageText(messages []openAIMessage) string {
	var parts []string
	for _, message := range messages {
		parts = append(parts, contentText(message.Content))
	}
	return strings.Join(parts, "\n")
}

func fixtureReport(prefix string) string {
	var out strings.Builder
	out.WriteString(prefix + "\n")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&out, "REPORT_LINE_%02d %s native report content", i, prefix)
		if i == 70 {
			out.WriteString(" safe \x1b]0;OSC_PAYLOAD_SHOULD_NOT_PAINT\a\x1b[2J\u202e\x00")
		}
		out.WriteByte('\n')
	}
	return out.String()
}

func (state *fixtureState) panelTurnLocked(request openAIRequest, tools []string) (providerTurn, bool, error) {
	text := allMessageText(request.Messages)
	if request.Model == "quick" && strings.Contains(text, "LIVE_QUICK_RUN") {
		if !assistantContains(request.Messages, state.quickHistoryMarker) {
			return providerTurn{}, true, fmt.Errorf("quick resume lost native retained report")
		}
		if strings.Contains(text, "LIVE_QUICK_RUN_RESTART") {
			return providerTurn{kind: "hold", name: "quick-restart", text: fixtureReport("RESTART_REPORT_DONE")}, true, nil
		}
		return providerTurn{kind: "hold", name: "quick-live", text: fixtureReport("LIVE_REPORT_DONE")}, true, nil
	}
	if request.Model == "held" && (strings.Contains(text, "HUMAN_RESUME_MARKER") || strings.Contains(text, "HUMAN_MESSAGE_RUN")) {
		if err := requireToolInventory(tools, []string{"read"}, []string{"ls"}, "resumed scout"); err != nil {
			return providerTurn{}, true, err
		}
		if !strings.Contains(text, "TUI_READ_MARKER") {
			return providerTurn{}, true, fmt.Errorf("human resume lost native child history")
		}
		if strings.Contains(text, "HUMAN_MESSAGE_RUN") {
			turn, err := state.messageDrainTurnLocked(request.Messages)
			return turn, true, err
		}
		return providerTurn{kind: "hold", name: childName(request.Messages), text: "CHILD_PRIVATE_RESUME_BOUNDARY"}, true, nil
	}
	if request.Model == "chain_sibling" {
		return providerTurn{kind: "hold", name: "chain-sibling"}, true, nil
	}
	if request.Model == "chain_grand" {
		if strings.Contains(text, "NESTED_RESUME_MARKER") {
			if !strings.Contains(text, "TUI_READ_MARKER") {
				return providerTurn{}, true, fmt.Errorf("grandchild resume lost native read history")
			}
			return providerTurn{kind: "text", text: "GRAND_RESUMED_REPORT"}, true, nil
		}
		if len(roleMessages(request.Messages, "tool")) == 0 {
			return providerTurn{kind: "tools", calls: []toolCall{{name: "read", args: map[string]string{"path": "fixture-read.txt"}}}}, true, nil
		}
		return providerTurn{kind: "hold", name: "chain-grandchild", text: "GRAND_PRIVATE_BOUNDARY"}, true, nil
	}
	if request.Model == "chain_worker" {
		required := append(append([]string(nil), fileTools...), controlTools...)
		if state.caseName == casePiglet {
			required = append(required, "codemode", "tool_search")
		}
		if err := requireToolInventory(tools, required, []string{"ls"}, "chain worker"); err != nil {
			return providerTurn{}, true, err
		}
		values, err := toolResultValues(request.Messages)
		if err != nil {
			return providerTurn{}, true, err
		}
		var retainedGrandchild childRef
		for _, value := range values {
			if value.Operation == "litter_spawn" && value.Child.Name == "chain-grandchild" {
				retainedGrandchild = childRef{ID: value.Child.ID, Name: value.Child.Name}
				state.chainGrandchild = retainedGrandchild
			}
		}
		if strings.Contains(text, "HUMAN_CHAIN_RESUME") {
			if retainedGrandchild.ID == "" || !strings.Contains(text, "WORKER_ORIGINAL_TASK") {
				return providerTurn{}, true, fmt.Errorf("resumed worker lost native grandchild identity")
			}
			for index := len(values) - 1; index >= 0; index-- {
				value := values[index]
				if value.Operation == "litter_wait" && value.State == "completed" && value.Outcome != nil && value.Outcome.Run.ID == retainedGrandchild.ID {
					if value.Outcome.State != "completed" || value.Outcome.Model != "fixture/chain_grand" || value.Outcome.Text != "GRAND_RESUMED_REPORT" {
						return providerTurn{}, true, fmt.Errorf("grandchild completed wait differs from expected native result")
					}
					return providerTurn{kind: "text", text: "WORKER_CHAIN_DONE"}, true, nil
				}
				if value.Operation == "litter_message" && value.Child.ID == state.chainGrandchild.ID {
					if value.Delivery != "resumed" {
						return providerTurn{}, true, fmt.Errorf("worker grandchild message did not acknowledge explicit resume")
					}
					return providerTurn{kind: "tools", calls: []toolCall{{name: "litter_wait", args: map[string]any{"id": state.chainGrandchild.ID, "timeoutMs": 30000}}}}, true, nil
				}
			}
			return providerTurn{kind: "hold", name: "chain-worker-resumed", calls: []toolCall{{name: "litter_message", args: map[string]any{"id": state.chainGrandchild.ID, "resume": true, "text": "NESTED_RESUME_MARKER"}}}}, true, nil
		}
		if state.chainGrandchild.ID == "" {
			return providerTurn{kind: "tools", calls: []toolCall{{name: "litter_spawn", args: map[string]string{"type": "scout", "name": "chain-grandchild", "model": "fixture/chain_grand", "task": "GRAND_ORIGINAL_TASK"}}}}, true, nil
		}
		return providerTurn{kind: "hold", name: "chain-worker", text: "WORKER_PRIVATE_BOUNDARY"}, true, nil
	}
	if request.Model != "parent" {
		return providerTurn{}, false, nil
	}
	marker := latestRootPrompt(request.Messages).marker
	if marker == "NEW_ROOT_HEADER" || marker == "COLD_ROOT_HEADER" {
		if err := requireToolInventory(tools, append(append([]string(nil), fileTools...), controlTools...), []string{"ls"}, "context-only root"); err != nil {
			return providerTurn{}, true, err
		}
		return providerTurn{kind: "text", text: marker + "_DONE"}, true, nil
	}
	if strings.HasPrefix(marker, "ROOT_NOTICE_") || strings.HasPrefix(marker, "ROOT_RESTART_") {
		count := 0
		for _, message := range request.Messages {
			var notice struct{ Origin, Kind, Report string }
			if json.Unmarshal([]byte(contentText(message.Content)), &notice) == nil && notice.Origin == "pig-litter" && notice.Kind == "litter_completion" {
				if strings.Contains(notice.Report, state.quickHistoryMarker) {
					return providerTurn{}, true, fmt.Errorf("consumed wait handback duplicated as automatic notice")
				}
				prefix := "LIVE_REPORT_DONE"
				if strings.HasPrefix(marker, "ROOT_RESTART_") {
					prefix = "RESTART_REPORT_DONE"
				}
				if strings.HasPrefix(notice.Report, prefix) {
					count++
				}
			}
		}
		want := 1
		if marker == "ROOT_RESTART_FIRST" {
			want = 0
		}
		if count != want {
			return providerTurn{}, true, fmt.Errorf("explicit root prompt saw %d deferred completion notices, want %d", count, want)
		}
		return providerTurn{kind: "text", text: marker + "_DONE"}, true, nil
	}
	if marker != "RECEIPT_WIDGET" && marker != "CHAIN_WIDGET" {
		return providerTurn{}, false, nil
	}
	if state.caseName == caseUnselected {
		return providerTurn{}, true, fmt.Errorf("unselected root invoked a child panel fixture")
	}
	if err := requireToolInventory(tools, append(append([]string(nil), fileTools...), controlTools...), []string{"ls"}, "panel root"); err != nil {
		return providerTurn{}, true, err
	}
	switch marker {
	case "RECEIPT_WIDGET":
		return providerTurn{kind: "hold", name: "root-receipt"}, true, nil
	case "CHAIN_WIDGET":
		values, err := spawnToolResults(request.Messages)
		if err != nil {
			return providerTurn{}, true, err
		}
		for _, value := range values {
			switch value.Child.Name {
			case "chain-worker":
				state.chainWorker = childRef{ID: value.Child.ID, Name: value.Child.Name}
			case "chain-sibling":
				state.chainSibling = childRef{ID: value.Child.ID, Name: value.Child.Name}
			}
		}
		if state.chainWorker.ID == "" || state.chainSibling.ID == "" {
			return providerTurn{kind: "tools", calls: []toolCall{
				{name: "litter_spawn", args: map[string]string{"type": "worker", "name": "chain-worker", "model": "fixture/chain_worker", "task": "WORKER_ORIGINAL_TASK"}},
				{name: "litter_spawn", args: map[string]string{"type": "scout", "name": "chain-sibling", "model": "fixture/chain_sibling", "task": "SIBLING_ORIGINAL_TASK"}},
			}}, true, nil
		}
		return providerTurn{kind: "tools", calls: []toolCall{{name: "litter_wait", args: map[string]any{"id": state.chainWorker.ID, "timeoutMs": 300000}}}}, true, nil
	}
	return providerTurn{}, false, nil
}

func (state *fixtureState) messageDrainTurnLocked(messages []openAIMessage) (providerTurn, error) {
	if childName(messages) != "held-a" {
		return providerTurn{}, fmt.Errorf("message exercise reached an unselected child")
	}
	var observed []string
	for _, message := range messages {
		text := contentText(message.Content)
		if message.Role == "user" {
			for _, marker := range []string{"KITTY_INPUT_MARKER", "STEER_MARKER", "FOLLOWUP_MARKER"} {
				if strings.Contains(text, marker) {
					if strings.TrimSpace(text) != marker {
						return providerTurn{}, fmt.Errorf("message exercise received batched or altered marked input")
					}
					observed = append(observed, "user "+marker)
				}
			}
		} else if message.Role == "assistant" && (text == "KITTY_HANDLED" || text == "STEER_HANDLED") {
			observed = append(observed, "assistant "+text)
		}
	}
	if state.messageDrain == drainKitty && len(observed) == 0 {
		return providerTurn{kind: "hold", name: "held-a", text: "CHILD_PRIVATE_RESUME_BOUNDARY"}, nil
	}
	if state.messageDrain == drainComplete {
		return providerTurn{}, fmt.Errorf("message exercise requested another turn after final drain")
	}
	histories := [][]string{
		{"user KITTY_INPUT_MARKER"},
		{"user KITTY_INPUT_MARKER", "assistant KITTY_HANDLED", "user STEER_MARKER"},
		{"user KITTY_INPUT_MARKER", "assistant KITTY_HANDLED", "user STEER_MARKER", "assistant STEER_HANDLED", "user FOLLOWUP_MARKER"},
	}
	want := histories[state.messageDrain]
	if len(observed) != len(want) {
		return providerTurn{}, fmt.Errorf("message drain stage %d has skipped, batched, or unacknowledged inputs", state.messageDrain)
	}
	for index, event := range observed {
		if event != want[index] {
			return providerTurn{}, fmt.Errorf("message drain stage %d has out-of-order role/history", state.messageDrain)
		}
	}
	stage := state.messageDrain
	state.messageDrain++
	if stage == drainKitty {
		return providerTurn{kind: "text", text: "KITTY_HANDLED"}, nil
	}
	if stage == drainSteer {
		return providerTurn{kind: "text", text: "STEER_HANDLED"}, nil
	}
	return providerTurn{kind: "text", text: fixtureReport("MESSAGES_HANDLED")}, nil
}

func (state *fixtureState) holdGate(name string) <-chan struct{} {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.gates[name]
}

func (state *fixtureState) releaseHold(name string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	gate, ok := state.gates[name]
	if !ok || state.released[name] {
		return fmt.Errorf("fixture hold %s is not awaiting release", name)
	}
	close(gate)
	state.released[name] = true
	return nil
}

func (state *fixtureState) completeHold(name string) {
	state.mu.Lock()
	defer state.mu.Unlock()
	delete(state.held, name)
}

func (state *fixtureState) holding(names ...string) bool {
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, name := range names {
		if _, ok := state.held[name]; !ok {
			return false
		}
	}
	return true
}

func (state *fixtureState) modelRequests(model string) int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.requestTotals[model]
}

func assistantContains(messages []openAIMessage, marker string) bool {
	if marker == "" {
		return false
	}
	for _, message := range messages {
		if message.Role == "assistant" && strings.Contains(contentText(message.Content), marker) {
			return true
		}
	}
	return false
}

func (state *fixtureState) totalRequests() int {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.requestCount
}

func (state *fixtureState) requestTotalSnapshot() map[string]int {
	state.mu.Lock()
	defer state.mu.Unlock()
	result := make(map[string]int, len(state.requestTotals))
	for model, count := range state.requestTotals {
		result[model] = count
	}
	return result
}

func (state *fixtureState) saveRequests(directory string) error {
	state.mu.Lock()
	defer state.mu.Unlock()
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	for i, raw := range state.rawRequests {
		if err := writePrivate(filepath.Join(directory, fmt.Sprintf("request-%03d.json", i)), raw); err != nil {
			return err
		}
	}
	return nil
}

func (state *fixtureState) captureRequest(raw []byte) int {
	state.mu.Lock()
	defer state.mu.Unlock()
	index := state.rawRequestCount
	state.rawRequestCount++
	if index < maxEvidenceRequests {
		state.rawRequests = append(state.rawRequests, append([]byte(nil), raw...))
	}
	return index
}
