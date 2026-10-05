package pig_litter

import (
	"encoding/json"
	"strings"

	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

const (
	statusKey      = "pig-litter"
	statusText     = "foreground scout and worker"
	diagnosticText = "Pig Litter ready. Use pig_litter_agent with scout or worker. Foreground only."
)

func Extension() *sdk.Extension {
	e := sdk.New("pig-litter")
	active := make(chan struct{}, 1)
	e.RegisterTool(sdk.ToolDefinition{
		Name:          "pig_litter_agent",
		Label:         "Pig Litter agent",
		ExecutionMode: "parallel",
		Description:   "Requires interactive PiG. Run a fresh foreground scout (read-only file tools) or worker (read/write/edit file tools). Supply a self-contained task and optional exact provider/model. Returns a bounded report, state, and usage. No parent conversation is copied. No background sessions or shell tool.",
		Parameters: sdk.Schema{"type": "object", "additionalProperties": false, "required": []string{"type", "task"}, "properties": map[string]any{
			"type":      map[string]any{"type": "string", "enum": []string{"scout", "worker"}},
			"task":      map[string]any{"type": "string", "minLength": 1, "maxLength": maxTaskBytes},
			"model":     map[string]any{"type": "string", "maxLength": 256, "description": "Exact provider-qualified model. Defaults to the parent's current model."},
			"mode":      map[string]any{"type": "string", "enum": []string{"foreground"}},
			"timeoutMs": map[string]any{"type": "integer", "minimum": 1, "maximum": maxTimeoutMS, "default": defaultTimeoutMS},
		}},
		Execute: func(ctx sdk.Context, params map[string]any) (any, error) {
			request, err := parseRequest(params)
			if err != nil {
				return toolResult(Result{State: Rejected, AgentType: request.AgentType, Report: err.Error()}), nil
			}
			select {
			case active <- struct{}{}:
				defer func() { <-active }()
			default:
				return toolResult(Result{State: Rejected, AgentType: request.AgentType, Report: "A Pig Litter child is already active. Retry after it ends."}), nil
			}
			result := launch(ctx, request)
			return toolResult(result), nil
		},
	})
	e.Command("pig-litter", "Show Pig Litter agents and foreground delegation help.", func(ctx sdk.Context, _ string) error { ctx.Notify(diagnosticText, "info"); return nil })
	e.OnSessionStart(func(ctx sdk.Context, _ map[string]any) (any, error) {
		ctx.SetStatus(statusKey, statusText)
		return nil, nil
	})
	return e
}

func launch(ctx sdk.Context, request LaunchRequest) Result {
	result := Result{State: Unavailable, AgentType: request.AgentType, Model: request.Model}
	if ctx.Mode() != "tui" {
		result.Report = "Delegation requires interactive PiG. The pinned headless host cannot forward child cancellation."
		return result
	}
	if ctx.Err() != nil {
		result.State = Stopped
		result.Report = "Delegation was cancelled."
		return result
	}
	trusted, err := ctx.IsProjectTrusted()
	if err != nil || !trusted {
		result.State = Rejected
		result.Report = "Delegation requires a trusted project and a successful host trust lookup."
		return result
	}
	model := request.Model
	if model == "" {
		model = ctx.ModelQualified()
	}
	result.Model = model
	provider, id, qualified := strings.Cut(model, "/")
	if !qualified || provider == "" || id == "" {
		result.Report = "An exact provider-qualified model is required."
		return result
	}
	available, err := ctx.ModelRegistry().GetAvailable()
	if err != nil {
		result.Report = "The host model registry is unavailable."
		return result
	}
	found := false
	for _, candidate := range available {
		if candidate["provider"] == provider && candidate["id"] == id {
			found = true
			break
		}
	}
	if !found {
		result.Report = "The requested model is unavailable in the host registry."
		return result
	}
	if ctx.Cwd() == "" {
		result.State = Rejected
		result.Report = "The host working directory is unavailable."
		return result
	}
	process, err := ctx.ExecWithOptions("pig", childArgs(request, model), sdk.ExecOptions{Timeout: float64(request.TimeoutMS), Cwd: ctx.Cwd()})
	if ctx.Err() != nil {
		result.State = Stopped
		result.Report = "Delegation was cancelled."
		return result
	}
	if err != nil || process == nil {
		result.State = Failed
		result.Report = "The host could not execute the child PiG process."
		return result
	}
	return parseChild(process.Stdout, process.ExitCode, process.Killed, request, model)
}

func toolResult(result Result) sdk.ToolResult {
	result.AgentType, _ = bounded(result.AgentType, 64)
	result.Model, _ = bounded(result.Model, 256)
	var truncated bool
	result.Report, truncated = bounded(result.Report, maxReportBytes)
	result.ReportTruncated = result.ReportTruncated || truncated
	data, _ := json.Marshal(result)
	return sdk.ToolResult{Content: string(data)}
}
