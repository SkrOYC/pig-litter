import { createHash } from 'node:crypto';
import { spawn } from 'node:child_process';
import { existsSync, mkdtempSync, mkdirSync, readFileSync, readdirSync, realpathSync, rmSync, statSync, writeFileSync, renameSync, copyFileSync, cpSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

if (typeof Bun === 'undefined') throw new Error('Run the performance comparator with Bun');

const baselineSHA = 'fc78bdc3b5482b027a74f9fa8a6d13923c6ed0e6';
const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), '..');
const metrics = {
  startup_ns: 100_000_000,
  spawn_to_first_request_ns: 100_000_000,
  peak_tree_rss_bytes: 20 * 1024 * 1024,
};
const measurement = {
  startup_ns: 'PiG process start to first valid parent handler arrival.',
  spawn_to_first_request_ns: 'First spawn response emission to earliest valid held-child handler arrival.',
  peak_tree_rss_bytes: 'Maximum 10 ms sampled sum of PiG process-tree RSS. Short peaks can be missed, shared pages can be counted more than once, and process reads are not atomic.',
  recipe: 'Both declared devenv shells are prepared serially. After setup exits, each sample runs the same candidate-built driver directly with the baseline shell Bash and an isolated fixture environment.',
};

export function schedule() {
  const slots = [{ ordinal: 0, stage: 'baseline_first', pair: 0, side: 'baseline' }];
  for (let pair = 1; pair <= 5; pair++) {
    for (const side of ['baseline', 'candidate']) slots.push({ ordinal: slots.length, stage: 'warmup', pair, side });
  }
  for (let pair = 1; pair <= 20; pair++) {
    for (const side of pair % 2 ? ['baseline', 'candidate'] : ['candidate', 'baseline']) {
      slots.push({ ordinal: slots.length, stage: 'measured', pair, side });
    }
  }
  return slots;
}

export function percentile(values, percent) {
  if (!Array.isArray(values) || values.length === 0 || !values.every(positiveInteger) || !(percent > 0 && percent <= 1)) throw new Error('invalid percentile input');
  return [...values].sort((a, b) => a - b)[Math.ceil(values.length * percent) - 1];
}

function positiveInteger(value) { return Number.isSafeInteger(value) && value > 0; }
function nonnegativeInteger(value) { return Number.isSafeInteger(value) && value >= 0; }
function same(a, b) { return JSON.stringify(a) === JSON.stringify(b); }
function validSHA(value) { return typeof value === 'string' && /^[a-f0-9]{40}$/.test(value); }
function validHash(value) { return typeof value === 'string' && /^[a-f0-9]{64}$/.test(value); }
function monotonicNS(value) { return typeof value === 'string' && /^(0|[1-9][0-9]{0,29})$/.test(value) ? BigInt(value) : null; }
function slotName(slot) { return `${String(slot.ordinal).padStart(3, '0')}-${slot.stage}-${slot.pair}-${slot.side}`; }

