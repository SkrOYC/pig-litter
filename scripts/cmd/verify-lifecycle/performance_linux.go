//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

type processIdentity struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
}

type residentProcess struct {
	Identity processIdentity `json:"identity"`
	RSSBytes uint64          `json:"rss_bytes"`
}

type residentObservation struct {
	ElapsedNS int64             `json:"elapsed_ns"`
	Processes []residentProcess `json:"processes"`
	RSSBytes  uint64            `json:"rss_bytes"`
}

type residentTrace struct {
	Root          processIdentity       `json:"root"`
	Observations  []residentObservation `json:"observations"`
	PeakBytes     uint64                `json:"peak_bytes"`
	MaxGapNS      int64                 `json:"max_gap_ns"`
	Vanished      uint64                `json:"vanished"`
	RootMaxRSSKiB int64                 `json:"root_max_rss_kib"`
	Error         error                 `json:"-"`
}

type procStat struct {
	identity processIdentity
	parent   int
	group    int
	state    byte
}

func performanceSupported() bool      { return true }
func performanceSignals() []os.Signal { return []os.Signal{os.Interrupt, syscall.SIGTERM} }
func newPerformance() (*performance, error) {
	if _, err := os.Stat("/proc/self/statm"); err != nil {
		return nil, fmt.Errorf("Linux /proc is unavailable: %w", err)
	}
	return &performance{}, nil
}

func readProcStat(root string, pid int) (procStat, error) {
	raw, err := os.ReadFile(filepath.Join(root, strconv.Itoa(pid), "stat"))
	if err != nil {
		return procStat{}, err
	}
	closeParen := strings.LastIndexByte(string(raw), ')')
	if closeParen < 0 {
		return procStat{}, fmt.Errorf("malformed stat for pid %d", pid)
	}
	fields := strings.Fields(string(raw[closeParen+1:]))
	if len(fields) < 20 || len(fields[0]) != 1 {
		return procStat{}, fmt.Errorf("malformed stat for pid %d", pid)
	}
	group, groupErr := strconv.Atoi(fields[2])
	parent, parentErr := strconv.Atoi(fields[1])
	start, startErr := strconv.ParseUint(fields[19], 10, 64)
	if groupErr != nil || parentErr != nil || startErr != nil || group < 0 || parent < 0 {
		return procStat{}, fmt.Errorf("invalid stat for pid %d", pid)
	}
	return procStat{identity: processIdentity{PID: pid, StartTicks: start}, parent: parent, group: group, state: fields[0][0]}, nil
}

func procDisappeared(err error) bool {
	return errors.Is(err, os.ErrNotExist) || errors.Is(err, syscall.ESRCH)
}

func sampleResidentTree(procRoot string, root processIdentity, pageSize int) (residentObservation, uint64, error) {
	if root.PID <= 0 || root.StartTicks == 0 || pageSize <= 0 {
		return residentObservation{}, 0, errors.New("invalid process identity or page size")
	}
	queue := []processIdentity{root}
	seen := map[int]bool{}
	var observation residentObservation
	var vanished uint64
	for len(queue) != 0 {
		identity := queue[0]
		queue = queue[1:]
		if seen[identity.PID] {
			continue
		}
		seen[identity.PID] = true
		before, err := readProcStat(procRoot, identity.PID)
		if procDisappeared(err) {
			vanished++
			continue
		}
		if err != nil {
			return observation, vanished, err
		}
		if before.identity != identity {
			vanished++
			continue
		}
		base := filepath.Join(procRoot, strconv.Itoa(identity.PID))
		raw, err := os.ReadFile(filepath.Join(base, "statm"))
		if procDisappeared(err) {
			vanished++
			continue
		}
		if err != nil {
			return observation, vanished, err
		}
		fields := strings.Fields(string(raw))
		if len(fields) < 2 {
			return observation, vanished, fmt.Errorf("malformed statm for pid %d", identity.PID)
		}
		pages, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil || pages > ^uint64(0)/uint64(pageSize) {
			return observation, vanished, fmt.Errorf("invalid RSS for pid %d", identity.PID)
		}
		tasks, err := os.ReadDir(filepath.Join(base, "task"))
		if procDisappeared(err) {
			vanished++
			continue
		}
		if err != nil {
			return observation, vanished, err
		}
		var childIDs []int
		for _, task := range tasks {
			if _, err := strconv.Atoi(task.Name()); err != nil {
				continue
			}
			children, err := os.ReadFile(filepath.Join(base, "task", task.Name(), "children"))
			if procDisappeared(err) {
				vanished++
				continue
			}
			if err != nil {
				return observation, vanished, err
			}
			for _, field := range strings.Fields(string(children)) {
				pid, err := strconv.Atoi(field)
				if err != nil || pid <= 0 {
					return observation, vanished, fmt.Errorf("invalid child pid %q", field)
				}
				childIDs = append(childIDs, pid)
			}
		}
		after, err := readProcStat(procRoot, identity.PID)
		if procDisappeared(err) {
			vanished++
			continue
		}
		if err != nil {
			return observation, vanished, err
		}
		if after.identity != identity {
			vanished++
			continue
		}
		bytes := pages * uint64(pageSize)
		if ^uint64(0)-observation.RSSBytes < bytes {
			return observation, vanished, errors.New("process-tree RSS overflow")
		}
		observation.RSSBytes += bytes
		observation.Processes = append(observation.Processes, residentProcess{Identity: identity, RSSBytes: bytes})
		for _, pid := range childIDs {
			if seen[pid] {
				continue
			}
			child, err := readProcStat(procRoot, pid)
			if procDisappeared(err) {
				vanished++
				continue
			}
			if err != nil {
				return observation, vanished, err
			}
			if child.parent != identity.PID || child.identity.StartTicks == 0 {
				vanished++
				continue
			}
			queue = append(queue, child.identity)
		}
	}
	return observation, vanished, nil
}

