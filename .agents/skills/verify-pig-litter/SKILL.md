---
name: verify-pig-litter
description: "Verify Pig Litter's direct and Piglet selection, background child sessions, retained generations, diagnostic command, and unselected startup through the terminal UI. Use real providers or isolated deterministic fixtures."
---

# Verify Pig Litter

Use the live helper for installed Piglets and real providers. Use the pinned terminal fixture for isolated selection and UI controls. Keep real-provider observations separate from deterministic lifecycle checks. The coordinator owns every PiG drive, including doctors.

Run the commands from the repository root.

## Configure the live run

Use the PiG executable and configuration root that contain the selected Piglet and credentials. The helper inherits `HOME`, `PIG_HOME`, agent-directory overrides, and provider environment variables. It doesn't read, copy, or write `auth.json` or `models.json`.

Pass `--piglet NAME_OR_PATH` for a named or local Piglet; the default is `pig-litter`. Pass `--model PROVIDER/MODEL` for an exact model, or omit it to inherit configured defaults. `PIG_LITTER_MODEL` in the examples is a caller-supplied variable containing that exact identifier. Use `pig --offline --list-models SEARCH` to resolve the identifier before the drive.

Set `PIG_HOME` or pass `--pig-home PATH` to choose an existing configuration root. An inherited agent-directory override remains active. A readiness mismatch must identify the checked home and agent directory before authentication changes.

Use `--pig-bin PATH` to choose another executable. The live doctor uses the selected runtime and extension build; it doesn't require the pinned devenv toolchain.

The helper clears ambient `PIG_PIGLET_NAME` and `PIG_PIGLET_PATH`. It preserves credential variables without recording their values. It doesn't persist a model preference.

## Launch the live proof

Check source resolution, exact model availability, and safe authentication metadata:

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs doctor \
    --piglet pig-litter --model "$PIG_LITTER_MODEL"
```

The doctor requests RPC state, available models, and commands without requesting a completion. It records configuration paths, the declared origin, the resolved command source, and its Git revision when available. Authentication uses `--no-refresh`, without `--credentials` or `--diagnose`.

Drive the named Piglet with a real provider:

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs \
    --piglet pig-litter --model "$PIG_LITTER_MODEL" --case selected
```

Use `--case all` for selected delegation, direct repository selection, and plain startup, in that order. Direct and plain cases make no completion request. The selected case always uses `--piglet`.

The helper starts each case in fresh scratch with a private tmux socket and parent session directory. It runs a doctor before each session. `--approve` trusts only that disposable workspace for the process.

## Assess child sessions

Selected startup requires an editor displaying the exact model and scratch path. `/pig-litter` must explain the six background controls. There is no foreground startup status.

The selected proof discovers agents, spawns a scout to read a private token, and spawns a worker to write and read another token. The scout token exists only in its input file. The parent has canonical file tools available because children call them through the parent host. The parent prompt restricts its own calls to `litter_*` controls; recorded calls enforce that restriction.

A passing run requires matching spawn admissions and completed exact-generation outcomes, inherited exact models, successful child file calls in retained transcripts, and expected file effects. It also requires a live `Litter N live M kept` widget. Parent prose alone cannot pass.

The proof resumes the scout through `litter_message` and checks generation 2 and recall of the private token. The resume message must omit that token. The complete appended transcript must contain no tool calls. Child histories stay in memory inside the extension's Go SDK sessions. Child OS processes and persisted child sessions aren't expected.

## Check cancellation and stopping

Run the optional real-provider cancellation case:

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs \
    --piglet pig-litter --model "$PIG_LITTER_MODEL" --case cancel
```

The helper waits for a live child and pending `litter_wait`, then sends **Escape**. It requires a recorded cancellation acknowledgement and a fresh diagnostic notification. Inspection must show that cancelling the wait leaves the child running. An explicit `litter_stop` must then return a stopped outcome. A cancelled wait can return plain tool error text; retain that text without treating it as completion.

A wait timeout also leaves the child running. Provider errors, run deadlines, permission checks, nested delegation, stale generations, and controlled races belong to deterministic fixtures.

## Retain evidence and clean up

Evidence stays under `.pstack/evidence/pig-litter-live/RUN_ID/` unless `--evidence-dir PATH` overrides the root. `run.json` records doctors, source provenance, actions, parent session projections, child transcripts, file markers, observed process identities, and cleanup. Screens and parent session JSONL stay beside it. Don't record credentials or the inherited environment.

After a failed drive, preserve available evidence, clean the instance, and run a fresh doctor when possible. Don't continue a wedged instance. Normal drives must exit with status zero, no signal, and no observed owned parent or extension process remaining. Cleanup removes only owned scratch and private tmux resources. If cleanup fails, preserve scratch and report failure.

Both `--command-timeout-ms` and `--timeout-ms` accept 1000 to 600000 milliseconds. Run `live.mjs --help` for every input.

Plain startup preserves ordinary resource discovery. Its doctor must show no `/pig-litter` registration, and its screen must show no child widget or diagnostic. If user configuration loads Pig Litter, report that observed selection rather than passing the unselected control.

## Run pinned terminal controls

Use the repository's deterministic terminal driver through the skill wrapper:

```sh
devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh doctor
devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh
```

The wrapper checks PiG 0.4.1, Go 1.27.1, and tmux 3.7c. It runs direct, local Piglet, and unselected cases using the maintained product terminal fixture. The fixture uses isolated credentials and a loopback provider, prebuilt Go code, private scratch, and its own tmux server. It exercises tool registration, children, widget, resize, editor, help, stopping, scrollback, and clean exit. Evidence stays under `.pstack/evidence/go-tui/`. These checks don't prove real provider readiness.

## Run deterministic lifecycle checks

Use the pinned Go SDK and loopback lifecycle fixtures:

```sh
devenv shell -- scripts/test-pig-litter.sh
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario core
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario nested
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario checks
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario messages
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario lifecycle
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario completion
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario shadow
```

The scenarios cover file authority, concurrency, depth, privacy, resume, stale generations, subtree stopping, history limits, completion delivery, and provider/tool failures. Keep these claims separate from the live proof. Check helper evidence parsing and bounded command teardown without launching PiG:

```sh
bun test ./.agents/skills/verify-pig-litter/scripts/live.test.mjs
```

Keep the [verification feature map](features/README.md) aligned with source and observed drives.
