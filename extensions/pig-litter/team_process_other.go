//go:build !linux && !darwin

package piglitter

import (
	"context"
	"errors"
)

type memberProcessHandle struct{}

func openMemberProcess(int, string, string) (*memberProcessHandle, error) {
	return nil, errors.New("native process handles are unavailable on this platform")
}

func (*memberProcessHandle) kill() error {
	return errors.New("native process handles are unavailable on this platform")
}
func (*memberProcessHandle) close() error { return nil }

func processBirth(int) (string, error) {
	return "", errors.New("tmux process identity is unavailable on this platform")
}

func memberProcessScope(int) (string, error) {
	return "", errors.New("native process scope is unavailable on this platform")
}

func memberProcessMatchesExecutable(int, string, string, string) (bool, error) {
	return false, errors.New("native process identity is unavailable on this platform")
}

func memberProcessDigest(context.Context, int, string, string) (string, error) {
	return "", errors.New("native process identity is unavailable on this platform")
}

func memberProcessPath(int, string, string) (string, error) {
	return "", errors.New("native process identity is unavailable on this platform")
}
