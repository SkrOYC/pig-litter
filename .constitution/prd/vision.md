# Product vision

Pig Litter gives PiG sessions the primitives to delegate work and coordinate across local conversations. A lead can launch children, children can delegate further, and peers can exchange messages while each session keeps its own context.

## Reference workflow

A developer starts a wave in PiG. The wave lead delegates epics to sessions with separate working directories and role profiles. Epic leads can delegate to workers and reviewers, then handle their responses. A lead can also communicate with an externally launched Claude Code session. The orchestration skill owns planning, approval, review, and integration decisions.

## Product scope

Pig Litter targets unmodified PiG releases on macOS and Linux in interactive and headless modes. In the terminal user interface (TUI), the operator can open a child overlay to follow and steer the child. Pig Litter reports any interaction that the released host cannot support.

## Delivery approach

Organize delivery around complete user outcomes. A delegation slice, for example, lets a parent give a child a task, receive its response, and continue. The [jobs](jobs.yaml) define the outcomes that delivery must cover. The Tasks stage decides the boundaries and order of the slices. Product priority expresses importance across the intended scope.

## Appendix of operator preferences

- Use Claude Code as inspiration for behavior. Use original wording and design.
- Use Claude Code's local peer protocol for messaging. Live Claude Code verification on both platforms isn't a release gate. Describe known limits and report an unconfirmed delivery as such.
- Define concurrency and delegation depth settings in `piglet.yaml`.
- Use capabilities already available in unmodified PiG releases. State hard limitations. Host patches and workarounds for hard host limitations are outside scope.
- Initialize each constitution stage at `v0.0.1` and proceed in stage order.

See the [interview rulings](../reports/2026-11-25-interview-prd.md) for the source of these requirements.
