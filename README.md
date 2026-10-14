# Pig Litter

Pig Litter is a selected Go Resource for released PiG 0.4.1. It starts independent background Sessions through the published Go SDK. Plain PiG has no Pig Litter tools.

## Build and select the Resource

The developer environment pins PiG 0.4.1 and Go 1.27.1 with CGO enabled. Build inside the devenv shell so Go can use its C compiler. With [direnv](https://direnv.net) installed, run `direnv allow` once and [.envrc](.envrc) loads that shell on entry.

```sh
devenv shell
scripts/test-pig-litter.sh
pig -e ./extensions/pig-litter
```

The source directory exports a conventional `Extension() *sdk.Extension` factory. [piglet.yaml](piglet.yaml) selects that directory. Its first load may fetch the exact dependencies in `go.mod` and `go.sum`. Standalone and verification commands live in a separate `scripts` Go module, outside the selected factory root.

Build the prebuilt standalone with the same script, then select it with `pig -e ./extensions/pig-litter/dist/pig-litter`. The prebuilt artifact needs no Go compiler, Node, or Bun at runtime. Product builds and tests use Go.

## Configure children

Put `litter.yaml` in `~/.config/pig/agent/` to configure children across projects. With no explicit configuration root, Pig Litter loads that file, then any settings in PiG's default `~/.pig/agent/litter.yaml`. Explicit `PIG_HOME`, `XDG_CONFIG_HOME`, agent-directory overrides, or Pi compatibility mode select their own user directory instead. Put [litter.yaml](litter.yaml) in a project root to override user settings for that project. Agent fields merge by name, so a project can change one field while retaining the user's other choices.

Either file can override `scout` or `worker`, including their role, instructions, tools, model, and delegation setting. Additional agent names use the same definition format. Changing a role resets omitted instructions, tools, and delegation to that role's defaults. The model stays inherited unless overridden. The scout role uses `read` and `ls` and cannot delegate. Workers default to `read`, `ls`, `write`, `edit`, `bash`, `skill:*`, `mcp__*`, `codemode`, and `tool_search`.

Each role has an explicit tool list. Named glob patterns such as `skill:*` and `mcp__docs__*` expand against the root Session's registered tools. Catch-all patterns are rejected. Scout definitions permit only `read` and `ls`.

Pig Litter refreshes the root registry on every spawn and resume, including activation. That registry includes inactive codemode and deferred tools and excludes hidden tools. Child directories cannot add tool names to the root ceiling. Nested delegation intersects each ancestor's admitted patterns with the refreshed registry. A root that omits codemode or MCP tools also omits them from children.

Children load PiG's native codemode, tool-search, and MCP extensions for their allowed tools. MCP connections use the root's configuration and extension registrations. Native MCP exposure controls discovery, including deferred tools loaded through `tool_search`. The same allowlist filters direct calls and calls through codemode. The child discovers skills and instructions from its own directory and reads skill files through its permitted tools.

For example, set a worker model across projects with this user configuration:

```yaml
agents:
  worker:
    model: PROVIDER/MODEL
```

Replace `PROVIDER/MODEL` with an exact configured model identifier. A project can override that model or add literal instructions under the same `worker` name.

The main Session is named `root`, has depth zero, and occupies no child slot. Depth two permits grandchildren through workers. A concurrency limit of four rejects a fifth live child without a launch queue. A second live writer in the same working directory is rejected.

Pig Litter mints each child UUID with PiG's UUIDv7 generator during admission. Spawn returns that UUID before creating a native Session. Children keep their UUID and immediate parent across turns, pause, and resume.

Configuration parsing rejects duplicate and unknown keys, aliases, invalid numeric types, empty documents, oversized files, nonregular files, and symlinks. Missing configuration files leave the preceding defaults in effect. The root configuration sets these shared limits:

| Setting | Default | Behavior |
| --- | ---: | --- |
| `concurrency` | 4 | Permits 1 to 16 live children across the tree. |
| `depth` | 2 | Permits depths from 0 to 8. Root has depth zero. |
| `max_result_bytes` | 50000 | Bounds retained reports. Accepts 1 byte to 1 MiB. |
| `max_mailbox` | 8 | Bounds queued messages and outstanding completion delivery. Accepts 1 to 1024. |
| `max_turns` | 50 | Sets a positive soft allowance for each execution, with no configured upper cap. |

At the turn allowance, the child receives one `<system-notice>` asking for a final partial report and remaining work. When the child settles, Pig Litter returns `partial` with that report. Explicit resume grants a fresh allowance. A child can continue beyond the allowance until it returns or you stop it. Child runs have no wall-clock deadline.

PiG owns persisted conversation history and context management. Pig Litter has no record-count or history-byte limit. `max_mailbox` controls delivery backpressure independently of conversation size.

## Use the tools

The Resource registers exactly `litter_spawn`, `litter_list`, `litter_inspect`, `litter_message`, `litter_stop`, and `litter_wait`.

Call `litter_list` to discover named agent types and your active direct children. It pages agent definitions and children independently. Paused and settled children are absent from this list. Their immediate parent can still retrieve their status and latest result with `litter_inspect` and the retained UUID.

Call `litter_spawn` with a self-contained task. Spawn returns admission before completion. Its optional `cwd` selects the child's working directory. Relative directories resolve against the caller's working directory. The directory must exist. The child uses native PiG settings, instructions, and skills from that directory. Named agent definitions resolve there within the root and caller tool ceilings.

Use `litter_inspect` with the child UUID to check its current state and latest result. Use `litter_wait` to wait for its result. Waiting defaults to 30 seconds and accepts an optional `timeoutMs`. A timeout leaves the child running. Cancelling a wait alone cancels only that wait. Pausing the parent also pauses its children. Inspection returns no transcript, tool evidence, mailbox, or entry pages. Result text is the child's report, with state, model, usage, and any error. Inspect, wait, and stop share the `operation`, `state`, `outcome`, and `handbackTruncated` envelope fields. The outcome's `truncated` flag marks the configured report bound.

Use `litter_message` to steer a live child or explicitly resume a paused or settled child with `resume:true`. Resume opens the native persisted conversation by UUID with the child's working directory and services. If setup stops before the first input is stored, resume also carries the original task. An unknown or missing persisted UUID returns `unavailable`.

Use `litter_stop` to pause the selected child and its descendants, then wait for their active work to stop. Stop returns the actual outcome state. A child that completed before stop retains its completed report. Nothing resumes automatically. Resuming a parent leaves its children paused until that parent explicitly resumes them.

Only the immediate parent can inspect, message, wait for, resume, or stop a child. Root follows the same rule. Knowing a sibling, ancestor, or grandchild UUID does not grant access.

```json
{"type":"worker","task":"Run the project checks and report the results.","name":"checks","cwd":"./packages/app"}
```

```json
{"id":"CHILD_ID","text":"Continue from retained conversation.","resume":true}
```

```json
{"id":"CHILD_ID"}
```

Children use PiG's native persisted sessions. The root's native log stores child definitions, parent ownership, names, and latest bounded outcomes. Reloading or reopening the same root UUID recovers those records. A different root has its own children. The root's persistence setting also applies to its metadata. An ephemeral root can recover during its live host lifetime, but its metadata does not survive process exit. Child conversations remain native persisted sessions in either case.

A compact terminal widget shows recent direct children with their names and states. `/pig-litter` explains the controls.

## Authority and limits

PiG controls extension loading. Once selected, Pig Litter adds no permission prompts or trust checks. The tool allowlist controls which tools a child can call. Each child uses an exact model available to the independent Go SDK, which resolves configured credentials through native services. Pig Litter does not copy the parent's conversation or credentials into a child. Each child discovers its own directory resources, with literal named-agent instructions appended to its normal prompt. There is no model fallback.

Native file and terminal tools execute in the child's own PiG session. PiG handles relative, absolute, home-relative, and leading-`@` file paths. Terminal commands use the native working directory. Pig Litter does not rewrite file arguments, prefix shell commands, or forward these calls to the parent. Role and inherited tool ceilings restrict callable tools; they do not sandbox filesystem paths.

A successful individual spawn starts its child independently of the enclosing script. Codemode can spawn and wait within one script. A later script failure leaves that child running. A failed or cancelled admission before success rolls back its own pending work. Normal caller completion leaves successful children running. Explicit pause preserves the session tree. Owner replacement, reload, and exit stop active executions while native persistence retains the recoverable records.

Root cancellation cascades while PiG exposes an active run's cancellation signal. In PiG 0.4.1, an SDK abort while a settlement handler waits after `agent_end` stops native continuation without publishing a parent pause. Explicit `litter_stop` still pauses the selected subtree.

PiG 0.4.1's public resource loader discovers directory settings, context, skills, templates, and prompt resources. It does not automatically load arbitrary CLI extensions or themes. Parent-only hooks and extension providers are not automatically inherited. Pig Litter loads the native codemode, tool-search, and MCP extensions explicitly. The child uses the embedded SDK's native pipeline and its explicit tool policy.

Delegation authorizes native project settings in the selected child directory. The embedded SDK uses those settings without a separate trust-store prompt. Role and inherited tool ceilings still apply.

Every active execution owns a separate Go Runtime, Session, subscription, and cancellation context. Cleanup dispatches native extension shutdown and releases the transcript manager. Failed tools produce partial or failed outcomes with canonical error context. Resume opens the preceding persisted conversation. Report clipping has a separate flag. Inspection retains the latest result even when the persisted conversation becomes unavailable for resume.

Completion JSON identifies `origin:"pig-litter"` and labels the report as child assistant prose. Wait and stop handoffs register before they block. A successful terminal handback consumes its selected outcome. Its completion then appends as an audit entry outside model context, without repeating the report to the model. Delivery capacity releases after the corresponding message or audit entry is stored.

Unconsumed outcomes reach their immediate parent through stored custom messages. A busy parent continues only for an unconsumed notice it has not seen. A successful wait suppresses a queued duplicate. Idle or paused parents retain notices without inference. A disposed parent Session reopens by UUID for completion storage. A provider or tool that ignores cancellation can delay pause and shutdown.

Delivery failures add a bounded error to the receiving parent's latest result. A successful result becomes partial; other terminal states are preserved. A failure delivering to root updates the child's latest result. Fresh inspection and waits reflect that result. Outcomes already returned to a caller remain unchanged.

The session model takes inspiration from [Claude Code subagents](https://code.claude.com/docs/en/sub-agents), [directory settings](https://code.claude.com/docs/en/settings), and [project instructions](https://code.claude.com/docs/en/memory). PiG determines native discovery behavior. Pig Litter defines direct-parent controls, nested delegation, and explicit subtree pause without automatic resume.

## Verify

Run the unit tests, race check, and build inside devenv.

```sh
devenv shell -- scripts/test-pig-litter.sh
devenv shell -- check-pig-extension
```

The second command validates the source factory, prebuilt standalone, and Piglet against the pinned release. The coordinator owns released-host lifecycle and terminal verification. Compilation and pure tests alone do not prove those checks.

Run the Go loopback fixtures in a disposable workspace with isolated fixture credentials and a restricted `PATH` containing pinned PiG and the required terminal tools.

```sh
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

The fixtures record provider requests, stdout, stderr, and results under `.pstack/evidence/go-lifecycle`. They verify native file and terminal operations, tool ceilings, child working directories, concurrency, depth, privacy, results, usage, retained resume, and subtree pause. Additional cases check provider and tool failures, scout leaf enforcement, model restrictions, conversation retention, message ordering, replacement, and live-child exit. The completion case forwards extension protocol frames unchanged to observe delivery acknowledgements. It verifies that a busy parent processes a hidden child completion without another user prompt, and that an idle parent appends it without inference.

The `unbounded` scenario keeps a child active beyond two minutes and requires successful completion. Fixture deadlines bound the verification process without limiting product child runs.

The `pause` scenario aborts the root during an active wait through PiG RPC. It requires child provider cancellation, retained paused UUIDs, and explicit resume of only the selected child.

The `admission` scenario fails a provisional spawn through a final result hook. It requires rollback, no child provider request, and a successful retry within the same capacity limit. The `codemode` scenario also verifies successful spawn and resume calls survive a later script failure.

The `persistence` scenario runs two stock PiG processes with a private parent session directory. The second opens the first root UUID with `--session ROOT_UUID`. It verifies recovered child ownership, names, working directory, native conversation recall, UUID resume, and the outcome of stopping a completed child. Unit fixtures also exercise native skills, deferred MCP discovery, codemode filtering, late root registration, an empty root ceiling, and fresh soft-turn allowances.

Run the terminal driver with `tmux` available. Each case uses a private socket and isolated provider fixture.

```sh
devenv shell -- scripts/verify-terminal-ui.sh --case direct
devenv shell -- scripts/verify-terminal-ui.sh --case piglet
devenv shell -- scripts/verify-terminal-ui.sh --case unselected
```

The driver checks two live children, widget state, resizing, editor input, help, stop, actual parent scrollback, and clean quit. The unselected case verifies plain PiG has no litter tools or widget.

## License

Licensed under the [Apache License 2.0](LICENSE).
