# Piglet selection

## Behavior

A named user Piglet selects its registered source and declared extension origins. `pig --piglet pig-litter` uses the active PiG configuration root. A path argument such as `./piglet.yaml` selects that source directly. The repository-local Piglet resolves `local:./extensions/pig-litter` relative to its file.

The selected extension shows startup status and handles `/pig-litter`. Source resolution and extension build must be observed through the selected runtime. A repository-local fixture does not establish named user resolution or Git Resource provenance.

## Drive named user selection

Use the live helper's doctor with the active config root and exact model supplied externally.

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs doctor \
  --piglet pig-litter --model "$PIG_LITTER_MODEL"
```

The doctor records the resolved Piglet source, declared extension origins, selected model, availability, and safe authentication metadata. It does not substitute a temporary PiG home or require a separate Go executable on `PATH`.

Run the same helper without `doctor` for startup, diagnostic, and real scout/worker proof. The actual launch contains `--piglet pig-litter`, not a direct extension replacement. Capture the source origin and revision that the named user declaration resolves for that run.

## Drive repository selection

Run `devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh`. Its local Piglet case captures status, `/pig-litter`, notification, and clean exit with pinned offline PiG.

## Configuration

The live helper inherits `PIG_HOME` and agent-directory overrides. Supply `--pig-home` if the selected runtime should use another existing root. A default root mismatch is a configuration mismatch, not evidence that the user must authenticate again. Ambient Piglet selectors are cleared so the explicit name or path controls the drive. Live trust applies only to the owned disposable workspace.
