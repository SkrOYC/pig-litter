# Foreground child delegation

## Behavior

Selecting Pig Litter exposes `pig_litter_agent`. Scout has `read` and `ls`; worker also has `write` and `edit`. Each gets a separate prompt and a fresh ephemeral child session without the parent conversation. The optional exact model defaults to the parent's current model.

A bounded report and aggregate usage reach the parent. Completed, failed, partial, unavailable, rejected, and stopped outcomes have different meanings. Timeout and cancellation both return `stopped`. The product rejects headless delegation; do not infer a newer host's cancellation capabilities from that guard.

## Drive with a real provider

Run the executable live helper with the actual Piglet, configuration root, and optional exact model as runtime inputs.

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs \
  --piglet pig-litter --model "$PIG_LITTER_MODEL" --case selected
```

The helper retains normal authentication and uses `--no-builtin-tools` for the parent. Scout reads an opaque token absent from the parent prompt. Worker writes another token and reports it after readback. Parent session records must contain completed `pig_litter_agent` results with the inherited exact model. Live child argv and process receipts establish actual child launches. The helper checks the file content and retains the parent session evidence.

An optional `--case cancel` records a live child and tool call before Escape, that child's exit, and the parent's cancellation acknowledgement. It requires a new diagnostic notification to establish responsiveness after the action. Cancellation can produce plain aborted tool error text instead of a structured handback. Keep the real-provider observation separate from controlled fixture outcomes.

## Drive deterministic controls

Run `devenv shell -- bun scripts/verify-child-delegation.mjs` for the loopback provider fixture. It supplies isolated synthetic model configuration and selects the repository extension directly. It verifies prompts, exact models and tools, usage, bounded handback, missing models, provider error, timeout, parallel rejection, headless rejection, and controlled cancellation cleanup.

Run `devenv shell -- scripts/test-pig-litter.sh` for malformed events, missing or incomplete terminal evidence, ordering, bounds, and literal task handling.

## Limits

The live run inherits PiG configuration and credentials without copying their files. Model and config values are runtime inputs. Parent scratch trust does not bypass child trust. Tool lists and worktrees do not sandbox filesystem access. SDK Exec buffers output before Pig Litter parses it. Neither the fixture nor an offline selection run proves real provider readiness or named user Piglet resolution.
