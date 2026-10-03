---
name: how
description: 'Use for "how does X work", code walkthroughs before changing something, and placement / ownership / layering questions ("where should this live", "which package owns this", "is this the right layer"). Explains subsystem architecture, runtime flow, onboarding mental models. Use why for motivation.'
---

# How

Explore the codebase to answer "how does X work?" questions. Produce architectural explanations at the level of a senior engineer onboarding onto a subsystem, enough to build a working mental model, not so much that it reads like annotated source code.

Each run below names a role line in the native pstack agent settings and a default. Resolve it with `bun .agents/scripts/pstack-models.mjs resolve "ROLE LABEL"`. Use that line's model and effort, or the role's default pair if the setting or line is missing. When the value is `auto` or `inherit-parent`, pass the active parent's effective model and effort because a standalone CLI session does not inherit UI overrides. If the selected model is rejected, retry with the default pair and say so. If the default model is rejected, use the closest valid model from its error message and retain the effort when supported. Run each read-only phase with `bun .agents/scripts/pstack-readonly.mjs --cwd WORKSPACE --prompt PROMPT_FILE --output REPORT_FILE --model MODEL --effort EFFORT`. Use one managed shell session per run, retain each session handle for collection or cancellation, and read its unchanged final report from `REPORT_FILE`; detailed events are written beside it. Do not add `--ephemeral`. See `.agents/pstack-runtime.md` for adapter behavior.

## Step 1. Assess Complexity

If the scope is ambiguous, state your interpretation and explore. The user can redirect.

- **Simple** (a single module, a small utility, a narrow question such as "how does function X work"): no explorers. One explainer explores and explains in a single pass. Go to Step 2b.
- **Complex** (a subsystem spanning multiple files or services, a cross-cutting feature, a full architectural overview): spawn parallel explorers first, then hand off to the explainer. Go to Step 2a.

When in doubt, take the simple path.

## Step 2a. Explore (complex questions only)

Decompose the question into 2 to 4 exploration angles, each a distinct slice of the subsystem. Launch all explorers before waiting for any of them:

- Build a separate prompt file from `references/explorer-prompt.md` for each angle. Start a separate managed shell session running the read-only adapter for each explorer.
- Resolve the `how explorer` role; its default is `gpt-6-luna` with `xhigh` reasoning effort.

Retain each shell session handle and wait for all explorers to finish. Read their final reports and use every explorer's findings in Step 3.

## Step 2b. Direct Explain (simple questions)

Run one read-only adapter session that explores and explains in one pass:

- Build its prompt from `references/explainer-prompt.md` without the explorer-findings section, then launch it in a managed shell session.
- Resolve the `how explainer` role; its default is `gpt-6-astra` with `max` reasoning effort.

Wait for the session to finish and collect its final report. Go to Step 4.

## Step 3. Synthesize (complex questions only)

Once all explorers have returned, run one read-only adapter session to synthesize their findings into one explanation:

- Build its prompt from `references/explainer-prompt.md` with every explorer's findings filled in, then launch it in a managed shell session.
- Resolve the `how explainer` role; its default is `gpt-6-astra` with `max` reasoning effort.

Wait for the session to finish and collect its final report.

## Step 4. Present

Present the explainer's output to the user. Light edits for clarity or context from the conversation are fine. Do not substantially rewrite it.

## Output Format

The explanation uses the sections defined in `references/explainer-prompt.md`, dropping any that do not apply: Overview, Key Concepts, How It Works, Where Things Live, Gotchas.
