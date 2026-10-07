# Child sessions

Selecting Pig Litter registers `litter_spawn`, `litter_list`, `litter_inspect`, `litter_message`, `litter_stop`, and `litter_wait`. Children run in separate in-memory Go SDK sessions inside the extension process.

## Behavior

Use `litter_list` to discover configured agents and retained children. Bundled scout has `read`; worker also has `write` and `edit`. Project `litter.yaml` can define named agents and limits. Agent tools cannot exceed the role or caller's ceiling.

Spawn returns a child ID and generation before completion. It inherits the caller's exact model unless an allowed explicit model or fixed agent model overrides it. There is no model fallback. Canonical parent file tools must remain callable because child file operations pass through the parent host and its permission hooks.

Wait for the exact ID and generation. A wait timeout or cancelled wait leaves the child running. `litter_inspect` returns bounded summaries and optional transcript pages. `litter_message` can steer a live child or resume a settled child with `resume:true`. Resume retains history, keeps the ID, and increments the generation. `litter_stop` cancels the owned subtree and waits for cleanup.

The `Litter N live M kept` widget appears after an operation initializes ownership. It shows the two most recent child names and states. Hidden `litter_completion` messages carry outcomes to the owner. An idle owner retains the completion without starting inference; a busy owner processes it as a follow-up.

Owner replacement, reload, exit, or a crash loses histories. Retention limits can retire history and reject resume. Tool restrictions don't sandbox filesystem paths.

Source entry points are `extensions/pig-litter/operations.go`, `adapter.go`, `tree.go`, `completion.go`, and `config.go`.

## Drive a real provider

Supply an exact provider/model in `PIG_LITTER_MODEL` and run:

```sh
.agents/skills/verify-pig-litter/scripts/live.mjs \
    --piglet pig-litter --model "$PIG_LITTER_MODEL" --case selected
```

The parent uses only child controls. Scout reads a token absent from the parent prompt. Worker writes and reads another token. The helper requires matching spawn and completed generation records, inherited exact models, successful file calls in retained transcripts, live widget evidence, and actual file contents. It then resumes scout at generation 2 and requires recall of the private token.

Use `--case cancel` to observe **Escape** cancelling a pending wait while the child continues. The helper requires a fresh diagnostic, a running inspection, explicit stop, and a stopped outcome. Keep real-provider timing observations separate from deterministic lifecycle guarantees.

## Drive controlled outcomes

The product lifecycle fixtures exercise paged discovery, transcripts, live messages, resume, stale generations, wait timeouts, stopping, nested ownership, history limits, and completion delivery. Run the scenarios listed in the skill's deterministic section. Their evidence stays under `.pstack/evidence/go-lifecycle/`.

The terminal fixture checks widget, resize, editor, help, stopping, and scrollback. Run the skill's pinned wrapper. Neither fixture establishes named user source resolution or real credentials.
