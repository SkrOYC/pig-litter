import { test, expect } from 'bun:test';
import { readFileSync } from 'node:fs';
import { mkdtemp, mkdir, readFile, writeFile, rm } from 'node:fs/promises';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import { liveWidgetPattern, configuration, environment, launchArgs, sessionEvidence, sourceIdentity, configuredAgents, currentInspection, pausedOutcome, pausedRetention, completedDelegation, resumedRecall, cancellationAcknowledged, runCommand, parseProcessStat, readOwnedProcess, signalOwnedProcess, cleanupOwnedProcesses, saveFailureArtifacts, finalizeArtifacts, launchTmuxSession } from './live.mjs';

const model = 'sample/model';
const token = 'private-marker-7f4a';
const child = { id: 'child-1', name: 'scout-proof', type: 'scout', model, state: 'completed' };

function record(type, id, message) { return { type, id, message }; }
function assistant(id, content, stopReason = 'toolUse') {
 return record('message', id, { role: 'assistant', provider: 'sample', model: 'model', stopReason, content });
}
function toolResult(id, toolCallId, toolName, value, isError = false) {
 return record('message', id, { role: 'toolResult', toolCallId, toolName, isError, content: [{ type: 'text', text: typeof value === 'string' ? value : JSON.stringify(value) }] });
}
function spawnResult({ type = 'scout', cwd, childModel = model, task = 'Read input.txt and report its complete token.' } = {}) {
 const snapshot = { ...child, type, model: childModel, ...(cwd ? { cwd } : {}) };
 return [
  assistant('spawn-call-entry', [{ type: 'toolCall', id: 'spawn-call', name: 'litter_spawn', arguments: { type, name: 'scout-proof', task, ...(cwd ? { cwd } : {}) } }]),
  toolResult('spawn-result-entry', 'spawn-call', 'litter_spawn', { state: 'starting', child: snapshot, operation: 'litter_spawn', handbackTruncated: false }),
 ];
}
function outcome({ state = 'completed', id = child.id, text = `Read from file: ${token}`, model: childModel = model, reportProvenance = 'child_assistant', truncated = false } = {}) {
 return { run: { id }, state, model: childModel, text, truncated, usage: { input: 18, output: 4, cacheRead: 0, cacheWrite: 0, totalTokens: 22, cost: 0 }, reportProvenance };
}
function completedSession({ state = 'completed', childModel = model, marker = token, operation = 'wait', cwd, type = 'scout', task, truncated = false, reportProvenance = 'child_assistant' } = {}) {
 const snapshot = { ...child, type, model: childModel, ...(cwd ? { cwd } : {}) };
 const waitCall = `${operation}-call`;
 const waitResult = `${operation}-result`;
 const target = operation === 'wait'
  ? { state, outcome: outcome({ state, text: `Read from file: ${marker}`, model: childModel, truncated, reportProvenance }), operation: 'litter_wait', handbackTruncated: false }
  : { state, child: snapshot, outcome: outcome({ state, text: `Read from file: ${marker}`, model: childModel, truncated, reportProvenance }), operation: 'litter_inspect', handbackTruncated: false };
 return [
  { type: 'session', id: 'parent', cwd: '/scratch' },
  ...spawnResult({ cwd, childModel, type, task }),
  assistant(`${operation}-call-entry`, [{ type: 'toolCall', id: waitCall, name: `litter_${operation}`, arguments: { id: child.id, ...(operation === 'wait' ? { timeoutMs: 120000 } : {}) } }]),
  toolResult(waitResult, waitCall, `litter_${operation}`, target),
  assistant('final-entry', [{ type: 'text', text: 'Done' }], 'stop'),
 ].map(value => JSON.stringify(value)).join('\n') + '\n';
}
function pausedCancellationSession({ resumeBeforeInspection = false, resumeAfterInspection = false } = {}) {
 const records = [
  { type: 'session', id: 'parent', cwd: '/scratch' },
  ...spawnResult(),
  assistant('cancel-wait-call-entry', [{ type: 'toolCall', id: 'cancel-wait-call', name: 'litter_wait', arguments: { id: child.id, timeoutMs: 300000 } }]),
  toolResult('cancel-wait-result-entry', 'cancel-wait-call', 'litter_wait', 'Tool execution aborted', true),
 ];
 const resumeRecords = [
  assistant('resume-message-entry', [{ type: 'toolCall', id: 'resume-message-call', name: 'litter_message', arguments: { id: child.id, resume: true, text: 'Read input.txt and report its token.' } }]),
  toolResult('resume-message-result', 'resume-message-call', 'litter_message', { delivery: 'resumed', child: { ...child, state: 'running' }, operation: 'litter_message', handbackTruncated: false, state: 'running' }),
 ];
 if (resumeBeforeInspection) records.push(...resumeRecords);
 records.push(
  assistant('paused-inspection-call-entry', [{ type: 'toolCall', id: 'paused-inspection-call', name: 'litter_inspect', arguments: { id: child.id } }]),
  toolResult('paused-inspection-result', 'paused-inspection-call', 'litter_inspect', { state: 'paused', child: { ...child, state: 'paused' }, outcome: outcome({ state: 'paused', text: '' }), operation: 'litter_inspect', handbackTruncated: false }),
 );
 if (resumeAfterInspection) records.push(
  ...resumeRecords,
  assistant('resume-wait-entry', [{ type: 'toolCall', id: 'resume-wait-call', name: 'litter_wait', arguments: { id: child.id, timeoutMs: 120000 } }]),
  toolResult('resume-wait-result', 'resume-wait-call', 'litter_wait', { state: 'completed', outcome: outcome(), operation: 'litter_wait', handbackTruncated: false }),
 );
 records.push(assistant('paused-session-final', [{ type: 'text', text: 'Done' }], 'stop'));
 return records.map(record => JSON.stringify(record)).join('\n') + '\n';
}

