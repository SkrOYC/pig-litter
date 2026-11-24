//go:build p08native && p08fixture

package piglitter

import (
	"context"
	"time"

	"github.com/MichaelKinsy/PiG/coding/extension"
)

func (member *nativeMember) fixtureHooks(api extension.API) {
	api.OnInput(func(ctx context.Context, event extension.InputEvent) (extension.InputEventResult, error) {
		if event.Text == "CAUSE_INNER" {
			var reply map[string]bool
			_ = member.call(ctx, "/fixture/hold-inner", map[string]bool{}, &reply)
			return extension.InputEventResultContinue{}, nil
		}
		if event.Text != "CAUSE_OUTER" {
			return extension.InputEventResultContinue{}, nil
		}
		var reply map[string]bool
		if err := member.call(ctx, "/fixture/reentrant", map[string]bool{}, &reply); err != nil {
			member.stop(ctx)
			return extension.InputEventResultHandled{}, nil
		}
		return extension.InputEventResultContinue{}, nil
	})
}

func (member *nativeMember) fixtureReceiptBoundary(input memberInputWire, path string) {
	if input.Text != "CAUSE_OUTER" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var reply map[string]bool
	_ = member.call(ctx, path, map[string]bool{}, &reply)
}
