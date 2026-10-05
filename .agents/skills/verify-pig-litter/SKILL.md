---
name: verify-pig-litter
description: "Drive PiG's Go extension selection through the terminal UI. Use when changing or verifying Pig Litter's direct extension selection, Piglet selection, diagnostic command, status, or unselected startup."
---

# Verify Pig Litter

Use this skill to prove the shipped PiG selection through its real terminal UI. Run it from the repository root.

## Launch

First verify extension registration and the local Piglet:

```sh
devenv shell -- check-pig-extension
```

Then run the complete terminal UI proof:

```sh
devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh
```

The helper starts each entry point in a private tmux server:

```sh
pig --offline --no-session -e ./extensions/pig-litter
pig --offline --no-session --piglet ./piglet.yaml
pig --offline --no-session
```

The first two launches select Pig Litter. The last launch leaves it unselected. The helper strips inherited credentials, preserves the current `HOME`, uses temporary PiG directories and Go caches, and sets PiG and Go offline flags. PiG can show a “No models available” warning. That warning does not block the local extension command.

When PiG asks to trust the repository, the helper records the prompt and navigates to **Trust (this session only)**. It waits up to five seconds for the selected option to appear before it presses Enter. An unexpected prompt or selection fails the run. It never trusts the parent folder or stores persistent trust.

## Doctor

Run the read-only environment check before investigating a failed drive:

```sh
devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh doctor
```

The doctor checks the pinned PiG path and version, Go 1.27.1, tmux 3.7c, `ps`, the extension source, `piglet.yaml`, and the absence of project-local `.pig` state. It does not start PiG or change files. The TUI helper runs the doctor before it launches any pane.

## Drive

The helper runs each real launch from the repository root, using a fresh temporary `PIG_HOME` for each case. It starts direct, Piglet, and unselected cases one at a time. Each case gets a distinct tmux session on the helper's private socket.

For a selected launch, the helper waits for a live PiG pane and the startup status `foreground scout and worker`. It captures that screen before it sends `/pig-litter`, records the action, then waits for the separate notification `Pig Litter ready. Use pig_litter_agent with scout or worker. Foreground only.` The final capture must show both literals.

For plain PiG, the helper waits for a live pane and the pinned build's “No models available” startup text. The capture must contain neither Pig Litter literal.

After each case, the helper sends Ctrl+D and requires tmux to report a dead pane, exit status 0, and no terminating signal. It records all PiG and extension descendant PIDs before exit, then verifies that every recorded PID has ended.

## Evidence

The helper saves evidence under `.pstack/evidence/pig-litter/<run-id>/`. Each selected case has a trust prompt capture when PiG requests trust, a trust-selection capture, a before capture, an action record, an after capture, process-tree captures, and an exit record. The unselected case has a ready capture and process-tree and exit records. `run.json` records tool versions, launch arguments, assertions, process cleanup, and scratch cleanup.

The report preserves the first failure in `failure`. It records cleanup problems separately in `cleanup.failureReasons` and any owned PIDs still alive after forced cleanup in `cleanup.remainingPids`.

The TUI proof drives PiG's actual slash command and checks the visible status and notification. The separate offline smoke checks that the extension exposes exactly pig_litter_agent to the model. These selection checks make no model request or network call. Run `devenv shell -- bun scripts/verify-child-delegation.mjs` for foreground delegation through the local provider fixture.

## Cleanup

The helper uses a unique socket path under a temporary directory and an empty tmux configuration with dead panes retained for exit checks. Its exit trap kills only that tmux server, removes its PiG state and Go caches, then writes the final result under `.pstack/evidence/`. It leaves the current `HOME` and project files untouched. The doctor refuses to run if `.pig` project state already exists, and the helper checks for new `.pig` state after every case. Evidence survives success and failure.

## Helpers

The executable helper is `.agents/skills/verify-pig-litter/scripts/verify.sh`. Run the full proof or its read-only doctor as shown above.

Update the [feature map](features/README.md) when shipped selection behavior changes. Use `$maintain-verification-skill` to keep the map accurate as the app changes.
