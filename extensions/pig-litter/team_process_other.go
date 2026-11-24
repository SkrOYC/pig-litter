//go:build !linux

package piglitter

import (
	"context"
	"errors"
)

type memberProcessHandle struct{}

func openMemberProcess(int, string) (*memberProcessHandle, error) {
	return nil, errors.New("native process handles are unavailable on this platform")
}

func (*memberProcessHandle) kill() error {
	return errors.New("native process handles are unavailable on this platform")
}
func (*memberProcessHandle) close() error { return nil }

func processBirth(int) (string, error) {
	return "", errors.New("tmux process identity is unavailable on this platform")
}

func memberProcessMatchesExecutable(int, string, string) (bool, error) {
	return false, errors.New("native process identity is unavailable on this platform")
}

func memberProcessDigest(context.Context, int, string) (string, error) {
	return "", errors.New("native process identity is unavailable on this platform")
}
