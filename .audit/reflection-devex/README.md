# Reviewer preparation and execution evidence

Read-only reviewers now have a shared preparation gate and canonical execution receipts. The work addresses [issue 23](https://github.com/SkrOYC/pig-litter/issues/23) and [issue 27](https://github.com/SkrOYC/pig-litter/issues/27).

The previous adapter could return an old report when a launcher exited successfully without writing a new one. It also retained an event stream without proving effective models, tool use, or task descendants. The new adapter stages each invocation privately and compares its report with the exact canonical final message before publication.

## Prepare one process before fan-out

Run a panel inside the declared environment. The adapter keeps that process's environment for its preparation command and every reviewer. It requests non-login reviewer shells and preserves existing environment filters.

The preparation check uses the installed Codex App Server `command/exec` endpoint with an explicit read-only sandbox. It makes no model request. The project probe checks Go 1.27.1, CGO, local toolchain selection, and the Nix C compiler with a bounded syntax/header check. A failed or incomplete preparation result prevents reviewer fan-out. Generic callers can omit the project-specific probe.

The reviewer launch still uses `codex exec --sandbox read-only`. Preparation evidence and actual reviewer execution evidence remain separate. A configured session sandbox does not prove the permissions of every later command.

## Inspect canonical execution

The receipt validates the exact root thread and completed turn, effective model settings, recorded native tools, and the report's canonical final item. It reconciles indexed metadata with retained rollout headers so a task child cannot disappear merely because an adapter event or index row is absent. Platform guardian records do not count as task children.

Receipts record the examined source and thread boundaries. Missing or unsupported relevant history remains inconclusive. A witnessed restriction violation remains a violation. Optional restrictions cover forbidden model names, allowed native tool names, and native task delegation. These checks do not certify arbitrary subprocess behavior.

The adapter reserves publication destinations and retains private diagnostic artifacts on failure. A final report and receipt must agree on identity and content. Concurrent publishers cannot certify a mixed pair.

## Verify the change

Run the script suite with explicit hidden-directory paths.

```sh
bun test ./.agents/scripts/*.test.js
```

The behavior fixtures cover stale and mismatched reports, preparation failure before fan-out, interrupted and stubborn preparation servers, incomplete unrestricted receipts, hidden or unindexed task children, guardian classification, model and tool restrictions, and concurrent publication.

The real prepared panel launched two non-ephemeral read-only reviewers from one activated process. Their canonical histories record the expected Luna/xhigh settings, successful compiler probes, and exact final-message equality. The first real run exposed numeric Unix-second task timestamps. It remained inconclusive and did not publish reports. The parser correction retains that native shape in a regression.

Bulk evidence remains in the implementation checkout under `.pstack/reflection-devex/`. The independent coordinator audit is retained under the primary checkout's `.pstack/reflection-devex/coordinator-adapter-canonical-audit.json`. The final PR review records the frozen source identities separately from these runtime artifacts.

## Keep the scope bounded

The selected design extends the existing adapter and history reader. It uses a small command-only preflight client rather than replacing reviewer execution with App Server. Three independent model reviewers compared the designs and selected the same existing-owner structure. Their findings added canonical report equality, complete native tool coverage, explicit source boundaries, and destination ownership.

Terminal capture and run-resource changes are a separate delivery unit for issues 24 through 26. The proposed generic negotiation-aware encoder in issue 24 is declined because the maintained tmux transport cannot establish live Kitty flag bits. The terminal unit uses ordinary tmux input and retains released-decoder fixtures instead.

Both units target the declared PiG and SDK v0.4.1 pins. An isolated assessment of [PiG v0.5.1](https://github.com/MichaelKinsy/PiG/releases/tag/v0.5.1) found SDK compilation failures and rejection of the unchanged standalone registration payload. Supporting that release requires separate product migration. The assessment does not claim a newer-host runtime matrix.

The append-only [decision trail](decisions.tsv) records the choices and retained evidence. Local evidence paths in that trail refer to the isolated reflection-devex implementation checkout unless an absolute path identifies coordinator evidence.
