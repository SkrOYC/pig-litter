package piglitter

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path"
	"slices"
	"strings"
	"time"

	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/coding/extension/builtin"
	"github.com/MichaelKinsy/PiG/coding/extensionset"
	"github.com/MichaelKinsy/PiG/coding/mcpext"
	"github.com/MichaelKinsy/PiG/extensions/sdk"
)

func validToolPattern(pattern string) bool {
	if pattern == "" || strings.ContainsAny(pattern, " \t\r\n/") {
		return false
	}
	if at := strings.IndexAny(pattern, "*?["); at == 0 {
		return false
	}
	_, err := path.Match(pattern, "")
	return err == nil
}

func matchesTool(patterns []string, name string) bool {
	for _, pattern := range patterns {
		if matched, _ := path.Match(pattern, name); matched {
			return true
		}
	}
	return false
}

func expandTools(patterns, available []string) []string {
	names := []string{}
	for _, name := range available {
		if matchesTool(patterns, name) {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}

func withinParent(parent, root *record, name string) bool {
	for parent != root {
		if !matchesTool(parent.tools, name) {
			return false
		}
		parent = parent.parent
	}
	return true
}

func childAllowedTools(r, root *record, available []string) map[string]struct{} {
	if r.role == "observer" {
		allowed := map[string]struct{}{"observer_report": {}}
		for _, item := range []struct{ parent, virtual string }{{"read", "observer_read"}, {"ls", "observer_ls"}} {
			if slices.Contains(available, item.parent) && matchesTool(r.tools, item.parent) && withinParent(r.parent, root, item.parent) {
				allowed[item.virtual] = struct{}{}
			}
		}
		return allowed
	}
	allowed := map[string]struct{}{}
	for _, name := range expandTools(r.tools, available) {
		if !slices.Contains(teamToolNames, name) && (r.canDelegate || !slices.Contains(controlNames, name)) && withinParent(r.parent, root, name) {
			allowed[name] = struct{}{}
		}
	}
	var names []string
	if r.canDelegate {
		names = append(names, controlNames...)
	}
	if r.team != nil {
		names = append(names, teamToolNames...)
	}
	for _, name := range names {
		if slices.Contains(available, name) {
			allowed[name] = struct{}{}
		}
	}
	return allowed
}

func (o *owner) rootToolNames() ([]string, error) {
	tools, err := o.context.GetAllTools()
	if err != nil {
		return nil, reject("unavailable", "root tool enumeration failed: "+err.Error())
	}
	names := []string{}
	for _, tool := range tools {
		if tool.Exposure != sdk.ToolExposureHidden {
			names = append(names, tool.Name)
		}
	}
	return names, nil
}

func isMCPTool(name string) bool {
	return strings.HasPrefix(name, "mcp__") || slices.Contains([]string{mcpext.ListMcpResourcesTool, mcpext.ListMcpResourceTemplatesTool, mcpext.ReadMcpResourceTool}, name)
}

func waitForChildToolStartup(ctx context.Context, session *coding.Session, allowed map[string]struct{}) []string {
	pending := map[string]struct{}{}
	for name := range allowed {
		if isMCPTool(name) {
			pending[name] = struct{}{}
		}
	}
	if len(pending) == 0 {
		return nil
	}
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		for name := range allowed {
			if isMCPTool(name) {
				pending[name] = struct{}{}
			}
		}
		tools := session.GetAllTools()
		active := session.ActiveToolNames()
		for _, tool := range tools {
			if tool.Exposure == extension.ToolExposureHidden {
				continue
			}
			if (tool.Exposure == "" || tool.Exposure == extension.ToolExposureDirect || tool.Exposure == extension.ToolExposureModelOnly) && !slices.Contains(active, tool.Name) {
				continue
			}
			delete(pending, tool.Name)
		}
		if len(pending) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return slices.Sorted(maps.Keys(pending))
		case <-timer.C:
			return slices.Sorted(maps.Keys(pending))
		case <-ticker.C:
		}
	}
}

func (o *owner) childExtensions(r *record, run *run, allowed map[string]struct{}) ([]extension.Extension, *extension.ExtensionRuntime, error) {
	extensions := []extension.Extension{o.completionHooks(r)}
	if r.team != nil && r.role != "observer" {
		extensions = append(extensions, o.peerHooks(r, run))
	}
	registrations := extension.CreateExtensionRuntime()
	mcpAllowed := false
	for name := range allowed {
		if isMCPTool(name) {
			mcpAllowed = true
		}
	}
	options := builtin.Options{Mcp: mcpext.Options{AgentDir: o.agentDir}}
	if mcpAllowed {
		trusted, err := o.context.IsProjectTrusted()
		if err != nil {
			return nil, nil, err
		}
		options.Mcp.LoadConfig = func(context.Context) mcpext.LoadedMcpConfig {
			return mcpext.LoadMcpConfig(mcpext.LoadOptions{AgentDir: o.agentDir, Cwd: o.cwd, ProjectTrusted: trusted, ConfigDirName: projectConfigDir()})
		}
		servers, err := o.context.GetMcpServers()
		if err != nil {
			return nil, nil, err
		}
		for _, server := range servers {
			config, err := json.Marshal(server.Config)
			if err != nil {
				return nil, nil, err
			}
			if err := registrations.RegisterMcpServer(server.ExtensionPath, server.Name, config); err != nil {
				return nil, nil, err
			}
		}
	}
	loader := extensionset.Loader{CWD: r.cwd, Runtime: registrations}
	var paths []string
	for _, item := range []struct{ extension, tool string }{{"codemode", "codemode"}, {"tool-search", "tool_search"}, {"mcp", ""}} {
		if _, present := allowed[item.tool]; !present && (item.extension != "mcp" || !mcpAllowed) {
			continue
		}
		entry, err := builtin.Resolve("builtin:"+item.extension, options)
		if err != nil {
			return nil, nil, err
		}
		loader.Inline = append(loader.Inline, extension.NamedInlineExtension{Name: entry.Name, Factory: entry.Factory, Builtin: true, Replaceable: entry.Replaceable})
		paths = append(paths, entry.Path())
	}
	loaded, failures := loader.LoadBuiltin(paths)
	if len(failures) != 0 {
		return nil, nil, fmt.Errorf("load %s: %s", failures[0].Path, failures[0].Error)
	}
	extensions = append(extensions, loaded...)
	if r.team != nil && r.role != "observer" && len(o.policy.TeammateIdle) != 0 {
		extensions = append(extensions, o.idlePolicyHooks(r, run))
	}
	return extensions, registrations, nil
}

func projectConfigDir() string {
	if os.Getenv("PIG_USE_PI_DIRS") == "1" {
		return ".pi"
	}
	return ".pig"
}
