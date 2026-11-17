import assert from 'node:assert/strict';
import { mkdtempSync, readFileSync, writeFileSync, readdirSync, rmSync } from 'node:fs';
import os from 'node:os';
import path from 'node:path';
import { spawnSync } from 'node:child_process';
import { onTestFinished, test } from 'bun:test';
import { fileURLToPath } from 'node:url';
import { evaluate, percentile, runnerIdentity, schedule, summarize } from './compare-pig-performance.mjs';

const identities = {
  driver: 'a'.repeat(64), bash: 'b'.repeat(64),
  baseline: { source: 'fc78bdc3b5482b027a74f9fa8a6d13923c6ed0e6', pig: 'c'.repeat(64), extension: 'd'.repeat(64) },
  candidate: { source: 'e'.repeat(40), pig: 'f'.repeat(64), extension: '1'.repeat(64) },
};

function samples(baseline, candidate) {
  return schedule().map(slot => ({
    slot, invocation: { start_ns: String(slot.ordinal * 1000 + 1), end_ns: String(slot.ordinal * 1000 + 501) }, passed: true, identities: { driver: identities.driver, bash: identities.bash, ...identities[slot.side] },
    measurements: slot.side === 'baseline' ? { ...baseline } : { ...candidate },
  }));
}

const defaultValues = { startup_ns: 500_000_000, spawn_to_first_request_ns: 500_000_000, peak_tree_rss_bytes: 50 * 1024 * 1024 };

test('runner identity records the Bun executable version and revision', () => {
  const runtime = runnerIdentity().runtime;
  const version = spawnSync(process.execPath, ['--version'], { encoding: 'utf8' });
  const revision = spawnSync(process.execPath, ['--revision'], { encoding: 'utf8' });
  assert.equal(version.status, 0);
  assert.equal(revision.status, 0);
  assert.equal(runtime.name, 'bun');
  assert.equal(runtime.version, version.stdout.trim());
  assert.equal(revision.stdout.trim(), `${runtime.version}+${runtime.revision.slice(0, 9)}`);
  assert.equal(runtime.executable, process.execPath);
});

test('schedule starts with baseline and alternates the measured pairs', () => {
  const slots = schedule();
  assert.equal(slots.length, 51);
  assert.deepEqual(slots[0], { ordinal: 0, stage: 'baseline_first', pair: 0, side: 'baseline' });
  assert.deepEqual(slots.slice(1, 5).map(slot => slot.side), ['baseline', 'candidate', 'baseline', 'candidate']);
  assert.deepEqual(slots.slice(11, 17).map(slot => slot.side), ['baseline', 'candidate', 'candidate', 'baseline', 'baseline', 'candidate']);
  assert.equal(slots.filter(slot => slot.stage === 'measured' && slot.side === 'baseline').length, 20);
  assert.equal(slots.filter(slot => slot.stage === 'measured' && slot.side === 'candidate').length, 20);
});

test('nearest rank selects the tenth and nineteenth values', () => {
  const values = Array.from({ length: 20 }, (_, index) => 20 - index);
  assert.equal(percentile(values, 0.5), 10);
  assert.equal(percentile(values, 0.95), 19);
  assert.throws(() => percentile([1, NaN], 0.95), /invalid/);
});

test('a latency regression requires both strict thresholds', () => {
  const baseline = { ...defaultValues };
  let candidate = { ...baseline, startup_ns: 600_000_000 };
  assert.equal(evaluate(samples(baseline, candidate), identities).status, 'passed');
  candidate = { ...candidate, startup_ns: 600_000_001 };
  const regressed = evaluate(samples(baseline, candidate), identities);
  assert.equal(regressed.status, 'regressed');
  assert.equal(regressed.gates.startup_ns.baseline_p95, 500_000_000);
  assert.equal(regressed.gates.startup_ns.candidate_p95, 600_000_001);
  assert.equal(regressed.gates.startup_ns.relative_exceeded, true);
  assert.equal(regressed.gates.startup_ns.absolute_exceeded, true);
  assert.equal(regressed.gates.spawn_to_first_request_ns.passed, true);
  const smallBaseline = { ...baseline, spawn_to_first_request_ns: 100_000_000 };
  const equalAbsolute = { ...smallBaseline, spawn_to_first_request_ns: 200_000_000 };
  assert.equal(evaluate(samples(smallBaseline, equalAbsolute), identities).status, 'passed');
  assert.equal(evaluate(samples(smallBaseline, { ...equalAbsolute, spawn_to_first_request_ns: 200_000_001 }), identities).status, 'regressed');
  const largeBaseline = { ...baseline, startup_ns: 1_000_000_000 };
  const equalRelative = { ...largeBaseline, startup_ns: 1_200_000_000 };
  assert.equal(evaluate(samples(largeBaseline, equalRelative), identities).status, 'passed');
  assert.equal(evaluate(samples(largeBaseline, { ...equalRelative, startup_ns: 1_200_000_001 }), identities).status, 'regressed');
});

