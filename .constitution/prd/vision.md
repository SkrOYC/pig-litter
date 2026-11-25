# Product vision

Pig Litter gives PiG sessions the primitives to delegate work and coordinate across local conversations. A session can launch dependent children inside its own tree, a root can launch detached sessions that run independently, and roots can exchange messages with other local roots and Claude Code sessions. Each session keeps its own context.

## Reference workflow

A developer starts a wave in PiG. The wave lead is a root. It launches each epic lead as a detached session with its own working directory and agent profile. Each epic lead heads its own tree, delegates to workers and reviewers as dependent children, and handles their handbacks. Epic leads report to the wave lead by peer message. A wave lead can also be an externally launched Claude Code session that messages the epic leads. The orchestration skill owns planning, approval, review, and integration decisions.

## Product scope

Pig Litter targets unmodified PiG releases on macOS and Linux in interactive and headless modes. In the terminal user interface (TUI), the operator can open a child overlay to follow and steer any dependent child in the root's tree. Where a terminal pane host is available, a detached session opens in its own pane. Pig Litter reports any interaction that the released host cannot support.

## Delivery approach

Organize delivery around complete user outcomes. A delegation slice, for example, lets a parent give a child a task, receive its response, and continue. The [jobs](jobs.yaml) define the outcomes that delivery must cover. The Tasks stage decides the boundaries and order of the slices. Product priority expresses importance across the intended scope.

## Appendix of operator preferences

- Use Claude Code as inspiration for behavior. Use original wording and design.
- Use Claude Code's local peer protocol for all peer messaging, including messaging between PiG roots. Live Claude Code verification on both platforms isn't a release gate. Describe known limits and report an unconfirmed delivery as such.
- Define Pig Litter's settings in `litter.yaml` at the user level and at the project level. Define each agent profile in its own file beside it.
- Use tmux and iTerm2 as the terminal pane hosts.
- Use capabilities already available in unmodified PiG releases. State hard limitations. Host patches and workarounds for hard host limitations are outside scope.
- Initialize each constitution stage at `v0.0.1` and proceed in stage order.

See the [product interview](../reports/2026-11-25-interview-prd.md) and the [architecture interview](../reports/2026-11-25-interview-architecture.md) for the source of these requirements.
