---
name: verify-pig-litter
description: "Verify Pig Litter's direct extension selection, named or local Piglet selection, status, diagnostic command, and foreground child delegation through the terminal UI. Use for installed user Piglets and real models, or for pinned offline and deterministic fixture checks."
---

# Verify Pig Litter

Choose the check that matches the claim. Use the live helper for an installed named Piglet and a real provider. Use the pinned offline helper for repository selection controls. Keep deterministic provider failures and cancellation checks in the existing fixture.

Run from the repository root. When maintaining this skill, the coordinator owns every PiG drive, including doctor probes. Code owners can run syntax checks and pure helper tests.

## Configure the live run

Use the PiG executable and configuration root that already contain the selected Piglet and model credentials. The live helper inherits `HOME`, `PIG_HOME`, PiG agent-directory overrides, and provider environment variables. It never reads, copies, or writes `auth.json` or `models.json`.

Pass `--piglet NAME_OR_PATH` to select another Piglet. The default is the named `pig-litter` Piglet. Pass `--model PROVIDER/MODEL` for an exact model, or omit it to use normal PiG and Piglet defaults. `PIG_LITTER_MODEL` in the commands below is a caller-supplied shell variable containing that exact model.

Set `PIG_HOME` to the configuration root you already use, or supply `--pig-home PATH`. Do not substitute an empty temporary root for a live credential check. An inherited PiG agent-directory override remains in effect even when `--pig-home` is supplied. A readiness mismatch must identify the checked root and agent directory before anyone changes authentication.

Use `--pig-bin PATH` to select another PiG executable. An owned temporary `pig` symlink on `PATH` makes the extension's child command use that same executable, even when its original filename differs. The live doctor uses PiG's actual selected runtime and extension build. It does not require the devenv Go executable or a fixed tmux version.

The helper clears only `PIG_PIGLET_NAME` and `PIG_PIGLET_PATH` to prevent an ambient selector from overriding the explicit Piglet or plain control. It preserves credential environment variables without recording their values. No run input persists a model preference.

## Launch the live proof

Check the selected configuration, source resolution, model availability, and authentication metadata first.

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs doctor \
  --piglet pig-litter --model "$PIG_LITTER_MODEL"
```

The doctor starts a bounded RPC metadata probe to build and inspect the selected runtime. It requests no model completion. It retains only selected model identifiers, command names, source origins, and configuration paths. The authentication check uses `--no-refresh` and never uses `--credentials` or `--diagnose`.

Drive the named Piglet with the real provider.

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs \
  --piglet pig-litter --model "$PIG_LITTER_MODEL" --case selected
```

Use `--case all` for the named positive proof followed by direct repository selection and plain startup controls. Direct and plain cases make no model request. The positive proof always uses `--piglet`; it never substitutes `-e` for named selection.

Use `--case cancel` for an optional real-provider cancellation drive. It sends Escape after it observes a live child and parent tool call. It requires that child's exit, a parent cancellation acknowledgement, and a new diagnostic notification after a fresh slash action. Keep this observational result separate from the controlled timeout, provider-error, and cancellation outcomes in the deterministic fixture.

The command and polling deadlines are configurable through `--command-timeout-ms` and `--timeout-ms`. Both accept 1000 to 600000 milliseconds. Use `--evidence-dir PATH` to choose the retained evidence root. Run `live.mjs --help` to list all inputs.

## Drive and assess the live proof

The helper creates one private tmux socket, disposable workspace, and private parent session directory. It starts fresh sessions serially and runs the doctor before each session. `--approve` applies only to that owned scratch workspace for the current process. It never creates persistent trust for the actual project.

The selected session verifies startup status and `/pig-litter`, then requests scout inspection and worker write/readback through `pig_litter_agent`. Parent built-in file tools are disabled with `--no-builtin-tools`. The scout input token exists only in the scratch file, not in the parent request. The worker writes another disposable token and reports it after readback.

A passing run requires actual parent session `toolCall` and `toolResult` records, completed child outcomes, inherited exact model identifiers, bounded reports, live scout and worker child process receipts, and the expected file effects. Parent prose or a visible success message alone is insufficient. The production children remain ephemeral with `--no-session`.

Plain startup preserves ordinary user discovery and uses normal configured TUI readiness. If ambient user resources load Pig Litter without explicit selection, record that observed configuration instead of declaring the control passed.

After a failure, retain the screen and available session evidence, clean the failed instance, and run a fresh doctor when possible. Never continue a wedged instance. After each normal drive, require status zero, no terminating signal, and termination of the observed parent and extension processes.

## Retain live evidence and clean up

Live evidence is saved under `.pstack/evidence/pig-litter-live/<run-id>/` by default. `run.json` records run inputs, doctor results, actions, selected source origins, session projections, file markers, process identities, exit status, failure, and cleanup. Screen captures and parent session JSONL remain beside it. Aborted tool results can be plain error text rather than a structured completed handback. Retain that text and error state without treating it as completion. Do not record the inherited environment or credential contents.

The helper handles interruption, stops only its private tmux server and observed owned processes, and removes only its scratch directory. Evidence survives failure and forced cleanup. If any owned process remains, the run fails and preserves scratch for investigation.

## Run the pinned offline controls

Use the existing devenv lane to verify repository-local selection without model requests.

```sh
devenv shell -- check-pig-extension
devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh
```

The pinned offline controls in `verify.sh` check PiG 0.4.1, Go 1.27.1, and tmux 3.7c. Its read-only doctor is `verify.sh doctor`. It strips inherited credentials, uses temporary PiG state and Go caches, and runs direct `-e ./extensions/pig-litter`, local `--piglet ./piglet.yaml`, and unselected PiG launches offline.

The selected controls require status `foreground scout and worker`, then notification `Pig Litter ready. Use pig_litter_agent with scout or worker. Foreground only.` They capture and select **Trust (this session only)** when prompted. The unselected isolated control requires the pinned runtime's no-model warning and absence of the Pig Litter literals.

The helper saves trust, screen, action, process, and clean-exit evidence under `.pstack/evidence/pig-litter/`. It refuses existing project `.pig` state and never removes user project state. Its exit trap stops only its private tmux server and owned scratch resources.

## Run deterministic delegation checks

Keep the fixture's claims separate from a real-provider run.

```sh
devenv shell -- scripts/test-pig-litter.sh
devenv shell -- bun scripts/verify-child-delegation.mjs
```

The Go tests use the SDK embedded in the pinned PiG binary. They cover malformed JSON, absent terminal evidence, incomplete outcomes, ordering, usage, literal tasks, and bounds. The loopback fixture uses temporary model configuration and direct repository extension selection. It proves controlled missing-model, provider-error, timeout, busy, headless, and cancellation behavior. Timeout and cancellation both map to `stopped` in the public result.

Check the live helper without starting PiG.

```sh
bun test ./.agents/skills/verify-pig-litter/scripts/live.test.mjs
```

Keep the [feature map](features/README.md) aligned with the source and observed drive results.
