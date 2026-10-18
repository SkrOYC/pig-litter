package piglitter

import (
	"fmt"

	"github.com/MichaelKinsy/PiG/tui/widthx"
)

type widgetCounts struct {
	active, paused, settled, errors int
}

func widgetLines(children []Snapshot, terminalWidth int) []string {
	width := max(0, terminalWidth-2)
	if width == 0 {
		return []string{}
	}
	var counts widgetCounts
	var active, retained [2]Snapshot
	activeRows, retainedRows := 0, 0
	for _, child := range children {
		switch child.State {
		case Paused:
			counts.paused++
		case Partial, Failed, Unavailable:
			counts.errors++
		case Completed, Stopped:
			counts.settled++
		default:
			counts.active++
		}
		if !child.State.terminal() {
			if activeRows < len(active) {
				active[activeRows] = child
				activeRows++
			}
		} else {
			retained[0], retained[1] = retained[1], child
			retainedRows = min(retainedRows+1, len(retained))
		}
	}
	errorLabel := "errors"
	if counts.errors == 1 {
		errorLabel = "error"
	}
	groups := []struct {
		count       int
		full, short string
	}{{counts.active, "active", "live"}, {counts.errors, errorLabel, "err"}, {counts.paused, "paused", "paused"}, {counts.settled, "settled", "done"}}
	summary := "Litter"
	for _, group := range groups {
		summary += fmt.Sprintf(" %d %s", group.count, group.full)
	}
	if len(children) == 0 {
		summary = "Litter 0 children"
	} else if widthx.VisibleWidth(summary) > width {
		summary = "Litter"
		for _, group := range groups {
			if group.count == 0 {
				continue
			}
			next := fmt.Sprintf(" %d %s", group.count, group.short)
			if widthx.VisibleWidth(summary+next) <= width {
				summary += next
			}
		}
	}
	lines := []string{summary}
	selected := active[:activeRows]
	selected = append(selected, retained[2-min(retainedRows, 2-activeRows):]...)
	for _, child := range selected {
		label := child.DisplayLabel
		if label == "" {
			label = child.Name
		}
		if label == "" {
			label = toolTarget(string(child.ID))
		}
		if child.Name == string(child.ID) && child.Type != "" {
			label += " " + child.Type
		}
		label = paintText(label, false)
		status := paintText(string(child.State), false)
		nameWidth := width - widthx.VisibleWidth(status) - 1
		row := status
		if nameWidth > 0 {
			row = widthx.StripTerminalSequences(widthx.TruncateToWidth(label, nameWidth, "…", false)) + " " + status
		}
		lines = append(lines, row)
	}
	hint := "/pig-litter"
	if hidden := len(children) - len(selected); hidden != 0 {
		hint += fmt.Sprintf(" | +%d more", hidden)
	}
	lines = append(lines, hint)
	for i, line := range lines {
		lines[i] = widthx.StripTerminalSequences(widthx.TruncateToWidth(line, width, "…", false))
	}
	return lines
}
