# Child sessions

Selecting Pig Litter registers `litter_spawn`, `litter_list`, `litter_inspect`, `litter_message`, `litter_stop`, and `litter_wait`. Children run in separate in-memory Go SDK sessions inside the extension process.

## Behavior

Use `litter_list` to discover configured agents and active direct children. Paused and settled children remain inspectable by their immediate parent. Root is named `root` and follows the same ownership rule. Sibling, ancestor, and grandchild UUIDs cannot bypass it.

Bundled scout has `read` and cannot delegate. Worker also has `write`, `edit`, and `bash`. The default `~/.config/pig/agent/litter.yaml` supplies settings across projects, followed by any legacy `~/.pig/agent/litter.yaml`. Explicit PiG configuration roots and agent-directory overrides select their own user settings instead. Project `litter.yaml` at the child's spawn directory overrides user settings. Either file can override scout or worker by name, or add agents. Directory settings select active tools within the role ceiling. Root's active file tools do not constrain first-level child roles; nested delegation cannot exceed the parent's ceiling.

Spawn returns a session UUID before completion. The UUID remains stable through turns, pause, and explicit resume. Its optional `cwd` selects the child's working directory, relative to the caller when necessary. The child uses native PiG file and terminal tools, settings, instructions, skills, and prompt resources for that directory. File arguments and terminal commands reach those tools unchanged. It inherits the caller's exact model unless an allowed explicit model or fixed agent model overrides it. There is no model fallback.

Wait by UUID. A wait timeout leaves the child running. Cancelling only the wait affects that wait. Pausing a parent pauses its children too. Child runs have no wall-clock deadline. `litter_inspect` returns current status and the latest result without transcripts, evidence receipts, or mailbox pages. `litter_message` can steer a live child or explicitly resume a paused or settled child with `resume:true`. Resume retains the conversation and UUID. `litter_stop` pauses the owned subtree and waits for its active work to stop. Nothing resumes automatically; a resumed parent must explicitly resume each paused child.

A successful spawn survives a later enclosing codemode script failure. Spawn followed by wait in one script works. Cancellation before the individual admission succeeds rolls back that pending admission.

The `Litter N live M kept` widget appears after an operation initializes ownership. It shows recent direct child names and states. Hidden `litter_completion` messages carry outcomes to the owner. Idle and paused owners retain completions without inference. A busy root processes an unconsumed completion as a follow-up. A successful wait or stop handback consumes its outcome and prevents a duplicate model-visible report.

Owner replacement, reload, exit, or a crash loses histories. Retention limits can retire history and reject resume. Tool restrictions don't sandbox filesystem paths. PiG 0.4.1's public resource loader does not automatically load arbitrary CLI extensions or themes, and parent-only hooks are not forwarded.

Source entry points are `extensions/pig-litter/operations.go`, `adapter.go`, `tree.go`, `completion.go`, and `config.go`.

## Drive a real provider

Supply an exact provider/model in `PIG_LITTER_MODEL` and run:

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs \
    --piglet pig-litter --model "$PIG_LITTER_MODEL" --case selected
```

The parent uses only child controls. Scout reads a token absent from the parent prompt. Worker uses terminal commands in an explicit working directory. The helper requires matching spawn and completed child IDs, inherited exact models, live widget evidence, and actual file contents. It removes the scout input before resuming that same ID and requires recall of the private token.

Use `--case cancel` to observe **Escape** pausing the parent and its active children. The helper requires a fresh diagnostic, retained child inspection, and explicit resume behavior. Keep real-provider timing observations separate from deterministic lifecycle guarantees.

## Drive controlled outcomes

The product lifecycle fixtures exercise discovery, live messages, resume, wait timeouts, pausing, direct-parent ownership, leaf scouts, history limits, and completion delivery. They also verify configuration layering, directory resources, canonical file paths, terminal access, and codemode lifecycle calls. Run the scenarios listed in the skill's deterministic section. Their evidence stays under `.pstack/evidence/go-lifecycle/`.

The terminal fixture checks widget, resize, editor, help, stopping, and scrollback. Run the skill's pinned wrapper. Neither fixture establishes named user source resolution or real credentials.
