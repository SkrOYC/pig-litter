# Foreground child delegation

## Sub-features

- Selecting Pig Litter exposes exactly `pig_litter_agent`.
- Scout and worker receive distinct prompts, exact models, and explicit file tool lists.
- Children start fresh ephemeral PiG sessions without the parent conversation.
- A bounded report and aggregate usage reach the parent.
- Missing models, provider errors, timeouts, and cancellation retain distinct outcomes.
- Headless launches return unavailable because pinned PiG cannot forward Exec cancellation in those modes.

## How to get to it

Select Pig Litter in interactive PiG. Ask the parent to delegate a self-contained task to scout or worker. Use Escape to cancel an active tool.

## Drive

Run `devenv shell -- bun scripts/verify-child-delegation.mjs`. The fixture drives real selected Go tools through tmux and a local OpenAI-compatible provider. It verifies a real read and a write followed by readback. It checks child prompts, models, tool lists, bounded handback, failures, timeout, and the child HTTP connection closing after Escape. It preserves requests and result evidence under `.pstack/evidence/child-delegation/`.

Run `devenv shell -- scripts/test-pig-litter.sh` for malformed JSON, missing terminal events, incomplete responses, invalid terminal ordering, usage, and input limits.

## Gotchas

The fixture has isolated PiG state and a local fixture key. It strips inherited provider credentials. Its parent explicitly trusts only the temporary test workspace. The extension never adds child trust approval. Tool lists and worktrees do not sandbox filesystem access. SDK Exec buffers output before Pig Litter parses it.
