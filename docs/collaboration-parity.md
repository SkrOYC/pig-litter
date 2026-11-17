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

An `Evidence` record identifies a requirement, a scenario, a branch, an exact commit, the host, relevant binary versions, the command, the observation, retained artifact paths, a verdict, and the scope of that verdict. Its kind distinguishes unit, live, and performance evidence. A result that covers one platform or fault point cannot establish a broader claim. The initial catalog contains no runtime records.

## Product evidence states

| State | Meaning |
| --- | --- |
| `matched` | The required Pig Litter behavior has current, applicable runtime evidence for every claimed environment. |
| `deliberately_different` | The implementation has a measured, documented difference from the reference. |
| `blocked` | A named prerequisite prevents a required behavior from being verified or implemented. |
| `unsupported` | The product explicitly does not support the behavior, with a stated reason. |
| `unverified` | Evidence is missing, stale, incomplete, waived, or inconclusive. |

Only `matched` counts as matched parity. A passing product scenario can establish `matched` only when the reference relationship is `equivalent` and all required evidence applies. A passing scenario for a `deliberately_different` target records that difference instead. An `unreviewed` relationship cannot establish a match. A deliberately different, blocked, unsupported, waived, unmeasured, stale, or inconclusive behavior cannot count as parity. The report must show these states separately and must not convert a waived check into a pass. A platform that has no real host evidence remains unverified or blocked for that platform. In particular, a Linux fixture does not establish macOS or iTerm2 behavior.

A scenario may have multiple evidence records when a claim spans different processes, fault points, or platforms. Source provenance stays in the requirement and is never substituted for a runtime record.

The scenario registry names the expected result and owner for every requirement. Its current `planned` status is not a test result. The `baseline` and `host-*` entries name the P02 experiments. Their driver and report are not implemented in this initial contract snapshot.

## Host research and product claims

P02 research must establish whether the upgraded PiG host can support the proposed delivery and enforcement contracts. A conclusive unsupported result can complete a research scenario when its command, observed side effects, and minimal upstream prerequisite are reproducible. It leaves the dependent product requirement blocked or unsupported. An inconclusive experiment fails the research scenario.

The message experiments must measure busy and idle delivery separately. The evidence must identify durable acceptance, actual native transcript append, acknowledgement, and any provider requests as separate events. The fault cases must interrupt the process between acceptance, append, and acknowledgement, then retry the same message ID after restart. Elapsed time or a successful send return alone does not prove transcript storage or exactly-once delivery.

The enforcement experiment must invoke a real rejecting tool hook and inspect the fixture side effect. It must cover nested calls as well as a direct write. A missing hook path or a successful side effect is a concrete blocker for the dependent plan gate. The independent-process experiment must start a real PiG process with the extension explicitly selected and record its session identity and registered tools. Neither experiment may infer support from a signature alone.

A member-to-member message must not require a lead model turn as a relay. The routing choice remains open until the host experiments establish the applicable runtime and process contracts. [PiG extension documentation](https://pi-in-go.dev/docs/latest/extensions/) and the [PiG Go SDK documentation](https://www.pi-in-go.dev/docs/latest/sdk/) describe public integration points, but those descriptions do not prove delivery timing, transcript persistence, or nested veto coverage.

## Verification artifacts

The planned scenario wrapper selects one registered scenario and gives each run isolated writable state. A complete run retains monotonic timing, process and provider logs, raw terminal output, ANSI frames, screenshots, terminal video, and a structured result. Failures and interruptions retain their artifacts and report an inconclusive or failed result. Two concurrent runs must not share fixture homes, ports, sockets, session identities, or output paths.

The existing lifecycle and terminal drivers remain the starting points for the wrapper. The current terminal driver has Linux-specific allocation and cleanup checks. A portable command-construction test cannot replace a real macOS or iTerm2 run. The catalog must not label any future scenario as passed until its actual artifacts and result exist.
