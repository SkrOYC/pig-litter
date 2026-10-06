//go:build !windows

package piglitter

import (
	"os"
	"syscall"
)

func openConfig(path string) (*os.File, error) {
	return os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK|syscall.O_NOFOLLOW, 0)
}
