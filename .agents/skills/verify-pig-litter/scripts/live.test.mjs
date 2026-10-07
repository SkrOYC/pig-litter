import { test, expect } from 'bun:test';
import { configuration, environment, launchArgs, sessionEvidence, completedDelegation, childTranscript, resumedRecall, cancellationAcknowledged, runCommand } from './live.mjs';

const model = 'sample/model';
function session(state = 'completed', childModel = model) {
 return [
  { type: 'session', id: 'parent', cwd: '/scratch' },
  { type: 'message', id: 'call-entry', message: { role: 'assistant', provider: 'sample', model: 'model', stopReason: 'toolUse', content: [{ type: 'toolCall', id: 'child-call', name: 'litter_spawn', arguments: { type: 'scout', task: 'Read input.txt' } }] } },
  { type: 'message', id: 'result-entry', message: { role: 'toolResult', toolCallId: 'child-call', toolName: 'litter_spawn', isError: false, content: [{ type: 'text', text: JSON.stringify({ state: 'starting', child: { id: 'child', generation: 1, type: 'scout', model: childModel } }) }] } },
  { type: 'message', id: 'wait-entry', message: { role: 'assistant', provider: 'sample', model: 'model', stopReason: 'toolUse', content: [{ type: 'toolCall', id: 'wait-call', name: 'litter_wait', arguments: { id: 'child', generation: 1 } }] } },
  { type: 'message', id: 'outcome-entry', message: { role: 'toolResult', toolCallId: 'wait-call', toolName: 'litter_wait', isError: false, content: [{ type: 'text', text: JSON.stringify({ state, outcome: { run: { id: 'child', generation: 1 }, state, model: childModel, text: 'Opaque file token: hidden-token' } }) }] } },
  { type: 'message', id: 'final-entry', message: { role: 'assistant', provider: 'sample', model: 'model', stopReason: 'stop', content: [{ type: 'text', text: 'Done' }] } },
 ].map(record => JSON.stringify(record)).join('\n') + '\n';
}

test('runtime inputs preserve inherited configuration without embedding models', () => {
 const config = configuration(['--piglet', 'my-agent', '--model', model, '--pig-home', '/config', '--case', 'all'], {});
 const env = environment({ ...config, pigBinary: '/selected/bin/pig' }, { PATH: '/bin', HOME: '/user', PIG_CODING_AGENT_DIR: '/agent', API_KEY: 'opaque', PIG_PIGLET_NAME: 'ambient' });
 expect(env).toEqual({ PATH: '/selected/bin:/bin', HOME: '/user', PIG_CODING_AGENT_DIR: '/agent', API_KEY: 'opaque', PIG_HOME: '/config', TERM: 'xterm-256color' });
 expect(configuration([], {}).model).toBeUndefined();
 expect(configuration([], { PIG_HOME: '/inherited' }).pigHome).toBe('/inherited');
 expect(launchArgs(config, 'selected', '/sessions')).not.toContain('--no-builtin-tools');
 expect(launchArgs(config, 'selected', '/sessions')).toContain('read,write,edit,litter_spawn,litter_list,litter_inspect,litter_message,litter_stop,litter_wait');
 expect(launchArgs(config, 'selected', '/sessions')).toContain('my-agent');
 expect(launchArgs(config, 'selected', '/sessions')).not.toContain('-e');
 expect(launchArgs(config, 'plain', '/sessions')).not.toContain('--no-builtin-tools');
 expect(launchArgs(config, 'plain', '/sessions')).not.toContain('--piglet');
 expect(launchArgs(config, 'plain', '/sessions')).not.toContain('--no-skills');
});

test('argument validation bounds command timers', () => {
 expect(() => configuration(['--timeout-ms', '0'], {})).toThrow();
 expect(() => configuration(['--case', 'unknown'], {})).toThrow();
 expect(() => configuration(['--model', 'unqualified'], {})).toThrow();
 expect(() => configuration(['--credentials'], {})).toThrow();
});

