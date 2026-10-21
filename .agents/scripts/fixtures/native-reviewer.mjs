import { Database } from "bun:sqlite";
import {
  appendFileSync,
  mkdirSync,
  readFileSync,
  renameSync,
  writeFileSync,
} from "node:fs";
import { join } from "node:path";
import { randomUUID } from "node:crypto";
import { createInterface } from "node:readline";
import { fileURLToPath } from "node:url";

const args = process.argv.slice(2);
if (args[0] === "restore-history") {
  const saved = JSON.parse(readFileSync(args[1], "utf8"));
  await Bun.sleep(50);
  if (saved.substituteReport)
    writeFileSync(saved.substituteReport, "SUBSTITUTED_AFTER_READ");
  await Bun.sleep(150);
  const db = new Database(saved.database);
  db.exec("PRAGMA busy_timeout=5000");
  renameSync(saved.pending, saved.path);
  db.query("INSERT INTO threads VALUES (?,?,?,?,?,?,?,?)").run(...saved.row);
  db.close();
  process.exit(0);
}
const scenario = process.env.PSTACK_FIXTURE_SCENARIO || "normal";
const record = (value) =>
  appendFileSync(
    process.env.PSTACK_FIXTURE_RECORD,
    `${JSON.stringify(value)}\n`,
  );
