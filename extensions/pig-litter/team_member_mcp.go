package piglitter

import (
	"encoding/json"
	"net/http"
	"slices"

	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/coding/mcpext"
)

type memberMCPConfig struct {
	Loaded  mcpext.LoadedMcpConfig          `json:"loaded"`
	Servers []extension.RegisteredMcpServer `json:"servers"`
}

func (queue *memberControlQueue) serveMCPConfig(w http.ResponseWriter, r *http.Request) {
	var request struct{}
	if decodeMemberControl(w, r, &request) != nil || !queue.bound() || !slices.ContainsFunc(queue.owned.Tools, isMCPTool) {
		http.Error(w, "native MCP configuration is unavailable", http.StatusForbidden)
		return
	}
	queue.mcpOnce.Do(func() {
		trusted, err := queue.owner.context.IsProjectTrusted()
		if err != nil {
			queue.mcpErr = err
			return
		}
		queue.mcpConfig.Loaded = mcpext.LoadMcpConfig(mcpext.LoadOptions{AgentDir: queue.owner.agentDir, Cwd: queue.owner.cwd, ProjectTrusted: trusted, ConfigDirName: projectConfigDir()})
		servers, err := queue.owner.context.GetMcpServers()
		if err == nil {
			var data []byte
			data, err = json.Marshal(servers)
			if err == nil {
				err = json.Unmarshal(data, &queue.mcpConfig.Servers)
			}
		}
		queue.mcpErr = err
	})
	if queue.mcpErr != nil || queue.valid() != nil {
		http.Error(w, "native MCP configuration is unavailable", http.StatusConflict)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(queue.mcpConfig)
}
