package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func TestMain(m *testing.M) {
	if marker := os.Getenv("LITTER_AMBIENT_EXTENSION_PROBE"); marker != "" && os.Getenv("PIG_EXT_SOCKET") != "" {
		if err := os.WriteFile(marker, []byte("started"), 0o600); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		name := filepath.Base(os.Args[0])
		if err := sdk.New(strings.TrimSuffix(name, filepath.Ext(name))).Run(); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func TestChildBedrockProviderIsLinked(t *testing.T) {
	provider, err := ai.NewOptionalAPIProvider(ai.APIBedrockConverseStream, ai.Model{ProviderMeta: ai.ProviderMetadata{ProviderID: "amazon-bedrock"}})
	if err != nil || provider == nil || provider.ID() != "amazon-bedrock" {
		t.Fatalf("Bedrock provider is unavailable: %v %v", provider, err)
	}
}

func TestChildToolCeilingTreatsRootNamesLiterally(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	t.Setenv("PIG_HOME", directory)
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	if err := os.WriteFile(filepath.Join(directory, "litter.yaml"), []byte("agents:\n  worker:\n    tools: ['wr*']\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	declared := make(chan []string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if requests.Add(1) == 1 {
			var body struct {
				Tools []struct {
					Function struct{ Name string } `json:"function"`
				} `json:"tools"`
			}
			if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			var names []string
			for _, tool := range body.Tools {
				names = append(names, tool.Function.Name)
			}
			declared <- names
			runtimeToolAnswer(w, "excluded-write", "write", map[string]any{"path": "excluded.txt", "content": "unexpected write"})
			return
		}
		runtimeAnswer(w, "LITERAL_TOOL_CEILING")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	host := &runtimeHost{tools: []string{"wr*"}}
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "literal-spawn", map[string]any{"type": "worker", "task": "try the write tool", "model": "fixture/child"})
		if err != nil {
			t.Fatal(err)
		}
		o.settleAck(ticket, false)
		selected := testRun(t, tree, Ref{value["child"].(Snapshot).ID})
		outcome, err := tree.wait(context.Background(), selected, 15000)
		if err != nil || outcome.Text != "LITERAL_TOOL_CEILING" {
			t.Fatalf("child did not finish the rejected write: %#v %v", outcome, err)
		}
		tree.acknowledge(selected)
	})
	if names := <-declared; slices.Contains(names, "write") {
		t.Fatalf("root name wr* admitted the write tool: %v", names)
	}
	if _, err := os.Stat(filepath.Join(directory, "excluded.txt")); !os.IsNotExist(err) {
		t.Fatalf("the child reached an unlisted write tool: %v", err)
	}
}

func TestChildResourceLoaderDoesNotStartAmbientExtensions(t *testing.T) {
	t.Setenv("PIG_OFFLINE", "1")
	t.Setenv("PIG_USE_PI_DIRS", "0")
	directory := t.TempDir()
	agentDir := filepath.Join(directory, "agent")
	childDir := filepath.Join(directory, "child")
	marker := filepath.Join(directory, "ambient-started")
	t.Setenv("PIG_HOME", directory)
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	t.Setenv("LITTER_AMBIENT_EXTENSION_PROBE", marker)
	skillDir := filepath.Join(childDir, ".pig", "skills", "probe")
	if err := os.MkdirAll(skillDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte("---\nname: probe\ndescription: CHILD_RESOURCE_SKILL\n---\nRead this skill.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var skillSeen atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Error(err)
		}
		skillSeen.Store(strings.Contains(string(body), "CHILD_RESOURCE_SKILL"))
		runtimeAnswer(w, "CHILD_RESOURCE_COMPLETE")
	}))
	defer server.Close()
	writeRuntimeModels(t, agentDir, server.URL)
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	settings, err := json.Marshal(map[string]any{"extensions": []string{executable}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentDir, "settings.json"), settings, 0o600); err != nil {
		t.Fatal(err)
	}
	host := &runtimeHost{tools: []string{"read"}}
	tree := testTree(t, defaultConfig())
	withRuntimeOwner(t, host, tree, directory, agentDir, func(o *owner) {
		value, ticket, err := runtimeOperation(t, o, "litter_spawn", "resource-spawn", map[string]any{"type": "worker", "task": "read the child skill", "model": "fixture/child", "cwd": childDir})
		if err != nil {
			t.Fatal(err)
		}
		o.settleAck(ticket, false)
		selected := testRun(t, tree, Ref{value["child"].(Snapshot).ID})
		outcome, err := tree.wait(context.Background(), selected, 15000)
		if err != nil || outcome.State != Completed || outcome.Text != "CHILD_RESOURCE_COMPLETE" {
			t.Fatalf("child did not complete: %#v %v", outcome, err)
		}
		tree.acknowledge(selected)
	})
	if !skillSeen.Load() {
		t.Fatal("child did not discover its skill")
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("child started the ambient extension: %v", err)
	}
	loader := coding.NewDefaultResourceLoader(coding.DefaultResourceLoaderOptions{CWD: childDir, AgentDir: agentDir})
	defer loader.Close()
	if err := loader.Reload(); err != nil {
		t.Fatal(err)
	}
	if result := loader.GetExtensions(); len(result.Errors) != 0 || len(result.Extensions) != 1 {
		t.Fatalf("control loader did not start the ambient extension: %#v", result)
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "started" {
		t.Fatalf("ambient extension control did not run: %q %v", data, err)
	}
}