if (args[0] === "app-server") {
  if (scenario === "stubborn-preflight") {
    process.on("SIGTERM", () => {});
    writeFileSync(
      join(process.env.CODEX_HOME, "preflight.pid"),
      String(process.pid),
    );
  }
  const lines = createInterface({ input: process.stdin });
  for await (const line of lines) {
    const request = JSON.parse(line);
    record({ method: request.method, params: request.params });
    if (scenario === "stubborn-preflight") continue;
    if (request.method === "initialize")
      console.log(JSON.stringify({ id: request.id, result: {} }));
    if (request.method === "command/exec")
      console.log(
        JSON.stringify({
          id: request.id,
          result: {
            exitCode: scenario === "preflight-failure" ? 5 : 0,
            stdout: scenario === "preflight-failure" ? "" : "toolchain ready\n",
            stderr: scenario === "preflight-failure" ? "Go is unavailable" : "",
          },
        }),
      );
  }
  process.exit(0);
}
const prompt = await Bun.stdin.text();
record({ method: "review", args, prompt });
if (scenario === "exit-failure") process.exit(7);
if (scenario === "stale") process.exit(0);
const cwd = args[args.indexOf("-C") + 1];
const output = args[args.indexOf("--output-last-message") + 1];
const model = args[args.indexOf("--model") + 1];
const effortArg = args.find((arg) => arg.startsWith("model_reasoning_effort="));
const effort = effortArg ? JSON.parse(effortArg.split("=")[1]) : "high";
const id = randomUUID();
const home = process.env.CODEX_HOME;
mkdirSync(home, { recursive: true });
const db = new Database(join(home, "state_99.sqlite"));
db.exec(
  "PRAGMA busy_timeout=5000; CREATE TABLE IF NOT EXISTS threads (id TEXT PRIMARY KEY,cwd TEXT,rollout_path TEXT,history_mode TEXT,created_at_ms INTEGER,updated_at_ms INTEGER,source TEXT,thread_source TEXT)",
);
const insert = db.query("INSERT INTO threads VALUES (?,?,?,?,?,?,?,?)");
const started = Date.now();
const final = scenario === "mismatch" ? "canonical finding" : "one finding";
function persist(
  threadId,
  source,
  threadSource,
  text,
  threadModel = model,
  threadCwd = cwd,
) {
  const turnId = randomUUID();
  const path = join(home, `rollout-${threadId}.jsonl`);
  const wrap = (type, payload) => ({
    timestamp: new Date().toISOString(),
    type,
    payload,
  });
  const records = [
    wrap("session_meta", {
      id: threadId,
      timestamp: new Date(started).toISOString(),
      cwd: threadCwd,
      source,
      thread_source: threadSource,
      originator: "codex_exec",
    }),
    wrap("event_msg", {
      type: "task_started",
      turn_id: turnId,
      started_at: new Date(started).toISOString(),
    }),
    wrap("turn_context", {
      turn_id: turnId,
      cwd: threadCwd,
      model: threadModel,
      effort,
      sandbox_policy: { type: "read-only" },
    }),
    wrap("response_item", {
      type: "function_call",
      name: "exec_command",
      call_id: "call-1",
      arguments: '{"cmd":"PRIVATE_ARGUMENT"}',
    }),
    wrap("response_item", {
      type: "function_call_output",
      call_id: "call-1",
      output: "PRIVATE_OUTPUT",
    }),
    wrap("response_item", {
      type: "message",
      id: `${threadId}-final`,
      role: "assistant",
      phase: "final_answer",
      content: [{ type: "output_text", text }],
    }),
    wrap("event_msg", {
      type: "task_complete",
      turn_id: turnId,
      started_at: new Date(started).toISOString(),
      completed_at: new Date().toISOString(),
      last_agent_message: text,
    }),
  ];
  if (scenario === "missing-context" && threadId === id) records.splice(2, 1);
  if (scenario === "incomplete" && threadId === id) records.pop();
  if (scenario === "changed-model" && threadId === id)
    records.splice(
      5,
      0,
      wrap("turn_context", {
        turn_id: turnId,
        model: "forbidden-model",
        effort,
        sandbox_policy: { type: "read-only" },
      }),
    );
  if (scenario === "tied-ordinals")
    records.forEach((r) => {
      r.rollout_ordinal = 1;
    });
  if (scenario === "numeric-time")
    records.forEach((r) => {
      if (r.payload.started_at)
        r.payload.started_at = Math.floor(started / 1000);
      if (r.payload.completed_at)
        r.payload.completed_at = Math.floor(Date.now() / 1000);
    });
  if (scenario === "spawn-reference" && threadId === id)
    records.splice(
      5,
      0,
      wrap("response_item", {
        type: "function_call",
        name: "spawn_agent",
        call_id: "spawn-1",
        arguments: "{}",
      }),
      wrap("response_item", {
        type: "function_call_output",
        call_id: "spawn-1",
        output: JSON.stringify({ agent_id: randomUUID() }),
      }),
    );
  const encoded = records.map(JSON.stringify).join("\n") + "\n";
  writeFileSync(
    path,
    scenario === "malformed" && threadId === id
      ? encoded + "{broken\n"
      : encoded,
  );
  if (scenario !== "unindexed-child" || threadId === id)
    insert.run(
      threadId,
      threadCwd,
      path,
      "jsonl",
      started,
      Date.now(),
      JSON.stringify(source),
      threadSource,
    );
  return turnId;
}
if (scenario === "hold") await Bun.sleep(500);
console.log(JSON.stringify({ type: "thread.started", thread_id: id }));
if (scenario !== "missing-history") {
  persist(id, "exec", "user", final);
  if (["hidden-child", "unindexed-child"].includes(scenario))
    persist(
      randomUUID(),
      {
        subagent: {
          thread_spawn: {
            parent_thread_id: id,
            depth: 1,
            agent_path: "/root/child",
          },
        },
      },
      "subagent",
      "child",
      "forbidden-model",
      join(cwd, "..", "other-worktree"),
    );
  if (scenario === "guardian")
    persist(
      randomUUID(),
      { subagent: { other: "guardian" } },
      "guardian_review",
      "guardian",
      "forbidden-model",
    );
  if (scenario === "large-inventory") {
    for (let i = 0; i < 110; i++)
      persist(randomUUID(), "exec", "user", "unrelated");
    persist(
      randomUUID(),
      { subagent: { thread_spawn: { parent_thread_id: id, depth: 1 } } },
      "subagent",
      "hidden child",
    );
  }
  if (scenario === "corrupt-source")
    writeFileSync(join(home, "state_100.sqlite"), "not a database");
  if (scenario === "unknown-lineage")
    persist(
      randomUUID(),
      { subagent: { other: "unknown" } },
      "subagent",
      "unknown child",
    );
  if (["late-history", "late-report-substitution"].includes(scenario)) {
    const row = db.query("SELECT * FROM threads WHERE id=?").get(id);
    db.query("DELETE FROM threads WHERE id=?").run(id);
    const path = row.rollout_path;
    const pending = path + ".pending";
    renameSync(path, pending);
    const restore = join(home, "restore.json");
    writeFileSync(
      restore,
      JSON.stringify({
        database: join(home, "state_99.sqlite"),
        path,
        pending,
        substituteReport:
          scenario === "late-report-substitution" ? output : undefined,
        row: [
          row.id,
          row.cwd,
          row.rollout_path,
          row.history_mode,
          row.created_at_ms,
          row.updated_at_ms,
          row.source,
          row.thread_source,
        ],
      }),
    );
    Bun.spawn(
      [
        process.execPath,
        fileURLToPath(import.meta.url),
        "restore-history",
        restore,
      ],
      { stdin: "ignore", stdout: "ignore", stderr: "ignore" },
    ).unref();
  }
}
db.close();
writeFileSync(
  output,
  scenario === "mismatch" ? "substituted finding" : "one finding",
);
console.log(JSON.stringify({ type: "turn.completed" }));
