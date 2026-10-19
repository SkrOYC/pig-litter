package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
)

func (p *panelProof) coldRootProof() error {
	if p.state.totalRequests() != 0 {
		return fmt.Errorf("cold context proof already has provider requests")
	}
	if err := p.sendText("COLD_ROOT_HEADER"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	var root nativeSession
	if err := waitFor(p.ctx, "cold context-only root and empty widget", stageTimeout, func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		view, err := rootNativeSession(p.rootSessions)
		if err != nil {
			return false, nil
		}
		text, err := p.visible()
		if err != nil {
			return false, nil
		}
		if !strings.Contains(text, "COLD_ROOT_HEADER_DONE") || !strings.Contains(text, "Litter 0 children") || !strings.Contains(text, "/pig-litter") || strings.Contains(text, "Working") {
			return false, nil
		}
		for _, raw := range view.Messages {
			var message openAIMessage
			if json.Unmarshal(raw, &message) == nil && message.Role == "assistant" && contentText(message.Content) == "COLD_ROOT_HEADER_DONE" {
				root = view
				return true, nil
			}
		}
		return false, nil
	}); err != nil {
		return err
	}
	requests := p.state.requestEvidence()
	if p.state.totalRequests() != 1 || p.state.modelRequests("parent") != 1 || len(requests) != 1 || requests[0].RootMarker != "COLD_ROOT_HEADER" || len(requests[0].ToolResultCounts) != 0 || root.ID == "" || root.Cwd != p.workspace || len(root.Children) != 0 {
		return fmt.Errorf("cold context proof did not retain exactly one tool-free root request/header")
	}
	p.rootPath, p.rootID = root.Path, root.ID
	p.state.mu.Lock()
	p.state.fixtureRoot = root.ID
	p.state.mu.Unlock()
	if _, err := p.frame("cold-context-only-root-empty-widget"); err != nil {
		return err
	}
	return p.checkpoint("cold-context-only-one-request-no-tools")
}

func (p *panelProof) quiescentLogs() (map[string]string, error) {
	var previous map[string]string
	var stable time.Time
	err := waitFor(p.ctx, "quiescent native logs", stageTimeout, func() (bool, error) {
		current, err := nativeDigests(p.rootSessions, filepath.Join(p.agentDir, "sessions"))
		if err != nil {
			return false, err
		}
		if !reflect.DeepEqual(previous, current) {
			previous, stable = current, time.Now()
		}
		return len(current) > 0 && time.Since(stable) >= 250*time.Millisecond, nil
	})
	return previous, err
}

func (p *panelProof) logsUnchanged(before map[string]string) error {
	after, err := nativeDigests(p.rootSessions, filepath.Join(p.agentDir, "sessions"))
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("passive action changed complete native root/child logs")
	}
	return nil
}

func (p *panelProof) emptyInput(label, view string) error {
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	const barrier = "EMPTY_INPUT_BARRIER"
	if err := p.action(label+"-processed-barrier", func() error { return p.sendText(barrier) }, func(text string) bool {
		return hasPanelView(view)(text) && strings.Contains(text, barrier)
	}); err != nil {
		return err
	}
	return p.key(label+"-clear-barrier", "C-u", func(text string) bool { return hasPanelView(view)(text) && !strings.Contains(text, barrier) })
}

