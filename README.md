# Pig Litter

Pig Litter is a selected Node Resource for released PiG 0.4.1. It starts independent background Sessions through PiG's provided Node SDK. Plain PiG has no Pig Litter tools.

## Build and select the Resource

The developer environment pins PiG 0.4.1, Node 24, Bun, and the build tools. Build the Resource before selecting it.

```sh
devenv shell
cd extensions/pig-litter
bun install --frozen-lockfile
bun run build
cd ../..
pig -e ./extensions/pig-litter/dist/pig-litter.mjs
```

You can also select [piglet.yaml](piglet.yaml). The build bundles the YAML parser and leaves PiG's provided SDK external.

## Configure children

Put [litter.yaml](litter.yaml) in the trusted project root. The file sets concurrency, depth, retention bounds, run limits, and named agents. The bundled `scout` uses `read`. The bundled `worker` can also use `write` and `edit`. Add `ls` to either agent's tools only when the parent has made the canonical `ls` builtin callable. An agent can narrow its list, set an exact default `provider/model`, change its instructions, or disable delegation. It cannot add a file tool outside its role.

The main Session is depth zero and does not use a child slot. Depth two permits grandchildren. Admission rejects a fifth live child when `concurrency` is four. It does not queue launches. A second live writer in the same working directory is rejected. Agent names can be reused as definitions; each child receives a stable run ID and generation.

## Use the tools

The Resource registers exactly six model tools: `litter_spawn`, `litter_list`, `litter_inspect`, `litter_message`, `litter_stop`, and `litter_wait`.

Ask the parent model to call `litter_list` to discover available named agent types, their roles, short descriptions, tools, and delegation setting. The list pages agent types and retained children independently. Call `litter_spawn` with a self-contained task. Spawn returns an admitted child immediately. Use `litter_wait` separately to wait for an exact ID and generation. A wait timeout or a cancelled wait leaves the child running. `litter_inspect` returns a bounded summary by default and a page of transcript entries only when requested. `litter_message` steers a live child or resumes a settled child with `resume:true`. Resume keeps the child ID and retained in-memory history and increments the generation. `litter_stop` aborts the selected live subtree.

```json
{"type":"scout","task":"Read the requested file and report its main functions.","name":"survey"}
```

```json
{"id":"<child ID>","generation":1,"text":"Continue from the retained history.","resume":true}
```

The selected Resource retains child histories only while its owning main Session and extension connection live. Reload, replacement, exit, or a crash loses them. This increment does not persist child Session files or recover them after restart. If one SDK turn pushes a history over `max_history_bytes`, Pig Litter drops that retained manager after cleanup and rejects resume. The current turn can briefly exceed the limit before cleanup. A compact widget lists recent children when the terminal UI is available. A focused `/litter` transcript inspector is not included in this increment.

## Authority and limits

The Resource requires a trusted project and an exact model available to both the parent and the provided Node SDK. It does not copy the parent conversation, credentials, skills, context files, other extensions, or prompt templates into a child. PiG's normal model runtime resolves credentials. Providers registered by a parent extension are unavailable to independent children, even when they shadow a normal provider ID. Pig Litter does not choose a fallback.

Child file tools call the original parent's `executeTool` with the child signal. PiG then applies the parent's tool validation and permission hooks. The Resource checks the currently callable tool, its canonical builtin provenance, and its schema before every call. It rejects an observed override. Stock PiG has no atomic registry freeze between that check and dispatch. This is a finite tool restriction for ordinary selected Resources, not a sandbox or protection against a malicious trusted extension that changes the parent registry concurrently. File tools can access paths outside the project if the parent host permits them.

Cancellation uses the provided Session's abort signal. A provider or trusted native tool that ignores abort can delay stop and shutdown. Pig Litter cannot impose an operating-system deadline on that code inside stock PiG.

An accepted root spawn starts its child prompt after PiG emits the matching spawn tool-result `message_end`. This proves the parent handback snapshot has frozen; it is not a disk persistence acknowledgement. After that point, an ordinary parent turn ending does not stop the child. The original spawn request can cancel admission before acknowledgement. Explicit stop, owner replacement, reload, and exit close the tree. The Resource schedules one bounded completion message per run while the owner is live. PiG's asynchronous send does not provide a durable delivery acknowledgement. Reports and retained mailboxes are bounded by `litter.yaml`.

## Verify

Run the pure state and configuration tests, then validate the built Resource and Piglet against the pinned release.

```sh
devenv shell -- scripts/test-pig-litter.sh
devenv shell -- check-pig-extension
```

The coordinator owns the real PiG lifecycle and terminal UI drive. Source-only and pure tests do not establish that the released host accepts the Resource.

For a disposable local loopback drive with no inherited provider credentials, run the core and nested fixtures against the pinned `pig` on `PATH`.

```sh
devenv shell -- bun scripts/verify-stock-lifecycle.mjs --scenario core
devenv shell -- bun scripts/verify-stock-lifecycle.mjs --scenario nested
```

## License

Licensed under the [Apache License 2.0](LICENSE).
