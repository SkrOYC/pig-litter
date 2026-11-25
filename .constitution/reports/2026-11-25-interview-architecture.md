# Architecture interview

This full interview prepares the first architecture pass for Pig Litter. The record is dated November 25, 2026 and was held in the Claude Code session on branch `docs/initialize-constitution` at `f238f9f`, against `prd/` v0.0.1. The mode is brownfield. The owner chose the full pass: four read-only reconnaissance agents, and up to three spikes by agreement.

The owner set one standing instruction for this interview: go over every decision explicitly and assume nothing from the state of the repository. The existing code is described below only as what exists today.

## Rulings that change the product requirements

These rulings contradict or extend `prd/` v0.0.1. Stage 2 can't apply them. They need a Stage 1 Evolution pass before the architecture is written, and the architecture then cites that PRD version.

1. **Ending the root is a full pause.** Ending the root stops all live work in its tree and leaves every dependent child reachable when the root is reopened. The owner's words: "A full 'pause' better describes it." This replaces the full stop in CAP-028 and JOB-07.
2. **Permanent stop goes through the root agent.** The operator has no separate control for permanently stopping a tree. The operator tells the root to stop its children.
3. **A session with an active child is active.** A session holds a slot while its own turn is open or any of its children holds a slot. The owner's words: "an agent with children is still an active one." This replaces the rule in CAP-010 and the glossary that finished and paused sessions take no slots. Waking a child under idle ancestors must admit every idle ancestor, and one wake can therefore need several slots.
4. **Two kinds of launched session.** A dependent child runs inside its tree's process and is hidden from everything outside the tree. A detached session runs in its own process, heads its own tree, and is the only session of that tree that outside sessions can discover and message. This is a new domain concept for the glossary and the domain model.
5. **Messaging topology.** Inside a tree, a session messages only its direct children and its own parent. Peer discovery and peer messaging belong to roots, and a dependent child can't send to or receive from a session outside its tree. This narrows CAP-014 to CAP-018, which let any peer reach a child.
6. **The launcher of a detached session keeps stop authority and nothing else.** No handback, wait, pause, or state query applies to a detached session. Stopping it ends its whole tree and its process, under the same cascade warning as any stop. Detached sessions it launched are untouched. The stop authority is remembered across a restart of the launcher.
7. **Detached launch is a launch mode for roots only.** An operator-started root or a detached session can launch detached sessions. A dependent child can't.
8. **Detached sessions are fully independent.** A detached session outlives its launcher and never ends by itself. It ends when its launcher stops it or the operator ends its process. It takes its own limits and its own permissions from its own configuration, and nothing caps the number of detached launches. The interviewer flagged that this lets a root create sessions with more tools than it holds and without a workflow-wide bound. The owner accepted it with detached launch restricted to roots.
9. **Detached sessions can run in a terminal pane.** With a supported pane host, a detached session opens in a pane as an interactive PiG session. Without one, it runs in the background, and the launch result states which form was used.
10. **A detached session is told its launcher's address** at launch, so it can report by peer message.
11. **Delegation is available by default.** Any session can launch dependent children unless it is at the maximum depth or its profile is read-only. This replaces the separate delegation access in CAP-008.
12. **A profile declares whether its role writes.** The directory overlap rule in CAP-013 uses that declaration.
13. **Settings live in `litter.yaml`.** The owner's words: "It's litter.yaml; piglet.yaml was a mistake." This replaces the `piglet.yaml` preference in the vision appendix and the declared surface.
14. **Peer messaging has an on or off switch** per root, on by default.
15. **Interrupted is a distinct state.** A child interrupted by a crash reports as interrupted until its next turn starts.
16. **A failed turn produces a failure handback** under the same rules as a normal handback.
17. **Wait returns at once on an inactive child,** and the maximum wait is configured with a default of 30 minutes.
18. **Child names are unique across the root tree.**
19. **Legacy features.** Teams, teammates, plan approval, shutdown requests, and teammate policy commands have no place in the target. The interviewer suggests that Stage 1 record them under `out-of-scope/`.