export function evaluate(samples, identities) {
  const slots = schedule();
  if (!Array.isArray(samples) || samples.length !== slots.length || !identities || !validHash(identities.driver) || !validHash(identities.bash) || !validSHA(identities.baseline?.source) || !validSHA(identities.candidate?.source) || identities.baseline.source !== baselineSHA || !['baseline', 'candidate'].every(side => validHash(identities[side].pig) && validHash(identities[side].extension))) {
    return { status: 'incomplete', reason: 'missing coverage or invalid identities' };
  }
  const byOrdinal = new Map();
  for (const sample of samples) {
    if (!sample || !Number.isInteger(sample.slot?.ordinal) || byOrdinal.has(sample.slot.ordinal)) return { status: 'incomplete', reason: 'duplicate or invalid slot' };
    const slot = slots[sample.slot.ordinal];
    if (!slot || !same(slot, sample.slot) || sample.passed !== true || !same(sample.identities, { driver: identities.driver, bash: identities.bash, ...identities[slot.side] }) || !Object.keys(metrics).every(metric => positiveInteger(sample.measurements?.[metric]))) {
      return { status: 'incomplete', reason: 'invalid sample, schedule, or identity' };
    }
    byOrdinal.set(slot.ordinal, sample);
  }
  let previousEnd = null;
  for (const slot of slots) {
    const invocation = byOrdinal.get(slot.ordinal).invocation;
    const start = monotonicNS(invocation?.start_ns), end = monotonicNS(invocation?.end_ns);
    if (start === null || end === null || start >= end || (previousEnd !== null && previousEnd > start)) return { status: 'incomplete', reason: 'invalid or overlapping invocation intervals' };
    previousEnd = end;
  }
  const gates = {};
  for (const [metric, absolute] of Object.entries(metrics)) {
    const values = side => samples.filter(sample => sample.slot.stage === 'measured' && sample.slot.side === side).map(sample => sample.measurements[metric]);
    const baseline = values('baseline');
    const candidate = values('candidate');
    if (baseline.length !== 20 || candidate.length !== 20) return { status: 'incomplete', reason: 'incomplete measured samples' };
    const b = percentile(baseline, 0.95), c = percentile(candidate, 0.95);
    const relativeExceeded = 5n * BigInt(c) > 6n * BigInt(b);
    const absoluteExceeded = BigInt(c) - BigInt(b) > BigInt(absolute);
    gates[metric] = { baseline_p50: percentile(baseline, 0.5), baseline_p95: b, candidate_p50: percentile(candidate, 0.5), candidate_p95: c, delta: c - b, ratio: c / b, relative_exceeded: relativeExceeded, absolute_exceeded: absoluteExceeded, passed: !(relativeExceeded && absoluteExceeded) };
  }
  return { status: Object.values(gates).every(gate => gate.passed) ? 'passed' : 'regressed', gates, measurement };
}

function atomicJSON(file, value) {
  const temporary = `${file}.tmp`;
  writeFileSync(temporary, `${JSON.stringify(value, null, 2)}\n`, { mode: 0o600 });
  renameSync(temporary, file);
}
function json(file) { return JSON.parse(readFileSync(file, 'utf8')); }
function sha256(file) { return createHash('sha256').update(readFileSync(file)).digest('hex'); }
function hashBytes(bytes) { return createHash('sha256').update(bytes).digest('hex'); }
function mkdir(dir) { mkdirSync(dir, { recursive: true, mode: 0o700 }); }
function createPrivate(parent, prefix) {
  const directory = mkdtempSync(path.join(parent, prefix));
  const stat = statSync(directory);
  return { path: realpathSync(directory), dev: stat.dev, ino: stat.ino };
}
function removePrivate(owned) {
  if (!owned) return;
  const stat = statSync(owned.path);
  if (stat.dev !== owned.dev || stat.ino !== owned.ino || realpathSync(owned.path) !== owned.path) throw new Error(`refusing to remove replaced scratch directory ${owned.path}`);
  rmSync(owned.path, { recursive: true });
}

let activeChild;
let interrupted = false;
for (const signal of ['SIGINT', 'SIGTERM']) process.on(signal, () => {
  interrupted = true;
  if (activeChild) activeChild.kill('SIGTERM');
});

async function command(program, args, options = {}) {
  if (interrupted) throw new Error('comparison interrupted');
  return await new Promise((resolve, reject) => {
    const child = spawn(program, args, { cwd: options.cwd, env: options.env, stdio: ['ignore', 'pipe', 'pipe'] });
    activeChild = child;
    const out = [], err = [];
    child.stdout.on('data', chunk => out.push(chunk));
    child.stderr.on('data', chunk => err.push(chunk));
    let timedOut = false;
    const timer = options.timeout ? setTimeout(() => { timedOut = true; child.kill('SIGTERM'); setTimeout(() => child.kill('SIGKILL'), 5000).unref(); }, options.timeout) : null;
    child.on('error', error => { if (timer) clearTimeout(timer); activeChild = undefined; reject(error); });
    child.on('close', (code, signal) => {
      if (timer) clearTimeout(timer);
      activeChild = undefined;
      const stdout = Buffer.concat(out).toString(), stderr = Buffer.concat(err).toString();
      if (options.log) writeFileSync(options.log, `stdout:\n${stdout}\nstderr:\n${stderr}\nexit: ${code} signal: ${signal}\n`, { mode: 0o600 });
      if (code !== 0 || interrupted || timedOut) reject(new Error(`${program} failed (exit ${code}, signal ${signal}, timeout ${timedOut}): ${stderr.slice(-3000)}`));
      else resolve(stdout.trim());
    });
  });
}

