package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

type ownedResource struct {
	Path     string `json:"path"`
	Identity string `json:"identity"`
}

type runAllocation struct {
	Root            string          `json:"root"`
	Token           string          `json:"token"`
	OwnerPID        int             `json:"ownerPID"`
	OwnerStartTicks string          `json:"ownerStartTicks"`
	Resources       []ownedResource `json:"resources"`
	Evidence        string          `json:"evidence"`
	digest          string
}

func resourceIdentity(path string) (ownedResource, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return ownedResource{}, err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ownedResource{}, fmt.Errorf("owned resource is not a directory: %s", path)
	}
	return ownedResource{Path: path, Identity: statIdentity(info)}, nil
}

func statIdentity(info os.FileInfo) string {
	stat := info.Sys().(*syscall.Stat_t)
	return fmt.Sprintf("%d:%d:%d", stat.Dev, stat.Ino, stat.Uid)
}

func (resource ownedResource) check() error {
	current, err := resourceIdentity(resource.Path)
	if err != nil {
		return err
	}
	if current != resource {
		return fmt.Errorf("owned resource was replaced: %s", resource.Path)
	}
	return nil
}

func readRunAllocation(path string) (*runAllocation, error) {
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var allocation runAllocation
	if err := json.Unmarshal(data, &allocation); err != nil {
		return nil, err
	}
	if allocation.OwnerPID != os.Getppid() || allocation.OwnerStartTicks == "" || allocation.Token == "" || !filepath.IsAbs(allocation.Root) || filepath.Clean(allocation.Root) != allocation.Root || filepath.Dir(allocation.Root) != "/tmp" || !strings.HasPrefix(filepath.Base(allocation.Root), "plt.") {
		return nil, errors.New("run allocation does not belong to the invoking wrapper")
	}
	if !observedAlive(observedProcess{PID: allocation.OwnerPID, StartTicks: allocation.OwnerStartTicks}) {
		return nil, errors.New("run allocation owner identity changed")
	}
	if err := allocation.check(); err != nil {
		return nil, err
	}
	if err := outsideAllocation(allocation.Root, allocation.Evidence); err != nil {
		return nil, err
	}
	allocation.digest, err = fileSHA256(path)
	return &allocation, err
}

func outsideAllocation(root, evidence string) error {
	resolved, err := filepath.EvalSymlinks(evidence)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(resolved) || resolved == root || strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return errors.New("evidence must stay outside the owned temporary allocation")
	}
	return nil
}

func (allocation *runAllocation) check() error {
	names := []string{"", "bin", "cache", "gotmp", "tmp", "modules", "runtime", "gopath", "state", "data"}
	if len(allocation.Resources) != len(names) {
		return errors.New("run allocation has an incomplete resource inventory")
	}
	for index, name := range names {
		resource := allocation.Resources[index]
		if resource.Path != filepath.Join(allocation.Root, name) {
			return errors.New("run allocation resource escaped its owner")
		}
		if err := resource.check(); err != nil {
			return err
		}
	}
	return nil
}

func (allocation *runAllocation) acknowledge(evidence string, cleanup cleanupEvidence) error {
	if err := allocation.check(); err != nil {
		return err
	}
	if err := outsideAllocation(allocation.Root, evidence); err != nil {
		return err
	}
	if !cleanup.ProviderJoined || !cleanup.ObservedProcessesExited || !cleanup.PaneJoined || !cleanup.TmuxSocketClosed || cleanup.TmuxServerAlive {
		return errors.New("owned process cleanup was not acknowledged")
	}
	processes := append(append([]observedProcess(nil), cleanup.ObservedProcesses...), cleanup.TmuxProcess)
	for _, process := range processes {
		if observedAlive(process) {
			return fmt.Errorf("owned process %d survived cleanup", process.PID)
		}
	}
	resultPath := filepath.Join(evidence, "result.json")
	digest, err := fileSHA256(resultPath)
	if err != nil {
		return err
	}
	return writeCaptureJSON(filepath.Join(allocation.Evidence, "cleanup-ack.json"), struct {
		OK               bool              `json:"ok"`
		Token            string            `json:"token"`
		AllocationSHA256 string            `json:"allocationSHA256"`
		Result           string            `json:"result"`
		ResultSHA256     string            `json:"resultSHA256"`
		Processes        []observedProcess `json:"processes"`
	}{true, allocation.Token, allocation.digest, resultPath, digest, processes})
}