## Rulings for the architecture

### Baseline

- Design the target architecture from the PRD alone. Existing code is reusable material. A boundary cites the code as evidence only where a module already matches the design. Refactor or rebuild is a later decision.

### Crash and recovery

- After a crash, each interrupted dependent child keeps its conversation and identity and sits inactive. A steer from its parent can wake it in the same conversation, subject to capacity. The tree's membership and each child's conversation are therefore stored durably.
- The report of interrupted children enters the parent's conversation as a labeled notice from Pig Litter, in interactive and headless runs.
- While a root is closed, nothing in its tree is discoverable or wakeable, and a send to it fails.
- A crashed detached session becomes unreachable. Sends to it fail and discovery stops listing it. Pig Litter restarts nothing. Its conversation stays on disk for the operator to reopen, and its own interrupted children are then reported as for any root.

### Headless runs

- A headless root stays alive while any descendant is active, so that handbacks can wake it. It ends once the root is idle and no descendant is active. If stock PiG can't hold a headless run open, that becomes a stated limitation.
- A root can't wait on a detached session. A headless root that coordinates detached sessions exits after its turn and misses their messages. The owner accepted this.

### Handbacks, pause, and stop

- A handback wakes a paused parent like any other steer. The interviewer noted that a pause on a session with busy children then lasts only until the first of them finishes. The owner chose uniform behavior.
- A handback never needs a new slot, because its parent already holds one under ruling 3.
- A handback has no size limit.
- A plain stop on a child with descendants is refused with a warning that states how many sessions would stop. The parent repeats the stop with an explicit cascade acknowledgement, and can pass that acknowledgement on the first call. No state is kept between the two calls.
- After a full stop, a state query on the child answers stopped for as long as the parent's tree exists. No conversation content is available.
- Stopping a child mid-turn returns status only. No handback is produced.

### Messaging

- A send does the hand-off and returns at once with its outcome. It never waits for the recipient to read or act. An accepted result says the recipient will see the message after its next tool boundary or was woken. A failure carries its reason.
- A wake rejected for capacity discards the message and tells the sender.
- A steer accepted for an active session and not yet delivered can be lost if the recipient's process crashes.
- An oversize send is rejected with the limit.
- Each session holds a bounded number of pending steers. Beyond the bound, a send fails as recipient busy. Handbacks are exempt.
- The receiving side does nothing special with a message it can't understand. The exchange fails, and the sender learns of it. The owner's words: "If it fails, it fails, and the sender is the one that knows it."
- Pig Litter treats every Claude Code version alike and reports only actual failures. The interviewer recommended marking unchecked versions as unverified. The owner chose not to.
- The trust boundary for peer messages between roots is the operating-system account. The architecture states that it doesn't protect against a hostile process under the same account.
- An operator-started root takes its discoverable name from its host session name, or a generated name when it has none. Peers address a root by a full address that doesn't depend on the name being unique.

### Wait

- A wait interrupted by a pause or by the operator ends with the interrupted turn. The awaited child is unaffected.

### Operator view

- The operator can open any dependent session in the root's tree, at any depth. Detached sessions aren't viewable from their launcher.
- Input from the overlay arrives as operator input, distinct from a parent's message and a peer message, under the same delivery rule as any steer. A wake over capacity shows the capacity error in the overlay.
- The overlay offers follow and steer only, and says so. The operator can't interrupt a child's turn from it.
- The parent isn't told when the operator steers its child.
- The root's terminal view lists the whole tree with each session's name, state, profile, and working directory. Detached sessions that the root launched appear in a separate group with only what a peer can know.
- No event record exists beyond these views. The interviewer recommended an operator-only record without conversation text. The owner chose none.

### Configuration and admission

