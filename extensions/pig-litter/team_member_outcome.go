package piglitter

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"maps"
	"os"
	"path/filepath"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
)

type memberOutcomeEvidence struct {
	outcome Outcome
	first   *agent.AssistantMessage
	report  string
	count   int
	digest  [32]byte
}

func memberAssistantMessages(snapshot memberLogSnapshot, baseline int) (*agent.AssistantMessage, *agent.AssistantMessage, error) {
	if baseline < 0 || baseline > len(snapshot.entries) {
		return nil, nil, errors.New("native execution baseline is invalid")
	}
	var first, terminal *agent.AssistantMessage
	for _, raw := range snapshot.entries[baseline:] {
		var entry struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Type != "message" {
			continue
		}
		var message agent.AgentMessage
		if err := json.Unmarshal(entry.Message, &message); err != nil {
			return nil, nil, errors.New("native session contains an invalid message")
		}
		if message.Assistant != nil {
			if first == nil {
				first = message.Assistant
			}
			terminal = message.Assistant
		}
	}
	if terminal == nil {
		return nil, nil, reject("unavailable", "native execution has no assistant outcome")
	}
	return first, terminal, nil
}

func (o *owner) memberOutcome(ctx context.Context, binding *memberBinding) (memberOutcomeEvidence, error) {
	var evidence memberOutcomeEvidence
	snapshot, err := o.memberLogSnapshot(ctx, binding)
	if err != nil {
		return evidence, err
	}
	first, terminal, err := memberAssistantMessages(snapshot, binding.log.baselineCount)
	if err != nil {
		return evidence, err
	}
	binding.denialMu.Lock()
	denials := maps.Clone(binding.denials)
	binding.denialMu.Unlock()
	failure, err := memberHistoryFailure(snapshot, binding.log.baselineCount, denials)
	if err != nil {
		return evidence, err
	}
	before, after, err := memberSnapshotStats(ctx, snapshot, binding.log.baselineCount, binding.log.cwd, o.agentDir, binding.log.session)
	if err != nil {
		return evidence, err
	}
	if err := o.current(); err != nil {
		return evidence, err
	}
	o.tree.mu.Lock()
	current := o.memberBindingCurrentLocked(binding)
	o.tree.mu.Unlock()
	if !current {
		return evidence, reject("unavailable", "native execution changed while reading its outcome")
	}
	evidence = memberOutcomeEvidence{outcome: outcomeFrom(terminal, before, after, failure, false, false), first: first, report: idleAssistantReport(terminal), count: len(snapshot.entries), digest: memberEntriesDigest(snapshot.entries)}
	return evidence, nil
}

func memberHistoryFailure(snapshot memberLogSnapshot, baseline int, denials map[memberPlanDenial]string) (string, error) {
	if baseline < 0 || baseline > len(snapshot.entries) {
		return "", errors.New("native execution baseline is invalid")
	}
	response := ""
	for _, raw := range snapshot.entries[baseline:] {
		var entry struct {
			ID      string          `json:"id"`
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if json.Unmarshal(raw, &entry) != nil || entry.Type != "message" {
			continue
		}
		var message agent.AgentMessage
		if err := json.Unmarshal(entry.Message, &message); err != nil {
			return "", errors.New("native session contains an invalid message")
		}
		if message.Assistant != nil {
			response = entry.ID
			continue
		}
		if message.ToolResult == nil {
			continue
		}
		var nested struct {
			Calls *struct {
				Calls []struct {
					ID, Name, Status, Error string
				} `json:"calls"`
				Complete bool `json:"complete"`
			} `json:"nestedCalls"`
		}
		if err := json.Unmarshal(entry.Message, &nested); err != nil {
			return "", errors.New("native nested tool history is invalid")
		}
		if nested.Calls != nil {
			for _, call := range nested.Calls.Calls {
				if call.Status == "ok" {
					continue
				}
				if reason, expected := denials[memberPlanDenial{response: response, call: planDenial{ID: call.ID, Name: call.Name}}]; expected && call.Status == "error" && call.Error == reason {
					continue
				}
				result := agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: call.Error}}, IsError: true}
				return toolFailure(call.Name, result), nil
			}
			if !nested.Calls.Complete {
				return "native nested tool history is incomplete", nil
			}
		}
		result := message.ToolResult
		if result.IsError {
			reason, expected := denials[memberPlanDenial{response: response, call: planDenial{ID: result.ToolCallID, Name: result.ToolName}}]
			if len(result.Content) == 1 {
				if text, ok := result.Content[0].(ai.TextContent); ok && expected && text.Text == reason {
					continue
				}
			}
			return toolFailure(result.ToolName, result.Result()), nil
		}
	}
	return "", nil
}

func memberSnapshotStats(ctx context.Context, snapshot memberLogSnapshot, baseline int, cwd, agentDir string, session ChildID) (coding.SessionStats, coding.SessionStats, error) {
	var before, after coding.SessionStats
	if baseline < 0 || baseline > len(snapshot.entries) {
		return before, after, errors.New("native execution baseline is invalid")
	}
	dir, err := os.MkdirTemp("", "pig-member-outcome-")
	if err != nil {
		return before, after, err
	}
	defer os.RemoveAll(dir)
	trusted := true
	services, err := coding.CreateAgentSessionServices(coding.CreateAgentSessionServicesOptions{CWD: cwd, AgentDir: agentDir, ProjectTrusted: &trusted})
	if err != nil {
		return before, after, err
	}
	defer services.Close()
	read := func(name string, entries []json.RawMessage) (coding.SessionStats, error) {
		var stats coding.SessionStats
		path := filepath.Join(dir, name)
		file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err != nil {
			return stats, err
		}
		writer := bufio.NewWriter(file)
		write := func(raw json.RawMessage) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			var compact bytes.Buffer
			if err := json.Compact(&compact, raw); err != nil {
				return err
			}
			if _, err := writer.Write(compact.Bytes()); err != nil {
				return err
			}
			return writer.WriteByte('\n')
		}
		err = write(snapshot.header)
		for _, entry := range entries {
			if err != nil {
				break
			}
			err = write(entry)
		}
		if err == nil {
			err = writer.Flush()
		}
		if err = errors.Join(err, file.Close()); err != nil {
			return stats, err
		}
		manager, err := coding.SessionManagerFor(services, coding.SessionStartOptions{ResumePath: path, SessionDir: dir})
		if err != nil {
			return stats, err
		}
		if manager.ID() != string(session) || manager.GetCwd() != cwd {
			return stats, errors.New("native outcome snapshot identity changed")
		}
		accounting := manager.Accounting()
		stats.Tokens = coding.SessionStatsTokens{Input: accounting.Tokens.Input, Output: accounting.Tokens.Output, CacheRead: accounting.Tokens.CacheRead, CacheWrite: accounting.Tokens.CacheWrite, Total: accounting.Tokens.Total}
		stats.Cost = accounting.Tokens.Cost
		return stats, nil
	}
	before, err = read("baseline.jsonl", snapshot.entries[:baseline])
	if err != nil {
		return before, after, err
	}
	after, err = read("settled.jsonl", snapshot.entries)
	return before, after, err
}
