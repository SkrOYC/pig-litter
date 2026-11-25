# Risks

Each risk names what would change if its assumption fails and the follow-up that tests it. Probes are routed to the technical specification interview or to planning. This file plans none itself.

## Structural risks

### R1. Peer messaging rests on a format nobody outside Claude Code documents

- **Assumption.** A PiG root can be listed and messaged through Claude Code's local peer protocol in both directions. The owner holds the protocol details. The public documentation confirms the delivery rule and the per-user socket and documents neither the message format nor a way for another program to join.
- **If it fails.** BND-06 changes, the peer messaging flow changes, and NFC-06 can't be met. A Claude Code release can also break messaging between two PiG roots, because the owner chose one mechanism for all peers.
- **Mitigation.** The peer gateway is the only boundary that knows the protocol. A send that can't be confirmed reports as sent with delivery unconfirmed, and a failure reports its reason, so a break is visible in the first send.
- **Cheapest probe.** List one PiG root from a Claude Code session and exchange one message in each direction. It holds the peer messaging flow and the detached session flow. Route it to the technical specification interview, where the owner supplies the protocol details.

### R2. The released host may not carry every in-tree interaction

- **Assumption.** The released host can create dependent children, deliver a steer after a tool batch, start a turn in an idle session, and stream a child's activity. Source reading supports each one. None has been exercised by Pig Litter's target design.
- **If it fails.** BND-04 changes, and CAP-040 requires the limit to be stated instead of worked around.
- **Mitigation.** The session host is the single seam. The technical specification decides between the host's child sessions and an embedded runtime, and both end with the root's process.
- **Cheapest probe.** A scratch extension that creates one child, steers it during a tool batch, and wakes it when idle. Route it to the technical specification interview.

### R3. The existing code calls two interfaces that only a patched host provides

- **Assumption.** The target design needs neither the host's process identity nor its provider availability check.
- **If it fails.** NFC-01 fails, or the session host must state a limit for root naming or for model checks at launch.
- **Follow-up.** The technical specification names the released replacement for each or records the limit.

### R4. The child overlay may not get a full view

- **Assumption.** The released host lets an extension draw a focused view that re-renders from live data and embeds a text input. The host's full-screen option isn't exposed to extensions.
- **If it fails.** The overlay is an approximation of the full view the product describes, and the operator view states that limit.
- **Cheapest probe.** Draw one overlay that follows a child's live activity and accepts a line of input. It holds the follow-and-steer flow.

### R5. Unattended sessions depend on one held run

- **Assumption.** A headless root and a background detached session stay alive by holding their run open. SPK-PL-OD001 measured this on Linux, including long idle holds and a launcher killed outright.
- **Not measured.** An abort or a termination signal during a hold, and macOS. The host's documentation says an abort skips the hold and ends the run.
- **If it fails.** A headless tree or a background detached session ends early. A background session held this way also never reports idle to its host, so its peer status must come from Pig Litter.
- **Mitigation.** The fallback measured by the spike, a long-lived host mode with a helper that owns its input, serves the detached session and not the headless root.
- **Cheapest probe.** Repeat the spike's hold and launcher-kill cases on macOS and send a termination signal during a hold. Route it to planning ahead of the work on CAP-047 and CAP-062.

### R6. One coordinator and one process per tree

- **Assumption.** Serializing every admission and state change in one place is fast enough for a tree's operations (NFC-04, NFC-05).
- **If it fails.** Operations queue behind each other. The boundary doesn't change, and its internals would.
- **Accepted consequence.** A crash of the root's process takes every dependent child. The tree record and the interrupted state are the recovery path.

### R7. The tree record shares the root's conversation storage

- **Assumption.** The released host lets Pig Litter store its own entries in the root's persisted conversation and read them back when the root is reopened.
- **If it fails.** BND-05 needs storage of its own, and the halt-and-recover flow changes.
- **Accepted consequence.** A root without a stored conversation has no durable tree. Pig Litter states this when that root first launches a child.

