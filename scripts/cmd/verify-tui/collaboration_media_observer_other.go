//go:build !linux

package main

import "errors"

func collaborationMonotonicNow() (int64, error) {
	return 0, errors.New("terminal startup measurement requires Linux CLOCK_MONOTONIC")
}

func runCollaborationMediaObserver(receipt, tmux, socket, session string) error {
	return errors.New("live terminal media observer requires Linux")
}
