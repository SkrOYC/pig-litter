# Collaboration parity contract

This contract tracks proposed Pig Litter behavior against the collaboration behavior extracted from Claude Code 2.1.322. The machine-readable catalog is [requirements.json](../scripts/testdata/collaboration/requirements.json). Each requirement has a stable ID, reference provenance, a proposed behavior, an owning pull request, an acceptance scenario, and a product evidence state. The catalog is version 1. Every product requirement starts as `unverified`.

The reference extraction is local research material. Its paths and section names identify the source of each observation. They do not imply that the extracted source or prompts are part of this repository. The [official agent teams documentation](https://code.claude.com/docs/en/agent-teams) describes the public team model. The catalog uses the version-specific extraction for internal protocol details.

## Reference evidence

Reference provenance and Pig Litter verification are separate. A source observation can be well supported while the corresponding Pig Litter behavior remains unverified.

| Source kind | Meaning | Limit |
| --- | --- | --- |
| `static_implementation` | The extraction identifies a code path, schema, or literal. | It is not a captured end-to-end reference team run. |
| `captured_runtime` | A reference screen or command was captured from the running reference binary. | The captured sessions did not include a live teammate or a model turn. |
| `reconstructed_screen` | A screen or interaction was reconstructed from reference implementation evidence. | Layout and behavior still need live verification. |
| `proposal` | The accepted collaboration plan specifies a target or stronger guarantee. | The target is not an observation of the reference. |

Confidence describes the source observation, not the state of Pig Litter. `high` means the cited source directly supports the observation. `medium` means it depends on reconstruction or inference. The extracted material also names unknowns, including parts of cross-session routing and background task schemas. Those unknowns do not become claims merely because a related requirement exists.

The extraction identifies 16 named protocol variants across its message discriminator, schemas, out-of-band forms, and protocol classifier. `shutdown_response` is a structured input and classifier form. `shutdown_approved` and `shutdown_rejected` are separate resulting mailbox frames. Ordinary text is a distinct message case and does not add another typed variant. The catalog records each variant independently so that a combined scenario cannot silently omit one. Run this command to count the recorded variants:

```sh
bun -e 'const c = await Bun.file("scripts/testdata/collaboration/requirements.json").json(); console.log(c.requirements.filter(r => r.frame).length)'
```

## Catalog records

The catalog contains a list of `Requirement` records and a registry of `Scenario` records. A requirement has an ID, title, reference, proposed behavior, reference relationship, owner, acceptance scenario ID, specific acceptance assertion, and product evidence state. The reference has a source kind, path, section, confidence, and observation. A protocol requirement also names its variant and its role. A scenario has an ID, owner, planned status, and expected result. Requirement and scenario IDs must be unique, and each requirement must reference a scenario with the same owner.

The reference relationship describes the proposed behavior, independently of whether Pig Litter implements it. Its states are `unreviewed`, `equivalent`, and `deliberately_different`. A deliberate difference includes a reason. For example, `TEAM-003` preserves named ordinary children and uses an explicit teammate tool, while the reference selects teammate creation by name. Requirements without a reviewed comparison remain `unreviewed`.

An `Evidence` record identifies a requirement, a scenario, a branch, an exact commit, the host, its platform and architecture, relevant binary versions, the command, the observation, retained artifact paths, a verdict, and the scope of that verdict. Terminal-specific evidence also identifies the terminal. Its kind distinguishes unit, live, and performance evidence. A result that covers one platform or fault point cannot establish a broader claim. The initial catalog contains no runtime records.

## Product evidence states

| State | Meaning |
| --- | --- |
| `matched` | The required Pig Litter behavior has current, applicable runtime evidence for every claimed environment. |
| `deliberately_different` | The implementation has a measured, documented difference from the reference. |
| `blocked` | A named prerequisite prevents a required behavior from being verified or implemented. |
| `unsupported` | The product explicitly does not support the behavior, with a stated reason. |
| `unverified` | Evidence is missing, stale, incomplete, waived, or inconclusive. |

Only `matched` counts as matched parity. A passing product scenario can establish `matched` only when the reference relationship is `equivalent` and all required evidence applies. A passing scenario for a `deliberately_different` target records that difference instead. An `unreviewed` relationship cannot establish a match. A deliberately different, blocked, unsupported, waived, unmeasured, stale, or inconclusive behavior cannot count as parity. The report must show these states separately and must not convert a waived check into a pass. A platform that has no real host evidence remains unverified or blocked for that platform. In particular, a Linux fixture does not establish macOS or iTerm2 behavior.

The trusted applicability profiles define the targets for a full-program parity claim. Core collaboration behavior covers Linux and macOS. The iTerm2 backend covers macOS with iTerm2. A profile can name an architecture when the requirement claims one. An omitted architecture makes no architecture-specific claim. A missing or caller-narrowed profile leaves the requirement unverified. Per-target rows describe the live evidence for each target. The aggregate full-program result also requires the unit and performance slots.

A scenario may have multiple evidence records when a claim spans different processes, fault points, or platforms. Source provenance stays in the requirement and is never substituted for a runtime record.

The scenario registry names the expected result and owner for every requirement. A `planned` or `implemented` status describes the driver, not a test result. The P02 entries cover baseline, startup, host probes, isolation, interruption, and performance. Future product scenarios remain planned until their owning changes provide reviewed proof rules.

## Host research and product claims

P02 research must establish whether the upgraded PiG host can support the proposed delivery and enforcement contracts. A conclusive unsupported result can complete a research scenario when its command, observed side effects, and minimal upstream prerequisite are reproducible. It leaves the dependent product requirement blocked or unsupported. An inconclusive experiment fails the research scenario.

The message experiments must measure busy and idle delivery separately. The evidence must identify durable acceptance, actual native transcript append, acknowledgement, and any provider requests as separate events. The fault cases must interrupt the process between acceptance, append, and acknowledgement, then retry the same message ID after restart. Elapsed time or a successful send return alone does not prove transcript storage or exactly-once delivery.

The enforcement experiment must invoke a real rejecting tool hook and inspect the fixture side effect. It must cover nested calls as well as a direct write. A missing hook path or a successful side effect is a concrete blocker for the dependent plan gate. The independent-process experiment must start a real PiG process with the extension explicitly selected and record its session identity and registered tools. Neither experiment may infer support from a signature alone.

A member-to-member message must not require a lead model turn as a relay. Use programmatic runtime routing for embedded sessions and a persistent mailbox for independently running processes. P04 must establish durable acceptance and idempotent delivery before this is a supported product contract. A product-owned serialized receipt and reconciliation mechanism or an upstream atomic idempotent append contract could supply that guarantee; the current probes do not prove either. [PiG extension documentation](https://pi-in-go.dev/docs/latest/extensions/) and the [PiG Go SDK documentation](https://www.pi-in-go.dev/docs/latest/sdk/) describe public integration points, but those descriptions do not prove delivery timing, transcript persistence, or nested veto coverage.

## PiG 0.7.0 research observations

The host probes use the public PiG 0.7.0 SDK and an isolated local fixture provider. Their observations are provisional until the final P01 host revision is verified. The tests establish host behavior, not Pig Litter product parity. They retain native session files, provider requests, monotonic timings, and process results when `P02_PROBE_EVIDENCE_ROOT` is set.

| Observation | Result |
| --- | --- |
| Fresh idle send with `TriggerTurn=false` | The call returned and emitted a message-end event while the custom entry existed only in memory. The native session file appeared after the first conversation. |
| Established idle send with `TriggerTurn=false` | One entry was visible in the native file on return, with no additional provider request. |
| Busy send with `TriggerTurn=false` | The call returned before the entry appeared in memory or on disk. The entry appeared after settlement without an additional provider request. |
| Busy steering send | The call returned before native storage. After settlement, one additional provider request contained the message. |
| Idle send with `TriggerTurn=true` | The call returned after one additional provider request, with one entry visible in the native file. |
| Same-ID replay after a process crash | Replay after acceptance but before append produced one entry. Replay after append produced a second entry, both before and after the send returned. |
| In-process tool hook | A rejecting hook stopped direct file writes, nested codemode writes, direct MCP writes, and codemode-to-MCP writes in the controlled fixture. The matching allowed controls produced their files. |
| Independently selected process | A separate PiG process loaded the standalone extension and exposed the six selected litter tools to its fixture provider. Its native session header carried the requested session ID. |
| Embedded tool registration | A factory-registered tool and two explicitly named extra tools appeared in both the native session and the provider request when allowed. Narrowing the allowlist exposed only the selected names; an allowlist containing no registered name exposed none. |

The message-end event is not a general storage acknowledgement. The observations establish process-visible file contents after the named events, not file synchronization or power-loss durability. PiG does not suppress replay based on the ID placed in custom-message details. One proposed delivery design persists acceptance separately and serializes replay with native storage checks. These probes do not establish that design's crash safety. An upstream atomic idempotent append contract is another possible prerequisite. The hook result covers the tested in-process paths. Subprocess extension hooks, shell, and delegated mutation still need their own evidence.

The terminal message scenarios also exercise a private SDK extension in the same captured PiG session. A fresh idle passive send appeared in memory before the native file existed, then appeared in that file after the first conversation. An established idle send appeared in the native file without starting another provider request. A passive send during a held provider turn returned before its native append; after the provider was released normally, one entry appeared without an additional request. The SDK call response, append observation, and provider requests are separate records. The SDK response does not establish durable acknowledgement.

The message and crash probes are in [collaboration_message_probe_test.go](../scripts/cmd/verify-lifecycle/collaboration_message_probe_test.go) and [collaboration_crash_probe_test.go](../scripts/cmd/verify-lifecycle/collaboration_crash_probe_test.go). The independent-process, embedded registration, and hook probes are in [collaboration_process_probe_test.go](../scripts/cmd/verify-lifecycle/collaboration_process_probe_test.go), [collaboration_registration_probe_test.go](../scripts/cmd/verify-lifecycle/collaboration_registration_probe_test.go), and [collaboration_hook_probe_test.go](../scripts/cmd/verify-lifecycle/collaboration_hook_probe_test.go). The registration probe establishes tool exposure, not tool execution or team behavior. These test results do not replace the required live terminal lanes or performance checks.

The handoff keeps the remaining work scoped to its owner:

- P03 can build explicit team registration on the observed embedded extra-tool and allowlist APIs. It must preserve ordinary child permissions and allow shared-checkout writers only in explicit team mode.
- P04 must prove a durable acceptance and same-ID replay contract. The observed native append behavior alone does not supply one.
- P06 must verify every enforcement path it relies on. The current in-process observations do not establish subprocess hook transport, shell behavior, or delegated mutation controls.
- P08 must verify independent team process startup and terminal backends. The selected standalone litter process establishes only the existing tool registration and session identity.

## Verification artifacts

The scenario wrapper selects one registered scenario and gives each run isolated writable state. A complete run retains monotonic timing, process and provider logs, raw terminal output, ANSI frames, screenshots, terminal video, and a structured result. Failures and interruptions retain their artifacts and report an inconclusive or failed result. Two concurrent runs must not share fixture homes, ports, sockets, session identities, or output paths.

The report records each research scenario's completion separately from any named host capability finding. It flags conflicting findings only when their capability, scenario revision, platform, architecture, and terminal match. A finding does not block a product requirement unless a reviewed product proof rule explicitly names that dependency. Program target rows require matched live evidence for every applicable requirement on that target. Full parity also requires all other proof slots and reference relationships to match.

The existing lifecycle and terminal drivers remain the starting points for the wrapper. The current terminal driver has Linux-specific allocation and cleanup checks. A portable command-construction test cannot replace a real macOS or iTerm2 run. The catalog must not label any future scenario as passed until its actual artifacts and result exist.

The report validates PNG screenshots and self-contained MOV or MP4 video from immutable snapshots of the selected bytes. Video decoding uses only the snapshot descriptor and disallows external references. The decoder has time, memory, pixel, and output limits. If the host cannot apply those limits or decode the media, the evidence remains incomplete.

Each target identity JSON contains `branch`, `product_commit`, `driver_commit`, `product_binary_sha256`, `host_binary_sha256`, and `catalog_sha256`. The product commit and extension digest identify a clean target checkout and its separately built standalone executable. Build each target with `go -C scripts build -p 2 -mod=readonly -buildvcs=false -trimpath -o "$P02_EXTENSION" ./cmd/pig-litter` inside its own `devenv` environment. The driver commit identifies the clean checkout running the wrapper. The host digest identifies the PiG executable declared by the target checkout's `devenv` environment. The catalog digest identifies the exact requirements file used by the run. Before accepting an attempt, the driver checks that the declared branch contains the checkout commit, records the resolved ref and its commit, rebuilds the extension, and compares the resulting digest.

With absolute paths to that target JSON, checkout, PiG executable, extension executable, and private evidence directory, run a scenario as follows:

```sh
devenv shell -- scripts/verify-collaboration.sh --scenario startup-standalone \
  --target "$P02_TARGET" --target-checkout "$P02_CHECKOUT" \
  --pig-bin "$P02_PIG" --extension "$P02_EXTENSION" \
  --evidence-root "$P02_EVIDENCE"
```

The `baseline` and `capture-performance` scenarios also require `--baseline-target`, `--baseline-checkout`, `--baseline-pig-bin`, and `--baseline-extension`. Their baseline is the final P01 parent. Both targets use a fixture driver built from the candidate checkout. Linux terminal recording needs Xvfb, xterm, and an FFmpeg build with `x11grab`; the `--media-xvfb`, `--media-xterm`, `--media-ffmpeg`, and `--media-ffprobe` options select their executables.

Each attempt writes a `selection.json` that identifies its manifest by digest. Pass an explicit selection and the same target identity to `scripts/verify-collaboration.sh --report "$P02_SELECTION" --target "$P02_TARGET" --evidence-root "$P02_EVIDENCE"`. Add `--require-parity` when the caller needs a nonzero exit unless the complete program has been verified.
