# Product requirements interview

This full interview establishes the first product requirements for an existing codebase. The interview record is dated November 25, 2026. The owner asked to initialize the constitution from the interview and proceed in stage order. Each stage starts at `v0.0.1`.

## Product and release scope

- Pig Litter supplies general-purpose delegation and coordination primitives for PiG sessions. The `execute-wave` skill is the practical example of their depth and breadth. The skill owns wave planning, approvals, review, and merge rules.
- Claude Code is the main inspiration. Pig Litter uses original wording and design. The owner supplied local collaboration research to clarify observable behavior.
- The agreed primitives are all in product scope. The owner revised the earlier single-release decision and asked for development in vertical slices. The PRD preserves the intended outcomes. The Tasks stage defines and sequences the increments that deliver them.
- Shared task tracking could help but isn't required. A separate observer role and conversation forks are outside scope. Approval modes are a separate area.
- The lead launches Claude Code outside Pig Litter. A Claude Code session can participate as a lead or peer through messaging. Pig Litter manages PiG children.

## Delegation and permissions

- Start each child asynchronously with a fresh conversation and an explicit brief. The parent can choose its name, working directory, provider and model, and reasoning effort. The working directory can be chosen freely.
- A launch can select a custom agent profile. Profiles can define instructions, tools, and a default model. Scout and worker are example profiles.
- Permissions narrow down the tree. A child can call only tools permitted by its parent and by its profile, if selected.
- Configure concurrency and delegation depth in `piglet.yaml`. Count active sessions across each root tree, including the root and agents blocked in a wait. Paused and finished sessions don't count. Reject launches over either limit and reject wakeups over capacity with the same capacity error as a launch.
- Concurrent write-capable descendants can use sibling directories. Reject writes being delegated to the same directory or to directories where one contains the other. Read-only descendants and the root are exempt from this coordination rule.

## Peers and messages

- PiG sessions can list and message local PiG peers, including independently started sessions. PiG and Claude Code sessions can discover and message one another through Claude Code's local peer protocol. Discoverable sessions under the same operating-system account can participate by default.
- Peer discovery shows coarse busy or idle status when available, as in Claude Code. The immediate parent can check its child's active state. Agent-facing tools expose no other session's traces or transcript.
- A peer message is a steer. On a running session, it takes effect after the running tool call, before the next model step that can take the message. It wakes a reachable inactive session. Pig Litter labels peer messages as peer messages so the recipient can distinguish their origin from human input.
- Report known delivery failures and let the sender decide whether to retry. A successful Claude Code socket write can return sent with delivery unconfirmed. Use protocol findings to improve the result when the recipient supplies stronger status. Pig Litter requests steering and states known limitations rather than silently substituting a follow-up turn.

## Lifecycle and handbacks

- Only the immediate parent agent can pause or stop its child. Pausing stops the child's current work and leaves its children running. A message from any peer can wake the paused child in the same session.
- Before a stop that would also stop descendants, warn the immediate parent about the cascade. The parent can then choose to proceed. Fully stopped sessions are unavailable to peers.
- A finished child stays reachable and can continue in the same session. Pig Litter doesn't stop a child automatically at the end of a turn.
- When a child ends a turn with a final response and has no active children, deliver the response and outcome automatically to its immediate parent. Suppress the automatic handback while the finishing session has active children. Explicit messages remain available.
- Deliver the handback as a steer labeled with the child as the sender. Wake an idle parent as a human message would. Keep the same message delivery mechanism whether the parent waits explicitly or ends its turn.
- An agent can wait for its own child and must specify a timeout. End the wait on an explicit message from the selected child to that parent, on its eligible final response, on its crash or full stop, or on timeout. In the two message cases, the tool returns a short status and the actual content arrives through the steer. Other peers' steers stay pending until the wait returns. Report a child crash or full stop immediately.
- Live work can stop when the parent process dies. Report interrupted children when the parent returns. Ending the root can also stop the whole tree for the human.

## Human interaction and compatibility

- In interactive PiG, each managed child has an openable full-view overlay inside the parent's terminal interface. It provides a live transcript and human steering in a view that feels like the parent view. State any unsupported interactions explicitly.
- The human observes and steers through the overlay. Agents use the launch, pause, and stop tools. The human can still end the root.
- Headless PiG can orchestrate without rendering a child view. Pig Litter targets macOS and Linux.
- Use unmodified, released PiG and capabilities available in it. The product cannot require a private host patch, an upstream change, or a workaround for a hard host limitation. Document hard limitations.
- Reuse findings about the local Claude Code protocol without naming the reference implementation in Pig Litter. Live Claude Code verification on both platforms is not a release gate.

## Evidence checked

- Repository baseline `ff4de97a537620a7b7459c061c7e04bcc03f96ab`. The README describes a wider surface than this release scope. The checkout compiles against a patched preview in `devenv.nix`. Isolated stock PiG 0.7.0 builds reproduced missing SDK and native input types.
- [PiG 0.7.0 release](https://github.com/MichaelKinsy/PiG/releases/tag/v0.7.0), [child-session SDK](https://github.com/MichaelKinsy/PiG/blob/v0.7.0/extensions/sdk/child_session.go#L388), and [focused extension UI](https://pi-in-go.dev/docs/latest/extensions/#focused-ui), checked November 25, 2026. The stock interfaces support a focused overlay and live child data. The evidence doesn't establish full native interaction parity.
- [PiG 0.7.0 RPC client](https://pkg.go.dev/github.com/MichaelKinsy/PiG@v0.7.0/coding/rpcclient#RpcClient.Start) and [RPC shutdown source](https://github.com/MichaelKinsy/PiG/blob/v0.7.0/coding/cli/rpc_mode.go#L216), checked November 25, 2026. Standard input closure ends the RPC process even though it runs separately. The extension SDK child belongs to the host connection.
- [Claude Code subagent documentation](https://code.claude.com/docs/en/sub-agents) and [agent teams documentation](https://code.claude.com/docs/en/agent-teams), checked November 25, 2026. The references informed peer discovery, explicit messaging, and automatic completion behavior.
- Owner-supplied Claude Code 2.1.322 local research on discovery and cross-session transport, checked November 25, 2026. It supports a request for immediate priority and some receiver status events. It provides no general acknowledgement of ordinary message consumption. Linux two-way messaging has prior live evidence. Equivalent busy-turn behavior and macOS interoperability haven't been measured here.

## Handoff

Use these rulings to initialize the product requirements. The next stage is Architecture in brownfield mode. Its inputs are the validated product requirements and this record. Resolve implementation choices in their owning stages. Report any stock-host behavior that prevents a requirement from being met. No product implementation or wave execution was authorized by this interview.
