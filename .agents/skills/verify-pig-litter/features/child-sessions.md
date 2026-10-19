# Child sessions

Selecting Pig Litter registers `litter_spawn`, `litter_list`, `litter_inspect`, `litter_message`, `litter_stop`, and `litter_wait`. Children run in separate persisted PiG Go SDK sessions inside the extension process.

## Behavior

Use `litter_list` to discover configured agents and active direct children. Paused and settled children remain inspectable by their immediate parent. Root is named `root` and follows the same ownership rule. Sibling, ancestor, and grandchild UUIDs cannot bypass it.

Bundled scout has `read` and `ls` and cannot delegate. Worker also has `write`, `edit`, `bash`, `skill:*`, `mcp__*`, `codemode`, and `tool_search`. The default `~/.config/pig/agent/litter.yaml` supplies settings across projects, followed by any legacy `~/.pig/agent/litter.yaml`. Explicit PiG configuration roots and agent-directory overrides select their own user settings instead. Project `litter.yaml` at the child's spawn directory overrides user settings. Either file can override scout or worker by name, or add agents.

Role tool lists accept explicit names and globs with a literal name prefix. Root enumeration includes inactive and deferred tools and excludes hidden tools. It refreshes on admission and each spawn or resume execution. Child tools cannot exceed that inventory, and nested delegation narrows the immediate parent's cumulative ceiling. MCP configuration comes from the root, regardless of child directory. Choose child tools in `litter.yaml`; directory `defaultTools` does not reduce admitted built-in tools. Litter adds no permission prompts or project-trust denials beyond its tool policy.

Native MCP connections start in the background. The child receives a notice naming allowed tools that remain pending after the startup wait. Those tools can become available during the run. Child runtime shutdown closes its MCP transports.

Spawn mints and returns a UUID before constructing the child session. The UUID remains stable through turns, pause, and explicit resume. Its optional `cwd` selects the child's working directory, relative to the caller when necessary. Native PiG file and terminal tools, settings, instructions, skills, and prompt resources use that directory. File arguments and terminal commands reach those tools unchanged. The child inherits the caller's exact model unless an allowed explicit model or fixed agent model overrides it. There is no model fallback.

PiG owns each child's conversation log. Litter stores ownership, names, admitted policy, latest outcomes, and pending completion receipts in the root's native log. Reopening the same persisted root restores that metadata. Interrupted executions recover paused and require explicit resume. An ephemeral root cannot restore ownership after the parent process exits. Different roots cannot control those children. Accepted continuation instructions remain pending until native storage succeeds. Before Litter records preparation of the first input, explicit resume can create the native session under the admitted UUID. After that preparation, missing or unreadable native history returns `unavailable`. Litter opens child conversations by UUID, never by a session-file path.

Wait by UUID. A wait timeout leaves the child running. Cancelling only the wait affects that wait. Pausing a parent pauses its children too. Child runs have no wall-clock deadline. `litter_inspect` returns current status and the latest result without transcripts, evidence receipts, or mailbox pages. `litter_message` can steer a live child or explicitly resume a paused or settled child with `resume:true`. Resume retains the conversation and UUID. `litter_stop` pauses the owned subtree and waits for its active work to stop. It returns the actual outcome, including a completed or failed outcome that preceded the stop. Nothing resumes automatically; a resumed parent must explicitly resume each paused child. Unknown UUIDs return `unavailable`.

Result envelopes include `operation` and `handbackTruncated`. The latest report has its own `truncated` flag. `max_result_bytes` defaults to 50,000 and retains its 1 MiB upper cap. `max_turns` defaults to 50 and accepts positive integers beyond 128. Reaching the allowance queues one `<system-notice>` asking for a final partial report. It never aborts the child. The outcome is `partial` when the child settles, and explicit resume grants a fresh allowance. A child that ignores the notice can continue until stopped.

Depth and concurrency remain configurable. `max_mailbox` bounds pending delivery. `max_records` and `max_history_bytes` are removed; configurations that contain them must delete those keys. Native history size does not disable resume.

A successful spawn survives a later enclosing codemode script failure. Spawn followed by wait in one script works. Cancellation before the individual admission succeeds rolls back that pending admission.

The `Litter` widget prioritizes active direct children and then recent settled children. It shows state counts and refreshes with lifecycle and width changes. Discovery can draw the initial counts, and restored ownership displays retained children. The `/pig-litter` panel retains direct children for inspection, messages, subtree pause, and explicit same-UUID resume. Opening or closing it starts no inference and consumes no completion receipt. Hidden `litter_completion` messages carry outcomes to the owner. Terminal root notices wait for a new explicit root turn after the previous turn settles. Steering into the active turn does not promote these notices. After a full process restart, a missing notice can take one additional explicit root turn to arrive. Inspection and wait results remain immediately available. RPC roots and child parents retain their native completion behavior. Idle and paused owners retain completions without inference. A successful wait or stop handback consumes its outcome and prevents a duplicate model-visible report.

Tool restrictions don't sandbox filesystem paths. PiG 0.4.1's public resource loader does not automatically load arbitrary CLI extensions or themes, and parent-only hooks are not forwarded.

Source entry points are `extensions/pig-litter/operations.go`, `adapter.go`, `tree.go`, `completion.go`, `config.go`, `persistence.go`, and `tool_policy.go`.

## Drive a real provider

Supply an exact provider/model in `PIG_LITTER_MODEL` and run:

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs \
    --piglet pig-litter --model "$PIG_LITTER_MODEL" --case selected
```

The parent uses only child controls. Scout reads a token absent from the parent prompt. Worker uses terminal commands in an explicit working directory. The helper requires matching spawn and completed child IDs, inherited exact models, live widget evidence, and actual file contents. It removes the scout input before resuming that same ID and requires recall of the private token.

Use `--case cancel` to observe **Escape** pausing the parent and its active children. The helper requires a fresh diagnostic, retained child inspection, and explicit resume behavior. Keep real-provider timing observations separate from deterministic lifecycle guarantees.

## Drive controlled outcomes

The product lifecycle fixtures exercise discovery, live messages, resume, wait timeouts, pausing, direct-parent ownership, leaf scouts, large native histories, and completion delivery. They also verify configuration layering, directory resources, canonical file paths, terminal access, and codemode lifecycle calls. Run the scenarios listed in the skill's deterministic section. Their evidence stays under `.pstack/evidence/go-lifecycle/`.

The `persistence` scenario runs two stock PiG processes. It stores a root in a private session directory, exits, then reopens that root with `--session ROOT_UUID`. Require the same child UUID, working directory, retained conversation, duplicate-name rejection, and completed stop outcome. Inspect `persistence.json` and `stdout.jsonl`. This proves orderly restart, not forced-crash durability. Go runtime tests separately exercise HTTP MCP, deferred search, root-only configuration, late registration on resume, empty tool ceilings, and soft allowances.

The terminal fixture checks tool cards, widget ordering, retained-child controls, messages, live reports, root Escape, reload, same-UUID and nested parent-owned resume, root isolation, resize, editor return, and scrollback. It records native JSONL, loopback requests, plain and ANSI screens, and input timings. Run the skill's pinned wrapper. Neither fixture establishes named user source resolution or real credentials.
