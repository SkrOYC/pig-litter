package main

import (
	"errors"
	"sync"
	"time"
)

type p05SpawnWaitTrace struct {
	mu         sync.Mutex
	spawnStart time.Time
	waitStart  time.Time
	spawnNS    int64
	waitNS     int64
	totalNS    int64
}

func (trace *p05SpawnWaitTrace) mark(callName string, at time.Time) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	switch callName {
	case "litter_spawn":
		trace.spawnStart = at
	case "litter_wait":
		trace.waitStart = at
	}
}

func (f *fixture) parentP05SpawnWait(body modelRequest, arrival time.Time) (string, []call, error) {
	tools, err := toolResults(body)
	if err != nil {
		return "", nil, err
	}
	switch len(tools) {
	case 0:
		return "", []call{{"litter_spawn", map[string]any{"type": "scout", "task": "P05 performance child", "model": "fixture/p05_perf_child"}}}, nil
	case 1:
		if tools[0].Child.ID == "" {
			return "", nil, errors.New("performance child was not admitted")
		}
		f.p05Trace.mu.Lock()
		f.p05Trace.spawnNS = arrival.Sub(f.p05Trace.spawnStart).Nanoseconds()
		f.p05Trace.mu.Unlock()
		return "", []call{{"litter_wait", map[string]any{"id": tools[0].Child.ID, "timeoutMs": 10000}}}, nil
	case 2:
		if tools[1].State != "completed" || tools[1].Outcome.State != "completed" || tools[1].Outcome.Run.ID != tools[0].Child.ID {
			return "", nil, errors.New("performance child did not complete under its admitted ID")
		}
		f.p05Trace.mu.Lock()
		f.p05Trace.waitNS = arrival.Sub(f.p05Trace.waitStart).Nanoseconds()
		f.p05Trace.totalNS = arrival.Sub(f.p05Trace.spawnStart).Nanoseconds()
		f.p05Trace.mu.Unlock()
		f.done = true
		return "P05 spawn and wait complete", nil, nil
	}
	return "", nil, errors.New("unexpected P05 spawn and wait turn")
}

func (trace *p05SpawnWaitTrace) result() (map[string]any, error) {
	trace.mu.Lock()
	defer trace.mu.Unlock()
	if trace.spawnStart.IsZero() || trace.waitStart.IsZero() || trace.spawnNS <= 0 || trace.waitNS <= 0 || trace.totalNS <= 0 {
		return nil, errors.New("missing P05 spawn and wait timings")
	}
	return map[string]any{"clock": "Go monotonic time", "spawn_ns": trace.spawnNS, "wait_ns": trace.waitNS, "spawn_wait_ns": trace.totalNS}, nil
}
