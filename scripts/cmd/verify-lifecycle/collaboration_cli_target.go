package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func (runner *collaborationCLIRunner) verifyTarget(ctx context.Context, name string, target collaborationCLITarget) error {
	if !filepath.IsAbs(target.Checkout) {
		return errors.New("target checkout must be an absolute path")
	}
	git, err := exec.LookPath("git")
	if err != nil {
		return err
	}
	devenv, err := exec.LookPath("devenv")
	if err != nil {
		return err
	}
	readOutput := func(step string, command []string) (string, error) {
		directory, err := runner.executeAt(ctx, name+"-"+step, command, target.Checkout)
		if err != nil {
			return "", err
		}
		data, err := os.ReadFile(filepath.Join(directory, "stdout.log"))
		return strings.TrimSpace(string(data)), err
	}
	commit, err := readOutput("commit", []string{git, "rev-parse", "HEAD"})
	if err != nil {
		return fmt.Errorf("read checkout HEAD: %w", err)
	}
	if commit != target.Identity.ProductCommit {
		return errors.New("checkout HEAD does not match product commit")
	}
	if err := runner.verifyTargetBranch(ctx, name, git, target); err != nil {
		return err
	}
	clean := func(step string) error {
		status, err := readOutput(step, []string{git, "status", "--porcelain", "--untracked-files=all"})
		if err != nil {
			return err
		}
		if status != "" {
			return errors.New("target checkout has tracked or untracked changes")
		}
		return nil
	}
	if err := clean("status-before"); err != nil {
		return err
	}
	buildName := name + "-rebuild"
	output := filepath.Join(runner.directory, buildName, "pig-litter")
	script := "set -eu\ncommand -v pig\ngo version\ngo -C scripts build -p 2 -mod=readonly -buildvcs=false -o \"$1\" ./cmd/pig-litter"
	buildOutput, err := readOutput("rebuild", []string{devenv, "shell", "--", "bash", "-c", script, "collaboration-build", output})
	if err != nil {
		return fmt.Errorf("resolve and rebuild target in its declared environment: %w", err)
	}
	lines := strings.Split(buildOutput, "\n")
	if len(lines) != 2 || !strings.HasPrefix(lines[1], "go version ") {
		return errors.New("target build did not report the selected host and Go version")
	}
	host := lines[0]
	if !filepath.IsAbs(host) {
		return errors.New("checkout's declared PiG host path is not absolute")
	}
	hostHash, err := collaborationCLIDigest(host)
	if err != nil {
		return fmt.Errorf("hash the checkout's declared PiG host: %w", err)
	}
	if hostHash != target.Identity.HostBinarySHA256 {
		return errors.New("checkout's declared PiG host digest differs from target")
	}
	selectedInfo, selectedErr := os.Stat(target.Pig)
	hostInfo, hostErr := os.Stat(host)
	if selectedErr != nil || hostErr != nil || !os.SameFile(selectedInfo, hostInfo) {
		return errors.New("selected PiG executable differs from the checkout's declared host")
	}
	digest, err := collaborationCLIDigest(output)
	if err != nil {
		return fmt.Errorf("hash reproduced extension: %w", err)
	}
	if digest != target.Identity.ProductBinarySHA256 {
		return errors.New("reproduced extension digest differs from target")
	}
	if err := clean("status-after"); err != nil {
		return err
	}
	return collaborationCLICheckTarget(target)
}

func (runner *collaborationCLIRunner) verifyTargetBranch(ctx context.Context, name, git string, target collaborationCLITarget) error {
	branchDir, err := runner.executeAt(ctx, name+"-branch-name", []string{git, "check-ref-format", "--branch", target.Identity.Branch}, target.Checkout)
	if err != nil {
		return fmt.Errorf("invalid target branch: %w", err)
	}
	branch, err := os.ReadFile(filepath.Join(branchDir, "stdout.log"))
	if err != nil || strings.TrimSpace(string(branch)) != target.Identity.Branch {
		return errors.New("target branch name did not resolve exactly")
	}
	local := "refs/heads/" + target.Identity.Branch
	remote := "refs/remotes/origin/" + target.Identity.Branch
	directory, err := runner.executeAt(ctx, name+"-branch-membership", []string{git, "for-each-ref", "--contains", target.Identity.ProductCommit, "--format=%(refname) %(objectname)", local, remote}, target.Checkout)
	if err != nil {
		return fmt.Errorf("check target branch membership: %w", err)
	}
	data, err := os.ReadFile(filepath.Join(directory, "stdout.log"))
	if err != nil {
		return err
	}
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && (fields[0] == local || fields[0] == remote) && collaborationCommit.MatchString(fields[1]) {
			path := filepath.Join(runner.directory, name+"-branch-identity.json")
			receipt := map[string]string{"branch": target.Identity.Branch, "target_commit": target.Identity.ProductCommit, "ref": fields[0], "ref_commit": fields[1]}
			if err := collaborationCLIWriteNew(path, receipt); err != nil {
				return err
			}
			artifact, err := runner.artifact(path, "log")
			if err != nil {
				return err
			}
			runner.artifacts = append(runner.artifacts, artifact)
			return nil
		}
	}
	return errors.New("target commit is not contained by the declared branch")
}
