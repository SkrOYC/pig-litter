//go:build !linux && !darwin

package piglitter

import "errors"

func embeddedPeerHost() (peerHostIdentity, error) {
	return peerHostIdentity{}, errors.New("peer process identity is unavailable")
}