function pauseResult(evidence, state, stoppedCount) {
 evidence.calls.push({ id: 'pause-call', name: 'litter_stop', arguments: { id: child.id } });
 evidence.results.push({ id: 'pause-call', name: 'litter_stop', isError: false, value: { state, outcome: outcome({ state, text: '' }), stoppedCount, operation: 'litter_stop', handbackTruncated: false } });
 return evidence;
}

test('runtime inputs preserve inherited configuration and select a local Piglet path', () => {
 const localPiglet = '/home/oscar/GitHub/pig-litter/piglet.yaml';
 const config = configuration(['--piglet', localPiglet, '--model', model, '--pig-home', '/config', '--case', 'all'], {});
 const env = environment({ ...config, pigBinary: '/selected/bin/pig' }, { PATH: '/bin', HOME: '/user', PIG_CODING_AGENT_DIR: '/agent', API_KEY: 'opaque', PIG_PIGLET_NAME: 'ambient' });
 expect(env).toEqual({ PATH: '/selected/bin:/bin', HOME: '/user', PIG_CODING_AGENT_DIR: '/agent', API_KEY: 'opaque', PIG_HOME: '/config', TERM: 'xterm-256color' });
 expect(configuration([], {}).model).toBeUndefined();
 expect(configuration([], { PIG_HOME: '/inherited' }).pigHome).toBe('/inherited');
 expect(launchArgs(config, 'selected', '/sessions')).toContain(localPiglet);
 expect(launchArgs(config, 'selected', '/sessions')).not.toContain('--no-builtin-tools');
 expect(launchArgs(config, 'selected', '/sessions')).toContain('read,write,edit,bash,litter_spawn,litter_list,litter_inspect,litter_message,litter_stop,litter_wait');
 expect(launchArgs(config, 'selected', '/sessions')).not.toContain('-e');
 expect(launchArgs(config, 'plain', '/sessions')).not.toContain('--no-builtin-tools');
 expect(launchArgs(config, 'plain', '/sessions')).not.toContain('--piglet');
 expect(launchArgs(config, 'plain', '/sessions')).not.toContain('--no-skills');
});

test('doctor source identity distinguishes the changed checkout from an installed Piglet', () => {
 const root = '/home/oscar/GitHub/pig-litter';
 expect(sourceIdentity(`${root}/piglet.yaml`, '/tmp/scratch', { name: 'pig-litter', source: `${root}/piglet.yaml` }).resolvedPath).toBe(`${root}/piglet.yaml`);
 expect(sourceIdentity('piglet.yml', '/tmp/scratch', { name: 'pig-litter', source: 'piglet.yml' }).resolvedPath).toBe('/tmp/scratch/piglet.yml');
 expect(sourceIdentity('pig-litter', '/tmp/scratch', { name: 'pig-litter', source: '/home/oscar/.pig/piglets/pig-litter/piglet.yaml' }).name).toBe('pig-litter');
 expect(() => sourceIdentity(`${root}/piglet.yaml`, '/tmp/scratch', { name: 'pig-litter', source: '/home/oscar/.pig/piglets/pig-litter/piglet.yaml' })).toThrow('different Piglet source');
 expect(() => sourceIdentity('pig-litter', '/tmp/scratch', { name: 'other', source: '/home/oscar/.pig/piglets/other/piglet.yaml' })).toThrow('resolved pig-litter as other');
});

test('agent discovery requires a read-only leaf scout and a worker with bash', () => {
 const definitions = [
  { name: 'scout', role: 'scout', canDelegate: false, tools: ['read'] },
  { name: 'worker', role: 'worker', canDelegate: true, tools: ['read', 'write', 'edit', 'bash'] },
 ];
 const evidence = sessionEvidence([
  assistant('list-call-entry', [{ type: 'toolCall', id: 'list-call', name: 'litter_list', arguments: {} }]),
  toolResult('list-result-entry', 'list-call', 'litter_list', { availableAgentTypes: definitions, children: [], handbackTruncated: false, more: false, moreAgentTypes: false, nextAgentOffset: 2, nextOffset: 0, operation: 'litter_list', state: 'listed', totalAgentTypes: 2, totalChildren: 0 }),
  assistant('list-final', [{ type: 'text', text: 'Ready' }], 'stop'),
 ].map(record => JSON.stringify(record)).join('\n'));
 expect(configuredAgents(evidence)).toEqual({ scout: definitions[0], worker: definitions[1] });
 const delegating = structuredClone(evidence);
 delegating.results[0].value.availableAgentTypes[0].canDelegate = true;
 expect(() => configuredAgents(delegating)).toThrow('delegation disabled');
 const noBash = structuredClone(evidence);
 noBash.results[0].value.availableAgentTypes[1].tools = ['read', 'write'];
 expect(() => configuredAgents(noBash)).toThrow('does not expose bash');
 expect(configuredAgents(sessionEvidence(''))).toBeUndefined();
});

