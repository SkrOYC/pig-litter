package piglitter

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"unicode"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/MichaelKinsy/PiG/tui/widthx"
)

var fileNames = []string{"read", "ls", "write", "edit"}
var controlNames = []string{"litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}
var descriptions = map[string]string{
	"litter_spawn":   "Start a background child. Use litter_list to discover named agent types.",
	"litter_list":    "List available agent types and a bounded page of owned children.",
	"litter_inspect": "Inspect one owned child with an optional bounded transcript page.",
	"litter_message": "Steer a live child or explicitly resume a settled generation.",
	"litter_stop":    "Stop an owned child subtree and report newly stopped runs.",
	"litter_wait":    "Wait for an exact child generation without stopping it on timeout.",
	"read":           "Use the original parent host's read tool.", "ls": "Use the original parent host's ls tool.", "write": "Use the original parent host's write tool.", "edit": "Use the original parent host's edit tool.",
}

func object(required []string, properties map[string]any) map[string]any {
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": properties}
}
func text(maximum int) map[string]any {
	return map[string]any{"type": "string", "minLength": 1, "maxLength": maximum}
}
func integer(minimum, maximum int) map[string]any {
	return map[string]any{"type": "integer", "minimum": minimum, "maximum": maximum}
}

var schemas = map[string]map[string]any{
	"litter_spawn":   object([]string{"type", "task"}, map[string]any{"type": text(64), "task": text(16384), "name": text(128), "model": text(256)}),
	"litter_list":    object([]string{}, map[string]any{"offset": integer(0, 1000000), "limit": integer(1, 50), "agentOffset": integer(0, 1000000), "agentLimit": integer(1, 50)}),
	"litter_inspect": object([]string{"id"}, map[string]any{"id": text(128), "transcript": map[string]any{"type": "boolean"}, "offset": integer(0, 1000000), "limit": integer(1, 50)}),
	"litter_message": object([]string{"id", "generation", "text"}, map[string]any{"id": text(128), "generation": integer(1, 2147483647), "text": text(16384), "mode": map[string]any{"type": "string", "enum": []string{"steer", "follow_up"}}, "resume": map[string]any{"type": "boolean"}}),
	"litter_stop":    object([]string{"id", "generation"}, map[string]any{"id": text(128), "generation": integer(1, 2147483647)}),
	"litter_wait":    object([]string{"id", "generation"}, map[string]any{"id": text(128), "generation": integer(1, 2147483647), "timeoutMs": integer(1, 300000)}),
}

type operationArgs struct {
	Ref
	Type, Task, Text, Mode string
	Name                   *string
	Model                  string
	Offset                 int
	Limit                  *int
	AgentOffset            int
	AgentLimit             *int
	Transcript             bool
	Resume                 bool
	TimeoutMs              *int
}

func pageLimit(request *int, fallback int) int {
	if request == nil {
		return fallback
	}
	return *request
}

func safeLabel(value string) (string, error) {
	if len(value) > 128 {
		return "", reject("rejected", "child name must be at most 128 bytes")
	}
	label := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, widthx.StripTerminalSequences(value))
	label = strings.Join(strings.Fields(label), " ")
	if label == "" {
		return "", reject("rejected", "child name must contain visible text")
	}
	return label, nil
}

