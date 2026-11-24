package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func collaborationCLISetup(t *testing.T, scenario string) (collaborationCLIOptions, collaborationTarget, []byte) {
	t.Helper()
	_, catalogData, target, _ := collaborationTestEvidence(t)
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	targetPath := filepath.Join(root, "target.json")
	if err := collaborationCLIWriteNew(targetPath, target); err != nil {
		t.Fatal(err)
	}
	return collaborationCLIOptions{
		repo: repo, root: root, target: targetPath, scenario: scenario,
		catalog: filepath.Join(repo, "scripts", "testdata", "collaboration", "requirements.json"),
	}, target, catalogData
}

func collaborationCLIManifest(t *testing.T, root, scenario string) (collaborationAttempt, collaborationSelectedRecord) {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join(root, scenario+"-*", "manifest.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("manifest paths = %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var attempt collaborationAttempt
	if err := json.Unmarshal(data, &attempt); err != nil {
		t.Fatal(err)
	}
	relative, err := filepath.Rel(root, paths[0])
	if err != nil {
		t.Fatal(err)
	}
	return attempt, collaborationSelectedRecord{Path: relative, SHA256: collaborationTestDataDigest(data)}
}

func collaborationCLIPlannedSetup(t *testing.T) (collaborationCLIOptions, collaborationTarget, []byte) {
	t.Helper()
	options, target, catalogData := collaborationCLISetup(t, "new-research")
	var catalog collaborationCatalog
	if err := json.Unmarshal(catalogData, &catalog); err != nil {
		t.Fatal(err)
	}
	catalog.Scenarios = append(catalog.Scenarios, collaborationScenario{ID: "new-research", Owner: "P02", Status: "planned", Expected: "A future host observation."})
	catalogData, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	options.catalog = filepath.Join(options.root, "catalog.json")
	if err := os.WriteFile(options.catalog, catalogData, 0o600); err != nil {
		t.Fatal(err)
	}
	target.CatalogSHA256 = collaborationTestDataDigest(catalogData)
	targetData, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.target, targetData, 0o600); err != nil {
		t.Fatal(err)
	}
	return options, target, catalogData
}

func TestCollaborationCLIPlannedScenarioRetainsInconclusiveEvidence(t *testing.T) {
	options, target, catalogData := collaborationCLIPlannedSetup(t)
	err := collaborationCLIRun(context.Background(), options, []string{"--scenario", options.scenario})
	if err == nil || !strings.Contains(err.Error(), "no integrated production driver") {
		t.Fatalf("planned scenario error = %v", err)
	}
	attempt, selected := collaborationCLIManifest(t, options.root, options.scenario)
	if attempt.Outcome != "inconclusive" || attempt.Purpose != "verification" || len(attempt.Claims) != 0 {
		t.Fatalf("planned attempt = %#v", attempt)
	}
	selection := collaborationSelectionData(t, target, selected)
	_, evidence, err := collaborationSelectEvidence(context.Background(), options.root, selection, catalogData, target, collaborationMediaTools{})
	if err != nil {
		t.Fatal(err)
	}
	report, err := collaborationReduceReport(evidence, collaborationProofRegistry{})
	if err != nil || report.Parity || len(report.Verification) != 1 || report.Verification[0].Complete {
		t.Fatalf("report = %#v, error = %v", report, err)
	}
}

