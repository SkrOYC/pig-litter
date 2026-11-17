//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func residentObservationForTest() residentObservation { return residentObservation{RSSBytes: 4096} }

func writeFakeProcess(t *testing.T, root string, pid int, start uint64, pages int, threads map[int]string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	fields := make([]string, 20)
	for index := range fields {
		fields[index] = "1"
	}
	fields[0], fields[2], fields[19] = "S", "42", strconv.FormatUint(start, 10)
	if pid != 42 {
		fields[1] = "42"
	}
	if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(fmt.Sprintf("%d (test name) %s\n", pid, strings.Join(fields, " "))), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "statm"), []byte(fmt.Sprintf("20 %d\n", pages)), 0o600); err != nil {
		t.Fatal(err)
	}
	for tid, children := range threads {
		task := filepath.Join(dir, "task", strconv.Itoa(tid))
		if err := os.MkdirAll(task, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(task, "children"), []byte(children), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func TestResidentTreeIdentityDeduplicationAndDisappearance(t *testing.T) {
	root := t.TempDir()
	writeFakeProcess(t, root, 42, 100, 3, map[int]string{42: "43 44", 99: "43"})
	writeFakeProcess(t, root, 43, 101, 4, map[int]string{43: ""})
	observation, vanished, err := sampleResidentTree(root, processIdentity{PID: 42, StartTicks: 100}, 4096)
	if err != nil || observation.RSSBytes != 28_672 || len(observation.Processes) != 2 || vanished != 1 {
		t.Fatalf("observation = %#v, vanished = %d, error = %v", observation, vanished, err)
	}
	observation, vanished, err = sampleResidentTree(root, processIdentity{PID: 42, StartTicks: 99}, 4096)
	if err != nil || len(observation.Processes) != 0 || vanished != 1 {
		t.Fatalf("reused PID attributed to old identity: %#v, %d, %v", observation, vanished, err)
	}
	if err := os.WriteFile(filepath.Join(root, "43", "statm"), []byte("invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := sampleResidentTree(root, processIdentity{PID: 42, StartTicks: 100}, 4096); err == nil {
		t.Fatal("accepted malformed RSS")
	}
}

func TestPerformanceProcessHelper(t *testing.T) {
	switch os.Getenv("PERFORMANCE_PROCESS_HELPER") {
	case "child":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	case "parent":
		child := exec.Command(os.Args[0], "-test.run=^TestPerformanceProcessHelper$")
		child.Env = append(os.Environ(), "PERFORMANCE_PROCESS_HELPER=child")
		if err := child.Start(); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(os.Getenv("PERFORMANCE_CHILD_PID"), []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(3)
		}
		time.Sleep(250 * time.Millisecond)
		os.Exit(0)
	}
}

func TestPerformanceCleanupStopsObservedDescendant(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	command := exec.Command(os.Args[0], "-test.run=^TestPerformanceProcessHelper$")
	command.Env = append(os.Environ(), "PERFORMANCE_PROCESS_HELPER=parent", "PERFORMANCE_CHILD_PID="+pidFile)
	prepareProcess(command)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	trace, waitErr, cleanupErr := monitorPerformance(command)
	if waitErr != nil || cleanupErr != nil || trace.Error != nil || trace.PeakBytes == 0 {
		t.Fatalf("monitor failed: wait=%v cleanup=%v trace=%v", waitErr, cleanupErr, trace.Error)
	}
	raw, err := os.ReadFile(pidFile)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil {
		t.Fatal(err)
	}
	stat, err := readProcStat("/proc", pid)
	if err == nil && stat.state != 'Z' && stat.state != 'X' {
		t.Fatalf("descendant %d remained alive after cleanup", pid)
	}
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
}

func TestPerformanceFixtureFailureDoesNotPublish(t *testing.T) {
	dir := t.TempDir()
	host := filepath.Join(dir, "host")
	marker := filepath.Join(dir, "host-started")
	if err := os.WriteFile(host, []byte("#!/bin/sh\nprintf started > '"+marker+"'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	perf, err := newPerformance()
	if err != nil {
		t.Fatal(err)
	}
	if err := run(dir, host, filepath.Join(dir, "unused-extension"), "core", perf); err == nil {
		t.Fatal("fixture without provider requests unexpectedly passed")
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fake host did not run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "performance.json")); !os.IsNotExist(err) {
		t.Fatalf("failed fixture published performance: %v", err)
	}
}

func TestPerformanceDisabledUsesOrdinaryCommand(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "ran")
	command := exec.Command("/bin/sh", "-c", "printf ran > \"$1\"", "sh", marker)
	if err := observePerformance(command, nil, dir); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(marker)
	if err != nil || string(contents) != "ran" {
		t.Fatalf("ordinary command did not run: %q, %v", contents, err)
	}
	if _, err := os.Stat(filepath.Join(dir, "performance.json")); !os.IsNotExist(err) {
		t.Fatalf("ordinary command wrote performance data: %v", err)
	}
}
