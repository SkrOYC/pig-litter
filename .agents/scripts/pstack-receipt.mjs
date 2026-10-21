import { createHash } from "node:crypto";
import { withinScope } from "./pstack-history.mjs";

const verified = (citations = []) => ({ status: "verified", citations });
const inconclusive = (reason) => ({ status: "inconclusive", reason });
const violated = (reason, citations = []) => ({
  status: "violated",
  reason,
  citations,
});
const notRequested = () => ({ status: "not-requested" });

function categories(issues) {
  const counts = new Map();
  for (const issue of issues) {
    const reason =
      [
        "Unsupported native session header",
        "Malformed native session header",
        "Unknown relevant native lineage",
        "Unreadable state source",
        "Contradictory index/header",
        "no matching canonical parent link",
        "history changed during inspection",
      ].find((value) => issue.includes(value)) ||
      "Other native history source issue";
    counts.set(reason, (counts.get(reason) || 0) + 1);
  }
  return [...counts].map(([reason, count]) => ({ reason, count }));
}

export function parseRestrictions(value = {}) {
  if (!value || typeof value !== "object" || Array.isArray(value))
    throw new Error("Receipt restrictions must be an object");
  for (const key of Object.keys(value)) {
    if (!["forbiddenModels", "allowedTools", "taskDelegation"].includes(key))
      throw new Error(`Unknown receipt restriction: ${key}`);
  }
  for (const key of ["forbiddenModels", "allowedTools"]) {
    if (
      value[key] !== undefined &&
      (!Array.isArray(value[key]) ||
        value[key].some((name) => typeof name !== "string" || !name))
    )
      throw new Error(`${key} must contain native names`);
  }
  if (
    value.taskDelegation !== undefined &&
    !["allow", "forbid"].includes(value.taskDelegation)
  )
    throw new Error("taskDelegation must be allow or forbid");
  return value;
}

function complete(thread) {
  return (
    thread &&
    !thread.issues.length &&
    thread.turns.length > 0 &&
    thread.turns.every(
      (turn) =>
        turn.id &&
        turn.startedAt &&
        turn.completedAt &&
        !turn.issues.length &&
        Number.isFinite(Date.parse(turn.startedAt)) &&
        Date.parse(turn.completedAt) >= Date.parse(turn.startedAt),
    )
  );
}