test('completion requires an actual matching toolResult and inherited exact model', () => {
 const evidence = sessionEvidence(session());
 expect(completedDelegation(evidence, 'scout', model, 'hidden-token').result.value.state).toBe('completed');
 expect(evidence.header.id).toBe('parent');
 expect(() => completedDelegation(sessionEvidence(session('failed')), 'scout', model, 'hidden-token')).toThrow();
 expect(() => completedDelegation(sessionEvidence(session('completed', 'other/model')), 'scout', model, 'hidden-token')).toThrow();
 expect(() => completedDelegation(evidence, 'scout', model, 'not-in-file')).toThrow();
 evidence.calls[0].arguments.model = model;
 expect(() => completedDelegation(evidence, 'scout', model, 'hidden-token')).toThrow();
});

test('parent prose alone and unfinished turns cannot satisfy the drive', () => {
 const evidence = sessionEvidence(session());
 evidence.results = [];
 expect(completedDelegation(evidence, 'scout', model, 'hidden-token')).toBeUndefined();
 const unfinished = sessionEvidence(session());
 unfinished.assistants.pop();
 expect(completedDelegation(unfinished, 'scout', model, 'hidden-token')).toBeUndefined();
 const cheating = sessionEvidence(session());
 cheating.calls.push({ name: 'read' });
 expect(() => completedDelegation(cheating, 'scout', model, 'hidden-token')).toThrow();
});

test('a real aborted tool result survives evidence parsing and cannot pass completion', () => {
 const lines = session().trim().split('\n').map(line => JSON.parse(line));
 lines[2].message.isError = true;
 lines[2].message.content = [{ type: 'text', text: 'Tool execution aborted' }];
 const evidence = sessionEvidence(lines.map(record => JSON.stringify(record)).join('\n'));
 expect(evidence.results[0].parseError).toBe('Child control result is not JSON.');
 expect(evidence.results[0].errorText).toBe('Tool execution aborted');
 expect(cancellationAcknowledged(evidence, 'child-call')).toBe(true);
 expect(cancellationAcknowledged(evidence, 'another-call')).toBe(false);
 expect(cancellationAcknowledged(sessionEvidence(session()), 'child-call')).toBe(false);
 expect(() => completedDelegation(evidence, 'scout', model, 'hidden-token')).toThrow();
});

test('spawn admission and a different generation cannot prove completion', () => {
 const evidence = sessionEvidence(session());
 evidence.results.pop();
 expect(completedDelegation(evidence, 'scout', model, 'hidden-token')).toBeUndefined();
 const wrongGeneration = sessionEvidence(session());
 wrongGeneration.results[1].value.outcome.run.generation = 2;
 expect(completedDelegation(wrongGeneration, 'scout', model, 'hidden-token')).toBeUndefined();
 expect(completedDelegation(wrongGeneration, 'scout', model, 'hidden-token', 2)).toBeUndefined();
 wrongGeneration.calls[1].arguments.generation = 2;
 expect(completedDelegation(wrongGeneration, 'scout', model, 'hidden-token', 2).outcome.state).toBe('completed');
 const orphanedResult = sessionEvidence(session());
 orphanedResult.calls.pop();
 expect(completedDelegation(orphanedResult, 'scout', model, 'hidden-token')).toBeUndefined();
});

test('hidden completion entries survive the parent session projection', () => {
 const completion = { type: 'custom_message', id: 'completion', customType: 'litter_completion', content: '{"id":"child","generation":1,"state":"completed"}', display: false };
 expect(sessionEvidence(session() + JSON.stringify(completion) + '\n').completions).toEqual([completion]);
});