func (o *owner) operation(ctx context.Context, name string, raw json.RawMessage, caller Ref, callID string, toolContext *sdk.Context) (map[string]any, error) {
	if err := o.current(); err != nil {
		return nil, err
	}
	var args operationArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return nil, reject("rejected", "invalid tool arguments")
	}
	var parent *record
	if caller.ID != "" {
		o.tree.mu.Lock()
		var err error
		parent, err = o.tree.currentLocked(caller)
		o.tree.mu.Unlock()
		if err != nil {
			return nil, err
		}
	}
	if name == "litter_spawn" {
		trusted, err := o.context.IsProjectTrusted()
		if err != nil {
			return nil, err
		}
		if !trusted {
			return nil, reject("permission_denied", "project trust was revoked")
		}
		agent, exists := o.config.Agents[args.Type]
		if !exists || strings.TrimSpace(args.Task) == "" || len(args.Task) > 16384 {
			return nil, reject("rejected", "choose a named agent and a bounded task")
		}
		if len(args.Task)+len(agent.Instructions)+512 > o.config.MaxHistoryBytes {
			return nil, reject("rejected", "task would exceed the retained history limit")
		}
		if agent.Model != "" && args.Model != "" && args.Model != agent.Model {
			return nil, reject("permission_denied", "the named agent has a fixed model")
		}
		model := agent.Model
		if model == "" {
			model = args.Model
		}
		if model == "" && parent != nil {
			model = parent.model
		}
		if model == "" {
			model = o.context.ModelQualified()
		}
		if _, err := o.modelFor(ctx, model, o.services.ModelRuntime()); err != nil {
			return nil, err
		}
		var bridge *authority
		if parent != nil {
			if !parent.canDelegate {
				return nil, reject("permission_denied", "this agent cannot delegate")
			}
			bridge = parent.authority
			for _, tool := range agent.Tools {
				if !slices.Contains(parent.tools, tool) {
					return nil, reject("permission_denied", "the named agent exceeds the caller tool ceiling")
				}
			}
		} else if toolContext != nil {
			bridge = &authority{context: *toolContext, sessionID: o.sessionID, epoch: o.epoch}
		}
		if bridge == nil {
			return nil, reject("unavailable", "root tool execution bridge is unavailable")
		}
		for _, tool := range agent.Tools {
			if _, err := o.canonicalTool(bridge.context, tool); err != nil {
				return nil, err
			}
		}
		label := ""
		if args.Name != nil {
			label, err = safeLabel(*args.Name)
			if err != nil {
				return nil, err
			}
		}
		manager, err := coding.NewInMemorySessionManager(o.cwd)
		if err != nil {
			return nil, err
		}
		snapshot, err := o.tree.admit(admission{parent: caller.ID, parentGeneration: caller.Generation, kind: args.Type, name: label, model: model, cwd: o.cwd, task: args.Task, agent: agent, history: manager, authority: bridge})
		if err != nil {
			return nil, err
		}
		o.deferActivation(ackKey{caller, callID}, snapshot.Ref, ctx.Done(), func() bool { return ctx.Err() != nil })
		if toolContext != nil {
			o.watchRequest(ackKey{caller, callID}, *toolContext)
		}
		o.widget()
		return map[string]any{"state": snapshot.State, "child": snapshot}, nil
	}
	if name == "litter_list" {
		children, err := o.tree.list(caller.ID)
		if err != nil {
			return nil, err
		}
		limit, agentLimit := pageLimit(args.Limit, 20), pageLimit(args.AgentLimit, 20)
		if args.Offset < 0 || args.Offset > 1000000 || args.AgentOffset < 0 || args.AgentOffset > 1000000 || limit < 1 || limit > 50 || agentLimit < 1 || agentLimit > 50 {
			return nil, reject("rejected", "invalid list page")
		}
		names := make([]string, 0, len(o.config.Agents))
		for name := range o.config.Agents {
			names = append(names, name)
		}
		sort.Strings(names)
		agents := []map[string]any{}
		for _, name := range names[min(args.AgentOffset, len(names)):min(args.AgentOffset+agentLimit, len(names))] {
			definition := o.config.Agents[name]
			description := []rune(definition.Instructions)
			description = description[:min(160, len(description))]
			value := map[string]any{"name": name, "role": definition.Role, "description": string(description), "tools": definition.Tools, "canDelegate": definition.CanDelegate}
			if definition.Model != "" {
				value["model"] = definition.Model
			}
			agents = append(agents, value)
		}
		page := children[min(args.Offset, len(children)):min(args.Offset+limit, len(children))]
		return map[string]any{"state": "listed", "children": page, "nextOffset": args.Offset + len(page), "more": len(children) > args.Offset+limit, "totalChildren": len(children), "availableAgentTypes": agents, "nextAgentOffset": args.AgentOffset + len(agents), "moreAgentTypes": len(names) > args.AgentOffset+agentLimit, "totalAgentTypes": len(names)}, nil
	}
	if err := o.tree.owned(caller.ID, args.ID); err != nil {
		return nil, err
	}
	if name == "litter_inspect" {
		inspection, err := o.tree.inspect(args.ID, args.Transcript, args.Offset, pageLimit(args.Limit, 10))
		if err != nil {
			return nil, err
		}
		return map[string]any{"state": inspection.Child.State, "child": inspection.Child, "outcome": inspection.Outcome, "historyAvailable": inspection.HistoryAvailable, "mailbox": inspection.Mailbox, "mailboxTotal": inspection.MailboxTotal, "mailboxMore": inspection.MailboxMore, "entries": inspection.Entries, "nextOffset": inspection.NextOffset, "more": inspection.More}, nil
	}
	if name == "litter_wait" {
		outcome, err := o.tree.wait(ctx, args.Ref, pageLimit(args.TimeoutMs, min(o.config.MaxRunMillis, 300000)))
		return map[string]any{"state": outcome.State, "outcome": &outcome}, err
	}
	if name == "litter_stop" {
		outcome, count, err := o.tree.stop(args.Ref)
		state := Stopping
		if outcome != nil {
			state = outcome.State
		}
		return map[string]any{"state": state, "outcome": outcome, "stoppedCount": count}, err
	}
	if name == "litter_message" {
		mode := args.Mode
		if mode == "" {
			mode = "steer"
		}
		if args.Resume {
			mode = "resume"
			trusted, err := o.context.IsProjectTrusted()
			if err != nil {
				return nil, err
			}
			if !trusted {
				return nil, reject("permission_denied", "project trust was revoked")
			}
			o.tree.mu.Lock()
			target, err := o.tree.currentLocked(args.Ref)
			var model string
			if err == nil {
				model = target.model
			}
			o.tree.mu.Unlock()
			if err != nil {
				return nil, err
			}
			if _, err := o.modelFor(ctx, model, o.services.ModelRuntime()); err != nil {
				return nil, err
			}
		}
		snapshot, err := o.tree.message(ctx, args.Ref, mode, args.Text)
		if err != nil {
			return nil, err
		}
		delivery := "queued"
		if mode == "resume" {
			delivery = "resumed"
			o.deferActivation(ackKey{caller, callID}, snapshot.Ref, ctx.Done(), func() bool { return ctx.Err() != nil })
			if toolContext != nil {
				o.watchRequest(ackKey{caller, callID}, *toolContext)
			}
		}
		o.widget()
		return map[string]any{"state": snapshot.State, "child": snapshot, "delivery": delivery}, nil
	}
	return nil, reject("rejected", "unknown child operation")
}

