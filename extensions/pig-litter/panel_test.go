package piglitter

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/tui/widthx"
)

func panelKey(t *testing.T, panel *childPanel, key string) {
	t.Helper()
	result, err := panel.HandleInput(key)
	if err != nil || result.Done {
		t.Fatalf("panel unexpectedly closed for %q: %#v %v", key, result, err)
	}
}

func panelAdmission(t *testing.T, tree *Tree, name string) *run {
	t.Helper()
	_, selected, err := tree.admit(admission{kind: "scout", name: name, model: "fixture/child", cwd: "/fixture", task: "task", agent: tree.config.Agents["scout"]})
	if err != nil {
		t.Fatal(err)
	}
	return selected
}

func TestPanelRetainedViewportResizeAndSelectionBeyondSixteen(t *testing.T) {
	tree := testTree(t, defaultConfig())
	tree.execute = func(r *record, _ *run) Outcome { return Outcome{State: Completed, Text: "report for " + r.name} }
	var last *run
	for i := range 40 {
		last = panelAdmission(t, tree, fmt.Sprintf("child-%02d", i))
		if err := tree.activate(last); err != nil {
			t.Fatal(err)
		}
		if _, err := tree.wait(context.Background(), last, 1000); err != nil {
			t.Fatal(err)
		}
		tree.acknowledge(last)
	}
	height := 32
	panel := newChildPanel(testCallOwner(tree), nil, func() int { return height })
	defer panel.Dispose()
	panelKey(t, panel, "\x1b[F")
	for _, dimensions := range [][2]int{{100, 32}, {80, 24}, {32, 24}, {2, 24}, {1, 24}, {32, 4}, {1, 1}, {100, 32}} {
		width := dimensions[0]
		height = dimensions[1]
		rows := panel.Render(width)
		if len(rows) > max(1, height-8) {
			t.Fatalf("panel exceeded height %d: %q", height, rows)
		}
		for _, row := range rows {
			if !utf8.ValidString(row) || widthx.VisibleWidth(row) > width {
				t.Fatalf("panel exceeded width %d: %q", width, row)
			}
		}
		if panel.selected != last {
			t.Fatal("resize changed the selected UUID or execution")
		}
		if width >= 32 && height >= 24 && !strings.Contains(strings.Join(rows, "\n"), "child-39") {
			t.Fatalf("retained selection beyond row 16 disappeared: %q", rows)
		}
		if width == 32 && height == 24 {
			text := strings.Join(rows, "\n")
			for _, hint := range []string{"Enter detail", "v result", "m msg", "p pause subtree", "r resume", "Esc"} {
				if !strings.Contains(text, hint) {
					t.Fatalf("32-column panel hid %q: %s", hint, text)
				}
			}
		}
	}
	panelKey(t, panel, "[")
	panel.Render(100)
	if panel.selected == last || panel.selected.owner.name != "child-20" {
		t.Fatalf("PageUp did not navigate the retained viewport: %s", panel.selected.owner.name)
	}
	panelKey(t, panel, "\x1b[H")
	if panel.selected.owner.name != "child-00" || len(tree.retained()) != 40 {
		t.Fatal("navigation changed retained children or could not return to the first child")
	}
}