function setupEnvironment(scratch) {
  const env = {};
  for (const key of ['PATH', 'HOME', 'USER', 'LOGNAME', 'NIX_PATH', 'NIX_CONFIG', 'NIX_SSL_CERT_FILE', 'SSL_CERT_FILE', 'XDG_RUNTIME_DIR', 'CI', 'GITHUB_ACTIONS']) if (process.env[key]) env[key] = process.env[key];
  return { ...env, TMPDIR: path.join(scratch, 'tmp'), GOCACHE: path.join(scratch, 'go-build'), GOMODCACHE: path.join(scratch, 'go-mod'), GOPATH: path.join(scratch, 'go'), GOENV: 'off', GOTOOLCHAIN: 'local', CGO_ENABLED: '1' };
}

async function git(cwd, ...args) { return await command('git', ['-C', cwd, ...args]); }
async function sourceIdentity(cwd, expected) {
  const sha = await git(cwd, 'rev-parse', 'HEAD');
  const tree = await git(cwd, 'rev-parse', 'HEAD^{tree}');
  const status = await git(cwd, 'status', '--porcelain', '--untracked-files=all');
  if (sha !== expected || !validSHA(tree) || status) throw new Error(`source checkout is not clean at ${expected}: ${status}`);
  return { sha, tree };
}

function releaseDeclaration(cwd) {
  const nix = readFileSync(path.join(cwd, 'devenv.nix'), 'utf8');
  const section = nix.match(/"x86_64-linux"\s*=\s*\{([^}]+)\}/)?.[1];
  if (!section) throw new Error('missing Linux release declaration');
  const field = name => section.match(new RegExp(`\\b${name}\\s*=\\s*"([^"]+)"`))?.[1];
  const archive = field('archive'), archiveRoot = field('root'), sri = field('hash');
  const match = archive?.match(/^pig-(\d+\.\d+\.\d+)-linux-amd64\.tar\.gz$/);
  if (!match || archiveRoot !== archive.slice(0, -7) || !/^sha256-[A-Za-z0-9+/]{43}=$/.test(sri ?? '')) throw new Error('invalid Linux release declaration');
  return { archive, archiveRoot, sri, version: match[1] };
}

