//go:build p08native && !p08fixture

package piglitter

import "github.com/MichaelKinsy/PiG/coding/extension"

func (member *nativeMember) fixtureHooks(extension.API) {}

func (member *nativeMember) fixtureReceiptBoundary(memberInputWire, string) {}