test('argument validation bounds command timers and rejects unsupported options', () => {
 expect(() => configuration(['--timeout-ms', '0'], {})).toThrow();
 expect(() => configuration(['--case', 'unknown'], {})).toThrow();
 expect(() => configuration(['--model', 'unqualified'], {})).toThrow();
 expect(() => configuration(['--credentials'], {})).toThrow();
});

test('a completed child is matched by ID, state, inherited model, and its final report', () => {
 const evidence = sessionEvidence(completedSession());
 const result = completedDelegation(evidence, 'scout', model, token);
 expect(result.outcome).toMatchObject({ run: { id: child.id }, state: 'completed', model, reportProvenance: 'child_assistant', truncated: false });
 expect(result.outcome.text).toBe(`Read from file: ${token}`);
 expect(result.resultCall.name).toBe('litter_wait');
 expect(evidence.header.id).toBe('parent');
 expect(() => completedDelegation(sessionEvidence(completedSession({ state: 'failed' })), 'scout', model, token)).toThrow('returned failed');
 expect(() => completedDelegation(sessionEvidence(completedSession({ childModel: 'other/model' })), 'scout', model, token)).toThrow('effective model');
 expect(() => completedDelegation(sessionEvidence(completedSession({ marker: 'wrong-marker' })), 'scout', model, token)).toThrow('lacks the expected file token');
 expect(() => completedDelegation(sessionEvidence(completedSession({ state: 'completed', task: `Read ${token}` })), 'scout', model, token)).toThrow('included in the parent child task');
 expect(() => completedDelegation(sessionEvidence(completedSession({ truncated: true })), 'scout', model, token)).toThrow('truncated');
 expect(() => completedDelegation(sessionEvidence(completedSession({ reportProvenance: 'tool_output' })), 'scout', model, token)).toThrow('report provenance');
 const statusMismatch = sessionEvidence(completedSession());
 statusMismatch.results.at(-1).value.state = 'running';
 expect(() => completedDelegation(statusMismatch, 'scout', model, token)).toThrow('status and latest outcome disagree');
});

test('worker completion requires an explicit path with spaces and apostrophes', () => {
 const cwd = "/tmp/worker's workspace";
 const evidence = sessionEvidence(completedSession({ type: 'worker', cwd }));
 const result = completedDelegation(evidence, 'worker', model, token, { cwd });
 expect(result.call.arguments.cwd).toBe(cwd);
 expect(result.child.cwd).toBe(cwd);
 expect(() => completedDelegation(evidence, 'worker', model, token, { cwd: '/tmp/other workspace' })).toThrow('required explicit working directory');
});

test('child admission and final outcome cannot carry a generation selector', () => {
 const admitted = sessionEvidence(completedSession());
 admitted.results[0].value.child.generation = 1;
 expect(() => completedDelegation(admitted, 'scout', model, token)).toThrow('exposed a generation');
 const completed = sessionEvidence(completedSession());
 completed.results.at(-1).value.outcome.run.generation = 1;
 expect(completedDelegation(completed, 'scout', model, token)).toBeUndefined();
 const guarded = sessionEvidence(completedSession());
 guarded.calls.find(call => call.name === 'litter_wait').arguments.generation = 1;
 expect(() => completedDelegation(guarded, 'scout', model, token)).toThrow('unsupported control field');
});

test('current inspection returns the common envelope, child, and latest outcome', () => {
 const evidence = sessionEvidence(completedSession({ operation: 'inspect' }));
 const inspection = currentInspection(evidence, child.id);
 expect(inspection.result.value.state).toBe('completed');
 expect(inspection.outcome.run).toEqual({ id: child.id });
 expect(inspection.result.value.operation).toBe('litter_inspect');
 expect(inspection.result.value.handbackTruncated).toBe(false);
 for (const field of ['operation', 'handbackTruncated']) {
  const missing = sessionEvidence(completedSession({ operation: 'inspect' }));
  delete missing.results.at(-1).value[field];
  expect(() => currentInspection(missing, child.id)).toThrow('beyond current status');
 }
 const truncated = sessionEvidence(completedSession({ operation: 'inspect' }));
 truncated.results.at(-1).value.handbackTruncated = true;
 expect(() => currentInspection(truncated, child.id)).toThrow('misidentified or truncated');
 const misidentified = sessionEvidence(completedSession({ operation: 'inspect' }));
 misidentified.results.at(-1).value.operation = 'litter_wait';
 expect(() => completedDelegation(misidentified, 'scout', model, token, { operation: 'inspect' })).toThrow('misidentified or truncated');
});

test('a valid report above eight kilobytes passes within the fifty-kilobyte default', () => {
 const marker = token + 'x'.repeat(9000);
 const result = completedDelegation(sessionEvidence(completedSession({ marker })), 'scout', model, token);
 expect(result.outcome.text).toBe(`Read from file: ${marker}`);
 const oversized = sessionEvidence(completedSession({ marker: token + 'x'.repeat(50001) }));
 expect(() => completedDelegation(oversized, 'scout', model, token)).toThrow('exceeds the report limit');
});

test('active stop pauses the retained session and reports one newly paused turn', () => {
 const pause = pausedOutcome(pauseResult(sessionEvidence(completedSession()), 'paused', 1), child.id);
 expect(pause.result.value.state).toBe('paused');
 expect(pause.result.value.stoppedCount).toBe(1);
 expect(pause.outcome.state).toBe('paused');
 expect(pause.outcome.run).toEqual({ id: child.id });
 const generated = pauseResult(sessionEvidence(completedSession()), 'paused', 1);
 generated.results.at(-1).value.outcome.run.generation = 1;
 expect(() => pausedOutcome(generated, child.id)).toThrow('did not match the requested ID');
});

