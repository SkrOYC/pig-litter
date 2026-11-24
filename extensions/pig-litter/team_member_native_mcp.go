//go:build p08native

package piglitter

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/coding/mcpext"
)

type frozenMCPAPI struct {
	extension.API
	servers []extension.RegisteredMcpServer
}

func (member *nativeMember) waitTools(ctx context.Context, ext *extension.Context) error {
	if !slices.ContainsFunc(member.manifest.Tools, isMCPTool) {
		active := slices.Clone(ext.GetActiveTools())
		slices.Sort(active)
		if !slices.Equal(active, member.manifest.Tools) {
			return errors.New("native tool ceiling does not match its launch")
		}
		return nil
	}
	wait, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		active := ext.GetActiveTools()
		for _, name := range active {
			if !slices.Contains(member.manifest.Tools, name) {
				return errors.New("native active tool exceeds its ceiling")
			}
		}
		available := map[string]bool{}
		for _, tool := range ext.GetAllTools() {
			if tool.Exposure == extension.ToolExposureHidden {
				continue
			}
			if isMCPTool(tool.Name) && !slices.Contains(member.manifest.Tools, tool.Name) {
				return errors.New("native MCP tool exceeds its ceiling")
			}
			available[tool.Name] = tool.Exposure == extension.ToolExposureCodemode || tool.Exposure == extension.ToolExposureDeferred || slices.Contains(active, tool.Name)
		}
		if slices.ContainsFunc(member.manifest.Tools, func(name string) bool { return !available[name] }) {
			select {
			case <-wait.Done():
				return errors.New("native tool ceiling did not become available")
			case <-ticker.C:
				continue
			}
		}
		return nil
	}
}

func (api frozenMCPAPI) RegisterCommand(string, extension.CommandOptions) {}

func (api frozenMCPAPI) GetMcpServers() []extension.RegisteredMcpServer {
	return slices.Clone(api.servers)
}

func (member *nativeMember) registerMCP(api extension.API) error {
	if !slices.ContainsFunc(member.manifest.Tools, isMCPTool) {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var config memberMCPConfig
	if err := member.retry(ctx, "/member/mcp/config", struct{}{}, &config); err != nil {
		return err
	}
	options := mcpext.Options{AgentDir: member.manifest.AgentDirectory, LoadConfig: func(context.Context) mcpext.LoadedMcpConfig { return config.Loaded }}
	return mcpext.CreateMcpExtension(options)(frozenMCPAPI{API: api, servers: config.Servers})
}
