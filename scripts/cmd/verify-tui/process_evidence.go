package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

func checkRawPTY(path string) error {
	collectorErrors, err := os.ReadFile(path + ".stderr")
	if err != nil {
		return err
	}
	if len(collectorErrors) > 0 {
		return fmt.Errorf("raw PTY recorder failed: %s", collectorErrors)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > maxRawPTY {
		return fmt.Errorf("raw PTY capture empty or exceeded %d byte bound", maxRawPTY)
	}
	for _, payload := range [][]byte{[]byte("OSC_PAYLOAD_SHOULD_NOT_PAINT"), []byte("CONFIG_OSC_SHOULD_NOT_PAINT"), []byte("\x1b]0;CONFIG_OSC_SHOULD_NOT_PAINT\a")} {
		if bytes.Contains(raw, payload) {
			return fmt.Errorf("attacker-controlled terminal sequence reached raw PTY")
		}
	}
	return nil
}

type observedProcess struct {
	PID        int    `json:"pid"`
	StartTicks string `json:"startTicks"`
	Executable string `json:"executable"`
}

func procStartTicks(data string) (string, error) {
	end := strings.LastIndexByte(data, ')')
	if end < 0 {
		return "", fmt.Errorf("invalid owned process stat")
	}
	fields := strings.Fields(data[end+1:])
	if len(fields) < 20 {
		return "", fmt.Errorf("owned process stat lacks start time")
	}
	return fields[19], nil
}

func readObservedProcess(pid int) (observedProcess, error) {
	base := filepath.Join("/proc", strconv.Itoa(pid))
	data, err := os.ReadFile(filepath.Join(base, "stat"))
	if err != nil {
		return observedProcess{}, err
	}
	start, err := procStartTicks(string(data))
	if err != nil {
		return observedProcess{}, err
	}
	executable, err := os.Readlink(filepath.Join(base, "exe"))
	if err != nil {
		return observedProcess{}, err
	}
	return observedProcess{PID: pid, StartTicks: start, Executable: executable}, nil
}

func observeDescendants(rootPID int) ([]observedProcess, error) {
	if rootPID <= 0 {
		return nil, nil
	}
	queue := []int{rootPID}
	seen := map[int]bool{}
	var result []observedProcess
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		if seen[pid] {
			continue
		}
		seen[pid] = true
		process, err := readObservedProcess(pid)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		result = append(result, process)
		paths, err := filepath.Glob(filepath.Join("/proc", strconv.Itoa(pid), "task", "*", "children"))
		if err != nil {
			return nil, err
		}
		for _, path := range paths {
			data, err := os.ReadFile(path)
			if os.IsNotExist(err) {
				continue
			}
			if err != nil {
				return nil, err
			}
			for _, value := range strings.Fields(string(data)) {
				child, err := strconv.Atoi(value)
				if err != nil {
					return nil, err
				}
				queue = append(queue, child)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].PID < result[j].PID })
	return result, nil
}

func observedAlive(process observedProcess) bool {
	data, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(process.PID), "stat"))
	if os.IsNotExist(err) {
		return false
	}
	if err != nil {
		return true
	}
	start, err := procStartTicks(string(data))
	return err != nil || start == process.StartTicks
}