func resultContent(operation string, value map[string]any, err error) (string, bool) {
	failed := false
	if err != nil {
		code := "failed"
		var treeErr *treeError
		if errors.As(err, &treeErr) {
			code = treeErr.code
		}
		state := "failed"
		switch code {
		case "wait_timeout":
			state = "timed_out"
		case "cancelled":
			state = "cancelled"
		case "unavailable":
			state = "unavailable"
		case "permission_denied", "rejected":
			state = "rejected"
		}
		value = map[string]any{"state": state, "code": code, "report": err.Error()}
		failed = code != "wait_timeout" && code != "cancelled"
	}
	if value == nil {
		value = map[string]any{}
	}
	value["operation"] = operation
	clipped := false
	if outcome, ok := value["outcome"].(*Outcome); ok && outcome != nil {
		copy := *outcome
		copy.Text, clipped = bounded(copy.Text, 8192)
		value["outcome"] = &copy
	}
	if report, ok := value["report"].(string); ok {
		var truncated bool
		value["report"], truncated = bounded(report, 1024)
		clipped = clipped || truncated
	}
	value["handbackTruncated"] = clipped
	data, marshalErr := json.Marshal(value)
	if marshalErr != nil {
		return `{"state":"failed","code":"failed","report":"cannot encode child result","handbackTruncated":false}`, true
	}
	return string(data), failed
}

func sdkResult(operation string, value map[string]any, err error) sdk.ToolResult {
	text, failed := resultContent(operation, value, err)
	return sdk.ToolResult{Content: text, IsError: failed}
}
func nativeResult(operation string, value map[string]any, err error) agent.AgentToolResult {
	text, failed := resultContent(operation, value, err)
	return agent.AgentToolResult{Content: []ai.ToolResultMessageContent{ai.TextContent{Text: text}}, IsError: failed}
}