func TestCollaborationCLIRejectsUnknownAndMismatchedTargets(t *testing.T) {
	options, _, _ := collaborationCLISetup(t, "unknown")
	if err := collaborationCLIRun(context.Background(), options, nil); err == nil || !strings.Contains(err.Error(), "unknown") {
		t.Fatalf("unknown scenario error = %v", err)
	}
	options.scenario = "startup-standalone"
	options.pig = filepath.Join(options.root, "pig")
	options.extension = filepath.Join(options.root, "pig-litter")
	for _, path := range []string{options.pig, options.extension} {
		if err := os.WriteFile(path, []byte("fixture executable"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	err := collaborationCLIRun(context.Background(), options, nil)
	if err == nil || !strings.Contains(err.Error(), "digest mismatch") {
		t.Fatalf("mismatched executable error = %v", err)
	}
	attempt, _ := collaborationCLIManifest(t, options.root, options.scenario)
	if attempt.Outcome != "inconclusive" || !strings.Contains(attempt.Reason, "digest mismatch") {
		t.Fatalf("mismatched target attempt = %#v", attempt)
	}
	if _, err := os.Stat(filepath.Join(options.root, attempt.ID, "started.json")); err != nil {
		t.Fatalf("started receipt: %v", err)
	}
}

func TestCollaborationCLIInterruptRetainsInconclusiveAttempt(t *testing.T) {
	options, _, _ := collaborationCLIPlannedSetup(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := collaborationCLIRun(ctx, options, nil); err == nil {
		t.Fatal("interrupted scenario passed")
	}
	attempt, _ := collaborationCLIManifest(t, options.root, options.scenario)
	if attempt.Outcome != "inconclusive" || !strings.Contains(attempt.Reason, "context canceled") {
		t.Fatalf("interrupted attempt = %#v", attempt)
	}
}

func TestCollaborationWrapperRetainsBuildFailure(t *testing.T) {
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "devenv"), []byte("#!/bin/sh\n[ \"$1\" = shell ] && [ \"$2\" = -- ] || exit 8\nshift 2\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte("#!/bin/sh\nif [ -n \"$GOFLAGS\" ] || [ \"$GOWORK\" != off ] || [ \"$GOTOOLCHAIN\" != local ]; then printf 'inherited Go configuration\\n' >&2; exit 9; fi\nprintf 'compiler unavailable\\n' >&2\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(root, "evidence with spaces")
	command := exec.Command("bash", filepath.Join(repo, "scripts", "verify-collaboration.sh"), "--scenario", "baseline", "--evidence-root", evidence)
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "GOFLAGS=-tags=unwanted", "GOWORK=/tmp/unwanted-workspace", "GOTOOLCHAIN=auto")
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if !errors.As(err, &exit) || exit.ExitCode() != 7 || !strings.Contains(string(output), "compiler unavailable") {
		t.Fatalf("wrapper failure = %v, output = %s", err, output)
	}
	paths, err := filepath.Glob(filepath.Join(evidence, "bootstrap-*", "result.json"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("bootstrap records = %v, %v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Stage    string `json:"stage"`
		Outcome  string `json:"outcome"`
		ExitCode int    `json:"exit_code"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.Stage != "build" || result.Outcome != "inconclusive" || result.ExitCode != 7 {
		t.Fatalf("bootstrap result = %#v, %v", result, err)
	}
}

func TestCollaborationWrapperStopsUnresponsiveBuild(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("this check requires Linux process identities")
	}
	if _, err := exec.LookPath("setsid"); err != nil {
		t.Skip("this check requires setsid")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := ""
	if parent := os.Getenv("P02_BOOTSTRAP_TEST_ROOT"); parent != "" {
		root, err = os.MkdirTemp(parent, "bootstrap-interrupt-")
	} else {
		root = t.TempDir()
	}
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(root, "bin")
	if err := os.Mkdir(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "devenv"), []byte("#!/bin/sh\n[ \"$1\" = shell ] && [ \"$2\" = -- ] || exit 8\nshift 2\nexec \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\ntrap '' INT TERM\nprintf 'ready\\n'\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	evidence := filepath.Join(root, "evidence")
	command := exec.Command("bash", filepath.Join(repo, "scripts", "verify-collaboration.sh"), "--scenario", "baseline", "--evidence-root", evidence)
	command.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	var output bytes.Buffer
	command.Stdout, command.Stderr = &output, &output
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	var bootstrap string
	for bootstrap == "" {
		paths, _ := filepath.Glob(filepath.Join(evidence, "bootstrap-*", "build.stdout"))
		if len(paths) == 1 {
			data, _ := os.ReadFile(paths[0])
			if string(data) == "ready\n" {
				bootstrap = filepath.Dir(paths[0])
				break
			}
		}
		select {
		case err := <-done:
			t.Fatalf("wrapper exited before build readiness: %v", err)
		case <-deadline.C:
			_ = command.Process.Kill()
			<-done
			t.Fatal("build did not reach its ready marker")
		case <-time.After(25 * time.Millisecond):
		}
	}
	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 130 {
			t.Fatalf("interrupted wrapper exit = %v", err)
		}
	case <-time.After(40 * time.Second):
		_ = command.Process.Kill()
		<-done
		t.Fatal("unresponsive build was not bounded")
	}
	if err := os.WriteFile(filepath.Join(root, "wrapper-output.log"), output.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(bootstrap, "result.json"))
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		Stage       string `json:"stage"`
		Outcome     string `json:"outcome"`
		ExitCode    int    `json:"exit_code"`
		Interrupted bool   `json:"interrupted"`
		Forced      bool   `json:"forced_cleanup"`
		Joined      bool   `json:"joined"`
		GroupOwned  bool   `json:"process_group_owned"`
		GroupAlive  bool   `json:"process_group_alive"`
	}
	if err := json.Unmarshal(data, &result); err != nil || result.Stage != "build" || result.Outcome != "inconclusive" || result.ExitCode != 130 || !result.Interrupted || !result.Forced || !result.Joined || !result.GroupOwned || result.GroupAlive {
		t.Fatalf("unresponsive build receipt = %#v, error = %v", result, err)
	}
	t.Logf("retained bootstrap interruption evidence: %s", root)
}

func TestCollaborationPerformanceBudgetRequiresBothLimits(t *testing.T) {
	for _, test := range []struct {
		name        string
		base, head  []int64
		wantFailure bool
	}{
		{"both limits exceeded", []int64{1_000_000_000}, []int64{1_110_000_000}, true},
		{"absolute limit only", []int64{1_000_000_000}, []int64{1_080_000_000}, false},
		{"relative limit only", []int64{100_000_000}, []int64{140_000_000}, false},
		{"unpaired samples", []int64{100_000_000}, []int64{}, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := collaborationPerformanceRegression(test.base, test.head)
			if (err != nil) != test.wantFailure {
				t.Fatalf("comparison error = %v, want failure %t", err, test.wantFailure)
			}
		})
	}
}

func TestCollaborationCLISourceSelectionRejectsChangedFile(t *testing.T) {
	checkout, evidence := t.TempDir(), t.TempDir()
	source := filepath.Join(checkout, "extensions", "pig-litter")
	if err := os.MkdirAll(source, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(source, "go.mod")
	if err := os.WriteFile(path, []byte("module example.org/fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	goExecutable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	goDigest, err := collaborationCLIDigest(goExecutable)
	if err != nil {
		t.Fatal(err)
	}
	files := []collaborationCLISourceFile{{Path: "go.mod", Size: 27, Mode: 0o600, SHA256: collaborationTestDataDigest([]byte("module example.org/fixture\n"))}}
	fileData, err := json.Marshal(files)
	if err != nil {
		t.Fatal(err)
	}
	selection := collaborationCLISourceSelection{Source: source, Fixture: filepath.Join(evidence, "fixture"), SHA256: collaborationTestDataDigest(fileData), Algorithm: "sha256-json-file-list-v1", Files: files, Excluded: []string{".git", "dist"}, GoExecutable: goExecutable, GoSHA256: goDigest}
	selectionData, err := json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "source-selection.json"), selectionData, 0o600); err != nil {
		t.Fatal(err)
	}
	runner := collaborationCLIRunner{}
	target := collaborationCLITarget{Checkout: checkout}
	if err := runner.validateSourceSelection(target, evidence, selectionData); err != nil {
		t.Fatalf("valid source selection: %v", err)
	}
	manifestData := []byte("declared source\n")
	if err := os.WriteFile(filepath.Join(checkout, "pig-source.json"), manifestData, 0o600); err != nil {
		t.Fatal(err)
	}
	selection.PigSource = filepath.Join(checkout, "pig-source")
	selection.ManifestSHA256 = collaborationTestDataDigest(manifestData)
	workspaceData := []byte(fmt.Sprintf("go 1.27\n\nuse (\n %q\n %q\n %q\n)\n", selection.Fixture, selection.PigSource, filepath.Join(selection.PigSource, "extensions", "sdk")))
	selection.WorkspaceSHA256 = collaborationTestDataDigest(workspaceData)
	selectionData, err = json.Marshal(selection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "source-selection.json"), selectionData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "source-workspace.go.work"), workspaceData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.validateSourceSelection(target, evidence, selectionData); err != nil {
		t.Fatalf("valid target workspace: %v", err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "source-workspace.go.work"), []byte("altered workspace\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.validateSourceSelection(target, evidence, selectionData); err == nil || !strings.Contains(err.Error(), "workspace differs") {
		t.Fatalf("changed workspace error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(evidence, "source-workspace.go.work"), workspaceData, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("module example.org/changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runner.validateSourceSelection(target, evidence, selectionData); err == nil || !strings.Contains(err.Error(), "differs") {
		t.Fatalf("changed source selection error = %v", err)
	}
}

func TestCollaborationCLIFixtureIsolationRequiresDistinctOverlappingResources(t *testing.T) {
	var left, right collaborationCLIFixtureResult
	leftData := `{"ok":true,"startup":{"process":{"pid":10}},"cleanup":{"providerJoined":true},"childRefs":[{"id":"left-child","name":"same-task"}],"fixture":{"clock":"CLOCK_MONOTONIC","boot_id":"boot","time_namespace":"time:[1]","provider_port":1001,"provider_started_ns":10,"provider_close_started_ns":40,"workspace":{"path":"/left","identity":"left-workspace"},"fixture_file":{"path":"/left/file","identity":"left-file"},"root_sessions":[{"id":"left-session","file":{"identity":"left-session-file"}}]}}`
	rightData := `{"ok":true,"startup":{"process":{"pid":11}},"cleanup":{"providerJoined":true},"childRefs":[{"id":"right-child","name":"same-task"}],"fixture":{"clock":"CLOCK_MONOTONIC","boot_id":"boot","time_namespace":"time:[1]","provider_port":1002,"provider_started_ns":20,"provider_close_started_ns":50,"workspace":{"path":"/right","identity":"right-workspace"},"fixture_file":{"path":"/right/file","identity":"right-file"},"root_sessions":[{"id":"right-session","file":{"identity":"right-session-file"}}]}}`
	if err := json.Unmarshal([]byte(leftData), &left); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(rightData), &right); err != nil {
		t.Fatal(err)
	}
	if err := collaborationCompareFixtureResults(left, right); err != nil {
		t.Fatalf("independent fixtures: %v", err)
	}
	right.Fixture.ProviderPort = 1001
	if err := collaborationCompareFixtureResults(left, right); err == nil || !strings.Contains(err.Error(), "ports") {
		t.Fatalf("shared port error = %v", err)
	}
	right.Fixture.ProviderPort = 1002
	right.Fixture.ProviderStartedNS = 50
	right.Fixture.ProviderCloseStartedNS = 60
	if err := collaborationCompareFixtureResults(left, right); err == nil || !strings.Contains(err.Error(), "lifetimes") {
		t.Fatalf("non-overlapping fixture error = %v", err)
	}
}

func TestCollaborationCLIJoinsCanceledProcess(t *testing.T) {
	parent := collaborationCLIRunner{directory: t.TempDir(), started: time.Now()}
	child, err := parent.fixtureRunner("child")
	if err != nil {
		t.Fatal(err)
	}
	defer child.eventFile.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := child.execute(ctx, "process", []string{"/bin/sh", "-c", "trap 'exit 0' INT; printf ready; while :; do sleep 1; done"})
		done <- err
	}()
	deadline := time.After(5 * time.Second)
	for {
		data, _ := os.ReadFile(filepath.Join(child.directory, "process", "stdout.log"))
		if string(data) == "ready" {
			break
		}
		select {
		case <-deadline:
			cancel()
			<-done
			t.Fatal("child did not reach its ready marker")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) || len(child.drivers) != 1 || !child.drivers[0].Joined || child.drivers[0].ForcedCleanup {
			t.Fatalf("canceled child = %#v, error = %v", child.drivers, err)
		}
		if runtime.GOOS == "linux" && child.drivers[0].ProcessStart == "" {
			t.Fatalf("canceled child has no recorded process identity: %#v", child.drivers[0])
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceled child did not join")
	}
}
