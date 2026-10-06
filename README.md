# Pig Litter

Pig Litter is a selected Go Resource for released PiG 0.4.1. It starts independent background Sessions through the published Go SDK. Plain PiG has no Pig Litter tools.

## Build and select the Resource

The developer environment pins PiG 0.4.1 and Go 1.27.1 with CGO enabled. Build inside the devenv shell so Go can use its C compiler.

```sh
devenv shell
scripts/test-pig-litter.sh
pig -e ./extensions/pig-litter
```

The source directory exports a conventional `Extension() *sdk.Extension` factory. [piglet.yaml](piglet.yaml) selects that directory. Its first load may fetch the exact dependencies in `go.mod` and `go.sum`. Standalone and verification commands live in a separate `scripts` Go module, outside the selected factory root.

Build the prebuilt standalone with the same script, then select it with `pig -e ./extensions/pig-litter/dist/pig-litter`. The prebuilt artifact needs no Go compiler, Node, or Bun at runtime. Product builds and tests use Go.

## Configure children

Put [litter.yaml](litter.yaml) in the trusted project root. It sets concurrency, depth, retained history, report and mailbox bounds, run limits, and named agents. The bundled `scout` uses `read`. The bundled `worker` also uses `write` and `edit`. Add `ls` only when the parent has made its canonical builtin callable. Named agents can narrow their tools, choose an exact `provider/model`, change their literal instructions, or disable delegation. They cannot exceed their role or caller's tool ceiling.

The main Session is depth zero and occupies no child slot. Depth two permits grandchildren. A concurrency limit of four rejects a fifth live child without a launch queue. A second live writer in the same working directory is rejected. Every admitted child has a stable ID and an explicit generation.

Configuration parsing rejects duplicate and unknown keys, aliases, invalid numeric types, empty documents, oversized files, nonregular files, and symlinks. Missing `litter.yaml` uses bundled defaults.

## Use the tools

The Resource registers exactly `litter_spawn`, `litter_list`, `litter_inspect`, `litter_message`, `litter_stop`, and `litter_wait`.

Call `litter_list` to discover named agent types and children. It pages agent definitions and retained children independently. Call `litter_spawn` with a self-contained task. Spawn returns admission immediately. Use `litter_wait` separately for an exact ID and generation. A timeout or cancelled wait leaves the child running. `litter_inspect` returns bounded summaries and optional transcript pages. `litter_message` steers a live child or resumes a settled child with `resume:true`. Resume keeps the in-memory history and child ID, then increments the generation. `litter_stop` aborts the selected subtree and waits for cleanup.

```json
{"type":"scout","task":"Read the requested file and report its main functions.","name":"survey"}
```

```json
{"id":"CHILD_ID","generation":1,"text":"Continue from retained history.","resume":true}
```

Histories last while their owning main Session and extension connection live. Replacement, reload, exit, and crashes lose them. An SDK turn can temporarily exceed `max_history_bytes`. Pig Litter then drops the retained manager after cleanup and rejects resume. A compact terminal widget shows recent children. `/pig-litter` explains the controls. A focused transcript inspector, durable history, workflow runner, and product worktrees are outside this increment.

## Authority and limits

The Resource requires project trust and an exact model available to both the parent and independent Go SDK. The SDK resolves normal configured credentials from the parent's agent directory. Pig Litter copies no parent conversation, credentials, skills, context files, other extensions, or prompt templates into a child. Instructions remain literal text. Parent extension-registered providers are unavailable to independent children, including providers that shadow a normal provider ID. There is no model fallback.

Child file tools call the original parent's `Context.ExecuteTool` with their own cancellation context. PiG applies the parent's validation and permission hooks. Pig Litter checks the callable tool, canonical builtin provenance, schema, trust, and finite ceiling before every call. Stock PiG provides no atomic registry freeze between that check and dispatch. A malicious trusted extension could change the registry concurrently. These checks are a tool restriction, not a filesystem sandbox. Paths outside the project remain accessible when the parent permits them.

Root inference starts after the matching spawn tool-result `message_end`. That event freezes the parent handback. It does not acknowledge disk persistence. Admission can be cancelled before that event. An ordinary caller turn ending leaves acknowledged children running. Explicit stop, owner replacement, reload, and exit retire the tree.

Every generation owns a separate Go Runtime, Session, subscription, and cancellation context. Terminal assistant events and usage deltas describe that generation. Failed tools produce truthful partial or failed outcomes. Report clipping has a separate flag. Completion reservations remain held until their custom message is appended. A disposed parent Session uses retained history as the fallback. Stock sends provide no durable acknowledgement. A provider or trusted tool that ignores cancellation can delay stop and shutdown.

## Verify

Run the unit tests, race check, and build inside devenv.

```sh
devenv shell -- scripts/test-pig-litter.sh
devenv shell -- check-pig-extension
```

The second command validates the source factory, prebuilt standalone, and Piglet against the pinned release. The coordinator owns released-host lifecycle and terminal verification. Compilation and pure tests alone do not prove those checks.

Run the Go loopback fixtures in a disposable workspace with isolated fixture credentials and only the pinned PiG directory on the child's `PATH`.

```sh
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario core
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario nested
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario checks
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario messages
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario lifecycle
devenv shell -- scripts/verify-stock-lifecycle.sh --scenario shadow
```

The fixtures record provider requests, stdout, stderr, and result receipts under `.pstack/evidence/go-lifecycle`. They verify parent-mediated file operations and permission denials, concurrency and depth limits, privacy, bounded reports and usage, retained resume, stale generations, and subtree stop. Additional cases check provider and tool failures, readonly delegation, exact model restrictions, history retirement, message ordering, replacement, and live-child exit.

Run the terminal driver with `tmux` available. Each case uses a private socket and isolated provider fixture.

```sh
devenv shell -- scripts/verify-terminal-ui.sh --case direct
devenv shell -- scripts/verify-terminal-ui.sh --case piglet
devenv shell -- scripts/verify-terminal-ui.sh --case unselected
```

The driver checks two live children, widget state, resizing, editor input, help, stop, actual parent scrollback, and clean quit. The unselected case verifies plain PiG has no litter tools or widget.

## License

Licensed under the [Apache License 2.0](LICENSE).
