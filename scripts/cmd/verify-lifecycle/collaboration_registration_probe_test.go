package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestCollaborationHostEmbeddedRegistration(t *testing.T) {
	root := collaborationProbeDirectory(t, "embedded-registration")
	collaborationProbeTestEnvironment(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	evidence, err := collaborationRunHostRegistrationProbe(ctx, root)
	if err != nil {
		t.Fatalf("embedded registration probe: %v", err)
	}
	if evidence.Status != "observed" || evidence.HostModuleVersion != "v0.7.0" || len(evidence.Cases) != 3 {
		t.Fatalf("registration evidence = %#v", evidence)
	}
	want := [][]string{{"p02_extra_alpha", "p02_extra_beta", "p02_factory"}, {"p02_extra_alpha", "p02_factory"}, {}}
	for i, observation := range evidence.Cases {
		if observation.SessionID == "" || !reflect.DeepEqual(observation.SessionTools, want[i]) || !reflect.DeepEqual(observation.ProviderTools, want[i]) {
			t.Fatalf("registration case = %#v, want tools %v", observation, want[i])
		}
	}
	data, err := os.ReadFile(filepath.Join(root, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var retained collaborationRegistrationEvidence
	if err := json.Unmarshal(data, &retained); err != nil || !reflect.DeepEqual(retained, evidence) {
		t.Fatalf("retained registration evidence = %#v, %v", retained, err)
	}
}
