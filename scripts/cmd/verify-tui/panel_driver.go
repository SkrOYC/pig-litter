package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

const panelBudget = 2 * time.Second

type ownerReplacement struct {
	Action string          `json:"action"`
	Before observedProcess `json:"before"`
	After  observedProcess `json:"after"`
}

type panelEvent struct {
	Action     string `json:"action"`
	SentAt     string `json:"sentAt"`
	ReturnedAt string `json:"returnedAt"`
	Input      string `json:"input,omitempty"`
}

type panelTiming struct {
	Action       string  `json:"action"`
	SentAt       string  `json:"sentAt"`
	VisibleAt    string  `json:"visibleAt"`
	Milliseconds float64 `json:"milliseconds"`
	Width        int     `json:"width"`
	Height       int     `json:"height"`
}

type panelProof struct {
	processes                                                     func() ([]observedProcess, error)
	events                                                        []panelEvent
	replacements                                                  []ownerReplacement
	scenario                                                      string
	firstRow                                                      string
	ctx                                                           context.Context
	state                                                         *fixtureState
	sendText                                                      func(string) error
	sendKey                                                       func(string) error
	visible                                                       func() (string, error)
	frame                                                         func(string) (framePair, error)
	geometry                                                      func() (paneGeometry, error)
	resize                                                        func(int, int) error
	rootSessions, agentDir, evidence, rootPath, rootID, newRootID string
	configPath                                                    string
	workspace                                                     string
	idleCommand, idleDirectory, idleObserver                      string
	idleReadiness                                                 []idleBarrierEvidence
	paneTitle                                                     func() (string, error)
	rawActive                                                     func() error
	restartHost                                                   func(string) error
	lastEscape                                                    time.Time
	handback                                                      nativeChild
	handbackEntries                                               int
	timings                                                       []panelTiming
	checks                                                        []string
}

func (p *panelProof) action(label string, send func() error, ready func(string) bool) error {
	started := time.Now()
	if err := send(); err != nil {
		return err
	}
	var observed time.Time
	if err := waitFor(p.ctx, label, panelBudget, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		text, err := p.visible()
		if err != nil {
			return false, nil
		}
		if ready(text) {
			observed = time.Now()
			return true, nil
		}
		return false, nil
	}); err != nil {
		return err
	}
	geometry, err := p.geometry()
	if err != nil {
		return err
	}
	p.timings = append(p.timings, panelTiming{Action: label, SentAt: started.UTC().Format(time.RFC3339Nano), VisibleAt: observed.UTC().Format(time.RFC3339Nano), Milliseconds: float64(observed.Sub(started)) / float64(time.Millisecond), Width: geometry.width, Height: geometry.height})
	_, err = p.frame(label)
	return err
}

func (p *panelProof) key(label, key string, ready func(string) bool) error {
	return p.action(label, func() error { return p.sendKey(key) }, ready)
}

func (p *panelProof) ordinaryInputProof() error {
	if err := p.open("ordinary-open"); err != nil {
		return err
	}
	text, err := p.visible()
	if err != nil {
		return err
	}
	before := selectedPanelRow(text)
	if err := p.key("ordinary-down", "Down", func(text string) bool { return selectedPanelRow(text) != "" && selectedPanelRow(text) != before }); err != nil {
		return err
	}
	if err := p.key("ordinary-enter-detail", "Enter", hasPanelView("detail")); err != nil {
		return err
	}
	if err := p.key("ordinary-escape-list", "Escape", hasPanelView("list")); err != nil {
		return err
	}
	if err := p.selectChild("ordinary-target", "held-a"); err != nil {
		return err
	}
	if err := p.key("ordinary-cancel-composer", "m", hasPanelView("message")); err != nil {
		return err
	}
	if err := p.action("ordinary-cancel-text", func() error { return p.sendText("KITTY_CANCELLED_MARKER") }, func(text string) bool {
		return hasPanelView("message")(text) && strings.Contains(text, "KITTY_CANCELLED_MARKER")
	}); err != nil {
		return err
	}
	if err := p.key("ordinary-cancel-escape", "Escape", hasPanelView("list")); err != nil {
		return err
	}
	if err := p.key("ordinary-accepted-composer", "m", hasPanelView("message")); err != nil {
		return err
	}
	if err := p.action("ordinary-accepted-text", func() error { return p.sendText("KITTY_INPUT_MARKER") }, func(text string) bool {
		return hasPanelView("message")(text) && strings.Contains(text, "KITTY_INPUT_MARKER")
	}); err != nil {
		return err
	}
	if err := p.key("ordinary-accepted-enter", "Enter", func(text string) bool { return hasPanelView("list")(text) && strings.Contains(text, "Message queued") }); err != nil {
		return err
	}
	return p.close("ordinary-close")
}

func hasPanelView(view string) func(string) bool {
	return func(text string) bool { return panelView(text) == "Pig Litter | "+view }
}

func selectedPanelRow(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if row := strings.TrimSpace(line); strings.HasPrefix(row, "> ") {
			return row
		}
	}
	return ""
}

func panelView(text string) string {
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "Pig Litter | ") {
			return strings.SplitN(line, " (", 2)[0]
		}
	}
	return ""
}

func panelListBodies(text string) []string {
	var rows []string
	started := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "Pig Litter | list") {
			started = true
			continue
		}
		if !started {
			continue
		}
		row := strings.TrimSpace(line)
		if row == "" {
			break
		}
		rows = append(rows, strings.TrimPrefix(row, "> "))
	}
	return rows
}

