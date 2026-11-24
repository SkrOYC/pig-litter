package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"golang.org/x/mod/modfile"
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
	if evidence.Status != "observed" || len(evidence.Cases) != 3 {
		t.Fatalf("registration evidence = %#v", evidence)
	}
	checkRegistrationHostSource(t, evidence.HostModuleVersion)
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

func checkRegistrationHostSource(t *testing.T, version string) {
	t.Helper()
	source := os.Getenv("PIG_LITTER_SOURCE_DIR")
	if source == "" && version == "v0.7.0" {
		return
	}
	if version != "(devel)" || !filepath.IsAbs(source) {
		t.Fatalf("unexpected PiG module version %q", version)
	}
	workspace := os.Getenv("GOWORK")
	if !filepath.IsAbs(workspace) {
		t.Fatal("workspace PiG module has no declared Go workspace")
	}
	data, err := os.ReadFile(workspace)
	if err != nil {
		t.Fatal(err)
	}
	work, err := modfile.ParseWork(workspace, data, nil)
	if err != nil {
		t.Fatalf("parse Go workspace: %v", err)
	}
	want, wantErr := os.Stat(source)
	if wantErr != nil {
		t.Fatal(wantErr)
	}
	matches := 0
	for _, use := range work.Use {
		path := use.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(filepath.Dir(workspace), path)
		}
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if os.SameFile(want, info) {
			matches++
		}
	}
	for _, replace := range work.Replace {
		if replace.Old.Path == "github.com/MichaelKinsy/PiG" {
			t.Fatal("workspace replaces the declared PiG module")
		}
	}
	moduleData, err := os.ReadFile(filepath.Join(source, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	module, err := modfile.Parse(filepath.Join(source, "go.mod"), moduleData, nil)
	if err != nil || module.Module == nil || module.Module.Mod.Path != "github.com/MichaelKinsy/PiG" || matches != 1 {
		t.Fatal("Go workspace does not select the declared PiG source exactly once")
	}
}
