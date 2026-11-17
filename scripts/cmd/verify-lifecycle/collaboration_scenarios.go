package main

type collaborationP02Scenario struct {
	Purpose      string
	Channel      string
	Revision     int
	Capabilities []string
}

var collaborationP02Scenarios = map[string]collaborationP02Scenario{
	"baseline":                         {Purpose: "verification", Channel: "live", Revision: 1},
	"startup-source":                   {Purpose: "verification", Channel: "live", Revision: 1},
	"startup-standalone":               {Purpose: "verification", Channel: "live", Revision: 1},
	"startup-piglet":                   {Purpose: "verification", Channel: "live", Revision: 1},
	"startup-unselected":               {Purpose: "verification", Channel: "live", Revision: 1},
	"host-busy-message":                {Purpose: "research", Channel: "research", Revision: 1, Capabilities: []string{"peer-delivery", "native-append-acknowledgement"}},
	"host-idle-message":                {Purpose: "research", Channel: "research", Revision: 1, Capabilities: []string{"peer-delivery", "native-append-acknowledgement"}},
	"host-crash-after-acceptance":      {Purpose: "research", Channel: "research", Revision: 1, Capabilities: []string{"native-append-acknowledgement", "native-idempotent-replay"}},
	"host-crash-after-append":          {Purpose: "research", Channel: "research", Revision: 1, Capabilities: []string{"native-append-acknowledgement", "native-idempotent-replay"}},
	"host-crash-after-acknowledgement": {Purpose: "research", Channel: "research", Revision: 1, Capabilities: []string{"native-append-acknowledgement", "native-idempotent-replay"}},
	"host-same-id-replay":              {Purpose: "research", Channel: "research", Revision: 1, Capabilities: []string{"native-idempotent-replay"}},
	"host-tool-veto":                   {Purpose: "research", Channel: "research", Revision: 1, Capabilities: []string{"direct-hook", "nested-hook"}},
	"host-independent-process":         {Purpose: "research", Channel: "research", Revision: 1, Capabilities: []string{"independent-selected-tools"}},
	"fixture-isolation":                {Purpose: "verification", Channel: "live", Revision: 1},
	"fixture-interrupt":                {Purpose: "verification", Channel: "live", Revision: 1},
	"capture-performance":              {Purpose: "verification", Channel: "performance", Revision: 1},
}

func collaborationP02ScenarioRegistered(catalog collaborationCatalog, attempt collaborationAttempt) bool {
	rule, exists := collaborationP02Scenarios[attempt.Scenario]
	if !exists || rule.Purpose != attempt.Purpose || rule.Channel != attempt.Channel || rule.Revision != attempt.ScenarioRevision || !collaborationOneOf(attempt.Platform, "linux", "macos") || (attempt.Terminal == "iterm2" && attempt.Platform != "macos") || (attempt.Finding != nil && !collaborationContains(rule.Capabilities, attempt.Finding.Capability)) {
		return false
	}
	for _, scenario := range catalog.Scenarios {
		if scenario.ID == attempt.Scenario {
			return scenario.Owner == "P02" && scenario.Status == "implemented"
		}
	}
	return false
}
