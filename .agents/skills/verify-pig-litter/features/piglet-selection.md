# Piglet selection

A named Piglet selects its registered source and declared extension origins under the active PiG configuration root. A path selects a declaration directly. Repository `piglet.yaml` resolves `local:./extensions/pig-litter` relative to that file.

The selected Resource registers background controls and `/pig-litter`, without a foreground startup status. The implementation lives in `extensions/pig-litter/adapter.go` and `operations.go`. Repository source alone doesn't prove what an installed Git origin resolves.

## Drive named selection

Supply an exact provider/model in `PIG_LITTER_MODEL` and run the doctor:

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs doctor \
    --piglet pig-litter --model "$PIG_LITTER_MODEL"
```

The doctor records the declaration path, declared origins, resolved command source, Git revision when available, model availability, and safe authentication metadata. It inherits the existing configuration root and agent-directory override.

Run the same helper without `doctor` for diagnostic, scout, worker, retained resume, widget, and clean-exit proof. The launch must contain `--piglet pig-litter`. A direct extension launch cannot establish named selection.

## Drive local selection

Pass `--piglet /ABSOLUTE_PATH/piglet.yaml --case selected` to drive the repository declaration with real credentials. For isolated selection and UI controls, run the pinned wrapper's local Piglet fixture. Preserve the distinction between local source and installed Git provenance.

## Configuration

The helper preserves credentials and clears only ambient Piglet selectors. Supply `--pig-home PATH` to choose another existing root. An inherited agent-directory override remains active. Identify both checked paths before changing authentication. Trust applies only to owned scratch for the process.
