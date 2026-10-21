package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode"

	"github.com/MichaelKinsy/PiG/tui"
)

func TestPanelNavigationParserUsesViewAndActualPaintedRows(t *testing.T) {
	const frame = " Litter 2 active\nPig Litter | list (3/3)\n  running held-b #bbbbbbbb\n  running held-a #aaaaaaaa\n> completed quick-newer #cccccccc\n\nWorking\ncontrols\n"
	want := []string{"running held-b #bbbbbbbb", "running held-a #aaaaaaaa", "completed quick-newer #cccccccc"}
	if got := panelListBodies(frame); !reflect.DeepEqual(got, want) {
		t.Fatalf("painted rows=%q", got)
	}
	if got := selectedPanelRow(frame); got != "> completed quick-newer #cccccccc" {
		t.Fatalf("selected row=%q", got)
	}
	if panelView(frame) != "Pig Litter | list" || !hasPanelView("list")(frame) || hasPanelView("detail")(frame) {
		t.Fatal("incorrect current view")
	}
}

func TestPanelViewIgnoresBackgroundStatusAndQuotedBodyHeadings(t *testing.T) {
	const before = "Pig Litter | latest result\nPig Litter | detail\nWorking frame one\n"
	const after = "Pig Litter | latest result\nPig Litter | detail\nWorking frame two\n"
	if panelView(before) != panelView(after) || hasPanelView("detail")(before) || !hasPanelView("latest result")(before) {
		t.Fatal("background repaint or report body changed the view oracle")
	}
}

func TestPanelGeometryRejectsActualClippedWideLegendAt32(t *testing.T) {
	const clipped = " Reloaded keybindings, extension\n\n Litter 0 active 0 errors 3 paus\n held-b paused\n quick-newer paused\n /pig-litter | +1 more\nPig Litter | list (1/3)\n> paused held-a #bfa5e14d\n  paused held-b #88778a3b\n  paused quick-newer #4291835f\n\n\n\n\n\n\n\n\n\n\nUp/Down move  Enter inspect  v r\npage  Home/End first/last  Esc b\n/tmp/litter-tui-proof-98057425/w\n↑8 ↓8 3.9%/32k (auto)"
	if !frameHasGeometry(clipped, 32, 24) || !hasPanelView("list")(clipped) || !strings.Contains(clipped, "Esc") {
		t.Fatal("literal failed capture must satisfy the previous weaker predicate")
	}
	if panelListHintsReady(clipped, 32, 24) {
		t.Fatal("accepted actual transitional frame with hidden message/pause/resume controls")
	}
	valid := strings.Split(clipped, "\n")
	copy(valid[18:22], []string{"↑↓ move Enter detail v result", "m msg p pause subtree r resume", "[ ] page Home/End first/last", "Esc back/close"})
	if !panelListHintsReady(strings.Join(valid, "\n"), 32, 24) {
		t.Fatal("rejected complete 32-column panel controls")
	}
	for _, placement := range []string{"body", "footer"} {
		t.Run(placement, func(t *testing.T) {
			spoof := strings.Split(clipped, "\n")
			if placement == "body" {
				copy(spoof[10:14], valid[18:22])
			} else {
				spoof[22] = "m msg p pause subtree r resume"
				spoof[23] = "↑↓ move Enter detail v result Esc"
			}
			if panelListHintsReady(strings.Join(spoof, "\n"), 32, 24) {
				t.Fatal("accepted control text outside the panel legend region")
			}
		})
	}
}

