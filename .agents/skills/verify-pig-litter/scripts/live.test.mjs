import { test, expect } from 'bun:test';
import { configuration, environment, launchArgs, sessionEvidence, completedDelegation, cancellationAcknowledged } from './live.mjs';

const model = 'sample/model';
function session(state = 'completed', childModel = model) {
 return [
  { type: 'session', id: 'parent', cwd: '/scratch' },
  { type: 'message', id: 'call-entry', message: { role: 'assistant', provider: 'sample', model: 'model', stopReason: 'toolUse', content: [{ type: 'toolCall', id: 'child-call', name: 'pig_litter_agent', arguments: { type: 'scout', task: 'Read input.txt' } }] } },
  { type: 'message', id: 'result-entry', message: { role: 'toolResult', toolCallId: 'child-call', toolName: 'pig_litter_agent', isError: false, content: [{ type: 'text', text: JSON.stringify({ state, type: 'scout', model: childModel, report: 'Opaque file token: hidden-token' }) }] } },
  { type: 'message', id: 'final-entry', message: { role: 'assistant', provider: 'sample', model: 'model', stopReason: 'stop', content: [{ type: 'text', text: 'Done' }] } },
 ].map(record => JSON.stringify(record)).join('\n') + '\n';
}

test('runtime inputs preserve inherited configuration without embedding models', () => {
 const config = configuration(['--piglet', 'my-agent', '--model', model, '--pig-home', '/config', '--case', 'all'], {});
 const env = environment({ ...config, pigBinary: '/selected/bin/pig' }, { PATH: '/bin', HOME: '/user', PIG_CODING_AGENT_DIR: '/agent', API_KEY: 'opaque', PIG_PIGLET_NAME: 'ambient' });
 expect(env).toEqual({ PATH: '/selected/bin:/bin', HOME: '/user', PIG_CODING_AGENT_DIR: '/agent', API_KEY: 'opaque', PIG_HOME: '/config', TERM: 'xterm-256color' });
 expect(configuration([], {}).model).toBeUndefined();
 expect(configuration([], { PIG_HOME: '/inherited' }).pigHome).toBe('/inherited');
 expect(launchArgs(config, 'selected', '/sessions')).toContain('--no-builtin-tools');
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
 expect(evidence.results[0].parseError).toBe('Delegation tool result is not JSON.');
 expect(evidence.results[0].errorText).toBe('Tool execution aborted');
 expect(cancellationAcknowledged(evidence, 'child-call')).toBe(true);
 expect(cancellationAcknowledged(evidence, 'another-call')).toBe(false);
 expect(cancellationAcknowledged(sessionEvidence(session()), 'child-call')).toBe(false);
 expect(() => completedDelegation(evidence, 'scout', model, 'hidden-token')).toThrow();
});