test('pausing a terminal session retains its last outcome without counting a new turn', () => {
 for (const state of ['completed', 'failed', 'partial', 'unavailable']) {
  const pause = pausedOutcome(pauseResult(sessionEvidence(completedSession()), state, 0), child.id);
  expect(pause.result.value.state).toBe(state);
  expect(pause.result.value.stoppedCount).toBe(0);
  expect(pause.outcome.state).toBe(state);
  expect(pause.outcome.run).toEqual({ id: child.id });
 }
 const active = pauseResult(sessionEvidence(completedSession()), 'running', 0);
 expect(() => pausedOutcome(active, child.id)).toThrow('did not retain a terminal outcome');
 const mismatched = pauseResult(sessionEvidence(completedSession()), 'completed', 0);
 mismatched.results.at(-1).value.state = 'paused';
 expect(() => pausedOutcome(mismatched, child.id)).toThrow('status and latest outcome disagree');
});

test('inspection rejects transcript selectors and history or evidence pages', () => {
 const transcript = sessionEvidence(completedSession({ operation: 'inspect' }));
 transcript.calls.find(call => call.name === 'litter_inspect').arguments.transcript = true;
 expect(() => currentInspection(transcript, child.id)).toThrow('unsupported control field');
 const pages = sessionEvidence(completedSession({ operation: 'inspect' }));
 pages.results.at(-1).value.entries = [];
 expect(() => currentInspection(pages, child.id)).toThrow('beyond current status');
 const waitPages = sessionEvidence(completedSession());
 waitPages.results.at(-1).value.mailbox = [];
 expect(() => completedDelegation(waitPages, 'scout', model, token)).toThrow('unexpected fields');
});

test('parent prose, missing tool results, and tool errors cannot prove completion', () => {
 const missingResult = sessionEvidence(completedSession());
 missingResult.results = [];
 expect(completedDelegation(missingResult, 'scout', model, token)).toBeUndefined();
 const unfinished = sessionEvidence(completedSession());
 unfinished.assistants.pop();
 expect(completedDelegation(unfinished, 'scout', model, token)).toBeUndefined();
 const unrelated = sessionEvidence(completedSession());
 unrelated.calls.push({ id: 'file-call', name: 'read', arguments: { path: 'input.txt' } });
 expect(() => completedDelegation(unrelated, 'scout', model, token)).toThrow('Parent called another tool');
 const aborted = sessionEvidence(completedSession());
 const result = aborted.results.at(-1);
 result.isError = true;
 result.value.state = 'failed';
 result.value.outcome.state = 'failed';
 expect(() => completedDelegation(aborted, 'scout', model, token)).toThrow('returned failed');
});

function resumeSession({ leaked = false, ackAfterOutcome = false } = {}) {
 const records = completedSession().trim().split('\n').map(line => JSON.parse(line));
 const resumedOutcomeCall = assistant('resume-wait-entry', [{ type: 'toolCall', id: 'resume-wait-call', name: 'litter_wait', arguments: { id: child.id, timeoutMs: 120000 } }]);
 const resumedOutcome = toolResult('resume-wait-result', 'resume-wait-call', 'litter_wait', { state: 'completed', outcome: outcome(), operation: 'litter_wait', handbackTruncated: false });
 const resumeCall = assistant('resume-message-entry', [{ type: 'toolCall', id: 'resume-message-call', name: 'litter_message', arguments: { id: child.id, resume: true, text: leaked ? `Recall ${token}` : 'Recall the input token from retained context.' } }]);
 const resumeAck = toolResult('resume-message-result', 'resume-message-call', 'litter_message', { delivery: 'resumed', child: { ...child, state: 'running' }, operation: 'litter_message', handbackTruncated: false, state: 'running' });
 const extra = ackAfterOutcome ? [resumedOutcomeCall, resumedOutcome, resumeCall, resumeAck] : [resumeCall, resumeAck, resumedOutcomeCall, resumedOutcome];
 return [...records, ...extra, assistant('resume-final', [{ type: 'text', text: 'Done' }], 'stop')].map(record => JSON.stringify(record)).join('\n') + '\n';
}

function acknowledgedResumeWithoutOutcome() {
 const records = completedSession().trim().split('\n').map(line => JSON.parse(line)).slice(0, -1);
 return [...records,
  assistant('resume-message-entry', [{ type: 'toolCall', id: 'resume-message-call', name: 'litter_message', arguments: { id: child.id, resume: true, text: 'Recall the input token from retained context.' } }]),
  toolResult('resume-message-result', 'resume-message-call', 'litter_message', { delivery: 'resumed', child: { ...child, state: 'running' }, operation: 'litter_message', handbackTruncated: false, state: 'running' }),
  assistant('resume-final', [{ type: 'text', text: 'Done' }], 'stop'),
 ].map(record => JSON.stringify(record)).join('\n') + '\n';
}