func (p *panelProof) open(label string) error {
	if err := p.sendText("/pig-litter"); err != nil {
		return err
	}
	if err := p.key(label, "Enter", func(text string) bool {
		return hasPanelView("list")(text) && (selectedPanelRow(text) != "" || strings.Contains(text, "No retained direct children"))
	}); err != nil {
		return err
	}
	text, err := p.visible()
	if err != nil {
		return err
	}
	p.firstRow = strings.TrimPrefix(selectedPanelRow(text), "> ")
	return nil
}

func (p *panelProof) close(label string) error {
	for step := 0; step < 4; step++ {
		text, err := p.visible()
		if err != nil {
			return err
		}
		if !strings.Contains(text, "Pig Litter | ") {
			return nil
		}
		before := panelView(text)
		if err := p.key(fmt.Sprintf("%s-%d", label, step), "Escape", func(next string) bool { return panelView(next) != before }); err != nil {
			return err
		}
	}
	return fmt.Errorf("panel did not return to the native editor")
}

func (p *panelProof) selectChild(label, name string) error {
	root, err := p.root()
	if err != nil {
		return err
	}
	if p.firstRow == "" {
		return fmt.Errorf("panel has no direct child to select")
	}
	if err := p.key(label+"-home", "Home", func(text string) bool {
		return hasPanelView("list")(text) && strings.TrimPrefix(selectedPanelRow(text), "> ") == p.firstRow
	}); err != nil {
		return err
	}
	for step := 0; step <= len(root.Children); step++ {
		text, err := p.visible()
		if err != nil {
			return err
		}
		if strings.Contains(selectedPanelRow(text), name) {
			return nil
		}
		before := selectedPanelRow(text)
		if err := p.key(fmt.Sprintf("%s-down-%d", label, step), "Down", func(next string) bool { return selectedPanelRow(next) != "" && selectedPanelRow(next) != before }); err != nil {
			return err
		}
	}
	return fmt.Errorf("direct child %s is absent from the panel", name)
}

func (p *panelProof) root() (nativeSession, error) { return readNativeSession(p.rootPath) }

func (p *panelProof) checkpoint(label string) error {
	if err := saveNativeEvidence(filepath.Join(p.evidence, label+"-native"), p.rootSessions, filepath.Join(p.agentDir, "sessions")); err != nil {
		return err
	}
	p.checks = append(p.checks, label)
	return nil
}

func (p *panelProof) save() error {
	p.state.mu.Lock()
	data, err := json.MarshalIndent(map[string]any{"rootId": p.rootID, "newRootId": p.newRootID, "events": p.events, "ownerReplacements": p.replacements, "timings": p.timings, "checks": p.checks, "expectedSpawns": p.state.expectedSpawns, "spawnAdmissions": p.state.admissions, "admissionCallIds": p.state.admissionCalls, "childProviderRequests": p.state.childRequests, "messageDrainStage": p.state.messageDrain, "idleReadiness": p.idleReadiness}, "", "  ")
	p.state.mu.Unlock()
	if err != nil {
		return err
	}
	return writePrivate(filepath.Join(p.evidence, "panel-proof.json"), data)
}

func unchangedPassive(before, after nativeSession) error {
	if !reflect.DeepEqual(before.Raw, after.Raw) {
		return fmt.Errorf("native child metadata or receipt state changed across passive observation")
	}
	return nil
}

func (p *panelProof) waitNative(label string, ready func(nativeSession) bool) error {
	return waitFor(p.ctx, label, stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		view, err := p.root()
		if err != nil {
			return false, nil
		}
		return ready(view), nil
	})
}

func (p *panelProof) resume(label, name, instructions string) error {
	if err := p.open(label + "-open"); err != nil {
		return err
	}
	if err := p.selectChild(label+"-select", name); err != nil {
		return err
	}
	if err := p.key(label+"-composer", "r", hasPanelView("resume")); err != nil {
		return err
	}
	if err := p.sendText(instructions); err != nil {
		return err
	}
	return p.key(label+"-accepted", "Enter", func(text string) bool { return strings.Contains(text, "Resumed") && !hasPanelView("resume")(text) })
}

