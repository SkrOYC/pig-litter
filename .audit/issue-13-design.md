# Native subagent session design

## Contract

Each subagent is a retained PiG session node. Its native session-manager UUID identifies it across turns, pause and resume. Parent ownership is immutable. Root uses the host identity and the name root. Public controls list active immediate children and authorize only that immediate parent. A UUID never grants authority.

Pausing a parent marks its subtree before cancelling active work. The nodes and conversations remain. Resume acts on one selected node; descendants need separate explicit resume calls. Normal completion and a later codemode script failure do not pause or revoke a successful child admission.

The existing six tools remain. Each child uses PiG native services, tools, settings and directory resources at its validated spawn cwd. File arguments and shell commands reach native tools unchanged. The child's own pipeline and inherited role/tool ceilings replace parent tool forwarding.

## Chosen shape

Use candidate A's bounded guarded session-node registry and fresh private execution pointers around retained PiG managers. Keep atomic tree invariants under one short mutex and execute native calls outside it. Do not introduce B's command-loop actor protocol or retain native runtime objects solely to avoid recreation.

Resolve each child's named agent definition and native resources against its spawn directory. The role supplies its tool ceiling. Root's active file-tool selection does not constrain first-level child roles; nested delegation can only narrow its parent's ceiling. Root-level tree budgets remain ceilings on shared capacity. Directory settings and role instructions use the native resource loader; literal named instructions are appended to its normal prompt.

Final individual tool-call success commits an admission for direct and nested calls. Use actual tool-execution result evidence after hooks, before a next nested wait can proceed. Enclosing script completion is never an activation barrier. Failed/cancelled individual calls before that point roll back only their own provisional work.

Private call tickets bind a native caller, model turn, and tool call before any side effect. Final event handlers capture that ticket and settle it once. Cancellation and retirement use the same pointer. Hook-editable result content and details carry no identity. PiG's synchronous dispatch closes a model turn before a reused provider call ID enters a new scope. Concurrent duplicate IDs in the same scope are rejected. Keep admission settlement, terminal handback consumption, and completion storage acknowledgement separate. A successful terminal handback suppresses duplicate model-visible notices. Release delivery capacity after actual stored message/audit evidence, or conclusive shutdown abandonment. Paused and idle nodes receive retained messages without inference.

Publish outcomes after native Send settles. Acknowledge PiG event barriers before filtering events. Join message workers and clear deferred inputs on pause so they cannot restart implicitly. Serialize native-session install/disposal and retained-manager delivery. Cleanup uses a live bounded context and joins drains before closing owned resources.

Keep the initial task pending until PiG stores its marked user-input transaction. An awaited Agent subscriber runs after native Session persistence and clears that pending input. Setup pause or failure before storage leaves the task available to explicit resume. Later resumes and compaction cannot replay an already stored task.

## Arena result

The parent and independent gpt-6-astra judge selected A, scoring it 26 of 30. The judge scored B 23 and C 15. The parent scored B 24 and C 20; both assessments rejected their final-success and consumed-result gaps.

Graft B's explicit embedded-native configuration boundary, post-Send publication, per-turn usage accounting and joined-input cleanup. Graft C's explicit acknowledgement-first event pump and live cleanup context. Native manager UUID, direct-parent ownership and original native tool arguments converge across all candidates.

Reject B's deliberate model-visible notice after a consumed wait, its actor protocol as unnecessary for this bounded tree, and C's handler-time admission/consumption before final result hooks. Reject C's duplicated topology indexes and single-notice replacement without an admission bound.

PiG 0.4.1's public resource loader discovers settings, context, skills, templates and prompt resources. Arbitrary CLI extension/theme discovery is internal to the CLI. That is a documented embedded-SDK boundary, not a reason to build a new CLI bootstrap or restore forwarding.

Trusted delegation authorizes the target directory's native project settings through the SDK. There is no independent CLI trust-store lookup for that target. Keep the root trust check and role ceilings. PiG's public final-event payload also has no originating request token for adversarial raw-envelope replay across model turns. The ticket design relies on measured native event ordering and protects delayed local callbacks through captured pointers.

## Evidence and verification

Pinned APIs were checked in the Go module cache and upstream PiG v0.4.1. The unchanged unit/race/vet/build suite and released-host core/cwd scenarios passed. A new codemode spawn+wait fixture is red, with a timeout and no child provider request. Its stabilized evidence is codemode-1094588191 in the fixture worktree.

Gate each implementation unit with behavioral unit/race checks. Then require released-host nested success, spawn/resume followed by outer failure, result-hook failure, cancellation, parallel/out-of-order/duplicate/stale callbacks, bounded completions, pause/resume ownership, native directory resources, canonical @ file targets, and terminal selection/interrupt/cleanup verification.

The model is inspired by [Claude Code subagents](https://code.claude.com/docs/en/sub-agents), [directory settings](https://code.claude.com/docs/en/settings) and [project instructions](https://code.claude.com/docs/en/memory). PiG APIs determine discovery details. Pig Litter retains its explicit nested delegation, ownership and pause contract.
