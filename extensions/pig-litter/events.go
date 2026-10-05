package pig_litter

import (
	"bufio"
	"encoding/json"
	"strings"
)

type childMessage struct {
	Role         string `json:"role"`
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	StopReason   string `json:"stopReason"`
	ErrorMessage string `json:"errorMessage"`
	Content      []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Usage struct {
		Input       int64 `json:"input"`
		Output      int64 `json:"output"`
		CacheRead   int64 `json:"cacheRead"`
		CacheWrite  int64 `json:"cacheWrite"`
		TotalTokens int64 `json:"totalTokens"`
		Cost        struct {
			Total float64 `json:"total"`
		} `json:"cost"`
	} `json:"usage"`
}

type childEvent struct {
	Type    string          `json:"type"`
	Message json.RawMessage `json:"message"`
}

func parseChild(stdout string, exitCode int, killed bool, request LaunchRequest, model string) Result {
	result := Result{State: Partial, AgentType: request.AgentType, Model: model, Report: "Child ended without a terminal assistant response."}
	if len(stdout) > maxOutputBytes {
		result.Report = "Child JSON output exceeds the parser limit."
		if killed {
			result.State = Stopped
		} else if exitCode != 0 {
			result.State = Failed
		}
		return result
	}
	scanner := bufio.NewScanner(strings.NewReader(stdout))
	scanner.Buffer(make([]byte, 4096), maxEventBytes)
	terminal := false
	malformed := false
	hasReport := false
	var last *childMessage
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var event childEvent
		if json.Unmarshal(line, &event) != nil || event.Type == "" {
			malformed = true
			break
		}
		switch event.Type {
		case "agent_start", "message_start", "message_update", "tool_execution_start", "tool_execution_end":
			terminal = false
		case "message_end":
			terminal = false
			if len(event.Message) == 0 {
				malformed = true
				break
			}
			var role struct {
				Role string `json:"role"`
			}
			if json.Unmarshal(event.Message, &role) != nil {
				malformed = true
				break
			}
			if role.Role != "assistant" {
				continue
			}
			var message childMessage
			if json.Unmarshal(event.Message, &message) != nil {
				malformed = true
				break
			}
			last = &message
			result.Usage.Input += message.Usage.Input
			result.Usage.Output += message.Usage.Output
			result.Usage.CacheRead += message.Usage.CacheRead
			result.Usage.CacheWrite += message.Usage.CacheWrite
			result.Usage.TotalTokens += message.Usage.TotalTokens
			result.Usage.Cost += message.Usage.Cost.Total
		case "agent_end":
			terminal = true
		}
	}
	if scanner.Err() != nil {
		malformed = true
	}
	if last != nil {
		if last.Provider != "" && last.Model != "" {
			result.Model = last.Provider + "/" + last.Model
		}
		var report strings.Builder
		for _, block := range last.Content {
			if block.Type == "text" {
				report.WriteString(block.Text)
			}
		}
		if last.ErrorMessage != "" {
			report.WriteString(last.ErrorMessage)
		}
		hasReport = strings.TrimSpace(report.String()) != ""
		result.Report, result.ReportTruncated = bounded(report.String(), maxReportBytes)
		if result.Report == "" {
			result.Report = "Child produced no text report."
		}
	}
	switch {
	case killed:
		result.State = Stopped
	case exitCode != 0:
		result.State = Failed
	case last != nil && last.StopReason == "error":
		result.State = Failed
	case last != nil && last.StopReason == "aborted":
		result.State = Stopped
	case malformed:
		result.State = Partial
		result.Report = "Child emitted invalid or oversized JSON events."
	case !terminal || last == nil:
		result.State = Partial
	case last.Provider+"/"+last.Model != model:
		result.State = Failed
		result.Report = "Child resolved a different model than requested."
	case !hasReport:
		result.State = Partial
	case last.StopReason == "stop":
		result.State = Completed
	default:
		result.State = Partial
	}
	return result
}
