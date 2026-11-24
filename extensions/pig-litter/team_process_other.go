//go:build !linux

package piglitter

import (
	"context"
	"errors"
)

func processBirth(int) (string, error) {
	return "", errors.New("tmux process identity is unavailable on this platform")
}

func memberProcessDigest(context.Context, int, string) (string, error) {
	return "", errors.New("native process identity is unavailable on this platform")
}
