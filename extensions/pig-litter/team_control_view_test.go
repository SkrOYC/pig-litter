package piglitter

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestTeamPanelRequestsAndCancelsSelectedShutdown(t *testing.T) {
	fixture, store, members, runs := teamViewFixture(t)
	withCompletionHost(t, fixture.tree, func(o *owner) {
		o.teamStore, o.agentDir = store, fixture.agentDir
		o.tree.mu.Lock()
		runs[0].owner.admitted = true
		o.tree.mu.Unlock()
		panel := newChildPanel(o, nil, func() int { return 32 })
		panel.mu.Lock()
		panel.team = newTeamView(panel.ctx, o, panel.requestRender)
		panel.mu.Unlock()
		defer func() {
			panel.Dispose()
			o.actionWG.Wait()
		}()
		waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool { return len(frame.Members) == 2 && frame.PreferenceLoaded })
		panel.Render(120)
		panelKey(t, panel, "s")
		panelKey(t, panel, "Please stop after your current work.")
		panelKey(t, panel, "\r")
		waitFeedback := func(expected string) {
			t.Helper()
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				if strings.Contains(strings.Join(panel.Render(120), "\n"), expected) {
					return
				}
				time.Sleep(10 * time.Millisecond)
			}
			t.Fatalf("shutdown feedback %q did not appear: %s", expected, strings.Join(panel.Render(120), "\n"))
		}
		waitFeedback("Shutdown request pending")
		lifecycle, err := store.snapshotLifecycle(members[0].Ref)
		if err != nil || len(lifecycle.Requests) != 1 || lifecycle.Requests[0].State != "pending" || lifecycle.Requests[0].Reason != "Please stop after your current work." || lifecycle.Requests[0].ID.Member != members[0].Ref {
			t.Fatalf("request was not stored for the selected teammate: %+v, %v", lifecycle, err)
		}
		other, err := store.snapshotLifecycle(members[1].Ref)
		if err != nil || len(other.Requests) != 0 {
			t.Fatalf("request changed another teammate: %+v, %v", other, err)
		}
		panelKey(t, panel, "h")
		waitTeamFrame(t, panel.team, func(frame *teamViewFrame) bool {
			return frame.ShutdownHealth.Status == "fresh" && len(frame.Shutdowns) == 1
		})
		if text := strings.Join(panel.Render(120), "\n"); !strings.Contains(text, "#1 pending | Alice") {
			t.Fatalf("shutdown list omitted its selected request: %s", text)
		}
		panelKey(t, panel, "\r")
		if text := strings.Join(panel.Render(120), "\n"); !strings.Contains(text, "request pending | phase open") || !strings.Contains(text, "Notice shutdown_request | delivery pending") || !strings.Contains(text, "Please stop after your current work.") {
			t.Fatalf("shutdown detail hid the request or lifecycle phase: %s", text)
		}
		panelKey(t, panel, "x")
		waitFeedback("Shutdown #1 cancelled")
		lifecycle, err = store.snapshotLifecycle(members[0].Ref)
		if err != nil || len(lifecycle.Requests) != 1 || lifecycle.Requests[0].State != "cancelled" {
			t.Fatalf("selected shutdown was not cancelled: %+v, %v", lifecycle, err)
		}
	}, func(string, json.RawMessage) json.RawMessage { return json.RawMessage(`"fixture"`) })
}