func (p *panelProof) preResize() error {
	root, err := rootNativeSession(p.rootSessions)
	if err != nil {
		return err
	}
	p.rootPath, p.rootID = root.Path, root.ID
	baseline, err := p.quiescentLogs()
	if err != nil {
		return err
	}
	root, err = p.root()
	if err != nil {
		return err
	}
	if err := p.verifyIdentities(root); err != nil {
		return err
	}
	p.handback = root.Children[p.state.quickReference().ID]
	for _, completion := range root.Completions {
		if completion.Receipt == p.handback.NoticeID && completion.ChildID == p.handback.ID {
			p.handbackEntries++
		}
	}
	if err := p.verifyHandback(root); err != nil {
		return err
	}
	requests := p.state.totalRequests()
	rootRequests := p.state.modelRequests("parent")
	if err := p.open("panel-passive-open"); err != nil {
		return err
	}
	text, err := p.visible()
	if err != nil {
		return err
	}
	directNames := panelListBodies(text)
	if len(directNames) < 3 {
		return fmt.Errorf("fixture needs three visible direct rows")
	}
	first, last := directNames[0], directNames[len(directNames)-1]
	if first == last {
		return fmt.Errorf("fixture needs distinct first and last retained children")
	}
	if err := p.key("panel-list-end", "End", func(text string) bool { return strings.TrimPrefix(selectedPanelRow(text), "> ") == last }); err != nil {
		return err
	}
	if err := p.key("panel-list-up", "Up", func(text string) bool {
		return strings.TrimPrefix(selectedPanelRow(text), "> ") == directNames[len(directNames)-2]
	}); err != nil {
		return err
	}
	if err := p.key("panel-list-down", "Down", func(text string) bool { return strings.Contains(selectedPanelRow(text), last) }); err != nil {
		return err
	}
	if err := p.key("panel-list-home", "Home", func(text string) bool { return strings.TrimPrefix(selectedPanelRow(text), "> ") == first }); err != nil {
		return err
	}
	if err := p.verifyPanelSet(root); err != nil {
		return err
	}
	if err := p.selectChild("panel-passive-select", "held-a"); err != nil {
		return err
	}
	var inspected nativeChild
	for _, child := range root.Children {
		if child.Name == "held-a" {
			inspected = child
		}
	}
	if inspected.ID == "" {
		return fmt.Errorf("native held-a identity missing")
	}
	if err := p.key("panel-passive-detail", "Enter", func(text string) bool {
		return hasPanelView("detail")(text) && strings.Contains(text, "UUID "+inspected.ID) && strings.Contains(text, "Model "+inspected.Model) && strings.Contains(text, "Cwd "+inspected.Cwd) && strings.Contains(text, "Tool starts 1") && strings.Contains(text, "Active tools none") && !strings.Contains(text, "Elapsed unknown")
	}); err != nil {
		return err
	}
	if err := p.key("panel-passive-result", "v", hasPanelView("latest result")); err != nil {
		return err
	}
	if err := p.key("panel-result-back", "Escape", hasPanelView("detail")); err != nil {
		return err
	}
	if err := p.key("panel-empty-message", "m", hasPanelView("message")); err != nil {
		return err
	}
	if err := p.emptyInput("panel-empty-message", "message"); err != nil {
		return err
	}
	if err := p.sendText("CANCELLED_UI_DRAFT"); err != nil {
		return err
	}
	if err := p.close("panel-cancel-draft"); err != nil {
		return err
	}
	after, err := p.root()
	if err != nil {
		return err
	}
	if err := unchangedPassive(root, after); err != nil {
		return err
	}
	if err := p.logsUnchanged(baseline); err != nil {
		return err
	}
	if p.state.totalRequests() != requests {
		return fmt.Errorf("passive view or cancelled draft started inference")
	}
	p.checks = append(p.checks, "passive-and-cancelled-draft-inert")
	if err := p.open("panel-pause-open"); err != nil {
		return err
	}
	if err := p.selectChild("panel-pause-select", "held-a"); err != nil {
		return err
	}
	if err := p.key("panel-pause-subtree", "p", func(text string) bool { return strings.Contains(text, "Paused subtree") }); err != nil {
		return err
	}
	refs := p.state.childReferences()
	var firstID, secondID string
	for _, ref := range refs {
		if ref.Name == "held-a" {
			firstID = ref.ID
		}
		if ref.Name == "held-b" {
			secondID = ref.ID
		}
	}
	if err := p.waitNative("selected child paused while sibling stays live", func(view nativeSession) bool {
		return view.Children[firstID].Paused && !view.Children[secondID].Paused && p.state.holding("held-b") && !p.state.holding("held-a")
	}); err != nil {
		return err
	}
	if err := p.key("panel-empty-resume", "r", hasPanelView("resume")); err != nil {
		return err
	}
	beforeResumeRequests := p.state.modelRequests("held")
	resumeBaseline, err := p.quiescentLogs()
	if err != nil {
		return err
	}
	if err := p.emptyInput("panel-empty-resume", "resume"); err != nil {
		return err
	}
	if err := p.sendText("CANCELLED_RESUME_DRAFT"); err != nil {
		return err
	}
	if err := p.close("panel-cancel-resume"); err != nil {
		return err
	}
	if p.state.modelRequests("held") != beforeResumeRequests {
		return fmt.Errorf("empty or cancelled resume started a child")
	}
	if err := p.logsUnchanged(resumeBaseline); err != nil {
		return err
	}
	if err := p.resume("human-first-resume", "held-a", "HUMAN_RESUME_MARKER"); err != nil {
		return err
	}
	if err := p.close("human-first-resume-close"); err != nil {
		return err
	}
	if err := p.waitNative("accepted child outlives panel closure", func(view nativeSession) bool {
		return !view.Children[firstID].Paused && view.Children[firstID].State == "running" && p.state.holding("held-a", "held-b")
	}); err != nil {
		return err
	}
	if p.state.modelRequests("parent") != rootRequests {
		return fmt.Errorf("human pause or resume prompted the root")
	}
	return p.checkpoint("human-pause-same-uuid-resume")
}

var reportLinePattern = regexp.MustCompile(`REPORT_LINE_[0-9]{2}`)

