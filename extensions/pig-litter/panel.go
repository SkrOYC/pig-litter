package piglitter

import (
	"context"
	"crypto/rand"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
	sdktui "github.com/MichaelKinsy/PiG/extensions/sdk/tui"
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
	panelTranscript
	panelTasks
	panelTaskDetail
	panelTaskFeedback
	panelPlans
	panelPlanDetail
	panelPlanReject
	panelShutdowns
	panelShutdownDetail
	panelShutdownRequest
)

type panelChild struct {
	child      Snapshot
	execution  *run
	actionable bool
}

type childPanel struct {
	mu                    sync.Mutex
	owner                 *owner
	ctx                   context.Context
	cancel                context.CancelFunc
	height                func() int
	mode, previous        panelMode
	previousScroll        int
	selected              *run
	listTop, scroll       int
	renderedScroll        int
	pageRows              int
	draft, messageMode    string
	cursor                int
	feedback              string
	feedbackDetail        string
	pending               <-chan humanResult
	pendingCancel         context.CancelFunc
	pendingSelection      *run
	invalidate            func()
	invalidateWG          sync.WaitGroup
	stop, joined          chan struct{}
	dispose               sync.Once
	closed                bool
	reportRun             *run
	reportOutcome         Outcome
	reportPresent         bool
	reportState           State
	reportWidth           int
	reportLines           []string
	team                  *teamView
	member                MemberRef
	memberSelected        bool
	teamRequest           uint64
	messageTarget         *humanMember
	task                  TaskID
	taskSelected          bool
	taskTop               int
	tasksReturn           panelMode
	tasksReturnScroll     int
	plan                  PlanRequestID
	planSelected          bool
	planTop               int
	plansReturn           panelMode
	plansReturnScroll     int
	shutdown              ShutdownRequestID
	shutdownSelected      bool
	shutdownTop           int
	shutdownsReturn       panelMode
	shutdownsReturnScroll int
	shutdownTarget        *humanMember
	shutdownSubmission    string
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
	team := strings.TrimSpace(args) == "team"
	if strings.TrimSpace(args) != "" && !team {
		ctx.Notify("Use /pig-litter to manage retained children, /pig-litter team to view teammates, or /pig-litter help for tool instructions.", "info")
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
	if team {
		panel.mu.Lock()
		panel.team = newTeamView(panel.ctx, o, panel.requestRender)
		panel.mu.Unlock()
	}
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
				if p.team != nil {
					invalidate = nil
				}
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

func (p *childPanel) requestRender() {
	p.mu.Lock()
	invalidate := p.invalidate
	if p.closed {
		invalidate = nil
	}
	if invalidate != nil {
		p.invalidateWG.Add(1)
	}
	p.mu.Unlock()
	if invalidate != nil {
		invalidate()
		p.invalidateWG.Done()
	}
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
		if p.team != nil {
			p.team.Close()
		}
		p.invalidateWG.Wait()
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
		if p.team == nil && p.selected == p.pendingSelection && result.selected != nil {
			p.selected = result.selected
		}
		if result.err != nil {
			p.feedback = "Action failed | " + result.err.Error()
		} else {
			p.feedback = result.feedback
			if result.detail != "" && p.team != nil {
				p.previous, p.previousScroll = p.mode, p.scroll
				p.mode, p.scroll, p.renderedScroll = panelTaskFeedback, 0, 0
				p.feedbackDetail = result.detail
			}
			if p.mode == panelMessage || p.mode == panelResume || p.mode == panelPlanReject || p.mode == panelShutdownRequest {
				p.mode = p.previous
				p.draft, p.cursor = "", 0
				p.messageTarget = nil
				p.shutdownTarget, p.shutdownSubmission = nil, ""
			}
		}
	default:
	}
}

func panelKeyMatches(data, binding string) bool {
	return sdktui.GetKeybindings().Matches(data, binding)
}

func panelKeyText(binding string) string {
	key := sdk.KeyDisplayText(binding)
	if key == "" {
		return "unbound"
	}
	return key
}

func panelMoveKeys() string {
	return panelKeyText("tui.select.up") + "/" + panelKeyText("tui.select.down")
}

func panelPageKeys() string {
	return "[ ]/" + panelKeyText("tui.select.pageUp") + "/" + panelKeyText("tui.select.pageDown")
}

func (p *childPanel) Render(width int) []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.receive()
	p.pageRows = 0
	if width <= 0 {
		return []string{}
	}
	if p.team != nil {
		return p.renderTeam(width)
	}
	height := p.height()
	if height <= 0 {
		height = 24
	}
	budget := max(1, height-8)
	legends := []string{panelMoveKeys() + " move  " + panelKeyText("tui.select.confirm") + " inspect  v result", "m message  p pause subtree  r resume", panelPageKeys() + " page  Home/End first/last", panelKeyText("tui.select.cancel") + " back/close"}
	if width < 40 {
		legends = []string{panelMoveKeys() + " move " + panelKeyText("tui.select.confirm") + " detail", "v result m msg p pause subtree", "r resume [ ] page Home/End", panelKeyText("tui.select.cancel") + " back/close"}
	}
	if width < 30 {
		legends = []string{panelMoveKeys() + " move", panelKeyText("tui.select.confirm") + " detail", "v result m message", "p pause subtree", "r resume " + panelPageKeys() + " page", panelKeyText("tui.select.cancel") + " back/close"}
	}
	if p.mode == panelMessage || p.mode == panelResume {
		legends = []string{panelKeyText("tui.input.submit") + " submit  " + panelKeyText("tui.select.cancel") + " cancel", panelKeyText("tui.editor.cursorLeft") + "/" + panelKeyText("tui.editor.cursorRight") + " edit  " + panelKeyText("tui.editor.deleteToLineStart") + " clear"}
		if p.mode == panelMessage {
			legends = append(legends, panelKeyText("tui.input.tab")+" steer/follow_up")
		}
	} else if p.mode == panelReport {
		legends = []string{panelMoveKeys() + " scroll  " + panelPageKeys() + " page", "Home/End  " + panelKeyText("tui.select.cancel") + " back"}
	} else if p.mode == panelDetail {
		legends[0] = panelMoveKeys() + " scroll  v result"
	}
	if width >= 80 {
		if p.mode == panelList {
			groups := []string{strings.Join(legends[:2], "  "), strings.Join(legends[2:], "  ")}
			if widthx.VisibleWidth(paintText(groups[0], false)) > width {
				groups = []string{legends[0], legends[1], groups[1]}
			}
			legends = nil
			for _, group := range groups {
				legends = append(legends, widthx.WrapTextWithAnsi(paintText(group, false), width)...)
			}
		} else {
			legends = widthx.WrapTextWithAnsi(paintText(strings.Join(legends, "  "), false), width)
		}
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
	if p.team != nil {
		return p.handleTeamInput(data, key), nil
	}
	if panelKeyMatches(data, "tui.select.cancel") {
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
		p.composerInput(data)
		return sdk.RemoteComponentResult{}, nil
	}
	children := p.owner.tree.panelChildren()
	index := slices.IndexFunc(children, func(child panelChild) bool { return p.selected != nil && child.child.ID == p.selected.ref.ID })
	step := 0
	switch {
	case panelKeyMatches(data, "tui.select.up"):
		step = -1
	case panelKeyMatches(data, "tui.select.down"):
		step = 1
	case key == "[" || panelKeyMatches(data, "tui.select.pageUp"):
		step = -max(1, p.pageRows-1)
	case key == "]" || panelKeyMatches(data, "tui.select.pageDown"):
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
	case panelKeyMatches(data, "tui.select.confirm"):
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

func (p *childPanel) composerInput(data string) {
	if panelKeyMatches(data, "tui.input.submit") {
		if strings.TrimSpace(p.draft) != "" {
			if p.mode == panelPlanReject {
				p.decideSelectedPlan(p.team.Snapshot(), "reject", p.draft)
				return
			}
			if p.mode == panelShutdownRequest {
				p.requestShutdown(p.draft)
				return
			}
			kind := humanMessage
			if p.mode == panelResume {
				kind = humanResume
			}
			p.submit(humanAction{kind: kind, mode: p.messageMode, text: p.draft})
		}
	} else if panelKeyMatches(data, "tui.input.tab") && p.mode == panelMessage {
		if p.messageMode == "steer" {
			p.messageMode = "follow_up"
		} else {
			p.messageMode = "steer"
		}
	} else {
		p.edit(data)
	}
}

func (p *childPanel) submit(action humanAction) {
	selected := p.selected
	if p.team != nil {
		member := action.member
		if member == nil {
			member = p.messageTarget
		}
		if member == nil {
			p.feedback = "Selected teammate is unavailable"
			return
		}
		action.member = member
		var err error
		selected, err = p.owner.teamMessageExecution(*member)
		if err != nil {
			p.feedback = "Action failed | " + err.Error()
			return
		}
	}
	p.pendingSelection = selected
	input, cancel := context.WithCancel(p.ctx)
	p.pendingCancel = cancel
	p.pending = p.owner.humanAction(input, selected, action)
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
	case panelKeyMatches(data, "tui.editor.cursorLeft"):
		p.cursor = previousGrapheme(p.draft, p.cursor)
	case panelKeyMatches(data, "tui.editor.cursorRight"):
		cluster, _ := widthx.FirstGrapheme(p.draft[p.cursor:])
		p.cursor += len(cluster)
	case panelKeyMatches(data, "tui.editor.cursorLineStart"):
		p.cursor = 0
	case panelKeyMatches(data, "tui.editor.cursorLineEnd"):
		p.cursor = len(p.draft)
	case panelKeyMatches(data, "tui.editor.deleteCharBackward"):
		left := previousGrapheme(p.draft, p.cursor)
		p.draft = p.draft[:left] + p.draft[p.cursor:]
		p.cursor = left
	case panelKeyMatches(data, "tui.editor.deleteCharForward"):
		cluster, _ := widthx.FirstGrapheme(p.draft[p.cursor:])
		p.draft = p.draft[:p.cursor] + p.draft[p.cursor+len(cluster):]
	case panelKeyMatches(data, "tui.editor.deleteToLineStart"):
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

func (p *childPanel) teamSelection(frame *teamViewFrame) int {
	if !p.memberSelected && frame.PreferenceLoaded {
		if frame.Preference != nil {
			p.member, p.memberSelected = *frame.Preference, true
		} else if len(frame.Members) != 0 {
			p.member, p.memberSelected = frame.Members[0].Member.Ref, true
		}
		if p.memberSelected {
			p.team.Select(p.member)
		}
	}
	if !p.memberSelected {
		return -1
	}
	return slices.IndexFunc(frame.Members, func(member teamMemberView) bool { return member.Member.Ref == p.member })
}

func (p *childPanel) taskSelection(frame *teamViewFrame) int {
	if !p.taskSelected && len(frame.Tasks) != 0 {
		p.task, p.taskSelected = frame.Tasks[0].ID, true
	}
	if !p.taskSelected {
		return -1
	}
	return slices.IndexFunc(frame.Tasks, func(task teamTaskView) bool { return task.ID == p.task })
}

func (p *childPanel) planSelection(frame *teamViewFrame) int {
	if !p.planSelected && len(frame.Plans) != 0 {
		index := slices.IndexFunc(frame.Plans, func(plan planView) bool { return plan.Request.State == "pending" })
		if index < 0 {
			index = 0
		}
		p.plan, p.planSelected = frame.Plans[index].Request.ID, true
	}
	if !p.planSelected {
		return -1
	}
	return slices.IndexFunc(frame.Plans, func(plan planView) bool { return plan.Request.ID == p.plan })
}

func (p *childPanel) shutdownSelection(frame *teamViewFrame) int {
	if !p.shutdownSelected && len(frame.Shutdowns) != 0 {
		index := slices.IndexFunc(frame.Shutdowns, func(view shutdownView) bool { return view.Request.State == "pending" })
		if index < 0 {
			index = 0
		}
		p.shutdown, p.shutdownSelected = frame.Shutdowns[index].Request.ID, true
	}
	if !p.shutdownSelected {
		return -1
	}
	return slices.IndexFunc(frame.Shutdowns, func(view shutdownView) bool { return view.Request.ID == p.shutdown })
}

func (p *childPanel) renderTeam(width int) []string {
	frame := p.team.Snapshot()
	index := p.teamSelection(frame)
	height := p.height()
	if height <= 0 {
		height = 24
	}
	budget := max(1, height-8)
	legend := []string{panelMoveKeys() + " move  " + panelKeyText("tui.select.confirm") + " detail  v report  t transcript", "m message  p pause  s shutdown  h shutdowns  k tasks  g plans", panelPageKeys() + " page  Home/End  " + panelKeyText("tui.select.cancel") + " close"}
	if p.mode == panelMessage || p.mode == panelPlanReject || p.mode == panelShutdownRequest {
		legend = []string{panelKeyText("tui.input.submit") + " submit  " + panelKeyText("tui.select.cancel") + " cancel", panelKeyText("tui.editor.cursorLeft") + "/" + panelKeyText("tui.editor.cursorRight") + " edit  " + panelKeyText("tui.editor.deleteToLineStart") + " clear  " + panelKeyText("tui.input.tab") + " steer/follow_up"}
		if p.mode != panelMessage {
			legend[1] = panelKeyText("tui.editor.cursorLeft") + "/" + panelKeyText("tui.editor.cursorRight") + " edit  " + panelKeyText("tui.editor.deleteToLineStart") + " clear"
		}
	} else if p.mode == panelTasks {
		legend = []string{panelMoveKeys() + " move  " + panelKeyText("tui.select.confirm") + " detail  c complete  " + panelPageKeys() + " page  Home/End  " + panelKeyText("tui.select.cancel") + " back"}
	} else if p.mode == panelTaskDetail {
		legend = []string{panelMoveKeys() + " scroll  c complete  " + panelPageKeys() + " page  Home/End  " + panelKeyText("tui.select.cancel") + " back"}
	} else if p.mode == panelPlans {
		legend = []string{panelMoveKeys() + " move  " + panelKeyText("tui.select.confirm") + " detail  a approve  r reject  " + panelPageKeys() + " page  " + panelKeyText("tui.select.cancel") + " back"}
	} else if p.mode == panelPlanDetail {
		legend = []string{panelMoveKeys() + " scroll  a approve  r reject  " + panelPageKeys() + " page  " + panelKeyText("tui.select.cancel") + " back"}
	} else if p.mode == panelShutdowns {
		legend = []string{panelMoveKeys() + " move  " + panelKeyText("tui.select.confirm") + " detail  x cancel request  " + panelPageKeys() + " page  " + panelKeyText("tui.select.cancel") + " back"}
	} else if p.mode == panelShutdownDetail {
		legend = []string{panelMoveKeys() + " scroll  x cancel request  " + panelPageKeys() + " page  " + panelKeyText("tui.select.cancel") + " back"}
	} else if p.mode != panelList && p.mode != panelDetail {
		legend = []string{panelMoveKeys() + " scroll  " + panelPageKeys() + " page  Home/End  " + panelKeyText("tui.select.cancel") + " back"}
	} else if p.mode == panelDetail {
		legend = []string{panelMoveKeys() + " scroll  v report  t transcript", "m message  p pause  s shutdown  h shutdowns  k tasks  g plans", panelKeyText("tui.select.cancel") + " back"}
	}
	if width >= 80 {
		legend = widthx.WrapTextWithAnsi(paintText(strings.Join(legend, "  "), false), width)
	}
	if budget <= 2 {
		return panelPaint(legend[len(legend)-min(budget, len(legend)):], width)
	}
	legendRows := min(len(legend), budget-2)
	legend = legend[len(legend)-legendRows:]
	contentBudget := max(0, budget-legendRows-2)
	p.pageRows = contentBudget
	title := "Pig Litter | team"
	var content []string
	if p.mode == panelList {
		title += fmt.Sprintf(" (%d/%d)", index+1, len(frame.Members))
		for _, section := range []struct {
			name   string
			health viewHealth
		}{{"Roster", frame.RosterHealth}, {"Tasks", frame.TaskHealth}, {"Selection", frame.PreferenceHealth}} {
			if section.health.Error != "" {
				content = append(content, section.name+" "+section.health.Status+" | "+section.health.Error)
			}
		}
		if frame.Team == nil && frame.RosterHealth.Status == "loading" {
			content = append(content, "Loading team")
		} else if frame.Team == nil {
			content = append(content, "No team")
		} else if len(frame.Members) == 0 {
			content = append(content, "No committed teammates")
		}
		if p.memberSelected && index < 0 {
			content = append(content, "Selected member unavailable | "+string(p.member.ID)+fmt.Sprintf(" generation %d", p.member.Generation))
		}
		if len(frame.Reservations) != 0 {
			content = append(content, fmt.Sprintf("Pending reservations %d", len(frame.Reservations)))
		}
		reserveRows := min(len(frame.Reservations), max(0, contentBudget-len(content)-1))
		rows := max(0, contentBudget-len(content)-reserveRows)
		p.listTop = min(p.listTop, max(0, len(frame.Members)-rows))
		if index >= 0 && index < p.listTop {
			p.listTop = index
		}
		if rows > 0 && index >= p.listTop+rows {
			p.listTop = index - rows + 1
		}
		for i := p.listTop; i < min(len(frame.Members), p.listTop+rows); i++ {
			member := frame.Members[i]
			prefix := "  "
			if i == index {
				prefix = "> "
			}
			state := "execution unavailable"
			if member.Execution != nil {
				state = "execution " + string(member.Execution.Child.State)
			}
			assigned := len(frame.Assigned[member.Member.Ref])
			content = append(content, fmt.Sprintf("%s%s | member %s | %s | tasks %d", prefix, member.Member.Name, member.Member.State, state, assigned))
		}
		for _, pending := range frame.Reservations[:reserveRows] {
			content = append(content, "  "+pending.Name+" | reservation "+pending.State)
		}
	} else if p.mode == panelTasks {
		taskIndex := p.taskSelection(frame)
		title += fmt.Sprintf(" | tasks (%d/%d)", taskIndex+1, len(frame.Tasks))
		if frame.TaskHealth.Error != "" {
			content = append(content, "Tasks "+frame.TaskHealth.Status+" | "+frame.TaskHealth.Error)
		}
		if p.taskSelected && taskIndex < 0 {
			content = append(content, fmt.Sprintf("Selected task #%d unavailable", p.task.Number))
		} else if len(frame.Tasks) == 0 {
			content = append(content, "No tasks")
		}
		rows := max(0, contentBudget-len(content))
		p.taskTop = min(p.taskTop, max(0, len(frame.Tasks)-rows))
		if taskIndex >= 0 && taskIndex < p.taskTop {
			p.taskTop = taskIndex
		}
		if rows > 0 && taskIndex >= p.taskTop+rows {
			p.taskTop = taskIndex - rows + 1
		}
		for i := p.taskTop; i < min(len(frame.Tasks), p.taskTop+rows); i++ {
			prefix := "  "
			if i == taskIndex {
				prefix = "> "
			}
			content = append(content, prefix+teamTaskSummary(frame, frame.Tasks[i]))
		}
	} else if p.mode == panelPlans {
		planIndex := p.planSelection(frame)
		title += fmt.Sprintf(" | plans (%d/%d)", planIndex+1, len(frame.Plans))
		if frame.PlanHealth.Error != "" {
			content = append(content, "Plans "+frame.PlanHealth.Status+" | "+frame.PlanHealth.Error)
		}
		if frame.Team == nil && frame.RosterHealth.Status == "fresh" {
			content = append(content, "No team")
		} else if p.planSelected && planIndex < 0 {
			content = append(content, "Selected plan unavailable")
		} else if frame.PlanHealth.Status == "loading" {
			content = append(content, "Loading plans")
		} else if len(frame.Plans) == 0 {
			content = append(content, "No plans")
		}
		rows := max(0, contentBudget-len(content))
		p.planTop = min(p.planTop, max(0, len(frame.Plans)-rows))
		if planIndex >= 0 && planIndex < p.planTop {
			p.planTop = planIndex
		}
		if rows > 0 && planIndex >= p.planTop+rows {
			p.planTop = planIndex - rows + 1
		}
		for i := p.planTop; i < min(len(frame.Plans), p.planTop+rows); i++ {
			prefix := "  "
			if i == planIndex {
				prefix = "> "
			}
			content = append(content, prefix+teamPlanSummary(frame, frame.Plans[i]))
		}
	} else if p.mode == panelShutdowns {
		shutdownIndex := p.shutdownSelection(frame)
		title += fmt.Sprintf(" | shutdowns (%d/%d)", shutdownIndex+1, len(frame.Shutdowns))
		if frame.ShutdownHealth.Error != "" {
			content = append(content, "Shutdowns "+frame.ShutdownHealth.Status+" | "+frame.ShutdownHealth.Error)
		}
		if frame.Team == nil && frame.RosterHealth.Status == "fresh" {
			content = append(content, "No team")
		} else if p.shutdownSelected && shutdownIndex < 0 {
			content = append(content, "Selected shutdown request unavailable")
		} else if frame.ShutdownHealth.Status == "loading" {
			content = append(content, "Loading shutdown requests")
		} else if len(frame.Shutdowns) == 0 {
			content = append(content, "No shutdown requests")
		}
		rows := max(0, contentBudget-len(content))
		p.shutdownTop = min(p.shutdownTop, max(0, len(frame.Shutdowns)-rows))
		if shutdownIndex >= 0 && shutdownIndex < p.shutdownTop {
			p.shutdownTop = shutdownIndex
		}
		if rows > 0 && shutdownIndex >= p.shutdownTop+rows {
			p.shutdownTop = shutdownIndex - rows + 1
		}
		for i := p.shutdownTop; i < min(len(frame.Shutdowns), p.shutdownTop+rows); i++ {
			prefix := "  "
			if i == shutdownIndex {
				prefix = "> "
			}
			content = append(content, prefix+teamShutdownSummary(frame, frame.Shutdowns[i]))
		}
	} else {
		switch p.mode {
		case panelDetail:
			title += " | detail"
			content = p.teamDetail(frame, index, width, false)
		case panelReport:
			title += " | report"
			content = p.teamDetail(frame, index, width, true)
		case panelTranscript:
			title += " | transcript"
			content = p.teamTranscriptLines(frame, width)
		case panelTaskDetail:
			title += " | task detail"
			taskIndex := p.taskSelection(frame)
			if taskIndex < 0 {
				content = []string{"Selected task unavailable"}
			} else {
				content = toolPaintRows(teamTaskDetail(frame, frame.Tasks[taskIndex]), "", width)
			}
		case panelTaskFeedback:
			title += " | task feedback"
			content = toolPaintRows([]string{"Task completion was refused", "Policy feedback", p.feedbackDetail}, "", width)
		case panelPlanDetail:
			title += " | plan detail"
			planIndex := p.planSelection(frame)
			if planIndex < 0 {
				content = []string{"Selected plan unavailable"}
			} else {
				content = toolPaintRows(teamPlanDetail(frame, frame.Plans[planIndex]), "", width)
			}
		case panelPlanReject:
			title += " | reject plan"
			content = []string{fmt.Sprintf("Reject plan #%d", p.plan.Sequence), "Enter feedback for the teammate", p.draftLine(width)}
		case panelShutdownDetail:
			title += " | shutdown detail"
			shutdownIndex := p.shutdownSelection(frame)
			if shutdownIndex < 0 {
				content = []string{"Selected shutdown request unavailable"}
			} else {
				content = toolPaintRows(teamShutdownDetail(frame, frame.Shutdowns[shutdownIndex]), "", width)
			}
		case panelShutdownRequest:
			title += " | request shutdown"
			content = []string{"Response window 5 minutes"}
			if p.shutdownTarget != nil {
				content = append(content, "Target "+p.shutdownTarget.Name+" | UUID "+string(p.shutdownTarget.Ref.ID))
			}
			content = append(content, "Enter a reason for the teammate", p.draftLine(width))
		case panelMessage:
			title += " | message"
			content = []string{"Message mode " + p.messageMode}
			if p.messageTarget != nil {
				content = append(content, "Target "+p.messageTarget.Name+" | UUID "+string(p.messageTarget.Ref.ID))
			}
			content = append(content, p.draftLine(width))
		}
		var issues []string
		for _, section := range []struct {
			name   string
			health viewHealth
		}{{"Roster", frame.RosterHealth}, {"Tasks", frame.TaskHealth}, {"Plans", frame.PlanHealth}, {"Shutdowns", frame.ShutdownHealth}, {"Selection", frame.PreferenceHealth}} {
			if section.health.Error != "" {
				issues = append(issues, section.name+" "+section.health.Status+" | "+section.health.Error)
			}
		}
		content = append(issues, content...)
		p.renderedScroll = min(p.scroll, max(0, len(content)-contentBudget))
		content = content[p.renderedScroll:min(len(content), p.renderedScroll+contentBudget)]
	}
	content = content[:min(len(content), contentBudget)]
	for len(content) < contentBudget {
		content = append(content, "")
	}
	rows := append([]string{title}, content...)
	feedback := p.feedback
	if p.pending != nil {
		feedback = "Working"
	}
	rows = append(rows, feedback)
	rows = append(rows, legend...)
	return panelPaint(rows, width)
}

func (p *childPanel) teamDetail(frame *teamViewFrame, index, width int, report bool) []string {
	if index < 0 {
		return []string{"Selected member is unavailable"}
	}
	view := frame.Members[index]
	member := view.Member
	metadata := []string{"Name " + member.Name, "UUID " + string(member.Ref.ID), fmt.Sprintf("Team %s epoch %d | generation %d", member.Ref.Team.Root, member.Ref.Team.Epoch, member.Ref.Generation), "Membership " + member.State, "Workspace " + member.Workspace.Cwd}
	if view.Execution == nil {
		metadata = append(metadata, "Execution unavailable")
		if report {
			return append(metadata, "No settled report")
		}
		metadata = append(metadata, "Assigned tasks")
		return append(metadata, teamTaskLines(frame, &member.Ref)...)
	}
	execution := view.Execution
	metadata = append(metadata, "Execution "+string(execution.Child.State), "Admission "+execution.Admission)
	outcome := execution.Latest
	if report {
		if outcome == nil {
			return append(metadata, "No settled report")
		}
		metadata = append(metadata, "Outcome "+string(outcome.State))
		if outcome.Error != "" {
			metadata = append(metadata, "Error "+outcome.Error)
		}
		if outcome.ReportProvenance != "" {
			metadata = append(metadata, "Report provenance "+outcome.ReportProvenance)
		}
		if outcome.Usage == (Usage{}) {
			metadata = append(metadata, "Latest settled usage unknown")
		} else {
			usage := outcome.Usage
			metadata = append(metadata, fmt.Sprintf("Latest settled usage | input %d output %d cache read %d cache write %d total %d cost %.6f", usage.Input, usage.Output, usage.CacheRead, usage.CacheWrite, usage.TotalTokens, usage.Cost))
		}
		if outcome.Truncated || outcome.HandbackTruncated {
			metadata = append(metadata, "Report was truncated")
		}
		return toolPaintRows(append(metadata, "Report"), outcome.Text, width)
	}
	progress := execution.Progress
	if progress.activated.IsZero() {
		metadata = append(metadata, "Elapsed unknown", "Tool starts unknown", "Active tools unknown")
	} else {
		end := progress.settled
		if end.IsZero() {
			end = time.Now()
		}
		metadata = append(metadata, "Elapsed "+end.Sub(progress.activated).Round(time.Second).String(), fmt.Sprintf("Tool starts %d", progress.toolStarts))
		tools := "none"
		if len(execution.Tools) != 0 {
			tools = strings.Join(execution.Tools, ", ")
		}
		if progress.toolsClipped {
			tools += " [additional tool activity omitted]"
		}
		metadata = append(metadata, "Active tools "+tools)
	}
	metadata = append(metadata, "Assigned tasks")
	metadata = append(metadata, teamTaskLines(frame, &member.Ref)...)
	return toolPaintRows(metadata, "", width)
}

func teamTaskLines(frame *teamViewFrame, owner *MemberRef) []string {
	var lines []string
	if frame.TaskHealth.Error != "" {
		lines = append(lines, "Tasks "+frame.TaskHealth.Status+" | "+frame.TaskHealth.Error)
	}
	for _, task := range frame.Tasks {
		if owner != nil && (task.Owner == nil || *task.Owner != *owner) {
			continue
		}
		lines = append(lines, teamTaskDetail(frame, task)...)
	}
	if len(lines) == 0 {
		return []string{"No tasks"}
	}
	return lines
}

func teamTaskSummary(frame *teamViewFrame, task teamTaskView) string {
	assigned := "unassigned"
	if task.Owner != nil {
		assigned = string(task.Owner.ID) + fmt.Sprintf(" generation %d", task.Owner.Generation)
		if i := slices.IndexFunc(frame.Members, func(member teamMemberView) bool { return member.Member.Ref == *task.Owner }); i >= 0 {
			assigned = frame.Members[i].Member.Name
		}
	}
	line := fmt.Sprintf("#%d %s", task.ID.Number, task.State)
	if task.Blocked {
		line += " | blocked"
	}
	return line + " | " + assigned + " | " + task.Subject
}

func teamTaskDetail(frame *teamViewFrame, task teamTaskView) []string {
	state := fmt.Sprintf("Task #%d | state %s", task.ID.Number, task.State)
	if task.Blocked {
		state += " | blocked"
	}
	lines := []string{state}
	if task.Owner == nil {
		lines = append(lines, "Owner unassigned")
	} else {
		if i := slices.IndexFunc(frame.Members, func(member teamMemberView) bool { return member.Member.Ref == *task.Owner }); i >= 0 {
			lines = append(lines, "Owner "+frame.Members[i].Member.Name)
		}
		lines = append(lines, fmt.Sprintf("Owner UUID %s | generation %d", task.Owner.ID, task.Owner.Generation))
	}
	lines = append(lines, "Subject "+task.Subject)
	if task.Description != "" {
		lines = append(lines, "  "+task.Description)
	}
	if len(task.BlockedBy) != 0 {
		dependencies := make([]string, len(task.BlockedBy))
		for i, dependency := range task.BlockedBy {
			dependencies[i] = fmt.Sprintf("#%d", dependency.Number)
		}
		lines = append(lines, "  Depends on "+strings.Join(dependencies, ", "))
	}
	keys := make([]string, 0, len(task.Metadata))
	for key := range task.Metadata {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		lines = append(lines, "  "+key+" "+task.Metadata[key])
	}
	return lines
}

func (p *childPanel) teamTranscriptLines(frame *teamViewFrame, width int) []string {
	if !slices.ContainsFunc(frame.Members, func(member teamMemberView) bool { return member.Member.Ref == p.member }) {
		return []string{"Selected member is unavailable"}
	}
	transcript := frame.Transcript
	if transcript.Member != p.member || transcript.Request != p.teamRequest {
		return []string{"Transcript unavailable for this selection"}
	}
	if transcript.Health.Status == "loading" {
		return []string{"Loading transcript"}
	}
	if transcript.Health.Error != "" {
		return []string{"Transcript unavailable | " + transcript.Health.Error}
	}
	var lines []string
	for _, entry := range transcript.Entries {
		lines = append(lines, widthx.WrapTextWithAnsi(paintText(entry.Role+" | "+entry.Text, true), width)...)
	}
	if len(lines) == 0 {
		lines = append(lines, "No conversation text")
	}
	if transcript.Clipped {
		lines = append(lines, "[Transcript display clipped]")
	}
	return lines
}

func (p *childPanel) handleTeamInput(data, key string) sdk.RemoteComponentResult {
	if panelKeyMatches(data, "tui.select.cancel") {
		switch p.mode {
		case panelList:
			p.cancel()
			return sdk.RemoteComponentResult{Done: true}
		case panelReport, panelTranscript:
			p.mode, p.scroll = p.previous, p.previousScroll
		case panelMessage, panelPlanReject, panelShutdownRequest:
			if p.pendingCancel != nil {
				p.pendingCancel()
			}
			p.mode, p.scroll = p.previous, p.previousScroll
			p.draft, p.cursor, p.messageTarget = "", 0, nil
			p.shutdownTarget, p.shutdownSubmission = nil, ""
		case panelTasks:
			p.mode, p.scroll = p.tasksReturn, p.tasksReturnScroll
		case panelTaskDetail:
			p.mode, p.scroll = panelTasks, 0
		case panelTaskFeedback:
			p.mode, p.scroll = p.previous, p.previousScroll
			p.feedbackDetail = ""
		case panelPlans:
			p.team.watchControl(teamControlNone)
			p.mode, p.scroll = p.plansReturn, p.plansReturnScroll
		case panelPlanDetail:
			p.mode, p.scroll = panelPlans, 0
		case panelShutdowns:
			p.team.watchControl(teamControlNone)
			p.mode, p.scroll = p.shutdownsReturn, p.shutdownsReturnScroll
		case panelShutdownDetail:
			p.mode, p.scroll = panelShutdowns, 0
		default:
			p.mode, p.scroll = panelList, 0
		}
		return sdk.RemoteComponentResult{}
	}
	if p.closed || p.pending != nil {
		return sdk.RemoteComponentResult{}
	}
	if p.mode == panelMessage || p.mode == panelPlanReject || p.mode == panelShutdownRequest {
		p.composerInput(data)
		return sdk.RemoteComponentResult{}
	}
	frame := p.team.Snapshot()
	index := p.teamSelection(frame)
	step := 0
	switch {
	case panelKeyMatches(data, "tui.select.up"):
		step = -1
	case panelKeyMatches(data, "tui.select.down"):
		step = 1
	case key == "[" || panelKeyMatches(data, "tui.select.pageUp"):
		step = -max(1, p.pageRows-1)
	case key == "]" || panelKeyMatches(data, "tui.select.pageDown"):
		step = max(1, p.pageRows-1)
	case tui.MatchesKeyID(data, "home"):
		step = -int(^uint(0) >> 2)
	case tui.MatchesKeyID(data, "end"):
		step = int(^uint(0) >> 2)
	}
	if step != 0 {
		if p.mode == panelList && len(frame.Members) != 0 {
			if index < 0 {
				index = 0
				if step < 0 {
					index = len(frame.Members) - 1
				}
			} else {
				index = min(max(0, index+step), len(frame.Members)-1)
			}
			p.member, p.memberSelected = frame.Members[index].Member.Ref, true
			p.team.Select(p.member)
		} else if p.mode == panelTasks && len(frame.Tasks) != 0 {
			taskIndex := p.taskSelection(frame)
			if taskIndex < 0 {
				taskIndex = 0
				if step < 0 {
					taskIndex = len(frame.Tasks) - 1
				}
			} else {
				taskIndex = min(max(0, taskIndex+step), len(frame.Tasks)-1)
			}
			p.task, p.taskSelected = frame.Tasks[taskIndex].ID, true
		} else if p.mode == panelPlans && len(frame.Plans) != 0 {
			planIndex := p.planSelection(frame)
			if planIndex < 0 {
				planIndex = 0
				if step < 0 {
					planIndex = len(frame.Plans) - 1
				}
			} else {
				planIndex = min(max(0, planIndex+step), len(frame.Plans)-1)
			}
			p.plan, p.planSelected = frame.Plans[planIndex].Request.ID, true
		} else if p.mode == panelShutdowns && len(frame.Shutdowns) != 0 {
			shutdownIndex := p.shutdownSelection(frame)
			if shutdownIndex < 0 {
				shutdownIndex = 0
				if step < 0 {
					shutdownIndex = len(frame.Shutdowns) - 1
				}
			} else {
				shutdownIndex = min(max(0, shutdownIndex+step), len(frame.Shutdowns)-1)
			}
			p.shutdown, p.shutdownSelected = frame.Shutdowns[shutdownIndex].Request.ID, true
		} else {
			p.scroll = max(0, p.renderedScroll+step)
		}
		return sdk.RemoteComponentResult{}
	}
	if key == "k" && (p.mode == panelList || p.mode == panelDetail) {
		p.tasksReturn, p.tasksReturnScroll = p.mode, p.scroll
		p.mode, p.scroll, p.renderedScroll = panelTasks, 0, 0
		return sdk.RemoteComponentResult{}
	}
	if key == "g" && (p.mode == panelList || p.mode == panelDetail) {
		p.plansReturn, p.plansReturnScroll = p.mode, p.scroll
		p.mode, p.scroll, p.renderedScroll = panelPlans, 0, 0
		p.feedback = ""
		p.team.watchControl(teamControlPlans)
		return sdk.RemoteComponentResult{}
	}
	if key == "h" && (p.mode == panelList || p.mode == panelDetail) {
		p.shutdownsReturn, p.shutdownsReturnScroll = p.mode, p.scroll
		p.mode, p.scroll, p.renderedScroll = panelShutdowns, 0, 0
		p.feedback = ""
		p.team.watchControl(teamControlShutdowns)
		return sdk.RemoteComponentResult{}
	}
	if p.mode == panelTasks && panelKeyMatches(data, "tui.select.confirm") {
		if p.taskSelection(frame) >= 0 {
			p.mode, p.scroll, p.renderedScroll = panelTaskDetail, 0, 0
		}
		return sdk.RemoteComponentResult{}
	}
	if key == "c" && (p.mode == panelTasks || p.mode == panelTaskDetail) {
		p.completeSelectedTask(frame)
		return sdk.RemoteComponentResult{}
	}
	if p.mode == panelPlans && panelKeyMatches(data, "tui.select.confirm") {
		if p.planSelection(frame) >= 0 {
			p.mode, p.scroll, p.renderedScroll = panelPlanDetail, 0, 0
		}
		return sdk.RemoteComponentResult{}
	}
	if (key == "a" || key == "r") && (p.mode == panelPlans || p.mode == panelPlanDetail) {
		planIndex := p.planSelection(frame)
		if frame.RosterHealth.Status != "fresh" || frame.PlanHealth.Status != "fresh" || planIndex < 0 || frame.Plans[planIndex].Request.State != "pending" {
			p.feedback = "Selected plan is unavailable or no longer pending"
			return sdk.RemoteComponentResult{}
		}
		if key == "a" {
			p.decideSelectedPlan(frame, "approve", "")
		} else {
			p.previous, p.previousScroll = p.mode, p.scroll
			p.mode, p.draft, p.cursor = panelPlanReject, "", 0
			p.feedback = ""
		}
		return sdk.RemoteComponentResult{}
	}
	if p.mode == panelShutdowns && panelKeyMatches(data, "tui.select.confirm") {
		if p.shutdownSelection(frame) >= 0 {
			p.mode, p.scroll, p.renderedScroll = panelShutdownDetail, 0, 0
		}
		return sdk.RemoteComponentResult{}
	}
	if key == "x" && (p.mode == panelShutdowns || p.mode == panelShutdownDetail) {
		p.cancelSelectedShutdown(frame)
		return sdk.RemoteComponentResult{}
	}
	if index < 0 {
		return sdk.RemoteComponentResult{}
	}
	switch {
	case key == "s" && (p.mode == panelList || p.mode == panelDetail):
		if frame.RosterHealth.Status != "fresh" {
			p.feedback = "Selected teammate is unavailable"
			return sdk.RemoteComponentResult{}
		}
		member := frame.Members[index].Member
		p.shutdownTarget = &humanMember{Ref: member.Ref, AdmissionID: member.AdmissionID, Name: member.Name}
		p.shutdownSubmission = rand.Text()
		p.previous, p.previousScroll = p.mode, p.scroll
		p.mode, p.draft, p.cursor = panelShutdownRequest, "", 0
		p.feedback = ""
	case panelKeyMatches(data, "tui.select.confirm") && p.mode == panelList:
		p.mode, p.scroll, p.renderedScroll = panelDetail, 0, 0
	case key == "v" && (p.mode == panelList || p.mode == panelDetail):
		p.previous, p.previousScroll = p.mode, p.scroll
		p.mode, p.scroll, p.renderedScroll = panelReport, 0, 0
	case key == "t" && (p.mode == panelList || p.mode == panelDetail):
		p.previous, p.previousScroll = p.mode, p.scroll
		p.mode, p.scroll, p.renderedScroll = panelTranscript, 0, 0
		p.teamRequest = p.team.Inspect(p.member)
	case (key == "m" || key == "p") && (p.mode == panelList || p.mode == panelDetail):
		member := frame.Members[index]
		if frame.RosterHealth.Status != "fresh" || member.Member.State != "running" || member.Execution == nil || member.Execution.Child.State != Running || member.Execution.Admission != "ready" {
			p.feedback = "Selected teammate is not active"
			return sdk.RemoteComponentResult{}
		}
		target := humanMember{Ref: member.Member.Ref, AdmissionID: member.Member.AdmissionID, Name: member.Member.Name}
		if key == "p" {
			p.submit(humanAction{kind: humanPause, member: &target})
			break
		}
		p.messageTarget = &target
		p.previous, p.previousScroll = p.mode, p.scroll
		p.mode, p.draft, p.cursor = panelMessage, "", 0
		p.feedback = ""
	}
	return sdk.RemoteComponentResult{}
}
