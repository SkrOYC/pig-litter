package piglitter

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestRootDiscoveryReloadsDefinitionsAndKeepsTreeBudgets(t *testing.T) {
	directory := t.TempDir()
	config := defaultConfig()
	tree := testTree(t, config)
	withCompletionHost(t, tree, func(o *owner) {
		o.cwd = directory
		if err := os.WriteFile(filepath.Join(directory, "litter.yaml"), []byte("concurrency: 1\nagents:\n  later:\n    role: scout\n    instructions: Discovered after owner initialization.\n    tools: [read]\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		ticket := testCall(t, o, nil, "fresh-list", "litter_list")
		value, err := o.operation(context.Background(), json.RawMessage(`{}`), ticket, nil)
		if err != nil {
			t.Fatal(err)
		}
		definitions := value["availableAgentTypes"].([]map[string]any)
		found := false
		for _, definition := range definitions {
			if definition["name"] == "later" && definition["description"] == "Discovered after owner initialization." {
				found = true
			}
		}
		if !found || tree.config.Concurrency != config.Concurrency || o.config.Concurrency != config.Concurrency {
			t.Fatalf("discovery stayed stale or changed tree budgets: %#v concurrency=%d", definitions, tree.config.Concurrency)
		}
	}, func(method string, _ json.RawMessage) json.RawMessage {
		if method != "sessionRead" {
			t.Fatalf("unexpected host call %s", method)
		}
		return json.RawMessage(`"fixture"`)
	})
}
