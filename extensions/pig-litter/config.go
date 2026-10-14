package piglitter

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

const configBytes = 32768

type AgentDefinition struct {
	Role         string   `json:"role"`
	Instructions string   `json:"instructions"`
	Tools        []string `json:"tools"`
	CanDelegate  bool     `json:"canDelegate"`
	Model        string   `json:"model,omitempty"`
}

type Config struct {
	Concurrency    int
	Depth          int
	MaxResultBytes int
	MaxMailbox     int
	MaxTurns       int
	Agents         map[string]AgentDefinition
}

var roleDefaults = map[string]AgentDefinition{
	"scout":  {Role: "scout", Instructions: "Inspect files for the task. Do not change files. Report relevant paths and uncertainty.", Tools: []string{"read", "ls"}},
	"worker": {Role: "worker", Instructions: "Complete the task with permitted tools. Preserve unrelated changes. Report changes and checks.", Tools: []string{"read", "ls", "write", "edit", "bash", "skill:*", "mcp__*", "codemode", "tool_search"}, CanDelegate: true},
}

var agentName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var exactModel = regexp.MustCompile(`^[^/\s]+/[^\s]+$`)

func agentDefaults(role string) (AgentDefinition, bool) {
	definition, ok := roleDefaults[role]
	definition.Tools = slices.Clone(definition.Tools)
	return definition, ok
}

func defaultConfig() Config {
	scout, _ := agentDefaults("scout")
	worker, _ := agentDefaults("worker")
	return Config{Concurrency: 4, Depth: 2, MaxResultBytes: 50000, MaxMailbox: 8, MaxTurns: 50,
		Agents: map[string]AgentDefinition{"scout": scout, "worker": worker},
	}
}

func cloneConfig(config Config) Config {
	clone := config
	clone.Agents = make(map[string]AgentDefinition, len(config.Agents))
	for name, definition := range config.Agents {
		definition.Tools = slices.Clone(definition.Tools)
		clone.Agents[name] = definition
	}
	return clone
}

func mapping(node *yaml.Node, label string) (map[string]*yaml.Node, error) {
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("%s must be a mapping", label)
	}
	out := make(map[string]*yaml.Node)
	for i := 0; i < len(node.Content); i += 2 {
		key := node.Content[i]
		if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
			return nil, fmt.Errorf("%s keys must be strings", label)
		}
		if _, exists := out[key.Value]; exists {
			return nil, fmt.Errorf("%s repeats setting %s", label, key.Value)
		}
		out[key.Value] = node.Content[i+1]
	}
	return out, nil
}

func validateYAML(node *yaml.Node) error {
	if node.Kind == yaml.AliasNode || node.Anchor != "" {
		return fmt.Errorf("litter.yaml does not permit anchors or aliases")
	}
	if node.Kind == yaml.MappingNode {
		if _, err := mapping(node, "litter.yaml"); err != nil {
			return err
		}
	}
	for _, child := range node.Content {
		if err := validateYAML(child); err != nil {
			return err
		}
	}
	return nil
}

func ParseConfig(source []byte) (Config, error) {
	return parseConfig(defaultConfig(), source)
}

