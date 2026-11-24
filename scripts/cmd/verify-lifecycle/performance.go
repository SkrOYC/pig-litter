package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"
)

type performance struct {
	mu                            sync.Mutex
	started, parent, spawn, child time.Time
	trace                         residentTrace
}

type performanceSample struct {
	StartupNS             int64           `json:"startup_ns"`
	SpawnToFirstRequestNS int64           `json:"spawn_to_first_request_ns"`
	PeakTreeRSSBytes      uint64          `json:"peak_tree_rss_bytes"`
	ParentOffsetNS        int64           `json:"parent_offset_ns"`
	SpawnOffsetNS         int64           `json:"spawn_offset_ns"`
	ChildOffsetNS         int64           `json:"child_offset_ns"`
	SamplingIntervalNS    int64           `json:"sampling_interval_ns"`
	SamplingCount         int             `json:"sampling_count"`
	MaxGapNS              int64           `json:"max_gap_ns"`
	PageSize              int             `json:"page_size"`
	Root                  processIdentity `json:"root"`
	Vanished              uint64          `json:"vanished"`
}

func (p *performance) markRequest(model string, arrival time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var target *time.Time
	switch model {
	case "parent":
		target = &p.parent
	case "held":
		target = &p.child
	default:
		return
	}
	if target.IsZero() || arrival.Before(*target) {
		*target = arrival
	}
}

func (p *performance) markSpawn(at time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.spawn.IsZero() {
		p.spawn = at
	}
}

func (p *performance) publish(directory string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.started.IsZero() || p.parent.IsZero() || p.spawn.IsZero() || p.child.IsZero() ||
		!p.parent.After(p.started) || p.spawn.Before(p.parent) || !p.child.After(p.spawn) {
		return errors.New("missing or out-of-order performance events")
	}
	if p.trace.PeakBytes == 0 || len(p.trace.Observations) == 0 {
		return errors.New("no positive process-tree RSS observation")
	}
	sample := performanceSample{
		StartupNS: p.parent.Sub(p.started).Nanoseconds(), SpawnToFirstRequestNS: p.child.Sub(p.spawn).Nanoseconds(),
		PeakTreeRSSBytes: p.trace.PeakBytes, ParentOffsetNS: p.parent.Sub(p.started).Nanoseconds(),
		SpawnOffsetNS: p.spawn.Sub(p.started).Nanoseconds(), ChildOffsetNS: p.child.Sub(p.started).Nanoseconds(),
		SamplingIntervalNS: int64(10 * time.Millisecond), SamplingCount: len(p.trace.Observations), MaxGapNS: p.trace.MaxGapNS,
		PageSize: os.Getpagesize(), Root: p.trace.Root, Vanished: p.trace.Vanished,
	}
	return writeJSON(filepath.Join(directory, "performance.json"), sample)
}

func publishFixtureResult(directory string, p *performance, result any) error {
	if p != nil {
		if err := p.publish(directory); err != nil {
			return err
		}
	}
	if err := writeJSON(filepath.Join(directory, "result.json"), result); err != nil {
		if p != nil {
			return errors.Join(err, os.Remove(filepath.Join(directory, "performance.json")))
		}
		return err
	}
	return nil
}

func observePerformance(command *exec.Cmd, p *performance, directory string) error {
	if p == nil {
		return command.Run()
	}
	p.started = time.Now()
	if err := command.Start(); err != nil {
		return err
	}
	trace, waitErr, cleanupErr := monitorPerformance(command)
	p.trace = trace
	traceErr := writeJSON(filepath.Join(directory, "resident-observations.json"), trace)
	if err := errors.Join(waitErr, cleanupErr, traceErr, trace.Error); err != nil {
		return fmt.Errorf("performance observation: %w", err)
	}
	return nil
}
