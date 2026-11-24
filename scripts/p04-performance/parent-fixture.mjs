import { createHash } from "node:crypto";
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { isAbsolute, join } from "node:path";

const pig = process.env.P04_PIG_BIN;
const extension = process.env.P04_EXTENSION;
const evidence = process.env.P04_EVIDENCE_DIR;
if (![pig, extension, evidence].every((value) => value && isAbsolute(value))) {
  throw new Error("P04_PIG_BIN, P04_EXTENSION, and P04_EVIDENCE_DIR must be absolute paths.");
}
mkdirSync(evidence, { recursive: true, mode: 0o700 });
const base = mkdtempSync("/tmp/p04-parent-");
const baseStat = statSync(base);
const baseIdentity = `${baseStat.dev}:${baseStat.ino}:${baseStat.uid}`;
const agent = join(base, "agent");
const workspace = join(base, "workspace");
const rootSessions = join(base, "root-sessions");
for (const dir of [agent, workspace, rootSessions, "config", "data", "state", "runtime", "cache"].map((value) => isAbsolute(value) ? value : join(base, value))) {
  mkdirSync(dir, { recursive: true, mode: 0o700 });
}
writeFileSync(join(workspace, "litter.yaml"), "concurrency: 4\ndepth: 2\n", { mode: 0o600 });
const state = { leadStep: 0, childRequests: 0, leadRequests: 0, childID: null, samples: [], events: [], failures: [], observed: [], outcome: "inconclusive" };
const requests = [];
let sequence = 0;
let readyResolve;
let releaseResolve;
const ready = new Promise((resolve) => { readyResolve = resolve; });
const release = new Promise((resolve) => { releaseResolve = resolve; });
const now = () => Number(process.hrtime.bigint());
const record = (kind, details = {}) => state.events.push({ kind, monotonic_ns: now(), ...details });
function hash(path) { return createHash("sha256").update(readFileSync(path)).digest("hex"); }
function response(model, call, text = "") {
  const id = `p04-parent-${++sequence}`;
  const delta = call ? { role: "assistant", tool_calls: [{ index: 0, id, type: "function", function: { name: call.name, arguments: JSON.stringify(call.args) } }] } : { role: "assistant", content: text };
  const base = { id, object: "chat.completion.chunk", created: 1, model };
  return new Response(`data: ${JSON.stringify({ ...base, choices: [{ index: 0, delta, finish_reason: null }] })}\n\ndata: ${JSON.stringify({ ...base, choices: [{ index: 0, delta: {}, finish_reason: call ? "tool_calls" : "stop" }], usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 } })}\n\ndata: [DONE]\n\n`, { headers: { "content-type": "text/event-stream" } });
}
function toolResult(message) {
  const content = Array.isArray(message.content) ? message.content.map((item) => item.text || "").join("") : message.content;
  return JSON.parse(content);
}
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  idleTimeout: 60,
  async fetch(request) {
    try {
      const body = await request.json();
      const model = body.model;
      requests.push(body);
      if (model === "child") {
        state.childRequests++;
        const messages = JSON.stringify(body.messages);
        for (const marker of ["P04_PARENT_STEER", "P04_PARENT_FOLLOW"]) {
          if (messages.includes(marker) && !state.observed.includes(marker)) {
            state.observed.push(marker);
            record("child-provider-marker", { marker });
          }
        }
        if (state.childRequests === 1) {
          record("child-provider-held");
          readyResolve();
          await release;
        }
        return response(model, null, "P04_PARENT_CHILD_FINISHED");
      }
      if (model !== "lead") throw new Error(`Unexpected model ${model}.`);
      state.leadRequests++;
      const messages = body.messages.filter((message) => message.role === "tool");
      const latest = messages.length ? toolResult(messages.at(-1)) : null;
      if (state.leadStep === 0) {
        state.leadStep = 1;
        return response(model, { name: "litter_spawn", args: { type: "scout", name: "parent-message-child", task: "Wait for messages.", model: "fixture/child" } });
      }
      if (state.leadStep === 1) {
        if (latest?.operation !== "litter_spawn" || !latest.child?.id) throw new Error(`Spawn failed: ${JSON.stringify(latest)}`);
        state.childID = latest.child.id;
        await ready;
        state.leadStep = 2;
        const sample = { mode: "steer", marker: "P04_PARENT_STEER", start_ns: now() };
        state.samples.push(sample);
        return response(model, { name: "litter_message", args: { id: state.childID, text: sample.marker, mode: sample.mode } });
      }
      if (state.leadStep === 2 || state.leadStep === 3) {
        const sample = state.samples.at(-1);
        sample.result_ns = now();
        sample.result = latest;
        sample.error = messages.at(-1)?.is_error ?? null;
        if (sample.error === true || latest?.operation !== "litter_message" || latest.state !== "running") throw new Error(`Parent message failed: ${JSON.stringify(latest)}`);
        if (state.leadStep === 2) {
          state.leadStep = 3;
          const next = { mode: "follow_up", marker: "P04_PARENT_FOLLOW", start_ns: now() };
          state.samples.push(next);
          return response(model, { name: "litter_message", args: { id: state.childID, text: next.marker, mode: next.mode } });
        }
        state.leadStep = 4;
        releaseResolve();
        return response(model, { name: "litter_wait", args: { id: state.childID, timeoutMs: 10000 } });
      }
      if (state.leadStep === 4) {
        if (latest?.operation !== "litter_wait" || latest.state !== "completed") throw new Error(`Wait failed: ${JSON.stringify(latest)}`);
        if (state.observed.join(",") !== "P04_PARENT_STEER,P04_PARENT_FOLLOW") throw new Error(`Child did not receive both messages in order: ${state.observed}`);
        state.outcome = "observed";
        return response(model, null, "P04_PARENT_FIXTURE_FINISHED");
      }
      throw new Error(`Unexpected lead step ${state.leadStep}.`);
    } catch (error) {
      state.failures.push(String(error));
      releaseResolve();
      return new Response(String(error), { status: 500 });
    }
  },
});
const models = ["lead", "child"].map((id) => ({ id, name: id, contextWindow: 100000, maxTokens: 20000 }));
writeFileSync(join(agent, "models.json"), JSON.stringify({ providers: { fixture: { api: "openai-completions", baseUrl: `http://127.0.0.1:${server.port}/v1`, apiKey: "local-fixture-only", models } } }), { mode: 0o600 });
writeFileSync(join(agent, "auth.json"), JSON.stringify({ fixture: { type: "api_key", key: "local-fixture-only" } }), { mode: 0o600 });
writeFileSync(join(agent, "settings.json"), JSON.stringify({ cacheWarming: "off", retry: { enabled: false }, compaction: { enabled: false } }), { mode: 0o600 });
const argv = [pig, "--no-extensions", "-e", extension, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--session-dir", rootSessions, "--approve", "--offline", "--model", "fixture/lead", "--mode", "json", "--print", "Run the parent-message fixture."];
const env = { PATH: process.env.PATH, LANG: "C.UTF-8", HOME: base, TMPDIR: base, XDG_CONFIG_HOME: join(base, "config"), XDG_DATA_HOME: join(base, "data"), XDG_STATE_HOME: join(base, "state"), XDG_RUNTIME_DIR: join(base, "runtime"), XDG_CACHE_HOME: join(base, "cache"), PIG_HOME: base, PIG_CODING_AGENT_DIR: agent, PIG_USE_PI_DIRS: "0", PIG_OFFLINE: "1", PI_OFFLINE: "1" };
const child = Bun.spawn(argv, { cwd: workspace, env, stdout: "pipe", stderr: "pipe" });
const timeout = setTimeout(() => { state.failures.push("Host timed out."); releaseResolve(); child.kill("SIGTERM"); }, 60000);
const [stdout, stderr, exitCode] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited]);
clearTimeout(timeout);
await server.stop(true);
function filesUnder(directory) {
  try { return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => entry.isDirectory() ? filesUnder(join(directory, entry.name)) : entry.isFile() ? [join(directory, entry.name)] : []); }
  catch (error) { if (error.code === "ENOENT") return []; throw error; }
}
const childSessions = filesUnder(join(agent, "sessions")).filter((path) => path.endsWith(`_${state.childID}.jsonl`));
if (childSessions.length === 1) copyFileSync(childSessions[0], join(evidence, "child-session.jsonl"));
else state.failures.push(`Child session path count is ${childSessions.length}.`);
const rootSessionFiles = filesUnder(rootSessions).filter((path) => path.endsWith(".jsonl"));
if (rootSessionFiles.length === 1) copyFileSync(rootSessionFiles[0], join(evidence, "root-session.jsonl"));
else state.failures.push(`Root session path count is ${rootSessionFiles.length}.`);
writeFileSync(join(evidence, "stdout.jsonl"), stdout, { mode: 0o600 });
writeFileSync(join(evidence, "stderr.log"), stderr, { mode: 0o600 });
writeFileSync(join(evidence, "requests.json"), JSON.stringify(requests, null, 2), { mode: 0o600 });
writeFileSync(join(evidence, "result.json"), JSON.stringify({ base, baseIdentity, argv, pid: child.pid, exitCode, pigSHA256: hash(pig), extensionSHA256: hash(extension), state }, null, 2), { mode: 0o600 });
console.log(JSON.stringify({ outcome: state.outcome, exitCode, failures: state.failures }));
if (exitCode !== 0 || state.failures.length || state.outcome !== "observed") process.exitCode = 1;
