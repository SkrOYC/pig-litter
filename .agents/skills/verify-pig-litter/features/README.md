# Verification features

This map covers the shipped foreground delegation and selection behavior.

- [Foreground child delegation](foreground-delegation.md) proves real scout and worker children, bounded handback, failures, timeout, and cancellation.
- [Direct extension selection](direct-extension.md) proves `pig -e ./extensions/pig-litter` loads the status and diagnostic command.
- [Piglet selection](piglet-selection.md) proves `pig --piglet ./piglet.yaml` loads the same behavior through the local Piglet.
- [Unselected startup](unselected-startup.md) proves plain `pig` does not load Pig Litter.
