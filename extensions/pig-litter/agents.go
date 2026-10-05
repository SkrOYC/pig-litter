package pig_litter

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	maxTaskBytes     = 16 * 1024
	maxReportBytes   = 8 * 1024
	maxEventBytes    = 1024 * 1024
	maxOutputBytes   = 8 * 1024 * 1024
	defaultTimeoutMS = 120000
	maxTimeoutMS     = 300000
)

type AgentDefinition struct {
	Name   string
	Tools  []string
	Prompt string
}

var agents = map[string]AgentDefinition{
	"scout":  {Name: "scout", Tools: []string{"read", "ls"}, Prompt: "You are Pig Litter's scout. Inspect files for the supplied task. Do not change files. Return a concise factual report with relevant paths and uncertainty."},
	"worker": {Name: "worker", Tools: []string{"read", "write", "edit", "ls"}, Prompt: "You are Pig Litter's worker. Complete the supplied task using the available file tools. You share the working directory with others. Preserve unrelated changes. Return a concise report of changes, checks, and remaining uncertainty."},
}

type LaunchRequest struct {
	AgentType string `json:"type"`
	Task      string `json:"task"`
	Model     string `json:"model,omitempty"`
	Mode      string `json:"mode,omitempty"`
	TimeoutMS int    `json:"timeoutMs,omitempty"`
}

type Outcome string

const (
	Completed   Outcome = "completed"
	Failed      Outcome = "failed"
	Stopped     Outcome = "stopped"
	Partial     Outcome = "partial"
	Unavailable Outcome = "unavailable"
	Rejected    Outcome = "rejected"
)

type Usage struct {
	Input       int64   `json:"input"`
	Output      int64   `json:"output"`
	CacheRead   int64   `json:"cacheRead"`
	CacheWrite  int64   `json:"cacheWrite"`
	TotalTokens int64   `json:"totalTokens"`
	Cost        float64 `json:"cost"`
}

type Result struct {
	State           Outcome `json:"state"`
	AgentType       string  `json:"type"`
	Model           string  `json:"model,omitempty"`
	Report          string  `json:"report"`
	ReportTruncated bool    `json:"reportTruncated"`
	Usage           Usage   `json:"usage"`
}

func parseRequest(params map[string]any) (LaunchRequest, error) {
	for key := range params {
		switch key {
		case "type", "task", "model", "mode", "timeoutMs":
		default:
			return LaunchRequest{}, fmt.Errorf("unknown argument; allowed arguments are type, task, model, mode, and timeoutMs")
		}
	}
	request := LaunchRequest{TimeoutMS: defaultTimeoutMS}
	for key, target := range map[string]*string{"type": &request.AgentType, "task": &request.Task, "model": &request.Model, "mode": &request.Mode} {
		if value, exists := params[key]; exists {
			text, ok := value.(string)
			if !ok {
				return request, fmt.Errorf("%s must be a string", key)
			}
			*target = text
		}
	}
	if _, ok := agents[request.AgentType]; !ok {
		return request, fmt.Errorf("unknown agent type; choose scout or worker")
	}
	if strings.TrimSpace(request.Task) == "" || len(request.Task) > maxTaskBytes || !utf8.ValidString(request.Task) {
		return request, fmt.Errorf("task must contain 1 to %d bytes of UTF-8 text", maxTaskBytes)
	}
	if request.Mode != "" && request.Mode != "foreground" {
		return request, fmt.Errorf("only foreground mode is available; background sessions are unavailable")
	}
	if len(request.Model) > 256 {
		return request, fmt.Errorf("model exceeds 256 bytes")
	}
	if value, exists := params["timeoutMs"]; exists {
		number, ok := value.(float64)
		if !ok || number < 1 || number > maxTimeoutMS || number != float64(int(number)) {
			return request, fmt.Errorf("timeoutMs must be an integer from 1 to %d", maxTimeoutMS)
		}
		request.TimeoutMS = int(number)
	}
	return request, nil
}

func childArgs(request LaunchRequest, model string) []string {
	definition := agents[request.AgentType]
	return []string{"--mode", "json", "--print", "--no-session", "--no-extensions", "--no-skills", "--no-prompt-templates", "--no-themes", "--tools", strings.Join(definition.Tools, ","), "--model", model, "--system-prompt", definition.Prompt, "--", "Task:\n" + request.Task}
}

func bounded(text string, limit int) (string, bool) {
	if len(text) <= limit {
		return text, false
	}
	text = text[:limit]
	for !utf8.ValidString(text) {
		text = text[:len(text)-1]
	}
	return text, true
}
