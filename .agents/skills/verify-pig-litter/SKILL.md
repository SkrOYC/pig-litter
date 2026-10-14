---
name: verify-pig-litter
description: "Verify Pig Litter's direct and Piglet selection, background children, persisted UUID resume, child working directories, terminal tools, diagnostic command, and unselected startup through the terminal UI. Use real providers or isolated deterministic fixtures."
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

To verify uncommitted repository changes, pass `--piglet /ABSOLUTE_PATH/piglet.yaml`. Check the doctor's resolved command source before making model requests. An installed Git origin can resolve a different revision. Record installed selection separately from the changed-build proof.

The helper starts each case in fresh scratch with a private tmux socket and parent session directory. It runs a doctor before each session. `--approve` trusts only that disposable workspace for the process.

## Assess child sessions

Selected startup requires an editor displaying the exact model and scratch path. `/pig-litter` must explain the six background controls. There is no foreground startup status.

The selected proof discovers agents and spawns a scout to read a private token. The scout token exists only in its input file. It also spawns a worker with an explicit `cwd` and requires terminal commands to produce independently checked file effects there. Each child is a persisted PiG session with one stable UUID across turns, pause, and explicit resume. File tools and resources use the spawn directory. Role patterns expand against refreshed root tool enumeration; nested children cannot widen their parent's ceiling. MCP uses root configuration. The parent prompt restricts its own calls to `litter_*` controls; recorded calls enforce that restriction.

A passing run requires matching spawn admissions and completed outcomes by child ID, inherited exact models, the private scout token, and expected worker file effects. It also requires a live `Litter N live M kept` widget. Parent prose alone cannot pass. `litter_inspect` returns status and the latest result. It exposes no transcript or tool receipts.

The proof removes the scout's input before resuming it through `litter_message`. The resume message must omit the private token. Require recall in a completed result for the same ID, after the resume acknowledgement. `litter_inspect` exposes current status and the latest outcome. It does not expose transcript history or tool receipts. Real-provider observations cannot establish every internal tool call. Use deterministic provider requests to verify forbidden tools and exact child configuration.

## Check cancellation and pause

Run the optional real-provider cancellation case:

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs \
    --piglet pig-litter --model "$PIG_LITTER_MODEL" --case cancel
```

The helper waits for an active child turn and pending `litter_wait`, then sends **Escape**. It requires a recorded cancellation acknowledgement and a fresh diagnostic notification. Inspection must show the same UUID in paused status with a paused outcome before any explicit resume. Then require a `litter_message` call with `resume: true` for that UUID and a completed result containing the private file token after the resume acknowledgement. The resume message must omit the token. A cancelled wait can return plain tool error text; retain that text without treating it as completion.

A wait cancelled in isolation or a wait timeout leaves the child running. Root Escape during an active turn pauses retained child sessions. A parent pause cascades to its children, and an explicit resume acts on the selected node only. Child runs have no wall-clock deadline. Provider errors, tool-ceiling checks, scout leaf enforcement, nested workers, resume races, and controlled outcomes belong to deterministic fixtures.

## Retain evidence and clean up

Evidence stays under `.pstack/evidence/pig-litter-live/RUN_ID/` unless `--evidence-dir PATH` overrides the root. `run.json` records doctors, source provenance, actions, parent session projections, child results, file markers, observed process identities, and cleanup. Screens and parent session JSONL stay beside it. Don't record credentials or the inherited environment.

After a failed drive, preserve available evidence, clean the instance, and run a fresh doctor when possible. Don't continue a wedged instance. Normal drives must exit with status zero, no signal, and no observed owned parent or extension process remaining. Cleanup removes only owned scratch and private tmux resources. If cleanup fails, preserve scratch and report failure.

Both `--command-timeout-ms` and `--timeout-ms` accept 1000 to 600000 milliseconds. These bound the verification drive, not the product's child runs. Run `live.mjs --help` for every input.

Plain startup preserves ordinary resource discovery. Its doctor must show no `/pig-litter` registration, and its screen must show no child widget or diagnostic. If user configuration loads Pig Litter, report that observed selection rather than passing the unselected control.

## Run pinned terminal controls

Use the repository's deterministic terminal driver through the skill wrapper:

```sh
devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh doctor
devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh
```

The wrapper checks PiG 0.4.1, Go 1.27.1, and tmux 3.7c. It runs direct, local Piglet, and unselected cases using the maintained product terminal fixture. The fixture uses isolated credentials and a loopback provider, prebuilt Go code, private scratch, and its own tmux server. It exercises tool registration, children, widget, resize, editor, help, stopping, scrollback, and pane and socket cleanup. Evidence stays under `.pstack/evidence/go-tui/`. These checks don't prove real provider readiness or record PiG's exit status.

Run the wrappers with writable build output and `/tmp`. The terminal and lifecycle drivers do not forward `TMPDIR` to their child environments. To keep source read-only during review, build into owned scratch and use copies of the drivers that forward `TMPDIR`. Choose a short physical temporary path for Unix sockets and canonical directory comparisons. Preserve the driver diff and all original assertions.

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
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario cwd
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario unbounded
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario codemode
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario pause
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario admission
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario persistence
```

The scenarios cover configuration layering, agent overrides, file and terminal authority, child working directories, concurrency, depth, privacy, persisted UUID resume, subtree pausing, large native histories, completion delivery, and provider/tool failures. The `persistence` case reopens the same root in a second stock PiG process and checks retained child identity, directory, conversation, names, and actual stop outcome. It covers orderly restart. Go runtime tests cover soft turn allowances, root glob ceilings, native skills, HTTP MCP, codemode, deferred search, late root tools on resume, transport shutdown, allowed child tools under false host trust, recovery of accepted input, and completion write failures. Keep these claims separate from real-provider proof. Check helper evidence parsing and bounded command teardown without launching PiG:

```sh
bun test ./.agents/skills/verify-pig-litter/scripts/live.test.mjs
```

Replay actual inspect and stop handbacks from a completed `core` or `persistence` lifecycle fixture:

```sh
bun .agents/skills/verify-pig-litter/scripts/replay.mjs "PATH_TO_STDOUT_JSONL"
```

Replace `PATH_TO_STDOUT_JSONL` with `stdout.jsonl` in the fixture's printed evidence directory. The replay requires successful inspect and stop results and applies the live helper's envelope and outcome checks. It makes no model request and does not establish real-provider readiness.

The `cwd` scenario checks relative file operations and terminal effects in the selected directory, including nested children. The `unbounded` scenario keeps a child active beyond the preceding two-minute deadline and requires its completed result. Its outer fixture deadline bounds verification teardown. It does not impose a product run deadline.

Keep the [verification feature map](features/README.md) aligned with source and observed drives.
