//go:build !linux && !darwin

package piglitter

func peerHostGone(_, _ peerHostIdentity) bool { return false }
