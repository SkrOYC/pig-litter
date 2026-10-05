# Direct extension selection

## Behavior

Explicit `pig -e ./extensions/pig-litter` loads the extension, sets status `foreground scout and worker`, and registers `/pig-litter`. The command shows a separate notification. Selection and the notification make no model request.

## Drive

Run `.agents/skills/verify-pig-litter/scripts/live.mjs --case direct` with the installed PiG and inherited configuration. The helper starts from owned scratch, selects the repository extension by absolute path, verifies status and the diagnostic, and requires clean process exit. It performs a runtime doctor before the drive.

Run `devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh` for the pinned offline control. It records the visible session-only trust prompt, startup status, action, and notification.

## Evidence limits

Direct repository selection proves that entry point and diagnostic. It does not establish named user Piglet discovery, Git origin resolution, or a real provider completion. Use the selected named-Piglet live case for those claims.
