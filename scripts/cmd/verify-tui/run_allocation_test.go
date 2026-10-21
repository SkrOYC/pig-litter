package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTerminalWrapperAllocatesBeforeItsFirstBuild(t *testing.T) {
	directory := t.TempDir()
	goPath := filepath.Join(directory, "go")
	log := filepath.Join(directory, "first-build.txt")
	script := "#!/bin/sh\nprintf '%s\\n' \"$GOCACHE\" \"$GOTMPDIR\" \"$TMPDIR\" \"$GOMODCACHE\" \"$XDG_CACHE_HOME\" \"$XDG_RUNTIME_DIR\" \"$@\" > " + shellQuote(log) + "\nexit 27\n"
	if err := os.WriteFile(goPath, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	wrapper, err := filepath.Abs("../../verify-terminal-ui.sh")
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command("bash", wrapper, "--case", "unselected", "--evidence-root", filepath.Join(directory, "evidence"))
	command.Env = append(os.Environ(), "PATH="+directory+string(os.PathListSeparator)+os.Getenv("PATH"), "GOCACHE=/unowned/cache", "GOTMPDIR=/unowned/gotmp", "TMPDIR=/unowned/tmp", "GOMODCACHE=/unowned/modules")
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatalf("expected the injected build failure, got success: %s", output)
	}
	data, err := os.ReadFile(log)
	if err != nil {
		t.Fatalf("first build was not reached: %v: %s", err, output)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	for _, path := range lines[:6] {
		if !strings.HasPrefix(path, "/tmp/plt.") {
			t.Fatalf("first build used an unowned path %q; build arguments and environment: %q", path, data)
		}
		if info, err := os.Stat(path); err != nil || !info.IsDir() {
			t.Fatalf("owned build path absent before build or lost on failure: %s: %v", path, err)
		}
	}
	if strings.Contains(string(data), "/extensions/pig-litter/dist/") {
		t.Fatalf("verification still builds into shared dist: %s", data)
	}
	t.Logf("failed build retained its owned resources at %s", filepath.Dir(lines[0]))
}

func TestOwnedResourceRejectsReplacementAndKeepsEvidence(t *testing.T) {
	directory := t.TempDir()
	resource := filepath.Join(directory, "runtime")
	evidence := filepath.Join(directory, "evidence")
	if err := os.Mkdir(resource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "proof.txt"), []byte("retained proof"), 0o600); err != nil {
		t.Fatal(err)
	}
	owned, err := resourceIdentity(resource)
	if err != nil {
		t.Fatal(err)
	}
	if err := owned.check(); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(resource, resource+"-original"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(resource, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resource, "unowned.txt"), []byte("replacement remains"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := owned.check(); err == nil || !strings.Contains(err.Error(), "replaced") {
		t.Fatalf("replacement was accepted: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(resource, "unowned.txt"))
	if err != nil || string(data) != "replacement remains" {
		t.Fatalf("replacement lost: %q %v", data, err)
	}
	data, err = os.ReadFile(filepath.Join(evidence, "proof.txt"))
	if err != nil || string(data) != "retained proof" {
		t.Fatalf("evidence lost: %q %v", data, err)
	}
	if err := outsideAllocation(directory, evidence); err == nil {
		t.Fatal("accepted evidence inside removable allocation")
	}
}

func TestAllocationAcknowledgementRequiresOwnedProcessesToExit(t *testing.T) {
	directory := t.TempDir()
	root := filepath.Join(directory, "run")
	evidence := filepath.Join(directory, "evidence")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(evidence, 0o700); err != nil {
		t.Fatal(err)
	}
	allocation := runAllocation{Root: root, Token: "allocation-token", Evidence: evidence, digest: "allocation-digest"}
	for _, name := range []string{"", "bin", "cache", "gotmp", "tmp", "modules", "runtime", "gopath", "state", "data"} {
		path := filepath.Join(root, name)
		if name != "" {
			if err := os.Mkdir(path, 0o700); err != nil {
				t.Fatal(err)
			}
		}
		resource, err := resourceIdentity(path)
		if err != nil {
			t.Fatal(err)
		}
		allocation.Resources = append(allocation.Resources, resource)
	}
	child := exec.Command("/bin/sh", "-c", "sleep 0.1")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	process, err := readObservedProcess(child.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := cleanupEvidence{ProviderJoined: true, ObservedProcessesExited: true, PaneJoined: true, TmuxSocketClosed: true, TmuxProcess: process}
	if err := allocation.acknowledge(evidence, cleanup); err == nil || !strings.Contains(err.Error(), "survived cleanup") {
		t.Fatalf("live owned process was acknowledged: %v", err)
	}
	if err := child.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "result.json"), []byte(`{"ok":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := allocation.acknowledge(evidence, cleanup); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(evidence, "cleanup-ack.json"))
	if err != nil {
		t.Fatal(err)
	}
	var ack struct {
		OK     bool   `json:"ok"`
		Token  string `json:"token"`
		Result string `json:"result"`
	}
	if err := json.Unmarshal(data, &ack); err != nil {
		t.Fatal(err)
	}
	if !ack.OK || ack.Token != "allocation-token" || ack.Result != filepath.Join(evidence, "result.json") {
		t.Fatalf("wrong allocation acknowledgement: %s", data)
	}
	if _, err := readRunAllocation(filepath.Join(evidence, "cleanup-ack.json")); err == nil {
		t.Fatal("unowned acknowledgement was accepted as a wrapper allocation")
	}
}