func (p *panelProof) verifyIdentities(root nativeSession) error {
	p.state.mu.Lock()
	admissions := make(map[string]admissionChild, len(p.state.admissions))
	calls := make(map[string]string, len(p.state.admissionCalls))
	spawns := make(map[string]spawnExpectation, len(p.state.expectedSpawns))
	for id, child := range p.state.admissions {
		admissions[id] = child
	}
	for id, call := range p.state.admissionCalls {
		calls[id] = call
	}
	for call, expected := range p.state.expectedSpawns {
		spawns[call] = expected
	}
	requests := append([]childRequest(nil), p.state.childRequests...)
	marker, fixtureRoot, workspace := p.state.quickHistoryMarker, p.state.fixtureRoot, p.state.fixtureWorkspace
	p.state.mu.Unlock()
	if root.ID != p.rootID || root.ID != fixtureRoot || root.Cwd != p.workspace || root.Cwd != workspace {
		return fmt.Errorf("root native header differs from independent fixture identity")
	}
	ids, err := expectedIdentityIDs(admissions, calls, spawns)
	if err != nil {
		return err
	}
	if len(admissions) != len(root.Children) {
		return fmt.Errorf("native children=%d spawn admissions=%d", len(root.Children), len(admissions))
	}
	models := map[string]string{}
	for id, admitted := range admissions {
		expected := spawns[calls[id]]
		parent := fixtureRoot
		if expected.ParentName != "$root" {
			parent = ids[expected.ParentName]
		}
		saved, exists := root.Children[id]
		if !exists || parent == "" || admitted.Parent != parent || admitted.Model != expected.Model || admitted.Cwd != expected.Cwd || admitted.Type != expected.Kind || saved.ID != id || saved.RootID != fixtureRoot || saved.ParentID != parent || saved.Name != expected.Name || saved.Model != expected.Model || saved.Cwd != expected.Cwd || saved.Kind != expected.Kind {
			return fmt.Errorf("spawn/native identity differs from emitted request for %s", expected.Name)
		}
		native, err := p.childNative(id)
		if err != nil {
			return err
		}
		if native.ID != id || native.Cwd != expected.Cwd {
			return fmt.Errorf("native child header differs from requested CWD for %s", expected.Name)
		}
		provider, model, ok := strings.Cut(expected.Model, "/")
		if !ok || provider != "fixture" || model == "" {
			return fmt.Errorf("fixture emitted non-loopback model %s", expected.Model)
		}
		models[expected.Name] = model
		if expected.Name == "quick-newer" {
			var messages []openAIMessage
			for _, raw := range native.Messages {
				var message openAIMessage
				if err := json.Unmarshal(raw, &message); err != nil {
					return err
				}
				messages = append(messages, message)
			}
			if !assistantContains(messages, marker) {
				return fmt.Errorf("quick native log lacks private prior assistant marker")
			}
		}
	}
	seen := map[string]bool{}
	for _, request := range requests {
		if request.Name == "" || models[request.Name] != request.Model {
			return fmt.Errorf("provider request %d model/task differs from emitted spawn", request.RequestIndex)
		}
		seen[request.Name] = true
	}
	if len(seen) != len(models) {
		return fmt.Errorf("missing provider observation for admitted fixture child")
	}
	return nil
}

var panelPosition = regexp.MustCompile(`Pig Litter \| list \(([0-9]+)/([0-9]+)\)`)
var detailUUID = regexp.MustCompile(`(?m)^UUID ([0-9a-f-]{36})\s*$`)

func (p *panelProof) verifyPanelSet(root nativeSession) error {
	expected := map[string]bool{}
	for id, child := range root.Children {
		if child.ParentID == root.ID {
			expected[id] = true
		}
	}
	if err := p.key("identity-list-home", "Home", func(text string) bool {
		return hasPanelView("list")(text) && strings.TrimPrefix(selectedPanelRow(text), "> ") == p.firstRow
	}); err != nil {
		return err
	}
	seen := map[string]bool{}
	for index := 0; index < len(expected); index++ {
		text, err := p.visible()
		if err != nil {
			return err
		}
		position := panelPosition.FindStringSubmatch(text)
		row := selectedPanelRow(text)
		if len(position) != 3 || position[1] != fmt.Sprint(index+1) || position[2] != fmt.Sprint(len(expected)) {
			return fmt.Errorf("panel direct-child cardinality/index differs from native ownership")
		}
		var id string
		if err := p.key(fmt.Sprintf("identity-detail-%d", index), "Enter", func(text string) bool {
			match := detailUUID.FindStringSubmatch(text)
			if !hasPanelView("detail")(text) || len(match) != 2 {
				return false
			}
			id = match[1]
			return true
		}); err != nil {
			return err
		}
		child := root.Children[id]
		if !expected[id] || seen[id] {
			return fmt.Errorf("panel contains unexpected or duplicate direct UUID %s", id)
		}
		text, err = p.visible()
		if err != nil {
			return err
		}
		if err := paintedIdentityMatches(row, text, child); err != nil {
			return err
		}
		seen[id] = true
		if err := p.key(fmt.Sprintf("identity-list-back-%d", index), "Escape", hasPanelView("list")); err != nil {
			return err
		}
		if index+1 < len(expected) {
			if err := p.key(fmt.Sprintf("identity-list-next-%d", index), "Down", func(text string) bool {
				return strings.Contains(text, fmt.Sprintf("Pig Litter | list (%d/%d)", index+2, len(expected)))
			}); err != nil {
				return err
			}
		}
	}
	if !reflect.DeepEqual(expected, seen) {
		return fmt.Errorf("panel omitted retained direct children")
	}
	return nil
}

