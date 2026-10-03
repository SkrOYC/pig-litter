#!/usr/bin/env bun
import { Database } from "bun:sqlite";
import { createReadStream, existsSync, realpathSync } from "node:fs";
import { homedir } from "node:os";
import { isAbsolute, join, relative, resolve, sep } from "node:path";
import { createInterface } from "node:readline";
import { spawnSync } from "node:child_process";

const DEFAULT_LIMIT = 20;
const MAX_LIMIT = 100;
const PAGE_SIZE = 250;

function fail(message) {
  throw new Error(message);
}

function parseArgs(argv) {
  if (argv.length === 0 || argv[0] === "--help" || argv[0] === "-h")
    return { command: "help" };
  const [command, ...rest] = argv;
  if (!["list", "read", "search", "last"].includes(command))
    fail(`Unknown command: ${command}`);
  const options = {};
  for (let i = 0; i < rest.length; i += 1) {
    const key = rest[i];
    if (!key.startsWith("--")) fail(`Unexpected argument: ${key}`);
    const name = key.slice(2);
    if (
      !["cwd", "since", "limit", "id", "after", "after-id", "pattern"].includes(
        name,
      )
    )
      fail(`Unknown option: ${key}`);
    if (options[name] !== undefined) fail(`Duplicate option: ${key}`);
    if (i + 1 >= rest.length || rest[i + 1].startsWith("--"))
      fail(`Missing value for ${key}`);
    options[name] = rest[++i];
  }
  if (["list", "search", "last"].includes(command) && !options.cwd)
    fail(`${command} requires --cwd PATH`);
  if (command === "read" && !options.id) fail("read requires --id ID");
  if (command === "search" && !options.pattern)
    fail("search requires --pattern TEXT");
  if (options.limit !== undefined) {
    options.limit = Number(options.limit);
    if (
      !Number.isSafeInteger(options.limit) ||
      options.limit < 1 ||
      options.limit > MAX_LIMIT
    ) {
      fail(`--limit must be an integer from 1 to ${MAX_LIMIT}`);
    }
  }
  if (options.after !== undefined) {
    options.after = Number(options.after);
    if (!Number.isSafeInteger(options.after) || options.after < 0)
      fail("--after must be a non-negative integer ordinal");
  }
  if (options["after-id"] !== undefined && options.after === undefined)
    fail("--after-id requires --after ORDINAL");
  if (
    options.since !== undefined &&
    !Number.isFinite(Date.parse(options.since))
  )
    fail("--since must be a valid ISO date or timestamp");
  return { command, ...options };
}

function printHelp() {
  return `Read-only Codex conversation history. Output is JSON.\n\nUsage:\n  bun .agents/scripts/pstack-history.mjs list --cwd PATH [--since ISO] [--limit N]\n  bun .agents/scripts/pstack-history.mjs read --id ID [--limit N] [--after ORDINAL [--after-id ITEM_ID]]\n  bun .agents/scripts/pstack-history.mjs search --cwd PATH --pattern TEXT [--since ISO] [--limit N]\n  bun .agents/scripts/pstack-history.mjs last --cwd PATH\n\nWorkspace scope includes threads whose recorded cwd is PATH or a descendant. Separate sibling worktrees require their own --cwd. Limits are capped at ${MAX_LIMIT}. Read pages are ordered by ordinal then item identity; to continue within a shared ordinal, pass both --after ORDINAL and --after-id ITEM_ID from the final returned item.\n`;
}

function dataRoot(env = process.env, home = homedir()) {
  return resolve(env.CODEX_HOME || join(home, ".codex"));
}

function commandFiles(root, glob) {
  const result = spawnSync(
    "rg",
    ["--files", "--hidden", "--glob", glob, root],
    { encoding: "utf8" },
  );
  if (result.error)
    fail(`Cannot discover history files with rg: ${result.error.message}`);
  if (result.status > 1)
    fail(`History file discovery failed: ${result.stderr.trim()}`);
  return result.stdout.split("\n").filter(Boolean);
}

