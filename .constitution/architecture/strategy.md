# Solution strategy

## The pattern

Pig Litter is a modular extension with one coordinator per root tree. A root and all its dependent children run as one process group on the released PiG host. A single tree coordinator owns the tree's membership, session states, and admissions. Everything that enters a session's conversation goes through one steer delivery path. Roots talk to the outside through a peer gateway, and a root starts independent work by launching another PiG process that becomes a root of its own.

This is the target design. It follows the product requirements at `prd/` v0.1.0 and the rulings of the [architecture interview](../reports/2026-11-25-interview-architecture.md). The owner ruled that the existing code is reusable material and not a constraint, so every boundary is assumed until code that matches it exists.

## Why it fits

- **The archetype is CLI tooling on someone else's host.** Pig Litter adds operations and a view to PiG and must run on unmodified releases (NFC-01). The session host is the only boundary that runs sessions on the host, so a host limitation is found and stated in one place (CAP-040).
- **Limits must hold under concurrency.** NFC-04 allows no admission beyond the active-session limit, the depth, or the directory rule. A parent with an active child holds a slot, and a wake can need slots for several inactive ancestors. One coordinator that serializes every admission is the simplest structure that can't race. The interview recorded this ground.
- **One process per tree matches the product's lifecycle.** The owner ruled that dependent children live inside the tree's process and are hidden outside it, that ending the root halts the tree, and that reopening the root restores it. A single failure unit with a durable tree record gives exactly that behavior with no cross-process control channel.
- **Authority and privacy are enforced at one door.** NFC-02 and NFC-03 count unauthorized actions and exposed conversation entries, and the goal for both is zero. Every agent call passes through agent operations, which checks the caller's relationship to the target and returns states and outcomes only.
- **One delivery rule for every input.** A parent's message, a child's message, a handback, a peer message, an operator steer, and a recovery notice differ only in their label. One path gives one timing rule (after running tool calls, or as a new turn), one pending bound, and one place that resolves waits (NFC-05).
- **Independence is bought with a process, not with a protocol.** A detached session is an ordinary root in its own process. Its launcher reaches it through the same peer gateway as any other peer and holds one extra authority, the stop. No supervision channel exists between trees.

## Grounds for the pattern-level choices

| Choice | Ground |
| :--- | :--- |
| One coordinator per tree | Owner ruling in the interview, resting on NFC-04 and the tree-wide name and slot rules |
| Tree record stored with the root's conversation | Owner ruling. The released host lets an extension store entries in the host session's record (source reading, pinned in the interview record) |
| In-process protocol between modules | All modules of a tree share one process group by the owner's ruling on dependent children |
| Claude Code's local peer protocol for all peer messaging | Owner ruling and the recorded operator preference. The owner holds the protocol details. The public documentation confirms the delivery rule and the per-user socket and documents no way for another program to join |
| Detached session as a separate PiG process | Owner ruling. SPK-PL-OD001 measured that the released host can run it unattended, in a pane or in the background |
| Hosting a dependent child through the host's child sessions or through an embedded runtime | No ground at this layer. Both end with the root's process. The technical specification decides |

## Trade-offs accepted

- **The tree coordinator is a single point of dependence.** Every operation in a tree waits on it. The tree is one process, so nothing is lost that wasn't already shared.
- **A crash takes the whole tree.** Dependent children can't outlive their root. The owner accepted this, and the tree record with the interrupted state is the mitigation.
- **Peer messaging depends on an undocumented format.** A Claude Code release can break messaging between PiG roots as well as with Claude Code. The owner chose one mechanism over an isolated one.
- **Detached sessions escape the launcher's limits and permissions.** A root can create sessions with their own limits and tools, and nothing caps their number. The owner accepted this with detached launch restricted to roots.
- **No operator event record.** After a headless run or a crash, the tree record and the stored conversations are the only trace.
- **A headless root that coordinates detached sessions exits after its turn.** It can't wait on them, and their later messages find it closed. SPK-PL-OD001 showed that holding it open is possible, so the owner can revisit this ruling.
- **A pause on a session with active children is short-lived.** The first handback wakes it. The owner chose one delivery rule over a special case.
