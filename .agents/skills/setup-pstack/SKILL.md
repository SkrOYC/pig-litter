---
name: setup-pstack
description: Configure which models pstack uses per role and at what reasoning budget. Detects available models and writes native Codex agent settings that override the skill defaults. Use for $setup-pstack, "configure pstack models", "pstack budget", or changing pstack's model choices.
---

# Setup pstack

Write native Codex custom-agent settings under `.codex/agents/pstack-*.toml` for project scope or `~/.codex/agents/pstack-*.toml` for user scope. Each role override is a standalone agent definition; an omitted role falls back to the translated source defaults. Project settings take precedence over user settings for the same role.

## Steps

### 1. Detect available models

Run `bun .agents/scripts/pstack-models.mjs models` to read the current Codex model catalog and its supported reasoning efforts. Use a directly exposed model-selection interface as additional runtime evidence when available. The cache shows the catalog Codex knows about; do not claim it proves account entitlement when the active runtime cannot confirm that. If no model can be verified, ask the user for a choice with verifiable model evidence. Never write a real model/effort pair you have not confirmed is available. The aliases `inherit-parent` and `auto` are always valid choices for the active parent's exact model and effort; resolve that pair with `bun .agents/scripts/pstack-models.mjs parent` before delegation and pass both values explicitly with `fork_turns: "none"` so a configured subagent default cannot replace them.

### 2. Load current state

Run `bun .agents/scripts/pstack-models.mjs show` to read the effective role choices, including project overrides, user overrides, translated fallback defaults, and `retiredFiles`. Use `bun .agents/scripts/pstack-models.mjs resolve "ROLE LABEL"` to inspect one role. The helper records the budget in a TOML comment and the role choices in native custom-agent TOML. Start from the fallback defaults when no override exists. A role that is not in step 3's role list, such as `how critics`, is retired; list it among the choices to drop.

### 3. Budget, map, and confirm

**(a) Ask for a budget.** Prefer the native Codex question interface over free text. Offer these four options with these exact labels, and name the current budget when the native settings record one. If different role files record different budgets, show that and ask which budget to apply to the complete role table.

- `unlimited — keep max`
- `large — xhigh reasoning`
- `medium — high reasoning`
- `small — medium reasoning`

**(b) Apply it.** Build the working table from the fallback defaults, and on a re-run keep any role you changed by model, panel list, or alias (`inherit-parent`, `auto`). `unlimited` leaves every effort as in that table. `large`, `medium`, and `small` set each real model choice, including panel entries, to the highest effort that model supports at or below `xhigh`, `high`, or `medium`. Use the model catalog's supported reasoning levels; do not infer model IDs by changing suffixes. If no supported effort is at or below the target, mark the role as needing a choice. `inherit-parent` and `auto` do not change. For example, `small` maps `gpt-6-luna` to `medium`; if `gpt-6-astra` does not support `xhigh`, `large` uses its highest supported effort below `xhigh`.

**(c) Show the roles and confirm.** Show every role with its model and effort, marking any real model/effort pair not in the verified set as needing a choice. Also list each retired role dropped in step 2. Ask whether to accept as-is or change specific roles, offering the verified model/effort pairs plus `inherit-parent` and `auto` as the options; both aliases mean use the active parent's current model and effort. Ask whether the settings should be project-scoped (`.codex/agents/`) or user-scoped (`~/.codex/agents/`); explain that project settings apply in this project and user settings apply across projects unless overridden. Prefer the active Codex question interface; if none is available, ask in the conversation. For panel roles (arena runners, architect runners, interrogate reviewers) the value is a list, and one subagent runs per entry, alias entries included, so the list length sets the count. `arena cross-judge pool` is also a list, but Arena selects one value from it whose model ID differs from the parent's when possible. `swarm workers` is the default model for every worker unless a race or comparison assigns another model per arm.

### 4. Validate

Every model/effort pair written must be in the current model catalog or supported by explicit runtime-verified evidence. For a pair absent from the catalog, include it as `{ "model": "MODEL", "effort": "EFFORT", "evidence": "..." }` in the input's `verifiedPairs` array. `inherit-parent` and `auto` always pass. If a chosen pair is not available, stop and ask again. `bun .agents/scripts/pstack-models.mjs parent` reads only the active thread's model and effort from `threads` in the Codex state database identified by `CODEX_THREAD_ID`; when the effort is omitted, it uses a cached model default only if that effort is listed as supported. If the thread ID or required fields are unavailable, stop and ask the caller for a runtime-known pair rather than guessing. Pass alias-resolved parent pairs explicitly with `fork_turns: "none"` when delegating. `bun .agents/scripts/pstack-models.mjs write --scope project|user --input @PATH` validates the full proposal before writing; use `null` for a role to remove only that role's override and return to its fallback. Include in `retiredFiles` only the retired basenames listed for the selected scope that the user agreed to drop. Do not run `write` before the user accepts the displayed proposal.

### 5. Write the rule

Write one native custom-agent TOML file per role and one numbered file per panel seat, using the same human role labels poteto-mode uses. Include every role shown in step 3(c) so the selected budget applies to the whole table. Each generated file has the required `name`, `description`, and `developer_instructions`, plus `model` and `model_reasoning_effort` for a real model choice. Alias choices omit the model fields; the invoking workflow resolves the active parent pair and passes it explicitly to the child. The helper stores the selected budget in a TOML comment, rewrites only roles present in the accepted input, retains omitted overrides, and removes only obsolete numbered seats owned by the panel being updated. Keep unrelated agent files. Include only retired basenames from the selected scope that the user agreed to drop. Native Codex custom-agent files do not use `alwaysApply` front matter. Example shape:

```
name = "pstack-bug-fix"
description = "Pstack bug-fix model role."
developer_instructions = "Pstack role: bug-fix. Follow the role instructions supplied by the invoking workflow."
model = "gpt-6-luna"
model_reasoning_effort = "xhigh"
```

Panel files add a two-digit seat suffix to `name` and the filename, for example `pstack-arena-runners-01.toml`. The helper reads the scope's effective choices with `show` and writes the accepted choices with `write`.

### 6. Confirm

Tell the user which scope was written and that Codex loads the custom-agent settings in new sessions. Re-running this skill updates only the roles the accepted input includes.

### 7. Offer a verification skill (optional)

Check whether the project has a way to drive the real app for proof (a `verify-*` skill, or an existing harness). If not, offer once: "want a project-local verification skill, so agents can drive the app the way a user does and prove changes work? I can generate one with $create-verification-skill." On yes, invoke `$create-verification-skill` (resolves wherever pstack is installed: workspace, user, or plugin). On no, move on without pushing.
