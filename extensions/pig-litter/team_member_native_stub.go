//go:build !p08native

package piglitter

import "errors"

func RunNativeMember(string) error {
	return errors.New("native teammate support is unavailable in this build")
}
