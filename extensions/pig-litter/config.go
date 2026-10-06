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
	Concurrency     int
	Depth           int
	MaxRecords      int
	MaxHistoryBytes int
	MaxResultBytes  int
	MaxMailbox      int
	MaxRunMillis    int
	MaxTurns        int
	Agents          map[string]AgentDefinition
}

var roleTools = map[string][]string{"scout": {"read", "ls"}, "worker": {"read", "ls", "write", "edit"}}
var agentName = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
var exactModel = regexp.MustCompile(`^[^/\s]+/[^\s]+$`)

func defaultConfig() Config {
	return Config{Concurrency: 4, Depth: 2, MaxRecords: 64, MaxHistoryBytes: 1024 * 1024, MaxResultBytes: 8192, MaxMailbox: 8, MaxRunMillis: 120000, MaxTurns: 20,
		Agents: map[string]AgentDefinition{
			"scout":  {Role: "scout", Instructions: "Inspect files for the task. Do not change files. Report relevant paths and uncertainty.", Tools: []string{"read"}, CanDelegate: true},
			"worker": {Role: "worker", Instructions: "Complete the task with permitted file tools. Preserve unrelated changes. Report changes and checks.", Tools: []string{"read", "write", "edit"}, CanDelegate: true},
		},
	}
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
	config := defaultConfig()
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
		"max_records": {&config.MaxRecords, 1, 4096}, "max_history_bytes": {&config.MaxHistoryBytes, 1024, 64 * 1024 * 1024},
		"max_result_bytes": {&config.MaxResultBytes, 1, 1024 * 1024}, "max_mailbox": {&config.MaxMailbox, 1, 1024},
		"max_run_millis": {&config.MaxRunMillis, 1000, 60 * 60 * 1000}, "max_turns": {&config.MaxTurns, 1, 128},
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
	if config.MaxRecords < config.Concurrency {
		return Config{}, fmt.Errorf("max_records must be at least concurrency")
	}
	if config.MaxHistoryBytes < config.MaxResultBytes {
		return Config{}, fmt.Errorf("max_history_bytes must be at least max_result_bytes")
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
			definition, bundled := config.Agents[name]
			for field := range fields {
				if !slices.Contains([]string{"role", "instructions", "tools", "can_delegate", "model"}, field) {
					return Config{}, fmt.Errorf("agents.%s has unknown setting %s", name, field)
				}
			}
			if !bundled {
				for _, field := range []string{"role", "instructions", "tools", "can_delegate"} {
					if fields[field] == nil {
						return Config{}, fmt.Errorf("agents.%s needs %s", name, field)
					}
				}
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
			originalRole := definition.Role
			for _, field := range []struct {
				key   string
				value *string
			}{{"role", &definition.Role}, {"instructions", &definition.Instructions}, {"model", &definition.Model}} {
				if err := readString(field.key, field.value); err != nil {
					return Config{}, err
				}
			}
			if _, exists := roleTools[definition.Role]; !exists || bundled && originalRole != definition.Role {
				return Config{}, fmt.Errorf("agents.%s has invalid role", name)
			}
			if strings.TrimSpace(definition.Instructions) == "" || len(definition.Instructions) > 4096 {
				return Config{}, fmt.Errorf("agents.%s needs 1 to 4096 bytes of instructions", name)
			}
			if node := fields["can_delegate"]; node != nil {
				if node.Tag != "!!bool" || node.Decode(&definition.CanDelegate) != nil {
					return Config{}, fmt.Errorf("agents.%s can_delegate must be a boolean", name)
				}
			}
			if node := fields["tools"]; node != nil {
				if node.Kind != yaml.SequenceNode {
					return Config{}, fmt.Errorf("agents.%s tools must be a sequence", name)
				}
				definition.Tools = []string{}
				for _, tool := range node.Content {
					if tool.Tag != "!!str" || !slices.Contains(roleTools[definition.Role], tool.Value) || slices.Contains(definition.Tools, tool.Value) {
						return Config{}, fmt.Errorf("agents.%s tools must be distinct %s file tools", name, definition.Role)
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

func LoadConfig(cwd string) (Config, error) {
	path := filepath.Join(cwd, "litter.yaml")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return defaultConfig(), nil
	}
	if err != nil {
		return Config{}, err
	}
	if !info.Mode().IsRegular() {
		return Config{}, fmt.Errorf("litter.yaml must be a regular file without symlinks")
	}
	if info.Size() > configBytes {
		return Config{}, fmt.Errorf("litter.yaml exceeds 32768 bytes")
	}
	file, err := openConfig(path)
	if err != nil {
		return Config{}, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return Config{}, err
	}
	if !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return Config{}, fmt.Errorf("litter.yaml must remain the same regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, configBytes+1))
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(data)
}