- A root reads its configuration once when it starts or is reopened. The tree uses those values until the root ends. A detached session reads its own configuration from its own working directory.
- Profiles and settings come from the user level and from the root's project. A child's working directory contributes nothing.
- The directory overlap rule applies within one tree. Pig Litter doesn't coordinate directories between trees.
- The launch call rejects everything checkable up front, with the reason: capacity, depth, a name already used in the tree, a missing or overlapping directory, an unknown profile, tools beyond the parent's permissions, a read-only or maximum-depth caller, and an unknown model or effort. Problems that appear once the child runs come back as a failure handback.
- Pig Litter imposes no bound on a child's turn.

### Design decisions

- **One coordinator per tree.** A single component per root owns membership, each session's state, admission, and routing inside the tree. Every session's delegation tools call into it at any depth. The grounds are the tree-wide name and slot rulings and NFC-04, which needs admissions that can't race.
- **The tree record lives with the root's conversation.** Reopening that conversation by any normal PiG means restores the tree. A root that runs without persistence has no durable tree, and the architecture states that as a limitation.
- **Claude Code's local peer protocol carries all peer messaging,** including PiG to PiG. The owner said to assume the protocol details are known to the owner, so feasibility isn't an open question. The technical specification gets those details from the owner. The interviewer stated the accepted risk: PiG-to-PiG messaging can break when a Claude Code release changes an undocumented format, and anything Pig Litter needs beyond what the protocol carries has to fit inside it.
- **Unattended hosting is closed by a spike.** The headless keep-alive and the background detached session depend on unmeasured stock behavior. The owner chose to run the spike in this interview with the system PiG binary. See OD-01.
- **Left to the technical specification:** whether a dependent child is hosted through the host's child-session call or through an embedded runtime in the extension's process. Both end with the root's process, so the logical design is the same.

### Spike result

[SPK-PL-OD001](../spikes/SPK-PL-OD001.md) ran against the system PiG 0.7.0 (build `beb5644`) on Linux with a scratch extension and closed OD-01 with the option "Print mode held open by the extension".

- A headless run stayed alive while the extension held it at the end of a turn, ran further turns in the same conversation, and exited normally on release.
- A background session started outside the host's process group survived a normal exit and a kill of its launcher and took later input. Started inside that group, it died with the launcher.
- A long-lived host mode with a helper that holds its input also worked for the background session. It is the fallback and can't serve a headless root.
- A pane host worked as the baseline for the pane form.
- Not measured: an abort or a termination signal during a hold, and macOS.
- The spike notes that a held headless root could also receive messages from detached sessions. The owner's ruling that detached sessions don't keep a headless root running stands until the owner revisits it.

## Preferences to carry to the technical specification

- One file per profile, `litter/<profile>.md`, beside `litter.yaml`. The user level is `~/.config/pig/agent/` and the project level is `.agents/`.
- Supported pane hosts are tmux and iTerm2, as in the existing code.
- Use Sonnet for reconnaissance and research without judgment, Haiku for token-heavy web research, and Opus for workers and reviews.
- Stock PiG's project directory is `.pig` and it reads `.agents` only for skills. The `.agents/litter.yaml` preference works as Pig Litter's own lookup. Raise the mismatch in the TechSpec interview.
- For measurements, use the system PiG at `/etc/profiles/per-user/oscar/bin/pig` (0.7.0, build `beb5644`), not the patched preview that the project environment puts on the path.

## Pinned sources

Checked November 25, 2026. Two Sonnet agents read the stock PiG 0.7.0 module and the Go extension SDK 0.7.0 from the local module cache, and a Haiku agent read the Claude Code documentation. The interviewer checked the cited lines for every item except those marked as reported.

