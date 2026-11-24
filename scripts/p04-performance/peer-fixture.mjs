import { createHash } from "node:crypto";
import { copyFileSync, mkdirSync, mkdtempSync, readFileSync, readdirSync, statSync, writeFileSync } from "node:fs";
import { basename, isAbsolute, join } from "node:path";

const pig = process.env.P04_PIG_BIN;
const extension = process.env.P04_EXTENSION;
const evidence = process.env.P04_EVIDENCE_DIR;
const perSender = Number(process.env.P04_MESSAGES_PER_SENDER || "25");
const pollMs = Number(process.env.P04_POLL_MS || "2");
if (![pig, extension, evidence].every((value) => value && isAbsolute(value)) || !Number.isInteger(perSender) || perSender < 1 || perSender > 25 || !Number.isInteger(pollMs) || pollMs < 1 || pollMs > 20) {
  throw new Error("Set absolute binary and evidence paths, 1 to 25 messages per sender, and a 1 to 20 ms polling interval.");
}
mkdirSync(evidence, { recursive: true, mode: 0o700 });
const base = mkdtempSync("/tmp/p04-peer-perf-");
const baseStat = statSync(base);
const baseIdentity = `${baseStat.dev}:${baseStat.ino}:${baseStat.uid}`;
const agent = join(base, "agent");
const workspace = join(base, "workspace");
const rootSessions = join(base, "root-sessions");
for (const dir of [agent, workspace, rootSessions, ...["config", "data", "state", "runtime", "cache"].map((name) => join(base, name)), join(evidence, "receipts"), join(evidence, "bodies"), join(evidence, "sessions")]) {
  mkdirSync(dir, { recursive: true, mode: 0o700 });
}
writeFileSync(join(workspace, "litter.yaml"), "concurrency: 8\ndepth: 2\n", { mode: 0o600 });
const names = ["alpha", "beta", "gamma", "delta"];
const state = { members: [], samples: [], providerCounts: { lead: 0, alpha: 0, beta: 0, gamma: 0, delta: 0 }, events: [], failures: [], outcome: "inconclusive" };
const requests = [];
let sequence = 0;
let activeBatch = null;
let activeSample = null;
const now = () => Number(process.hrtime.bigint());
const same = (a, b) => JSON.stringify(a) === JSON.stringify(b);
const hash = (bytes) => createHash("sha256").update(bytes).digest("hex");
const hashFile = (path) => hash(readFileSync(path));
function event(kind, details = {}) { state.events.push({ kind, monotonic_ns: now(), ...details }); }
function check(condition, message) { if (!condition) throw new Error(message); }
function deferred() {
  let resolve;
  const promise = new Promise((done) => { resolve = done; });
  return { promise, resolve };
}
function response(model, call, text = "") {
  const id = `p04-peer-perf-${++sequence}`;
  const delta = call ? { role: "assistant", tool_calls: [{ index: 0, id, type: "function", function: { name: call.name, arguments: JSON.stringify(call.args) } }] } : { role: "assistant", content: text };
  const base = { id, object: "chat.completion.chunk", created: 1, model };
  return new Response(`data: ${JSON.stringify({ ...base, choices: [{ index: 0, delta, finish_reason: null }] })}\n\ndata: ${JSON.stringify({ ...base, choices: [{ index: 0, delta: {}, finish_reason: call ? "tool_calls" : "stop" }], usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 } })}\n\ndata: [DONE]\n\n`, { headers: { "content-type": "text/event-stream" } });
}
function toolResult(message) {
  const content = Array.isArray(message.content) ? message.content.map((item) => item.text || "").join("") : message.content;
  return JSON.parse(content);
}
function filesUnder(directory) {
  try {
    return readdirSync(directory, { withFileTypes: true }).flatMap((entry) => {
      const path = join(directory, entry.name);
      return entry.isDirectory() ? filesUnder(path) : entry.isFile() ? [path] : [];
    });
  } catch (error) {
    if (error.code === "ENOENT") return [];
    throw error;
  }
}
function readMaybe(path) {
  try { return readFileSync(path); } catch (error) { if (error.code === "ENOENT") return null; throw error; }
}
async function waitFor(predicate, description, timeoutMs = 20000) {
  const deadline = now() + timeoutMs * 1e6;
  while (now() < deadline) {
    const value = predicate();
    if (value) return value;
    await Bun.sleep(pollMs);
  }
  throw new Error(`Timed out waiting for ${description}.`);
}
function rosterPath() { return join(agent, "pig-litter", "roots", state.members[0].ref.team.root, "team.json"); }
function roster() { return JSON.parse(readFileSync(rosterPath(), "utf8")); }
async function allIdle() {
  await waitFor(() => {
    const current = roster();
    return state.members.length === 4 && state.members.every((member) => current.members.some((item) => same(item.ref, member.ref) && item.state === "idle"));
  }, "all four teammates to be idle");
}
function sessionPath(member) {
  const found = filesUnder(join(agent, "sessions")).filter((path) => basename(path).endsWith(`_${member.ref.id}.jsonl`));
  check(found.length === 1, `Session path count for ${member.name} is ${found.length}.`);
  return found[0];
}
function expectedPaths(sender, recipient, messageID) {
  const key = hash(Buffer.from(JSON.stringify({ team: sender.ref.team, sender: sender.ref, recipient: recipient.ref, messageId: messageID })));
  const directory = join(agent, "pig-litter", "roots", sender.ref.team.root);
  return { receipt: join(directory, `peer-receipt-${key}.json`), body: join(directory, `peer-body-${key}.json`) };
}
async function observe(sample) {
  const deadline = now() + 20000e6;
  let lastReceiptAbsent = sample.start_ns;
  let lastNativeAbsent = sample.start_ns;
  let finalReceipt;
  while (now() < deadline) {
    const receiptPollStart = now();
    const receiptBytes = readMaybe(sample.paths.receipt);
    if (receiptBytes) {
      const receipt = JSON.parse(receiptBytes);
      check(receipt.envelope.messageId === sample.message_id && same(receipt.envelope.sender, sample.sender.ref) && same(receipt.envelope.recipient, sample.recipient.ref), "Receipt identity differs.");
      if (!sample.receipt_visible_ns) {
        sample.receipt_visible_ns = now();
        sample.receipt_previous_absent_ns = lastReceiptAbsent;
        sample.receipt_visible_state = receipt.state;
      }
      if (receipt.state === "native_observed") {
        finalReceipt = receipt;
        if (!sample.receipt_observed_ns) sample.receipt_observed_ns = now();
      }
    } else if (!sample.receipt_visible_ns) {
      lastReceiptAbsent = receiptPollStart;
    }
    const nativePollStart = now();
    const bytes = readMaybe(sample.recipient.session);
    if (bytes) {
      const text = bytes.toString("utf8");
      const lines = text.split("\n");
      if (!text.endsWith("\n")) lines.pop();
      const entries = lines.filter(Boolean).map(JSON.parse);
      check(entries[0]?.type === "session" && entries[0]?.id === sample.recipient.ref.id, "Recipient session identity differs.");
      const matches = entries.filter((entry) => entry.type === "custom_message" && entry.customType === "pig_litter_peer" && entry.details?.envelope?.messageId === sample.message_id);
      check(matches.length <= 1, "The recipient transcript contains a duplicate peer message.");
      if (matches.length && !sample.native_visible_ns) {
        const entry = matches[0];
        const content = JSON.parse(entry.content);
        check(same(entry.details.envelope.sender, sample.sender.ref) && same(entry.details.envelope.recipient, sample.recipient.ref) && content.body.text === sample.marker && content.senderName === sample.sender.name, "The native peer entry differs.");
        sample.native_visible_ns = now();
        sample.native_previous_absent_ns = lastNativeAbsent;
        sample.native_entry_id = entry.id;
      } else if (!matches.length && !sample.native_visible_ns) {
        lastNativeAbsent = nativePollStart;
      }
    }
    if (sample.native_visible_ns && finalReceipt && sample.wake_ns) {
      check(finalReceipt.sessionId === sample.recipient.ref.id && finalReceipt.nativeEntryId === sample.native_entry_id, "Native receipt reconciliation differs.");
      const body = readFileSync(sample.paths.body);
      check(hash(body) === finalReceipt.envelope.bodySha256 && same(JSON.parse(body), { text: sample.marker, summary: "P04 performance fixture" }), "The retained peer body differs.");
      writeFileSync(join(evidence, "receipts", `${sample.message_id}.json`), JSON.stringify(finalReceipt, null, 2));
      writeFileSync(join(evidence, "bodies", `${sample.message_id}.json`), body);
      sample.receipt = finalReceipt;
      return;
    }
    await Bun.sleep(pollMs);
  }
  throw new Error(`Timed out observing ${sample.message_id}.`);
}
function beginSample(batch, requestIndex) {
  const index = state.samples.length;
  const messageID = `p04-perf-${String(index).padStart(3, "0")}`;
  const marker = `P04_PEER_${String(index).padStart(3, "0")}`;
  const beforeRoster = roster();
  const recipientBefore = beforeRoster.members.find((member) => same(member.ref, batch.recipient.ref));
  check(recipientBefore?.state === "idle", "The recipient was not idle before the send.");
  const sample = { index, message_id: messageID, marker, sender: batch.sender, recipient: batch.recipient, recipient_before: recipientBefore, start_request_index: requestIndex, start_ns: now(), lead_before: state.providerCounts.lead, paths: expectedPaths(batch.sender, batch.recipient, messageID) };
  state.samples.push(sample);
  activeSample = sample;
  batch.pending = sample;
  sample.observation = observe(sample);
  return response(batch.sender.name, { name: "team_send", args: { recipient: batch.recipient.name, messageId: messageID, text: marker, summary: "P04 performance fixture" } });
}
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  idleTimeout: 60,
  async fetch(request) {
    const receivedNS = now();
    try {
      const body = await request.json();
      const model = body.model;
      check(Object.hasOwn(state.providerCounts, model), `Unexpected fixture model ${model}.`);
      state.providerCounts[model]++;
      const requestIndex = requests.length;
      requests.push(body);
      if (model !== "lead") {
        if (activeSample && activeSample.recipient.name === model && JSON.stringify(body.messages).includes(activeSample.marker)) {
          check(!activeSample.wake_ns, "Recipient made duplicate wake requests for a sample.");
          activeSample.wake_ns = receivedNS;
          activeSample.wake_request_index = requestIndex;
          return response(model, null, "P04_PEER_RECEIVED");
        }
        if (!activeBatch || activeBatch.sender.name !== model) return response(model, null, "P04_MEMBER_READY");
        const batch = activeBatch;
        await batch.start.promise;
        if (batch.pending) {
          const sample = batch.pending;
          sample.result_ns = receivedNS;
          sample.result_request_index = requestIndex;
          const toolMessages = body.messages.filter((message) => message.role === "tool");
          sample.result = toolMessages.length ? toolResult(toolMessages.at(-1)) : null;
          sample.result_error = toolMessages.at(-1)?.is_error ?? null;
          check(sample.result_error !== true && sample.result?.operation === "team_send" && sample.result.state === "accepted" && sample.result.messageId === sample.message_id, "Sender did not receive the accepted peer result.");
          await sample.observation;
          delete sample.observation;
          sample.lead_after = state.providerCounts.lead;
          sample.end_request_index = requests.length - 1;
          check(sample.lead_after === sample.lead_before, "Lead inference count changed during peer delivery.");
          batch.pending = null;
          activeSample = null;
          batch.completed++;
          if (batch.completed === perSender) {
            batch.finished.resolve();
            return response(model, null, "P04_BATCH_FINISHED");
          }
          await waitFor(() => roster().members.some((member) => same(member.ref, batch.recipient.ref) && member.state === "idle"), "recipient to return to idle");
        }
        return beginSample(batch, requestIndex);
      }
      const tools = body.messages.filter((message) => message.role === "tool");
      const latest = tools.length ? toolResult(tools.at(-1)) : null;
      if (state.members.length < 4) {
        if (state.members.length > 0 || latest) {
          check(latest?.operation === "team_spawn" && latest.member?.id, `Teammate spawn failed: ${JSON.stringify(latest)}`);
          const name = names[state.members.length];
          state.members.push({ name, ref: latest.member });
        }
        if (state.members.length < 4) {
          const name = names[state.members.length];
          return response(model, { name: "team_spawn", args: { type: "worker", name, task: "Participate in the P04 measurement fixture.", model: `fixture/${name}`, workspace: { kind: "shared_checkout" } } });
        }
        check(new Set(state.members.map((member) => member.ref.id)).size === 4 && state.members.every((member) => same(member.ref.team, state.members[0].ref.team)), "Four distinct teammates were not admitted.");
        await allIdle();
        for (const member of state.members) member.session = sessionPath(member);
      } else if (activeBatch) {
        check(latest?.operation === "litter_message" && latest.delivery === "resumed", `Sender resume failed: ${JSON.stringify(latest)}`);
        const batch = activeBatch;
        event("batch-start", { sender: batch.sender.name, recipient: batch.recipient.name });
        batch.start.resolve();
        await batch.finished.promise;
        event("batch-finish", { sender: batch.sender.name, recipient: batch.recipient.name });
        activeBatch = null;
        await allIdle();
      }
      const batchIndex = state.samples.length / perSender;
      if (batchIndex === 4) {
        check(state.samples.length === perSender * 4, "Peer sample count differs.");
        state.outcome = "observed";
        return response(model, null, "P04_PEER_FIXTURE_FINISHED");
      }
      check(Number.isInteger(batchIndex), "A peer batch is incomplete.");
      activeBatch = { sender: state.members[batchIndex], recipient: state.members[(batchIndex + 1) % 4], start: deferred(), finished: deferred(), completed: 0, pending: null };
      return response(model, { name: "litter_message", args: { id: activeBatch.sender.ref.id, text: `P04_START_BATCH_${batchIndex}`, resume: true } });
    } catch (error) {
      state.failures.push(String(error));
      activeBatch?.start.resolve();
      activeBatch?.finished.resolve();
      return new Response(String(error), { status: 500 });
    }
  },
});
const models = ["lead", ...names].map((id) => ({ id, name: id, contextWindow: 1000000, maxTokens: 20000 }));
writeFileSync(join(agent, "models.json"), JSON.stringify({ providers: { fixture: { api: "openai-completions", baseUrl: `http://127.0.0.1:${server.port}/v1`, apiKey: "local-fixture-only", models } } }), { mode: 0o600 });
writeFileSync(join(agent, "auth.json"), JSON.stringify({ fixture: { type: "api_key", key: "local-fixture-only" } }), { mode: 0o600 });
writeFileSync(join(agent, "settings.json"), JSON.stringify({ cacheWarming: "off", retry: { enabled: false }, compaction: { enabled: false } }), { mode: 0o600 });
const argv = [pig, "--no-extensions", "-e", extension, "--no-skills", "--no-prompt-templates", "--no-themes", "--no-context-files", "--session-dir", rootSessions, "--approve", "--offline", "--model", "fixture/lead", "--mode", "json", "--print", "Run the P04 peer performance fixture."];
const env = { PATH: process.env.PATH, LANG: "C.UTF-8", HOME: base, TMPDIR: base, XDG_CONFIG_HOME: join(base, "config"), XDG_DATA_HOME: join(base, "data"), XDG_STATE_HOME: join(base, "state"), XDG_RUNTIME_DIR: join(base, "runtime"), XDG_CACHE_HOME: join(base, "cache"), PIG_HOME: base, PIG_CODING_AGENT_DIR: agent, PIG_USE_PI_DIRS: "0", PIG_OFFLINE: "1", PI_OFFLINE: "1" };
const child = Bun.spawn(argv, { cwd: workspace, env, stdout: "pipe", stderr: "pipe" });
const timeout = setTimeout(() => { state.failures.push("Host timed out."); activeBatch?.start.resolve(); activeBatch?.finished.resolve(); child.kill("SIGTERM"); }, 600000);
const [stdout, stderr, exitCode] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited]);
clearTimeout(timeout);
await server.stop(true);
for (const member of state.members) if (member.session) copyFileSync(member.session, join(evidence, "sessions", `${member.name}.jsonl`));
if (state.members.length) copyFileSync(rosterPath(), join(evidence, "team-final.json"));
const rootSessionFiles = filesUnder(rootSessions).filter((path) => path.endsWith(".jsonl"));
if (rootSessionFiles.length === 1) copyFileSync(rootSessionFiles[0], join(evidence, "root-session.jsonl"));
else state.failures.push(`Root session path count is ${rootSessionFiles.length}.`);
for (const sample of state.samples) delete sample.observation;
writeFileSync(join(evidence, "stdout.jsonl"), stdout, { mode: 0o600 });
writeFileSync(join(evidence, "stderr.log"), stderr, { mode: 0o600 });
writeFileSync(join(evidence, "requests.json"), JSON.stringify(requests), { mode: 0o600 });
writeFileSync(join(evidence, "result.json"), JSON.stringify({ base, baseIdentity, argv, pid: child.pid, exitCode, poll_ms: pollMs, per_sender: perSender, pigSHA256: hashFile(pig), extensionSHA256: hashFile(extension), state }, null, 2), { mode: 0o600 });
console.log(JSON.stringify({ outcome: state.outcome, exitCode, failures: state.failures, samples: state.samples.length, providerCounts: state.providerCounts }));
if (exitCode !== 0 || state.failures.length || state.outcome !== "observed") process.exitCode = 1;
