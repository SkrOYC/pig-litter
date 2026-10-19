package main

import (
	"fmt"
	"os"
	"regexp"

	"github.com/MichaelKinsy/PiG/extensions/sdk"
	"github.com/SkrOYC/pig-litter/scripts/internal/tuiidle"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	command, directory := os.Getenv("PIG_TUI_IDLE_COMMAND"), os.Getenv("PIG_TUI_IDLE_DIRECTORY")
	if !regexp.MustCompile(`^fixture-idle-[a-z0-9]+$`).MatchString(command) || directory == "" {
		return fmt.Errorf("missing private fixture idle configuration")
	}
	ext := sdk.New("tui-idle-observer")
	ext.RegisterCommand(command, sdk.CommandOptions{Description: "Observe private fixture turn settlement", Handler: func(ctx sdk.Context, nonce string) error {
		if !regexp.MustCompile(`^[A-Za-z0-9]+$`).MatchString(nonce) {
			return fmt.Errorf("invalid fixture readiness nonce")
		}
		readiness := tuiidle.Observe(command, nonce, ctx.WaitForIdle, ctx.IsIdle, ctx.SessionManager().GetSessionID)
		return tuiidle.Write(directory, readiness)
	}})
	return ext.Run()
}