test('retained recall uses the same ID after acknowledgement and excludes the supplied token', () => {
 const evidence = sessionEvidence(resumeSession());
 const result = resumedRecall(evidence, { child }, model, token);
 expect(result.child.id).toBe(child.id);
 expect(result.outcome.text).toBe(`Read from file: ${token}`);
 expect(result.result.position).toBeGreaterThan(result.message.position);
 expect(resumedRecall(sessionEvidence(acknowledgedResumeWithoutOutcome()), { child }, model, token)).toBeUndefined();
 expect(() => resumedRecall(sessionEvidence(resumeSession({ leaked: true })), { child }, model, token)).toThrow('supplied the private token');
 expect(resumedRecall(sessionEvidence(resumeSession({ ackAfterOutcome: true })), { child }, model, token)).toBeUndefined();
});

test('Escape leaves the child paused until the same ID is explicitly resumed', () => {
 const evidence = sessionEvidence(pausedCancellationSession({ resumeAfterInspection: true }));
 const pause = pausedRetention(evidence, child.id, 'cancel-wait-call');
 expect(pause.child.id).toBe(child.id);
 expect(pause.inspection.result.value.state).toBe('paused');
 expect(pause.outcome.state).toBe('paused');
 expect(pause.outcome.run).toEqual({ id: child.id });
 const resumed = resumedRecall(evidence, { child: pause.child }, model, token, pause.inspection.result.position);
 expect(resumed.child.id).toBe(child.id);
 expect(resumed.outcome.state).toBe('completed');
 expect(resumed.outcome.text).toBe(`Read from file: ${token}`);
 const resumedBeforeInspection = sessionEvidence(pausedCancellationSession({ resumeBeforeInspection: true }));
 expect(() => pausedRetention(resumedBeforeInspection, child.id, 'cancel-wait-call')).toThrow('resumed before its paused status was inspected');
});

test('cancellation acknowledgement is scoped to the pending wait', () => {
 const evidence = sessionEvidence(completedSession());
 expect(cancellationAcknowledged(evidence, 'other-call')).toBe(false);
 evidence.results.push({ id: 'cancel-call', name: 'litter_wait', isError: true, errorText: 'Tool execution aborted' });
 expect(cancellationAcknowledged(evidence, 'cancel-call')).toBe(true);
 evidence.results.push({ id: 'cancelled-call', name: 'litter_wait', value: { state: 'cancelled', code: 'cancelled', report: 'wait cancelled', operation: 'litter_wait', handbackTruncated: false } });
 expect(cancellationAcknowledged(evidence, 'cancelled-call')).toBe(true);
});

function fixtureIdentity(pid) {
 try {
  const stat = readFileSync(`/proc/${pid}/stat`, 'utf8');
  return { pid, startTicks: stat.slice(stat.lastIndexOf(')') + 2).trim().split(/\s+/)[19] };
 } catch (error) { if (['ENOENT', 'ESRCH'].includes(error.code)) return undefined; throw error; }
}

async function joinFixtureProcesses(rows) {
 const alive = row => fixtureIdentity(row.pid)?.startTicks === row.startTicks;
 for (const row of rows.filter(Boolean).toReversed()) {
  if (!alive(row)) continue;
  try { process.kill(row.pid, 'SIGKILL'); } catch (error) { if (error.code !== 'ESRCH') throw error; }
  const deadline = Date.now() + 1000;
  while (alive(row) && Date.now() < deadline) await new Promise(resolve => setTimeout(resolve, 10));
 }
 const remaining = rows.filter(row => row && alive(row));
 if (remaining.length) throw new Error(`Independent fixture cleanup left owned processes: ${remaining.map(row => row.pid).join(', ')}.`);
}

async function expectBoundedProcessFailure(options, script, message) {
 let leader;
 let descendant;
 const started = performance.now();
 try {
  await expect(runCommand(['sh', '-c', script], {
   timeoutMs: 100,
   ...options,
   onStart(childProcess) { leader = fixtureIdentity(childProcess.pid); },
   onStdout(text, childProcess) {
    const pid = Number(text.trim().split('\n')[0]);
    if (Number.isInteger(pid) && pid > 0) descendant = fixtureIdentity(pid);
    if (options.input) childProcess.stdin.end();
   },
  })).rejects.toThrow(message);
  expect(performance.now() - started).toBeLessThan(1200);
  expect(Number.isInteger(descendant?.pid)).toBe(true);
  expect(() => process.kill(leader.pid, 0)).toThrow();
  expect(() => process.kill(descendant.pid, 0)).toThrow();
 } finally { await joinFixtureProcesses([leader, descendant]); }
}

test('command deadline covers descendant-held pipes and reaps the owned group', async () => {
 await expectBoundedProcessFailure({}, 'sleep 30 & echo $!; wait', 'Command timed out after 100 ms.');
});

test('RPC stdin closure cannot bypass the output and exit deadline', async () => {
 await expectBoundedProcessFailure({ input: 'metadata request\n' }, 'read request; sleep 30 & echo $!; wait', 'Command timed out after 100 ms.');
});

test('stream output overflow tears down descendants without waiting for EOF', async () => {
 await expectBoundedProcessFailure({ timeoutMs: 1000, maxOutputBytes: 128 }, 'sleep 30 & echo $!; sleep 0.03; while true; do printf "oversized metadata chunk\\n"; done', 'Command output exceeds its byte limit.');
});

test('ordinary command output remains available after bounded teardown', async () => {
 expect(await runCommand(['sh', '-c', 'printf "metadata ready"'], { timeoutMs: 1000 })).toBe('metadata ready');
});

