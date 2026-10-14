package piglitter

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestToolPatternsExpandFromRefreshedRootInventory(t *testing.T) {
	config, err := ParseConfig([]byte("agents:\n  worker:\n    tools: [read, ls, 'skill:*', 'mcp__docs__*', codemode, tool_search, project_probe]\n"))
	if err != nil {
		t.Fatal(err)
	}
	tree := testTree(t, config)
	names := []string{"read", "ls", "codemode", "tool_search", "skill:review", "mcp__docs__search", "project_probe"}
	withCompletionHost(t, tree, func(o *owner) {
		root, err := o.rootToolNames()
		if err != nil {
			t.Fatal(err)
		}
		if got := expandTools(config.Agents["worker"].Tools, root); !reflect.DeepEqual(got, []string{"codemode", "ls", "mcp__docs__search", "project_probe", "read", "skill:review", "tool_search"}) {
			t.Fatalf("root glob expansion = %v", got)
		}
		names = append(names, "mcp__docs__late")
		root, err = o.rootToolNames()
		if err != nil {
			t.Fatal(err)
		}
		if got := expandTools(config.Agents["worker"].Tools, root); !reflect.DeepEqual(got, []string{"codemode", "ls", "mcp__docs__late", "mcp__docs__search", "project_probe", "read", "skill:review", "tool_search"}) {
			t.Fatalf("late root tool was not reachable after refresh: %v", got)
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method != "getAllTools" {
			t.Fatalf("unexpected enumeration call %s", method)
		}
		tools := []map[string]any{{"name": "mcp__docs__hidden", "exposure": "hidden"}}
		for _, name := range names {
			tools = append(tools, map[string]any{"name": name, "exposure": "codemode"})
		}
		raw, _ := json.Marshal(map[string]any{"tools": tools})
		return raw
	})
}

func TestNestedGlobsNarrowEveryParentAndNewRootTools(t *testing.T) {
	config := defaultConfig()
	config.Depth = 3
	tree := testTree(t, config)
	root := []string{"read", "ls", "mcp__docs__search", "codemode", "tool_search"}
	definition := AgentDefinition{Role: "worker", Instructions: "coordinate", Tools: []string{"mcp__docs__*"}, CanDelegate: true}
	parent, selected, err := tree.admit(admission{kind: "coordinator", name: "parent", agent: definition, available: root})
	if err != nil {
		t.Fatal(err)
	}
	definition.Tools = []string{"mcp__*"}
	_, nested, err := tree.admit(admission{parentRun: selected, kind: "coordinator", name: "nested", agent: definition, available: root})
	if err != nil {
		t.Fatal(err)
	}
	root = append(root, "mcp__other__write")
	if _, _, err := tree.admit(admission{parentRun: nested, kind: "coordinator", name: "denied", agent: definition, available: root}); err == nil || err.(*treeError).code != "permission_denied" {
		t.Fatalf("third-level globs widened past grandparent %s: %v", parent.ID, err)
	}
	definition.Tools = []string{"mcp__docs__*"}
	child, _, err := tree.admit(admission{parentRun: nested, kind: "coordinator", name: "narrowed", agent: definition, available: root})
	if err != nil || child.Depth != 3 {
		t.Fatalf("narrowed third-level policy failed: %#v %v", child, err)
	}
}

func TestScoutPatternsCannotGrantOtherTools(t *testing.T) {
	for _, tool := range []string{"write", "codemode", "tool_search", "mcp__*", "skill:*", "r*", "*", "["} {
		if _, err := ParseConfig([]byte("agents: {scout: {tools: ['" + tool + "']}}")); err == nil {
			t.Errorf("scout accepted tool pattern %q", tool)
		}
	}
	config, err := ParseConfig([]byte("agents: {scout: {tools: [read, ls]}}"))
	if err != nil || !reflect.DeepEqual(expandTools(config.Agents["scout"].Tools, []string{"read", "ls", "write", "mcp__docs__search", "codemode"}), []string{"ls", "read"}) {
		t.Fatalf("scout lost its read-only tool set: %#v %v", config, err)
	}
}