func (p *panelProof) message(label, mode, text string) error {
	if err := p.open(label + "-open"); err != nil {
		return err
	}
	if err := p.selectChild(label+"-select", "held-a"); err != nil {
		return err
	}
	if err := p.key(label+"-composer", "m", func(screen string) bool {
		return hasPanelView("message")(screen) && strings.Contains(screen, "Message mode steer")
	}); err != nil {
		return err
	}
	if mode == "follow_up" {
		if err := p.key(label+"-mode", "Tab", func(screen string) bool { return strings.Contains(screen, "Message mode follow_up") }); err != nil {
			return err
		}
	}
	if err := p.sendText(text); err != nil {
		return err
	}
	if err := p.key(label+"-queued", "Enter", func(screen string) bool {
		return strings.Contains(screen, "Message queued") && !hasPanelView("message")(screen)
	}); err != nil {
		return err
	}
	return p.close(label + "-close")
}

func nativeInputCount(session nativeSession, marker string) int {
	count := 0
	for _, raw := range session.Messages {
		var message openAIMessage
		if json.Unmarshal(raw, &message) == nil && message.Role == "user" && strings.Contains(contentText(message.Content), marker) {
			count++
		}
	}
	return count
}

func (p *panelProof) childNative(id string) (nativeSession, error) {
	sessions, err := readNativeSessions(filepath.Join(p.agentDir, "sessions"))
	if err != nil {
		return nativeSession{}, err
	}
	for _, session := range sessions {
		if session.ID == id {
			return session, nil
		}
	}
	return nativeSession{}, fmt.Errorf("native child session %s absent", id)
}

func (p *panelProof) passivePaused(label string, before nativeSession, restored bool) error {
	baseline, err := p.quiescentLogs()
	if err != nil {
		return err
	}
	start, err := p.root()
	if err != nil {
		return err
	}
	if before.ID != start.ID || !reflect.DeepEqual(before.Children, start.Children) || !reflect.DeepEqual(before.Completions, start.Completions) {
		return fmt.Errorf("owner replacement changed retained metadata or completion receipts")
	}
	requests := p.state.totalRequests()
	if err := p.open(label + "-open"); err != nil {
		return err
	}
	if err := p.selectChild(label+"-select", "quick-newer"); err != nil {
		return err
	}
	if err := p.key(label+"-detail", "Enter", func(text string) bool {
		return hasPanelView("detail")(text) && (!restored || (strings.Contains(text, "Elapsed unknown") && strings.Contains(text, "Tool starts unknown") && strings.Contains(text, "Active tools unknown")))
	}); err != nil {
		return err
	}
	if err := p.key(label+"-result", "v", hasPanelView("latest result")); err != nil {
		return err
	}
	if err := p.close(label + "-close"); err != nil {
		return err
	}
	after, err := p.root()
	if err != nil {
		return err
	}
	if err := unchangedPassive(start, after); err != nil {
		return err
	}
	if err := p.logsUnchanged(baseline); err != nil {
		return err
	}
	if p.state.totalRequests() != requests {
		return fmt.Errorf("passive paused panel started inference")
	}
	return p.checkpoint(label)
}

