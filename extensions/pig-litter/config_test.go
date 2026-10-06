package piglitter

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/coding"
)

func TestConfigParsesTypedLimitsAndLiteralAgents(t *testing.T) {
	config, err := ParseConfig([]byte("concurrency: 3 # active descendants\ndepth: 0\nmax_records: 5\nmax_mailbox: 8\nagents:\n  reviewer:\n    role: scout\n    tools: [read]\n    instructions: input.txt\n    can_delegate: false\n    model: fixture/reviewer\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Concurrency != 3 || config.Depth != 0 || config.MaxRecords != 5 {
		t.Fatalf("parsed limits %#v", config)
	}
	if got := config.Agents["reviewer"]; !reflect.DeepEqual(got, AgentDefinition{Role: "scout", Instructions: "input.txt", Tools: []string{"read"}, Model: "fixture/reviewer"}) {
		t.Fatalf("literal definition %#v", got)
	}
}

func TestConfiguredAgentDirectoryResolvesTheParentsExactModel(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIG_HOME", filepath.Join(dir, "pig-home"))
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	t.Setenv("PIG_USE_PI_DIRS", "0")
	t.Setenv("PIG_OFFLINE", "1")
	if err := os.WriteFile(filepath.Join(agentDir, "models.json"), []byte(`{"providers":{"fixture":{"api":"openai-completions","baseUrl":"http://127.0.0.1:1/v1","apiKey":"fixture-only","models":[{"id":"child","name":"child","contextWindow":32000,"maxTokens":1024}]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	trusted := true
	settings, err := memorySettings()
	if err != nil {
		t.Fatal(err)
	}
	services, err := coding.NewServices(coding.ServicesOptions{CWD: dir, AgentDir: configuredAgentDir(), ProjectTrusted: &trusted, SettingsManager: settings})
	if err != nil {
		t.Fatal(err)
	}
	defer services.Close()
	model := services.ModelRuntime().GetModel("fixture", "child")
	if model == nil || model.ID != "child" || services.AgentDir() != agentDir {
		t.Fatalf("parent model did not resolve from explicit agent directory %v %s", model, services.AgentDir())
	}
}

func TestConfigRejectsAmbiguousOrUnboundedDocuments(t *testing.T) {
	for _, source := range []string{
		"", " ", "---", "null", "[]", "{}\n---\n{}", "concurrency: 2\nconcurrency: 3", "worktrees: 1", "depth: NaN", "depth: .inf", "depth: 1.5", "depth: '2'", "depth: true", "concurrency: 9007199254740993", "concurrency: 999999999999999999999999999999", "concurrency: 5\nmax_records: 4", "max_history_bytes: 1024\nmax_result_bytes: 2048", "concurrency: 0", "agents: {scout: {role: scout, role: worker}}", "agents: {scout: {role: worker}}", "agents: {scout: {tools: [write]}}", "agents: {scout: {tools: [read, read]}}", "agents: {custom: {role: worker, can_delegate: true}}", "agents: &a {scout: {role: scout}}\ncopy: *a", "agents: {scout: {model: missing-provider}}", "agents: {scout: {can_delegate: yes}}", "agents: {scout: {instructions: null}}", strings.Repeat("x", 32769),
	} {
		t.Run(source[:min(len(source), 64)], func(t *testing.T) {
			if _, err := ParseConfig([]byte(source)); err == nil {
				t.Fatalf("accepted %q", source)
			}
		})
	}
}

func TestConfigFileRejectsSymlinkDirectoryAndSize(t *testing.T) {
	dir := t.TempDir()
	config, err := LoadConfig(dir)
	if err != nil || config.Concurrency != 4 {
		t.Fatalf("missing config %#v %v", config, err)
	}
	path := filepath.Join(dir, "litter.yaml")
	if err := os.WriteFile(path, []byte("concurrency: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadConfig(dir)
	if err != nil || config.Concurrency != 1 {
		t.Fatalf("regular config %#v %v", config, err)
	}
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 32769)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("accepted oversized config")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("accepted directory")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("concurrency: 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, path); err != nil {
		t.Skip(err)
	}
	if _, err := LoadConfig(dir); err == nil {
		t.Fatal("accepted regular-file symlink")
	}
}
