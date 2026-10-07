package piglitter

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

type generationMarker struct {
	Run Ref `json:"run"`
}

type ToolReceipt struct {
	Generation    int    `json:"generation"`
	Tool          string `json:"tool"`
	CallID        string `json:"callId"`
	Path          string `json:"path,omitempty"`
	IsError       bool   `json:"isError"`
	Text          string `json:"text"`
	TextBlocks    int    `json:"textBlocks"`
	NonTextBlocks int    `json:"nonTextBlocks"`
	EntryID       string `json:"entryId"`
	EntryOffset   int    `json:"entryOffset"`
	Truncated     bool   `json:"truncated"`
}

type ToolEvidence struct {
	Source               string        `json:"source"`
	TextFormat           string        `json:"textFormat"`
	Unavailable          string        `json:"unavailable,omitempty"`
	CurrentTotal         int           `json:"currentTotal"`
	CurrentFailed        int           `json:"currentFailed"`
	Current              []ToolReceipt `json:"current"`
	FirstFailure         *ToolReceipt  `json:"firstFailure,omitempty"`
	PreviousFailureTotal int           `json:"previousFailureTotal"`
	PreviousFailures     []ToolReceipt `json:"previousFailures"`
	More                 bool          `json:"more"`
	Truncated            bool          `json:"truncated"`
}

type historyToolEntry struct {
	Type, ID, CustomType string
	Data                 json.RawMessage
	Message              json.RawMessage
}

type historyToolMessage struct {
	Role, ToolName, ToolCallID string
	IsError                    bool
	Content                    json.RawMessage
}

type historyToolBlock struct {
	Type, Text, ID, Name string
	Arguments            map[string]json.RawMessage
}

func historyEntries(manager *coding.SessionManager) []json.RawMessage {
	entries := manager.Entries()
	result := make([]json.RawMessage, 0, len(entries))
	for _, entry := range entries {
		result = append(result, entry.Raw())
	}
	return result
}

func appendGeneration(manager *coding.SessionManager, ref Ref, maximum int) error {
	if _, err := manager.AppendCustomEntry("litter_generation", generationMarker{Run: ref}); err != nil {
		return err
	}
	if historyBytes(manager) > maximum {
		return reject("unavailable", "child generation metadata exceeded the retained history limit")
	}
	return nil
}

func projectEvidence(ref Ref, entries []json.RawMessage) ToolEvidence {
	evidence := ToolEvidence{Source: "retained_tool_results", TextFormat: "concatenated_text_blocks", Current: []ToolReceipt{}, PreviousFailures: []ToolReceipt{}}
	generation := 0
	paths := map[string]string{}
	for offset, raw := range entries {
		var entry historyToolEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			evidence.Unavailable = "cannot decode retained tool history"
			return boundedEvidence(evidence)
		}
		if entry.Type == "custom" && entry.CustomType == "litter_generation" {
			var marker generationMarker
			if json.Unmarshal(entry.Data, &marker) != nil || marker.Run.ID != ref.ID || marker.Run.Generation < 1 || marker.Run.Generation > ref.Generation || marker.Run.Generation <= generation {
				evidence.Unavailable = "invalid retained generation metadata"
				return boundedEvidence(evidence)
			}
			generation = marker.Run.Generation
			paths = map[string]string{}
			continue
		}
		if entry.Type != "message" {
			continue
		}
		var message historyToolMessage
		if json.Unmarshal(entry.Message, &message) != nil {
			evidence.Unavailable = "cannot decode retained message"
			return boundedEvidence(evidence)
		}
		if message.Role != "assistant" && message.Role != "toolResult" {
			continue
		}
		var blocks []json.RawMessage
		if json.Unmarshal(message.Content, &blocks) != nil {
			evidence.Unavailable = "cannot decode retained tool content"
			return boundedEvidence(evidence)
		}
		if message.Role == "assistant" {
			for _, rawBlock := range blocks {
				var block historyToolBlock
				if json.Unmarshal(rawBlock, &block) != nil {
					evidence.Unavailable = "cannot decode retained tool call"
					return boundedEvidence(evidence)
				}
				if block.Type == "toolCall" {
					var path string
					_ = json.Unmarshal(block.Arguments["path"], &path)
					paths[block.ID] = path
				}
			}
			continue
		}
		if generation == 0 {
			evidence.Unavailable = "retained tool results have no generation metadata"
			return boundedEvidence(evidence)
		}
		receipt := ToolReceipt{Generation: generation, Tool: message.ToolName, CallID: message.ToolCallID, Path: paths[message.ToolCallID], IsError: message.IsError, EntryID: entry.ID, EntryOffset: offset}
		var text strings.Builder
		for _, rawBlock := range blocks {
			var block historyToolBlock
			if json.Unmarshal(rawBlock, &block) != nil {
				evidence.Unavailable = "cannot decode retained tool result"
				return boundedEvidence(evidence)
			}
			if block.Type == "text" {
				receipt.TextBlocks++
				text.WriteString(block.Text)
			} else {
				receipt.NonTextBlocks++
			}
		}
		receipt.Text = text.String()
		receipt = clipReceipt(receipt, 512)
		if generation == ref.Generation {
			evidence.CurrentTotal++
			if receipt.IsError {
				evidence.CurrentFailed++
				if evidence.FirstFailure == nil {
					copy := receipt
					evidence.FirstFailure = &copy
				}
			}
			evidence.Current = append(evidence.Current, receipt)
			if len(evidence.Current) > 8 {
				evidence.Current = evidence.Current[1:]
			}
		} else if receipt.IsError {
			evidence.PreviousFailureTotal++
			evidence.PreviousFailures = append(evidence.PreviousFailures, receipt)
			if len(evidence.PreviousFailures) > 8 {
				evidence.PreviousFailures = evidence.PreviousFailures[1:]
			}
		}
	}
	return boundedEvidence(evidence)
}

