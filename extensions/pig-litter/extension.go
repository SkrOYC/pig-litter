package pig_litter

import (
	sdk "github.com/MichaelKinsy/PiG/extensions/sdk"
)

const (
	statusKey      = "pig-litter"
	statusText     = "bootstrap only, no child delegation"
	diagnosticText = "Pig Litter is loaded. This bootstrap has no child delegation."
)

func Extension() *sdk.Extension {
	e := sdk.New("pig-litter")

	e.Command("pig-litter", "Show the Pig Litter bootstrap diagnostic.", func(ctx sdk.Context, _ string) error {
		ctx.Notify(diagnosticText, "info")
		return nil
	})

	e.OnSessionStart(func(ctx sdk.Context, _ map[string]any) (any, error) {
		ctx.SetStatus(statusKey, statusText)
		return nil, nil
	})

	return e
}
