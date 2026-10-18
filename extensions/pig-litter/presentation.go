package piglitter

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/MichaelKinsy/PiG/tui/widthx"
)

const (
	paintByteLimit = 64 * 1024
	toolRowLimit   = 80
)

type toolPresentation struct {
	Operation           string     `json:"operation"`
	State               string     `json:"state"`
	Code                string     `json:"code"`
	Report              string     `json:"report"`
	Child               *Snapshot  `json:"child"`
	Outcome             *Outcome   `json:"outcome"`
	Children            []Snapshot `json:"children"`
	Delivery            string     `json:"delivery"`
	StoppedCount        *int       `json:"stoppedCount"`
	TotalChildren       int        `json:"totalChildren"`
	More                bool       `json:"more"`
	TotalAgentTypes     int        `json:"totalAgentTypes"`
	MoreAgentTypes      bool       `json:"moreAgentTypes"`
	HandbackTruncated   bool       `json:"handbackTruncated"`
	AvailableAgentTypes []struct {
		Name, Role, Description, Model string
	} `json:"availableAgentTypes"`
}

func paintText(raw string, multiline bool) string {
	var out strings.Builder
	state := byte(0)
	for _, r := range strings.ToValidUTF8(raw, "\uFFFD") {
		switch state {
		case 's', 't':
			if r == '\a' || r == '\u009c' || state == 't' && r == '\\' {
				state = 0
			} else if r == '\x1b' {
				state = 't'
			} else {
				state = 's'
			}
			continue
		case 'c':
			if r >= 0x40 && r <= 0x7e {
				state = 0
			}
			continue
		case 'e':
			switch r {
			case '[':
				state = 'c'
			case ']', 'P', 'X', '^', '_':
				state = 's'
			default:
				if r < 0x20 || r > 0x2f {
					state = 0
				}
			}
			continue
		}
		switch r {
		case '\x1b':
			state = 'e'
		case '\u009b':
			state = 'c'
		case '\u0090', '\u0098', '\u009d', '\u009e', '\u009f':
			state = 's'
		case '\n':
			if multiline {
				out.WriteByte('\n')
			} else {
				out.WriteByte(' ')
			}
		case '\r':
		case '\t':
			out.WriteString("   ")
		default:
			if unicode.IsControl(r) || r == '\u061c' || r == '\u200e' || r == '\u200f' || r >= '\u202a' && r <= '\u202e' || r >= '\u2066' && r <= '\u2069' {
				continue
			}
			if r == '\u2028' || r == '\u2029' {
				r = ' '
			}
			out.WriteRune(r)
		}
	}
	clean := out.String()
	if len(clean) <= paintByteLimit {
		return clean
	}
	end := 0
	for rest := clean; rest != ""; {
		cluster, remaining := widthx.FirstGrapheme(rest)
		if end+len(cluster) > paintByteLimit {
			break
		}
		end += len(cluster)
		rest = remaining
	}
	return clean[:end] + " [display text clipped]"
}

func toolCallRenderer(operation string) sdk.ToolRenderCallFunc {
	return func(_ sdk.Context, args map[string]any, render sdk.ToolRenderContext, width int) ([]string, error) {
		arg := func(key string) string { value, _ := args[key].(string); return value }
		target := arg("id")
		if !render.Expanded {
			target = toolTarget(target)
		}
		if operation == "litter_spawn" {
			target = arg("type")
			if name := arg("name"); name != "" {
				target += " as " + name
			}
		}
		header := strings.TrimSpace(operation + " " + target)
		preview := arg("task")
		if operation == "litter_message" {
			mode := arg("mode")
			if mode == "" {
				mode = "steer"
			}
			if resume, _ := args["resume"].(bool); resume {
				mode = "resume"
			}
			header += " " + mode
			preview = arg("text")
		}
		if !render.Expanded {
			if preview != "" {
				header += " | " + preview
			}
			return []string{widthx.StripTerminalSequences(widthx.TruncateToWidth(paintText(header, false), width, "…", false))}, nil
		}
		lines := []string{header}
		for _, field := range []struct{ key, label string }{{"model", "Model"}, {"cwd", "Cwd"}} {
			if value := arg(field.key); value != "" {
				lines = append(lines, field.label+" "+value)
			}
		}
		if timeout, ok := args["timeoutMs"].(float64); ok {
			lines = append(lines, fmt.Sprintf("Timeout %.0f ms", timeout))
		}
		return toolPaintRows(lines, preview, width), nil
	}
}