async function prepareSide(side, cwd, scratch, evidence, buildDriver) {
  const setup = path.join(evidence, 'setup'), binaries = path.join(evidence, 'binaries'), releases = path.join(evidence, 'releases');
  const sideScratch = path.join(scratch, `build-${side}`);
  for (const dir of ['tmp', 'go-build', 'go-mod', 'go']) mkdir(path.join(sideScratch, dir));
  const declaration = releaseDeclaration(cwd);
  const url = `https://github.com/MichaelKinsy/PiG/releases/download/v${declaration.version}`;
  const archive = path.join(releases, `${side}-${declaration.archive}`);
  const sums = path.join(releases, `${side}-SHA256SUMS`);
  await command('curl', ['--fail', '--silent', '--show-error', '--location', '--retry', '3', `${url}/SHA256SUMS`, '-o', sums]);
  await command('curl', ['--fail', '--silent', '--show-error', '--location', '--retry', '3', `${url}/${declaration.archive}`, '-o', archive]);
  const entries = readFileSync(sums, 'utf8').split(/\r?\n/).map(line => line.match(/^([a-fA-F0-9]{64})\s+\*?(.+)$/)).filter(match => match?.[2] === declaration.archive);
  const archiveHash = sha256(archive);
  const actualSRI = `sha256-${Buffer.from(archiveHash, 'hex').toString('base64')}`;
  if (entries.length !== 1 || entries[0][1].toLowerCase() !== archiveHash || actualSRI !== declaration.sri) throw new Error(`${side} archive checksum mismatch`);
  const listing = (await command('tar', ['-tzf', archive])).split('\n');
  if (listing.filter(item => item === `${declaration.archiveRoot}/pig`).length !== 1) throw new Error(`${side} release binary is missing or duplicated`);
  const extract = path.join(sideScratch, 'extract'); mkdir(extract);
  await command('tar', ['-xzf', archive, '-C', extract, `${declaration.archiveRoot}/pig`]);
  const extracted = path.join(extract, declaration.archiveRoot, 'pig');
  const pigDir = path.join(binaries, `${side}-pig-bin`); mkdir(pigDir);
  const pig = path.join(pigDir, 'pig'); copyFileSync(extracted, pig);
  const elf = readFileSync(pig).subarray(0, 20);
  if (elf.length < 20 || !elf.subarray(0, 4).equals(Buffer.from([0x7f, 0x45, 0x4c, 0x46])) || elf[4] !== 2 || elf[5] !== 1 || elf.readUInt16LE(18) !== 62) throw new Error(`${side} PiG is not native x86-64 ELF`);
  const prep = path.join(sideScratch, 'prepare.sh');
  const extension = path.join(binaries, `${side}-pig-litter`);
  const driver = path.join(binaries, 'verify-lifecycle');
  writeFileSync(prep, `set -eu\ncd "$1"\ncommand -v go > "$2/go-path"\ncommand -v bash > "$2/bash-path"\ncommand -v pig > "$2/pig-path"\ngo version > "$2/go-version"\ngo env -json GOVERSION GOHOSTOS GOHOSTARCH GOOS GOARCH CGO_ENABLED GOTOOLCHAIN GOROOT > "$2/go-env.json"\ngo -C scripts mod download\ngo -C extensions/pig-litter mod download\nGOPROXY=off go -C scripts build -mod=readonly -o "$3" ./cmd/pig-litter\nif [ "$5" = yes ]; then\n  GOPROXY=off go -C scripts test -mod=readonly ./cmd/verify-lifecycle -run 'TestPerformance|TestResident' -count=1\n  GOPROXY=off go -C scripts build -mod=readonly -o "$4" ./cmd/verify-lifecycle\nfi\ngo version -m "$3" > "$2/build-info"\n`, { mode: 0o700 });
  const env = setupEnvironment(sideScratch);
  await command('devenv', ['shell', '--', 'bash', prep, cwd, sideScratch, extension, driver, buildDriver ? 'yes' : 'no'], { cwd, env, log: path.join(setup, `${side}.log`) });
  const goEnv = json(path.join(sideScratch, 'go-env.json'));
  const declared = readFileSync(path.join(cwd, 'scripts/go.mod'), 'utf8').match(/^toolchain go(\S+)$/m)?.[1];
  if (!declared || goEnv.GOVERSION !== `go${declared}` || goEnv.GOHOSTOS !== 'linux' || goEnv.GOOS !== 'linux' || goEnv.GOHOSTARCH !== 'amd64' || goEnv.GOARCH !== 'amd64' || goEnv.CGO_ENABLED !== '1' || goEnv.GOTOOLCHAIN !== 'local') throw new Error(`${side} compiler does not match its declared native toolchain`);
  const goPath = realpathSync(readFileSync(path.join(sideScratch, 'go-path'), 'utf8').trim());
  const bashPath = realpathSync(readFileSync(path.join(sideScratch, 'bash-path'), 'utf8').trim());
  const shellPig = realpathSync(readFileSync(path.join(sideScratch, 'pig-path'), 'utf8').trim());
  if (sha256(shellPig) !== sha256(pig)) throw new Error(`${side} shell PiG differs from the verified archive`);
  copyFileSync(path.join(sideScratch, 'go-env.json'), path.join(setup, `${side}-go-env.json`));
  copyFileSync(path.join(sideScratch, 'build-info'), path.join(setup, `${side}-build-info.txt`));
  return { declaration, archive_sha256: archiveHash, official_sha256: entries[0][1].toLowerCase(), pig, pig_sha256: sha256(pig), extension, extension_sha256: sha256(extension), compiler: { path: goPath, sha256: sha256(goPath), version: goEnv.GOVERSION, environment: goEnv }, shell_pig: shellPig, bash: bashPath, bash_sha256: sha256(bashPath), build_command: 'GOPROXY=off go -C scripts build -mod=readonly ./cmd/pig-litter' };
}

