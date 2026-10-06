package main

import "os/exec"

func prepareProcess(*exec.Cmd) {}
func stopProcess(command *exec.Cmd) {
	if command.Process != nil {
		_ = command.Process.Kill()
	}
}
