//go:build !linux

package piglitter

import "errors"

func processBirth(int) (string, error) {
	return "", errors.New("tmux process identity is unavailable on this platform")
}