function validateObservation(performance, trace) {
  if (!performance || !trace || !Object.keys(metrics).every(key => positiveInteger(performance[key])) || performance.sampling_interval_ns !== 10_000_000 || !positiveInteger(performance.sampling_count) || !positiveInteger(performance.page_size) || !nonnegativeInteger(performance.max_gap_ns) || !nonnegativeInteger(performance.vanished) || !positiveInteger(performance.root?.pid) || !positiveInteger(performance.root?.start_ticks) || !same(performance.root, trace.root) || !Array.isArray(trace.observations) || trace.observations.length !== performance.sampling_count || performance.parent_offset_ns !== performance.startup_ns || !positiveInteger(performance.spawn_offset_ns) || !positiveInteger(performance.child_offset_ns) || performance.spawn_offset_ns < performance.parent_offset_ns || performance.child_offset_ns - performance.spawn_offset_ns !== performance.spawn_to_first_request_ns) return false;
  let peak = 0, previous = -1, gap = 0;
  for (const observation of trace.observations) {
    if (!nonnegativeInteger(observation.elapsed_ns) || observation.elapsed_ns <= previous || !Array.isArray(observation.processes) || observation.processes.length === 0 || !nonnegativeInteger(observation.rss_bytes)) return false;
    if (previous >= 0) gap = Math.max(gap, observation.elapsed_ns - previous);
    previous = observation.elapsed_ns;
    let total = 0; const pids = new Set();
    for (const process of observation.processes) {
      if (!positiveInteger(process.identity?.pid) || !positiveInteger(process.identity?.start_ticks) || !nonnegativeInteger(process.rss_bytes) || process.rss_bytes % performance.page_size !== 0 || pids.has(process.identity.pid)) return false;
      pids.add(process.identity.pid); total += process.rss_bytes;
    }
    if (!Number.isSafeInteger(total) || total !== observation.rss_bytes || !observation.processes.some(process => same(process.identity, trace.root))) return false;
    peak = Math.max(peak, total);
  }
  return peak === performance.peak_tree_rss_bytes && peak === trace.peak_bytes && gap === performance.max_gap_ns && gap === trace.max_gap_ns && trace.vanished === performance.vanished;
}

function identitiesFor(provenance) {
  return { driver: provenance.driver.sha256, bash: provenance.runtime.bash_sha256, baseline: { source: provenance.baseline.source.sha, pig: provenance.baseline.pig_sha256, extension: provenance.baseline.extension_sha256 }, candidate: { source: provenance.candidate.source.sha, pig: provenance.candidate.pig_sha256, extension: provenance.candidate.extension_sha256 } };
}

export function runnerIdentity() {
  return { platform: process.platform, architecture: process.arch, machine: os.machine(), release: os.release(), version: os.version(), hostname: os.hostname(), cpus: os.cpus(), total_memory: os.totalmem(), runtime: { name: 'bun', version: Bun.version, revision: Bun.revision, executable: process.execPath }, runner_name: process.env.RUNNER_NAME ?? null, image_os: process.env.ImageOS ?? null, image_version: process.env.ImageVersion ?? null, repository: process.env.GITHUB_REPOSITORY ?? null, job: process.env.GITHUB_JOB ?? null, workflow_sha: process.env.GITHUB_SHA ?? null, run_id: process.env.GITHUB_RUN_ID ?? null, run_attempt: process.env.GITHUB_RUN_ATTEMPT ?? null };
}