func parseConfig(base Config, source []byte) (Config, error) {
	config := cloneConfig(base)
	if len(source) > configBytes {
		return Config{}, fmt.Errorf("litter.yaml exceeds 32768 bytes")
	}
	decoder := yaml.NewDecoder(bytes.NewReader(source))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return Config{}, fmt.Errorf("litter.yaml must contain one mapping: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, fmt.Errorf("litter.yaml must contain one document")
	}
	if len(document.Content) != 1 {
		return Config{}, fmt.Errorf("litter.yaml must contain one mapping")
	}
	if err := validateYAML(&document); err != nil {
		return Config{}, err
	}
	raw, err := mapping(document.Content[0], "litter.yaml")
	if err != nil {
		return Config{}, err
	}
	limits := map[string]struct {
		value    *int
		min, max int
	}{
		"concurrency": {&config.Concurrency, 1, 16}, "depth": {&config.Depth, 0, 8},
		"max_result_bytes": {&config.MaxResultBytes, 1, 1024 * 1024}, "max_mailbox": {&config.MaxMailbox, 1, 1024},
		"max_turns": {&config.MaxTurns, 1, int(^uint(0) >> 1)},
	}
	for name, node := range raw {
		if name == "agents" {
			continue
		}
		limit, exists := limits[name]
		if !exists {
			return Config{}, fmt.Errorf("litter.yaml has unknown setting %s", name)
		}
		var number int64
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" || node.Decode(&number) != nil || number < int64(limit.min) || number > int64(limit.max) {
			return Config{}, fmt.Errorf("litter.yaml %s must be an integer from %d to %d", name, limit.min, limit.max)
		}
		*limit.value = int(number)
	}
	if node := raw["agents"]; node != nil {
		agents, err := mapping(node, "agents")
		if err != nil {
			return Config{}, err
		}
		for name, node := range agents {
			if !agentName.MatchString(name) {
				return Config{}, fmt.Errorf("invalid agent name %s", name)
			}
			fields, err := mapping(node, "agents."+name)
			if err != nil {
				return Config{}, err
			}
			for field := range fields {
				if !slices.Contains([]string{"role", "instructions", "tools", "can_delegate", "model"}, field) {
					return Config{}, fmt.Errorf("agents.%s has unknown setting %s", name, field)
				}
			}

			definition, exists := config.Agents[name]
			role := definition.Role
			if !exists {
				role = ""
			}
			if roleNode := fields["role"]; roleNode != nil {
				if roleNode.Kind != yaml.ScalarNode || roleNode.Tag != "!!str" {
					return Config{}, fmt.Errorf("agents.%s role must be a string", name)
				}
				role = roleNode.Value
			}
			if role == "" {
				return Config{}, fmt.Errorf("agents.%s needs role", name)
			}
			roleDefinition, validRole := agentDefaults(role)
			if !validRole {
				return Config{}, fmt.Errorf("agents.%s has invalid role", name)
			}
			if !exists || definition.Role != role {
				model := definition.Model
				definition = roleDefinition
				definition.Model = model
			} else {
				definition.Role = role
			}

			readString := func(key string, target *string) error {
				if node := fields[key]; node != nil {
					if node.Tag != "!!str" || node.Kind != yaml.ScalarNode {
						return fmt.Errorf("agents.%s %s must be a string", name, key)
					}
					*target = node.Value
				}
				return nil
			}
			for _, field := range []struct {
				key   string
				value *string
			}{{"instructions", &definition.Instructions}, {"model", &definition.Model}} {
				if err := readString(field.key, field.value); err != nil {
					return Config{}, err
				}
			}
			if strings.TrimSpace(definition.Instructions) == "" || len(definition.Instructions) > 4096 {
				return Config{}, fmt.Errorf("agents.%s needs 1 to 4096 bytes of instructions", name)
			}
			if node := fields["can_delegate"]; node != nil {
				if node.Tag != "!!bool" || node.Decode(&definition.CanDelegate) != nil {
					return Config{}, fmt.Errorf("agents.%s can_delegate must be a boolean", name)
				}
			}
			if definition.Role == "scout" && definition.CanDelegate {
				return Config{}, fmt.Errorf("agents.%s scout role cannot delegate", name)
			}
			if node := fields["tools"]; node != nil {
				if node.Kind != yaml.SequenceNode {
					return Config{}, fmt.Errorf("agents.%s tools must be a sequence", name)
				}
				definition.Tools = []string{}
				for _, tool := range node.Content {
					if tool.Tag != "!!str" || !validToolPattern(tool.Value) || definition.Role == "scout" && tool.Value != "read" && tool.Value != "ls" || slices.Contains(definition.Tools, tool.Value) {
						return Config{}, fmt.Errorf("agents.%s tools must be distinct named tool patterns; scouts allow only read and ls", name)
					}
					definition.Tools = append(definition.Tools, tool.Value)
				}
			}
			if definition.Model != "" && !exactModel.MatchString(definition.Model) {
				return Config{}, fmt.Errorf("agents.%s model must be an exact provider/model", name)
			}
			config.Agents[name] = definition
		}
	}
	return config, nil
}

func loadConfigLayer(config Config, path string) (Config, error) {
	loadError := func(err error) (Config, error) {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return config, nil
	}
	if err != nil {
		return loadError(err)
	}
	if !info.Mode().IsRegular() {
		return loadError(fmt.Errorf("litter.yaml must be a regular file without symlinks"))
	}
	if info.Size() > configBytes {
		return loadError(fmt.Errorf("litter.yaml exceeds 32768 bytes"))
	}
	file, err := openConfig(path)
	if err != nil {
		return loadError(err)
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return loadError(err)
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return loadError(fmt.Errorf("litter.yaml must remain the same regular file"))
	}
	data, err := io.ReadAll(io.LimitReader(file, configBytes+1))
	if err != nil {
		return loadError(err)
	}
	config, err = parseConfig(config, data)
	if err != nil {
		return loadError(err)
	}
	return config, nil
}

func LoadConfig(cwd string) (Config, error) {
	config := defaultConfig()
	agentDir := configuredAgentDir()
	directories := []string{}
	if os.Getenv("PIG_HOME") == "" && os.Getenv("XDG_CONFIG_HOME") == "" && os.Getenv("PIG_CODING_AGENT_DIR") == "" && os.Getenv("PIG_USE_PI_DIRS") != "1" {
		home, err := os.UserHomeDir()
		if err != nil {
			return Config{}, err
		}
		directories = append(directories, filepath.Join(home, ".config", "pig", "agent"))
	}
	if agentDir != "" && !slices.Contains(directories, agentDir) {
		directories = append(directories, agentDir)
	}
	for _, directory := range directories {
		var err error
		config, err = loadConfigLayer(config, filepath.Join(directory, "litter.yaml"))
		if err != nil {
			return Config{}, err
		}
	}
	return loadConfigLayer(config, filepath.Join(cwd, "litter.yaml"))
}
