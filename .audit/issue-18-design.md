# Native child persistence and tool policies

## Contract

Children are native persisted PiG sessions addressed by UUID. Admission mints `ai.UUIDv7(nil)` and returns the ID before constructing a Session. Initial execution calls `Runtime.New` with `SessionID`. Continuation calls `Runtime.Resume` with that UUID and the child's working directory and services. Litter never addresses a child transcript by path or supplies an in-memory session manager.

The root's native custom entries own child definitions, immediate-parent relationships, permanent sibling names, pending inputs, latest bounded outcomes, and completion receipts. A fresh owner of the same root recovers those records. Another root cannot acquire them. Root metadata follows the root's persistence setting, while child conversations always use native persistence. An ephemeral root cannot preserve its ownership records after its host exits.

Every child result operation includes `operation` and `handbackTruncated`. Stop returns its actual terminal outcome, including a completion that won the race with stop. The result default is 50,000 bytes, with the 1 MiB upper cap retained. PiG stores the full conversation independently of that report bound.

## Chosen structure

`savedChild` is the durable data shape. The root log stores successive snapshots by child UUID. `record` holds the recovered definition and bounded latest result. `run` owns one disposable execution, cancellation, call tickets, and native Session. Its active transcript manager is cleared after native disposal. Completion storage for an idle parent reopens its UUID without inference.

Persist a provisional admission before returning it. Final individual tool success journals activation before execution. A lifecycle lock orders that write against retirement. Failed admission frees its provisional record and name. Recovery can confirm an incomplete provisional root spawn from its native final result. A resumed native file proves the initial task was stored. Only a child with no recorded native input attempt can initialize its first file under the admitted UUID.

`pendingInput` retains accepted continuation text and a native leaf anchor. Recovery checks whether the matching user message follows that anchor. It resends an unstored input and avoids replaying a stored input. The first turn clears pending input after native user-message persistence. An interrupted current run receives a paused outcome instead of the previous run's completed result. Recovered completion work is captured before publishing the owner, and replacement waits for prior native writers to drain.

PiG's public UUID discovery cannot distinguish absence from an unreadable header. Once an input anchor is recorded, failed discovery returns unavailable. This conservative case includes interruption between anchor persistence and the first native message. Litter does not inspect transcript paths or recreate an ambiguous identity.

The existing call tickets remain authoritative for live final-result callbacks. A later enclosing codemode failure cannot revoke a successful spawn or resume. Terminal publication waits for metadata storage. Storage errors reach wait and completion through the same outcome, and resume rejects unfinished terminal storage. Immediate-parent controls, subtree pause, depth, concurrency, and writer coordination remain.

Completion audits retain content, nonce, handback call, and its native leaf anchor. An audit alone does not retire a receipt. A fresh UUID reader must find the custom message or the matching stored final tool result. The root reader uses its public session-directory setting with UUID resume and performs no inference. Native append failures stay pending. Retry reopens storage instead of trusting the SDK manager's memory after a failed write.

Root `GetAllTools` supplies concrete names, including inactive codemode and deferred tools. Hidden exposure is excluded. Configured role patterns expand on each spawn, resume, and activation. Every ancestor's admitted patterns narrow nested delegation. Empty expansion remains an explicit empty allowlist. Child directory settings and resources cannot add tool names to the root inventory.

Scouts use only `read` and `ls`. Workers default to those tools, file mutation tools, `skill:*`, `mcp__*`, `codemode`, and `tool_search`. Native codemode, tool-search, and MCP extensions are constructed and bound explicitly. Root MCP configuration uses the root's actual trust selection. Registered servers keep their authority over root-untrusted project overrides. Native exposure is preserved. SDK registration filtering also constrains codemode and tool-search.

The HTTP fixture reproduced a native connection/registration race on immediate resume. Litter waits up to PiG's normal startup interval for admitted MCP names and resource tools to register. The helper returns any names still pending, and the child receives that status while native connections continue. Native shutdown events close MCP transports before Session and Runtime disposal.

The only tool permission control is the allowlist. PiG retains its extension-loading boundary. Litter performs no independent trust denial or prompt during initialization, spawn, activation, or resume. Model validity, ownership, and execution capacity remain operational requirements.

## Design comparison

The architecture panel compared root-native snapshots, a separate per-root disk ledger, and child metadata with a root index. The independent gpt-5.6-sol judge preferred the ledger because the native sketch duplicated state with child witnesses and repair events. The selected design removes those duplicated authorities instead. One native root record owns metadata. Native child logs own conversations and completion entries. No separate file format or lease is introduced. Native parent entries can confirm an incomplete provisional admission and delivery receipts.

Adopt the judge's corrections for the public API, strict scout tools, persisted glob policies, model-derived storage, soft notices, and terminal stop races. Reject invented `litter_resume` controls and annotation-based MCP permission checks. Keep the existing six tools and their arguments.

