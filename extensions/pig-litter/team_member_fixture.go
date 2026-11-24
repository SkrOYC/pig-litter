//go:build p08native && p08fixture

package piglitter

import (
	"context"

	"github.com/MichaelKinsy/PiG/coding/extension"
)

func (member *nativeMember) fixtureHooks(api extension.API) {
	api.OnInput(func(ctx context.Context, event extension.InputEvent) (extension.InputEventResult, error) {
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
