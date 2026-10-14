package main

import (
	"fmt"
	"sort"
)

type toolInventory struct {
	name     string
	required []string
	optional []string
}

var controlTools = []string{"litter_spawn", "litter_list", "litter_inspect", "litter_message", "litter_stop", "litter_wait"}

var toolInventories = map[string]toolInventory{
	"scout":           {name: "scout leaf", required: []string{"read"}, optional: []string{"ls"}},
	"coordinator":     {name: "read-only worker coordinator", required: append([]string{"read", "ls"}, controlTools...)},
	"worker":          {name: "full worker", required: append([]string{"read", "write", "edit", "bash"}, controlTools...), optional: []string{"ls"}},
	"worker_native":   {name: "native-settings worker", required: append([]string{"read", "ls", "write", "edit", "bash"}, controlTools...)},
	"worker_child":    {name: "full child worker", required: append([]string{"read", "ls", "write", "edit", "bash"}, controlTools...), optional: []string{"codemode", "tool_search"}},
	"worker_codemode": {name: "full worker with codemode", required: append([]string{"read", "write", "edit", "bash", "codemode"}, controlTools...)},
}

var fixtureModelInventories = map[string]string{
	"parent": "worker", "parent_cwd": "worker", "parent_unbounded": "worker", "parent_nested": "worker",
	"parent_checks": "worker", "parent_admission": "worker", "parent_shadow": "worker", "parent_messages": "worker", "parent_lifecycle": "worker", "parent_pause": "worker", "parent_completion": "worker", "parent_codemode": "worker_codemode",
	"parent_persistence": "worker", "persistence_child": "scout",
	"held": "scout", "lifecycle_child": "scout", "completion_child": "scout", "missing_file": "scout", "roles": "scout", "history": "scout", "message_child": "scout", "provider_error": "scout", "cwd_nested": "scout", "nested_sibling": "scout",
	"codemode_child": "scout",
	"denied":         "worker", "worker": "worker_child", "cwd_worker": "worker_native", "unbounded_child": "worker_child",
	"nester": "coordinator", "grandchild": "coordinator",
}

var fixtureModelIDs = []string{"parent", "held", "worker", "parent_cwd", "cwd_worker", "cwd_nested", "parent_unbounded", "unbounded_child", "parent_nested", "nester", "grandchild", "nested_sibling", "parent_checks", "parent_admission", "denied", "provider_error", "missing_file", "roles", "history", "parent_messages", "message_child", "parent_lifecycle", "parent_pause", "lifecycle_child", "parent_completion", "completion_child", "parent_codemode", "codemode_child", "parent_persistence", "persistence_child"}

func requireToolInventory(request modelRequest, inventory toolInventory) error {
	allowed := make(map[string]bool, len(inventory.required)+len(inventory.optional))
	for _, name := range inventory.required {
		allowed[name] = true
	}
	for _, name := range inventory.optional {
		allowed[name] = true
	}
	got := toolNames(request)
	for _, name := range inventory.required {
		if !contains(got, name) {
			return fmt.Errorf("%s tool inventory is missing %s: %v", inventory.name, name, got)
		}
	}
	for _, name := range got {
		if !allowed[name] {
			return fmt.Errorf("%s tool inventory contains unexpected %s: %v", inventory.name, name, got)
		}
	}
	return nil
}

func requireFixtureModelTools(request modelRequest) error {
	profileName, exists := fixtureModelInventories[request.Model]
	if !exists {
		return fmt.Errorf("fixture model %q has no tool inventory", request.Model)
	}
	profile, exists := toolInventories[profileName]
	if !exists {
		return fmt.Errorf("fixture model %q references unknown tool inventory %q", request.Model, profileName)
	}
	return requireToolInventory(request, profile)
}

func configuredFixtureModelIDs() []string {
	ids := append([]string(nil), fixtureModelIDs...)
	sort.Strings(ids)
	return ids
}
