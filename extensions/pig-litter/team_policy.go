package piglitter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"
)

type teamPolicy struct {
	TaskCompletion []string `json:"taskCompletion"`
	TeammateIdle   []string `json:"teammateIdle"`
}

type policyDecision struct {
	Permit   *bool  `json:"permit"`
	Feedback string `json:"feedback"`
}

type policyOutput struct {
	bytes.Buffer
}

func (b *policyOutput) Write(data []byte) (int, error) {
	if b.Len()+len(data) > 8192 {
		return 0, fmt.Errorf("policy output exceeds 8192 bytes")
	}
	return b.Buffer.Write(data)
}

func loadTeamPolicy(agentDir string) (teamPolicy, error) {
	var policy teamPolicy
	path := filepath.Join(agentDir, "team-policy.json")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return policy, nil
	}
	if err != nil {
		return policy, err
	}
	if !info.Mode().IsRegular() || info.Size() > configBytes {
		return policy, fmt.Errorf("%s must be a bounded regular file", path)
	}
	file, err := openConfig(path)
	if err != nil {
		return policy, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		return policy, fmt.Errorf("%s changed while opening", path)
	}
	data, err := io.ReadAll(io.LimitReader(file, configBytes+1))
	if err != nil || len(data) > configBytes || !utf8.Valid(data) || len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' || checkUniqueJSON(data) != nil {
		return policy, fmt.Errorf("%s contains invalid policy configuration", path)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&policy); err != nil {
		return policy, fmt.Errorf("%s: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return policy, fmt.Errorf("%s must contain one policy object", path)
	}
	for _, command := range [][]string{policy.TaskCompletion, policy.TeammateIdle} {
		if len(command) > 16 || len(command) != 0 && !filepath.IsAbs(command[0]) {
			return policy, fmt.Errorf("%s policy commands need an absolute executable and at most 16 arguments", path)
		}
		for _, arg := range command {
			if len(arg) > 4096 || !utf8.ValidString(arg) || strings.ContainsRune(arg, '\x00') {
				return policy, fmt.Errorf("%s contains an invalid policy argument", path)
			}
		}
	}
	return policy, nil
}

func runTeamPolicy(ctx context.Context, command []string, candidate any) (bool, string) {
	if len(command) == 0 {
		return true, ""
	}
	input, err := json.Marshal(candidate)
	if err != nil || len(input) > 1024*1024 {
		return false, "Policy input could not be prepared."
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	process := exec.CommandContext(ctx, command[0], command[1:]...)
	process.WaitDelay = time.Second
	process.Stdin = bytes.NewReader(input)
	var output policyOutput
	process.Stdout = &output
	process.Stderr = io.Discard
	if err := process.Run(); err != nil {
		return false, "Policy command failed or timed out."
	}
	data := output.Bytes()
	if !utf8.Valid(data) || checkUniqueJSON(data) != nil {
		return false, "Policy returned invalid feedback."
	}
	var decision policyDecision
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decision); err != nil || decision.Permit == nil {
		return false, "Policy returned invalid feedback."
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return false, "Policy returned invalid feedback."
	}
	if *decision.Permit {
		if decision.Feedback != "" {
			return false, "Policy returned contradictory feedback."
		}
		return true, ""
	}
	if strings.TrimSpace(decision.Feedback) == "" || len(decision.Feedback) > 4096 || !utf8.ValidString(decision.Feedback) {
		return false, "Policy refusal needs bounded feedback."
	}
	return false, decision.Feedback
}
