import { closeSync, fstatSync, openSync, readSync } from "node:fs";
import { basename } from "node:path";
import { createHash } from "node:crypto";
import {
  allThreads,
  commandFiles,
  dataRoot,
  decodeItem,
  discoverDatabases,
  withinScope,
} from "./pstack-history.mjs";

function prefix(path, headerOnly = false) {
  const fd = openSync(path, "r");
  try {
    const before = fstatSync(fd);
    if (!before.isFile())
      throw new Error("Native history source is not a regular file");
    const limit = headerOnly ? 1024 * 1024 : 64 * 1024 * 1024;
    if (!headerOnly && before.size > limit)
      throw new Error("Native history exceeds the 64 MiB inspection bound");
    let bytes = Buffer.alloc(Math.min(before.size, headerOnly ? 16384 : limit));
    let count = 0;
    while (true) {
      const read = readSync(fd, bytes, count, bytes.length - count, count);
      if (!read)
        throw new Error(
          "Native history prefix ended before its recorded boundary",
        );
      count += read;
      if (headerOnly && bytes.subarray(0, count).includes(10)) break;
      if (count < bytes.length) continue;
      if (!headerOnly || count >= before.size || count >= limit) break;
      const expanded = Buffer.alloc(
        Math.min(before.size, limit, bytes.length * 2),
      );
      bytes.copy(expanded);
      bytes = expanded;
    }
    const after = fstatSync(fd);
    if (
      before.ino !== after.ino ||
      (!headerOnly &&
        (before.size !== after.size || before.mtimeMs !== after.mtimeMs))
    )
      throw new Error("Native history changed during inspection");
    if (headerOnly) {
      const end = bytes.indexOf(10);
      if (end < 0) throw new Error("Native history has no complete header");
      try {
        return JSON.parse(bytes.subarray(0, end).toString("utf8"));
      } catch {
        throw new Error("Malformed native session header JSON");
      }
    }
    if (!bytes.length || bytes.at(-1) !== 10)
      throw new Error("Native history has an incomplete final record");
    return {
      bytes,
      boundary: {
        source: basename(path),
        bytes: before.size,
        sha256: createHash("sha256").update(bytes).digest("hex"),
      },
    };
  } finally {
    closeSync(fd);
  }
}

function sourceValue(value) {
  if (typeof value !== "string") return value;
  try {
    return JSON.parse(value);
  } catch {
    return value;
  }
}

function timestamp(value) {
  const milliseconds =
    typeof value === "number"
      ? value < 1e12
        ? value * 1000
        : value
      : Date.parse(value);
  return Number.isFinite(milliseconds)
    ? new Date(milliseconds).toISOString()
    : undefined;
}

function lineage(meta) {
  const source = sourceValue(meta.source);
  const spawn = source?.subagent?.thread_spawn;
  if (
    spawn &&
    typeof spawn.parent_thread_id === "string" &&
    meta.thread_source === "subagent"
  )
    return { kind: "task", parent: spawn.parent_thread_id };
  if (
    source?.subagent?.other === "guardian" &&
    !spawn &&
    meta.thread_source === "guardian_review"
  )
    return { kind: "guardian" };
  if (
    ["cli", "exec", "vscode", "mcp"].includes(source) &&
    [undefined, null, "user"].includes(meta.thread_source)
  )
    return { kind: "root" };
  return { kind: "unknown" };
}

function header(record) {
  if (
    record.type !== "session_meta" ||
    typeof record.payload?.id !== "string" ||
    typeof record.payload?.cwd !== "string"
  )
    throw new Error("Unsupported native session header");
  const p = record.payload;
  return {
    id: p.id,
    cwd: p.cwd,
    timestamp: timestamp(p.timestamp),
    source: p.source,
    thread_source: p.thread_source,
    originator: p.originator,
  };
}