function tableInfo(db, table) {
  return db
    .query(`PRAGMA table_info("${table.replaceAll('"', '""')}")`)
    .all()
    .map((row) => row.name);
}

function tables(db) {
  return db
    .query("SELECT name FROM sqlite_master WHERE type = 'table'")
    .all()
    .map((row) => row.name);
}

function discoverDatabases(root) {
  const files = commandFiles(root, "*.sqlite").filter((file) =>
    /(?:^|\/)state_.*\.sqlite$/.test(file),
  );
  const candidates = [];
  for (const file of files) {
    let db;
    try {
      db = new Database(file, { readonly: true });
      const names = tables(db);
      if (!names.includes("threads")) continue;
      const columns = tableInfo(db, "threads");
      if (["id", "cwd"].every((name) => columns.includes(name)))
        candidates.push({ file, columns });
    } catch {
      // A corrupt candidate is reported only when no usable state database exists.
    } finally {
      db?.close();
    }
  }
  if (!candidates.length)
    fail(
      `No readable state database with a compatible threads schema under ${root}`,
    );
  const histories = commandFiles(root, "thread_history_*.sqlite");
  return { states: candidates, histories };
}

function resolvedPath(path) {
  const expanded = resolve(path);
  try {
    return realpathSync.native(expanded);
  } catch {
    return expanded;
  }
}

function withinScope(threadCwd, requestedCwd) {
  const base = resolvedPath(requestedCwd);
  const candidate = resolvedPath(threadCwd);
  const rel = relative(base, candidate);
  return (
    rel === "" ||
    (rel !== ".." && !rel.startsWith(`..${sep}`) && !isAbsolute(rel))
  );
}

function timestampOf(row, columns) {
  const candidates = [
    "updated_at_ms",
    "last_activity_at_ms",
    "updated_at",
    "created_at_ms",
    "created_at",
  ];
  for (const key of candidates) {
    if (!columns.includes(key) || row[key] == null) continue;
    const raw = row[key];
    const numeric = Number(raw);
    const ms = Number.isFinite(numeric)
      ? numeric > 1e12
        ? numeric
        : numeric * 1000
      : Date.parse(String(raw));
    if (Number.isFinite(ms)) return { value: new Date(ms).toISOString(), ms };
  }
  return { value: null, ms: 0 };
}

function readTimestamp(row, columns, choices) {
  for (const key of choices) {
    if (!columns.includes(key) || row[key] == null) continue;
    const numeric = Number(row[key]);
    const ms = Number.isFinite(numeric)
      ? numeric > 1e12
        ? numeric
        : numeric * 1000
      : Date.parse(String(row[key]));
    if (Number.isFinite(ms)) return new Date(ms).toISOString();
  }
  return null;
}

function rowSummary(row, columns) {
  const timestamp = timestampOf(row, columns);
  return {
    thread_id: row.id,
    cwd: row.cwd,
    history_mode: columns.includes("history_mode")
      ? (row.history_mode ?? null)
      : null,
    timestamp: timestamp.value,
    createdAt: readTimestamp(row, columns, ["created_at_ms", "created_at"]),
    updatedAt: readTimestamp(row, columns, [
      "updated_at_ms",
      "last_activity_at_ms",
      "updated_at",
    ]),
    rollout_path: columns.includes("rollout_path")
      ? (row.rollout_path ?? null)
      : null,
    archived: columns.includes("archived") ? (row.archived ?? null) : null,
  };
}

