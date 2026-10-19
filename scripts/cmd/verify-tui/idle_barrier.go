package main

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/SkrOYC/pig-litter/scripts/internal/tuiidle"
)

type idleBarrierEvidence struct {
	BeforePrompt string            `json:"beforePrompt"`
	RequestedAt  time.Time         `json:"requestedAt"`
	Readiness    tuiidle.Readiness `json:"readiness"`
	Process      observedProcess   `json:"process"`
}

func (p *panelProof) awaitRootIdle(prompt string) error {
	nonce := rand.Text()
	path := filepath.Join(p.idleDirectory, nonce+".json")
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("idle readiness path must be fresh: %v", err)
	}
	requests := p.state.requestTotalSnapshot()
	requestedAt := time.Now().UTC()
	if err := p.sendText("/" + p.idleCommand + " " + nonce); err != nil {
		return err
	}
	if err := p.sendKey("Enter"); err != nil {
		return err
	}
	var readiness tuiidle.Readiness
	var process observedProcess
	if err := waitFor(p.ctx, "full native root turn idle before "+prompt, stageTimeout, func() (bool, error) {
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, p.state.providerFailure()
		}
		if err != nil {
			return false, err
		}
		readiness, err = tuiidle.Decode(data)
		if err != nil {
			return false, err
		}
		if err := readiness.Validate(p.idleCommand, nonce, p.rootID, requestedAt); err != nil {
			return false, err
		}
		processes, err := p.processes()
		if err != nil {
			return false, err
		}
		process, err = idleObserverProcess(readiness.PID, p.idleObserver, processes)
		return err == nil, err
	}); err != nil {
		return err
	}
	if !reflect.DeepEqual(requests, p.state.requestTotalSnapshot()) {
		return fmt.Errorf("idle observer command started inference")
	}
	p.state.mu.Lock()
	for _, request := range p.state.rawRequests {
		if strings.Contains(string(request), nonce) || strings.Contains(string(request), p.idleCommand) {
			p.state.mu.Unlock()
			return fmt.Errorf("idle observer command entered provider context")
		}
	}
	p.state.mu.Unlock()
	root, err := p.root()
	if err != nil {
		return err
	}
	for _, message := range root.Messages {
		if strings.Contains(string(message), nonce) || strings.Contains(string(message), p.idleCommand) {
			return fmt.Errorf("idle observer command entered native message history")
		}
	}
	p.idleReadiness = append(p.idleReadiness, idleBarrierEvidence{BeforePrompt: prompt, RequestedAt: requestedAt, Readiness: readiness, Process: process})
	_, err = p.frame(strings.ToLower(prompt) + "-full-turn-idle")
	return err
}

func idleObserverProcess(pid int, executable string, processes []observedProcess) (observedProcess, error) {
	for _, current := range processes {
		if current.PID == pid && pid > 0 && current.StartTicks != "" && current.Executable == executable && executable != "" {
			return current, nil
		}
	}
	return observedProcess{}, fmt.Errorf("idle readiness lacks owned observer PID provenance")
}
