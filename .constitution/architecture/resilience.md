# Resilience

Pig Litter is tooling that runs inside an operator's terminal session. Its cross-cutting design follows the CLI defaults: clear error surfaces, defined behavior under partial failure, and stated limits. The [constraint meters](../prd/constraints.yaml) shape each section.

## Security and identity

- **Authority follows the delegation relationship.** Agent operations accepts a state query, a pause, a stop, or a wait only from the child's immediate parent, and a detached stop only from the launcher. A rejection reveals nothing about the target. This serves NFC-02.
- **Permissions only narrow inside a tree.** Settings and profiles resolves a dependent child's tools as the parent's permissions narrowed by the profile. The session host applies that set to the child's session, so the limit holds for every route to a tool.
- **A detached session isn't narrowed.** It takes its permissions from its own configuration. Only a root can launch one, which is the control the owner chose.
- **The operating-system account is the peer trust boundary.** Any process running as the same user can list a root and message it. A peer message is labeled as such, is never treated as operator input, and grants no tools. Pig Litter doesn't protect against a hostile process under the same account.
- **A dependent child has no outside surface.** It is absent from discovery and can neither send to nor receive from a session outside its tree.
- **Configuration can't be raised from below.** Settings and profiles come from the user level and the root's project. A child's working directory contributes nothing.

## Privacy

- Agent operations returns states, send outcomes, and wait statuses. It never returns another session's conversation, tool activity, or pending steers. This serves NFC-03.
- A session's content leaves it only as a message it sends or as its handback.
- Live conversation content flows from the session host to the operator view and nowhere else.
- Pig Litter keeps no event record. The tree record holds identities, relationships, and states, without conversation text.

## Failure handling

- **Reject before acting.** Every launch problem that can be checked up front is a rejection with its reason. Capacity, depth, and directory rejections use one wording each for launches and wakes.
- **No queues for capacity.** A launch or wake that doesn't fit is rejected at once. A message whose wake is rejected is discarded and the sender is told.
- **The sender owns retries.** A failed send is reported with its reason and never retried. A send for which only the write is known is reported as sent with delivery unconfirmed. This serves NFC-06.
- **Bounded backlog.** Each session holds a bounded number of pending steers. A send beyond the bound fails as recipient busy. Handbacks are exempt because nobody can resend them.
- **Bounded waits.** Every wait has a timeout no greater than the configured maximum and ends early on the selected child's message, handback, crash, or stop. This serves NFC-05.
- **A crash is visible.** A child cut off by a crash reports as interrupted, and its parent receives a notice when the root returns.
- **Limits are stated.** When the released host can't support an interaction, the session host states it at the point of use, and the result of the operation carries the statement.

## State integrity

- **One writer per tree.** The tree coordinator serializes every admission and state change, so two launches or wakes can't both take the last slot. This serves NFC-04.
- **A slot is freed only on confirmation.** A session's slot is released when the session host confirms its turn has ended, not when a pause or stop is requested.
- **Record before answering.** The tree coordinator writes a new child or a detached launch to the tree record before the launch call returns, so an admitted session can't be lost to a crash that follows the answer.
- **Restore is conservative.** A session recorded as active is restored as interrupted after a crash and as paused after an orderly end. Nothing resumes by itself.
- **Pending steers aren't durable.** A steer accepted for an active session and not yet delivered is lost if the process dies. Handbacks that were not yet delivered are also lost, and the parent learns of the child through the recovery notice.

## Telemetry

Pig Litter keeps no event record of its own. The operator observes a tree through the session list and the child overlay, and an agent observes through the results of its own operations. Verification relies on those results and on the host's stored conversations.

## Configuration

- A root reads its settings and profiles once, when it starts or is reopened. The values hold for the whole tree until the root ends.
- A detached session reads its own configuration from its own working directory as any root does.
- An invalid settings or profile file stops the affected operation with the reason. Pig Litter doesn't fall back to defaults for a file that exists and can't be read.
