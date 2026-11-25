---
job: JOB-05
capabilities: [CAP-035, CAP-036, CAP-037, CAP-061]
boundaries: [BND-02, BND-03, BND-04, BND-08]
view: sequence
certainty: assumed
assumption: "The target design is unbuilt. Behavior verification through real interfaces has yet to exercise this flow."
---

# Follow and steer a child from the root's view

In interactive PiG, the operator finds a dependent child in the root's view, opens its conversation, follows it live, and steers it.

```mermaid
sequenceDiagram
    actor Op as Operator
    participant V as Operator view
    participant T as Tree coordinator
    participant H as Session host
    participant S as Steer delivery
    Op->>V: open the session list
    V->>T: read tree state
    V-->>Op: every dependent session with name, state, profile, and directory
    Op->>V: open a child
    H-->>V: the child's conversation so far and its live activity
    V-->>Op: child overlay
    Op->>V: type a steer
    V->>S: steer for the child, labeled as operator input
    S->>H: deliver after running tool calls, or start a turn
```

## Rules

- The operator can open any dependent session in the tree at any depth. Detached sessions that the root launched appear in a separate group with only their name, address, and coarse status.
- The overlay offers follow and steer. It states that pause, stop, and interrupting the child's turn aren't available from it, and it states any other interaction the released host can't support.
- The child's parent isn't told that the operator steered it.
- Conversation content reaches only the operator view. No agent operation reads it.

## Failure path

- **The steer would wake an inactive child in a tree at capacity.** The overlay shows the capacity error and the steer is discarded.
- **The child is fully stopped while its overlay is open.** The overlay shows the stopped state and refuses further steers.
- **The run has no terminal view.** The operator view doesn't exist, and orchestration works without it.
- **The released host can't give the overlay the full view.** The overlay uses the largest area the host allows and says so.
