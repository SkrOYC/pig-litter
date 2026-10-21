# Codex runtime translations

Read this when a pstack workflow delegates, selects models, reads history, or uses a host control. The workflow's instructions and report format stay intact.

## Delegation

Use the native `collaboration` operations exposed by the session:

| Operation | Native call |
| --- | --- |
| Launch | `spawn_agent` with a task name and delegation message. |
| Steer | `send_message` to the returned child identity. |
| Resume retained work | `followup_task` to that same identity. |
| Inspect | `list_agents`. |
| Interrupt | `interrupt_agent`. |
| Join | `wait_agent` and collect completion notifications. |

These are model-callable tools, not shell commands. A launch already allows the parent to continue working. Preserve the workflow's child identities, ownership, join order, retries, prompts, and handback format. Batch only when runtime capacity requires it, retaining every requested task or panel seat.

Cursor's `subagent_type`, `environment`, `run_in_background`, and `readonly` fields are not arguments to this interface. Load the original named agent's `developer_instructions` from `.codex/agents/<name>.toml` into the delegation prompt when the runtime cannot select the registered role directly. A generic agent corresponds to Codex's `default` role.

For an explicit `model` or `reasoning_effort` override, use `fork_turns: "none"` and supply the source prompt plus necessary task context. A full-history fork cannot accept overrides in this interface. For `auto` or `inherit-parent`, run `bun .agents/scripts/pstack-models.mjs parent` and pass the active parent's pair explicitly. Omitting settings can select a configured generic child default instead of the source's requested parent model. Keep the alias marker for the source's no-fallback-on-alias rules.

The registered `comment-sicko` agent uses a read-only sandbox. A prompt alone does not select or enforce that configuration. For a source `readonly: true` launch, use a configured native role if the interface exposes it. Otherwise, run `bun .agents/scripts/pstack-readonly.mjs --cwd WORKSPACE --prompt PROMPT_FILE --output REPORT_FILE --model MODEL --effort EFFORT`, adding `--role comment-sicko` for that named reviewer. The adapter uses `codex exec --sandbox read-only` with non-login tool shells. It preserves native configuration, credentials, and the original prompt. It publishes the unchanged report only when canonical native history confirms the session, effective model, completed turn, and exact final message. Events and receipts use adjacent `.events.jsonl` and `.receipt.json` files. Pass the active parent's effective model and effort explicitly for an inheritance choice. An ephemeral session without persisted history produces an inconclusive receipt and no accepted report. Do not add read-only enforcement to writable workflows such as Reflect. An external MCP is not read-only merely because the filesystem is read-only.

For a panel, write one manifest with a single workspace and distinct output files. Use one managed launcher so preparation completes before fan-out. In this repository, run `devenv shell -- bun .agents/scripts/pstack-readonly.mjs --panel PANEL_JSON`. The manifest has this shape:

```json
{
  "cwd": "/absolute/workspace",
  "preflight": "/absolute/workspace/.agents/scripts/pstack-toolchain-probe.mjs",
  "jobs": [
    {"prompt": "/evidence/a.prompt.md", "output": "/evidence/a.md", "model": "MODEL", "effort": "EFFORT"},
    {"prompt": "/evidence/b.prompt.md", "output": "/evidence/b.md", "model": "MODEL", "effort": "EFFORT"}
  ],
  "restrictions": {"taskDelegation": "forbid", "forbiddenModels": []}
}
```

Choose restrictions from the active task. `forbiddenModels` contains exact model IDs. `allowedTools`, when supplied, contains exact native tool names. `taskDelegation` accepts `allow` or `forbid`. Unknown keys fail before launch. These restrictions audit canonical execution. They do not establish subprocess behavior. The receipt distinguishes the configured session sandbox from recorded per-command permission requests.

The optional project preflight runs through App Server `command/exec` with an explicit read-only policy, network disabled, a timeout, and bounded output. It starts no thread or model turn. The Go probe belongs to this repository. Omit it for a generic non-Go reviewer, or provide that project's script. A singleton accepts `--preflight SCRIPT` and `--restrictions JSON_FILE`. Run the launcher inside the project's declared environment. A successful receipt records preparation evidence; it does not carry environment variables to a later process. The launcher preserves the operator's environment filters.

Retain the panel process handle for collection or cancellation. Each report has its own native thread ID and artifacts. Failed or inconclusive jobs retain a private `.pstack-review-*` directory. Destination reservations prevent concurrent publication. After a crash, inspect the named reservation and owner before manually removing it. Never infer successful execution from a preexisting report or a receipt whose report digest no longer matches.

