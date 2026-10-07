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

Put `litter.yaml` in `~/.config/pig/agent/` to configure children across projects. Pig Litter uses the active PiG agent directory, including `PIG_HOME` and `PIG_CODING_AGENT_DIR` overrides. Put [litter.yaml](litter.yaml) in a trusted project root to override user settings for that project. Configuration applies in order from bundled defaults to user settings to project settings. Agent fields merge by name, so a project can change one field while retaining the user's other choices.

Either file can override `scout` or `worker`, including their role, instructions, tools, model, and delegation setting. Additional agent names use the same definition format. Changing a role resets omitted instructions, tools, and delegation to that role's defaults. The model stays inherited unless overridden. The scout role uses `read` by default and cannot delegate. The worker role also uses `write`, `edit`, and `bash` by default. Add `ls` only when the parent has made its canonical builtin callable. Agent tools cannot exceed the selected role or caller's tool ceiling.

For example, set a worker model across projects with this user configuration:

```yaml
agents:
  worker:
    model: PROVIDER/MODEL
```

Replace `PROVIDER/MODEL` with an exact configured model identifier. A project can override that model or add literal instructions under the same `worker` name.

The main Session is depth zero and occupies no child slot. Depth two permits grandchildren through workers. A concurrency limit of four rejects a fifth live child without a launch queue. A second live writer in the same working directory is rejected. Every admitted child has one stable ID.

Configuration parsing rejects duplicate and unknown keys, aliases, invalid numeric types, empty documents, oversized files, nonregular files, and symlinks. Missing configuration files leave the preceding defaults in effect. Limits cover concurrency, depth, retained conversations, results, queued messages, and turns. Child runs have no wall-clock deadline.

## Use the tools

The Resource registers exactly `litter_spawn`, `litter_list`, `litter_inspect`, `litter_message`, `litter_stop`, and `litter_wait`.

Call `litter_list` to discover named agent types and children. It pages agent definitions and retained children independently. Call `litter_spawn` with a self-contained task. Spawn returns admission immediately. Its optional `cwd` selects the child's working directory. Relative directories resolve against the caller's working directory. The directory must exist. Child file tools and terminal commands run relative to that directory.

Use `litter_inspect` with the child ID to check its current state and latest result. Use `litter_wait` to wait for its result. Waiting defaults to 30 seconds and accepts an optional `timeoutMs`. A wait timeout or cancelled wait leaves the child running. Inspection returns no transcript, tool evidence, mailbox, or entry pages. Result text is the child's report, with state, model, usage, and any error.

Use `litter_message` to steer a live child or resume a settled child with `resume:true`. Resume retains the conversation and uses the same ID. Use `litter_stop` to cancel the selected subtree and wait for cleanup.

```json
{"type":"worker","task":"Run the project checks and report the results.","name":"checks","cwd":"./packages/app"}
```

```json
{"id":"CHILD_ID","text":"Continue from retained conversation.","resume":true}
```

```json
{"id":"CHILD_ID"}
```

Conversations last while their owning main Session and extension connection live. Replacement, reload, exit, and crashes lose them. An SDK turn can temporarily exceed `max_history_bytes`. Pig Litter then drops the retained conversation after cleanup and rejects resume. A compact terminal widget shows recent children. Named children keep their logical name. Unnamed children use `#` followed by the ID sequence suffix. `/pig-litter` explains the controls.

## Authority and limits

The Resource requires project trust and an exact model available to both the parent and independent Go SDK. The SDK resolves normal configured credentials from the parent's agent directory. Pig Litter copies no parent conversation, credentials, skills, context files, other extensions, or prompt templates into a child. Instructions remain literal text. Parent extension-registered providers are unavailable to independent children, including providers that shadow a normal provider ID. There is no model fallback.

Child file and terminal tools call the original parent's `Context.ExecuteTool` with their own cancellation context. PiG applies the parent's validation and permission hooks. Relative file paths resolve against the child's working directory. Terminal commands change to that directory before execution. Pig Litter checks callable tools, canonical builtin provenance, schema, trust, and the caller's tool ceiling before every call. Stock PiG provides no atomic registry freeze between that check and dispatch. A malicious trusted extension could change the registry concurrently. These checks restrict tools but don't sandbox filesystem paths. Paths outside the project remain accessible when the parent permits them.

Root inference starts after the matching spawn tool-result `message_end`. That event freezes the parent handback. It does not acknowledge disk persistence. Admission can be cancelled before that event. An ordinary caller turn ending leaves acknowledged children running. Explicit stop, owner replacement, reload, and exit retire the tree.

Every active execution owns a separate Go Runtime, Session, subscription, and cancellation context. Failed tools produce partial or failed outcomes with canonical error context. Resume keeps the preceding conversation internally. Report clipping has a separate flag. Inspection retains the latest result even when the conversation becomes unavailable for resume.

Completion JSON identifies `origin:"pig-litter"` and labels the report as child assistant prose. Wait and stop handoffs register before they block. After a successful terminal tool-result `message_end`, the matching completion appends as an audit custom entry outside model context. Its reservation releases after that append succeeds. Other completion reservations remain held until their custom message is appended. A busy root receives asynchronous completions as follow-up messages. An idle root retains them without inference. A disposed parent Session uses retained history as the fallback. Stock sends provide no durable acknowledgement. A provider or trusted tool that ignores cancellation can delay stop and shutdown.

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
```

The fixtures record provider requests, stdout, stderr, and results under `.pstack/evidence/go-lifecycle`. They verify parent-mediated file and terminal operations, permission denials, child working directories, concurrency, depth, privacy, results, usage, retained resume, and subtree stop. Additional cases check provider and tool failures, scout leaf enforcement, model restrictions, conversation retention, message ordering, replacement, and live-child exit. The completion case forwards extension protocol frames unchanged to observe delivery acknowledgements. It verifies that a busy parent processes a hidden child completion without another user prompt, and that an idle parent appends it without inference.

The `unbounded` scenario keeps a child active beyond two minutes and requires successful completion. Fixture deadlines bound the verification process without limiting product child runs.

Run the terminal driver with `tmux` available. Each case uses a private socket and isolated provider fixture.

```sh
devenv shell -- scripts/verify-terminal-ui.sh --case direct
devenv shell -- scripts/verify-terminal-ui.sh --case piglet
devenv shell -- scripts/verify-terminal-ui.sh --case unselected
```

The driver checks two live children, widget state, resizing, editor input, help, stop, actual parent scrollback, and clean quit. The unselected case verifies plain PiG has no litter tools or widget.

## License

Licensed under the [Apache License 2.0](LICENSE).