func toolResultRenderer(operation string) sdk.ToolRenderResultFunc {
	return func(_ sdk.Context, result sdk.ToolRenderResult, options sdk.ToolRenderResultOptions, render sdk.ToolRenderContext, width int) ([]string, error) {
		var view toolPresentation
		found := false
		for _, block := range result.Content {
			if block["type"] != "text" {
				continue
			}
			text, ok := block["text"].(string)
			if !ok {
				return nil, fmt.Errorf("missing tool result text")
			}
			if err := json.Unmarshal([]byte(text), &view); err != nil {
				return nil, err
			}
			found = true
			break
		}
		if !found || view.Operation != operation || view.State == "" {
			return nil, fmt.Errorf("unrecognized %s handback", operation)
		}
		header := view.State
		if view.Child != nil {
			header += " " + toolChildLabel(*view.Child)
		} else if view.Outcome != nil {
			header += " " + toolTarget(string(view.Outcome.Run.ID))
		}
		if operation == "litter_list" && view.State == "listed" {
			header += fmt.Sprintf(" %d/%d children | %d/%d types", len(view.Children), view.TotalChildren, len(view.AvailableAgentTypes), view.TotalAgentTypes)
		}
		if view.Delivery != "" {
			header += " " + view.Delivery
		}
		if options.IsPartial || render.IsPartial {
			header += " (receiving)"
		}
		preview := view.Report
		if view.Outcome != nil {
			preview = view.Outcome.Error
			if preview == "" {
				preview = view.Outcome.Text
			}
		}
		if !options.Expanded {
			if preview != "" {
				header += " | " + preview
			}
			return []string{widthx.StripTerminalSequences(widthx.TruncateToWidth(paintText(header, false), width, "…", false))}, nil
		}
		lines := []string{header}
		if view.Child != nil {
			lines = append(lines, toolChildDetails(*view.Child)...)
		} else if view.Outcome != nil {
			lines = append(lines, "UUID "+string(view.Outcome.Run.ID), "Model "+view.Outcome.Model)
		}
		for _, child := range view.Children {
			lines = append(lines, toolChildLabel(child)+" "+string(child.State), "  UUID "+string(child.ID), "  Model "+child.Model, "  Cwd "+child.Cwd)
		}
		for _, agent := range view.AvailableAgentTypes {
			lines = append(lines, "Agent "+agent.Name+" "+agent.Role, agent.Description)
			if agent.Model != "" {
				lines = append(lines, "  Model "+agent.Model)
			}
		}
		if view.More {
			lines = append(lines, "More children available")
		}
		if view.MoreAgentTypes {
			lines = append(lines, "More agent types available")
		}
		if view.StoppedCount != nil {
			lines = append(lines, fmt.Sprintf("Paused executions %d", *view.StoppedCount))
		}
		if view.Code != "" {
			lines = append(lines, "Code "+view.Code)
		}
		body := view.Report
		if view.Outcome != nil {
			outcome := view.Outcome
			lines = append(lines, "Outcome "+string(outcome.State))
			if outcome.Error != "" {
				lines = append(lines, "Error "+outcome.Error)
			}
			usage := outcome.Usage
			lines = append(lines, fmt.Sprintf("Usage %d in / %d out / %d total / $%.6f", usage.Input, usage.Output, usage.TotalTokens, usage.Cost))
			if usage.CacheRead != 0 || usage.CacheWrite != 0 {
				lines = append(lines, fmt.Sprintf("Cache %d read / %d write", usage.CacheRead, usage.CacheWrite))
			}
			if outcome.ReportProvenance != "" {
				lines = append(lines, "Report source "+outcome.ReportProvenance)
			}
			if outcome.Truncated {
				lines = append(lines, "Result truncated")
			}
			if outcome.HandbackTruncated {
				lines = append(lines, "Outcome handback truncated")
			}
			body = outcome.Text
		}
		if view.HandbackTruncated {
			lines = append(lines, "Handback truncated")
		}
		if body != "" {
			lines = append(lines, "Report")
		}
		return toolPaintRows(lines, body, width), nil
	}
}

func toolTarget(id string) string {
	if len(id) > 8 {
		return "#" + id[len(id)-8:]
	}
	return id
}

func toolChildLabel(child Snapshot) string {
	if child.Name == string(child.ID) {
		return toolTarget(string(child.ID))
	}
	label := child.DisplayLabel
	if label == "" {
		label = child.Name
	}
	if label == "" {
		return toolTarget(string(child.ID))
	}
	return label + " #" + string(child.ID)[max(0, len(child.ID)-8):]
}

func toolChildDetails(child Snapshot) []string {
	return []string{"UUID " + string(child.ID), "Name " + child.Name, "Type " + child.Type, "Model " + child.Model, "Cwd " + child.Cwd}
}

func toolPaintRows(metadata []string, body string, width int) []string {
	if width <= 0 {
		return []string{}
	}
	var rows []string
	for _, line := range metadata {
		rows = append(rows, widthx.WrapTextWithAnsi(paintText(line, false), width)...)
	}
	if body != "" {
		rows = append(rows, widthx.WrapTextWithAnsi(paintText(body, true), width)...)
	}
	if len(rows) > toolRowLimit {
		rows = rows[:toolRowLimit]
		rows[toolRowLimit-1] = "[display rows clipped]"
	}
	for i, row := range rows {
		rows[i] = widthx.StripTerminalSequences(widthx.TruncateToWidth(row, width, "…", false))
	}
	return rows
}