func TestPanelGeometryRequiresCompleteWideControls(t *testing.T) {
	for _, size := range []struct {
		width, height int
		first, last   string
	}{
		{80, 24, "Up/Down move  Enter inspect  v result  m message  p pause subtree  r resume  [ ]", "page  Home/End first/last  Esc back/close"},
		{100, 32, "Up/Down move  Enter inspect  v result  m message  p pause subtree  r resume  [ ] page  Home/End", "first/last  Esc back/close"},
	} {
		frame := make([]string, size.height)
		frame[6] = "Pig Litter | list (1/3)"
		frame[7] = "> paused held-a #bfa5e14d"
		frame[size.height-4], frame[size.height-3] = size.first, size.last
		frame[size.height-2] = "/private/workspace"
		frame[size.height-1] = strings.Repeat(" ", size.width-len("parent")) + "parent"
		if !panelListHintsReady(strings.Join(frame, "\n"), size.width, size.height) {
			t.Fatalf("rejected complete %d-column controls", size.width)
		}
		frame[size.height-4] = strings.ReplaceAll(size.first, "m message  p pause subtree  r resume", "")
		if panelListHintsReady(strings.Join(frame, "\n"), size.width, size.height) {
			t.Fatalf("accepted %d-column panel missing action controls", size.width)
		}
	}
}

func TestHostileConfigErrorAcceptsOnlyFreshDedicatedWrappedRegion(t *testing.T) {
	const wrapped = " Extension \"command:pig-litter\" error:\n /tmp/v2D.6xlKUd/litter-tui-proof-2083484939/workspace/litter.yaml: litter.yaml has unknown setting\n HOSTILE_CONFIG_BEGINHOSTILE_CONFIG_END\n\n/private/workspace\nparent"
	const unwrapped = " Extension \"command:pig-litter\" error:\n /private/litter.yaml: litter.yaml has unknown setting HOSTILE_CONFIG_BEGINHOSTILE_CONFIG_END\n\n/private/workspace\nparent"
	for _, frame := range []string{wrapped, unwrapped} {
		if !hostileConfigErrorReady(frame, "") {
			t.Fatalf("rejected sanitized native command error %q", frame)
		}
		if hostileConfigErrorReady(frame, latestLitterCommandError(frame)) {
			t.Fatal("accepted an unchanged command error from before submission")
		}
	}
	if got := latestLitterCommandError(wrapped); got != `Extension "command:pig-litter" error: /tmp/v2D.6xlKUd/litter-tui-proof-2083484939/workspace/litter.yaml: litter.yaml has unknown setting HOSTILE_CONFIG_BEGINHOSTILE_CONFIG_END` {
		t.Fatalf("wrapped native error region=%q", got)
	}
	for name, frame := range map[string]string{
		"unrelated command":     strings.ReplaceAll(unwrapped, "command:pig-litter", "command:other"),
		"older matching region": unwrapped + "\nExtension \"command:pig-litter\" error:\nfile unavailable\n",
		"newer unrelated error": unwrapped + "\nExtension \"command:other\" error:\nfile unavailable\n",
		"body only":             "Assistant report\n/private/litter.yaml: litter.yaml has unknown setting HOSTILE_CONFIG_BEGINHOSTILE_CONFIG_END\n",
		"quoted heading":        strings.ReplaceAll(unwrapped, " Extension", "> Extension"),
		"panel report body":     "Pig Litter | latest result\n" + unwrapped,
		"missing marker end":    strings.ReplaceAll(unwrapped, "HOSTILE_CONFIG_END", ""),
		"different setting":     strings.ReplaceAll(unwrapped, "unknown setting", "invalid value"),
		"unsafe OSC text":       strings.ReplaceAll(unwrapped, "HOSTILE_CONFIG_END", "CONFIG_OSC_SHOULD_NOT_PAINTHOSTILE_CONFIG_END"),
		"unsafe bidi":           unwrapped + "\u202e",
		"unsafe escape":         unwrapped + "\x1b[2J",
	} {
		t.Run(name, func(t *testing.T) {
			if hostileConfigErrorReady(frame, "") {
				t.Fatalf("accepted unrelated or unsafe error frame %q", frame)
			}
		})
	}
}