- Stock PiG runs a Go extension as a subprocess over a local socket and kills its process group at host shutdown (reported; `coding/extension/host/subprocess/host.go`, `process_tree_unix.go`).
- The SDK's child sessions support prompt, steer, follow-up, abort, and an event stream, and end with the extension's connection (`extensions/sdk@v0.7.0/child_session.go:16-22,487-588`).
- An extension can inject a message into its host session: a steer during a turn, or a new turn when idle (`coding/session_custom_messages.go:58-72`). A steer takes effect after the turn's tool calls (`agent/agent_loop.go:255-257`).
- A before-settle handler can request another run (`coding/session_boundaries.go:145-202`).
- RPC mode exits at end of input (`coding/cli/rpc_mode.go:216`). Stock has no daemon, server, or attach point (reported; `docs/site/docs/cli.md`).
- The host's full-screen overlay flag (`tui/tui.go:897-901`) isn't in the SDK's overlay options. Custom views do nothing in RPC mode (reported; `docs/site/docs/rpc-extension-ui.md`).
- The stock SDK has neither `HostProcessIdentity` nor `ProviderAvailability`.
- [Claude Code cross-session messaging](https://code.claude.com/docs/en/cross-session-messaging). Each session binds an inbox socket restricted to the operating-system user. A message is read between tool calls during an active turn and starts a new turn in an idle session. The receiver delivers, holds, or refuses each message under its own inbound controls. Refusals reach the sender as results that begin `Not sent` or `Not delivered`. Messages are plain text with a cap near one million characters. The page requires v2.1.224 on macOS and Linux. It doesn't document how another program becomes a listed peer or the message format on the socket.

## Repository findings

Four Sonnet agents inspected the code at `f238f9f`. The interviewer checked the cited lines for the first nine items.

- A child is a PiG runtime created inside the extension's process (`extensions/pig-litter/adapter.go:593-646`).
- Peer startup calls `HostProcessIdentity` (`peer_root.go:66`) and child model selection calls `ProviderAvailability` (`adapter.go:408`). The reconnaissance reports both as absent from stock PiG 0.7.0.
- No Go file refers to Claude Code. The peer wire protocol is Pig Litter's own.
- `litter_stop` pauses a child and its descendants (`operations.go:39`). A plain child has no fully stopped state.
- The wait timeout is optional and defaults to 30 seconds (`operations.go:119,660`). A wait returns the child's result and consumes the handback.
- A handback doesn't wake an idle parent, and a root in the terminal interface takes a different delivery path from a headless root (`completion.go:637-653`).
- Admission counts children only and compares writer directories by equality (`tree.go:226-249`).
- Peer sessions are off until the operator opts in (`peer_root.go:28-34`).
- Limits live in `litter.yaml`.
- Unchecked by the interviewer: teams, shared tasks, plans, policies, observers, and pane backends are over half of the non-test code and are wired into the tree, persistence, tool registration, and the panel. Peer messaging is implemented inside the team store. The panel shows child metadata and no live transcript. The extension already subscribes to child events and keeps only counters.

## Continuation and handoff

- **Authorized continuation.** The owner chose to update the product requirements first and then write the architecture, and then said: "Continue autonomously now until architecture is done and everything commited and pushed". The Stage 1 Evolution pass (`prd/` v0.1.0) and the Stage 2 pass (`architecture/` v0.0.1) ran under that authority, each in its own commit on `docs/initialize-constitution`.
- **Stop boundary.** Work stops when the architecture is committed and the branch is pushed. No pull request, technical specification, or code change was authorized.
- **Channel.** The Claude Code session in the terminal.
- **Register.** OD-01 is closed by the spike. No entry is open.

## For the technical specification interview

- The choice between the host's child sessions and an embedded runtime for dependent children.
- The Claude Code peer protocol details, which the owner holds.
- The settings and profile paths, given that the released host uses a different project directory.
- Released replacements for the two patched-only host interfaces the existing code calls.
- The unmeasured cases from the spike: an abort during a hold, and macOS.

## Files written

- `reports/2026-11-25-interview-architecture.md`
- `reports/open-decisions.yaml`
- `spikes/SPK-PL-OD001.md`