test('memory absolute equality passes and one additional byte fails', () => {
  const equal = { ...defaultValues, peak_tree_rss_bytes: 70 * 1024 * 1024 };
  assert.equal(evaluate(samples(defaultValues, equal), identities).status, 'passed');
  assert.equal(evaluate(samples(defaultValues, { ...equal, peak_tree_rss_bytes: equal.peak_tree_rss_bytes + 1 }), identities).status, 'regressed');
  const largeBaseline = { ...defaultValues, peak_tree_rss_bytes: 200 * 1024 * 1024 };
  const equalRelative = { ...largeBaseline, peak_tree_rss_bytes: 240 * 1024 * 1024 };
  assert.equal(evaluate(samples(largeBaseline, equalRelative), identities).status, 'passed');
  assert.equal(evaluate(samples(largeBaseline, { ...equalRelative, peak_tree_rss_bytes: equalRelative.peak_tree_rss_bytes + 1 }), identities).status, 'regressed');
});

test('missing, duplicate, invalid, and substituted samples cannot pass', () => {
  const valid = samples(defaultValues, defaultValues);
  assert.equal(evaluate(valid, identities).status, 'passed');
  assert.equal(evaluate(valid.slice(1), identities).status, 'incomplete');
  const duplicate = structuredClone(valid); duplicate[50] = duplicate[49];
  assert.equal(evaluate(duplicate, identities).status, 'incomplete');
  const substituted = structuredClone(valid); substituted[12].identities.pig = '0'.repeat(64);
  assert.equal(evaluate(substituted, identities).status, 'incomplete');
  const invalid = structuredClone(valid); invalid[0].measurements.startup_ns = 0;
  assert.equal(evaluate(invalid, identities).status, 'incomplete');
  const contiguous = structuredClone(valid); contiguous[12].invocation.start_ns = contiguous[11].invocation.end_ns;
  assert.equal(evaluate(contiguous, identities).status, 'passed');
  const overlap = structuredClone(valid); overlap[12].invocation.start_ns = overlap[11].invocation.end_ns;
  overlap[12].invocation.end_ns = String(Number(overlap[12].invocation.start_ns) + 1);
  overlap[11].invocation.end_ns = String(Number(overlap[12].invocation.start_ns) + 1);
  assert.equal(evaluate(overlap, identities).status, 'incomplete');
  const reordered = structuredClone(valid); [reordered[11].invocation, reordered[12].invocation] = [reordered[12].invocation, reordered[11].invocation];
  assert.equal(evaluate(reordered, identities).status, 'incomplete');
});

test('an interrupted artifact remains incomplete and summarization is read-only', () => {
  const dir = mkdtempSync(path.join(os.tmpdir(), 'pigperf-test-'));
  onTestFinished(() => rmSync(dir, { recursive: true, force: true }));
  const state = JSON.stringify({ status: 'running', completed: 50 });
  writeFileSync(path.join(dir, 'state.json'), state);
  assert.equal(summarize(dir).status, 'incomplete');
  assert.deepEqual(readdirSync(dir), ['state.json']);
  assert.equal(readFileSync(path.join(dir, 'state.json'), 'utf8'), state);
});

test('bare invocation and malformed run arguments do not start a comparison', () => {
  const script = fileURLToPath(new URL('./compare-pig-performance.mjs', import.meta.url));
  for (const args of [[], ['run'], ['run', 'invalid', '/tmp/no-comparison'], ['run', 'f'.repeat(40), 'relative']]) {
    const result = spawnSync(process.execPath, [script, ...args], { encoding: 'utf8' });
    assert.equal(result.status, 2);
    assert.match(result.stderr, /usage:/);
  }
});

