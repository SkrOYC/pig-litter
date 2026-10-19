package piglitter

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/MichaelKinsy/PiG/tui"
	"github.com/MichaelKinsy/PiG/tui/widthx"
)

type panelMode uint8

const (
	panelList panelMode = iota
	panelDetail
	panelReport
	panelMessage
	panelResume
)

type panelChild struct {
	child      Snapshot
	execution  *run
	actionable bool
}

type childPanel struct {
	mu                 sync.Mutex
	owner              *owner
	ctx                context.Context
	cancel             context.CancelFunc
	height             func() int
	mode, previous     panelMode
	previousScroll     int
	selected           *run
	listTop, scroll    int
	renderedScroll     int
	pageRows           int
	draft, messageMode string
	cursor             int
	feedback           string
	pending            <-chan humanResult
	pendingCancel      context.CancelFunc
	pendingSelection   *run
	invalidate         func()
	stop, joined       chan struct{}
	dispose            sync.Once
	closed             bool
	reportRun          *run
	reportOutcome      Outcome
	reportPresent      bool
	reportState        State
	reportWidth        int
	reportLines        []string
}

func (t *Tree) panelChildren() []panelChild {
	t.mu.Lock()
	defer t.mu.Unlock()
	children := make([]panelChild, 0)
	for _, id := range t.order {
		r := t.records[id]
		if r.parent == t.root {
			children = append(children, panelChild{child: t.snapshotLocked(r), execution: r.run, actionable: r.run.admission == admissionReady})
		}
	}
	return children
}

func (s *extensionState) openPanel(ctx sdk.Context, args string) (err error) {
	defer func() {
		err = paintError(err)
	}()
	if strings.TrimSpace(args) == "help" {
		ctx.Notify("Use litter_list to discover named agents and children. Use litter_spawn to start a background child, then inspect, message, stop, or wait by ID.", "info")
		return nil
	}
	if strings.TrimSpace(args) != "" {
		ctx.Notify("Use /pig-litter to manage retained children, or /pig-litter help for tool instructions.", "info")
		return nil
	}
	if !ctx.HasUI() || ctx.Mode() != "tui" {
		ctx.Notify("Open /pig-litter in the terminal UI. Use litter_list and litter_inspect for model controls.", "info")
		return nil
	}
	o, err := s.acquire(ctx, false)
	if err != nil {
		return err
	}
	panel := newChildPanel(o, ctx.Done(), ctx.Height)
	defer panel.Dispose()
	_, err = ctx.Custom(panel, sdk.RemoteOverlayOptions{})
	return err
}

func newChildPanel(o *owner, requestDone <-chan struct{}, height func() int) *childPanel {
	ctx, cancel := context.WithCancel(context.Background())
	p := &childPanel{owner: o, ctx: ctx, cancel: cancel, height: height, messageMode: "steer", stop: make(chan struct{}), joined: make(chan struct{})}
	if children := o.tree.panelChildren(); len(children) != 0 {
		p.selected = children[0].execution
	}
	go func() {
		defer close(p.joined)
		ticker := time.NewTicker(250 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				p.mu.Lock()
				invalidate := p.invalidate
				p.mu.Unlock()
				if invalidate != nil {
					invalidate()
				}
			case <-requestDone:
				cancel()
				return
			case <-o.tree.ctx.Done():
				cancel()
				return
			case <-p.stop:
				return
			}
		}
	}()
	return p
}

func (p *childPanel) SetInvalidate(fn func()) {
	p.mu.Lock()
	if !p.closed {
		p.invalidate = fn
	}
	p.mu.Unlock()
}

func (p *childPanel) Dispose() {
	p.dispose.Do(func() {
		p.mu.Lock()
		p.closed = true
		p.invalidate = nil
		p.cancel()
		p.mu.Unlock()
		close(p.stop)
		<-p.joined
	})
}

func (p *childPanel) receive() {
	if p.pending == nil {
		return
	}
	select {
	case result := <-p.pending:
		p.pendingCancel()
		p.pendingCancel = nil
		p.pending = nil
		if p.selected == p.pendingSelection && result.selected != nil {
			p.selected = result.selected
		}
		if result.err != nil {
			p.feedback = "Action failed | " + result.err.Error()
		} else {
			p.feedback = result.feedback
			if p.mode == panelMessage || p.mode == panelResume {
				p.mode = p.previous
				p.draft, p.cursor = "", 0
			}
		}
	default:
	}
}