func (p *panelProof) waitAfterEscape(label string, ready func(nativeSession) bool) error {
	err := waitFor(p.ctx, label, panelBudget-time.Since(p.lastEscape), func() (bool, error) {
		if err := p.state.providerFailure(); err != nil {
			return false, err
		}
		root, err := p.root()
		if err != nil {
			return false, nil
		}
		text, err := p.visible()
		return err == nil && !strings.Contains(text, "Working") && ready(root), nil
	})
	if err != nil {
		return err
	}
	observed := time.Now()
	size, err := p.geometry()
	if err != nil {
		return err
	}
	p.timings = append(p.timings, panelTiming{Action: label, SentAt: p.lastEscape.UTC().Format(time.RFC3339Nano), VisibleAt: observed.UTC().Format(time.RFC3339Nano), Milliseconds: float64(observed.Sub(p.lastEscape)) / float64(time.Millisecond), Width: size.width, Height: size.height})
	_, err = p.frame(label)
	return err
}

func (p *panelProof) verifyHandback(root nativeSession) error {
	if p.handback.ID == "" || p.handback.NoticeID == "" {
		return fmt.Errorf("consumed wait handback identity missing")
	}
	child := root.Children[p.handback.ID]
	child.Latest = p.handback.Latest
	child.NoticeID = p.handback.NoticeID
	count := 0
	for _, completion := range root.Completions {
		if completion.ChildID == child.ID && completion.Receipt == p.handback.NoticeID {
			count++
			if err := completionMatches(completion, child, p.handback.NoticeID); err != nil {
				return err
			}
		}
	}
	if count != p.handbackEntries || count > 1 {
		return fmt.Errorf("consumed wait handback duplicated after owner replacement")
	}
	return checkReservation(child, p.handback.NoticeID, false)
}

func (p *panelProof) fullRestartProof(quickID string) error {
	if err := p.resume("restart-child-resume", "quick-newer", "LIVE_QUICK_RUN_RESTART"); err != nil {
		return err
	}
	if err := p.close("restart-child-resume-close"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "restart child held", stageTimeout, func() (bool, error) { return p.state.holding("quick-restart"), p.state.providerFailure() }); err != nil {
		return err
	}
	if err := p.sendText("RECEIPT_WIDGET"); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	if err := waitFor(p.ctx, "restart root blocked", stageTimeout, func() (bool, error) { return p.state.holding("root-receipt"), p.state.providerFailure() }); err != nil {
		return err
	}
	if err := p.state.releaseHold("quick-restart"); err != nil {
		return err
	}
	var receipt string
	if err := p.waitNative("restart queue holds canonical natural completion", func(root nativeSession) bool {
		child := root.Children[quickID]
		var err error
		receipt, err = pendingReceipt(child)
		return err == nil && strings.HasPrefix(child.Latest.Text, "RESTART_REPORT_DONE")
	}); err != nil {
		return err
	}
	requests := p.state.modelRequests("parent")
	if err := p.rootEscape(); err != nil {
		return err
	}
	if err := p.waitAfterEscape("restart-root-Escape-paused", func(root nativeSession) bool {
		return root.Children[quickID].Paused && !p.state.holding("root-receipt")
	}); err != nil {
		return err
	}
	before, err := p.root()
	if err != nil {
		return err
	}
	if err := checkReservation(before.Children[quickID], receipt, true); err != nil {
		return err
	}
	if err := p.checkpoint("before-full-host-restart-queue-loss"); err != nil {
		return err
	}
	if err := p.restartHost(p.rootPath); err != nil {
		return err
	}
	if p.state.modelRequests("parent") != requests {
		return fmt.Errorf("full restart started root inference")
	}
	if err := p.passivePaused("full-host-restart-passive", before, true); err != nil {
		return err
	}
	for index, prompt := range []string{"ROOT_RESTART_FIRST", "ROOT_RESTART_SECOND", "ROOT_RESTART_THIRD"} {
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
		if err := p.waitNative(prompt+" settled", func(root nativeSession) bool {
			count := 0
			for _, completion := range root.Completions {
				if completion.Receipt == receipt && completion.ChildID == quickID {
					count++
					if completionMatches(completion, root.Children[quickID], receipt) != nil {
						return false
					}
				}
			}
			want := 1
			if index == 0 {
				want = 0
				if checkReservation(root.Children[quickID], receipt, true) != nil {
					return false
				}
			}
			text, err := p.visible()
			return err == nil && count == want && strings.Contains(text, prompt+"_DONE") && !strings.Contains(text, "Working")
		}); err != nil {
			return err
		}
		if p.state.modelRequests("parent") != requests+1 {
			return fmt.Errorf("restart explicit prompt caused extra inference")
		}
		root, err := p.root()
		if err != nil {
			return err
		}
		if err := p.verifyHandback(root); err != nil {
			return err
		}
		if err := p.checkpoint(strings.ToLower(prompt) + "-receipt-proof"); err != nil {
			return err
		}
	}
	return nil
}