function allThreads(stateCandidates, { cwd, since, id, limit } = {}) {
  const sinceMs = since === undefined ? -Infinity : Date.parse(since);
  const found = [];
  for (const candidate of stateCandidates) {
    let db;
    try {
      db = new Database(candidate.file, { readonly: true });
      const columns = tableInfo(db, "threads");
      if (!["id", "cwd"].every((name) => columns.includes(name)))
        fail(`Incompatible threads schema in ${candidate.file}`);
      const selected = [
        "id",
        "cwd",
        "history_mode",
        "rollout_path",
        "archived",
        "updated_at_ms",
        "last_activity_at_ms",
        "updated_at",
        "created_at_ms",
        "created_at",
      ].filter((column) => columns.includes(column));
      const predicates = [];
      const bindings = [];
      if (id !== undefined) {
        predicates.push("id = ?");
        bindings.push(id);
      }
      if (cwd) {
        const scopes = [...new Set([resolve(cwd), resolvedPath(cwd)])];
        const scopeFilters = scopes.map(
          () => "(cwd = ? OR substr(cwd, 1, length(?)) = ?)",
        );
        predicates.push(`(${scopeFilters.join(" OR ")})`);
        for (const scope of scopes) {
          const prefix = scope.endsWith(sep) ? scope : `${scope}${sep}`;
          bindings.push(scope, prefix, prefix);
        }
      }
      if (since !== undefined && columns.includes("updated_at_ms")) {
        predicates.push("updated_at_ms >= ?");
        bindings.push(sinceMs);
      }
      const where = predicates.length
        ? ` WHERE ${predicates.join(" AND ")}`
        : "";
      const order = columns.includes("updated_at_ms")
        ? " ORDER BY updated_at_ms DESC, id"
        : " ORDER BY id";
      const cap =
        limit === undefined || !columns.includes("updated_at_ms")
          ? ""
          : " LIMIT ?";
      if (cap) bindings.push(limit);
      const rows = db
        .query(
          `SELECT ${selected.join(", ")} FROM threads${where}${order}${cap}`,
        )
        .all(...bindings);
      for (const row of rows) {
        if (cwd && (typeof row.cwd !== "string" || !withinScope(row.cwd, cwd)))
          continue;
        const item = rowSummary(row, columns);
        if (item.timestamp && Date.parse(item.timestamp) < sinceMs) continue;
        if (!item.timestamp && since !== undefined) continue;
        found.push(item);
      }
    } catch (error) {
      fail(`Could not read state database ${candidate.file}: ${error.message}`);
    } finally {
      db?.close();
    }
  }
  found.sort(
    (a, b) =>
      Date.parse(b.timestamp ?? "1970-01-01") -
        Date.parse(a.timestamp ?? "1970-01-01") ||
      String(a.thread_id).localeCompare(String(b.thread_id)),
  );
  const newest = new Map();
  for (const item of found) {
    if (!newest.has(item.thread_id)) newest.set(item.thread_id, item);
  }
  return [...newest.values()];
}

function historySchema(files, threadId) {
  for (const file of files) {
    let db;
    try {
      db = new Database(file, { readonly: true });
      if (!tables(db).includes("thread_items")) continue;
      const columns = tableInfo(db, "thread_items");
      if (!["thread_id", "item_json"].every((name) => columns.includes(name)))
        continue;
      if (
        db
          .query("SELECT 1 FROM thread_items WHERE thread_id = ? LIMIT 1")
          .get(threadId)
      )
        return { file, columns };
    } catch {
      // Continue searching alternate history database versions.
    } finally {
      db?.close();
    }
  }
  return null;
}

function decodeItem(raw, itemType) {
  let parsed;
  try {
    parsed = typeof raw === "string" ? JSON.parse(raw) : raw;
  } catch {
    fail("Malformed item_json in selected history record");
  }
  if (!parsed || typeof parsed !== "object")
    fail("History item is not a JSON object");
  const texts = [];
  const visit = (value) => {
    if (typeof value === "string") return;
    if (!value || typeof value !== "object") return;
    if (typeof value.text === "string") texts.push(value.text);
    if (Array.isArray(value)) for (const item of value) visit(item);
    else
      for (const [key, item] of Object.entries(value))
        if (key !== "text") visit(item);
  };
  visit(parsed);
  return {
    item_type: itemType ?? parsed.type ?? null,
    text: [...new Set(texts)].join("\n"),
    item: parsed,
  };
}