function verifyBinaryIdentities(provenance) {
  for (const side of ['baseline', 'candidate']) {
    if (sha256(provenance[side].pig) !== provenance[side].pig_sha256 || sha256(provenance[side].extension) !== provenance[side].extension_sha256) throw new Error(`${side} binary identity changed`);
  }
  if (sha256(provenance.driver.path) !== provenance.driver.sha256 || sha256(provenance.runtime.bash) !== provenance.runtime.bash_sha256) throw new Error('driver or common Bash identity changed');
}

function binaryStats(provenance) {
  const files = [provenance.baseline.pig, provenance.baseline.extension, provenance.candidate.pig, provenance.candidate.extension, provenance.driver.path, provenance.runtime.bash];
  return files.map(file => {
    const info = statSync(file);
    return { file, dev: info.dev, ino: info.ino, size: info.size, mtimeMs: info.mtimeMs, ctimeMs: info.ctimeMs };
  });
}

function collectSamples(evidence, identities) {
  const samplesDir = path.join(evidence, 'samples');
  const expected = schedule().map(slotName).sort();
  if (!existsSync(samplesDir) || !same(readdirSync(samplesDir).sort(), expected)) throw new Error('sample directories do not match the schedule');
  return schedule().map(slot => {
    const dir = path.join(samplesDir, slotName(slot));
    const sample = json(path.join(dir, 'sample.json'));
    const invocation = json(path.join(dir, 'invocation.json'));
    if (!same(invocation, { slot: sample.slot, invocation: sample.invocation })) throw new Error('invocation record differs from sample');
    if (typeof sample.fixture !== 'string' || !/^core-[A-Za-z0-9]+$/.test(sample.fixture)) throw new Error('invalid fixture directory');
    const fixture = path.join(dir, sample.fixture);
    const performance = json(path.join(fixture, 'performance.json'));
    const trace = json(path.join(fixture, 'resident-observations.json'));
    const result = json(path.join(fixture, 'result.json'));
    if (!validateObservation(performance, trace) || result.passed !== true || result.scenario !== 'core' || result.exitCode !== 0 || result.isolatedEnvironment !== true || !same(sample.measurements, Object.fromEntries(Object.keys(metrics).map(key => [key, performance[key]])))) throw new Error('invalid fixture result or observations');
    return sample;
  });
}

export function summarize(evidence) {
  try {
    const state = json(path.join(evidence, 'state.json'));
    if (!['passed', 'regressed'].includes(state.status)) return { status: 'incomplete', reason: 'run has no terminal comparison state' };
    const plan = json(path.join(evidence, 'plan.json'));
    const provenance = json(path.join(evidence, 'provenance.json'));
    if (plan.baseline !== baselineSHA || !validSHA(plan.candidate) || !same(plan.schedule, schedule()) || plan.candidate !== provenance.candidate.source.sha) throw new Error('invalid plan or source identity');
    for (const side of ['baseline', 'candidate']) {
      if (sha256(path.join(evidence, 'binaries', `${side}-pig-bin`, 'pig')) !== provenance[side].pig_sha256 || sha256(path.join(evidence, 'binaries', `${side}-pig-litter`)) !== provenance[side].extension_sha256) throw new Error(`${side} retained binary identity changed`);
    }
    if (sha256(path.join(evidence, 'binaries', 'verify-lifecycle')) !== provenance.driver.sha256) throw new Error('retained driver identity changed');
    const identities = identitiesFor(provenance);
    const samples = collectSamples(evidence, identities);
    const result = evaluate(samples, identities);
    if (result.status !== state.status) throw new Error('terminal state differs from recomputed result');
    return result;
  } catch (error) { return { status: 'incomplete', reason: error.message }; }
}

