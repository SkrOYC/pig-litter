package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"sort"
	"time"

	"github.com/MichaelKinsy/PiG/agent"
	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/coding"
	"github.com/MichaelKinsy/PiG/coding/extension"
	"github.com/MichaelKinsy/PiG/coding/extension/factoryload"
)

type collaborationRegistrationTool string

func (tool collaborationRegistrationTool) Name() string  { return string(tool) }
func (tool collaborationRegistrationTool) Label() string { return string(tool) }
func (tool collaborationRegistrationTool) Schema() ai.ToolSchema {
	return ai.ToolSchema{Name: string(tool), Description: "Private registration probe", Parameters: map[string]any{"type": "object", "properties": map[string]any{}}}
}
func (collaborationRegistrationTool) ExecutionMode() agent.ToolExecutionMode {
	return agent.ToolModeSequential
}
func (collaborationRegistrationTool) Execute(context.Context, string, json.RawMessage, agent.ToolUpdateCallback) (agent.AgentToolResult, error) {
	return agent.AgentToolResult{}, errors.New("registration probe does not execute tools")
}

type collaborationRegistrationCase struct {
	Name          string   `json:"name"`
	Allowed       []string `json:"allowed"`
	SessionID     string   `json:"session_id,omitempty"`
	SessionTools  []string `json:"session_tools"`
	ProviderTools []string `json:"provider_tools"`
	Error         string   `json:"error,omitempty"`
}

type collaborationRegistrationEvidence struct {
	Schema            int                             `json:"schema"`
	Purpose           string                          `json:"purpose"`
	Status            string                          `json:"status"`
	HostModuleVersion string                          `json:"host_module_version"`
	ProcessID         int                             `json:"process_id"`
	StartedUTC        string                          `json:"started_utc"`
	ElapsedNS         int64                           `json:"elapsed_ns"`
	FactoryRegistered []string                        `json:"factory_registered"`
	ExtraRequested    []string                        `json:"extra_requested"`
	Cases             []collaborationRegistrationCase `json:"cases"`
	Error             string                          `json:"error,omitempty"`
}

func collaborationRunHostRegistrationProbe(ctx context.Context, root string) (evidence collaborationRegistrationEvidence, resultErr error) {
	started := time.Now()
	evidence = collaborationRegistrationEvidence{
		Schema: 1, Purpose: "provisional embedded host registration research; product team tools remain unverified",
		Status: "inconclusive", HostModuleVersion: "unknown", ProcessID: os.Getpid(), StartedUTC: started.UTC().Format(time.RFC3339Nano),
		FactoryRegistered: []string{"p02_factory"}, ExtraRequested: []string{"p02_extra_alpha", "p02_extra_beta"}, Cases: []collaborationRegistrationCase{},
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, dependency := range info.Deps {
			if dependency.Path == "github.com/MichaelKinsy/PiG" {
				evidence.HostModuleVersion = dependency.Version
			}
		}
	}
	if err := collaborationProbePrepare(root); err != nil {
		return evidence, err
	}
	defer func() {
		evidence.ElapsedNS = time.Since(started).Nanoseconds()
		if resultErr != nil {
			evidence.Status = "inconclusive"
			evidence.Error = resultErr.Error()
		}
		resultErr = errors.Join(resultErr, collaborationProbeWriteJSON(filepath.Join(root, "result.json"), evidence))
	}()
	for _, test := range []struct {
		name    string
		allowed []string
	}{
		{"all", []string{"p02_extra_alpha", "p02_extra_beta", "p02_factory"}},
		{"filtered", []string{"p02_extra_alpha", "p02_factory"}},
		{"none", []string{"p02_not_registered"}},
	} {
		observation, err := collaborationRunRegistrationCase(ctx, filepath.Join(root, test.name), test.name, test.allowed)
		evidence.Cases = append(evidence.Cases, observation)
		if err != nil {
			return evidence, fmt.Errorf("%s registration: %w", test.name, err)
		}
	}
	evidence.Status = "observed"
	return evidence, nil
}

func collaborationRunRegistrationCase(ctx context.Context, root, name string, names []string) (observation collaborationRegistrationCase, resultErr error) {
	observation = collaborationRegistrationCase{Name: name, Allowed: append([]string(nil), names...), SessionTools: []string{}, ProviderTools: []string{}}
	defer func() {
		if resultErr != nil {
			observation.Error = resultErr.Error()
		}
	}()
	fixture, err := collaborationMessageFixtureNew(root, 0)
	if err != nil {
		return observation, err
	}
	defer func() { resultErr = errors.Join(resultErr, fixture.close()) }()
	registrations := extension.CreateExtensionRuntime()
	factory := func(api extension.API) error {
		api.RegisterTool(extension.ToolDefinition{Name: "p02_factory", Label: "p02_factory", Description: "Private factory registration probe", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)})
		return nil
	}
	loaded, err := factoryload.LoadExtensionFromFactory(factory, root, extension.CreateEventBus(), registrations, "p02-registration")
	if err != nil {
		return observation, err
	}
	loaded.Name = "p02-registration"
	runtime, err := coding.NewRuntime(coding.RuntimeOptions{Services: fixture.services, NewExtensions: []extension.Extension{loaded}, ExtensionRuntime: registrations})
	if err != nil {
		return observation, err
	}
	defer func() { resultErr = errors.Join(resultErr, runtime.Close()) }()
	allowed := make(map[string]struct{}, len(names))
	for _, name := range names {
		allowed[name] = struct{}{}
	}
	session, err := runtime.New(coding.SessionStartOptions{
		Model: fixture.services.ModelRuntime().GetModel("fixture", "probe"), SessionDir: filepath.Join(root, "registered-sessions"), ResourceLoader: coding.NoResources,
		SkipBuiltinTools: true, AllowedTools: allowed, ExtraTools: []agent.AgentTool{collaborationRegistrationTool("p02_extra_alpha"), collaborationRegistrationTool("p02_extra_beta")},
	})
	if err != nil {
		return observation, err
	}
	drained := make(chan struct{})
	go func() {
		defer close(drained)
		for event := range session.Events() {
			coding.AcknowledgeEvent(event)
		}
	}()
	defer func() {
		resultErr = errors.Join(resultErr, session.Close())
		<-drained
	}()
	observation.SessionID = session.ID()
	for _, tool := range session.Tools() {
		observation.SessionTools = append(observation.SessionTools, tool.Name())
	}
	sort.Strings(observation.SessionTools)
	if _, err := session.Send(ctx, "Report that the registration fixture is ready."); err != nil {
		return observation, err
	}
	fixture.mu.Lock()
	requests := append([]json.RawMessage(nil), fixture.requests...)
	fixture.mu.Unlock()
	if len(requests) != 1 {
		return observation, fmt.Errorf("provider requests = %d, want one", len(requests))
	}
	var request struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(requests[0], &request); err != nil {
		return observation, err
	}
	for _, tool := range request.Tools {
		observation.ProviderTools = append(observation.ProviderTools, tool.Function.Name)
	}
	sort.Strings(observation.ProviderTools)
	return observation, nil
}