async function* legacyItems(
  path,
  threadId,
  { after = 0, afterId, offset = 0 } = {},
) {
  if (!path || !existsSync(path))
    fail(`Legacy rollout record is missing for thread ${threadId}`);
  const input = createReadStream(path, { encoding: "utf8" });
  const lines = createInterface({ input, crlfDelay: Infinity });
  let index = 0;
  let skipped = 0;
  let groupOrdinal;
  let group = [];
  function* emitGroup(items) {
    items.sort((left, right) =>
      String(left.item_id).localeCompare(String(right.item_id)),
    );
    for (const item of items) {
      if (
        item.ordinal < after ||
        (item.ordinal === after &&
          (afterId === undefined ||
            String(item.item_id).localeCompare(afterId) <= 0))
      )
        continue;
      if (skipped < offset) {
        skipped += 1;
        continue;
      }
      yield item;
    }
  }
  try {
    for await (const line of lines) {
      index += 1;
      if (line.length === 0) continue;
      let record;
      try {
        record = JSON.parse(line);
      } catch {
        fail(`Malformed JSONL record at line ${index} in thread ${threadId}`);
      }
      const type = record.type ?? record.item_type ?? record.payload?.type;
      if (
        [
          "session_meta",
          "session_meta_block",
          "turn_context",
          "rollout_header",
        ].includes(type)
      )
        continue;
      const ordinal = Number.isSafeInteger(record.rollout_ordinal)
        ? record.rollout_ordinal
        : index;
      const itemId =
        record.item_id ??
        record.id ??
        `line-${String(index).padStart(12, "0")}`;
      if (group.length && ordinal !== groupOrdinal) {
        yield* emitGroup(group);
        group = [];
      }
      groupOrdinal = ordinal;
      group.push({
        ordinal,
        item_id: itemId,
        ...decodeItem(record.item ?? record.payload ?? record, type),
      });
    }
    yield* emitGroup(group);
  } finally {
    lines.close();
    input.destroy();
  }
}

async function readLegacy(
  path,
  threadId,
  limit,
  after = 0,
  afterId,
  offset = 0,
) {
  const items = [];
  for await (const item of legacyItems(path, threadId, {
    after,
    afterId,
    offset,
  })) {
    items.push(item);
    if (items.length >= limit) break;
  }
  return items;
}

function sqliteItems(
  db,
  schema,
  threadId,
  limit,
  after = 0,
  afterId,
  offset = 0,
) {
  const { columns } = schema;
  const order = columns.includes("rollout_ordinal")
    ? "rollout_ordinal"
    : columns.includes("ordinal")
      ? "ordinal"
      : "rowid";
  const identity = columns.includes("item_id") ? "item_id" : "rowid";
  const fields = [
    "item_json",
    columns.includes("item_type") ? "item_type" : "NULL AS item_type",
    `${order} AS stable_ordinal`,
    `${identity} AS stable_identity`,
  ];
  const tieBreak =
    afterId === undefined ? "" : ` OR (${order} = ? AND ${identity} > ?)`;
  const where = ` AND (${order} > ?${tieBreak})`;
  const stmt = db.query(
    `SELECT ${fields.join(", ")} FROM thread_items WHERE thread_id = ?${where} ORDER BY ${order}, ${identity} LIMIT ? OFFSET ?`,
  );
  const bindings =
    afterId === undefined
      ? [threadId, after, limit, offset]
      : [threadId, after, after, afterId, limit, offset];
  const rows = stmt.all(...bindings);
  return rows.map((row, index) => {
    const decoded = decodeItem(row.item_json, row.item_type);
    return {
      ordinal: Number(row.stable_ordinal) || index + 1,
      item_id: row.stable_identity ?? `ordinal-${index + 1}`,
      ...decoded,
    };
  });
}

