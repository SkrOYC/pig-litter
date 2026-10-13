package piglitter

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/coding"
)

func isolatedConfigHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	for _, key := range []string{"PIG_HOME", "XDG_CONFIG_HOME", "PIG_CODING_AGENT_DIR", "PI_CODING_AGENT_DIR", "PIG_USE_PI_DIRS"} {
		t.Setenv(key, "")
	}
	if actual, err := os.UserHomeDir(); err != nil || actual != home {
		t.Fatalf("isolated user home = %q, %v; want %q", actual, err, home)
	}
	return home
}

func TestLoadConfigLayersStandardDefaultActiveAndProjectFiles(t *testing.T) {
	home := isolatedConfigHome(t)
	standard := filepath.Join(home, ".config", "pig", "agent")
	active := filepath.Join(home, ".pig", "agent")
	project := t.TempDir()
	for _, directory := range []string{standard, active} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(standard, "litter.yaml"), []byte("concurrency: 3\nagents:\n  personal:\n    role: scout\n    instructions: standard user instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := LoadConfig(project)
	if err != nil || config.Concurrency != 3 || config.Agents["personal"].Instructions != "standard user instructions" {
		t.Fatalf("standard user defaults %#v: %v", config, err)
	}
	if configuredAgentDir() != active {
		t.Fatal("loading standard litter defaults changed the SDK agent directory")
	}
	if err := os.WriteFile(filepath.Join(active, "litter.yaml"), []byte("concurrency: 5\nagents:\n  personal:\n    tools: [read, ls]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadConfig(project)
	if err != nil || config.Concurrency != 5 || config.Agents["personal"].Instructions != "standard user instructions" || !reflect.DeepEqual(config.Agents["personal"].Tools, []string{"read", "ls"}) {
		t.Fatalf("active agent directory overlay %#v: %v", config, err)
	}
	if err := os.WriteFile(filepath.Join(project, "litter.yaml"), []byte("concurrency: 2\nagents:\n  personal:\n    instructions: project instructions\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err = LoadConfig(project)
	if err != nil || config.Concurrency != 2 || config.Agents["personal"].Instructions != "project instructions" || !reflect.DeepEqual(config.Agents["personal"].Tools, []string{"read", "ls"}) {
		t.Fatalf("project overlay %#v: %v", config, err)
	}
	if err := os.WriteFile(filepath.Join(standard, "litter.yaml"), []byte("unknown_setting: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(project); err == nil || !strings.Contains(err.Error(), filepath.Join(standard, "litter.yaml")) {
		t.Fatalf("malformed standard user file was not rejected with its path: %v", err)
	}
}

func TestLoadConfigExplicitDirectoriesSkipUnrelatedUserDefaults(t *testing.T) {
	for _, selection := range []string{"PIG_HOME", "XDG_CONFIG_HOME", "PIG_CODING_AGENT_DIR", "shared default", "shared override"} {
		t.Run(selection, func(t *testing.T) {
			home := isolatedConfigHome(t)
			for _, directory := range []string{filepath.Join(home, ".config", "pig", "agent"), filepath.Join(home, ".pig", "agent")} {
				if err := os.MkdirAll(directory, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(directory, "litter.yaml"), []byte("unrelated_invalid_config: true\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			chosen := t.TempDir()
			active := chosen
			switch selection {
			case "PIG_HOME":
				t.Setenv(selection, chosen)
				active = filepath.Join(chosen, "agent")
			case "XDG_CONFIG_HOME":
				t.Setenv(selection, chosen)
				active = filepath.Join(chosen, "pig", "agent")
			case "PIG_CODING_AGENT_DIR":
				t.Setenv(selection, chosen)
			case "shared default":
				t.Setenv("PIG_USE_PI_DIRS", "1")
				active = filepath.Join(home, ".pi", "agent")
			case "shared override":
				t.Setenv("PIG_USE_PI_DIRS", "1")
				t.Setenv("PI_CODING_AGENT_DIR", chosen)
			}
			if err := os.MkdirAll(active, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(active, "litter.yaml"), []byte("concurrency: 6\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			config, err := LoadConfig(t.TempDir())
			if err != nil || config.Concurrency != 6 {
				t.Fatalf("explicit directory selection %#v: %v", config, err)
			}
		})
	}
}

func TestLoadConfigFollowsExpandedAndSharedAgentDirectory(t *testing.T) {
	isolatedConfigHome(t)
	userDir := t.TempDir()
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(userDir, "litter.yaml"), []byte("concurrency: 6\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(home, userDir)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIG_USE_PI_DIRS", "0")
	t.Setenv("PIG_CODING_AGENT_DIR", "~/"+filepath.ToSlash(relative))
	config, err := LoadConfig(project)
	if err != nil || config.Concurrency != 6 {
		t.Fatalf("expanded agent directory config %#v: %v", config, err)
	}
	shared := t.TempDir()
	if err := os.WriteFile(filepath.Join(shared, "litter.yaml"), []byte("concurrency: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIG_USE_PI_DIRS", "1")
	t.Setenv("PI_CODING_AGENT_DIR", shared)
	config, err = LoadConfig(project)
	if err != nil || config.Concurrency != 7 {
		t.Fatalf("shared agent directory config %#v: %v", config, err)
	}
}

func TestConfigParsesTypedLimitsAndLiteralAgents(t *testing.T) {
	config, err := ParseConfig([]byte("concurrency: 3 # active descendants\ndepth: 0\nmax_records: 5\nmax_mailbox: 12\nmax_turns: 37\nagents:\n  reviewer:\n    role: scout\n    tools: [read]\n    instructions: input.txt\n    can_delegate: false\n    model: fixture/reviewer\n"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Concurrency != 3 || config.Depth != 0 || config.MaxRecords != 5 || config.MaxMailbox != 12 || config.MaxTurns != 37 {
		t.Fatalf("parsed limits %#v", config)
	}
	if got := config.Agents["reviewer"]; !reflect.DeepEqual(got, AgentDefinition{Role: "scout", Instructions: "input.txt", Tools: []string{"read"}, Model: "fixture/reviewer"}) {
		t.Fatalf("literal definition %#v", got)
	}
}

func TestConfigRoleDefaultsAllowBuiltInRoleChangesAndCustomInheritance(t *testing.T) {
	config, err := ParseConfig([]byte("agents:\n  scout:\n    role: worker\n    tools: [bash]\n  worker:\n    role: scout\n  reader:\n    role: scout\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got := config.Agents["scout"]; got.Role != "worker" || got.Instructions != "Complete the task with permitted tools. Preserve unrelated changes. Report changes and checks." || !reflect.DeepEqual(got.Tools, []string{"bash"}) || !got.CanDelegate {
		t.Fatalf("changed built-in scout %#v", got)
	}
	if got := config.Agents["worker"]; got.Role != "scout" || got.Instructions != "Inspect files for the task. Do not change files. Report relevant paths and uncertainty." || !reflect.DeepEqual(got.Tools, []string{"read"}) || got.CanDelegate {
		t.Fatalf("changed built-in worker %#v", got)
	}
	if got := config.Agents["reader"]; got.Role != "scout" || !reflect.DeepEqual(got.Tools, []string{"read"}) || got.CanDelegate {
		t.Fatalf("custom role defaults %#v", got)
	}
	if got := defaultConfig().Agents["worker"]; !slices.Contains(got.Tools, "bash") {
		t.Fatalf("worker role default did not include bash %#v", got.Tools)
	}
}

func TestLoadConfigOverlaysAgentDirectoryThenProject(t *testing.T) {
	dir := t.TempDir()
	agentDir := filepath.Join(dir, "agent")
	projectDir := filepath.Join(dir, "project")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(projectDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
	if err := os.WriteFile(filepath.Join(agentDir, "litter.yaml"), []byte("agents:\n  analyst:\n    role: worker\n    instructions: user instructions\n    tools: [read, bash]\n    can_delegate: true\n    model: fixture/analyst\n  worker:\n    role: scout\n    model: fixture/worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(projectDir, "litter.yaml"), []byte("agents:\n  analyst:\n    instructions: project instructions\n    can_delegate: false\n  worker:\n    role: worker\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := LoadConfig(projectDir)
	if err != nil {
		t.Fatal(err)
	}
	if got := config.Agents["analyst"]; got.Role != "worker" || got.Instructions != "project instructions" || !reflect.DeepEqual(got.Tools, []string{"read", "bash"}) || got.CanDelegate || got.Model != "fixture/analyst" {
		t.Fatalf("project patch did not preserve user definition fields %#v", got)
	}
	if got := config.Agents["worker"]; got.Role != "worker" || got.Instructions != "Complete the task with permitted tools. Preserve unrelated changes. Report changes and checks." || !reflect.DeepEqual(got.Tools, []string{"read", "write", "edit", "bash"}) || !got.CanDelegate || got.Model != "fixture/worker" {
		t.Fatalf("role change did not reset role defaults and inherit model %#v", got)
	}
}

func TestLoadConfigUsesCodingDefaultAgentDirectory(t *testing.T) {
	dir := t.TempDir()
	pigHome := filepath.Join(dir, "pig-home")
	agentDir := filepath.Join(pigHome, "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIG_CODING_AGENT_DIR", "")
	t.Setenv("PIG_HOME", pigHome)
	t.Setenv("PIG_USE_PI_DIRS", "0")
	if err := os.WriteFile(filepath.Join(agentDir, "litter.yaml"), []byte("concurrency: 7\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	config, err := LoadConfig(filepath.Join(dir, "project"))
	if err != nil {
		t.Fatal(err)
	}
	if config.Concurrency != 7 {
		t.Fatalf("did not load SDK default agent directory config %#v", config)
	}
}

func TestLoadConfigRejectsMalformedLayer(t *testing.T) {
	for _, malformed := range []string{"agent", "project"} {
		t.Run(malformed, func(t *testing.T) {
			dir := t.TempDir()
			agentDir := filepath.Join(dir, "agent")
			projectDir := filepath.Join(dir, "project")
			if err := os.MkdirAll(agentDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(projectDir, 0o700); err != nil {
				t.Fatal(err)
			}
			t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
			contents := map[string]string{
				"agent":   "concurrency: 2\n",
				"project": "max_mailbox: 10\n",
			}
			contents[malformed] = "max_run_millis: 120000\n"
			if err := os.WriteFile(filepath.Join(agentDir, "litter.yaml"), []byte(contents["agent"]), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(projectDir, "litter.yaml"), []byte(contents["project"]), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadConfig(projectDir); err == nil {
				t.Fatalf("accepted malformed %s layer", malformed)
			}
		})
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
	services, err := coding.NewServices(coding.ServicesOptions{CWD: dir, AgentDir: configuredAgentDir(), ProjectTrusted: &trusted})
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
		"", " ", "---", "null", "[]", "{}\n---\n{}", "concurrency: 2\nconcurrency: 3", "worktrees: 1", "max_run_millis: 120000", "depth: NaN", "depth: .inf", "depth: 1.5", "depth: '2'", "depth: true", "concurrency: 9007199254740993", "concurrency: 999999999999999999999999999999", "concurrency: 5\nmax_records: 4", "max_history_bytes: 1024\nmax_result_bytes: 2048", "concurrency: 0", "agents: {scout: {role: scout, role: worker}}", "agents: {scout: {tools: [write]}}", "agents: {scout: {tools: [read, read]}}", "agents: {custom: {}}", "agents: {custom: {can_delegate: true}}", "agents: &a {scout: {role: scout}}\ncopy: *a", "agents: {scout: {model: missing-provider}}", "agents: {scout: {can_delegate: yes}}", "agents: {scout: {instructions: null}}", "agents: {custom: {role: scout, can_delegate: true}}", "agents: {scout: {role: scout, can_delegate: true}}", strings.Repeat("x", 32769),
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
	agentDir := filepath.Join(dir, "agent")
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PIG_CODING_AGENT_DIR", agentDir)
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