func TestHostileConfigErrorRecognizesActualInlineNativeHeading(t *testing.T) {
	const inline = " Reloaded keybindings, extensions, skills, prompts, themes, and context files\n Extension \"command:pig-litter\" error: /tmp/litter-tui-proof-1034039390/workspace/litter.yaml:\n litter.yaml has unknown setting HOSTILE_CONFIG_BEGINHOSTILE_CONFIG_END\n\n/tmp/litter-tui-proof-1034039390/workspace\n↑1 ↓1 0.0%/32k (auto) parent"
	if !hostileConfigErrorReady(inline, "") {
		t.Fatal("rejected actual native error with path beside the heading")
	}
	if got := latestLitterCommandError(inline); got != `Extension "command:pig-litter" error: /tmp/litter-tui-proof-1034039390/workspace/litter.yaml: litter.yaml has unknown setting HOSTILE_CONFIG_BEGINHOSTILE_CONFIG_END` {
		t.Fatalf("inline native region=%q", got)
	}
	if hostileConfigErrorReady(inline, latestLitterCommandError(inline)) {
		t.Fatal("accepted unchanged inline error from before submission")
	}
	for _, latest := range []string{
		"Extension \"command:other\" error: file unavailable\n",
		"Extension \"command:pig-litter\" error: file unavailable\n",
	} {
		if hostileConfigErrorReady(inline+"\n"+latest, "") {
			t.Fatalf("used old matching error before newer inline region %q", latest)
		}
	}
}

func TestKittyTextUsesSupportedKeyEvents(t *testing.T) {
	if got := kittyText("KTT_a界🦊É"); got != "\x1b[107:75;2:1u\x1b[116:84;2:1u\x1b[116:84;2:2u\x1b[45:95;2:1u\x1b[97;1:1u\x1b[30028;1:1u\x1b[129418;1:1u\x1b[233:201;2:1u" {
		t.Fatalf("encoded key events=%q", got)
	}
}

func TestKittyTextThroughReleasedTerminalDecoder(t *testing.T) {
	for _, text := range []string{"KITTY_CANCELLED_MARKER", "KITTY_INPUT_MARKER", "A_界🦊É"} {
		t.Run(text, func(t *testing.T) {
			buffer := tui.NewStdinBuffer(tui.StdinBufferOptions{})
			var decoded strings.Builder
			for _, event := range buffer.ProcessTerminalBytes([]byte(kittyText(text))) {
				value, ok := tui.DecodePrintableKey(event)
				if !ok || tui.IsKeyRelease(event) {
					t.Fatalf("undecodable key event=%q", event)
				}
				decoded.WriteString(value)
			}
			if got := decoded.String(); got != text {
				t.Fatalf("decoded text=%q, want %q", got, text)
			}
		})
	}
}

func TestReleasedTerminalPressRepeatAndRelease(t *testing.T) {
	buffer := tui.NewStdinBuffer(tui.StdinBufferOptions{})
	events := buffer.ProcessTerminalBytes([]byte("\x1b[107;1:1u\x1b[107;1:2u\x1b[107;1:3u"))
	if len(events) != 3 {
		t.Fatalf("terminal events=%q", events)
	}
	var delivered strings.Builder
	for _, event := range events {
		if !tui.ShouldDeliverKey(nil, event) {
			continue
		}
		value, ok := tui.DecodePrintableKey(event)
		if !ok {
			t.Fatalf("undecodable key event=%q", event)
		}
		delivered.WriteString(value)
	}
	if got := delivered.String(); got != "kk" || !tui.IsKeyRelease(events[2]) {
		t.Fatalf("delivered=%q, events=%q", got, events)
	}
}

func kittyText(text string) string {
	var encoded strings.Builder
	var previous rune
	for _, r := range text {
		event := 1
		if r == previous {
			event = 2
		}
		switch {
		case r == '_':
			fmt.Fprintf(&encoded, "\x1b[45:95;2:%du", event)
		case unicode.IsUpper(r):
			fmt.Fprintf(&encoded, "\x1b[%d:%d;2:%du", unicode.ToLower(r), r, event)
		default:
			fmt.Fprintf(&encoded, "\x1b[%d;1:%du", r, event)
		}
		previous = r
	}
	return encoded.String()
}
