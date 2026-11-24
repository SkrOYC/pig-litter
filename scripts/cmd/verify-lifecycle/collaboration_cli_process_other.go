//go:build !linux

package main

import (
	"errors"
	"os/exec"
)

func collaborationCLIProcessIdentity(*exec.Cmd) (string, error) {
	return "", errors.New("process identity checking is unavailable on this platform")
}

func collaborationCLIForceOwned(*exec.Cmd, string) error {
	return errors.New("cannot verify process identity for forced cleanup on this platform")
}
