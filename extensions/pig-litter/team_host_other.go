//go:build !linux

package piglitter

func peerHostGone(_, _ peerHostIdentity) bool { return false }