function readExecution(entry, includeItems) {
  if (entry.paths.size !== 1)
    throw new Error(
      "Native execution has missing or ambiguous rollout sources",
    );
  const path = [...entry.paths][0];
  const { bytes, boundary } = prefix(path);
  const records = bytes
    .toString("utf8")
    .split("\n")
    .slice(0, -1)
    .map((line, index) => {
      try {
        return {
          ...JSON.parse(line),
          citation: `${entry.id}/line-${index + 1}`,
        };
      } catch {
        throw new Error(`Malformed canonical history at line ${index + 1}`);
      }
    });
  const meta = header(records[0]);
  if (meta.id !== entry.id || meta.cwd !== entry.cwd)
    throw new Error("Canonical session identity contradicts its inventory");
  const turns = new Map();
  const issues = [];
  const spawnCalls = new Map();
  const spawnReferences = new Set();
  let current;
  let previousOrdinal = -1;
  for (const record of records.slice(1)) {
    if (Number.isSafeInteger(record.rollout_ordinal)) {
      if (record.rollout_ordinal < previousOrdinal)
        issues.push("Canonical ordinals moved backwards");
      previousOrdinal = record.rollout_ordinal;
    }
    const p = record.payload;
    if (!p || typeof p !== "object") continue;
    if (record.type === "event_msg" && p.type === "task_started") {
      if (
        !p.turn_id ||
        turns.has(p.turn_id) ||
        (current && !current.completedAt)
      )
        issues.push("Ambiguous native turn start");
      current = {
        id: p.turn_id,
        startedAt: timestamp(p.started_at ?? record.timestamp),
        startCitation: record.citation,
        contexts: [],
        tools: [],
        finals: [],
        issues: [],
      };
      turns.set(p.turn_id, current);
    } else if (record.type === "turn_context") {
      const turn = turns.get(p.turn_id);
      if (!turn || turn.completedAt) {
        issues.push("Model context has no active canonical turn");
        continue;
      }
      turn.contexts.push({
        model: p.model,
        effort: p.effort,
        sandbox: p.sandbox_policy?.type,
        citation: record.citation,
      });
    } else if (record.type === "event_msg" && p.type === "task_complete") {
      const turn = turns.get(p.turn_id);
      if (!turn || turn.completedAt) {
        issues.push("Completion has no unique native turn start");
        continue;
      }
      turn.completedAt = timestamp(p.completed_at ?? record.timestamp);
      turn.completeCitation = record.citation;
      turn.finalText = p.last_agent_message;
    } else if (record.type === "response_item") {
      if (["function_call", "custom_tool_call"].includes(p.type)) {
        if (!current || current.completedAt || typeof p.name !== "string") {
          issues.push("Native tool call has no active turn or name");
          continue;
        }
        let permissions;
        if (typeof p.arguments === "string") {
          try {
            permissions = JSON.parse(p.arguments).sandbox_permissions;
          } catch {}
        }
        current.tools.push({
          name: p.name,
          citation: record.citation,
          ...(typeof permissions === "string"
            ? { requestedPermissions: permissions }
            : {}),
        });
        if (/(^|\.)spawn_agent$/.test(p.name))
          spawnCalls.set(p.call_id, record.citation);
      } else if (
        ["function_call_output", "custom_tool_call_output"].includes(p.type) &&
        spawnCalls.has(p.call_id)
      ) {
        try {
          const output =
            typeof p.output === "string" ? JSON.parse(p.output) : p.output;
          const id = output?.agent_id || output?.thread_id;
          if (typeof id === "string") spawnReferences.add(id);
          else if (!output?.error)
            issues.push("Native spawn result has no supported child identity");
        } catch {
          issues.push("Native spawn result could not be inspected");
        }
        spawnCalls.delete(p.call_id);
      } else if (
        p.type === "message" &&
        p.role === "assistant" &&
        p.phase === "final_answer"
      ) {
        if (!current || current.completedAt) {
          issues.push("Final message is outside an active turn");
          continue;
        }
        const content = p.content;
        if (
          !Array.isArray(content) ||
          content.some(
            (part) =>
              part.type !== "output_text" || typeof part.text !== "string",
          )
        )
          current.issues.push("Unsupported canonical final content");
        else
          current.finals.push({
            id: p.id,
            text: content.map((part) => part.text).join(""),
            citation: record.citation,
          });
      } else if (typeof p.type === "string" && /_call$/.test(p.type))
        issues.push(`Unsupported native call record ${p.type}`);
    }
  }
  if (spawnCalls.size) issues.push("Native spawn call has no completed result");
  return {
    id: meta.id,
    cwd: meta.cwd,
    meta,
    boundary,
    turns: [...turns.values()],
    issues,
    spawnReferences: [...spawnReferences],
    ...(includeItems
      ? {
          items: records.map((record, index) => ({
            citation: record.citation,
            ordinal: record.rollout_ordinal ?? index + 1,
            item_id: record.payload?.id || `line-${index + 1}`,
            ...decodeItem(record.payload || record, record.type),
          })),
        }
      : {}),
  };
}

