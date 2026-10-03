---
name: swarm
description: "Fan out N parallel workers, drain them, and return one report. Use for $swarm, 'swarm this', or parallel coverage, races, gauntlets, and exploration."
---

# Swarm

Fan out N parallel workers. They may cover separate slices, race the same brief, or mix both. The parent waits, aggregates, and returns one report. In Codex CLI, workers use the native collaboration lifecycle and share the local checkout; this collaboration interface has no per-child cloud environment selector. Codex Cloud task submission is a separate native CLI operation, not a field on this launch.

## Start

Open a native plan with one entry per phase before launching anything.

1. Frame
2. Fan out
3. Aggregate
4. Report

## Phase A: Frame

1. State the done predicate and the artifact or report the swarm must return.
2. Choose the shape. Partition into slices, race N workers on identical briefs, or mix both. For a race or mixed shape, declare `first pass`, `rank all`, or `best-of` before spawning.
3. Set N from the user or derive it from the shape. N is total workers, not the configured concurrency limit. If runtime capacity is smaller than N, launch in batches while retaining every task and result.
4. Pick the worker model and effort by running `bun .agents/scripts/pstack-models.mjs resolve "swarm workers"`. If the role or that line is missing, use the translated default `gpt-6-luna` with `xhigh`. For `auto` or `inherit-parent`, resolve the active parent pair with `bun .agents/scripts/pstack-models.mjs parent` and pass both settings explicitly. Pass an explicit model and effort separately with `fork_turns: "none"`, and include the task context in each brief. If the spawn operation rejects a model, omit the override and use the configured/default model, then report that fallback. If the default is rejected, use the closest supported model from the same model family and a supported effort reported by the runtime; if none is available, report the launch failure. For a model race, name each arm's model and effort up front.
5. Give each worker its own writable output when it writes. Native workers share the checkout, so keep writable paths disjoint. When workers verify or measure commits, each brief names the exact SHAs. A measurement brief also names the method (sample count, what one sample is, order). The worker records both in its result.

## Phase B: Fan out

Launch all N workers through `collaboration.spawn_agent`, using one task name and standalone delegation message per worker. `generalPurpose` maps to Codex's `default` agent; include the requested role instructions in the message if the runtime cannot select the registered agent directly. Pass the step 4 model and effort only when explicit, with `fork_turns: "none"`. The native launch runs in the local session and makes the child available in the background; use `collaboration.send_message` to steer a running child, `collaboration.list_agents` to inspect it, `collaboration.interrupt_agent` to interrupt it, and `collaboration.wait_agent` to wait for notifications and collect completion results. Codex CLI has no `environment: "cloud"` or `environment: "local"` field: workers run against this session's local runtime and permissions.

The native launch has no `cloud_base_branch` argument. If workers must start from a non-default pushed branch, ensure the shared checkout is already on that branch before launch; if the current collaboration runtime cannot supply the required checkout, record a launch gap instead of claiming the workers used that base.

Every brief stands alone. Include the goal, scope, exact slice or race arm, how to verify, and what to report. Reports use `PASS`, `ISSUES`, or `BLOCKED` with evidence. A worker that can prove a defect reports `ISSUES` and lists every issue it can prove, not only the first.

If a worker drops out, inspect its state with `collaboration.list_agents`; send a follow-up with `collaboration.followup_task` only when resuming its retained work is appropriate. Otherwise proceed with N-1 and note it. Use `collaboration.interrupt_agent` when the parent decides to stop an active worker, and record the interruption in the report.

## Phase C: Aggregate

Wait for all workers with `collaboration.wait_agent` and read their completion results. Drop a result that does not record the SHAs and method its brief names, and rerun that worker once using `collaboration.followup_task` on the same child. After a second miss, record a gap. A gap does not count as a pass. For coverage, every required slice needs a result. For a race, apply the selection rule declared up front. Use first pass, rank all, or best-of. Do not paste raw worker dumps.

Keep a compact result table, one-line evidenced issues, and explicit gaps or dropouts.

## Phase D: Report

Return one consolidated in-chat report with the table, issue one-liners, gaps or dropouts, and the race rule when used.