test('live widget recognizes active counts at wide and narrow widths', () => {
 expect(liveWidgetPattern.test(' Litter 2 active 0 errors 1 paused 3 settled')).toBe(true);
 expect(liveWidgetPattern.test(' Litter 2 live 1 done')).toBe(true);
 expect(liveWidgetPattern.test(' Litter 0 active 1 error 2 paused 3 settled')).toBe(false);
 expect(liveWidgetPattern.test(' Litter 0 children')).toBe(false);
});

test('Linux stat identity preserves start ticks and a command containing parentheses', () => {
 const row = parseProcessStat('420 (worker ) name) S 7 420 420 0 -1 0 0 0 0 0 0 0 0 0 20 0 1 0 99112233445566 0');
 expect(row).toEqual({ pid: 420, parent: 7, group: 420, state: 'S', startTicks: '99112233445566', command: 'worker ) name' });
 expect(() => parseProcessStat('420 (broken) S 7')).toThrow('Malformed Linux process identity');
});

test('a different start identity cannot signal the live process that reused its PID', async () => {
 const child = Bun.spawn(['sleep', '30'], { stdin: 'ignore', stdout: 'ignore', stderr: 'ignore' });
 const fixture = fixtureIdentity(child.pid);
 try {
  const owner = readOwnedProcess(child.pid);
  expect(signalOwnedProcess({ ...owner, startTicks: `${owner.startTicks}0` }, 'SIGTERM')).toBe(false);
  expect(readOwnedProcess(child.pid).startTicks).toBe(owner.startTicks);
  expect(signalOwnedProcess(owner, 'SIGTERM')).toBe(true);
  expect(await child.exited).toBe(143);
  expect(readOwnedProcess(child.pid)).toBeUndefined();
 } finally {
  await joinFixtureProcesses([fixture]);
  await child.exited;
 }
});

const cleanupServer = { pid: 420, parent: 1, group: 420, state: 'S', startTicks: '5000', command: 'tmux' };

test('a closed socket cannot prove that the private tmux process exited', async () => {
 const result = await cleanupOwnedProcesses({ processes: [], server: cleanupServer, read: () => cleanupServer, socketAlive: async () => false, killServer: async () => {}, send() {}, timeoutMs: 20 });
 expect(result.serverExited).toBe(false);
 expect(result.socketClosed).toBe(true);
 expect(result.remaining).toEqual([cleanupServer]);
 expect(result.failures).toContain('Owned processes still running: 420.');
});

test('kill-server failure remains a cleanup failure after fallback signals stop the server', async () => {
 let current = cleanupServer;
 const result = await cleanupOwnedProcesses({ processes: [], server: cleanupServer, read: () => current, socketAlive: async () => Boolean(current), killServer: async () => { throw new Error('permission denied'); }, send() { current = undefined; }, timeoutMs: 20 });
 expect(result.serverExited).toBe(true);
 expect(result.socketClosed).toBe(true);
 expect(result.killServer).toEqual({ error: 'permission denied', benignPriorExit: false });
 expect(result.failures).toContain('kill-server: permission denied');
});

test('kill-server failure is recorded as benign only when process and socket already exited', async () => {
 let current = cleanupServer;
 const result = await cleanupOwnedProcesses({ processes: [], server: cleanupServer, read: () => current, socketAlive: async () => Boolean(current), killServer: async () => { current = undefined; throw new Error('server already exited'); }, send() { throw new Error('must not signal an exited owner'); }, timeoutMs: 20 });
 expect(result.killServer).toEqual({ error: 'server already exited', benignPriorExit: true });
 expect(result.serverExited).toBe(true);
 expect(result.socketClosed).toBe(true);
 expect(result.failures).toEqual([]);
});

test('signal failures remain visible and prevent scratch removal', async () => {
 const scratch = await mkdtemp(join(tmpdir(), 'litter-helper-signal-'));
 const evidencePath = await mkdtemp(join(tmpdir(), 'litter-helper-evidence-'));
 try {
  await writeFile(join(scratch, 'raw.log'), 'required raw evidence\n');
  const result = await cleanupOwnedProcesses({ processes: [cleanupServer], read: () => cleanupServer, socketAlive: async () => false, send() { throw new Error('signal denied'); }, timeoutMs: 20 });
  const report = { status: 'passed', cleanup: { ...result, evidenceFailures: [], scratchRemoved: false } };
  await finalizeArtifacts(report, { scratch, evidencePath });
  expect(report.status).toBe('failed');
  expect(report.cleanup.failures).toContain('SIGTERM 420: signal denied');
  expect(report.cleanup.failures).toContain('SIGKILL 420: signal denied');
  expect(report.cleanup.scratchRemoved).toBe(false);
  expect(await readFile(join(scratch, 'raw.log'), 'utf8')).toBe('required raw evidence\n');
  expect(JSON.parse(await readFile(join(evidencePath, 'run.json'), 'utf8')).status).toBe('failed');
 } finally { await rm(scratch, { recursive: true, force: true }); await rm(evidencePath, { recursive: true, force: true }); }
});