export function collectExecution({
  cwd,
  threadId,
  root = dataRoot(),
  includeRootItems = false,
}) {
  const observedAt = new Date().toISOString();
  const inventoryIssues = [];
  const inventoryWarnings = [];
  const headerProblems = [];
  const entries = new Map();
  const sources = [];
  let rows = [];
  try {
    const databases = discoverDatabases(root);
    inventoryIssues.push(...databases.issues);
    for (const state of databases.states) {
      const capturedAt = new Date().toISOString();
      const records = allThreads([state]);
      rows.push(...records);
      sources.push({
        source: basename(state.file),
        kind: "thread-index",
        capturedAt,
        count: records.length,
        sha256: createHash("sha256")
          .update(JSON.stringify(records))
          .digest("hex"),
        exhausted: true,
      });
    }
  } catch (error) {
    inventoryIssues.push(error.message);
  }
  for (const row of rows)
    entries.set(row.thread_id, {
      id: row.thread_id,
      cwd: row.cwd,
      timestamp: row.createdAt,
      source: sourceValue(row.source),
      thread_source: row.thread_source,
      paths: new Set(row.rollout_path ? [row.rollout_path] : []),
      indexed: true,
    });
  let paths = [];
  try {
    paths = commandFiles(root, "rollout-*.jsonl");
  } catch (error) {
    inventoryIssues.push(error.message);
  }
  const retained = new Set([
    ...paths,
    ...rows.flatMap((row) => (row.rollout_path ? [row.rollout_path] : [])),
  ]);
  for (const path of retained) {
    let record;
    try {
      record = prefix(path, true);
      const meta = header(record);
      const existing = entries.get(meta.id);
      if (
        existing &&
        (existing.cwd !== meta.cwd ||
          (existing.source &&
            JSON.stringify(sourceValue(existing.source)) !==
              JSON.stringify(meta.source)))
      )
        inventoryIssues.push(`Contradictory index/header for ${meta.id}`);
      const paths = existing?.paths || new Set();
      paths.add(path);
      entries.set(meta.id, {
        ...meta,
        paths,
        indexed: existing?.indexed || false,
      });
    } catch (error) {
      headerProblems.push({
        source: basename(path),
        reason: error.message,
        timestamp:
          record?.payload?.timestamp ||
          record?.timestamp ||
          rows.find((row) => row.rollout_path === path)?.createdAt,
      });
    }
  }
  sources.push({
    source: "retained-rollouts",
    kind: "session-headers",
    count: retained.size,
    exhausted: true,
  });
  const rootEntry = entries.get(threadId);
  if (
    !rootEntry ||
    typeof rootEntry.cwd !== "string" ||
    !withinScope(rootEntry.cwd, cwd)
  )
    return {
      observedAt,
      sources,
      inventoryIssues,
      rootIssue: "Canonical root is missing or outside the requested workspace",
      threads: [],
      guardianCount: 0,
    };
  const rootTime = Date.parse(rootEntry.timestamp);
  for (const problem of headerProblems) {
    if (
      Number.isFinite(rootTime) &&
      Number.isFinite(Date.parse(problem.timestamp)) &&
      Date.parse(problem.timestamp) < rootTime - 1000
    )
      inventoryWarnings.push(problem);
    else inventoryIssues.push(`${problem.source}: ${problem.reason}`);
  }
  const children = new Map();
  let guardianCount = 0;
  for (const entry of entries.values()) {
    const relation = lineage(entry);
    if (relation.kind === "task") {
      const list = children.get(relation.parent) || [];
      list.push(entry.id);
      children.set(relation.parent, list);
    } else if (relation.kind === "guardian") guardianCount++;
    else if (
      relation.kind === "unknown" &&
      (!Number.isFinite(rootTime) ||
        !Number.isFinite(Date.parse(entry.timestamp)) ||
        Date.parse(entry.timestamp) >= rootTime - 1000)
    )
      inventoryIssues.push(`Unknown relevant native lineage for ${entry.id}`);
  }
  const threads = [];
  const queue = [threadId];
  const visited = new Set();
  while (queue.length) {
    const id = queue.shift();
    if (visited.has(id)) {
      inventoryIssues.push(`Cyclic or duplicate task lineage for ${id}`);
      continue;
    }
    visited.add(id);
    const entry = entries.get(id);
    if (!entry) {
      inventoryIssues.push(`Referenced task ${id} has no canonical header`);
      continue;
    }
    try {
      const execution = readExecution(
        entry,
        includeRootItems && id === threadId,
      );
      execution.parent = lineage(entry).parent;
      threads.push(execution);
      for (const spawned of execution.spawnReferences) {
        const child = entries.get(spawned);
        if (!child || lineage(child).parent !== id)
          inventoryIssues.push(
            `Spawn reference ${spawned} has no matching canonical parent link`,
          );
      }
    } catch (error) {
      threads.push({
        id,
        parent: lineage(entry).parent,
        issues: [error.message],
        turns: [],
      });
    }
    queue.push(...(children.get(id) || []));
  }
  return {
    observedAt: new Date().toISOString(),
    sources,
    inventoryIssues,
    inventoryWarnings,
    threads,
    guardianCount,
  };
}
