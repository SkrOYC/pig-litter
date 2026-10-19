package piglitter

import (
	"encoding/json"

	"github.com/MichaelKinsy/PiG/coding"
)

type outcomeHandback struct {
	CallID    string  `json:"callId"`
	Operation string  `json:"operation"`
	After     *string `json:"after,omitempty"`
}

type completionAudit struct {
	Content  string            `json:"content"`
	Details  completionDetails `json:"details"`
	Handback *outcomeHandback  `json:"handback,omitempty"`
}

type completionEntry struct {
	ID         string            `json:"id"`
	CustomType string            `json:"customType"`
	Content    string            `json:"content"`
	Details    completionDetails `json:"details"`
	Data       json.RawMessage   `json:"data"`
	Message    *struct {
		Role       string            `json:"role"`
		CustomType string            `json:"customType"`
		Content    json.RawMessage   `json:"content"`
		Details    completionDetails `json:"details"`
		ToolCallID string            `json:"toolCallId"`
		ToolName   string            `json:"toolName"`
		IsError    bool              `json:"isError"`
	} `json:"message"`
}

func handbackStored(entries []json.RawMessage, handback *outcomeHandback, child ChildID) bool {
	return storedHandback(entries, handback, child) != nil
}

func storedHandback(entries []json.RawMessage, handback *outcomeHandback, child ChildID) *Outcome {
	if handback == nil || handback.After == nil || handback.CallID == "" || handback.Operation != "litter_wait" && handback.Operation != "litter_stop" {
		return nil
	}
	after := *handback.After == ""
	for _, raw := range entries {
		var entry completionEntry
		if json.Unmarshal(raw, &entry) != nil || entry.ID == "" {
			continue
		}
		if entry.ID == *handback.After {
			after = true
			continue
		}
		message := entry.Message
		if !after || message == nil || message.Role != "toolResult" || message.IsError || message.ToolCallID != handback.CallID || message.ToolName != handback.Operation {
			continue
		}
		var blocks []struct{ Type, Text string }
		if json.Unmarshal(message.Content, &blocks) != nil {
			continue
		}
		for _, block := range blocks {
			var result struct {
				Operation string   `json:"operation"`
				Outcome   *Outcome `json:"outcome"`
			}
			if block.Type == "text" && json.Unmarshal([]byte(block.Text), &result) == nil && result.Operation == handback.Operation && result.Outcome != nil && result.Outcome.Run.ID == child && result.Outcome.State.terminal() {
				return result.Outcome
			}
		}
	}
	return nil
}

func handbackMatches(entries []json.RawMessage, handback *outcomeHandback, details completionDetails, content string) bool {
	outcome := storedHandback(entries, handback, details.ChildID)
	if outcome == nil {
		return false
	}
	stored, err := completionContent(*outcome)
	return err == nil && stored == content
}

func completionStored(entries []json.RawMessage, details completionDetails, content string) bool {
	for _, raw := range entries {
		var entry completionEntry
		if json.Unmarshal(raw, &entry) != nil || entry.ID == "" {
			continue
		}
		if entry.CustomType == "litter_completion" && entry.Details == details && entry.Content == content {
			return true
		}
		if entry.Message != nil && entry.Message.CustomType == "litter_completion" && entry.Message.Details == details {
			var text string
			if json.Unmarshal(entry.Message.Content, &text) == nil && text == content {
				return true
			}
		}
		var audit completionAudit
		if entry.CustomType == "litter_completion" && json.Unmarshal(entry.Data, &audit) == nil && audit.Details == details && audit.Content == content && handbackMatches(entries, audit.Handback, details, content) {
			return true
		}
	}
	return false
}

func consumedCompletions(entries []json.RawMessage, root ChildID) map[completionDetails]string {
	authority := map[completionDetails]string{}
	for _, raw := range entries {
		var entry completionEntry
		var child savedChild
		if json.Unmarshal(raw, &entry) != nil || entry.ID == "" || entry.CustomType != childEntryType || json.Unmarshal(entry.Data, &child) != nil || child.RootID != root || child.ParentID != root || !childUUID.MatchString(string(child.ID)) || !child.Admitted && !spawnStored(entries, child.ID) {
			continue
		}
		for _, notice := range child.Notices {
			if notice.Nonce == "" || notice.Outcome.Run.ID != child.ID || !notice.Outcome.State.terminal() {
				continue
			}
			content, err := completionContent(notice.Outcome)
			if err == nil {
				authority[completionDetails{ChildID: child.ID, Receipt: notice.Nonce}] = content
			}
		}
	}
	consumed := map[completionDetails]string{}
	for _, raw := range entries {
		var entry completionEntry
		var audit completionAudit
		if json.Unmarshal(raw, &entry) != nil || entry.ID == "" || entry.CustomType != "litter_completion" || json.Unmarshal(entry.Data, &audit) != nil || audit.Content == "" || authority[audit.Details] != audit.Content {
			continue
		}
		if handbackMatches(entries, audit.Handback, audit.Details, audit.Content) {
			consumed[audit.Details] = audit.Content
		}
	}
	return consumed
}

func (o *owner) persistedCompletionEntries(parentID ChildID) ([]json.RawMessage, error) {
	cwd, directory := o.cwd, ""
	if o.tree.rootID(parentID) {
		file, err := o.context.SessionManager().GetSessionFile()
		if err != nil {
			return nil, err
		}
		if file == nil {
			return o.completionEntries(parentID)
		}
		directory, err = o.context.SessionManager().GetSessionDir()
		if err != nil {
			return nil, err
		}
		parentID = ChildID(o.sessionID)
	} else {
		o.tree.mu.Lock()
		parent := o.tree.records[parentID]
		var active *coding.SessionManager
		if parent != nil {
			cwd, active = parent.cwd, parent.activeLog
		}
		o.tree.mu.Unlock()
		if parent == nil {
			return nil, reject("unavailable", "parent UUID is unavailable")
		}
		if active != nil {
			cwd, directory = active.GetCwd(), active.GetSessionDir()
		}
	}
	trusted := true
	services, err := coding.NewServices(coding.ServicesOptions{CWD: cwd, AgentDir: o.agentDir, ProjectTrusted: &trusted})
	if err != nil {
		return nil, err
	}
	defer services.Close()
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: services})
	if err != nil {
		return nil, err
	}
	defer runtime.Close()
	session, err := runtime.Resume(string(parentID), coding.SessionStartOptions{SessionDir: directory, ResourceLoader: coding.NoResources, AllowedTools: map[string]struct{}{}})
	if err != nil {
		return nil, err
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	defer func() { _ = session.Close(); <-drained }()
	return rawEntries(session.SessionManager().Entries())
}