func monitorPerformance(command *exec.Cmd) (residentTrace, error, error) {
	root, rootErr := readProcStat("/proc", command.Process.Pid)
	trace := residentTrace{Root: root.identity, Error: rootErr}
	if rootErr != nil {
		trace.Root.PID = command.Process.Pid
	}
	stop := make(chan struct{})
	done := make(chan residentTrace, 1)
	go func() {
		if trace.Error != nil {
			done <- trace
			return
		}
		started := time.Now()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			observation, vanished, err := sampleResidentTree("/proc", trace.Root, os.Getpagesize())
			trace.Vanished += vanished
			if err != nil {
				trace.Error = err
				done <- trace
				return
			}
			observation.ElapsedNS = time.Since(started).Nanoseconds()
			if len(observation.Processes) > 0 {
				if n := len(trace.Observations); n > 0 {
					gap := observation.ElapsedNS - trace.Observations[n-1].ElapsedNS
					if gap > trace.MaxGapNS {
						trace.MaxGapNS = gap
					}
				}
				trace.Observations = append(trace.Observations, observation)
				if observation.RSSBytes > trace.PeakBytes {
					trace.PeakBytes = observation.RSSBytes
				}
			}
			select {
			case <-stop:
				done <- trace
				return
			case <-ticker.C:
			}
		}
	}()
	waitErr := command.Wait()
	close(stop)
	trace = <-done
	if usage, ok := command.ProcessState.SysUsage().(*syscall.Rusage); ok {
		trace.RootMaxRSSKiB = usage.Maxrss
	}
	cleanupErr := cleanupPerformanceProcesses(trace)
	return trace, waitErr, cleanupErr
}

func cleanupPerformanceProcesses(trace residentTrace) error {
	known := map[int]processIdentity{trace.Root.PID: trace.Root}
	for _, observation := range trace.Observations {
		for _, process := range observation.Processes {
			known[process.Identity.PID] = process.Identity
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		entries, err := os.ReadDir("/proc")
		if err != nil {
			return err
		}
		var group []procStat
		for _, entry := range entries {
			pid, err := strconv.Atoi(entry.Name())
			if err != nil || pid <= 0 {
				continue
			}
			stat, err := readProcStat("/proc", pid)
			if procDisappeared(err) {
				continue
			}
			if err != nil {
				return err
			}
			if stat.group == trace.Root.PID && stat.state != 'Z' && stat.state != 'X' {
				group = append(group, stat)
			}
		}
		verifiedGroup := false
		for _, stat := range group {
			if known[stat.identity.PID] == stat.identity {
				verifiedGroup = true
			}
		}
		if len(group) > 0 && verifiedGroup {
			if err := syscall.Kill(-trace.Root.PID, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
				return err
			}
		}
		live := len(group) > 0
		for pid, identity := range known {
			if pid <= 0 {
				continue
			}
			stat, err := readProcStat("/proc", pid)
			if procDisappeared(err) {
				continue
			}
			if err != nil {
				return err
			}
			if stat.identity == identity && stat.state != 'Z' && stat.state != 'X' {
				live = true
				if err := syscall.Kill(pid, syscall.SIGKILL); err != nil && !errors.Is(err, syscall.ESRCH) {
					return err
				}
			}
		}
		if !live {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("live or unverified process group members remain after cleanup")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