Children share the checkout. When the source asks for candidate isolation, create its separate worktree first and put the absolute directory in the child prompt. Each child command must use that directory as its working directory, and each file edit must target it. Verify the child diff there before integrating it. If the runtime cannot carry the source's required isolation, report the launch limitation rather than using the parent checkout.

Codex also exposes `codex cloud exec --env ENV_ID --branch BRANCH PROMPT`, `cloud status`, `cloud list`, `cloud diff`, and `cloud apply`. These are separate Cloud task operations, not an `environment` argument to `collaboration`. They require a selected remote environment. Per-seat model selection, steering, cancellation, and the source cloud-sleeper wake chain have not been verified for that interface. Preserve those source requirements and report the specific gap when a workflow requires them; do not claim Codex has no Cloud submission feature.

## Models

Run `bun .agents/scripts/pstack-models.mjs show` to read all roles and panel seats, or `resolve "ROLE LABEL"` for one source role. Native settings use `.codex/agents/pstack-*.toml`, with user-level settings under `~/.codex/agents` when no project override exists. Panels use numbered seat files, so their configured list length still determines fan-out.

Use the same `parent` command for standalone read-only sessions. It reads only the current thread's settings from native state using `CODEX_THREAD_ID` and reports missing data rather than guessing; a runtime-provided effective pair can also supply these values.

`$setup-pstack` retains the source's budget/map/confirm workflow and writes those native settings. The helper's defaults translate the original fast code, judgment, and GPT seats to `gpt-6-luna` with `xhigh`, `gpt-6-astra` with `max`, and `gpt-6.1-sol` with `max`. Setup can change every role. Model availability and supported efforts must be checked against the active runtime or its model catalog; these defaults are not an account entitlement claim.

Pass a model ID and effort separately. Preserve `auto` and `inherit-parent` as pstack setup choices by omitting model fields in their native configuration files, then resolving the active parent's pair at launch. They are not Codex model IDs. Where the source compares provider families, compare distinct Codex model lines instead, retaining the panel seats and cross-judge selection. Keep the source's rejected-choice fallback/reporting sequence.

## History

Use `bun .agents/scripts/pstack-history.mjs --help` for scoped history acquisition. It resolves `CODEX_HOME` or `~/.codex` and reads paginated SQLite items or retained native rollout JSONL. `list` and `search` require a workspace scope. Select an explicit native identity for `read`; concurrent runs make `last` ambiguous. `read --cwd WORKSPACE --id THREAD_ID --complete` returns retained canonical history with its fixed byte boundary and explicit completeness. Unsupported or unfinished records remain inconclusive. Existing paged reads remain available. Continue them with both `--after ORDINAL` and `--after-id ITEM_ID`.

Execution receipts reconcile indexed threads, retained session headers, and native spawn references. Canonical task-parent links define descendants across workspace paths. Guardian records are separate. An exhausted 100-row list is not an exhaustive task census. Receipts include source and per-thread boundaries, effective contexts, and at most 256 native tool records with an omitted-record count. Historical source warnings use compact counts; private staged diagnostics retain detailed source names. Keep the original mining steps, time window, source categories, and canonical citations.

## Host controls

Use the active Codex user-question interface for `AskQuestion`, retaining the source questions, options, and gate. Use native plan tracking for todo updates. If the question UI is absent, ask the same question in the conversation.

Use `/goal` for source requests to keep working toward an outcome, retaining their stop and pause rules. Fixed-cadence Scheduled management belongs to ChatGPT web/desktop; a goal is not a clock scheduler. For the source's supervised local audit timer, run `bun .agents/scripts/pstack-tick.mjs --thread NATIVE_ID --minutes 30` in a managed shell session. It queues one tick using native `codex queue`, then exits. Preserve per-tick rearming and cancel the owned process when the source loop stops. This does not install cron or an independent service.

Poteto Mode's hook adapter keeps per-session activation and opt-out state and re-injects the source reminder on user turns, resume, and compaction. Review new hook definitions through Codex's `/hooks` interface before they run. The mode is not enabled just because its skills or hooks are installed. An explicit leading `$poteto-mode` invocation activates state in the prompt hook. State lives under ignored `.pstack/state`, outside Codex's protected `.codex` configuration directory, so ordinary workspace-write execution can maintain it. Activation is idempotent when the hook has already set the flag.

`control-cli`, `control-ui`, and `deslop` are local dependency skills. Retain their workflow calls. `skill-creator` supplies the native authoring capability.
