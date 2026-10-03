# Verification features

This map covers the shipped bootstrap behavior only.

- [Direct extension selection](direct-extension.md) proves `pig -e ./extensions/pig-litter` loads the status and diagnostic command.
- [Piglet selection](piglet-selection.md) proves `pig --piglet ./piglet.yaml` loads the same behavior through the local Piglet.
- [Unselected startup](unselected-startup.md) proves plain `pig` does not load Pig Litter.