func TestPanelCompletionPreservesReportScrollModeAndExactSelection(t *testing.T) {
	tree := testTree(t, defaultConfig())
	selected := panelAdmission(t, tree, "live child")
	var report strings.Builder
	for i := range 70 {
		fmt.Fprintf(&report, "line %02d before\n", i)
	}
	tree.finish(selected.owner, selected, Outcome{State: Completed, Text: report.String()})
	tree.acknowledge(selected)
	_, next, err := tree.resume(nil, selected, "continue")
	if err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	tree.execute = func(*record, *run) Outcome {
		close(entered)
		<-release
		return Outcome{State: Completed, Text: strings.ReplaceAll(report.String(), "before", "after")}
	}
	if err := tree.activate(next); err != nil {
		t.Fatal(err)
	}
	<-entered
	panel := newChildPanel(testCallOwner(tree), nil, func() int { return 32 })
	defer panel.Dispose()
	panelKey(t, panel, "v")
	panelKey(t, panel, "]")
	before := panel.Render(100)
	scroll := panel.scroll
	if !strings.Contains(strings.Join(before, "\n"), "before") || scroll == 0 {
		t.Fatalf("report did not scroll: %q", before)
	}
	finishing, finishStorage := make(chan struct{}), make(chan struct{})
	tree.persist = func(*record) error {
		close(finishing)
		<-finishStorage
		return nil
	}
	close(release)
	<-finishing
	panel.Render(100)
	if panel.scroll != scroll || panel.mode != panelReport || panel.selected != next {
		t.Fatal("transient final storage reset report navigation")
	}
	close(finishStorage)
	if _, err := tree.wait(context.Background(), next, 1000); err != nil {
		t.Fatal(err)
	}
	after := panel.Render(100)
	if panel.mode != panelReport || panel.scroll != scroll || panel.selected != next || !strings.Contains(strings.Join(after, "\n"), "after") {
		t.Fatalf("completion disrupted the open report: mode=%d scroll=%d rows=%q", panel.mode, panel.scroll, after)
	}
	panelKey(t, panel, "\x1b[H")
	if rows := strings.Join(panel.Render(100), "\n"); !strings.Contains(rows, "State completed") || !strings.Contains(rows, "Outcome completed") {
		t.Fatalf("latest result did not refresh current state: %s", rows)
	}
}

func TestPanelPaintsUnsafeMetadataReportProgressFeedbackAndDraftSafely(t *testing.T) {
	tree := testTree(t, defaultConfig())
	unsafe := "猫👩‍💻e\u0301\x1b]52;c;STEAL\a\x1b[2J\x1b[31mRED\x00\u202e\xff"
	selected := panelAdmission(t, tree, unsafe)
	selected.owner.cwd, selected.owner.model = unsafe, unsafe
	tree.finish(selected.owner, selected, Outcome{State: Failed, Text: unsafe + "\nsecond line", Error: unsafe})
	panel := newChildPanel(testCallOwner(tree), nil, func() int { return 32 })
	defer panel.Dispose()
	panel.feedback = unsafe
	for _, view := range []panelMode{panelList, panelDetail, panelReport, panelMessage, panelResume} {
		panel.mode, panel.draft, panel.cursor = view, unsafe, len(unsafe)
		for _, width := range []int{100, 80, 32, 2, 1, 0} {
			for _, row := range panel.Render(width) {
				if strings.ContainsAny(row, "\x1b\x00\u202e\r\n") || strings.Contains(row, "STEAL") || !utf8.ValidString(row) || widthx.VisibleWidth(row) > width {
					t.Fatalf("unsafe paint at mode %d width %d: %q", view, width, row)
				}
			}
		}
	}
	inspection, _ := tree.inspect(selected.ref.ID)
	if inspection.Outcome.Text != unsafe+"\nsecond line" || inspection.Outcome.Error != unsafe {
		t.Fatal("human paint rewrote the model-visible outcome")
	}
}

