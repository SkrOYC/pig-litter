import { createHash } from "node:crypto";
import { createReadStream, lstatSync, mkdirSync, readFileSync, readdirSync, readlinkSync, realpathSync, rmSync, statSync, writeFileSync } from "node:fs";
import { isAbsolute, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";
import { stats, verdict } from "./policy.mjs";

const here = fileURLToPath(new URL(".", import.meta.url));
const mode = process.env.P04_RUN_MODE || "smoke";
const source = process.env.P04_SOURCE_ROOT;
const output = process.env.P04_OUTPUT;
const manifestPath = process.env.P04_BUILD_MANIFEST;
const commits = { parent: "e40b6bbe985c5663ebea7756f175f210402044fd", head: process.env.P04_EXPECTED_HEAD };
const trees = { parent: "11748466bf25b6591c1dec9d5d83c0d398167b7a", head: process.env.P04_EXPECTED_HEAD_TREE };
const productCommit = "b5b79f231903bd90ed0cf43fd8eaa5960125f312";
const packageIdentities = JSON.parse(readFileSync(join(here, "packages.json"), "utf8"));
const check = (condition, message) => { if (!condition) throw new Error(message); };
check(["smoke", "measure"].includes(mode), "P04_RUN_MODE must be smoke or measure.");
check([commits.head, trees.head].every((value) => /^[a-f0-9]{40}$/.test(value || "")), "Head and tree must be full lowercase Git IDs.");
check([source, output, manifestPath].every((value) => value && isAbsolute(value)), "Source, output, and manifest paths must be absolute.");
if (mode === "measure") check(process.env.RUNNER_ENVIRONMENT === "github-hosted", "Measurement requires the isolated GitHub-hosted runner.");
const read = (file) => JSON.parse(readFileSync(file, "utf8"));
const manifest = read(manifestPath);
function command(args, cwd = source) {
  const result = Bun.spawnSync(args, { cwd, stdout: "pipe", stderr: "pipe" });
  if (result.exitCode !== 0) throw new Error(`${args[0]} failed: ${result.stderr.toString()}`);
  return result.stdout.toString().trim();
}
const git = (...args) => command(["git", "-C", source, ...args]);
function checkSource() {
  check(git("rev-parse", "HEAD") === commits.head, "Candidate checkout is not at the required head.");
  if (mode === "measure") check(!git("status", "--porcelain=v1", "--untracked-files=all"), "Candidate checkout is not clean.");
  for (const name of ["parent", "head"]) check(git("rev-parse", `${commits[name]}^{tree}`) === trees[name], `${name} source tree differs.`);
  check(git("merge-base", commits.parent, commits.head) === commits.parent, "Required parent is not an ancestor of the head.");
  check(git("merge-base", productCommit, commits.head) === productCommit, "Product commit is not an ancestor of the head.");
  const changed = git("diff", "--name-only", productCommit, commits.head).split("\n").filter(Boolean);
  check(changed.every((path) => path.startsWith("scripts/p04-performance/") || path === ".github/workflows/compare-pig-performance.yml"), "Candidate changes product source after the verified product commit.");
}
async function digest(path) {
  const hasher = createHash("sha256");
  for await (const chunk of createReadStream(path)) hasher.update(chunk);
  return hasher.digest("hex");
}
checkSource();
const hashes = {};
const goMetadata = {};
for (const side of ["parent", "head"]) {
  const item = manifest[side];
  check(item?.commit === commits[side] && item.tree === trees[side], `${side} build identity differs.`);
  check(isAbsolute(item.checkout || "") && command(["git", "-C", item.checkout, "rev-parse", "HEAD"]) === commits[side], `${side} checkout differs.`);
  if (mode === "measure") check(!command(["git", "-C", item.checkout, "status", "--porcelain=v1", "--untracked-files=all"]), `${side} checkout is not clean.`);
  const declarationPath = join(item.checkout, "pig-source.json");
  check(await digest(declarationPath) === item.source?.declaration_sha256 && JSON.stringify(read(declarationPath)) === JSON.stringify(item.source.declaration), `${side} source declaration differs from its checkout.`);
  for (const name of ["product", "host", "toolchain"]) {
    const binary = item[name];
    check(isAbsolute(binary?.path || "") && statSync(binary.path).isFile(), `${side} ${name} is not an absolute regular file.`);
    hashes[`${side}_${name}`] = await digest(binary.path);
    check(binary.sha256 === hashes[`${side}_${name}`], `${side} ${name} digest differs.`);
  }
  check(item.host.sha256 === packageIdentities[side].host, `${side} host differs from its declaration.`);
  check(item.source.declaration.patch.commit === packageIdentities[side].patch, `${side} source declaration differs.`);
  check(item.source.path === packageIdentities[side].source && item.source.nar_hash === packageIdentities[side].sourceHash && item.sdk?.path === `${item.source.path}/extensions/sdk` && item.sdk.nar_hash === packageIdentities[side].sdkHash, `${side} source or SDK differs.`);
  check(item.product.go_version === "go1.27.4" && JSON.stringify(item.product.build_flags) === JSON.stringify(["-p=2", "-mod=readonly", "-trimpath", "-buildvcs=false"]), `${side} build inputs differ.`);
  check(command([item.toolchain.path, "version"]) === "go version go1.27.4 linux/amd64", `${side} Go toolchain differs.`);
  const metadata = command([item.toolchain.path, "version", "-m", item.product.path]);
  check(metadata.split("\n")[0].includes("go1.27.4") && metadata.includes("-trimpath=true") && !metadata.includes("vcs.revision="), `${side} product Go metadata differs.`);
  goMetadata[side] = metadata;
}
check(manifest.parent.toolchain.sha256 === manifest.head.toolchain.sha256 && JSON.stringify(manifest.parent.toolchain.environment) === JSON.stringify(manifest.head.toolchain.environment), "Go toolchains or build environments differ.");
check(command(["stat", "-f", "-c", "%T", "/tmp"]) !== "tmpfs" && command(["stat", "-f", "-c", "%T", "/tmp"]) !== "ramfs", "/tmp must be disk-backed.");
mkdirSync(output, { mode: 0o700 });
const scripts = {};
for (const name of ["run.mjs", "policy.mjs", "packages.json", "parent-fixture.mjs", "peer-fixture.mjs", "inspect-peer.mjs"]) scripts[name] = await digest(join(here, name));
const bunSHA256 = await digest(process.execPath);
check(Bun.version === "1.4.4", "The Bun version differs from the pinned fixture runtime.");
if (mode === "measure") {
  const release = read(process.env.P04_RELEASE_PATH);
  const smoke = read(join(release.smoke, "summary.json"));
  const smokeIdentity = read(join(release.smoke, "identity.json"));
  check(release.root_released === true && release.candidate === commits.head && release.parent === commits.parent && release.run_id === process.env.GITHUB_RUN_ID, "Measurement release differs.");
  check(release.manifest_sha256 === await digest(manifestPath) && release.smoke_identity_sha256 === await digest(join(release.smoke, "identity.json")) && release.smoke_summary_sha256 === await digest(join(release.smoke, "summary.json")), "Measurement release evidence differs.");
  check(smoke.status === "functional_smoke_passed" && smoke.mode === "smoke" && smoke.pairs === 1 && smoke.peer_messages === 8, "Required smoke did not pass.");
  check(smokeIdentity.build_manifest_sha256 === release.manifest_sha256 && smokeIdentity.bun_sha256 === bunSHA256 && JSON.stringify(smokeIdentity.scripts_sha256) === JSON.stringify(scripts), "Smoke used different builds, fixture scripts, or Bun runtime.");
}
const identity = { mode, started_utc: new Date().toISOString(), source_root: realpathSync(source), commits, trees, binary_sha256: hashes, build_manifest: manifest, build_manifest_sha256: await digest(manifestPath), scripts_sha256: scripts, uname: command(["uname", "-a"]), cpu: readFileSync("/proc/cpuinfo", "utf8"), load_before: readFileSync("/proc/loadavg", "utf8"), tmp_filesystem: command(["stat", "-f", "-c", "%T", "/tmp"]), bun_version: Bun.version, bun_sha256: bunSHA256, go_binary_metadata: goMetadata };
writeFileSync(join(output, "identity.json"), JSON.stringify(identity, null, 2));
const records = [];
function references(base) {
  const hits = [];
  for (const name of readdirSync("/proc")) {
    if (!/^\d+$/.test(name) || Number(name) === process.pid) continue;
    const directory = join("/proc", name);
    try {
      const cwd = readlinkSync(join(directory, "cwd"));
      if (cwd === base || cwd.startsWith(`${base}/`)) { hits.push(`${name}:cwd`); continue; }
    } catch {}
    for (const file of ["cmdline", "environ"]) {
      try { if (readFileSync(join(directory, file)).includes(Buffer.from(base))) { hits.push(`${name}:${file}`); break; } } catch {}
    }
    try {
      if (statSync(directory).uid !== process.getuid()) continue;
      for (const descriptor of readdirSync(join(directory, "fd"))) {
        try {
          const target = readlinkSync(join(directory, "fd", descriptor));
          if (target === base || target.startsWith(`${base}/`)) { hits.push(`${name}:fd/${descriptor}`); break; }
        } catch {}
      }
    } catch {}
  }
  return hits;
}
function cleanBase(result, kind) {
  const base = result.base;
  check(typeof base === "string" && base.startsWith(`/tmp/p04-${kind}-`) && realpathSync(base) === base, "Fixture scratch path is not the owned physical path.");
  const metadata = lstatSync(base);
  check(metadata.isDirectory() && !metadata.isSymbolicLink() && `${metadata.dev}:${metadata.ino}:${metadata.uid}` === result.baseIdentity && metadata.uid === process.getuid(), "Fixture scratch identity changed.");
  const hits = references(base);
  check(hits.length === 0, `Fixture scratch still has live process references: ${hits.join(", ")}`);
  rmSync(base, { recursive: true });
  return { base, identity: result.baseIdentity, process_references: hits, removed: true };
}
async function fixture(name, label, index, side, extra = {}) {
  const binary = manifest[side].product.path;
  const directory = join(output, `${label}-${String(index).padStart(2, "0")}`);
  const environment = { ...process.env, P04_PIG_BIN: manifest[side].host.path, P04_EXTENSION: binary, P04_EVIDENCE_DIR: directory, ...extra };
  const startedNS = Number(process.hrtime.bigint());
  const child = Bun.spawn([process.execPath, join(here, name)], { env: environment, stdout: "pipe", stderr: "pipe", cwd: source });
  const [stdout, stderr, exit] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited]);
  const record = { label, index, argv: [process.execPath, join(here, name)], start_ns: startedNS, end_ns: Number(process.hrtime.bigint()), exit };
  mkdirSync(directory, { recursive: true, mode: 0o700 });
  writeFileSync(join(directory, "driver.stdout"), stdout);
  writeFileSync(join(directory, "driver.stderr"), stderr);
  records.push(record);
  writeFileSync(join(output, "runs.json"), JSON.stringify(records, null, 2));
  check(exit === 0, `${label} fixture ${index} failed. Inspect ${directory}.`);
  const result = read(join(directory, "result.json"));
  check(result.exitCode === 0 && result.state.outcome === "observed" && result.state.failures.length === 0 && result.pigSHA256 === hashes[`${side}_host`] && result.extensionSHA256 === hashes[`${side}_product`], `${label} fixture identity or outcome differs.`);
  if (name === "peer-fixture.mjs") {
    const inspect = Bun.spawnSync([process.execPath, join(here, "inspect-peer.mjs"), directory, String(Number(extra.P04_MESSAGES_PER_SENDER) * 4)], { stdout: "pipe", stderr: "pipe" });
    writeFileSync(join(directory, "inspection.stdout"), inspect.stdout);
    writeFileSync(join(directory, "inspection.stderr"), inspect.stderr);
    check(inspect.exitCode === 0, "Independent peer inspection failed.");
  } else {
    const samples = result.state.samples;
    check(samples.length === 2 && samples[0].mode === "steer" && samples[1].mode === "follow_up" && result.state.observed.join(",") === "P04_PARENT_STEER,P04_PARENT_FOLLOW", "Parent message coverage differs.");
    for (const sample of samples) check(sample.result_ns > sample.start_ns && sample.result?.operation === "litter_message" && sample.result.state === "running" && sample.result.child.id === result.state.childID, "Parent tool result differs.");
    const entries = readFileSync(join(directory, "child-session.jsonl"), "utf8").trim().split("\n").map(JSON.parse);
    check(entries[0]?.type === "session" && entries[0]?.id === result.state.childID, "Parent child session identity differs.");
    const entryIndexes = samples.map((sample) => entries.flatMap((entry, index) => entry.type === "message" && entry.message?.role === "user" && JSON.stringify(entry.message.content).includes(sample.marker) ? [index] : []));
    check(entryIndexes.every((indexes) => indexes.length === 1) && entryIndexes[0][0] < entryIndexes[1][0], "Parent child transcript does not contain both messages in order.");
    const requests = read(join(directory, "requests.json")).filter((request) => request.model === "child");
    const requestIndexes = samples.map((sample) => requests.findIndex((request) => JSON.stringify(request.messages).includes(sample.marker)));
    check(requestIndexes[0] > 0 && requestIndexes[1] > requestIndexes[0], "Parent child provider did not receive both messages in order.");
  }
  record.cleanup = cleanBase(result, name === "peer-fixture.mjs" ? "peer-perf" : "parent");
  writeFileSync(join(output, "runs.json"), JSON.stringify(records, null, 2));
  return result;
}
try {
  const pairs = mode === "measure" ? 20 : 1;
  const perSender = mode === "measure" ? 25 : 2;
  if (mode === "measure") {
    for (let i = 0; i < 5; i++) await fixture("parent-fixture.mjs", "warmup-parent", i, "parent");
    for (let i = 0; i < 5; i++) await fixture("parent-fixture.mjs", "warmup-head", i, "head");
  }
  const measurements = { parent: [], head: [] };
  for (let i = 0; i < pairs; i++) {
    const order = i % 2 === 0 ? ["parent", "head"] : ["head", "parent"];
    for (const label of order) measurements[label].push(await fixture("parent-fixture.mjs", label, i, label));
  }
  const peer = await fixture("peer-fixture.mjs", "peer", 0, "head", { P04_MESSAGES_PER_SENDER: String(perSender), P04_POLL_MS: "2" });
  checkSource();
  for (const side of ["parent", "head"]) for (const name of ["host", "product", "toolchain"]) check(await digest(manifest[side][name].path) === hashes[`${side}_${name}`], `${side} ${name} changed during the run.`);
  const summary = { status: "functional_smoke_passed", mode, pairs, peer_messages: peer.state.samples.length, finished_utc: new Date().toISOString(), load_after: readFileSync("/proc/loadavg", "utf8"), boundaries: { parent: "Fixture provider emits litter_message through the next lead provider request containing its accepted tool result.", acceptance: "Fixture provider emits team_send through the sender provider request containing the accepted result, after product file and directory sync. This is a conservative upper bound that includes fixture and RPC overhead.", receipt_visibility: "First observed receipt filename and JSON. The link can precede directory sync, so this is not a durable acknowledgment.", delivery: "First poll that finds the matching native custom message in the recipient transcript.", idle_wake: "First recipient provider request containing the unique peer marker after the recipient was observed idle.", poll_interval_ms: 2, polling: "Actual last-absent and first-present timestamps are retained per sample. Scheduling and reads may increase the gap.", percentile: "Nearest rank." } };
  if (mode === "measure") {
    const parentStats = {};
    for (const label of ["parent", "head"]) {
      const all = measurements[label].flatMap((result) => result.state.samples);
      parentStats[label] = { overall: stats(all.map((sample) => sample.result_ns - sample.start_ns)), steer: stats(all.filter((sample) => sample.mode === "steer").map((sample) => sample.result_ns - sample.start_ns)), follow_up: stats(all.filter((sample) => sample.mode === "follow_up").map((sample) => sample.result_ns - sample.start_ns)) };
    }
    const peerStats = Object.fromEntries([["acceptance", "result_ns"], ["receipt_visibility", "receipt_visible_ns"], ["delivery", "native_visible_ns"], ["idle_wake", "wake_ns"]].map(([name, field]) => [name, stats(peer.state.samples.map((sample) => sample[field] - sample.start_ns))]));
    const outcome = verdict(parentStats, peerStats);
    summary.status = outcome.status;
    summary.parent = parentStats;
    summary.peer = peerStats;
    summary.extra_lead_requests = 0;
    summary.parent_regression = outcome.parentRegression;
    summary.parent_regressions_by_flow = outcome.regressions;
    summary.peer_thresholds_proven = outcome.peerProven;
  }
  writeFileSync(join(output, "summary.json"), JSON.stringify(summary, null, 2));
  console.log(JSON.stringify({ status: summary.status, mode, pairs, peer_messages: summary.peer_messages, output }));
  if (summary.status === "regressed") process.exitCode = 1;
  if (summary.status === "inconclusive") process.exitCode = 2;
} catch (error) {
  writeFileSync(join(output, "failure.json"), JSON.stringify({ error: String(error), records }, null, 2));
  console.error(String(error));
  process.exitCode = 1;
}