async function runComparison(candidate, evidence) {
  if (process.platform !== 'linux' || process.arch !== 'x64' || os.machine() !== 'x86_64') throw new Error('comparison requires native Linux x86-64');
  if ((process.env.RUNNER_OS && process.env.RUNNER_OS !== 'Linux') || (process.env.RUNNER_ARCH && process.env.RUNNER_ARCH !== 'X64') || (process.env.GITHUB_SHA && process.env.GITHUB_SHA !== candidate)) throw new Error('runner or workflow SHA does not match the requested candidate');
  await sourceIdentity(root, candidate);
  mkdirSync(evidence, { mode: 0o700 });
  for (const sub of ['setup', 'releases', 'binaries', 'samples']) mkdir(path.join(evidence, sub));
  atomicJSON(path.join(evidence, 'state.json'), { status: 'preparing' });
  atomicJSON(path.join(evidence, 'plan.json'), { baseline: baselineSHA, candidate, schedule: schedule(), measurement });
  let scratch, runtime, retained = false, outcome = { status: 'incomplete', reason: 'comparison did not finish' };
  try {
    const scratchParent = process.env.PIG_PERFORMANCE_SCRATCH || process.env.RUNNER_TEMP || os.homedir();
    if (!path.isAbsolute(scratchParent)) throw new Error('scratch parent must be absolute');
    mkdir(scratchParent);
    scratch = createPrivate(scratchParent, 'pig-performance-');
    runtime = createPrivate('/tmp', 'pigperf-');
    const baseline = path.join(scratch.path, 'baseline');
    await command('git', ['clone', '--local', '--no-hardlinks', '--no-checkout', root, baseline], { log: path.join(evidence, 'setup', 'clone.log') });
    await git(baseline, 'checkout', '--detach', baselineSHA);
    const baselineSource = await sourceIdentity(baseline, baselineSHA);
    const candidateSource = await sourceIdentity(root, candidate);
    atomicJSON(path.join(evidence, 'runner.json'), runnerIdentity());
    const base = await prepareSide('baseline', baseline, scratch.path, evidence, false);
    const candidateSide = await prepareSide('candidate', root, scratch.path, evidence, true);
    const driver = path.join(evidence, 'binaries', 'verify-lifecycle');
    const provenance = { baseline: { source: baselineSource, ...base }, candidate: { source: candidateSource, ...candidateSide }, driver: { path: driver, sha256: sha256(driver) }, runtime: { bash: base.bash, bash_sha256: base.bash_sha256, path: path.dirname(base.bash), driver_environment: { PATH: path.dirname(base.bash), HOME: '<private sample directory>', TMPDIR: '<private sample directory>', LANG: 'C.UTF-8', TERM: 'xterm-256color' }, fixture_environment: 'The driver supplies a private home, the staged PiG and common Bash directories on PATH, offline flags, and its loopback-only provider.', recipe: measurement.recipe } };
    atomicJSON(path.join(evidence, 'provenance.json'), provenance);
    verifyBinaryIdentities(provenance);
    const sealedStats = binaryStats(provenance);
    const identities = identitiesFor(provenance);
    atomicJSON(path.join(evidence, 'state.json'), { status: 'running', completed: 0 });
    for (const slot of schedule()) {
      if (interrupted) throw new Error('comparison interrupted');
      if (!same(binaryStats(provenance), sealedStats)) throw new Error('binary identity changed during the sample series');
      const dir = path.join(runtime.path, slotName(slot)); mkdir(dir);
      const side = provenance[slot.side];
      const env = { PATH: provenance.runtime.path, HOME: dir, TMPDIR: dir, LANG: 'C.UTF-8', TERM: 'xterm-256color' };
      let stdout;
      const start = process.hrtime.bigint();
      let invocation;
      try {
        stdout = await command(driver, ['--pig-bin', side.pig, '--extension', side.extension, '--scenario', 'core', '--measure-performance', '--evidence-root', dir], { env, timeout: 70_000, log: path.join(dir, 'driver.log') });
      } catch (error) {
        invocation = { start_ns: start.toString(), end_ns: process.hrtime.bigint().toString() };
        atomicJSON(path.join(dir, 'invocation.json'), { slot, invocation });
        atomicJSON(path.join(dir, 'failure.json'), { slot, error: error.message });
        throw error;
      }
      invocation = { start_ns: start.toString(), end_ns: process.hrtime.bigint().toString() };
      atomicJSON(path.join(dir, 'invocation.json'), { slot, invocation });
      const response = JSON.parse(stdout);
      if (response.passed !== true || response.scenario !== 'core' || path.dirname(response.directory) !== dir || !/^core-[A-Za-z0-9]+$/.test(path.basename(response.directory))) throw new Error(`invalid driver result at slot ${slot.ordinal}`);
      const performance = json(path.join(response.directory, 'performance.json'));
      const trace = json(path.join(response.directory, 'resident-observations.json'));
      const result = json(path.join(response.directory, 'result.json'));
      if (!validateObservation(performance, trace) || result.passed !== true || result.scenario !== 'core' || result.exitCode !== 0 || result.isolatedEnvironment !== true) throw new Error(`invalid measurement at slot ${slot.ordinal}`);
      if (!same(binaryStats(provenance), sealedStats)) throw new Error('binary identity changed during the sample series');
      atomicJSON(path.join(dir, 'sample.json'), { slot, invocation, passed: true, identities: { driver: identities.driver, bash: identities.bash, ...identities[slot.side] }, fixture: path.basename(response.directory), measurements: Object.fromEntries(Object.keys(metrics).map(key => [key, performance[key]])) });
      atomicJSON(path.join(evidence, 'state.json'), { status: 'running', completed: slot.ordinal + 1 });
    }
    verifyBinaryIdentities(provenance);
    if (!same(await sourceIdentity(baseline, baselineSHA), baselineSource) || !same(await sourceIdentity(root, candidate), candidateSource)) throw new Error('source identity changed');
    cpSync(runtime.path, path.join(evidence, 'samples'), { recursive: true });
    retained = true;
    outcome = evaluate(collectSamples(evidence, identities), identities);
  } catch (error) {
    outcome = { status: 'incomplete', reason: error.message };
  } finally {
    try {
      if (runtime && !retained) cpSync(runtime.path, path.join(evidence, 'samples'), { recursive: true, force: true });
      removePrivate(runtime);
      removePrivate(scratch);
    } catch (error) { outcome = { status: 'incomplete', reason: `evidence retention or cleanup failed: ${error.message}` }; }
    await new Promise(resolve => setImmediate(resolve));
    if (interrupted) outcome = { status: 'incomplete', reason: 'comparison interrupted' };
    atomicJSON(path.join(evidence, 'summary.json'), outcome);
    atomicJSON(path.join(evidence, 'state.json'), { status: outcome.status, reason: outcome.reason ?? null });
  }
  return outcome;
}

async function main(args) {
  if (args.length === 2 && args[0] === 'summarize' && path.isAbsolute(args[1])) {
    const outcome = summarize(args[1]); console.log(JSON.stringify(outcome, null, 2)); return outcome.status === 'passed' ? 0 : outcome.status === 'regressed' ? 1 : 2;
  }
  if (args.length !== 3 || args[0] !== 'run' || !validSHA(args[1]) || !path.isAbsolute(args[2]) || existsSync(args[2])) throw new Error('usage: compare-pig-performance.mjs run FULL_CANDIDATE_SHA FRESH_ABSOLUTE_EVIDENCE_DIR | summarize ABSOLUTE_EVIDENCE_DIR');
  const outcome = await runComparison(args[1], args[2]);
  console.log(JSON.stringify(outcome, null, 2));
  return outcome.status === 'passed' ? 0 : outcome.status === 'regressed' ? 1 : 2;
}

if (process.argv[1] && import.meta.url === pathToFileURL(path.resolve(process.argv[1])).href) {
  main(process.argv.slice(2)).then(code => { process.exitCode = code; }).catch(error => { console.error(error.message); process.exitCode = 2; });
}
