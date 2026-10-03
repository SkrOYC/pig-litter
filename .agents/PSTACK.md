# pstack skills

These skills originate from [Cursor's pstack plugin](https://github.com/cursor/plugins/tree/main/pstack). This repository maintains its own copy for customization.

Edit the local skill files in `.agents/skills`, the [repository skill directory for Codex](https://learn.chatgpt.com/docs/build-skills#where-codex-loads-local-skills). The skills retain their [MIT license](PSTACK-LICENSE); the project's Apache 2.0 license does not replace it.

The local copy uses Codex skill metadata, native agent definitions under `.codex/agents`, and the adapters described in [pstack runtime](pstack-runtime.md). Run `$setup-pstack` to select models and reasoning budgets. Invoke `$poteto-mode` to enter the original workflow; installation alone does not activate it.

The `control-cli`, `control-ui`, and `deslop` dependency skills come from Cursor's `cursor-team-kit` plugin and retain its [MIT license](CURSOR-TEAM-KIT-LICENSE). The `make-bot-ui` skill is excluded.

Use Codex's `/hooks` interface to review and trust the local reminder hooks. The original pstack instructions and workflow structure are preserved; runtime substitutions are documented where the host interfaces differ.