func TestPanelEditorUsesGraphemesAndEmptyOrCancelledInputIsInert(t *testing.T) {
	tree := testTree(t, defaultConfig())
	selected := panelAdmission(t, tree, "accepted")
	tree.execute = func(_ *record, selected *run) Outcome {
		<-selected.ctx.Done()
		return Outcome{State: Stopped}
	}
	if err := tree.activate(selected); err != nil {
		t.Fatal(err)
	}
	panel := newChildPanel(testCallOwner(tree), nil, func() int { return 24 })
	defer panel.Dispose()
	panelKey(t, panel, "m")
	panelKey(t, panel, "\r")
	if panel.pending != nil || panel.mode != panelMessage || panel.draft != "" {
		t.Fatal("empty composer submitted an action")
	}
	panelKey(t, panel, "\t")
	if panel.messageMode != "follow_up" {
		t.Fatal("message mode did not switch to follow_up")
	}
	panelKey(t, panel, "猫👩‍💻e\u0301")
	panelKey(t, panel, "\x7f")
	if panel.draft != "猫👩‍💻" {
		t.Fatalf("backspace split a grapheme: %q", panel.draft)
	}
	panelKey(t, panel, "\x1b[D")
	panelKey(t, panel, "\x1b[3~")
	if panel.draft != "猫" || panel.cursor != len("猫") {
		t.Fatalf("delete split a joined emoji: %q cursor=%d", panel.draft, panel.cursor)
	}
	panelKey(t, panel, "\x1b[200~safe\n\x1b]52;c;STEAL\a猫\x1b[201~")
	if panel.draft != "猫safe 猫" {
		t.Fatalf("paste was not sanitized into one line: %q", panel.draft)
	}
	panelKey(t, panel, "\x15")
	panelKey(t, panel, strings.Repeat("👩‍💻", 3000))
	if len(panel.draft) > 16384 || !utf8.ValidString(panel.draft) || strings.Trim(panel.draft, "👩‍💻") != "" {
		t.Fatalf("editor byte limit split UTF-8 or a grapheme: bytes=%d", len(panel.draft))
	}
	panelKey(t, panel, "\x1b")
	panelKey(t, panel, "\r")
	panelKey(t, panel, "v")
	panelKey(t, panel, "\x1b")
	if panel.mode != panelDetail {
		t.Fatal("latest result did not return to its originating detail view")
	}
	panelKey(t, panel, "\x1b")
	panelKey(t, panel, "v")
	panelKey(t, panel, "\x1b")
	if panel.mode != panelList {
		t.Fatal("latest result did not return to its originating list view")
	}
	panelKey(t, panel, "r")
	panelKey(t, panel, "\x1b")
	if panel.mode != panelList || panel.pending != nil || panel.draft != "" || tree.live != 1 || selected.state != Starting {
		t.Fatal("cancelled input changed child state")
	}
}

func TestPanelAcceptsPublicEncodedKeysAndIgnoresReleases(t *testing.T) {
	tree := testTree(t, defaultConfig())
	tree.execute = func(*record, *run) Outcome { return Outcome{State: Completed, Text: "native result"} }
	var children []*run
	for _, name := range []string{"first", "second"} {
		selected := panelAdmission(t, tree, name)
		if err := tree.activate(selected); err != nil {
			t.Fatal(err)
		}
		if _, err := tree.wait(context.Background(), selected, 1000); err != nil {
			t.Fatal(err)
		}
		tree.acknowledge(selected)
		children = append(children, selected)
	}
	o := testCallOwner(tree)
	panel := newChildPanel(o, nil, func() int { return 32 })
	defer panel.Dispose()
	panelKey(t, panel, "\x1b[1;1:1B")
	if panel.selected != children[1] {
		t.Fatal("Kitty Down did not move selection")
	}
	panelKey(t, panel, "\x1b[1;1:2A")
	panelKey(t, panel, "\x1b[1;1:3B")
	if panel.selected != children[0] {
		t.Fatal("repeat/release handling moved selection twice")
	}
	panelKey(t, panel, "\x1b[13u")
	if panel.mode != panelDetail {
		t.Fatal("encoded Enter did not inspect")
	}
	panelKey(t, panel, "\x1b[27u")
	panelKey(t, panel, "\x1b[109u")
	if panel.mode != panelMessage {
		t.Fatal("encoded printable action did not open message")
	}
	panelKey(t, panel, "\x1b[97u")
	panelKey(t, panel, "\x1b[98;1:2u")
	panelKey(t, panel, "\x1b[99;1:3u")
	if panel.draft != "ab" {
		t.Fatalf("encoded printable press/repeat/release = %q", panel.draft)
	}
	panelKey(t, panel, "\x1b[128049u")
	panelKey(t, panel, "\x1b[127u")
	panelKey(t, panel, "\x1b[1;1H")
	panelKey(t, panel, "\x1b[88u")
	panelKey(t, panel, "\x1b[3;1~")
	if panel.draft != "Xb" || panel.cursor != 1 {
		t.Fatalf("encoded home/backspace/delete corrupted editing: draft=%q cursor=%d", panel.draft, panel.cursor)
	}
	panelKey(t, panel, "\x1b[117;5u")
	panelKey(t, panel, "\x1b[200~paste\n猫\x1b[13;1:3u\x1b[201~")
	if panel.draft != "paste 猫" {
		t.Fatalf("encoded Ctrl+U or bracketed paste failed: %q", panel.draft)
	}
	panelKey(t, panel, "\x1b[13;1:3u")
	if panel.pending != nil || panel.feedback != "" {
		t.Fatal("Enter release submitted the draft")
	}
	o.state.owner = nil
	panelKey(t, panel, "\x1b[13;1:1u")
	if text := strings.Join(panel.Render(100), "\n"); !strings.Contains(text, "Action failed | the root Session retired") {
		t.Fatalf("encoded Enter did not submit through the owner boundary: %s", text)
	}
	panelKey(t, panel, "\x1b[27u")
	closed, err := panel.HandleInput("\x1b[27u")
	if err != nil || !closed.Done {
		t.Fatalf("encoded Escape did not close the list: %#v %v", closed, err)
	}
}

