//go:build darwin && !cgo

package piglitter

import (
	"context"
	"errors"
)

type memberProcessHandle struct{}

func openMemberProcess(int, string) (*memberProcessHandle, error) {
	return nil, errors.New("native process identity needs a CGO-enabled build")
}
func (*memberProcessHandle) kill() error  { return errors.New("native process identity is unavailable") }
func (*memberProcessHandle) close() error { return nil }
func processBirth(int) (string, error) {
	return "", errors.New("native process identity is unavailable")
}
func memberProcessScope(int) (string, error) {
	return "", errors.New("native process identity is unavailable")
}
func memberProcessPath(int, string) (string, error) {
	return "", errors.New("native process identity is unavailable")
}
func memberProcessMatchesExecutable(int, string, string) (bool, error) {
	return false, errors.New("native process identity is unavailable")
}
func memberProcessDigest(context.Context, int, string) (string, error) {
	return "", errors.New("native process identity is unavailable")
}
