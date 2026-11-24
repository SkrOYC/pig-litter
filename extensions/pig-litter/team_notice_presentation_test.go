package piglitter

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/MichaelKinsy/PiG/tui/widthx"
)

func TestTeamNoticeRenderersPreserveContentAndShowReadableFacts(t *testing.T) {
	o, _, members, _ := teamViewFixture(t)
	ref := members[0].Ref
	turn := turnNotice{Member: ref, Execution: "run", Kind: "failed", Report: "Report marker", Error: "Tool failed \x1b]52;c;STEAL\a\u202e"}
	control := struct {
		Kind      string            `json:"kind"`
		RequestID ShutdownRequestID `json:"requestId"`
		Message   string            `json:"message"`
	}{Kind: "shutdown_rejected", RequestID: ShutdownRequestID{Member: ref, Sequence: 1, Nonce: "nonce"}, Message: "Finishing current work."}
	for _, test := range []struct {
		name     string
		value    any
		renderer sdk.RendererFunc
		want     []string
	}{
		{"turn", turn, teamTurnNoticeRenderer(o.state), []string{"Teammate Alice | failed", "Error Tool failed", "Report marker"}},
		{"shutdown", control, teamControlNoticeRenderer(o.state), []string{"Teammate Alice | shutdown declined", "Finishing current work."}},
	} {
		t.Run(test.name, func(t *testing.T) {
			encoded, err := json.Marshal(test.value)
			if err != nil {
				t.Fatal(err)
			}
			message := map[string]any{"content": string(encoded), "details": map[string]any{"marker": "unchanged"}}
			before, err := json.Marshal(message)
			if err != nil {
				t.Fatal(err)
			}
			for _, width := range []int{80, 120, 150} {
				rows, err := test.renderer(sdk.Context{}, message, sdk.MessageRenderOptions{}, width)
				if err != nil {
					t.Fatal(err)
				}
				text := strings.Join(rows, "\n")
				for _, want := range test.want {
					if !strings.Contains(text, want) {
						t.Fatalf("notice omitted %q at %d columns: %s", want, width, text)
					}
				}
				if strings.ContainsAny(text, "\x1b\u202e") || strings.Contains(text, "STEAL") || strings.Contains(text, `"member"`) {
					t.Fatalf("notice exposed unsafe or raw content: %s", text)
				}
				for _, row := range rows {
					if !utf8.ValidString(row) || widthx.VisibleWidth(row) > width {
						t.Fatalf("notice exceeded %d columns: %q", width, row)
					}
				}
			}
			after, err := json.Marshal(message)
			if err != nil || string(after) != string(before) {
				t.Fatalf("rendering changed the stored message: %s, %v", after, err)
			}
		})
	}
}

func TestTeamNoticeRendererHandlesMissingIdentityAndMalformedContent(t *testing.T) {
	o, _, members, _ := teamViewFixture(t)
	notice := turnNotice{Member: members[0].Ref, Kind: "available", Report: "Ready"}
	encoded, err := json.Marshal(notice)
	if err != nil {
		t.Fatal(err)
	}
	o.state.mu.Lock()
	o.state.owner = nil
	o.state.mu.Unlock()
	rows, err := teamTurnNoticeRenderer(o.state)(sdk.Context{}, map[string]any{"content": string(encoded)}, sdk.MessageRenderOptions{}, 80)
	if err != nil || !strings.Contains(strings.Join(rows, "\n"), "Teammate #"+string(members[0].Ref.ID)[len(members[0].Ref.ID)-8:]+" | available") {
		t.Fatalf("retired owner did not fall back to the member identity: %q, %v", rows, err)
	}
	for _, renderer := range []sdk.RendererFunc{teamTurnNoticeRenderer(o.state), teamControlNoticeRenderer(o.state)} {
		rows, err := renderer(sdk.Context{}, map[string]any{"content": "{\x1b]52;c;STEAL\a"}, sdk.MessageRenderOptions{}, 80)
		if err != nil || len(rows) != 1 || !strings.Contains(rows[0], "unavailable") || strings.Contains(rows[0], "STEAL") {
			t.Fatalf("malformed notice did not render safely: %q, %v", rows, err)
		}
	}
}
