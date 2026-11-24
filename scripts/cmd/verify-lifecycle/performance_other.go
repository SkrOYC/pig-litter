//go:build !linux

package main

import (
	"errors"
	"os"
	"os/exec"
)

type processIdentity struct {
	PID        int    `json:"pid"`
	StartTicks uint64 `json:"start_ticks"`
}

type residentTrace struct {
	Root         processIdentity `json:"root"`
	Observations []struct{}      `json:"observations"`
	PeakBytes    uint64          `json:"peak_bytes"`
	MaxGapNS     int64           `json:"max_gap_ns"`
	Vanished     uint64          `json:"vanished"`
	Error        error           `json:"-"`
}

func performanceSupported() bool      { return false }
func performanceSignals() []os.Signal { return []os.Signal{os.Interrupt} }
func newPerformance() (*performance, error) {
	return nil, errors.New("performance measurement requires Linux")
}
func monitorPerformance(*exec.Cmd) (residentTrace, error, error) {
	return residentTrace{}, errors.New("performance measurement requires Linux"), nil
}
