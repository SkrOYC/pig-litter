package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

func hostilePTY(raw []byte) bool {
	for _, payload := range [][]byte{[]byte("OSC_PAYLOAD_SHOULD_NOT_PAINT"), []byte("CONFIG_OSC_SHOULD_NOT_PAINT")} {
		if bytes.Contains(raw, payload) {
			return true
		}
	}
	return false
}

func checkRawPayload(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("inconclusive capture: %w", err)
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, maxRawPTY+1))
	if err != nil {
		return fmt.Errorf("inconclusive capture: %w", err)
	}
	if hostilePTY(raw) {
		return errors.New("attacker-controlled terminal sequence reached raw PTY")
	}
	collectorErrors, err := os.ReadFile(path + ".stderr")
	if err != nil {
		return fmt.Errorf("inconclusive capture: %w", err)
	}
	if len(collectorErrors) > 0 {
		return fmt.Errorf("inconclusive capture: raw PTY recorder failed: %s", collectorErrors)
	}
	if len(raw) == 0 || len(raw) > maxRawPTY {
		return fmt.Errorf("inconclusive capture: raw PTY empty or exceeded %d byte bound", maxRawPTY)
	}
	return nil
}

func checkRawPTY(path string) error {
	payloadErr := checkRawPayload(path)
	data, err := os.ReadFile(filepath.Join(filepath.Dir(path), "pty.capture.json"))
	if err != nil {
		return errors.Join(payloadErr, fmt.Errorf("inconclusive capture: completion evidence missing: %w", err))
	}
	var evidence captureEvidence
	if err := json.Unmarshal(data, &evidence); err != nil {
		return errors.Join(payloadErr, fmt.Errorf("inconclusive capture: %w", err))
	}
	for _, interval := range evidence.Intervals {
		if interval.Hostile {
			return errors.Join(payloadErr, errors.New("attacker-controlled terminal sequence reached raw PTY"))
		}
	}
	info, statErr := os.Lstat(path)
	digest, hashErr := fileSHA256(path)
	if statErr != nil || hashErr != nil || evidence.Status != "complete" || len(evidence.Intervals) == 0 || info.Size() != evidence.Saved || statIdentity(info) != evidence.RawIdentity || evidence.SHA256 != digest {
		return errors.Join(payloadErr, errors.New("inconclusive capture: missing, incomplete, or changed recording"))
	}
	for _, interval := range evidence.Intervals {
		if interval.Status != "complete" || !interval.EOF || !interval.Joined || !interval.Boundary || interval.Overflow || interval.BytesSaved == 0 || interval.Error != "" || interval.Generation != interval.Completion.Generation || interval.Completion.ExitedAt == "" || interval.Completion.ClosingID == "" || interval.Completion.ClosingID == interval.Generation {
			return errors.Join(payloadErr, errors.New("inconclusive capture: incomplete pane generation"))
		}
	}
	return payloadErr
}

func validateTerminalText(text string) error {
	if !utf8.ValidString(text) {
		return errors.New("unsupported live terminal input: ordinary text must be valid UTF-8")
	}
	for _, r := range text {
		if r < 32 || r == 127 || r >= 128 && r < 160 {
			return errors.New("unsupported live terminal input: send ordinary text or a tmux-owned named key; enhanced and associated input require unobserved negotiation")
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
