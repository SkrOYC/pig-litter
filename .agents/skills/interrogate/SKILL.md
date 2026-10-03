---
name: interrogate
description: 'Use for "interrogate", "adversarial review", "multi-model review", "challenge this", "stress test this code", "find blind spots", or "tear this apart". Multiple LLM reviewers challenge changes from independent angles.'
---

# Interrogate

Spawn one reviewer per configured model to adversarially review code changes. Each model gets the same prompt and rubric. The adversarial signal comes from model diversity, not assigned personas.

The deliverable is a synthesized verdict. Do NOT auto-apply changes.

## Step 1, Determine Scope

Identify what to review from context:

- If the user points at specific files or a diff, use that
- If on a feature branch, run `git diff main...HEAD` (or the appropriate base branch) for the full changeset
- If the user's message references recent work, gather the relevant files

Package the diff (or file contents) plus any surrounding context files the reviewers need to understand the code.

## Step 2, State the Intent

Before spawning reviewers, state the intent explicitly. Derive this from:

- The user's message
- Commit messages
- PR description if one exists
- The code itself

Write one clear paragraph. If you're unsure about the intent, ask the user before proceeding.

## Step 3, Spawn Reviewers

Use `bun .agents/scripts/pstack-models.mjs resolve "interrogate reviewers"` to read the configured model and reasoning effort for each seat, one reviewer per entry, extending or shrinking the Reviewer A/B/C labels below to the configured entry count. If the role or its line is missing, use the table defaults. Launch all reviewers in parallel. Use `collaboration.spawn_agent` when it can select a native reviewer role configured with `sandbox_mode = "read-only"`. Otherwise, run one `bun .agents/scripts/pstack-readonly.mjs` session per reviewer, with the same prompt file and separate output files. Pass `--cwd` for the reviewed workspace, the seat's `--model` and `--effort`. For an `auto` or `inherit-parent` entry, pass the active parent's effective model and effort explicitly. The adapter enforces read-only access and returns the final report; its event log is saved beside the output. If neither a configured read-only role nor the adapter is available, report that the required reviewer launch is unavailable.

| Subagent   | Default model          |
| ---------- | ---------------------- |
| Reviewer A | `gpt-6-astra` / `max`  |
| Reviewer B | `gpt-6.1-sol` / `max`  |
| Reviewer C | `gpt-6-luna` / `xhigh` |

For native launches, use `spawn_agent` with a distinct task name and the original reviewer prompt. Pass the seat's model and reasoning effort separately when specified; for an `auto` or `inherit-parent` entry, resolve the active parent pair with `bun .agents/scripts/pstack-models.mjs parent` and pass both settings explicitly. When passing overrides, use `fork_turns: "none"` and include the review prompt and necessary context in the task. For adapter launches, save the filled reviewer prompt to a file and start a separate managed shell session for each seat. Use distinct output paths and collect each session's returned final report before synthesis.

If the native runtime rejects a configured model, run that reviewer on the table default from the same model family when available and say so. If that model is unavailable, check the valid models in the runtime's error message, pick the closest available model from that line (prefer the highest supported reasoning effort), spawn with it, and open a separate PR to update the default table. Do not block the review on the model issue. Never treat an `auto` or `inherit-parent` entry as a rejected model or apply either fallback to it. For these Codex defaults, compare `gpt-6-astra`, `gpt-6.1-sol`, and `gpt-6-luna` as distinct model lines; do not assume cross-provider availability.

Read `references/reviewer-prompt.md` and fill in the template with:

1. The stated intent
2. The diff or file contents
3. The review rubric from `references/rubric.md`
4. The code-quality lens from `references/code-quality-review.md`

The same filled template goes to all reviewers, so every model applies the code-quality lens.

## Step 4, Synthesize

As results come back, build a unified picture:

1. **Parse all findings** from the reviewers
2. **Identify consensus**. Findings raised by 2+ models independently are highest signal.
3. **Identify lone-model findings**. Still worth reading, but weight accordingly.
4. **Deduplicate**. Different models may describe the same issue differently. Merge these and note which models raised it.
5. **Note disagreements**. If one model flags something and another explicitly says the opposite, that's useful context for the verdict.

## Step 5, Lead Judgment

You are the lead reviewer, a pragmatic senior engineer, not a neutral aggregator.

Read `references/lead-judgment.md` for the full framework.

Categorize every finding using these buckets:

- **Act on**. Real issues affecting correctness, security, or maintainability given the actual goals. These would block a real PR.
- **Consider**. Legitimate points, but you're not sure they outweigh the cost of addressing them right now. Worth the user's attention.
- **Noted**. Technically valid but not actionable. Context-dependent, premature optimization, or low-impact given the current stage.
- **Dismissed**. Wrong, nitpicky, or missing context. Brief explanation why.

For each finding, include:

- Which model(s) raised it
- The category (act on / consider / noted / dismissed)
- A one-line rationale for the categorization

## Output Format

Present the verdict in this structure:

### Intent

> [The stated intent paragraph from Step 2]

### Reviewers

- Reviewer [label]: [model name], [N findings] (one bullet per reviewer)

### Act On

[Findings that should be addressed. For each: description, which models raised it, why it matters.]

### Consider

[Findings worth thinking about. For each: description, which models raised it, tradeoff involved.]

### Noted

[Valid but low-priority. Brief list.]

### Dismissed

[Rejected findings with brief rationale.]

### Agreement Map

[Where did models agree, where did they diverge, and what does the pattern of agreement/disagreement tell us?]