Model the Domain shaped the separate durable record and execution. Laziness removed history managers and a second persistence system. Boundary Discipline confines raw native entries and configuration parsing to their adapters. Make Operations Idempotent shaped UUID recovery and stable completion receipts. Test Behavior, Not Implementation required provider requests, native file effects, and stock-host restart evidence.

## Limits and decisions

Remove record-count and history-byte limits. Keep `max_mailbox` because queued messages and outstanding delivery are independent of retained conversation history. Keep depth and concurrency configurable. A soft turn allowance defaults to 50 without an artificial upper cap. At the threshold, one system notice requests a partial report. The child can continue until it settles or receives stop. Settlement reports partial. Explicit resume grants a fresh allowance.

Refresh the root inventory at spawn and resume because the SDK exposes no tools-changed push event. Existing executions retain their admitted concrete registry. Later executions expand the persisted patterns again. Nested policy checks use the cumulative ancestor policies so late registration cannot broaden past an older ancestor.

## Verification

The native SDK probe confirmed UUID identity, deferred file creation, child-CWD lookup, and resume after disposing all original native objects. Pre-fix behavioral tests captured missing inspect metadata, the hidden 8 KiB handback cut, retained record limits, and rejection above 128 turns.

Registered operation tests verify UUID admission before materialization, fresh-owner reconstruction, name uniqueness, unknown UUIDs, native history recall, soft allowance reset, and an empty root ceiling. The HTTP MCP fixture verifies child skills, root configuration despite a different child CWD, codemode filtering, deferred declarations before and after tool-search discovery, late registration on resume, and transport shutdown. False host trust still permits allowed native writes and resume.

The stock `persistence` scenario runs two PiG 0.4.1 processes with a private parent session directory. The second uses PiG's actual noninteractive UUID flag, `--session ROOT_UUID`. It recovers the same child ID, directory, name, and private conversation, then verifies stop preserves a completed outcome. Other stock scenarios retain final-result rollback, privacy, subtree pause, live-child cancellation, native directory resources, and completion delivery.

The first three-seat final review found concrete activation, current-run recovery, receipt, and MCP authority gaps. Targeted tests reproduced them before correction. Four receipt crash forks now distinguish stored and missing final tool results, with either saved consumption bit, across two fresh owners. Additional native tests cover accepted input before storage and after storage with a stale root snapshot, failed disk append followed by UUID retry, conservative header failure, and provider-free root reads. These tests cover those interruption points. Orderly stock restart evidence does not establish a general crash guarantee.

Full-suite and comment evidence through `4900809` preceded the retirement-join commit. `recovery-all-checks.log` and fresh terminal/lifecycle logs cover `4971849`. The subsequent review corrections require fresh final gates. Raw reports and run outputs remain under `.pstack/issue-18` and `.pstack/evidence`. The decision trail is `.audit/issue-18.tsv`. Its appended correction records an earlier timestamp rewrite.

## Final review disposition

| Finding | Decision | Correction or limit |
| --- | --- | --- |
| Recovery scans a new run with a nil outcome. | Act on. | Capture immutable restored work before publication. |
| Acknowledged codemode activation can be lost. | Act on. | Store activation before execution and order it against retirement. |
| Interrupted resume returns its old completed report. | Act on. | Reconstruct the interrupted current outcome as paused. |
| Accepted continuation disappears before native input. | Act on. | Persist pending text and reconcile its native leaf anchor. |
| Audit receipt loses identity or clears before tool-result storage. | Act on. | Store typed receipts and require persisted handback evidence. |
| Failed append appears successful through SDK memory. | Act on. | Reopen by UUID for proof and retry without the old manager. |
| Terminal result precedes storage or hides storage errors. | Act on. | Gate publication and resume, then hand back one outcome. |
| Corrupt-header discovery resembles a missing session. | Act on and limit. | Initialize only before a recorded input attempt; ambiguous lookup is unavailable. |
| Forced MCP trust changes the root endpoint. | Act on. | Reproduce root configuration selection without admission denials. |
| Resource MCP tools skip startup waiting. | Act on. | Wait for all three native resource tool names. |
| Validation scope, broad scan wording, and timestamp history are inaccurate. | Act on. | Scope claims to revisions and tested forks; append the trail correction. |
| A reviewer said no model processes launched. | Dismiss as a code finding. | That statement describes its own nested activity; the coordinator launched the review. |

The pinned source governs API behavior. Current [PiG SDK documentation](https://raw.githubusercontent.com/MichaelKinsy/PiG/main/docs/site/docs/sdk.md) and [extension SDK documentation](https://raw.githubusercontent.com/MichaelKinsy/PiG/main/docs/extension-sdk-surface.md) were checked against PiG 0.4.1.