func boundedEvidence(evidence ToolEvidence) ToolEvidence {
	for preview := 512; ; {
		raw, _ := json.Marshal(evidence)
		if len(raw) <= 4096 {
			break
		}
		evidence.Truncated = true
		if len(evidence.PreviousFailures) > 1 {
			evidence.PreviousFailures = evidence.PreviousFailures[1:]
		} else if len(evidence.Current) > 1 {
			evidence.Current = evidence.Current[1:]
		} else {
			preview /= 2
			for i := range evidence.Current {
				evidence.Current[i] = clipReceipt(evidence.Current[i], preview)
			}
			for i := range evidence.PreviousFailures {
				evidence.PreviousFailures[i] = clipReceipt(evidence.PreviousFailures[i], preview)
			}
			if evidence.FirstFailure != nil {
				copy := clipReceipt(*evidence.FirstFailure, preview)
				evidence.FirstFailure = &copy
			}
		}
	}
	currentIncluded := len(evidence.Current)
	if evidence.FirstFailure != nil {
		found := false
		for _, receipt := range evidence.Current {
			found = found || receipt.EntryOffset == evidence.FirstFailure.EntryOffset
		}
		if !found {
			currentIncluded++
		}
	}
	evidence.More = currentIncluded < evidence.CurrentTotal || len(evidence.PreviousFailures) < evidence.PreviousFailureTotal
	for _, receipt := range evidence.Current {
		evidence.Truncated = evidence.Truncated || receipt.Truncated
	}
	for _, receipt := range evidence.PreviousFailures {
		evidence.Truncated = evidence.Truncated || receipt.Truncated
	}
	if evidence.FirstFailure != nil {
		evidence.Truncated = evidence.Truncated || evidence.FirstFailure.Truncated
	}
	return evidence
}

func clipReceipt(receipt ToolReceipt, preview int) ToolReceipt {
	for _, field := range []struct {
		value   *string
		maximum int
	}{{&receipt.Text, preview}, {&receipt.Path, min(256, preview)}, {&receipt.CallID, min(128, preview)}, {&receipt.Tool, min(64, preview)}, {&receipt.EntryID, min(64, preview)}} {
		var clipped bool
		*field.value, clipped = bounded(*field.value, field.maximum)
		receipt.Truncated = receipt.Truncated || clipped
	}
	return receipt
}

func toolFailure(tool string, result agent.AgentToolResult) string {
	var text strings.Builder
	for _, block := range result.Content {
		if block, ok := block.(ai.TextContent); ok {
			text.WriteString(block.Text)
		}
	}
	preview, clipped := bounded(text.String(), 512)
	message := fmt.Sprintf("tool %s failed", tool)
	if preview != "" {
		message += ": " + preview
	}
	if clipped {
		message += " [truncated; inspect the retained transcript for the full result]"
	}
	return message
}