func TestPanelEditorNormalizesCursorAfterGraphemeJoins(t *testing.T) {
	panel := &childPanel{}
	panel.edit("\u0301")
	panel.edit("\x1b[H")
	panel.edit("a")
	if panel.draft != "a\u0301" || panel.cursor != 3 || panel.draftLine(32) != "Draft a\u0301|" {
		t.Fatalf("cursor split a joined accent: draft=%q cursor=%d line=%q", panel.draft, panel.cursor, panel.draftLine(32))
	}
	panel.edit("\x7f")
	if panel.draft != "" || panel.cursor != 0 {
		t.Fatalf("Backspace left part of a joined accent: %q", panel.draft)
	}
	panel.edit("👩")
	panel.edit("\x1b[H")
	panel.edit("👨")
	panel.edit("\u200d")
	if panel.draft != "👨‍👩" || panel.cursor != 11 || panel.draftLine(32) != "Draft 👨‍👩|" {
		t.Fatalf("cursor split a joined emoji: draft=%q cursor=%d", panel.draft, panel.cursor)
	}
	panel.edit("\x7f")
	panel.edit("🇦X🇧")
	panel.edit("\x1b[H")
	panel.edit("\x1b[C")
	panel.edit("\x1b[3~")
	if panel.draft != "🇦🇧" || panel.cursor != 8 {
		t.Fatalf("Delete split joined regional indicators: draft=%q cursor=%d", panel.draft, panel.cursor)
	}
	panel.edit("\x7f")
	if panel.draft != "" || panel.cursor != 0 {
		t.Fatalf("Backspace left part of a joined flag: %q", panel.draft)
	}
}

func TestPanelPagingUsesRenderedRowsWithOverlapAfterResize(t *testing.T) {
	tree := testTree(t, defaultConfig())
	selected := panelAdmission(t, tree, "detail pages")
	var path strings.Builder
	for i := range 400 {
		fmt.Fprintf(&path, "/directory-%03d", i)
	}
	selected.owner.cwd = path.String()
	tree.execute = func(*record, *run) Outcome { return Outcome{State: Completed, Text: "result"} }
	if err := tree.activate(selected); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.wait(context.Background(), selected, 1000); err != nil {
		t.Fatal(err)
	}
	panel := newChildPanel(testCallOwner(tree), nil, func() int { return 24 })
	defer panel.Dispose()
	panelKey(t, panel, "\r")
	for _, width := range []int{32, 80, 32} {
		panelKey(t, panel, "\x1b[H")
		before := panel.Render(width)
		last := before[panel.pageRows]
		panelKey(t, panel, "]")
		after := panel.Render(width)
		if after[1] != last {
			t.Fatalf("page at %d columns skipped content: previous last=%q next first=%q", width, last, after[1])
		}
		if width == 32 && panel.scroll != 9 {
			t.Fatalf("32x24 detail did not advance its actual nine-row overlap step: %d", panel.scroll)
		}
	}
}