test('failed raw evidence saves aggregate errors and preserve the malformed source log', async () => {
 const scratch = await mkdtemp(join(tmpdir(), 'litter-helper-save-'));
 const evidencePath = await mkdtemp(join(tmpdir(), 'litter-helper-evidence-'));
 const sessions = join(scratch, 'sessions');
 try {
  await mkdir(sessions);
  await writeFile(join(sessions, 'native.jsonl'), '{malformed native log\n');
  const failures = await saveFailureArtifacts({ name: 'selected', sessionDir: sessions, evidencePath, capture: async () => { throw new Error('capture unavailable'); }, save: async () => { throw new Error('disk full'); } });
  const report = { status: 'failed', cleanup: { failures: [], evidenceFailures: failures, remaining: [], scratchRemoved: false } };
  await finalizeArtifacts(report, { scratch, evidencePath });
  expect(failures).toEqual(['failure screen: capture unavailable', 'native.jsonl: disk full']);
  expect(report.cleanup.scratchRemoved).toBe(false);
  expect(await readFile(join(sessions, 'native.jsonl'), 'utf8')).toBe('{malformed native log\n');
  expect(JSON.parse(await readFile(join(evidencePath, 'run.json'), 'utf8')).cleanup.evidenceFailures).toEqual(failures);
 } finally { await rm(scratch, { recursive: true, force: true }); await rm(evidencePath, { recursive: true, force: true }); }
});

test('an unsaved run report retains scratch even when every owned process exited', async () => {
 const scratch = await mkdtemp(join(tmpdir(), 'litter-helper-report-'));
 try {
  await writeFile(join(scratch, 'raw.log'), 'only raw evidence\n');
  const report = { status: 'passed', cleanup: { failures: [], evidenceFailures: [], remaining: [], scratchRemoved: false } };
  await finalizeArtifacts(report, { scratch, evidencePath: '/not-used', save: async () => { throw new Error('report denied'); } });
  expect(report.status).toBe('failed');
  expect(report.cleanup.evidenceFailures).toContain('run.json: report denied');
  expect(report.cleanup.scratchRemoved).toBe(false);
  expect(await readFile(join(scratch, 'raw.log'), 'utf8')).toBe('only raw evidence\n');
 } finally { await rm(scratch, { recursive: true, force: true }); }
});

test('successful finalization records actual scratch removal after the evidence is saved', async () => {
 const scratch = await mkdtemp(join(tmpdir(), 'litter-helper-success-'));
 const evidencePath = await mkdtemp(join(tmpdir(), 'litter-helper-evidence-'));
 try {
  const report = { status: 'passed', cleanup: { failures: [], evidenceFailures: [], remaining: [], scratchRemoved: false } };
  await finalizeArtifacts(report, { scratch, evidencePath });
  const saved = JSON.parse(await readFile(join(evidencePath, 'run.json'), 'utf8'));
  expect(saved.status).toBe('passed');
  expect(saved.cleanup.scratchRemoved).toBe(true);
  expect(saved.cleanup.retainedScratchRemoved).toBe(false);
  expect(report.cleanup.retainedScratchRemoved).toBe(true);
  await expect(readFile(scratch)).rejects.toThrow('ENOENT');
  await expect(readFile(report.cleanup.retainedScratchPath)).rejects.toThrow('ENOENT');
 } finally { await rm(scratch, { recursive: true, force: true }); await rm(evidencePath, { recursive: true, force: true }); }
});

test('a failed second report save retains the raw directory at its recorded path', async () => {
 const scratch = await mkdtemp(join(tmpdir(), 'litter-helper-second-save-'));
 const evidencePath = await mkdtemp(join(tmpdir(), 'litter-helper-evidence-'));
 const report = { status: 'passed', cleanup: { failures: [], evidenceFailures: [], remaining: [], scratchRemoved: false } };
 let saves = 0;
 try {
  await writeFile(join(scratch, 'raw.log'), 'only raw evidence\n');
  await finalizeArtifacts(report, { scratch, evidencePath, save: async (path, value) => { if (++saves === 2) throw new Error('second save denied'); await writeFile(path, value); } });
  const saved = JSON.parse(await readFile(join(evidencePath, 'run.json'), 'utf8'));
  expect(saves).toBe(2);
  expect(report.status).toBe('failed');
  expect(report.cleanup.evidenceFailures).toContain('run.json: second save denied');
  expect(report.cleanup.retainedScratchRemoved).toBe(false);
  expect(saved.status).toBe('finalizing');
  expect(await readFile(join(saved.cleanup.retainedScratchPath, 'raw.log'), 'utf8')).toBe('only raw evidence\n');
 } finally { await rm(scratch, { recursive: true, force: true }); await rm(`${scratch}.retained`, { recursive: true, force: true }); await rm(evidencePath, { recursive: true, force: true }); }
});

test('retained raw disposal failure is reported without claiming successful disposal', async () => {
 const scratch = await mkdtemp(join(tmpdir(), 'litter-helper-disposal-'));
 const evidencePath = await mkdtemp(join(tmpdir(), 'litter-helper-evidence-'));
 const report = { status: 'passed', cleanup: { failures: [], evidenceFailures: [], remaining: [], scratchRemoved: false } };
 try {
  await writeFile(join(scratch, 'raw.log'), 'required raw evidence\n');
  await finalizeArtifacts(report, { scratch, evidencePath, remove: async () => { throw new Error('disposal denied'); } });
  const saved = JSON.parse(await readFile(join(evidencePath, 'run.json'), 'utf8'));
  expect(report.status).toBe('failed');
  expect(saved.status).toBe('failed');
  expect(saved.cleanup.failures).toContain('retained scratch removal: disposal denied');
  expect(saved.cleanup.retainedScratchRemoved).toBe(false);
  expect(await readFile(join(saved.cleanup.retainedScratchPath, 'raw.log'), 'utf8')).toBe('required raw evidence\n');
 } finally { await rm(scratch, { recursive: true, force: true }); await rm(`${scratch}.retained`, { recursive: true, force: true }); await rm(evidencePath, { recursive: true, force: true }); }
});