func (p *childPanel) Render(width int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.receive()
	p.pageRows = 0
	if width <= 0 {
		return []string{}
	}
	height := p.height()
	if height <= 0 {
		height = 24
	}
	budget := max(1, height-8)
	legends := []string{"Up/Down move  Enter inspect  v result", "m message  p pause subtree  r resume", "[ ] page  Home/End first/last", "Esc back/close"}
	if width < 40 {
		legends = []string{"↑↓ move Enter detail v result", "m msg p pause subtree r resume", "[ ] page Home/End first/last", "Esc back/close"}
	}
	if width < 30 {
		legends = []string{"↑↓ move Enter detail", "v result m message", "p pause subtree", "r resume [ ] page", "Esc back/close"}
	}
	if p.mode == panelMessage || p.mode == panelResume {
		legends = []string{"Enter submit  Esc cancel", "Left/Right edit  Ctrl+U clear"}
		if p.mode == panelMessage {
			legends = append(legends, "Tab steer/follow_up")
		}
	} else if p.mode == panelReport {
		legends = []string{"Up/Down scroll  [ ] page", "Home/End  Esc back"}
	} else if p.mode == panelDetail {
		legends[0] = "Up/Down scroll  v result"
	}
	if width >= 80 {
		legends = widthx.WrapTextWithAnsi(paintText(strings.Join(legends, "  "), false), width)
	}
	if budget <= 2 {
		return panelPaint(legends[len(legends)-min(budget, len(legends)):], width)
	}
	legendRows := min(len(legends), budget-2)
	legends = legends[len(legends)-legendRows:]
	contentBudget := max(0, budget-legendRows-2)
	p.pageRows = contentBudget
	mode := []string{"list", "detail", "latest result", "message", "resume"}[p.mode]
	title := "Pig Litter | " + mode
	children := p.owner.tree.panelChildren()
	index := slices.IndexFunc(children, func(child panelChild) bool { return p.selected != nil && child.child.ID == p.selected.ref.ID })
	if p.selected == nil && len(children) != 0 {
		p.selected, index = children[0].execution, 0
	}
	var content []string
	if p.mode == panelList {
		title += fmt.Sprintf(" (%d/%d)", index+1, len(children))
		if len(children) == 0 {
			content = []string{"No retained direct children"}
		} else {
			p.listTop = min(p.listTop, max(0, len(children)-contentBudget))
			if index < p.listTop {
				p.listTop = max(0, index)
			}
			if index >= p.listTop+contentBudget {
				p.listTop = max(0, index-contentBudget+1)
			}
			for i := p.listTop; i < min(len(children), p.listTop+contentBudget); i++ {
				prefix := "  "
				if i == index {
					prefix = "> "
				}
				status := string(children[i].child.State)
				if !children[i].actionable {
					status = "Admission pending"
				}
				content = append(content, prefix+status+" "+toolChildLabel(children[i].child))
			}
		}
	} else if p.mode == panelMessage || p.mode == panelResume {
		label := "Resume instructions"
		if p.mode == panelMessage {
			label = "Message mode " + p.messageMode
		}
		content = []string{label}
		if index >= 0 {
			content = append(content, "Target "+toolChildLabel(children[index].child))
		}
		content = append(content, p.draftLine(width))
	} else {
		content = p.detailLines(width, p.mode == panelReport)
		p.renderedScroll = min(p.scroll, max(0, len(content)-contentBudget))
		content = content[p.renderedScroll:min(len(content), p.renderedScroll+contentBudget)]
	}
	content = content[:min(len(content), contentBudget)]
	for len(content) < contentBudget {
		content = append(content, "")
	}
	feedback := p.feedback
	if p.pending != nil {
		feedback = "Working"
	}
	rows := append([]string{title}, content...)
	rows = append(rows, feedback)
	rows = append(rows, legends...)
	return panelPaint(rows, width)
}

func panelPaint(rows []string, width int) []string {
	for i, row := range rows {
		rows[i] = widthx.StripTerminalSequences(widthx.TruncateToWidth(paintText(row, false), width, "…", false))
	}
	return rows
}

