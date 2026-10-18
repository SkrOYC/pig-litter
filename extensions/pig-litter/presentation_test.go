package piglitter

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode"
	"unicode/utf8"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/MichaelKinsy/PiG/tui/widthx"
)

func renderFixture(t *testing.T, operation, raw string, expanded bool, width int) []string {
	t.Helper()
	rows, err := toolResultRenderer(operation)(sdk.Context{}, sdk.ToolRenderResult{Content: []map[string]any{{"type": "text", "text": raw}}}, sdk.ToolRenderResultOptions{Expanded: expanded}, sdk.ToolRenderContext{}, width)
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestToolCallPresentation(t *testing.T) {
	for _, test := range []struct {
		operation string
		args      map[string]any
		want      string
	}{
		{"litter_spawn", map[string]any{"type": "scout", "name": "review", "task": "Check API"}, "litter_spawn scout as review | Check API"},
		{"litter_list", nil, "litter_list"},
		{"litter_inspect", map[string]any{"id": "child-1"}, "litter_inspect child-1"},
		{"litter_message", map[string]any{"id": "child-1", "text": "Check tests"}, "litter_message child-1 steer | Check tests"},
		{"litter_message", map[string]any{"id": "child-1", "mode": "follow_up", "text": "Then docs"}, "litter_message child-1 follow_up | Then docs"},
		{"litter_message", map[string]any{"id": "child-1", "resume": true, "text": "Continue"}, "litter_message child-1 resume | Continue"},
		{"litter_stop", map[string]any{"id": "child-1"}, "litter_stop child-1"},
		{"litter_wait", map[string]any{"id": "child-1", "timeoutMs": float64(30000)}, "litter_wait child-1"},
	} {
		t.Run(test.want, func(t *testing.T) {
			rows, err := toolCallRenderer(test.operation)(sdk.Context{}, test.args, sdk.ToolRenderContext{}, 100)
			if err != nil || !reflect.DeepEqual(rows, []string{test.want}) {
				t.Fatalf("got %q, %v; want %q", rows, err, test.want)
			}
		})
	}
	rows, err := toolCallRenderer("litter_spawn")(sdk.Context{}, map[string]any{"type": "scout", "model": "fixture/child", "cwd": "/work", "task": "Line one\nLine two"}, sdk.ToolRenderContext{Expanded: true}, 100)
	if err != nil || !reflect.DeepEqual(rows, []string{"litter_spawn scout", "Model fixture/child", "Cwd /work", "Line one", "Line two"}) {
		t.Fatalf("expanded call = %q, %v", rows, err)
	}
	rows, err = toolCallRenderer("litter_wait")(sdk.Context{}, map[string]any{"id": "child-1", "timeoutMs": float64(42)}, sdk.ToolRenderContext{Expanded: true}, 100)
	if err != nil || !reflect.DeepEqual(rows, []string{"litter_wait child-1", "Timeout 42 ms"}) {
		t.Fatalf("expanded wait = %q, %v", rows, err)
	}
	for _, args := range []map[string]any{nil, {}, {"id": 42, "type": false, "task": []string{"partial"}}} {
		rows, err := toolCallRenderer("litter_spawn")(sdk.Context{}, args, sdk.ToolRenderContext{ArgsComplete: false}, 32)
		if err != nil || !reflect.DeepEqual(rows, []string{"litter_spawn"}) {
			t.Fatalf("partial args = %q, %v", rows, err)
		}
	}
}

func TestToolResultPresentationForSixOperations(t *testing.T) {
	for _, test := range []struct {
		operation, payload, compact string
		details                     []string
	}{
		{"litter_spawn", `"state":"starting","child":{"id":"child-1","name":"review","type":"scout","model":"fixture/child","cwd":"/work","state":"starting"}`, "starting review #child-1", []string{"UUID child-1", "Name review", "Type scout", "Model fixture/child", "Cwd /work"}},
		{"litter_list", `"state":"listed","children":[{"id":"child-1","name":"review","state":"running","model":"fixture/child","cwd":"/work"}],"totalChildren":2,"more":true,"totalAgentTypes":1,"availableAgentTypes":[{"name":"scout","role":"scout","description":"Read source","model":"fixture/child"}]`, "listed 1/2 children | 1/1 types", []string{"review #child-1 running", "UUID child-1", "Agent scout scout", "Read source", "More children available"}},
		{"litter_inspect", `"state":"paused","child":{"id":"child-1","name":"review","state":"paused"},"outcome":{"run":{"id":"child-1"},"state":"completed","model":"fixture/child","text":"Retained report"}`, "paused review #child-1 | Retained report", []string{"Outcome completed", "Report", "Retained report"}},
		{"litter_message", `"state":"running","child":{"id":"child-1","name":"review"},"delivery":"queued"`, "running review #child-1 queued", []string{"UUID child-1"}},
		{"litter_stop", `"state":"paused","outcome":{"run":{"id":"child-1"},"state":"paused","model":"fixture/child","text":"Paused report"},"stoppedCount":2`, "paused child-1 | Paused report", []string{"UUID child-1", "Model fixture/child", "Paused executions 2", "Outcome paused"}},
		{"litter_wait", `"state":"completed","outcome":{"run":{"id":"child-1"},"state":"completed","model":"fixture/child","text":"Checked API\nNo regressions","usage":{"input":10,"output":5,"totalTokens":15,"cost":0.01,"cacheRead":3,"cacheWrite":2},"truncated":true,"handbackTruncated":true,"reportProvenance":"assistant"},"handbackTruncated":true`, "completed child-1 | Checked API No regressions", []string{"Usage 10 in / 5 out / 15 total / $0.010000", "Cache 3 read / 2 write", "Report source assistant", "Result truncated", "Outcome handback truncated", "Handback truncated", "Checked API\nNo regressions"}},
	} {
		t.Run(test.operation, func(t *testing.T) {
			raw := `{"operation":"` + test.operation + `",` + test.payload + `}`
			if got := renderFixture(t, test.operation, raw, false, 100); !reflect.DeepEqual(got, []string{test.compact}) {
				t.Fatalf("compact = %q; want %q", got, test.compact)
			}
			expanded := strings.Join(renderFixture(t, test.operation, raw, true, 100), "\n")
			for _, want := range test.details {
				if !strings.Contains(expanded, want) {
					t.Errorf("expanded missing %q in %q", want, expanded)
				}
			}
		})
	}
}

func TestToolResultStatesAndFallback(t *testing.T) {
	for _, state := range []string{"completed", "partial", "failed", "paused", "stopped", "unavailable"} {
		raw := `{"operation":"litter_wait","state":"` + state + `","outcome":{"run":{"id":"child-1"},"state":"` + state + `","text":"Useful report","error":"Tool failed"}}`
		got := renderFixture(t, "litter_wait", raw, false, 100)
		if !reflect.DeepEqual(got, []string{state + " child-1 | Tool failed"}) {
			t.Fatalf("%s = %q", state, got)
		}
		expanded := strings.Join(renderFixture(t, "litter_wait", raw, true, 100), "\n")
		if !strings.Contains(expanded, "Error Tool failed") || !strings.Contains(expanded, "Useful report") {
			t.Fatalf("lost error/report %q", expanded)
		}
	}
	for _, test := range []struct {
		err   error
		state string
	}{
		{reject("wait_timeout", "wait timed out"), "timed_out"},
		{reject("cancelled", "wait cancelled"), "cancelled"},
		{reject("permission_denied", "only immediate parent"), "rejected"},
	} {
		raw, _ := resultContent("litter_wait", nil, test.err)
		if got := renderFixture(t, "litter_wait", raw, false, 100); !reflect.DeepEqual(got, []string{test.state + " | " + test.err.Error()}) {
			t.Fatalf("error = %q", got)
		}
	}
	for _, raw := range []string{"", "{", `null`, `[]`, `{}`, `{"state":"completed"}`, `{"operation":"litter_stop","state":"paused"}`, `{"operation":"litter_wait","state":5}`} {
		_, err := toolResultRenderer("litter_wait")(sdk.Context{}, sdk.ToolRenderResult{Content: []map[string]any{{"type": "text", "text": raw}}}, sdk.ToolRenderResultOptions{IsPartial: true}, sdk.ToolRenderContext{}, 32)
		if err == nil {
			t.Fatalf("malformed result %q did not request native fallback", raw)
		}
	}
	for _, content := range [][]map[string]any{nil, {{"type": "image"}}, {{"type": "text", "text": 3}}} {
		_, err := toolResultRenderer("litter_wait")(sdk.Context{}, sdk.ToolRenderResult{Content: content}, sdk.ToolRenderResultOptions{}, sdk.ToolRenderContext{}, 32)
		if err == nil {
			t.Fatalf("content %v did not request native fallback", content)
		}
	}
	rows, err := toolResultRenderer("litter_list")(sdk.Context{}, sdk.ToolRenderResult{Content: []map[string]any{{"type": "text", "text": `{"operation":"litter_list","state":"listed"}`}}}, sdk.ToolRenderResultOptions{IsPartial: true}, sdk.ToolRenderContext{}, 100)
	if err != nil || !reflect.DeepEqual(rows, []string{"listed 0/0 children | 0/0 types (receiving)"}) {
		t.Fatalf("partial result = %q %v", rows, err)
	}
}

func TestPaintTextTerminalControls(t *testing.T) {
	for _, test := range []struct{ input, want string }{
		{"plain\x1b[31mred\x1b[0m", "plainred"},
		{"one\x1b[2Jtwo", "onetwo"},
		{"one\x1b]0;hostile\aok", "oneok"},
		{"one\x1b]8;;https://evil\x1b\\link\x1b]8;;\x1b\\", "onelink"},
		{"one\x1bPpayload\x1b\\ok", "oneok"},
		{"one\x1b_ignored\x1b\\ok", "oneok"},
		{"one\x1b^ignored\x1b\\ok", "oneok"},
		{"one\x1bXignored\x1b\\ok", "oneok"},
		{"one\x1b(Btwo", "onetwo"},
		{"one\x1b7two", "onetwo"},
		{"one\x1b", "one"},
		{"one\x1b[123;", "one"},
		{"one\x1b]8;;incomplete", "one"},
		{"one\x1bPunfinished", "one"},
		{"one\u009b2Jtwo\u009dtitle\u009cend", "onetwoend"},
		{"one\u0090payload\u009cend", "oneend"},
		{"a\x00\a\b\v\f\x7f\u0085b", "ab"},
		{"a\u061c\u200e\u200f\u202a\u202b\u202c\u202d\u202e\u2066\u2067\u2068\u2069b", "ab"},
		{"a\xffb", "a\uFFFDb"},
		{"a\t中", "a   中"},
	} {
		if got := paintText(test.input, true); got != test.want {
			t.Errorf("paint %q = %q; want %q", test.input, got, test.want)
		}
	}
	if got := paintText("one\r\ntwo\nthree", true); got != "one\ntwo\nthree" {
		t.Fatalf("report lines = %q", got)
	}
	if got := paintText("one\r\ntwo\nthree", false); got != "one two three" {
		t.Fatalf("metadata lines = %q", got)
	}
}

func TestPresentationWidthsSanitizationAndRawHandbacks(t *testing.T) {
	attack := "中👩🏽‍💻e\u0301\x1b[2J\x1b]0;hostile\a\x00\u202e\nnext"
	child := Snapshot{Ref: Ref{ID: "child-1"}, Name: attack, DisplayLabel: attack, Type: attack, Model: attack, Cwd: attack, State: Paused}
	outcome := &Outcome{Run: Ref{ID: "child-1"}, State: Partial, Text: attack, Error: attack, Model: attack, ReportProvenance: attack}
	for _, operation := range controlNames {
		value := map[string]any{"state": Partial, "child": child, "outcome": outcome, "children": []Snapshot{child}, "availableAgentTypes": []map[string]any{{"name": attack, "role": attack, "description": attack, "model": attack}}}
		raw, failed := resultContent(operation, value, nil)
		before, _ := json.Marshal(value)
		for _, width := range []int{100, 32, 2, 1, 0} {
			for _, expanded := range []bool{false, true} {
				rows := renderFixture(t, operation, raw, expanded, width)
				assertSafeRows(t, rows, width)
				rows, err := toolCallRenderer(operation)(sdk.Context{}, map[string]any{"id": attack, "type": attack, "name": attack, "model": attack, "cwd": attack, "task": attack, "text": attack, "mode": attack}, sdk.ToolRenderContext{Expanded: expanded}, width)
				if err != nil {
					t.Fatal(err)
				}
				assertSafeRows(t, rows, width)
			}
		}
		after, _ := json.Marshal(value)
		again, againFailed := resultContent(operation, value, nil)
		if string(before) != string(after) || again != raw || failed != againFailed || outcome.Text != attack {
			t.Fatal("presentation mutated machine handback or retained report")
		}
		if !strings.Contains(raw, `\u001b`) || !strings.Contains(raw, `\u0000`) {
			t.Fatalf("machine controls no longer JSON escaped: %q", raw)
		}
	}
	for _, width := range []int{100, 32, 2, 1} {
		rows := toolPaintRows(nil, "👩🏽‍💻e\u0301中", width)
		assertSafeRows(t, rows, width)
		if width >= 2 && !strings.Contains(strings.Join(rows, ""), "👩🏽‍💻") {
			t.Fatalf("split emoji at width %d: %q", width, rows)
		}
	}
}

func assertSafeRows(t *testing.T, rows []string, width int) {
	t.Helper()
	for _, row := range rows {
		if !utf8.ValidString(row) || widthx.VisibleWidth(row) > width {
			t.Fatalf("unsafe width %d row %q", width, row)
		}
		for _, r := range row {
			if unicode.IsControl(r) || r == '\u202e' {
				t.Fatalf("unsafe control %U in %q", r, row)
			}
		}
	}
}

func TestToolPresentationLimits(t *testing.T) {
	rows := toolPaintRows([]string{"Header"}, strings.Repeat("line\n", 200), 32)
	if len(rows) != 80 || rows[len(rows)-1] != "[display rows clipped]" {
		t.Fatalf("row clipping = %d, %q", len(rows), rows[len(rows)-1])
	}
	large := strings.Repeat("👩🏽‍💻", 6000)
	clean := paintText(large, true)
	if !strings.HasSuffix(clean, " [display text clipped]") || !utf8.ValidString(clean) || len(clean) > paintByteLimit+len(" [display text clipped]") {
		t.Fatal("unbounded or invalid paint text")
	}
	for _, cluster := range strings.Split(strings.TrimSuffix(clean, " [display text clipped]"), "👩🏽‍💻") {
		if cluster != "" {
			t.Fatalf("clipped grapheme = %q", cluster)
		}
	}
}

func TestPresentationDoesNotChangeLiteralErrorHandback(t *testing.T) {
	const want = `{"code":"cancelled","handbackTruncated":false,"operation":"litter_wait","report":"wait \u001b[2Jcancelled","state":"cancelled"}`
	raw, failed := resultContent("litter_wait", nil, reject("cancelled", "wait \x1b[2Jcancelled"))
	if raw != want || failed {
		t.Fatalf("machine contract = %s %t", raw, failed)
	}
	if got := renderFixture(t, "litter_wait", raw, false, 100); !reflect.DeepEqual(got, []string{"cancelled | wait cancelled"}) {
		t.Fatalf("human result = %q", got)
	}
}

func TestCompactUUIDTargetsRemainDistinctAt32Columns(t *testing.T) {
	for _, test := range []struct{ id, suffix string }{
		{"01995bf7-5dd4-7000-8000-000082ad1f00", "#82ad1f00"},
		{"01995bf7-5dd4-7000-8000-0000450aab91", "#450aab91"},
	} {
		for _, operation := range []string{"litter_inspect", "litter_message", "litter_stop", "litter_wait"} {
			rows, err := toolCallRenderer(operation)(sdk.Context{}, map[string]any{"id": test.id}, sdk.ToolRenderContext{}, 32)
			if err != nil || !strings.Contains(strings.Join(rows, "\n"), test.suffix) {
				t.Fatalf("compact %s target = %q %v", operation, rows, err)
			}
			expanded, err := toolCallRenderer(operation)(sdk.Context{}, map[string]any{"id": test.id}, sdk.ToolRenderContext{Expanded: true}, 100)
			if err != nil || !strings.Contains(strings.Join(expanded, "\n"), test.id) {
				t.Fatalf("expanded call lost UUID = %q %v", expanded, err)
			}
		}
		raw := `{"operation":"litter_wait","state":"completed","outcome":{"run":{"id":"` + test.id + `"},"state":"completed","text":"Done"}}`
		if got := renderFixture(t, "litter_wait", raw, false, 32); !reflect.DeepEqual(got, []string{"completed " + test.suffix + " | Done"}) {
			t.Fatalf("compact outcome = %q", got)
		}
		if got := strings.Join(renderFixture(t, "litter_wait", raw, true, 100), "\n"); !strings.Contains(got, "UUID "+test.id) {
			t.Fatalf("expanded outcome lost UUID = %q", got)
		}
	}
}

func TestPaginatedToolListPresentation(t *testing.T) {
	const raw = `{"operation":"litter_list","state":"listed","children":[{"id":"child-1","name":"review","state":"running","model":"fixture/child","cwd":"/work"}],"totalChildren":2,"more":true,"totalAgentTypes":7,"moreAgentTypes":true,"availableAgentTypes":[{"name":"scout","role":"scout","description":"Read source"}]}`
	if got := renderFixture(t, "litter_list", raw, false, 100); !reflect.DeepEqual(got, []string{"listed 1/2 children | 1/7 types"}) {
		t.Fatalf("compact agent total = %q", got)
	}
	want := []string{"listed 1/2 children | 1/7 types", "review #child-1 running", "  UUID child-1", "  Model fixture/child", "  Cwd /work", "Agent scout scout", "Read source", "More children available", "More agent types available"}
	if got := renderFixture(t, "litter_list", raw, true, 100); !reflect.DeepEqual(got, want) {
		t.Fatalf("expanded agent page = %q; want %q", got, want)
	}
}

func TestUnnamedChildPresentationUsesSuffixOnce(t *testing.T) {
	const id = "01995bf7-5dd4-7000-8000-000082ad1f00"
	child := Snapshot{Ref: Ref{ID: id}, Name: id, DisplayLabel: "#82ad1f00", Type: "scout", Model: "fixture/child", Cwd: "/work", State: Running}
	for _, test := range []struct {
		operation string
		value     map[string]any
		compact   string
	}{
		{"litter_spawn", map[string]any{"state": Starting, "child": child}, "starting #82ad1f00"},
		{"litter_list", map[string]any{"state": "listed", "children": []Snapshot{child}, "totalChildren": 1, "totalAgentTypes": 0}, "listed 1/1 children | 0/0 types"},
		{"litter_inspect", map[string]any{"state": Running, "child": child}, "running #82ad1f00"},
		{"litter_message", map[string]any{"state": Running, "child": child, "delivery": "queued"}, "running #82ad1f00 queued"},
	} {
		t.Run(test.operation, func(t *testing.T) {
			raw, failed := resultContent(test.operation, test.value, nil)
			if failed {
				t.Fatal(raw)
			}
			if got := renderFixture(t, test.operation, raw, false, 32); !reflect.DeepEqual(got, []string{test.compact}) {
				t.Fatalf("compact unnamed child = %q; want %q", got, test.compact)
			}
			for _, width := range []int{32, 100} {
				rows := renderFixture(t, test.operation, raw, true, width)
				assertSafeRows(t, rows, width)
				if got := strings.Join(rows, ""); !strings.Contains(got, id) || strings.Count(got, "#82ad1f00") != 1 || !strings.Contains(got, "UUID") {
					t.Fatalf("expanded unnamed child at width %d = %q", width, rows)
				}
			}
		})
	}
}