test('ambiguous tmux launch is not proven exited even when its socket disappears', async () => {
 let attempts = 0;
 const result = await cleanupOwnedProcesses({ launchAttempted: true, processes: [], socketAlive: async () => false, killServer: async () => { attempts++; throw new Error('no server'); }, timeoutMs: 20 });
 expect(attempts).toBe(1);
 expect(result.serverExited).toBeNull();
 expect(result.socketClosed).toBe(true);
 expect(result.killServer).toEqual({ error: 'no server', benignPriorExit: false });
 expect(result.failures).toContain('Private tmux server identity was not captured after a launch attempt.');
});

test('post-creation tmux identity capture failure still shuts down the private server', async () => {
 const scratch = await mkdtemp(join(tmpdir(), 'litter-helper-launch-'));
 const socket = join(scratch, 'tmux.sock');
 const launch = { attempted: false };
 const owned = [];
 const tmux = (...args) => runCommand(['tmux', '-S', socket, '-f', '/dev/null', ...args], { timeoutMs: 1000, cwd: scratch });
 let killAttempts = 0;
 try {
  await expect(launchTmuxSession({ launch, create: async () => {
   const response = await tmux('new-session', '-d', '-P', '-F', '#{pid}|#{pane_pid}', '-s', 'launch', 'sleep', '30');
   for (const pid of response.trim().split('|').map(Number)) owned.push(fixtureIdentity(pid));
   return response;
  }, read() { throw new Error('capture denied'); } })).rejects.toThrow('capture denied');
  expect(launch.attempted).toBe(true);
  expect(launch.serverPID).toBe(owned[0].pid);
  expect(launch.panePID).toBe(owned[1].pid);
  const result = await cleanupOwnedProcesses({ launchAttempted: launch.attempted, processes: [], socket, killServer: async () => { killAttempts++; await tmux('kill-server'); }, timeoutMs: 2000 });
  expect(killAttempts).toBe(1);
  expect(result.serverExited).toBeNull();
  expect(result.socketClosed).toBe(true);
  expect(result.failures).toContain('Private tmux server identity was not captured after a launch attempt.');
  for (const row of owned) expect(fixtureIdentity(row.pid)?.startTicks).not.toBe(row.startTicks);
 } finally { await joinFixtureProcesses(owned); await rm(scratch, { recursive: true, force: true }); }
});

test('a lost successful tmux launch response still triggers private-socket teardown', async () => {
 const scratch = await mkdtemp(join(tmpdir(), 'litter-helper-launch-response-'));
 const socket = join(scratch, 'tmux.sock');
 const launch = { attempted: false };
 const owned = [];
 const tmux = (...args) => runCommand(['tmux', '-S', socket, '-f', '/dev/null', ...args], { timeoutMs: 1000, cwd: scratch });
 let killAttempts = 0;
 try {
  await expect(launchTmuxSession({ launch, create: async () => {
   const response = await tmux('new-session', '-d', '-P', '-F', '#{pid}|#{pane_pid}', '-s', 'launch', 'sleep', '30');
   for (const pid of response.trim().split('|').map(Number)) owned.push(fixtureIdentity(pid));
   throw new Error('launch response lost');
  } })).rejects.toThrow('launch response lost');
  const result = await cleanupOwnedProcesses({ launchAttempted: launch.attempted, processes: [], socket, killServer: async () => { killAttempts++; await tmux('kill-server'); }, timeoutMs: 2000 });
  expect(launch.attempted).toBe(true);
  expect(launch.serverPID).toBeUndefined();
  expect(killAttempts).toBe(1);
  expect(result.serverExited).toBeNull();
  expect(result.socketClosed).toBe(true);
  expect(result.failures).toContain('Private tmux server identity was not captured after a launch attempt.');
 } finally { await joinFixtureProcesses(owned); await rm(scratch, { recursive: true, force: true }); }
});

test('private tmux cleanup joins its exact process independently of the socket', async () => {
 const scratch = await mkdtemp(join(tmpdir(), 'litter-helper-tmux-'));
 const socket = join(scratch, 'tmux.sock');
 const tmux = (...args) => runCommand(['tmux', '-S', socket, '-f', '/dev/null', ...args], { timeoutMs: 1000, cwd: scratch });
 let server;
 let pane;
 const owned = [];
 try {
  const launch = await launchTmuxSession({ launch: { attempted: false }, create: async () => {
   const response = await tmux('new-session', '-d', '-P', '-F', '#{pid}|#{pane_pid}', '-s', 'cleanup', 'sleep', '30');
   for (const pid of response.trim().split('|').map(Number)) owned.push(fixtureIdentity(pid));
   return response;
  } });
  server = launch.server;
  pane = launch.process;
  const result = await cleanupOwnedProcesses({ processes: [pane], server, socket, killServer: () => tmux('kill-server'), timeoutMs: 2000 });
  expect(result.serverExited).toBe(true);
  expect(result.socketClosed).toBe(true);
  expect(result.remaining).toEqual([]);
  expect(result.failures).toEqual([]);
  expect(readOwnedProcess(server.pid)).toBeUndefined();
  expect(readOwnedProcess(pane.pid)).toBeUndefined();
 } finally {
  await joinFixtureProcesses(owned);
  await rm(scratch, { recursive: true, force: true });
 }
});
