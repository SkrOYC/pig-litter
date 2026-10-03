# Piglet selection

## Sub-features

- The local Piglet resolves the Go extension relative to `piglet.yaml`.
- The selected extension sets its startup status and handles `/pig-litter`.

## How to get to it (user POV)

From the repository root, run `devenv shell -- pig --piglet ./piglet.yaml`. The Piglet selects `./extensions/pig-litter` from its local origin.

## Driving it with tmux

Run `devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh`. The helper captures the status before it sends `/pig-litter`, then captures the distinct notification and retained status.

## Gotchas

The helper runs the real Piglet from the repository root, so its relative extension origin matches the user's launch. It accepts only PiG's visible **Trust (this session only)** option.
