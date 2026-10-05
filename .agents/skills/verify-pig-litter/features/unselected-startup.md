# Unselected startup

## Behavior

Registering a named Piglet makes it available for explicit selection. Plain PiG still follows ordinary user resource discovery. Absence of a command-triggered notification alone does not prove that an extension was not loaded.

## Drive configured startup

Run `.agents/skills/verify-pig-litter/scripts/live.mjs --case plain` with the installed runtime and inherited configuration. The helper removes ambient Piglet selector variables, supplies no extension or Piglet flag, and preserves ordinary resource discovery and built-in tools.

Wait for a live configured TUI, capture it, and require absence of the Pig Litter status and notification. Use the selected named-Piglet case as the positive control. If ordinary user configuration loads Pig Litter, report that observed configuration rather than marking the unselected control passed.

## Drive isolated startup

Run `devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh` for the pinned offline control. It uses an empty temporary PiG home without credentials, waits for `No models available.`, and checks that the Pig Litter literals are absent. That warning is expected only for this isolated control.

Both paths require a live pane before assertions, normal exit, and termination of observed owned processes.
