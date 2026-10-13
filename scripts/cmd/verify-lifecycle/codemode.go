package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

func codemodeTool(code string) call {
	return call{"codemode", map[string]string{"code": code}}
}

func codemodeOutput(message message) (string, error) {
	output := content(message.Content)
	const prefix = "Output:\n"
	index := strings.LastIndex(output, prefix)
	if !strings.HasPrefix(output, "Script completed\n") || index < 0 {
		return "", fmt.Errorf("codemode script did not complete: %s", output)
	}
	return strings.TrimSpace(output[index+len(prefix):]), nil
}

func decodeCodemodeOutput(message message, target any) error {
	value, err := codemodeOutput(message)
	if err != nil {
		return err
	}
	if err := json.Unmarshal([]byte(value), target); err != nil {
		return fmt.Errorf("codemode output is not JSON: %w", err)
	}
	return nil
}

func (f *fixture) markCheck(name string) {
	f.mu.Lock()
	f.checks[name] = true
	f.mu.Unlock()
}

func (f *fixture) parentCodemode(ctx context.Context, body modelRequest) (string, []call, error) {
	messages := toolMessages(body)
	if f.stage == 0 {
		if len(messages) != 0 {
			return "", nil, errors.New("codemode fixture started with prior tool results")
		}
		f.stage = 1
		code := "// @options: {\"timeout_ms\": 30000, \"max_output_tokens\": 128}\n" +
			"let listCalls = 0;\n" +
			"for (; listCalls < 257; listCalls++) await tools.litter_list({});\n" +
			"const spawned = JSON.parse(await tools.litter_spawn({ type: \"scout\", task: \"codemode-resume-target\", name: \"resume-target\", model: \"fixture/codemode_child\" }));\n" +
			"const waited = JSON.parse(await tools.litter_wait({ id: spawned.child.id, timeoutMs: 5000 }));\n" +
			"const outcome = waited.outcome || {};\n" +
			"return { listCalls, id: spawned.child.id, waitedId: outcome.run ? outcome.run.id : \"\", state: waited.state, text: outcome.text || \"\", report: waited.report || \"\" };"
		return "", []call{codemodeTool(code)}, nil
	}
	if len(messages) == 0 {
		return "", nil, fmt.Errorf("codemode stage %d has no tool result", f.stage)
	}
	last := messages[len(messages)-1]
	switch f.stage {
	case 1:
		var value struct {
			ListCalls                         int
			ID, WaitedID, State, Text, Report string
		}
		if err := decodeCodemodeOutput(last, &value); err != nil {
			return "", nil, err
		}
		if value.State != "completed" {
			return "", nil, fmt.Errorf("codemode wait ended in %s: %s", value.State, value.Report)
		}
		if value.ListCalls != 257 || value.ID == "" || value.WaitedID != value.ID || value.Text != "CODEMODE_INITIAL_RESULT" {
			return "", nil, fmt.Errorf("codemode did not await its spawned child: %#v", value)
		}
		f.codemodeResumeTarget = ref{ID: value.ID}
		f.runs = append(f.runs, f.codemodeResumeTarget)
		f.markCheck("codemodeSpawnWait")
		f.stage = 2
		code := "// @options: {\"timeout_ms\": 15000}\n" +
			"await tools.litter_message({ id: " + jsonString(f.codemodeResumeTarget.ID) + ", text: \"CODEMODE_RESUME_NOW\", resume: true });\n" +
			"await tools.litter_spawn({ type: \"scout\", task: \"codemode-survivor\", name: \"codemode-survivor\", model: \"fixture/codemode_child\" });\n" +
			"throw new Error(\"outer codemode failure after successful resume and spawn\");"
		return "", []call{codemodeTool(code)}, nil
	case 2:
		output := content(last.Content)
		if !strings.Contains(output, "Script failed") || !strings.Contains(output, "outer codemode failure after successful resume and spawn") {
			return "", nil, fmt.Errorf("codemode script did not report its outer failure: %s", output)
		}
		if err := f.wait(ctx, func() bool { return f.held["codemode-survivor"] != nil }); err != nil {
			return "", nil, fmt.Errorf("successful spawn did not survive its outer script failure: %w", err)
		}
		f.stage = 3
		return "", []call{
			{"litter_list", map[string]any{}},
			{"litter_wait", with(f.codemodeResumeTarget, "timeoutMs", 10000)},
		}, nil
	case 3:
		results, err := trailingToolResults(body, 2)
		if err != nil {
			return "", nil, fmt.Errorf("codemode resume results are not JSON: %w", err)
		}
		if results[1].State != "completed" || results[1].Outcome.Run != f.codemodeResumeTarget || results[1].Outcome.Text != "CODEMODE_RESUMED_RESULT" {
			return "", nil, fmt.Errorf("codemode resume did not survive its outer script failure: %#v", results[1])
		}
		for _, child := range results[0].Children {
			if child.Name == "codemode-survivor" {
				if child.State != "running" {
					return "", nil, fmt.Errorf("surviving child state is %q", child.State)
				}
				f.codemodeSurvivor = child.ref
			}
		}
		if f.codemodeSurvivor.ID == "" {
			return "", nil, errors.New("successful spawn disappeared after the outer script failed")
		}
		f.markCheck("codemodeSpawnSurvivesScriptFailure")
		f.markCheck("codemodeResumeSurvivesScriptFailure")
		if err := f.releaseTask("codemode-survivor"); err != nil {
			return "", nil, err
		}
		f.stage = 4
		return "", []call{{"litter_wait", with(f.codemodeSurvivor, "timeoutMs", 10000)}}, nil
	case 4:
		var waited result
		if err := json.Unmarshal([]byte(content(last.Content)), &waited); err != nil {
			return "", nil, fmt.Errorf("codemode survivor wait is not JSON: %w", err)
		}
		if waited.State != "completed" || waited.Outcome.Run != f.codemodeSurvivor || !strings.Contains(waited.Outcome.Text, "completed codemode-survivor") {
			return "", nil, fmt.Errorf("spawned child did not complete under its original ID: %#v", waited)
		}
		f.markCheck("codemodeSurvivorWait")
		f.stage = 5
		code := "// @options: {\"timeout_ms\": 15000}\n" +
			"const [resumed, stopped, waited] = await Promise.all([\n" +
			"  tools.litter_message({ id: " + jsonString(f.codemodeResumeTarget.ID) + ", text: \"CODEMODE_RESUME_NOW\", resume: true }),\n" +
			"  tools.litter_spawn({ type: \"scout\", task: \"codemode-stop-target\", name: \"codemode-stop-target\", model: \"fixture/codemode_child\" }),\n" +
			"  tools.litter_spawn({ type: \"scout\", task: \"codemode-wait-target\", name: \"codemode-wait-target\", model: \"fixture/codemode_child\" })\n" +
			"]);\n" +
			"return {\n" +
			"  resumedId: JSON.parse(resumed).child.id,\n" +
			"  stoppedId: JSON.parse(stopped).child.id,\n" +
			"  waitedId: JSON.parse(waited).child.id\n" +
			"};"
		return "", []call{codemodeTool(code)}, nil
	case 5:
		var value struct{ ResumedID, StoppedID, WaitedID string }
		if err := decodeCodemodeOutput(last, &value); err != nil {
			return "", nil, err
		}
		if value.ResumedID != f.codemodeResumeTarget.ID || value.StoppedID == "" || value.WaitedID == "" || value.StoppedID == value.WaitedID {
			return "", nil, fmt.Errorf("parallel spawn and resume returned unexpected identities: %#v", value)
		}
		f.codemodeStopped = ref{ID: value.StoppedID}
		f.codemodeWaited = ref{ID: value.WaitedID}
		f.runs = append(f.runs, f.codemodeStopped, f.codemodeWaited)
		if err := f.wait(ctx, func() bool {
			return f.held["codemode-stop"] != nil && f.held["codemode-parallel-wait"] != nil
		}); err != nil {
			return "", nil, fmt.Errorf("parallel child streams did not start: %w", err)
		}
		f.markCheck("codemodeParallelSpawnAndResume")
		if err := f.releaseTask("codemode-parallel-wait"); err != nil {
			return "", nil, err
		}
		f.stage = 6
		code := "// @options: {\"timeout_ms\": 15000}\n" +
			"const [resumed, stopped, waited] = await Promise.all([\n" +
			"  tools.litter_wait({ id: " + jsonString(f.codemodeResumeTarget.ID) + ", timeoutMs: 10000 }),\n" +
			"  tools.litter_stop({ id: " + jsonString(f.codemodeStopped.ID) + " }),\n" +
			"  tools.litter_wait({ id: " + jsonString(f.codemodeWaited.ID) + ", timeoutMs: 10000 })\n" +
			"]);\n" +
			"return { resumed, stopped, waited };"
		return "", []call{codemodeTool(code)}, nil
	case 6:
		var value struct{ Resumed, Stopped, Waited string }
		if err := decodeCodemodeOutput(last, &value); err != nil {
			return "", nil, err
		}
		var resumed, stopped, waited result
		if err := json.Unmarshal([]byte(value.Resumed), &resumed); err != nil {
			return "", nil, fmt.Errorf("parallel resume wait result is not JSON: %w", err)
		}
		if err := json.Unmarshal([]byte(value.Stopped), &stopped); err != nil {
			return "", nil, fmt.Errorf("parallel stop result is not JSON: %w", err)
		}
		if err := json.Unmarshal([]byte(value.Waited), &waited); err != nil {
			return "", nil, fmt.Errorf("parallel child wait result is not JSON: %w", err)
		}
		if resumed.State != "completed" || resumed.Outcome.Run != f.codemodeResumeTarget || resumed.Outcome.Text != "CODEMODE_RESUMED_RESULT" {
			return "", nil, fmt.Errorf("resumed child changed identity or result: %#v", resumed)
		}
		if stopped.State == "" || stopped.Code != "" {
			return "", nil, fmt.Errorf("parallel stop did not acknowledge: %#v", stopped)
		}
		if waited.State != "completed" || waited.Outcome.Run != f.codemodeWaited || !strings.Contains(waited.Outcome.Text, "completed codemode-parallel-wait") {
			return "", nil, fmt.Errorf("parallel wait did not return the selected child's result: %#v", waited)
		}
		if err := f.wait(ctx, func() bool {
			return f.aborted["codemode-stop"] && f.held["codemode-stop"] == nil
		}); err != nil {
			return "", nil, fmt.Errorf("parallel stop did not cancel the child provider request: %w", err)
		}
		f.markCheck("codemodeParallelWaitStopAndCancellation")
		f.done = true
		return "CODEMODE_DONE", nil, nil
	}
	return "", nil, fmt.Errorf("unexpected codemode stage %d", f.stage)
}

func jsonString(value string) string {
	data, _ := json.Marshal(value)
	return string(data)
}

func (f *fixture) codemodeChild(ctx context.Context, body modelRequest) (string, []call, error) {
	user := ""
	for _, message := range body.Messages {
		if message.Role == "user" {
			user += content(message.Content) + "\n"
		}
	}
	switch {
	case strings.Contains(user, "codemode-resume-target"):
		if strings.Contains(user, "CODEMODE_RESUME_NOW") {
			return "CODEMODE_RESUMED_RESULT", nil, nil
		}
		return "CODEMODE_INITIAL_RESULT", nil, nil
	case strings.Contains(user, "codemode-survivor"):
		return f.hold(ctx, "codemode-survivor"), nil, nil
	case strings.Contains(user, "codemode-stop-target"):
		return f.hold(ctx, "codemode-stop"), nil, nil
	case strings.Contains(user, "codemode-wait-target"):
		return f.hold(ctx, "codemode-parallel-wait"), nil, nil
	}
	return "", nil, errors.New("codemode child received an unexpected task")
}
