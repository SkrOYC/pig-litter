//go:build !teammate_native

package piglitter

import "errors"

func NativeMemberAvailable() bool { return false }

func RunNativeMember(string) error {
	return errors.New("native teammate support is unavailable in this build")
}
