//go:build p08native

package piglitter

import (
	"context"
	"testing"
	"time"
)

func TestDependentMemberInputKeepsCauseAndWindowLifetime(t *testing.T) {
	type causeKey struct{}
	window, closeWindow := context.WithCancel(context.Background())
	cause, closeCause := context.WithCancel(context.WithValue(context.Background(), causeKey{}, "callback"))
	input, release := dependentMemberInputContext(window, cause)
	defer release()
	closeCause()
	if input.Value(causeKey{}) != "callback" || input.Err() != nil {
		t.Fatal("parent receipt release removed the dependent cause or cancelled its input")
	}
	closeWindow()
	select {
	case <-input.Done():
	case <-time.After(time.Second):
		t.Fatal("closing the owning window did not cancel the dependent input")
	}

	input, release = dependentMemberInputContext(context.Background(), context.WithValue(context.Background(), causeKey{}, "callback"))
	release()
	if input.Err() == nil {
		t.Fatal("releasing the dependent ticket did not cancel its input")
	}
}
