# Unselected startup

## Sub-features

- Plain PiG starts without loading the project extension.
- The startup status and diagnostic notification remain absent.

## How to get to it (user POV)

From the repository root, run `devenv shell -- pig`. This does not select the direct extension or the local Piglet.

## Driving it with tmux

Run `devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh`. The helper waits for the isolated plain PiG pane, saves its ready screen, and checks for the absence of both Pig Litter literals.

## Gotchas

The isolated process has no provider credentials and runs offline, so PiG can display “No models available.” The helper treats that warning as a ready PiG screen only while the pane is live.
