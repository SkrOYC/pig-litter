//go:build linux

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
)

func collaborationCLIProcessIdentity(command *exec.Cmd) (string, error) {
	if command.Process == nil {
		return "", errors.New("process has not started")
	}
	data, err := os.ReadFile("/proc/" + strconv.Itoa(command.Process.Pid) + "/stat")
	if err != nil {
		return "", err
	}
	end := strings.LastIndexByte(string(data), ')')
	if end < 0 {
		return "", errors.New("invalid process identity record")
	}
	fields := strings.Fields(string(data[end+1:]))
	if len(fields) <= 19 || fields[19] == "" {
		return "", errors.New("process start identity is absent")
	}
	return fields[19], nil
}

func collaborationCLIForceOwned(command *exec.Cmd, identity string) error {
	if identity == "" {
		return errors.New("cannot verify process identity for forced cleanup")
	}
	actual, err := collaborationCLIProcessIdentity(command)
	if err != nil {
		return err
	}
	if actual != identity {
		return fmt.Errorf("process identity changed before forced cleanup")
	}
	return syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
}