test('child transcript requires successful file operations and exact model', () => {
 const entries = [
  { type: 'message', message: { role: 'assistant', provider: 'sample', model: 'model', stopReason: 'toolUse', content: [{ type: 'toolCall', id: 'read-call', name: 'read', arguments: { path: 'input.txt' } }] } },
  { type: 'message', message: { role: 'toolResult', toolCallId: 'read-call', toolName: 'read', isError: false, content: [{ type: 'text', text: 'hidden-token' }] } },
 ];
 const child = { id: 'child', generation: 1 };
 const evidence = { calls: [{ id: 'inspect-call', name: 'litter_inspect', arguments: { id: 'child', transcript: true, offset: 0 } }], results: [{ id: 'inspect-call', name: 'litter_inspect', value: { child, entries, more: false } }] };
 expect(childTranscript(evidence, child, model, 'hidden-token', ['read']).calls[0].name).toBe('read');
 expect(() => childTranscript(evidence, child, model, 'hidden-token', ['write'])).toThrow();
 entries[1].message.isError = true;
 expect(() => childTranscript(evidence, child, model, 'hidden-token', ['read'])).toThrow();
 entries[1].message.isError = false;
 evidence.results[0].value.more = true;
 expect(childTranscript(evidence, child, model, 'hidden-token', ['read'])).toBeUndefined();
});

function recallEvidence() {
 const evidence = sessionEvidence(session());
 evidence.calls.push(
  { id: 'resume-call', name: 'litter_message', arguments: { id: 'child', generation: 1, resume: true, text: 'Recall the token without tools.' } },
  { id: 'resume-wait', name: 'litter_wait', arguments: { id: 'child', generation: 2 } },
  { id: 'resume-inspect', name: 'litter_inspect', arguments: { id: 'child', transcript: true, offset: 7 } },
 );
 const outcome = structuredClone(evidence.results[1]);
 outcome.id = 'resume-wait';
 outcome.value.outcome.run.generation = 2;
 evidence.results.push(
  { id: 'resume-call', name: 'litter_message', value: { delivery: 'resumed', child: { id: 'child', generation: 2 } } },
  outcome,
  { id: 'resume-inspect', name: 'litter_inspect', value: { child: { id: 'child', generation: 2 }, more: false, entries: [{ type: 'message', message: { role: 'assistant', provider: 'sample', model: 'model', stopReason: 'stop', content: [{ type: 'text', text: 'hidden-token' }] } }] } },
 );
 return evidence;
}

test('retained recall excludes leaked tokens, skipped reads, and incomplete transcript pages', () => {
 const scout = { child: { id: 'child' }, transcript: { entries: Array(7) } };
 expect(resumedRecall(recallEvidence(), scout, model, 'hidden-token').outcome.run.generation).toBe(2);
 const leaked = recallEvidence();
 leaked.calls[2].arguments.text = 'Recall hidden-token';
 expect(() => resumedRecall(leaked, scout, model, 'hidden-token')).toThrow();
 const skipped = recallEvidence();
 skipped.calls[4].arguments.offset = 9;
 expect(resumedRecall(skipped, scout, model, 'hidden-token')).toBeUndefined();
 const incomplete = recallEvidence();
 incomplete.results[4].value.more = true;
 expect(resumedRecall(incomplete, scout, model, 'hidden-token')).toBeUndefined();
 const reread = recallEvidence();
 reread.results[4].value.entries[0].message.content.push({ type: 'toolCall', id: 'reread', name: 'read', arguments: { path: 'input.txt' } });
 expect(() => resumedRecall(reread, scout, model, 'hidden-token')).toThrow();
});

test('an unrelated aborted assistant does not acknowledge a specific cancelled wait', () => {
 const evidence = sessionEvidence(session());
 evidence.assistants.at(-1).stopReason = 'aborted';
 expect(cancellationAcknowledged(evidence, 'wait-call')).toBe(false);
 evidence.results[1].value = { state: 'cancelled' };
 expect(cancellationAcknowledged(evidence, 'wait-call')).toBe(true);
});

async function expectBoundedProcessFailure(options, script, message) {
 let leader;
 let descendant;
 const started = performance.now();
 await expect(runCommand(['sh', '-c', script], {
  timeoutMs: 100,
  ...options,
  onStart(child) { leader = child.pid; },
  onStdout(text, child) {
   const pid = Number(text.trim().split('\n')[0]);
   if (Number.isInteger(pid) && pid > 0) descendant = pid;
   if (options.input) child.stdin.end();
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