func (p *childPanel) detailLines(width int, report bool) []string {
	t := p.owner.tree
	t.mu.Lock()
	r, err := t.currentLocked(p.selected)
	if err != nil || r.parent != t.root {
		t.mu.Unlock()
		return []string{"Selection changed. Return to the list and select again."}
	}
	child := t.snapshotLocked(r)
	var outcome Outcome
	present := r.latest != nil && !r.run.finishing
	if present {
		outcome = *r.latest
	}
	admission := "ready"
	if r.run.admission != admissionReady {
		admission = "pending"
	}
	progress := r.run.progress
	tools := make([]string, 0, len(progress.activeTools))
	for _, name := range progress.activeTools {
		tools = append(tools, name)
	}
	t.mu.Unlock()
	if report {
		if p.reportRun != p.selected || p.reportWidth != width || p.reportPresent != present || p.reportOutcome != outcome || p.reportState != child.State {
			p.reportRun, p.reportWidth, p.reportPresent, p.reportOutcome = p.selected, width, present, outcome
			p.reportState = child.State
			metadata := []string{"State " + string(child.State), "Name " + toolChildLabel(child), "No result yet"}
			if present {
				metadata = []string{"State " + string(child.State), "Name " + toolChildLabel(child), "Outcome " + string(outcome.State), "UUID " + string(child.ID)}
				if outcome.Error != "" {
					metadata = append(metadata, "Error "+outcome.Error)
				}
				metadata = append(metadata, "Report")
			}
			p.reportLines = toolPaintRows(metadata, outcome.Text, width)
		}
		return p.reportLines
	}
	metadata := append(toolChildDetails(child), "State "+string(child.State), "Admission "+admission)
	if progress.activated.IsZero() {
		metadata = append(metadata, "Elapsed unknown", "Tool starts unknown", "Active tools unknown")
	} else {
		end := progress.settled
		if end.IsZero() {
			end = time.Now()
		}
		metadata = append(metadata, "Elapsed "+end.Sub(progress.activated).Round(time.Second).String(), fmt.Sprintf("Tool starts %d", progress.toolStarts))
		slices.Sort(tools)
		active := "none"
		if len(tools) != 0 {
			active = strings.Join(tools, ", ")
		}
		if progress.toolsClipped {
			if len(tools) == 0 {
				active = "unknown"
			}
			active += " [additional tool activity omitted]"
		}
		metadata = append(metadata, "Active tools "+active)
	}
	if present {
		metadata = append(metadata, "Outcome "+string(outcome.State))
	}
	return toolPaintRows(metadata, "", width)
}

func (p *childPanel) HandleInput(data string) (sdk.RemoteComponentResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.receive()
	if tui.IsKeyRelease(data) {
		return sdk.RemoteComponentResult{}, nil
	}
	key := data
	if decoded, ok := tui.DecodePrintableKey(data); ok {
		key = decoded
	}
	if tui.MatchesKeyID(data, "escape") {
		if p.mode == panelList {
			p.cancel()
			return sdk.RemoteComponentResult{Done: true}, nil
		}
		if p.mode == panelMessage || p.mode == panelResume {
			if p.pendingCancel != nil {
				p.pendingCancel()
			}
			p.mode = p.previous
			p.draft, p.cursor = "", 0
		} else if p.mode == panelReport {
			p.mode, p.scroll = p.previous, p.previousScroll
		} else {
			p.mode, p.scroll = panelList, 0
		}
		return sdk.RemoteComponentResult{}, nil
	}
	if p.pending != nil || p.closed {
		return sdk.RemoteComponentResult{}, nil
	}
	if p.mode == panelMessage || p.mode == panelResume {
		if tui.MatchesKeyID(data, "enter") {
			if strings.TrimSpace(p.draft) != "" {
				kind := humanMessage
				if p.mode == panelResume {
					kind = humanResume
				}
				p.submit(humanAction{kind: kind, mode: p.messageMode, text: p.draft})
			}
		} else if tui.MatchesKeyID(data, "tab") && p.mode == panelMessage {
			if p.messageMode == "steer" {
				p.messageMode = "follow_up"
			} else {
				p.messageMode = "steer"
			}
		} else {
			p.edit(data)
		}
		return sdk.RemoteComponentResult{}, nil
	}
	children := p.owner.tree.panelChildren()
	index := slices.IndexFunc(children, func(child panelChild) bool { return p.selected != nil && child.child.ID == p.selected.ref.ID })
	step := 0
	switch {
	case tui.MatchesKeyID(data, "up"):
		step = -1
	case tui.MatchesKeyID(data, "down"):
		step = 1
	case key == "[" || tui.MatchesKeyID(data, "pageUp"):
		step = -max(1, p.pageRows-1)
	case key == "]" || tui.MatchesKeyID(data, "pageDown"):
		step = max(1, p.pageRows-1)
	case tui.MatchesKeyID(data, "home"):
		step = -int(^uint(0) >> 2)
	case tui.MatchesKeyID(data, "end"):
		step = int(^uint(0) >> 2)
	}
	if step != 0 {
		if p.mode == panelList && len(children) != 0 {
			index = min(max(0, index+step), len(children)-1)
			p.selected = children[index].execution
		} else {
			p.scroll = max(0, p.renderedScroll+step)
		}
		return sdk.RemoteComponentResult{}, nil
	}
	if index < 0 {
		return sdk.RemoteComponentResult{}, nil
	}
	if (key == "m" || key == "r" || key == "p") && !children[index].actionable {
		p.feedback = "Admission pending | wait for the model tool acknowledgement or storage"
		return sdk.RemoteComponentResult{}, nil
	}
	switch {
	case tui.MatchesKeyID(data, "enter"):
		if p.mode == panelList {
			p.mode, p.scroll, p.renderedScroll = panelDetail, 0, 0
		}
	case key == "v":
		if p.mode != panelReport {
			p.previous, p.previousScroll = p.mode, p.scroll
			p.mode, p.scroll, p.renderedScroll = panelReport, 0, 0
		}
	case key == "m" || key == "r":
		if p.mode != panelReport {
			p.previous, p.draft, p.cursor = p.mode, "", 0
			p.mode = panelMessage
			if key == "r" {
				p.mode = panelResume
			}
		}
	case key == "p":
		if p.mode != panelReport {
			p.submit(humanAction{kind: humanPause})
		}
	}
	return sdk.RemoteComponentResult{}, nil
}

