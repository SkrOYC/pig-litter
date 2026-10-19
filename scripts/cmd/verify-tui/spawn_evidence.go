package main

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
)

type spawnExpectation struct {
	CallID     string `json:"callId"`
	Name       string `json:"name"`
	ParentName string `json:"parentName"`
	Model      string `json:"model"`
	Cwd        string `json:"cwd"`
	Kind       string `json:"kind"`
	Task       string `json:"task"`
}

type childRequest struct {
	RequestIndex int    `json:"requestIndex"`
	Name         string `json:"name"`
	Model        string `json:"model"`
}

func (state *fixtureState) requestChildNameLocked(request openAIRequest) string {
	name := ""
	for _, message := range request.Messages {
		if message.Role != "user" {
			continue
		}
		task := strings.TrimPrefix(contentText(message.Content), "Task:\n")
		for _, expected := range state.expectedSpawns {
			if task == expected.Task {
				if name != "" && name != expected.Name {
					return ""
				}
				name = expected.Name
			}
		}
	}
	return name
}

func (state *fixtureState) expectSpawn(callID string, request openAIRequest, arguments []byte) error {
	var args struct{ Name, Model, Cwd, Type, Task string }
	if err := json.Unmarshal(arguments, &args); err != nil {
		return err
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	parentName := "$root"
	cwd := state.fixtureWorkspace
	if request.Model != "parent" {
		parentName = state.requestChildNameLocked(request)
		for _, parent := range state.expectedSpawns {
			if parent.Name == parentName {
				cwd = parent.Cwd
			}
		}
	}
	if callID == "" || state.fixtureRoot == "" || parentName == "" || cwd == "" || args.Name == "" || args.Model == "" || args.Type == "" || args.Task == "" {
		return fmt.Errorf("fixture emitted spawn without independent identity context")
	}
	if args.Cwd != "" {
		if filepath.IsAbs(args.Cwd) {
			cwd = filepath.Clean(args.Cwd)
		} else {
			cwd = filepath.Join(cwd, args.Cwd)
		}
	}
	for _, old := range state.expectedSpawns {
		if old.Name == args.Name {
			return fmt.Errorf("fixture emitted duplicate child name %s", args.Name)
		}
	}
	state.expectedSpawns[callID] = spawnExpectation{CallID: callID, Name: args.Name, ParentName: parentName, Model: args.Model, Cwd: cwd, Kind: args.Type, Task: args.Task}
	return nil
}

func expectedIdentityIDs(admissions map[string]admissionChild, calls map[string]string, spawns map[string]spawnExpectation) (map[string]string, error) {
	if len(admissions) != len(spawns) {
		return nil, fmt.Errorf("spawn request/admission cardinality differs")
	}
	ids := make(map[string]string, len(spawns))
	for id, admitted := range admissions {
		call := calls[id]
		expected, ok := spawns[call]
		if call == "" || !ok || expected.CallID != call || admitted.ID != id || admitted.Name != expected.Name || admitted.Parent == "" || admitted.Model == "" || admitted.Cwd == "" || admitted.Type == "" {
			return nil, fmt.Errorf("spawn admission lacks matching call ID/name or mandatory identity fields")
		}
		if ids[expected.Name] != "" {
			return nil, fmt.Errorf("multiple admissions for fixture child %s", expected.Name)
		}
		ids[expected.Name] = id
	}
	return ids, nil
}

func parsePanelDetail(text string) (map[string]string, error) {
	if !hasPanelView("detail")(text) {
		return nil, fmt.Errorf("identity detail is not the current view")
	}
	fields := map[string]string{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimRight(line, " \r")
		for _, key := range []string{"UUID", "Name", "Type", "Model", "Cwd", "State", "Admission"} {
			if value, ok := strings.CutPrefix(line, key+" "); ok {
				if _, duplicate := fields[key]; duplicate {
					return nil, fmt.Errorf("duplicate painted identity field %s", key)
				}
				fields[key] = value
			}
		}
	}
	for _, key := range []string{"UUID", "Name", "Type", "Model", "Cwd", "State", "Admission"} {
		if fields[key] == "" {
			return nil, fmt.Errorf("missing painted identity field %s", key)
		}
	}
	return fields, nil
}

func paintedIdentityMatches(row, detail string, child nativeChild) error {
	if len(child.ID) < 8 {
		return fmt.Errorf("native UUID lacks distinguishing suffix")
	}
	state := child.State
	if child.Paused {
		state = "paused"
	}
	if row != "> "+state+" "+child.Name+" #"+child.ID[len(child.ID)-8:] {
		return fmt.Errorf("painted row differs from admitted child identity")
	}
	fields, err := parsePanelDetail(detail)
	if err != nil {
		return err
	}
	for key, expected := range map[string]string{"UUID": child.ID, "Name": child.Name, "Type": child.Kind, "Model": child.Model, "Cwd": child.Cwd, "State": state, "Admission": "ready"} {
		if fields[key] != expected {
			return fmt.Errorf("painted %s differs from independent child expectation", key)
		}
	}
	return nil
}