func (p *panelProof) afterHistory() error {
	if err := p.resume("human-message-resume", "held-a", "HUMAN_MESSAGE_RUN"); err != nil {
		return err
	}
	if err := p.close("human-message-resume-close"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "resumed message target stream", stageTimeout, func() (bool, error) { return p.state.holding("held-a"), p.state.providerFailure() }); err != nil {
		return err
	}
	if err := p.ordinaryInputProof(); err != nil {
		return err
	}
	if err := p.message("human-steer", "steer", "STEER_MARKER"); err != nil {
		return err
	}
	if err := p.message("human-follow-up", "follow_up", "FOLLOWUP_MARKER"); err != nil {
		return err
	}
	if err := p.state.releaseHold("held-a"); err != nil {
		return err
	}
	var heldID string
	for _, ref := range p.state.childReferences() {
		if ref.Name == "held-a" {
			heldID = ref.ID
		}
	}
	if err := p.waitNative("real native message continuations completed", func(root nativeSession) bool {
		child := root.Children[heldID]
		return child.Latest != nil && strings.HasPrefix(child.Latest.Text, "MESSAGES_HANDLED") && child.Latest.State == "completed"
	}); err != nil {
		return err
	}
	child, err := p.childNative(heldID)
	if err != nil {
		return err
	}
	for _, raw := range child.Messages {
		var message openAIMessage
		if err := json.Unmarshal(raw, &message); err != nil {
			return err
		}
		if message.Role == "user" && strings.TrimSpace(contentText(message.Content)) == "" {
			return fmt.Errorf("empty human input reached native child history")
		}
	}
	if nativeInputCount(child, "STEER_MARKER") != 1 || nativeInputCount(child, "FOLLOWUP_MARKER") != 1 || nativeInputCount(child, "KITTY_INPUT_MARKER") != 1 || nativeInputCount(child, "KITTY_CANCELLED_MARKER") != 0 || nativeInputCount(child, "CANCELLED_UI_DRAFT") != 0 || nativeInputCount(child, "CANCELLED_RESUME_DRAFT") != 0 {
		return fmt.Errorf("native messages did not contain exactly the accepted steer/follow-up input")
	}
	if err := p.checkpoint("native-steer-follow-up-once"); err != nil {
		return err
	}
	if err := p.resume("human-live-result-resume", "quick-newer", "LIVE_QUICK_RUN"); err != nil {
		return err
	}
	if err := p.close("human-live-result-resume-close"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "resumed quick child held", stageTimeout, func() (bool, error) { return p.state.holding("quick-live"), p.state.providerFailure() }); err != nil {
		return err
	}
	if err := p.sendText("RECEIPT_WIDGET"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "genuine root stream blocked", stageTimeout, func() (bool, error) { return p.state.holding("root-receipt"), p.state.providerFailure() }); err != nil {
		return err
	}
	if err := p.open("live-result-open"); err != nil {
		return err
	}
	if err := p.selectChild("live-result-select", "quick-newer"); err != nil {
		return err
	}
	if err := p.key("live-result-view", "v", hasPanelView("latest result")); err != nil {
		return err
	}
	if err := p.key("live-result-end", "End", func(text string) bool {
		return hasPanelView("latest result")(text) && strings.Contains(text, "REPORT_LINE_70") && strings.Contains(text, "[display rows clipped]") && !strings.Contains(text, "OSC_PAYLOAD_SHOULD_NOT_PAINT")
	}); err != nil {
		return err
	}
	if err := p.key("live-result-home", "Home", func(text string) bool {
		return hasPanelView("latest result")(text) && reportLinePattern.FindString(text) == "REPORT_LINE_00"
	}); err != nil {
		return err
	}
	before, err := p.visible()
	if err != nil {
		return err
	}
	beforeLine := reportLinePattern.FindString(before)
	if err := p.key("live-result-scroll", "]", func(text string) bool {
		return hasPanelView("latest result")(text) && reportLinePattern.FindString(text) != "" && reportLinePattern.FindString(text) != beforeLine
	}); err != nil {
		return err
	}
	scrolled, err := p.visible()
	if err != nil {
		return err
	}
	scrollLine := reportLinePattern.FindString(scrolled)
	if err := p.action("live-completion-refresh", func() error { return p.state.releaseHold("quick-live") }, func(text string) bool {
		return hasPanelView("latest result")(text) && strings.Contains(text, "LIVE_REPORT_DONE") && reportLinePattern.FindString(text) == scrollLine
	}); err != nil {
		return err
	}
	if err := p.close("live-result-close"); err != nil {
		return err
	}
	quickID := p.state.quickReference().ID
	var quickReceipt string
	if err := p.waitNative("genuine canonical outstanding completion receipt", func(root nativeSession) bool {
		var err error
		quickReceipt, err = pendingReceipt(root.Children[quickID])
		return err == nil
	}); err != nil {
		return err
	}
	if err := p.checkpoint("busy-root-outstanding-receipt"); err != nil {
		return err
	}
	rootRequestsBeforeEscape := p.state.modelRequests("parent")
	if err := p.rootEscape(); err != nil {
		return err
	}
	if err := p.waitAfterEscape("actual root Escape paused all direct children", func(root nativeSession) bool {
		visible, err := p.visible()
		if err != nil || strings.Contains(visible, "Working") {
			return false
		}
		if p.state.holding("root-receipt") {
			return false
		}
		for _, child := range root.Children {
			if child.ParentID == root.ID && !child.Paused {
				return false
			}
		}
		return len(root.Children[quickID].Notices) > 0
	}); err != nil {
		return err
	}
	paused, err := p.root()
	if err != nil {
		return err
	}
	if err := checkReservation(paused.Children[quickID], quickReceipt, true); err != nil {
		return err
	}
	if p.scenario == "receipt-control" {
		return p.noPanelControl(rootRequestsBeforeEscape, paused)
	}
	if p.state.modelRequests("parent") != rootRequestsBeforeEscape {
		return fmt.Errorf("root restarted inference after Escape before panel observation")
	}
	if err := p.passivePaused("root-escape-passive", paused, false); err != nil {
		return err
	}
	oldProcess, err := p.extensionProcess()
	if err != nil {
		return err
	}
	if err := p.sendText("/reload"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "released host reload completed", stageTimeout, func() (bool, error) {
		text, err := p.visible()
		return err == nil && strings.Contains(text, "Reloaded keybindings, extensions"), nil
	}); err != nil {
		return err
	}
	if err := p.waitReplacement("released-owner-reload", oldProcess); err != nil {
		return err
	}
	if err := p.passivePaused("fresh-owner-passive-receipts", paused, true); err != nil {
		return err
	}
	if err := p.geometryProof(); err != nil {
		return err
	}
	if err := p.deferredReceiptProof(quickID, quickReceipt); err != nil {
		return err
	}
	if err := p.fullRestartProof(quickID); err != nil {
		return err
	}
	if err := p.chainProof(); err != nil {
		return err
	}
	if err := p.newRootProof(); err != nil {
		return err
	}
	return p.hostileConfigProof()
}

func (p *panelProof) geometryProof() error {
	if err := p.open("geometry-panel-open"); err != nil {
		return err
	}
	for _, size := range []paneGeometry{{80, 24}, {32, 24}, {100, 32}} {
		label := fmt.Sprintf("panel-%dx%d", size.width, size.height)
		if err := p.action(label, func() error { return p.resize(size.width, size.height) }, func(text string) bool {
			geometry, err := p.geometry()
			return err == nil && geometry == size && panelListHintsReady(text, size.width, size.height)
		}); err != nil {
			return err
		}
		saved, err := os.ReadFile(filepath.Join(p.evidence, label+"-visible.txt"))
		if err != nil {
			return err
		}
		if !panelListHintsReady(string(saved), size.width, size.height) {
			return fmt.Errorf("saved %s frame lacks the complete controls for its width", label)
		}
	}
	return p.close("geometry-panel-close")
}

