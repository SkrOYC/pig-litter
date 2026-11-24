import { createHash } from "node:crypto";
import { readFile, mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";

const baseline = "941699052cf51a8648335674534b0b59e2611fac";
const candidate = process.env.P05_PERF_CANDIDATE;
const output = process.env.P05_PERF_OUTPUT;
const driver = process.env.P05_PERF_DRIVER;
const pig = process.env.P05_PERF_PIG;
const baselineBinary = process.env.P05_PERF_BASELINE;
const candidateBinary = process.env.P05_PERF_CANDIDATE_BINARY;
if (![candidate, output, driver, pig, baselineBinary, candidateBinary].every(Boolean)) {
  throw new Error("P05 performance paths and candidate identity are required");
}

async function run(args) {
  const child = Bun.spawn(args, { stdout: "pipe", stderr: "pipe" });
  const [stdout, stderr, code] = await Promise.all([
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
    child.exited,
  ]);
  if (code !== 0) throw new Error(`${args[0]} exited ${code}: ${stderr}`);
  return stdout.trim();
}

async function digest(path) {
  return createHash("sha256").update(await readFile(path)).digest("hex");
}

function percentile(values, fraction) {
  const ordered = [...values].sort((a, b) => a - b);
  return ordered[Math.ceil(fraction * ordered.length) - 1];
}

await mkdir(output, { recursive: true, mode: 0o700 });
const current = await run(["git", "rev-parse", "HEAD"]);
const status = await run(["git", "status", "--porcelain=v1", "--untracked-files=all"]);
if (current !== candidate || status !== "") throw new Error("candidate checkout is not clean at the requested commit");
await run(["git", "merge-base", "--is-ancestor", baseline, candidate]);
const identities = {
  baseline,
  candidate,
  baseline_binary_sha256: await digest(baselineBinary),
  candidate_binary_sha256: await digest(candidateBinary),
  driver_sha256: await digest(driver),
  pig_sha256: await digest(pig),
};
const schedule = [{ side: "baseline", phase: "baseline-first", pair: 0 }];
for (let pair = 1; pair <= 2; pair++) {
  for (const side of ["baseline", "candidate"]) schedule.push({ side, phase: "warmup", pair });
}
for (let pair = 1; pair <= 20; pair++) {
  for (const side of pair % 2 ? ["baseline", "candidate"] : ["candidate", "baseline"]) {
    schedule.push({ side, phase: "measured", pair });
  }
}
const samples = [];
for (const slot of schedule) {
  const binary = slot.side === "baseline" ? baselineBinary : candidateBinary;
  const started = process.hrtime.bigint();
  const result = JSON.parse(await run([driver, "--pig-bin", pig, "--extension", binary, "--scenario", "p05-spawn-wait", "--evidence-root", join(output, "runs")]));
  const ended = process.hrtime.bigint();
  if (!result.passed) throw new Error(`fixture failed: ${JSON.stringify(result)}`);
  const measurement = JSON.parse(await readFile(join(result.directory, "p05-spawn-wait.json"), "utf8"));
  for (const name of ["spawn_ns", "wait_ns", "spawn_wait_ns"]) {
    if (!Number.isSafeInteger(measurement[name]) || measurement[name] <= 0) throw new Error(`invalid ${name} sample`);
  }
  samples.push({ ...slot, started_ns: started.toString(), ended_ns: ended.toString(), directory: result.directory, ...measurement });
  await writeFile(join(output, "spawn-wait-samples.json"), JSON.stringify({ identities, samples }, null, 2) + "\n", { mode: 0o600 });
}
const stats = {};
for (const metric of ["spawn_ns", "wait_ns", "spawn_wait_ns"]) {
  const values = (side) => samples.filter((sample) => sample.phase === "measured" && sample.side === side).map((sample) => sample[metric]);
  const before = values("baseline");
  const after = values("candidate");
  if (before.length !== 20 || after.length !== 20) throw new Error("incomplete paired measurements");
  const base95 = percentile(before, 0.95);
  const head95 = percentile(after, 0.95);
  stats[metric] = {
    baseline_p50_ns: percentile(before, 0.5), baseline_p95_ns: base95,
    candidate_p50_ns: percentile(after, 0.5), candidate_p95_ns: head95,
    passed: !(head95 > base95 * 1.2 && head95 - base95 > 100_000_000),
  };
}
const summary = { identities, pairs: 20, stats, passed: Object.values(stats).every((entry) => entry.passed) };
await writeFile(join(output, "spawn-wait-summary.json"), JSON.stringify(summary, null, 2) + "\n", { mode: 0o600 });
console.log(JSON.stringify(summary));
if (!summary.passed) process.exit(1);
