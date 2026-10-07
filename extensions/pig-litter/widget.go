package piglitter

import (
	"fmt"

	"github.com/MichaelKinsy/PiG/tui/widthx"
)

func widgetLines(children []Snapshot, terminalWidth int) []string {
	width := min(29, max(1, terminalWidth))
	live := 0
	for _, child := range children {
		if !child.State.terminal() {
			live++
		}
	}

	lines := []string{widthx.TruncateToWidth(fmt.Sprintf("Litter %d live %d kept", live, len(children)-live), width, "", false)}
	for _, child := range children[max(0, len(children)-2):] {
		row := child.DisplayLabel + " " + string(child.State)
		if child.Name == string(child.ID) {
			row = fmt.Sprintf("%s g%d %s %s", child.DisplayLabel, child.Generation, child.State, child.Type)
		}
		lines = append(lines, widthx.TruncateToWidth(row, width, "", false))
	}
	return lines
}