export function executionReceipt(
  snapshot,
  { cwd, threadId, requested, report, restrictions = {}, startedAt, endedAt },
) {
  const root = snapshot.threads.find((thread) => thread.id === threadId);
  const descendants = snapshot.threads.filter(
    (thread) => thread.id !== threadId,
  );
  const turns = snapshot.threads.flatMap((thread) =>
    thread.turns.map((turn) => ({ ...turn, threadId: thread.id })),
  );
  const contexts = turns.flatMap((turn) =>
    turn.contexts.map((context) => ({
      ...context,
      threadId: turn.threadId,
      turnId: turn.id,
    })),
  );
  const tools = turns.flatMap((turn) =>
    turn.tools.map((tool) => ({
      ...tool,
      threadId: turn.threadId,
      turnId: turn.id,
    })),
  );
  const rootComplete = complete(root);
  const treeComplete =
    rootComplete &&
    snapshot.inventoryIssues.length === 0 &&
    snapshot.threads.every(complete);
  const modelComplete =
    treeComplete &&
    turns.every(
      (turn) =>
        turn.contexts.length &&
        turn.contexts.every(
          (context) =>
            typeof context.model === "string" &&
            typeof context.effort === "string",
        ),
    );
  const checks = {};
  if (!root?.meta)
    checks.identity = inconclusive(
      snapshot.rootIssue ||
        root?.issues.join("; ") ||
        "Canonical root history is missing",
    );
  else if (
    !withinScope(root.cwd, cwd) ||
    !withinScope(cwd, root.cwd) ||
    root.meta.id !== threadId ||
    root.meta.source !== "exec" ||
    root.meta.originator !== "codex_exec"
  )
    checks.identity = violated(
      "Canonical session identity does not match this launch",
    );
  else if (
    !Number.isFinite(Date.parse(root.meta.timestamp)) ||
    Date.parse(root.meta.timestamp) < Date.parse(startedAt) - 1000 ||
    Date.parse(root.meta.timestamp) > Date.parse(endedAt) + 1000
  )
    checks.identity = violated(
      "Canonical session predates or falls outside this launch",
    );
  else checks.identity = verified([`${threadId}/line-1`]);
  checks.completion =
    rootComplete && root.turns.length === 1
      ? verified(root.turns.map((turn) => turn.completeCitation))
      : inconclusive("The fresh review has no unique complete canonical turn");
  const rootContexts = root?.turns.flatMap((turn) => turn.contexts) || [];
  const mismatch = rootContexts.filter(
    (context) =>
      context.model !== requested.model ||
      (requested.effort && context.effort !== requested.effort),
  );
  checks.requestedModel = mismatch.some(
    (context) =>
      (typeof context.model === "string" &&
        context.model !== requested.model) ||
      (requested.effort &&
        typeof context.effort === "string" &&
        context.effort !== requested.effort),
  )
    ? violated(
        "Effective model or effort differs from the requested reviewer",
        mismatch.map((context) => context.citation),
      )
    : rootComplete &&
        root.turns.every((turn) => turn.contexts.length) &&
        rootContexts.every(
          (context) =>
            typeof context.model === "string" &&
            (!requested.effort || typeof context.effort === "string"),
        )
      ? verified(rootContexts.map((context) => context.citation))
      : inconclusive("Effective reviewer model metadata is missing");
  checks.configuredSandbox = rootContexts.some(
    (context) => context.sandbox && context.sandbox !== "read-only",
  )
    ? violated("The reviewer turn is not configured read-only")
    : rootComplete &&
        root.turns.every((turn) => turn.contexts.length) &&
        rootContexts.every((context) => context.sandbox === "read-only")
      ? verified(rootContexts.map((context) => context.citation))
      : inconclusive("The configured session sandbox is unknown");
  const turn = root?.turns.length === 1 ? root.turns[0] : undefined;
  const final = turn?.finals.length === 1 ? turn.finals[0] : undefined;
  checks.finalMessage =
    final && typeof final.id === "string" && typeof turn.finalText === "string"
      ? final.text === turn.finalText && final.text === report
        ? verified([final.citation, turn.completeCitation])
        : violated(
            "Staged report differs from the exact canonical final message",
            [final.citation],
          )
      : inconclusive("The canonical final message is missing or ambiguous");
  checks.taskDelegation =
    restrictions.taskDelegation === "forbid"
      ? descendants.length ||
        tools.some((tool) => /(^|\.)spawn_agent$/.test(tool.name))
        ? violated(
            "Native task delegation was forbidden",
            descendants.map((thread) => `${thread.id}/line-1`),
          )
        : treeComplete
          ? verified()
          : inconclusive("Task lineage is not complete")
      : notRequested();
  const forbiddenModels = contexts.filter((context) =>
    restrictions.forbiddenModels?.includes(context.model),
  );
  checks.forbiddenModels = restrictions.forbiddenModels?.length
    ? forbiddenModels.length
      ? violated(
          "A forbidden effective model appears in native history",
          forbiddenModels.map((context) => context.citation),
        )
      : modelComplete
        ? verified()
        : inconclusive("Effective models are not known for every task turn")
    : notRequested();
  const forbiddenTools = tools.filter(
    (tool) =>
      restrictions.allowedTools &&
      !restrictions.allowedTools.includes(tool.name),
  );
  checks.allowedTools = restrictions.allowedTools
    ? forbiddenTools.length
      ? violated(
          "A recorded native tool is outside allowedTools",
          forbiddenTools.map((tool) => tool.citation),
        )
      : treeComplete
        ? verified()
        : inconclusive("The native tool inventory is incomplete")
    : notRequested();
  const statuses = Object.values(checks).map((check) => check.status);
  if (!treeComplete || !modelComplete) statuses.push("inconclusive");
  const status = statuses.includes("violated")
    ? "violated"
    : statuses.includes("inconclusive")
      ? "inconclusive"
      : "verified";
  return {
    version: 1,
    status,
    threadId,
    cwd,
    requested,
    checks,
    boundary: {
      launchedAt: startedAt,
      launcherExitedAt: endedAt,
      observedAt: snapshot.observedAt,
      sources: snapshot.sources,
      threads: snapshot.threads.map((thread) => ({
        threadId: thread.id,
        parentThreadId: thread.parent,
        source: thread.boundary,
        turns: thread.turns.map((turn) => ({
          turnId: turn.id,
          startedAt: turn.startedAt,
          completedAt: turn.completedAt,
          startCitation: turn.startCitation,
          completeCitation: turn.completeCitation,
        })),
      })),
    },
    coverage: {
      taskLineage: treeComplete
        ? verified()
        : inconclusive("Native inventory or task execution is incomplete"),
      effectiveModels: modelComplete
        ? verified()
        : inconclusive("One or more task turns lack complete model metadata"),
      issues: categories([
        ...snapshot.inventoryIssues,
        ...snapshot.threads.flatMap((thread) => thread.issues),
      ]),
      historicalSourceWarnings: {
        count: snapshot.inventoryWarnings?.length || 0,
        categories: categories(
          (snapshot.inventoryWarnings || []).map((warning) => warning.reason),
        ),
      },
      guardianThreadsExcluded: snapshot.guardianCount,
    },
    effectiveModels: contexts,
    nativeTools: tools.slice(0, 256),
    nativeToolCount: tools.length,
    nativeToolsOmitted: Math.max(0, tools.length - 256),
    restrictions,
    report: {
      sha256: createHash("sha256").update(report).digest("hex"),
      bytes: Buffer.byteLength(report),
      canonicalItemId: final?.id,
      canonicalCitation: final?.citation,
    },
    limits:
      "Native tool names do not establish subprocess behavior. Configured session sandbox and recorded per-command permission requests are separate evidence.",
  };
}
