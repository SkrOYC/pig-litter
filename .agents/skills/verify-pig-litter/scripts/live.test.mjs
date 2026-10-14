import { test, expect } from 'bun:test';
import { configuration, environment, launchArgs, sessionEvidence, sourceIdentity, configuredAgents, currentInspection, pausedOutcome, pausedRetention, completedDelegation, resumedRecall, cancellationAcknowledged, runCommand } from './live.mjs';

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

async function expectBoundedProcessFailure(options, script, message) {
 let leader;
 let descendant;
 const started = performance.now();
 await expect(runCommand(['sh', '-c', script], {
  timeoutMs: 100,
  ...options,
  onStart(childProcess) { leader = childProcess.pid; },
  onStdout(text, childProcess) {
   const pid = Number(text.trim().split('\n')[0]);
   if (Number.isInteger(pid) && pid > 0) descendant = pid;
   if (options.input) childProcess.stdin.end();
  },
 })).rejects.toThrow(message);
 expect(performance.now() - started).toBeLessThan(1200);
 expect(Number.isInteger(descendant)).toBe(true);
 expect(() => process.kill(leader, 0)).toThrow();
 expect(() => process.kill(descendant, 0)).toThrow();
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