### R8. Detached sessions sit outside every limit

- **Assumption.** Restricting detached launch to roots is enough control, by the owner's ruling.
- **Consequence.** A root can start any number of sessions with their own limits and tools, and each of them can do the same. Nothing counts them.
- **Mitigation.** The root's session list shows the sessions that root launched. A session launched by a detached session appears only in that session's own list.
- **Follow-up.** If runaway launches appear in use, an Evolution pass on the product requirements adds a cap. The architecture has one place to enforce it, the detached launcher.

### R9. A lost launcher record orphans a detached session

- **Assumption.** The launch is written to the tree record before the launch call answers.
- **If it fails.** A detached session keeps running with no session able to stop it through Pig Litter. The operator can still end the process.
- **Related case.** A launcher root without a stored conversation loses its stop authority when it ends. Pig Litter states this at the detached launch.

### R10. Undelivered input is lost in a crash

- **Accepted consequence.** Pending steers and undelivered handbacks aren't durable. After a crash the parent learns that its child was interrupted and doesn't get the lost handback's text. The child's stored conversation still holds its final response.

### R11. Settings are looked up where the host doesn't look

- **Observation.** The owner places project settings and profiles under the project's agent directory. The released host uses a different project directory for its own files.
- **Consequence.** Pig Litter does its own lookup, and an operator may put a file where only the host reads. The technical specification interview settles the paths and the message for a misplaced file.

### R12. The owner's ruling leaves a headless root deaf to detached sessions

- **Observation.** By ruling, detached sessions don't keep a headless root running, so their later messages find it closed. SPK-PL-OD001 showed that a held root could stay alive and receive them.
- **Follow-up.** The owner can revisit the ruling in an Evolution pass. Until then the flows follow the ruling.

## Logical debt

- More than half of the existing code serves features the product now excludes. None of it settles a boundary, and every boundary record is assumed until matching code exists.
- The existing code has five delivery paths, a stop that pauses, a wait that consumes the handback, and a peer protocol of its own. Planning treats each as replacement work and not as a base to extend.

## Threat notes

Pig Litter has two trust boundaries. The depth here matches a single-user local tool. No money, tenant data, or safety function is involved.

### BND-01 Agent operations

| Threat | Note |
| :--- | :--- |
| Spoofing | A caller's identity comes from the session that the session host bound the call to, never from an argument the agent supplies. |
| Tampering | An agent can't change limits, profiles, or another session's permissions through any operation. Settings and profiles are fixed at root start. |
| Repudiation | Pig Litter keeps no event record, by ruling. The stored conversations show each call and its result. |
| Information disclosure | Results carry states, send outcomes, and wait statuses only. A rejection reveals nothing about the target (NFC-03). |
| Denial of service | A child can fill its parent's pending bound with messages. Sends beyond the bound fail as recipient busy, and handbacks are exempt. A child can't take more slots than the tree's limit. |
| Elevation of privilege | A dependent child's tools only narrow from its parent's. A child can't launch a detached session, which is the one route to a wider permission set (NFC-02). |

### BND-06 Peer gateway

| Threat | Note |
| :--- | :--- |
| Spoofing | Any process under the same operating-system account can claim any peer name. A sender's name is a label and not proof. A detached session can't verify that a stop or a message really comes from its launcher beyond the account. |
| Tampering | A peer message is delivered as labeled peer input. It is never treated as operator input and grants no tools. |
| Repudiation | No record beyond the recipient's stored conversation. |
| Information disclosure | Discovery exposes a root's name, address, and coarse status to every process under the account. Dependent children are never listed. |
| Denial of service | A local process can fill the root's pending bound. Later sends fail as recipient busy, and the operator can switch peer messaging off. |
| Elevation of privilege | A peer message can try to persuade the root's agent to act. The root acts under its own permissions, so the exposure is the root's tool set. An operator who needs isolation switches peer messaging off. |

Pig Litter doesn't defend against a hostile process running as the same user. The owner ruled that the operating-system account is the trust boundary.