async function readThread(
  thread,
  databases,
  limit,
  after = 0,
  afterId,
  offset = 0,
) {
  if (thread.history_mode === "legacy" || thread.history_mode === "jsonl") {
    return readLegacy(
      thread.rollout_path,
      thread.thread_id,
      limit,
      after,
      afterId,
      offset,
    );
  }
  const schema = historySchema(databases.histories, thread.thread_id);
  if (schema) {
    let db;
    try {
      db = new Database(schema.file, { readonly: true });
      const items = sqliteItems(
        db,
        schema,
        thread.thread_id,
        limit,
        after,
        afterId,
        offset,
      );
      return items;
    } finally {
      db?.close();
    }
  }
  if (
    thread.history_mode !== "paginated" &&
    thread.rollout_path &&
    existsSync(thread.rollout_path)
  )
    return readLegacy(
      thread.rollout_path,
      thread.thread_id,
      limit,
      after,
      afterId,
      offset,
    );
  fail(`No readable history records found for thread ${thread.thread_id}`);
}

async function* iterateThread(thread, databases) {
  const schema = historySchema(databases.histories, thread.thread_id);
  if (
    thread.history_mode === "legacy" ||
    thread.history_mode === "jsonl" ||
    (!schema && thread.history_mode !== "paginated" && thread.rollout_path)
  ) {
    yield* legacyItems(thread.rollout_path, thread.thread_id);
    return;
  }
  if (!schema)
    fail(`No readable history records found for thread ${thread.thread_id}`);
  let db;
  try {
    db = new Database(schema.file, { readonly: true });
    let offset = 0;
    while (true) {
      const page = sqliteItems(
        db,
        schema,
        thread.thread_id,
        PAGE_SIZE,
        0,
        undefined,
        offset,
      );
      yield* page;
      if (page.length < PAGE_SIZE) return;
      offset += page.length;
    }
  } finally {
    db?.close();
  }
}

function citation(thread, item) {
  return `${thread.thread_id}/${item.item_id}@${item.ordinal}`;
}

async function main(args) {
  if (args.command === "help") return printHelp();
  const root = dataRoot();
  if (!existsSync(root))
    fail(`Codex history directory does not exist: ${root}`);
  const databases = discoverDatabases(root);
  const limit = args.limit ?? DEFAULT_LIMIT;
  if (args.command === "list" || args.command === "last") {
    const threads = allThreads(databases.states, {
      cwd: args.cwd,
      since: args.since,
      limit: args.command === "list" ? limit : 1,
    });
    if (args.command === "last")
      return JSON.stringify(threads[0] ?? null, null, 2);
    return JSON.stringify(threads.slice(0, limit), null, 2);
  }
  if (args.command === "read") {
    const thread = allThreads(databases.states, { id: args.id })[0];
    if (!thread)
      fail(`Thread not found in compatible state databases: ${args.id}`);
    const items = await readThread(
      thread,
      databases,
      limit,
      args.after ?? 0,
      args["after-id"],
    );
    return JSON.stringify(
      items.map((item) => ({ citation: citation(thread, item), ...item })),
      null,
      2,
    );
  }
  const threads = allThreads(databases.states, {
    cwd: args.cwd,
    since: args.since,
  });
  const matches = [];
  const pattern = args.pattern.toLocaleLowerCase();
  for (const thread of threads) {
    for await (const item of iterateThread(thread, databases)) {
      if (item.text.toLocaleLowerCase().includes(pattern)) {
        matches.push({
          citation: citation(thread, item),
          thread_id: thread.thread_id,
          timestamp: thread.timestamp,
          item_type: item.item_type,
          excerpt: item.text.slice(0, 500),
        });
        if (matches.length >= limit) return JSON.stringify(matches, null, 2);
      }
    }
  }
  return JSON.stringify(matches, null, 2);
}

if (import.meta.main) {
  try {
    const args = parseArgs(Bun.argv.slice(2));
    const output = await main(args);
    process.stdout.write(`${output}\n`);
  } catch (error) {
    process.stderr.write(`pstack-history: ${error.message}\n`);
    process.exitCode = 1;
  }
}

export {
  dataRoot,
  decodeItem,
  discoverDatabases,
  historySchema,
  main,
  parseArgs,
  readLegacy,
  withinScope,
};
