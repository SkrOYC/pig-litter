package piglitter

import (
	"reflect"
	"testing"

	"github.com/MichaelKinsy/PiG/tui/widthx"
)

func visibleWidgetLines(lines []string) []string {
	visible := make([]string, len(lines))
	for i, line := range lines {
		visible[i] = widthx.StripTerminalSequences(line)
	}
	return visible
}

func TestWidgetLinesUsesSequenceForUnnamedChildren(t *testing.T) {
	firstID := ChildID("0123456789abcdef0123456789abcdef:7")
	secondID := ChildID("0123456789abcdef0123456789abcdef:8")
	children := []Snapshot{
		{Ref: Ref{ID: firstID, Generation: 1}, Name: string(firstID), DisplayLabel: "#7", Type: "worker", State: Running},
		{Ref: Ref{ID: secondID, Generation: 2}, Name: string(secondID), DisplayLabel: "#8", Type: "scout", State: Failed},
	}

	got := widgetLines(children, 80)
	want := []string{"Litter 1 live 1 kept", "#7 g1 running worker", "#8 g2 failed scout"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("widgetLines() = %#v, want %#v", got, want)
	}
	if children[0].Name != string(firstID) || children[1].Name != string(secondID) {
		t.Fatalf("widgetLines() changed stored names: %#v", children)
	}
}

func TestWidgetLinesKeepsLastTwoNamedChildRows(t *testing.T) {
	children := []Snapshot{
		{Ref: Ref{ID: "first:1", Generation: 1}, Name: "setup", DisplayLabel: "setup", State: Completed},
		{Ref: Ref{ID: "second:1", Generation: 1}, Name: "builder", DisplayLabel: "builder", State: Running},
		{Ref: Ref{ID: "third:1", Generation: 1}, Name: "reviewer", DisplayLabel: "reviewer", State: Failed},
		{Ref: Ref{ID: "fourth:1", Generation: 1}, Name: "finisher", DisplayLabel: "finisher", State: Stopping},
	}

	got := widgetLines(children, 80)
	want := []string{"Litter 2 live 2 kept", "reviewer failed", "finisher stopping"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("widgetLines() = %#v, want %#v", got, want)
	}
}

func TestWidgetLinesBoundsNarrowUnicodeRows(t *testing.T) {
	children := []Snapshot{
		{Ref: Ref{ID: "child:1", Generation: 1}, Name: "猫犬鳥走亀", DisplayLabel: "猫犬鳥走亀", State: Running},
	}

	got := visibleWidgetLines(widgetLines(children, 6))
	want := []string{"Litter", "猫犬鳥"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("widgetLines() = %#v, want %#v", got, want)
	}

	got = visibleWidgetLines(widgetLines(children, 0))
	want = []string{"L", ""}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("widgetLines() at minimum width = %#v, want %#v", got, want)
	}
}

func TestWidgetLinesCapsWideTerminalRows(t *testing.T) {
	children := []Snapshot{
		{Ref: Ref{ID: "child:1", Generation: 1}, Name: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN", DisplayLabel: "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMN", State: Running},
	}

	got := visibleWidgetLines(widgetLines(children, 80))
	want := []string{"Litter 1 live 0 kept", "abcdefghijklmnopqrstuvwxyzABC"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("widgetLines() = %#v, want %#v", got, want)
	}
}