func TestMaximumDraftRenderStaysBounded(t *testing.T) {
	tree := testTree(t, defaultConfig())
	panel := newChildPanel(testCallOwner(tree), nil, func() int { return 32 })
	defer panel.Dispose()
	panel.mode = panelMessage
	panel.edit(strings.Repeat("x", 16384))
	for _, width := range []int{100, 32} {
		started := time.Now()
		rows := panel.Render(width)
		if time.Since(started) > 2*time.Second || len(rows) > 24 {
			t.Fatalf("maximum draft exceeded render budget at %d columns", width)
		}
		found := false
		for _, row := range rows {
			if !utf8.ValidString(row) || widthx.VisibleWidth(row) > width {
				t.Fatalf("maximum draft exceeded %d columns: %q", width, row)
			}
			found = found || strings.HasPrefix(row, "Draft ") && strings.HasSuffix(row, "|")
		}
		if !found || len(panel.draft) != 16384 {
			t.Fatalf("maximum draft or visible cursor was lost: %q", rows)
		}
	}
}

func BenchmarkPanelMaximumDraft(b *testing.B) {
	for _, width := range []int{100, 32} {
		b.Run(fmt.Sprint(width), func(b *testing.B) {
			tree := newTree(defaultConfig())
			defer tree.close()
			panel := newChildPanel(testCallOwner(tree), nil, func() int { return 32 })
			defer panel.Dispose()
			panel.mode, panel.draft, panel.cursor = panelMessage, strings.Repeat("x", 16384), 16384
			b.ResetTimer()
			for range b.N {
				panel.Render(width)
			}
		})
	}
}

func TestPanelMarksProvisionalRowsAndWaitsForActualActivation(t *testing.T) {
	tree := testTree(t, defaultConfig())
	selected := panelAdmission(t, tree, "model-owned child")
	panel := newChildPanel(testCallOwner(tree), nil, func() int { return 32 })
	defer panel.Dispose()
	if text := strings.Join(panel.Render(100), "\n"); !strings.Contains(text, "> Admission pending model-owned child") {
		t.Fatalf("provisional row looked actionable: %s", text)
	}
	for _, key := range []string{"m", "p", "r"} {
		panelKey(t, panel, key)
		if panel.mode != panelList || panel.pending != nil || !strings.Contains(panel.feedback, "Admission pending") {
			t.Fatalf("provisional %s control took ownership: mode=%d feedback=%s", key, panel.mode, panel.feedback)
		}
	}
	tree.execute = func(*record, *run) Outcome {
		return Outcome{State: Completed, Text: "genuine model admission finished"}
	}
	if err := tree.activate(selected); err != nil {
		t.Fatal(err)
	}
	if _, err := tree.wait(context.Background(), selected, 1000); err != nil {
		t.Fatal(err)
	}
	panelKey(t, panel, "r")
	if panel.mode != panelResume || !strings.Contains(strings.Join(panel.Render(100), "\n"), "Resume instructions") {
		t.Fatal("resolved model admission never became actionable")
	}
}

func TestPanelTickerInvalidatesWhileOpenAndDisposeJoins(t *testing.T) {
	tree := testTree(t, defaultConfig())
	panelAdmission(t, tree, "retained")
	panel := newChildPanel(testCallOwner(tree), nil, func() int { return 24 })
	var ticks atomic.Int32
	invalidation := make(chan struct{}, 4)
	panel.SetInvalidate(func() {
		ticks.Add(1)
		invalidation <- struct{}{}
	})
	select {
	case <-invalidation:
	case <-time.After(2 * time.Second):
		t.Fatal("open panel did not request live rendering")
	}
	panel.Dispose()
	panel.Dispose()
	before := ticks.Load()
	select {
	case <-panel.joined:
	default:
		t.Fatal("Dispose returned before the ticker joined")
	}
	panel.SetInvalidate(func() { ticks.Add(100) })
	if ticks.Load() != before || !panel.closed || panel.ctx.Err() == nil {
		t.Fatal("disposed panel retained invalidation or input ownership")
	}
}