func panelListHintsReady(frame string, width, height int) bool {
	if !frameHasGeometry(frame, width, height) || !hasPanelView("list")(frame) {
		return false
	}
	lines := strings.Split(strings.TrimSuffix(frame, "\n"), "\n")
	rows := 2
	want := "Up/Down move Enter inspect v result m message p pause subtree r resume [ ] page Home/End first/last Esc back/close"
	if width == 32 {
		rows = 4
		want = "↑↓ move Enter detail v result\nm msg p pause subtree r resume\n[ ] page Home/End first/last\nEsc back/close"
	}
	start := len(lines) - 2 - rows
	if start <= 0 {
		return false
	}
	header := -1
	for index, line := range lines[:start] {
		if strings.HasPrefix(strings.TrimSpace(line), "Pig Litter | list (") {
			header = index
			break
		}
	}
	if header < 0 {
		return false
	}
	hints := make([]string, rows)
	for index, line := range lines[start : len(lines)-2] {
		hints[index] = strings.TrimSpace(line)
	}
	if width == 32 {
		return strings.Join(hints, "\n") == want
	}
	return strings.Join(strings.Fields(strings.Join(hints, " ")), " ") == want
}

func (p *panelProof) deferredReceiptProof(childID, receipt string) error {
	if receipt == "" {
		return fmt.Errorf("natural outstanding completion has no native retained receipt")
	}
	for _, prompt := range []string{"ROOT_NOTICE_FIRST", "ROOT_NOTICE_SECOND"} {
		if err := p.awaitRootIdle(prompt); err != nil {
			return err
		}
		requests := p.state.modelRequests("parent")
		if err := p.sendText(prompt); err != nil {
			return err
		}
		if err := p.sendKey("Enter"); err != nil {
			return err
		}
		if err := p.waitNative(prompt+" received one genuine native notice", func(root nativeSession) bool {
			count := 0
			for _, completion := range root.Completions {
				if completion.ChildID == childID && completion.Receipt == receipt {
					if completionMatches(completion, root.Children[childID], receipt) != nil {
						return false
					}
					count++
				}
			}
			text, err := p.visible()
			return err == nil && count == 1 && strings.Contains(text, prompt+"_DONE") && !strings.Contains(text, "Working")
		}); err != nil {
			return err
		}
		if p.state.modelRequests("parent") != requests+1 {
			return fmt.Errorf("explicit root prompt caused extra inference")
		}
		root, err := p.root()
		if err != nil {
			return err
		}
		if err := p.verifyHandback(root); err != nil {
			return err
		}
		if err := p.checkpoint(strings.ToLower(prompt) + "-native-entry-once"); err != nil {
			return err
		}
	}
	return nil
}

func (p *panelProof) chainProof() error {
	if err := p.sendText("CHAIN_WIDGET"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "worker sibling and grandchild native streams", stageTimeout, func() (bool, error) {
		return p.state.holding("chain-worker", "chain-sibling", "chain-grandchild"), p.state.providerFailure()
	}); err != nil {
		return err
	}
	if p.rootPath == "" {
		root, err := rootNativeSession(p.rootSessions)
		if err != nil {
			return err
		}
		p.rootPath, p.rootID = root.Path, root.ID
	}
	rootRequests := p.state.modelRequests("parent")
	if err := p.rootEscape(); err != nil {
		return err
	}
	p.state.mu.Lock()
	worker, sibling, grand := p.state.chainWorker, p.state.chainSibling, p.state.chainGrandchild
	p.state.mu.Unlock()
	if err := p.waitAfterEscape("root Escape paused the worker sibling and grandchild", func(root nativeSession) bool {
		return root.Children[worker.ID].Paused && root.Children[sibling.ID].Paused && root.Children[grand.ID].Paused && !p.state.holding("chain-worker") && !p.state.holding("chain-sibling") && !p.state.holding("chain-grandchild")
	}); err != nil {
		return err
	}
	grandRequests, siblingRequests := p.state.modelRequests("chain_grand"), p.state.modelRequests("chain_sibling")
	if err := p.open("chain-direct-list-open"); err != nil {
		return err
	}
	if err := p.selectChild("chain-worker-select", worker.Name); err != nil {
		return err
	}
	text, err := p.visible()
	if err != nil {
		return err
	}
	root, err := p.root()
	if err != nil {
		return err
	}
	if root.Children[grand.ID].ParentID != worker.ID {
		return fmt.Errorf("grandchild does not belong to selected worker")
	}
	if err := p.verifyIdentities(root); err != nil {
		return err
	}
	if err := p.verifyPanelSet(root); err != nil {
		return err
	}
	if strings.Contains(text, "chain-grandchild") {
		return fmt.Errorf("root panel disclosed a grandchild row")
	}
	if err := p.close("chain-direct-list-close"); err != nil {
		return err
	}
	if err := p.resume("human-chain-worker-resume", worker.Name, "HUMAN_CHAIN_RESUME"); err != nil {
		return err
	}
	if err := p.close("human-chain-worker-resume-close"); err != nil {
		return err
	}
	if err := p.waitNative("only selected worker resumed", func(root nativeSession) bool {
		return !root.Children[worker.ID].Paused && root.Children[sibling.ID].Paused && root.Children[grand.ID].Paused && p.state.holding("chain-worker-resumed")
	}); err != nil {
		return err
	}
	if p.state.modelRequests("chain_grand") != grandRequests || p.state.modelRequests("chain_sibling") != siblingRequests || p.state.modelRequests("parent") != rootRequests {
		return fmt.Errorf("human worker resume restarted an unselected session")
	}
	if err := p.checkpoint("selected-worker-only-grandchild-still-paused"); err != nil {
		return err
	}
	if err := p.state.releaseHold("chain-worker-resumed"); err != nil {
		return err
	}
	if err := p.waitNative("current worker explicitly resumed its own grandchild", func(root nativeSession) bool {
		w, g := root.Children[worker.ID], root.Children[grand.ID]
		return w.Latest != nil && w.Latest.Text == "WORKER_CHAIN_DONE" && g.Latest != nil && g.Latest.Text == "GRAND_RESUMED_REPORT" && root.Children[sibling.ID].Paused
	}); err != nil {
		return err
	}
	if p.state.modelRequests("parent") != rootRequests || p.state.modelRequests("chain_sibling") != siblingRequests {
		return fmt.Errorf("nested parent-owned resume restarted root or sibling")
	}
	child, err := p.childNative(grand.ID)
	if err != nil {
		return err
	}
	if nativeInputCount(child, "NESTED_RESUME_MARKER") != 1 {
		return fmt.Errorf("grandchild native input did not retain exactly one explicit continuation")
	}
	return p.checkpoint("parent-owned-grandchild-resume-under-paused-root")
}

