# Direct extension selection

## Sub-features

- The extension sets a compact status when its session starts.
- The `/pig-litter` command shows a separate diagnostic notification.

## How to get to it (user POV)

From the repository root, run `devenv shell -- pig -e ./extensions/pig-litter`. The extension loads only because this command selects it.

## Driving it with tmux

Run `devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh`. The helper captures the startup status, records `/pig-litter` as the action, then captures the notification and retained status.

## Gotchas

The verifier selects **Trust (this session only)** only after it captures and checks PiG's visible project-trust prompt. It does not use `--approve`. A missing-provider warning is expected in the isolated offline session.
