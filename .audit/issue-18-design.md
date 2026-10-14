# Native child persistence and tool policies

## Contract

Children are native persisted PiG sessions addressed by UUID. Admission mints `ai.UUIDv7(nil)` and returns the ID before constructing a Session. Initial execution calls `Runtime.New` with `SessionID`. Continuation calls `Runtime.Resume` with that UUID and the child's working directory and services. Litter never addresses a child transcript by path or supplies an in-memory session manager.

The root's native custom entries own child definitions, immediate-parent relationships, permanent sibling names, pending initial tasks, latest bounded outcomes, and completion receipts. A fresh owner of the same root recovers those records. Another root cannot acquire them. Root metadata follows the root's persistence setting, while child conversations always use native persistence. An ephemeral root cannot preserve its ownership records after its host exits.

Every child result operation includes `operation` and `handbackTruncated`. Stop returns its actual terminal outcome, including a completion that won the race with stop. The result default is 50,000 bytes, with the 1 MiB upper cap retained. PiG stores the full conversation independently of that report bound.

## Chosen structure

`savedChild` is the durable data shape. The root log stores successive snapshots by child UUID. `record` holds the recovered definition and bounded latest result. `run` owns one disposable execution, cancellation, call tickets, and native Session. Its active transcript manager is cleared after native disposal. Completion storage for an idle parent reopens its UUID without inference.

Persist a provisional admission before returning it. Final individual tool success activates the child. Failed admission frees its provisional record and name. Recovery can confirm a provisional root spawn from the native final result when activation confirmation is missing. A resumed native file proves the initial task was stored, so continuation sends only the additional instructions. A child that never reached first-input persistence can create its first file under the admitted UUID. A previously established missing file returns unavailable.

The existing call tickets remain authoritative for live final-result callbacks. A later enclosing codemode failure cannot revoke a successful spawn or resume. Completion receipts retain their stable nonce. Native stored-message evidence suppresses repeated delivery after recovery. Immediate-parent controls, subtree pause, depth, concurrency, and writer coordination remain.

Root `GetAllTools` supplies concrete names, including inactive codemode and deferred tools. Hidden exposure is excluded. Configured role patterns expand on each spawn, resume, and activation. Every ancestor's admitted patterns narrow nested delegation. Empty expansion remains an explicit empty allowlist. Child directory settings and resources cannot add tool names to the root inventory.

Scouts use only `read` and `ls`. Workers default to those tools, file mutation tools, `skill:*`, `mcp__*`, `codemode`, and `tool_search`. Native codemode, tool-search, and MCP extensions are constructed and bound explicitly. Root MCP configuration and extension server registrations are reproduced, with their native exposure preserved. SDK registration filtering also constrains codemode and tool-search.

The HTTP fixture reproduced a native connection/registration race on immediate resume. Litter waits up to PiG's normal startup interval for admitted MCP names to register. The helper returns any names still pending, and the child receives that status while native connections continue. Native shutdown events close MCP transports before Session and Runtime disposal.

The only tool permission control is the allowlist. PiG retains its extension-loading boundary. Litter performs no independent trust denial or prompt during initialization, spawn, activation, or resume. Model validity, ownership, and execution capacity remain operational requirements.

## Design comparison

The architecture panel compared root-native snapshots, a separate per-root disk ledger, and child metadata with a root index. The independent gpt-5.6-sol judge preferred the ledger because the native sketch duplicated state with child witnesses and repair events. The selected design removes those duplicated authorities instead. One native root record owns metadata. Native child logs own conversations and completion entries. No second file format, lease, event reducer, or transcript scan establishes ownership.

Adopt the judge's corrections for the public API, strict scout tools, persisted glob policies, model-derived storage, soft notices, and terminal stop races. Reject invented `litter_resume` controls and annotation-based MCP permission checks. Keep the existing six tools and their arguments.

Model the Domain shaped the separate durable record and execution. Laziness removed history managers and a second persistence system. Boundary Discipline confines raw native entries and configuration parsing to their adapters. Make Operations Idempotent shaped UUID recovery and stable completion receipts. Test Behavior, Not Implementation required provider requests, native file effects, and stock-host restart evidence.

## Limits and decisions

Remove record-count and history-byte limits. Keep `max_mailbox` because queued messages and outstanding delivery are independent of retained conversation history. Keep depth and concurrency configurable. A soft turn allowance defaults to 50 without an artificial upper cap. At the threshold, one system notice requests a partial report. The child can continue until it settles or receives stop. Settlement reports partial. Explicit resume grants a fresh allowance.

Refresh the root inventory at spawn and resume because the SDK exposes no tools-changed push event. Existing executions retain their admitted concrete registry. Later executions expand the persisted patterns again. Nested policy checks use the cumulative ancestor policies so late registration cannot broaden past an older ancestor.

## Verification

The native SDK probe confirmed UUID identity, deferred file creation, child-CWD lookup, and resume after disposing all original native objects. Pre-fix behavioral tests captured missing inspect metadata, the hidden 8 KiB handback cut, retained record limits, and rejection above 128 turns.

Registered operation tests verify UUID admission before materialization, fresh-owner reconstruction, name uniqueness, unknown UUIDs, native history recall, soft allowance reset, and an empty root ceiling. The HTTP MCP fixture verifies child skills, root configuration despite a different child CWD, codemode filtering, deferred declarations before and after tool-search discovery, late registration on resume, and transport shutdown. False host trust still permits allowed native writes and resume.

The stock `persistence` scenario runs two PiG 0.4.1 processes with a private parent session directory. The second uses PiG's actual noninteractive UUID flag, `--session ROOT_UUID`. It recovers the same child ID, directory, name, and private conversation, then verifies stop preserves a completed outcome. Other stock scenarios retain final-result rollback, privacy, subtree pause, live-child cancellation, native directory resources, and completion delivery.

Both Go modules pass unit tests, race checks, vet, and builds. Raw reports and run outputs remain under `.pstack/issue-18` and `.pstack/evidence`. The append-only decision trail is `.audit/issue-18.tsv`.

The pinned source governs API behavior. Current [PiG SDK documentation](https://raw.githubusercontent.com/MichaelKinsy/PiG/main/docs/site/docs/sdk.md) and [extension SDK documentation](https://raw.githubusercontent.com/MichaelKinsy/PiG/main/docs/extension-sdk-surface.md) were checked against PiG 0.4.1.