func (p *panelProof) newRootProof() error {
	requests := p.state.totalRequests()
	oldProcess, err := p.extensionProcess()
	if err != nil {
		return err
	}
	if err := p.sendText("/new"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "released host created a new root", stageTimeout, func() (bool, error) {
		text, err := p.visible()
		return err == nil && strings.Contains(text, "New session started"), nil
	}); err != nil {
		return err
	}
	if err := p.waitReplacement("released-owner-new-root", oldProcess); err != nil {
		return err
	}
	if err := p.open("separate-root-empty-panel"); err != nil {
		return err
	}
	text, err := p.visible()
	if err != nil {
		return err
	}
	if !strings.Contains(text, "No retained direct children") || strings.Contains(text, "chain-worker") || strings.Contains(text, "held-a") {
		return fmt.Errorf("new root panel retained another root's children")
	}
	if err := p.close("separate-root-empty-panel-close"); err != nil {
		return err
	}
	if err := p.sendText("/session"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	identity := regexp.MustCompile(`ID:\s*([0-9a-f]{8}-[0-9a-f-]{27})`)
	if err := waitFor(p.ctx, "new root public session identity", stageTimeout, func() (bool, error) {
		captured, err := p.frame("new-root-native-session-info")
		if err != nil {
			return false, nil
		}
		match := identity.FindStringSubmatch(captured.scrollback + "\n" + captured.visible)
		if len(match) != 2 {
			return false, nil
		}
		p.newRootID = match[1]
		return p.newRootID != p.rootID, nil
	}); err != nil {
		return err
	}
	if p.state.totalRequests() != requests {
		return fmt.Errorf("root replacement or empty panel started inference")
	}
	p.checks = append(p.checks, "new-root-identity-observed-through-public-session-command")
	if err := p.sendText("NEW_ROOT_HEADER"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "new root genuine native header", stageTimeout, func() (bool, error) {
		sessions, err := readNativeSessions(p.rootSessions)
		if err != nil {
			return false, err
		}
		for _, native := range sessions {
			if native.ID == p.newRootID {
				return native.Cwd == p.workspace, nil
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "new root explicit header prompt settled", stageTimeout, func() (bool, error) {
		text, err := p.visible()
		return err == nil && strings.Contains(text, "NEW_ROOT_HEADER_DONE") && !strings.Contains(text, "Working"), p.state.providerFailure()
	}); err != nil {
		return err
	}
	if p.state.totalRequests() != requests+1 {
		return fmt.Errorf("new root header prompt caused extra inference")
	}
	return p.checkpoint("new-root-passive-isolation-then-explicit-native-header")
}

func (p *panelProof) hostileConfigProof() (result error) {
	if err := p.rawActive(); err != nil {
		return err
	}
	titleBefore, err := p.paneTitle()
	if err != nil {
		return err
	}
	original, err := os.ReadFile(p.configPath)
	if err != nil {
		return err
	}
	defer func() {
		if err := writePrivate(p.configPath, original); result == nil {
			result = err
		}
	}()
	const hostile = "concurrency: 6\ndepth: 2\n\"HOSTILE_CONFIG_BEGIN\\u001b]0;CONFIG_OSC_SHOULD_NOT_PAINT\\u0007\\u001b[2J\\u202eHOSTILE_CONFIG_END\": 1\n"
	if err := writePrivate(p.configPath, []byte(hostile)); err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(p.evidence, "hostile-litter.yaml"), []byte(hostile)); err != nil {
		return err
	}
	requests := p.state.totalRequests()
	if err := p.sendText("/reload"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "hostile config owner reload", stageTimeout, func() (bool, error) {
		text, err := p.visible()
		return err == nil && strings.Contains(text, "Reloaded keybindings, extensions"), nil
	}); err != nil {
		return err
	}
	if err := p.sendText("/pig-litter"); err != nil {
		return err
	}
	before, err := p.visible()
	if err != nil {
		return err
	}
	previousError := latestLitterCommandError(before)
	if err := p.key("hostile-config-command-error", "Enter", func(text string) bool {
		return hostileConfigErrorReady(text, previousError)
	}); err != nil {
		return err
	}
	frame, err := p.frame("hostile-config-command-error-transport")
	if err != nil {
		return err
	}
	if !hostileConfigErrorReady(frame.visible, previousError) {
		return fmt.Errorf("saved hostile config frame lacks the fresh sanitized command error")
	}
	if strings.Contains(frame.ansi, "CONFIG_OSC_SHOULD_NOT_PAINT") || strings.ContainsRune(frame.ansi, '\u202e') {
		return fmt.Errorf("unsafe config key reached native terminal paint")
	}
	titleAfter, err := p.paneTitle()
	if err != nil {
		return err
	}
	if titleBefore != titleAfter {
		return fmt.Errorf("hostile config changed pane title from %q to %q", titleBefore, titleAfter)
	}
	titles, err := json.Marshal(map[string]string{"before": titleBefore, "after": titleAfter})
	if err != nil {
		return err
	}
	if err := writePrivate(filepath.Join(p.evidence, "hostile-pane-titles.json"), titles); err != nil {
		return err
	}
	if err := writePrivate(p.configPath, original); err != nil {
		return err
	}
	if err := p.open("valid-config-retry-empty-panel"); err != nil {
		return err
	}
	if err := p.close("valid-config-retry-close"); err != nil {
		return err
	}
	if err := p.rawActive(); err != nil {
		return err
	}
	if p.state.totalRequests() != requests {
		return fmt.Errorf("config error or retry started inference")
	}
	return p.checkpoint("native-command-error-sanitized-and-retry")
}

func latestLitterCommandError(frame string) string {
	lines := strings.Split(frame, "\n")
	start := -1
	for index, line := range lines {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, `Extension "`) && strings.Contains(line, `" error:`) {
			start = index
		}
	}
	if start < 0 {
		return ""
	}
	const heading = `Extension "command:pig-litter" error:`
	line := strings.TrimSpace(lines[start])
	if line != heading && !strings.HasPrefix(line, heading+" ") {
		return ""
	}
	end := start + 1
	for end < len(lines) && strings.TrimSpace(lines[end]) != "" {
		end++
	}
	return strings.Join(strings.Fields(strings.Join(lines[start:end], " ")), " ")
}

func hostileConfigErrorReady(frame, previous string) bool {
	if panelView(frame) != "" || strings.Contains(frame, "CONFIG_OSC_SHOULD_NOT_PAINT") || strings.ContainsRune(frame, '\u202e') || strings.ContainsRune(frame, '\x1b') {
		return false
	}
	region := latestLitterCommandError(frame)
	return region != "" && region != previous && strings.HasSuffix(region, "litter.yaml has unknown setting HOSTILE_CONFIG_BEGINHOSTILE_CONFIG_END")
}

func (p *panelProof) noPanelControl(rootRequests int, before nativeSession) error {
	baseline, err := p.quiescentLogs()
	if err != nil {
		return err
	}
	if err := p.checkpoint("no-panel-control-after-escape"); err != nil {
		return err
	}
	timer := time.NewTimer(panelBudget)
	defer timer.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		if p.state.modelRequests("parent") != rootRequests {
			return fmt.Errorf("no-panel control observed root inference after Escape")
		}
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-tick.C:
		case <-timer.C:
			after, err := p.root()
			if err != nil {
				return err
			}
			if err := unchangedPassive(before, after); err != nil {
				return err
			}
			if err := p.logsUnchanged(baseline); err != nil {
				return err
			}
			return p.checkpoint("no-panel-control-quiet-two-seconds")
		}
	}
}