func TestPrivateProgressCountsParallelNativeToolsAndIgnoresStaleRun(t *testing.T) {
	tree := testTree(t, defaultConfig())
	selected := panelAdmission(t, tree, "parallel")
	tree.execute = func(_ *record, selected *run) Outcome {
		<-selected.ctx.Done()
		return Outcome{State: Stopped}
	}
	if err := tree.activate(selected); err != nil {
		t.Fatal(err)
	}
	tree.observeProgress(selected, agent.ToolExecutionStartEvent{ToolCallID: "one", ToolName: "read"})
	tree.observeProgress(selected, agent.ToolExecutionStartEvent{ToolCallID: "two", ToolName: "ls\x1b]52;c;STEAL\a\u202e"})
	tree.observeProgress(selected, agent.ToolExecutionEndEvent{ToolCallID: "one"})
	panel := newChildPanel(testCallOwner(tree), nil, func() int { return 40 })
	defer panel.Dispose()
	panelKey(t, panel, "\r")
	text := strings.Join(panel.Render(100), "\n")
	if !strings.Contains(text, "Tool starts 2") || !strings.Contains(text, "Active tools ls") || strings.Contains(text, "Active tools read") || strings.Contains(text, "STEAL") || strings.ContainsAny(text, "\x1b\u202e") {
		t.Fatalf("parallel tool end erased another active call: %s", text)
	}
	for i := range 100 {
		tree.observeProgress(selected, agent.ToolExecutionStartEvent{ToolCallID: fmt.Sprint(i), ToolName: "tool"})
	}
	text = strings.Join(panel.Render(100), "\n")
	if !strings.Contains(text, "Tool starts 102") || !strings.Contains(text, "omitted]") {
		t.Fatalf("bounded observation did not describe omitted parallel activity: %s", text)
	}
	tree.observeProgress(selected, agent.TurnEndEvent{})
	if _, _, err := tree.stop(nil, selected); err != nil {
		t.Fatal(err)
	}
	tree.observeProgress(selected, agent.ToolExecutionStartEvent{ToolCallID: "late", ToolName: "late tool"})
	text = strings.Join(panel.Render(100), "\n")
	if !strings.Contains(text, "Tool starts 102") || !strings.Contains(text, "Active tools none") || !strings.Contains(text, "State paused") {
		t.Fatalf("settled run accepted late native progress: %s", text)
	}
	encoded, _ := resultContent("litter_inspect", map[string]any{"child": tree.retained()[0], "outcome": selected.outcome}, nil)
	if strings.Contains(encoded, "toolStarts") || strings.Contains(encoded, "activated") || strings.Contains(encoded, "activeTools") {
		t.Fatalf("private progress escaped into machine results: %s", encoded)
	}
}

func BenchmarkPanelRetainedViewport(b *testing.B) {
	for _, count := range []int{100, 10000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			tree := newTree(defaultConfig())
			defer tree.close()
			for i := range count {
				id := ChildID(fmt.Sprintf("retained-%d", i))
				r := &record{id: id, name: string(id), parent: tree.root, run: &run{ref: Ref{id}, state: Completed, admission: admissionReady}}
				tree.order = append(tree.order, id)
				tree.records[id] = r
			}
			panel := newChildPanel(testCallOwner(tree), nil, func() int { return 32 })
			defer panel.Dispose()
			panel.selected = tree.records[tree.order[len(tree.order)-1]].run
			selected := panel.selected
			b.ResetTimer()
			for range b.N {
				rows := panel.Render(100)
				if len(rows) != 24 || panel.selected != selected || !strings.Contains(strings.Join(rows, "\n"), fmt.Sprint(count-1)) {
					b.Fatal("retained viewport lost its bounds or selected UUID")
				}
			}
		})
	}
}
