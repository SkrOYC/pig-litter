# pig-litter

Pig Litter adds foreground child delegation to interactive PiG through its published Go extension SDK. Select the extension or Piglet to expose `pig_litter_agent`. Plain PiG remains unchanged.

## Develop with devenv

Install Nix and devenv 2.4 or newer with the [official devenv install guide](https://devenv.sh/getting-started/). The environment pins PiG 0.4.1, Go 1.27.1, Bun, and tmux.

```sh
devenv shell
```

## Select and delegate

Run either entry point from the repository root.

```sh
pig -e ./extensions/pig-litter
pig --piglet ./piglet.yaml
```

Use `/pig-litter` for a reminder of the available agents. Ask the parent model to call `pig_litter_agent` with a self-contained task.

```json
{"type":"scout","task":"Find the extension entry point and report its registered tools."}
```

```json
{"type":"worker","task":"Update the requested file and report the change.","model":"provider/model","timeoutMs":120000}
```

The bundled `scout` has `read` and `ls`. The bundled `worker` also has `write` and `edit`. Neither agent has `bash`, `grep`, `find`, or delegation tools. Each receives its own system prompt, the supplied task, and a fresh ephemeral PiG session in the parent's working directory. Pig Litter never copies the parent conversation. PiG can load trusted context files from that directory.

The optional model must name an exact available `provider/model`. Without it, the child uses the parent's current model. PiG owns credential resolution. A provider registered only by a parent extension can be unavailable in the child because child extensions are disabled. Pig Litter returns that failure and never substitutes another model.

## Foreground contract

Only one child can run per extension instance. A second call returns `rejected`. Tasks are limited to 16 KiB. The default timeout is two minutes, with a maximum of five minutes. Child extensions, skills, prompt templates, and themes are disabled. The bundled agents omit `grep` and `find` because those PiG tools can spawn search processes that direct-process Exec cancellation does not own. The explicit tool list controls which tools PiG offers the child.

Children use `--offline` to suppress startup package installs and resource networking. Normal model requests still reach the selected provider.

The result contains `state`, `type`, `model`, `report`, `reportTruncated`, and aggregate `usage`. Reports are limited to 8 KiB of UTF-8 text. `reportTruncated` describes the handback size independently of the child's outcome.

| State | Meaning |
| --- | --- |
| `completed` | A valid terminal assistant response stopped normally with the exact requested model. |
| `failed` | PiG, the provider, or the child process failed. |
| `stopped` | The child was cancelled, aborted, or killed by its timeout. |
| `partial` | Terminal evidence is missing, malformed, unknown, or reports an incomplete response. |
| `unavailable` | The model or execution mode is unavailable. |
| `rejected` | The request is invalid, the project is untrusted, or another child is active. |

Launches require a successful host project-trust check. Pig Litter does not pass `--approve` to the child. A parent trust grant does not bypass the child's own trust policy. Child trust or startup errors remain failures.

Pig Litter enables delegation only in the interactive PiG terminal UI until cancellation has been verified in other modes. Press Escape in the terminal UI to cancel the active tool. Each child has a finite timeout.

The tool list is not an operating-system sandbox. File tools can access paths outside the working directory, and a worker can overwrite files. Worktrees do not provide a security boundary. The SDK Exec call buffers process output in the host. Pig Litter bounds parsed output and the parent report, but does not bound total host memory.

Background sessions, separate stop or list commands, steering, retained history, resume, custom agent discovery, and workflows are unavailable in this first version. The agent definitions are bundled in the selected extension Resource.

## Verify

Build and validate the Go extension and Piglet. The check requires exactly one registered model tool, `pig_litter_agent`.

```sh
devenv shell -- check-pig-extension
```

Run parser and boundary tests against the SDK embedded in the pinned PiG binary. The script stages that SDK through PiG and uses a temporary module replacement.

```sh
devenv shell -- scripts/test-pig-litter.sh
```

Drive real selected Go tools and child PiG sessions with a loopback provider fixture. The test uses isolated PiG state and no inherited provider credentials. It verifies read and write tool calls, models, prompts, bounded handback, unavailable models, provider failure, timeout, interactive cancellation, and headless rejection.

```sh
devenv shell -- bun scripts/verify-child-delegation.mjs
```

Run the project-local [verify-pig-litter skill](.agents/skills/verify-pig-litter/SKILL.md) to prove direct selection, Piglet selection, and unselected PiG through the real terminal UI.

```sh
devenv shell -- .agents/skills/verify-pig-litter/scripts/verify.sh
```

Both live checks retain evidence under `.pstack/evidence/` and clean up their own processes and temporary PiG state.

## License

Licensed under the [Apache License 2.0](LICENSE).
