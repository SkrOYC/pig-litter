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
	panelTranscript
	panelTasks
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
	invalidateWG       sync.WaitGroup
	stop, joined       chan struct{}
	dispose            sync.Once
	closed             bool
	reportRun          *run
	reportOutcome      Outcome
	reportPresent      bool
	reportState        State
	reportWidth        int
	reportLines        []string
	team               *teamView
	member             MemberRef
	memberSelected     bool
	teamRequest        uint64
	messageTarget      *humanMember
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
			if p.mode == panelMessage || p.mode == panelResume {
				p.mode = p.previous
				p.draft, p.cursor = "", 0
				p.messageTarget = nil
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
	if p.team != nil {
		return p.renderTeam(width)
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
	if p.team != nil {
		return p.handleTeamInput(data, key), nil
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
		p.composerInput(data)
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

func (p *childPanel) composerInput(data string) {
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
}

func (p *childPanel) submit(action humanAction) {
	selected := p.selected
	if p.team != nil {
		if p.messageTarget == nil {
			p.feedback = "Selected teammate is unavailable"
			return
		}
		member := *p.messageTarget
		action.member = &member
		var err error
		selected, err = p.owner.teamMessageExecution(member)
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

func (p *childPanel) renderTeam(width int) []string {
	frame := p.team.Snapshot()
	index := p.teamSelection(frame)
	height := p.height()
	if height <= 0 {
		height = 24
	}
	budget := max(1, height-8)
	legend := []string{"Up/Down move  Enter detail  v report  t transcript", "m message active teammate  k all tasks  [ ] page  Home/End  Esc close"}
	if p.mode == panelMessage {
		legend = []string{"Enter submit  Esc cancel", "Left/Right edit  Ctrl+U clear  Tab steer/follow_up"}
	} else if p.mode != panelList && p.mode != panelDetail {
		legend = []string{"Up/Down scroll  [ ] page  Home/End  Esc back"}
	} else if p.mode == panelDetail {
		legend = []string{"Up/Down scroll  v report  t transcript", "m message active teammate  k all tasks  Esc back"}
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
		case panelTasks:
			title += " | tasks"
			content = teamTaskLines(frame, nil)
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
		}{{"Roster", frame.RosterHealth}, {"Tasks", frame.TaskHealth}, {"Selection", frame.PreferenceHealth}} {
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
		assigned := "unassigned"
		if task.Owner != nil {
			assigned = string(task.Owner.ID) + fmt.Sprintf(" generation %d", task.Owner.Generation)
			if i := slices.IndexFunc(frame.Members, func(member teamMemberView) bool { return member.Member.Ref == *task.Owner }); i >= 0 {
				assigned = frame.Members[i].Member.Name + " | " + assigned
			}
		}
		line := fmt.Sprintf("#%d %s | %s | %s", task.ID.Number, task.Subject, task.State, assigned)
		if task.Blocked {
			line += " | blocked"
		}
		lines = append(lines, line)
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
	}
	if len(lines) == 0 {
		return []string{"No tasks"}
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
	if tui.MatchesKeyID(data, "escape") {
		switch p.mode {
		case panelList:
			p.cancel()
			return sdk.RemoteComponentResult{Done: true}
		case panelReport, panelTranscript:
			p.mode, p.scroll = p.previous, p.previousScroll
		case panelMessage:
			if p.pendingCancel != nil {
				p.pendingCancel()
			}
			p.mode, p.scroll = p.previous, p.previousScroll
			p.draft, p.cursor, p.messageTarget = "", 0, nil
		default:
			p.mode, p.scroll = panelList, 0
		}
		return sdk.RemoteComponentResult{}
	}
	if p.closed || p.pending != nil {
		return sdk.RemoteComponentResult{}
	}
	if p.mode == panelMessage {
		p.composerInput(data)
		return sdk.RemoteComponentResult{}
	}
	frame := p.team.Snapshot()
	index := p.teamSelection(frame)
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
		} else {
			p.scroll = max(0, p.renderedScroll+step)
		}
		return sdk.RemoteComponentResult{}
	}
	if key == "k" && (p.mode == panelList || p.mode == panelDetail) {
		p.mode, p.scroll, p.renderedScroll = panelTasks, 0, 0
		return sdk.RemoteComponentResult{}
	}
	if index < 0 {
		return sdk.RemoteComponentResult{}
	}
	switch {
	case tui.MatchesKeyID(data, "enter") && p.mode == panelList:
		p.mode, p.scroll, p.renderedScroll = panelDetail, 0, 0
	case key == "v" && (p.mode == panelList || p.mode == panelDetail):
		p.previous, p.previousScroll = p.mode, p.scroll
		p.mode, p.scroll, p.renderedScroll = panelReport, 0, 0
	case key == "t" && (p.mode == panelList || p.mode == panelDetail):
		p.previous, p.previousScroll = p.mode, p.scroll
		p.mode, p.scroll, p.renderedScroll = panelTranscript, 0, 0
		p.teamRequest = p.team.Inspect(p.member)
	case key == "m" && (p.mode == panelList || p.mode == panelDetail):
		member := frame.Members[index]
		if frame.RosterHealth.Status != "fresh" || member.Member.State != "running" || member.Execution == nil || member.Execution.Child.State != Running || member.Execution.Admission != "ready" {
			p.feedback = "Selected teammate is not active"
			return sdk.RemoteComponentResult{}
		}
		p.messageTarget = &humanMember{Ref: member.Member.Ref, AdmissionID: member.Member.AdmissionID, Name: member.Member.Name}
		p.previous, p.previousScroll = p.mode, p.scroll
		p.mode, p.draft, p.cursor = panelMessage, "", 0
		p.feedback = ""
	}
	return sdk.RemoteComponentResult{}
}