func (p *childPanel) submit(action humanAction) {
	p.pendingSelection = p.selected
	input, cancel := context.WithCancel(p.ctx)
	p.pendingCancel = cancel
	p.pending = p.owner.humanAction(input, p.selected, action)
}

func previousGrapheme(text string, end int) int {
	previous, offset := 0, 0
	for rest := text[:end]; rest != ""; {
		previous = offset
		cluster, remaining := widthx.FirstGrapheme(rest)
		offset += len(cluster)
		rest = remaining
	}
	return previous
}

func (p *childPanel) edit(data string) {
	switch {
	case tui.MatchesKeyID(data, "left"):
		p.cursor = previousGrapheme(p.draft, p.cursor)
	case tui.MatchesKeyID(data, "right"):
		cluster, _ := widthx.FirstGrapheme(p.draft[p.cursor:])
		p.cursor += len(cluster)
	case tui.MatchesKeyID(data, "home"):
		p.cursor = 0
	case tui.MatchesKeyID(data, "end"):
		p.cursor = len(p.draft)
	case tui.MatchesKeyID(data, "backspace"):
		left := previousGrapheme(p.draft, p.cursor)
		p.draft = p.draft[:left] + p.draft[p.cursor:]
		p.cursor = left
	case tui.MatchesKeyID(data, "delete"):
		cluster, _ := widthx.FirstGrapheme(p.draft[p.cursor:])
		p.draft = p.draft[:p.cursor] + p.draft[p.cursor+len(cluster):]
	case tui.MatchesKeyID(data, "ctrl+u"):
		p.draft, p.cursor = "", 0
	default:
		if strings.HasPrefix(data, "\x1b[200~") && strings.HasSuffix(data, "\x1b[201~") {
			data = strings.TrimSuffix(strings.TrimPrefix(data, "\x1b[200~"), "\x1b[201~")
		} else if decoded, ok := tui.DecodePrintableKey(data); ok {
			data = decoded
		} else if strings.HasPrefix(data, "\x1b") {
			return
		}
		data = paintText(data, false)
		length := 0
		for rest := data; rest != ""; {
			cluster, remaining := widthx.FirstGrapheme(rest)
			if len(p.draft)+length+len(cluster) > 16384 {
				break
			}
			length += len(cluster)
			rest = remaining
		}
		p.draft = p.draft[:p.cursor] + data[:length] + p.draft[p.cursor:]
		p.cursor += length
	}
	offset, rest := 0, p.draft
	for offset < p.cursor {
		cluster, remaining := widthx.FirstGrapheme(rest)
		offset += len(cluster)
		rest = remaining
	}
	p.cursor = offset
}

func (p *childPanel) draftLine(width int) string {
	before := paintText(p.draft[:p.cursor], false)
	cells := widthx.VisibleWidth(before)
	for before != "" && cells > max(0, width-7) {
		cluster, remaining := widthx.FirstGrapheme(before)
		cells -= widthx.VisibleWidth(cluster)
		before = remaining
	}
	return "Draft " + before + "|" + paintText(p.draft[p.cursor:], false)
}