func (p *panelProof) rootEscape() error {
	p.lastEscape = time.Now()
	started := time.Now().UTC().Format(time.RFC3339Nano)
	err := p.sendKey("Escape")
	p.events = append(p.events, panelEvent{Action: "actual-root-Escape", SentAt: started, ReturnedAt: time.Now().UTC().Format(time.RFC3339Nano)})
	return err
}

func (p *panelProof) extensionProcess() (observedProcess, error) {
	processes, err := p.processes()
	if err != nil {
		return observedProcess{}, err
	}
	var extension []observedProcess
	for _, process := range processes {
		if filepath.Base(strings.TrimSuffix(process.Executable, " (deleted)")) == selectedExtensionName {
			extension = append(extension, process)
		}
	}
	if len(extension) != 1 {
		return observedProcess{}, fmt.Errorf("expected one selected Resource process, found %d", len(extension))
	}
	return extension[0], nil
}

func (p *panelProof) waitReplacement(action string, before observedProcess) error {
	var after observedProcess
	if err := waitFor(p.ctx, action, stageTimeout, func() (bool, error) {
		current, err := p.extensionProcess()
		if err != nil {
			return false, nil
		}
		if current.PID == before.PID && current.StartTicks == before.StartTicks {
			return false, nil
		}
		after = current
		return !observedAlive(before), nil
	}); err != nil {
		return err
	}
	p.replacements = append(p.replacements, ownerReplacement{Action: action, Before: before, After: after})
	return nil
}
