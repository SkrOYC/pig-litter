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

Put `litter.yaml` in `~/.config/pig/agent/` to configure children across projects. With no explicit configuration root, Pig Litter loads that file, then any settings in PiG's default `~/.pig/agent/litter.yaml`. Explicit `PIG_HOME`, `XDG_CONFIG_HOME`, agent-directory overrides, or Pi compatibility mode select their own user directory instead. Put [litter.yaml](litter.yaml) in a trusted project root to override user settings for that project. Agent fields merge by name, so a project can change one field while retaining the user's other choices.

Either file can override `scout` or `worker`, including their role, instructions, tools, model, and delegation setting. Additional agent names use the same definition format. Changing a role resets omitted instructions, tools, and delegation to that role's defaults. The model stays inherited unless overridden. The scout role uses `read` by default and cannot delegate. The worker role also uses `write`, `edit`, and `bash` by default.

The configured role sets each child's tool ceiling. PiG's directory settings select active tools within that ceiling. Root can spawn a configured child independently of the root model's active file tools. Nested delegation cannot grant tools outside the delegating parent's ceiling.

For example, set a worker model across projects with this user configuration:

```yaml
agents:
  worker:
    model: PROVIDER/MODEL
```

Replace `PROVIDER/MODEL` with an exact configured model identifier. A project can override that model or add literal instructions under the same `worker` name.

The main Session is named `root`, has depth zero, and occupies no child slot. Depth two permits grandchildren through workers. A concurrency limit of four rejects a fifth live child without a launch queue. A second live writer in the same working directory is rejected.

Each subagent is a session node with its own native PiG UUID. That UUID identifies the retained session across turns, pause, and resume. Children stay attached to their immediate parent throughout that lifetime.

Configuration parsing rejects duplicate and unknown keys, aliases, invalid numeric types, empty documents, oversized files, nonregular files, and symlinks. Missing configuration files leave the preceding defaults in effect. Limits cover concurrency, depth, retained conversations, results, queued messages, and turns. Child runs have no wall-clock deadline.

## Use the tools

The Resource registers exactly `litter_spawn`, `litter_list`, `litter_inspect`, `litter_message`, `litter_stop`, and `litter_wait`.

Call `litter_list` to discover named agent types and your active direct children. It pages agent definitions and children independently. Paused and settled children are absent from this list. Their immediate parent can still retrieve their status and latest result with `litter_inspect` and the retained UUID.

Call `litter_spawn` with a self-contained task. Spawn returns admission before completion. Its optional `cwd` selects the child's working directory. Relative directories resolve against the caller's working directory. The directory must exist. The child uses native PiG tools, settings, instructions, and skills from that directory. Named agent definitions resolve there within the caller's tool ceiling.

Use `litter_inspect` with the child UUID to check its current state and latest result. Use `litter_wait` to wait for its result. Waiting defaults to 30 seconds and accepts an optional `timeoutMs`. A timeout leaves the child running. Cancelling a wait alone cancels only that wait. Pausing the parent also pauses its children. Inspection returns no transcript, tool evidence, mailbox, or entry pages. Result text is the child's report, with state, model, usage, and any error.

Use `litter_message` to steer a live child or explicitly resume a paused or settled child with `resume:true`. Resume retains the conversation and UUID. If setup stops before the first input is stored, resume also carries the original task. Use `litter_stop` to pause the selected child and its descendants, then wait for their active work to stop. Nothing resumes automatically. Resuming a parent leaves its children paused until that parent explicitly resumes them.

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

Conversations last while their owning main Session and extension connection live. Replacement, reload, exit, and crashes lose them. An SDK turn can temporarily exceed `max_history_bytes`. Pig Litter then drops the retained conversation after cleanup and rejects resume. A compact terminal widget shows recent direct children with their names and states. `/pig-litter` explains the controls.

## Authority and limits

The Resource requires project trust and an exact model available to the independent Go SDK. The SDK resolves configured credentials through its native services. Pig Litter does not copy the parent's conversation or credentials into a child. Each child discovers its own directory resources, with literal named-agent instructions appended to its normal prompt. There is no model fallback.

Native file and terminal tools execute in the child's own PiG session. PiG handles relative, absolute, home-relative, and leading-`@` file paths. Terminal commands use the native working directory. Pig Litter does not rewrite file arguments, prefix shell commands, or forward these calls to the parent. Role and inherited tool ceilings restrict callable tools; they do not sandbox filesystem paths.

A successful individual spawn starts its child independently of the enclosing script. Codemode can spawn and wait within one script. A later script failure leaves that child running. A failed or cancelled admission before success rolls back its own pending work. Normal caller completion leaves successful children running. Explicit pause preserves the session tree; owner replacement, reload, and exit retire it.

PiG 0.4.1's public resource loader discovers directory settings, context, skills, templates, and prompt resources. It does not automatically load arbitrary CLI extensions or themes. Parent-only hooks and extension providers are not automatically inherited. The child uses the embedded SDK's native pipeline and its explicit tool policy.

A trusted parent's delegation authorizes native project settings in the selected child directory. The embedded SDK does not consult the CLI's trust store for that target independently. The parent trust check and role tool ceilings still apply.

Every active execution owns a separate Go Runtime, Session, subscription, and cancellation context. Failed tools produce partial or failed outcomes with canonical error context. Resume keeps the preceding conversation internally. Report clipping has a separate flag. Inspection retains the latest result even when the conversation becomes unavailable for resume.

Completion JSON identifies `origin:"pig-litter"` and labels the report as child assistant prose. Wait and stop handoffs register before they block. A successful terminal handback consumes its selected outcome. Its completion then appends as an audit entry outside model context, without repeating the report to the model. Delivery capacity releases after the corresponding message or audit entry is stored.

Unconsumed outcomes reach their immediate parent through stored custom messages. A busy parent continues only for an unconsumed notice it has not seen. A successful wait suppresses a queued duplicate. Idle or paused parents retain notices without inference. A disposed parent Session uses retained history as the fallback. A provider or tool that ignores cancellation can delay pause and shutdown.

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
```

The fixtures record provider requests, stdout, stderr, and results under `.pstack/evidence/go-lifecycle`. They verify native file and terminal operations, tool ceilings, child working directories, concurrency, depth, privacy, results, usage, retained resume, and subtree pause. Additional cases check provider and tool failures, scout leaf enforcement, model restrictions, conversation retention, message ordering, replacement, and live-child exit. The completion case forwards extension protocol frames unchanged to observe delivery acknowledgements. It verifies that a busy parent processes a hidden child completion without another user prompt, and that an idle parent appends it without inference.

The `unbounded` scenario keeps a child active beyond two minutes and requires successful completion. Fixture deadlines bound the verification process without limiting product child runs.

The `pause` scenario aborts the root during an active wait through PiG RPC. It requires child provider cancellation, retained paused UUIDs, and explicit resume of only the selected child.

The `admission` scenario fails a provisional spawn through a final result hook. It requires rollback, no child provider request, and a successful retry within the same capacity limit. The `codemode` scenario also verifies successful spawn and resume calls survive a later script failure.

Run the terminal driver with `tmux` available. Each case uses a private socket and isolated provider fixture.

```sh
devenv shell -- scripts/verify-terminal-ui.sh --case direct
devenv shell -- scripts/verify-terminal-ui.sh --case piglet
devenv shell -- scripts/verify-terminal-ui.sh --case unselected
```

The driver checks two live children, widget state, resizing, editor input, help, stop, actual parent scrollback, and clean quit. The unselected case verifies plain PiG has no litter tools or widget.

## License

Licensed under the [Apache License 2.0](LICENSE).
