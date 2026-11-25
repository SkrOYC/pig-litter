---
job: JOB-01
capabilities: [CAP-001, CAP-002, CAP-003, CAP-004, CAP-005, CAP-006, CAP-007, CAP-020, CAP-029, CAP-030, CAP-054, CAP-055, CAP-056]
boundaries: [BND-01, BND-02, BND-03, BND-04, BND-05, BND-09]
view: sequence
certainty: assumed
assumption: "The target design is unbuilt. Behavior verification through real interfaces has yet to exercise this flow."
---

# Delegate work to a dependent child

A parent gives a fresh dependent child a bounded assignment, keeps working, and continues with the child's response.

```mermaid
sequenceDiagram
    participant P as Parent agent
    participant O as Agent operations
    participant T as Tree coordinator
    participant C as Settings and profiles
    participant H as Session host
    participant S as Steer delivery
    P->>O: launch(brief, name, directory, model, effort, profile)
    O->>T: admit launch
    T->>C: resolve tools and writer declaration
    T->>T: check slots, depth, name, directory, permissions
    T-->>O: admitted, or rejected with the reason
    O-->>P: launch status
    T->>H: create child and start its first turn
    T->>T: record the child in the tree record
    H-->>T: turn ended with a final response or a failure
    T->>S: handback when the child has no active children
    S->>H: deliver the labeled handback to the parent
    H-->>P: handback after running tool calls, or as a new turn
```

## Rules

- The launch call answers before the child does any work. Every problem that can be checked before the child exists is a rejection of the call.
- The child's tools are the parent's permissions narrowed by the profile. The profile declares whether the role writes.
- The parent already holds a slot while its child is active, so the handback wakes an idle or paused parent without a new admission.
- A handback is delivered whole and carries the child as its origin.

## Failure path

- **The launch is rejected.** The parent gets the reason in the launch status and no child exists.
- **The child's first turn can't start or its turn fails.** The tree coordinator marks the child finished and the parent receives a failure handback. The child stays reachable, so the parent can message it to try again.
- **The child finishes while it has active children.** No handback is sent. The child's own children wake it later, and the handback follows the turn in which it has none active.
- **The root's process ends before the handback.** The tree record restores the child as interrupted, and the parent receives the recovery notice described in the halt and recovery flow.