test('summarization recomputes a complete artifact and detects a changed retained binary', async () => {
  const { createHash } = await import('node:crypto');
  const { mkdirSync } = await import('node:fs');
  const dir = mkdtempSync(path.join(os.tmpdir(), 'pigperf-summary-'));
  onTestFinished(() => rmSync(dir, { recursive: true, force: true }));
  const binaries = path.join(dir, 'binaries');
  mkdirSync(path.join(binaries, 'baseline-pig-bin'), { recursive: true });
  mkdirSync(path.join(binaries, 'candidate-pig-bin'), { recursive: true });
  const fileHash = file => createHash('sha256').update(readFileSync(file)).digest('hex');
  const paths = ['baseline-pig-bin/pig', 'baseline-pig-litter', 'candidate-pig-bin/pig', 'candidate-pig-litter', 'verify-lifecycle'];
  for (const file of paths) writeFileSync(path.join(binaries, file), file);
  const actual = {
    driver: fileHash(path.join(binaries, 'verify-lifecycle')), bash: 'b'.repeat(64),
    baseline: { source: identities.baseline.source, pig: fileHash(path.join(binaries, paths[0])), extension: fileHash(path.join(binaries, paths[1])) },
    candidate: { source: identities.candidate.source, pig: fileHash(path.join(binaries, paths[2])), extension: fileHash(path.join(binaries, paths[3])) },
  };
  const provenance = {
    baseline: { source: { sha: actual.baseline.source }, pig_sha256: actual.baseline.pig, extension_sha256: actual.baseline.extension },
    candidate: { source: { sha: actual.candidate.source }, pig_sha256: actual.candidate.pig, extension_sha256: actual.candidate.extension },
    driver: { sha256: actual.driver }, runtime: { bash_sha256: actual.bash },
  };
  const writeJSON = (file, value) => writeFileSync(file, JSON.stringify(value));
  writeJSON(path.join(dir, 'provenance.json'), provenance);
  writeJSON(path.join(dir, 'plan.json'), { baseline: actual.baseline.source, candidate: actual.candidate.source, schedule: schedule() });
  writeJSON(path.join(dir, 'state.json'), { status: 'passed' });
  for (const slot of schedule()) {
    const slotDir = path.join(dir, 'samples', `${String(slot.ordinal).padStart(3, '0')}-${slot.stage}-${slot.pair}-${slot.side}`);
    const fixture = path.join(slotDir, 'core-123');
    mkdirSync(fixture, { recursive: true });
    const root = { pid: 123, start_ticks: 456 };
    const performance = { ...defaultValues, parent_offset_ns: defaultValues.startup_ns, spawn_offset_ns: 600_000_000, child_offset_ns: 1_100_000_000, sampling_interval_ns: 10_000_000, sampling_count: 1, max_gap_ns: 0, page_size: 4096, root, vanished: 0 };
    const trace = { root, observations: [{ elapsed_ns: 100, rss_bytes: defaultValues.peak_tree_rss_bytes, processes: [{ identity: root, rss_bytes: defaultValues.peak_tree_rss_bytes }] }], peak_bytes: defaultValues.peak_tree_rss_bytes, max_gap_ns: 0, vanished: 0 };
    writeJSON(path.join(fixture, 'performance.json'), performance);
    writeJSON(path.join(fixture, 'resident-observations.json'), trace);
    writeJSON(path.join(fixture, 'result.json'), { passed: true, scenario: 'core', exitCode: 0, isolatedEnvironment: true });
    const invocation = { start_ns: String(slot.ordinal * 1000 + 1), end_ns: String(slot.ordinal * 1000 + 501) };
    writeJSON(path.join(slotDir, 'invocation.json'), { slot, invocation });
    writeJSON(path.join(slotDir, 'sample.json'), { slot, invocation, passed: true, identities: { driver: actual.driver, bash: actual.bash, ...actual[slot.side] }, fixture: 'core-123', measurements: defaultValues });
  }
  const result = summarize(dir);
  assert.equal(result.status, 'passed');
  assert.equal(result.gates.startup_ns.baseline_p95, 500_000_000);
  const script = fileURLToPath(new URL('./compare-pig-performance.mjs', import.meta.url));
  const cli = spawnSync(process.execPath, [script, 'summarize', dir], { encoding: 'utf8' });
  assert.equal(cli.status, 0);
  assert.equal(JSON.parse(cli.stdout).status, 'passed');
  const overlappingFile = path.join(dir, 'samples', '012-measured-1-candidate', 'sample.json');
  const originalSample = readFileSync(overlappingFile, 'utf8');
  const overlap = JSON.parse(originalSample);
  overlap.invocation.start_ns = '11000';
  writeJSON(overlappingFile, overlap);
  const invocationFile = path.join(path.dirname(overlappingFile), 'invocation.json');
  const originalInvocation = readFileSync(invocationFile, 'utf8');
  writeJSON(invocationFile, { slot: overlap.slot, invocation: overlap.invocation });
  assert.equal(summarize(dir).status, 'incomplete');
  writeFileSync(overlappingFile, originalSample);
  writeFileSync(invocationFile, originalInvocation);
  const traceFile = path.join(dir, 'samples', '000-baseline_first-0-baseline', 'core-123', 'resident-observations.json');
  const originalTrace = readFileSync(traceFile, 'utf8');
  const corrupted = JSON.parse(originalTrace);
  corrupted.observations[0].rss_bytes++;
  writeJSON(traceFile, corrupted);
  assert.equal(summarize(dir).status, 'incomplete');
  writeFileSync(traceFile, originalTrace);
  writeFileSync(path.join(binaries, 'candidate-pig-litter'), 'different');
  assert.equal(summarize(dir).status, 'incomplete');
});
