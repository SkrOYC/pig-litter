import assert from 'node:assert/strict';
import { mkdtemp, mkdir, readFile, rm, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';

const root = resolve(import.meta.dir, '..');
const option = process.argv.indexOf('--pig-bin');
const pigBin = option < 0 ? process.env.PIG_BIN ?? Bun.which('pig') : process.argv[option + 1];
if (!pigBin) throw new Error('pass --pig-bin PATH or put the released pig on PATH');
const scenarioOption = process.argv.indexOf('--scenario');
const scenario = scenarioOption < 0 ? 'core' : process.argv[scenarioOption + 1];
assert.ok(['core', 'nested'].includes(scenario));
const scratch = await mkdtemp(join(tmpdir(), 'litter-stock-'));
const workspace = join(scratch, 'workspace');
const agentDir = join(scratch, 'agent');
const evidence = join(root, '.pstack/evidence/stock-lifecycle', new Date().toISOString().replaceAll(':', '-'));
await mkdir(workspace);
await mkdir(agentDir);
await mkdir(evidence, { recursive: true });
await mkdir(join(workspace, '.pig'));
await writeFile(join(workspace, 'litter.yaml'), 'concurrency: 2\ndepth: 2\nmax_result_bytes: 8192\nagents:\n  scout:\n    instructions: input.txt\n');
await writeFile(join(workspace, 'input.txt'), 'stock fixture read marker\n');
await writeFile(join(workspace, '.pig', 'APPEND_SYSTEM.md'), 'PRIVATE_PROJECT_APPEND_MARKER\n');
await writeFile(join(agentDir, 'APPEND_SYSTEM.md'), 'PRIVATE_AGENT_APPEND_MARKER\n');

const receipts = [];
const held = new Map();
let processHandle;
let server;
let failure;
let callSequence = 0;
let hostStdout = '';
let hostStderr = '';
const parent = { stage: 0, runs: [], resumed: undefined, stopped: undefined, done: false };
const nested = { stage: 0, parent: undefined, grandchild: undefined, rejectedDepth: false, done: false };

function content(value) {
  if (typeof value === 'string') return value;
  if (Array.isArray(value)) return value.filter(block => block.type === 'text').map(block => block.text).join('\n');
  return '';
}

function toolResults(body) { return (body.messages ?? []).filter(message => message.role === 'tool').map(message => JSON.parse(content(message.content))); }

function answer(model, message, calls = []) {
  const delta = calls.length ? { role: 'assistant', tool_calls: calls.map(([name, args], index) => ({ index, id: `call_${++callSequence}`, type: 'function', function: { name, arguments: JSON.stringify(args) } })) } : { role: 'assistant', content: message };
  const base = { id: 'stock-fixture', object: 'chat.completion.chunk', created: 1, model };
  return new Response(`data: ${JSON.stringify({ ...base, choices: [{ index: 0, delta, finish_reason: null }] })}\n\ndata: ${JSON.stringify({ ...base, choices: [{ index: 0, delta: {}, finish_reason: calls.length ? 'tool_calls' : 'stop' }], usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 } })}\n\ndata: [DONE]\n\n`, { headers: { 'Content-Type': 'text/event-stream' } });
}

async function until(check, label) {
  const deadline = Date.now() + 10000;
  while (!check()) {
    assert.ok(Date.now() < deadline, `timed out waiting for ${label}`);
    await Bun.sleep(20);
  }
}

function release() {
  for (const [task, finish] of held) finish(answer('held', `completed ${task} ${'bounded report '.repeat(900)}`));
  held.clear();
}

async function parentRequest(body) {
  const tools = toolResults(body);
  if (parent.stage === 0) {
    assert.equal(tools.length, 0);
    parent.stage = 1;
    return answer(body.model, '', [
      ['litter_spawn', { type: 'scout', task: 'hold-A', name: 'A', model: 'fixture/held' }],
      ['litter_spawn', { type: 'scout', task: 'hold-B', name: 'B', model: 'fixture/held' }],
      ['litter_spawn', { type: 'scout', task: 'hold-C', name: 'C', model: 'fixture/held' }],
    ]);
  }
  if (parent.stage === 1) {
    assert.equal(tools.length, 3);
    receipts.push({ stage: 'first-spawns', results: tools.map(item => ({ state: item.state, code: item.code, report: item.report, child: item.child })) });
    const admitted = tools.filter(item => item.child?.id);
    const rejected = tools.filter(item => item.state === 'rejected');
    assert.equal(admitted.length, 2);
    assert.equal(rejected.length, 1);
    parent.runs = admitted.map(item => ({ id: item.child.id, generation: item.child.generation }));
    assert.equal(new Set(parent.runs.map(run => run.id)).size, 2);
    await until(() => held.size === 2, 'two independent child model requests');
    receipts.push({ stage: 'admission', runs: parent.runs, rejected: rejected[0].code });
    parent.stage = 2;
    return answer(body.model, '', [['litter_list', {}], ['litter_wait', { ...parent.runs[0], timeoutMs: 100 }]]);
  }
  if (parent.stage === 2) {
    assert.equal(tools.length, 5);
    const [listed, timed] = tools.slice(3);
    assert.equal(listed.children.length, 2);
    assert.deepEqual(listed.availableAgentTypes.map(agent => agent.name), ['scout', 'worker']);
    assert.equal(listed.totalAgentTypes, 2);
    assert.ok(listed.availableAgentTypes.every(agent => agent.description.length <= 160 && agent.tools.length <= 4));
    assert.equal(timed.state, 'timed_out');
    assert.equal(held.size, 2);
    release();
    parent.stage = 3;
    return answer(body.model, '', parent.runs.map(run => ['litter_wait', { ...run, timeoutMs: 10000 }]));
  }
  if (parent.stage === 3) {
    assert.equal(tools.length, 7);
    for (const result of tools.slice(5)) {
      assert.equal(result.state, 'completed');
      assert.equal(result.outcome.model, 'fixture/held');
      assert.equal(result.outcome.usage.totalTokens, 30);
      assert.equal(result.outcome.truncated, true);
      assert.ok(Buffer.byteLength(result.outcome.text) <= 8192);
    }
    parent.stage = 4;
    return answer(body.model, '', [['litter_inspect', { id: parent.runs[0].id, transcript: true, offset: 0, limit: 10 }]]);
  }
  if (parent.stage === 4) {
    assert.equal(tools.length, 8);
    const inspection = tools.at(-1);
    assert.ok(inspection.entries.length > 0 && inspection.entries.length <= 10);
    assert.deepEqual(inspection.mailbox, []);
    parent.stage = 5;
    return answer(body.model, '', [['litter_message', { ...parent.runs[0], text: 'resume marker', resume: true }]]);
  }
  if (parent.stage === 5) {
    assert.equal(tools.length, 9);
    parent.resumed = { id: tools.at(-1).child.id, generation: tools.at(-1).child.generation };
    assert.equal(parent.resumed.id, parent.runs[0].id);
    assert.equal(parent.resumed.generation, 2);
    parent.stage = 6;
    return answer(body.model, '', [
      ['litter_message', { ...parent.runs[0], text: 'stale marker', mode: 'follow_up' }],
      ['litter_wait', { ...parent.resumed, timeoutMs: 10000 }],
    ]);
  }
  if (parent.stage === 6) {
    assert.equal(tools.length, 11);
    const [stale, resumed] = tools.slice(9);
    assert.equal(stale.state, 'rejected');
    assert.equal(resumed.state, 'completed');
    parent.stage = 7;
    return answer(body.model, '', [['litter_spawn', { type: 'worker', task: 'worker-write', name: 'writer', model: 'fixture/worker' }]]);
  }
  if (parent.stage === 7) {
    assert.equal(tools.length, 12);
    parent.worker = { id: tools.at(-1).child.id, generation: tools.at(-1).child.generation };
    parent.stage = 8;
    return answer(body.model, '', [['litter_wait', { ...parent.worker, timeoutMs: 10000 }]]);
  }
  if (parent.stage === 8) {
    assert.equal(tools.length, 13);
    assert.equal(tools.at(-1).state, 'completed');
    assert.match(tools.at(-1).outcome.text, /worker readback verified/);
    parent.stage = 9;
    return answer(body.model, '', [['litter_spawn', { type: 'scout', task: 'stop-D', name: 'D', model: 'fixture/held' }]]);
  }
  if (parent.stage === 9) {
    assert.equal(tools.length, 14);
    parent.stopped = { id: tools.at(-1).child.id, generation: tools.at(-1).child.generation };
    parent.stage = 10;
    return answer(body.model, '', [['litter_stop', parent.stopped]]);
  }
  assert.equal(parent.stage, 10);
  assert.equal(tools.length, 15);
  assert.equal(tools.at(-1).stoppedCount, 1);
  parent.done = true;
  receipts.push({ stage: 'done', resumed: parent.resumed, stopped: parent.stopped, stopCount: 1 });
  return answer(body.model, 'stock lifecycle complete');
}

async function parentNested(body) {
  const tools = toolResults(body);
  if (nested.stage === 0) {
    nested.stage = 1;
    return answer(body.model, '', [['litter_spawn', { type: 'scout', task: 'Nested parent task.', name: 'nested-parent', model: 'fixture/nester' }]]);
  }
  if (nested.stage === 1) {
    nested.parent = { id: tools.at(-1).child.id, generation: tools.at(-1).child.generation };
    await until(() => nested.grandchild && nested.rejectedDepth && held.has('nested-grandchild'), 'model-called grandchild and depth rejection');
    nested.stage = 2;
    return answer(body.model, '', [['litter_wait', { ...nested.parent, timeoutMs: 10000 }]]);
  }
  if (nested.stage === 2) {
    assert.equal(tools.at(-1).state, 'completed');
    nested.stage = 3;
    return answer(body.model, '', [['litter_list', {}]]);
  }
  if (nested.stage === 3) {
    const children = tools.at(-1).children;
    assert.equal(children.length, 2);
    assert.equal(children.find(child => child.id === nested.grandchild.id).parent, nested.parent.id);
    nested.stage = 4;
    return answer(body.model, '', [['litter_stop', nested.parent]]);
  }
  if (nested.stage === 4) {
    assert.equal(tools.at(-1).stoppedCount, 1);
    nested.stage = 5;
    return answer(body.model, '', [['litter_wait', { ...nested.grandchild, timeoutMs: 10000 }]]);
  }
  assert.equal(nested.stage, 5);
  assert.equal(tools.at(-1).state, 'stopped');
  nested.done = true;
  receipts.push({ stage: 'nested-done', parent: nested.parent, grandchild: nested.grandchild, rejectedDepth: nested.rejectedDepth, stopCount: 1 });
  return answer(body.model, 'nested stock lifecycle complete');
}

async function nesterRequest(body) {
  const tools = toolResults(body);
  assert.ok((body.tools ?? []).some(tool => tool.function.name === 'litter_spawn'));
  if (tools.length === 0) return answer(body.model, '', [['litter_spawn', { type: 'scout', task: 'Nested grandchild task.', name: 'nested-grandchild', model: 'fixture/grandchild' }]]);
  assert.equal(tools.length, 1);
  assert.equal(tools[0].state, 'starting');
  nested.grandchild = { id: tools[0].child.id, generation: tools[0].child.generation };
  return answer(body.model, 'nested parent complete');
}

async function grandchildRequest(req, body) {
  const tools = toolResults(body);
  if (tools.length === 0) return answer(body.model, '', [['litter_spawn', { type: 'scout', task: 'Forbidden depth three.', name: 'too-deep', model: 'fixture/held' }]]);
  assert.equal(tools.length, 1);
  assert.equal(tools[0].state, 'rejected');
  nested.rejectedDepth = true;
  return await new Promise(resolve => {
    held.set('nested-grandchild', resolve);
    req.signal.addEventListener('abort', () => {
      if (held.get('nested-grandchild') === resolve) held.delete('nested-grandchild');
      resolve(answer(body.model, 'stopped grandchild'));
    }, { once: true });
  });
}

async function childRequest(req, body) {
  const messages = body.messages ?? [];
  assert.ok(!JSON.stringify(messages).includes('PARENT_PRIVATE_MARKER'));
  const user = messages.filter(message => message.role === 'user').map(message => content(message.content)).join('\n');
  assert.deepEqual((body.tools ?? []).map(tool => tool.function.name).sort(), ['read', 'litter_spawn', 'litter_list', 'litter_inspect', 'litter_message', 'litter_stop', 'litter_wait'].sort());
  if (user.includes('resume marker')) {
    assert.match(user, /hold-[AB]/);
    return answer(body.model, 'resume completed with retained history');
  }
  const task = user.match(/hold-[ABC]|stop-D/)?.[0];
  assert.ok(task);
  const tools = (body.messages ?? []).filter(message => message.role === 'tool');
  if (task !== 'stop-D' && tools.length === 0) return answer(body.model, '', [['read', { path: 'input.txt' }]]);
  if (task !== 'stop-D') {
    assert.equal(tools.length, 1);
    assert.match(content(tools.at(-1).content), /stock fixture read marker/);
  }
  return await new Promise(resolve => {
    held.set(task, resolve);
    req.signal.addEventListener('abort', () => {
      if (held.get(task) === resolve) held.delete(task);
      resolve(answer(body.model, 'aborted child'));
    }, { once: true });
  });
}

async function workerRequest(body) {
  const tools = (body.messages ?? []).filter(message => message.role === 'tool');
  assert.deepEqual((body.tools ?? []).map(tool => tool.function.name).sort(), ['read', 'write', 'edit', 'litter_spawn', 'litter_list', 'litter_inspect', 'litter_message', 'litter_stop', 'litter_wait'].sort());
  if (tools.length === 0) return answer(body.model, '', [['write', { path: 'output.txt', content: 'stock worker write marker\n' }]]);
  if (tools.length === 1) return answer(body.model, '', [['read', { path: 'output.txt' }]]);
  assert.equal(tools.length, 2);
  assert.match(content(tools.at(-1).content), /stock worker write marker/);
  return answer(body.model, 'worker readback verified');
}

server = Bun.serve({ hostname: '127.0.0.1', port: 0, idleTimeout: 0, async fetch(req) {
  try {
    const body = await req.json();
    if (!body.model.startsWith('parent')) {
      const messages = JSON.stringify(body.messages);
      assert.ok(!messages.includes('PARENT_PRIVATE_MARKER'));
      assert.ok(!messages.includes('PRIVATE_PROJECT_APPEND_MARKER'));
      assert.ok(!messages.includes('PRIVATE_AGENT_APPEND_MARKER'));
      if (body.model !== 'worker') {
        const systemPrompt = content(body.messages.find(message => message.role === 'system')?.content);
        assert.match(systemPrompt, /^input\.txt(?:\n|$)/, 'an existing instruction path stays literal in the child system prompt');
        assert.ok(!systemPrompt.includes('stock fixture read marker'), 'instruction path contents must not be loaded');
      }
    }
    receipts.push({ model: body.model, roles: (body.messages ?? []).map(message => message.role), tools: (body.tools ?? []).map(tool => tool.function.name) });
    if (body.model === 'parent') return await parentRequest(body);
    if (body.model === 'parent_nested') return await parentNested(body);
    if (body.model === 'held') return await childRequest(req, body);
    if (body.model === 'worker') return await workerRequest(body);
    if (body.model === 'nester') return await nesterRequest(body);
    if (body.model === 'grandchild') return await grandchildRequest(req, body);
    throw new Error(`unexpected fixture model ${body.model}`);
  } catch (error) {
    failure = error;
    return new Response(JSON.stringify({ error: { message: String(error), type: 'fixture_error' } }), { status: 500, headers: { 'Content-Type': 'application/json' } });
  }
} });

await writeFile(join(agentDir, 'models.json'), JSON.stringify({ providers: { fixture: { api: 'openai-completions', baseUrl: `http://127.0.0.1:${server.port}/v1`, apiKey: 'local-fixture-only', models: ['parent', 'held', 'worker', 'parent_nested', 'nester', 'grandchild'].map(id => ({ id, name: id, contextWindow: 100000, maxTokens: 20000 })) } } }));
const env = { PATH: process.env.PATH ?? '', PIG_HOME: scratch, PIG_CODING_AGENT_DIR: agentDir, PIG_USE_PI_DIRS: '0', PIG_OFFLINE: '1', PI_OFFLINE: '1', PI_SKIP_VERSION_CHECK: '1', GOPROXY: 'off', GOENV: 'off', TERM: 'xterm-256color', LANG: 'C.UTF-8' };
function kill(signal) { try { if (processHandle?.pid) process.kill(-processHandle.pid, signal); } catch {} }
try {
  const args = [pigBin, '--no-extensions', '-e', join(root, 'extensions/pig-litter/dist/pig-litter.mjs'), '--no-skills', '--no-prompt-templates', '--no-themes', '--no-context-files', '--no-session', '--approve', '--model', scenario === 'nested' ? 'fixture/parent_nested' : 'fixture/parent', '--mode', 'json', '--print', 'Run the fixture. PARENT_PRIVATE_MARKER'];
  processHandle = Bun.spawn(args, { cwd: workspace, env, stdout: 'pipe', stderr: 'pipe', detached: true });
  const timer = setTimeout(() => kill('SIGKILL'), 60000);
  const [stdout, stderr, code] = await Promise.all([new Response(processHandle.stdout).text(), new Response(processHandle.stderr).text(), processHandle.exited]);
  hostStdout = stdout;
  hostStderr = stderr;
  clearTimeout(timer);
  assert.equal(code, 0, `PiG exited ${code}: ${stderr.slice(-2000)} ${stdout.slice(-1000)}`);
  assert.equal(failure, undefined, String(failure));
  assert.equal(scenario === 'nested' ? nested.done : parent.done, true);
  if (scenario === 'core') assert.equal(await readFile(join(workspace, 'output.txt'), 'utf8'), 'stock worker write marker\n');
  await writeFile(join(evidence, 'result.json'), JSON.stringify({ passed: true, scenario, receipts }, null, 2));
  console.log(`Stock lifecycle passed. Evidence ${evidence}`);
} catch (error) {
  await writeFile(join(evidence, 'failure.txt'), String(error?.stack ?? error).replaceAll('PARENT_PRIVATE_MARKER', '[redacted]').replaceAll('PRIVATE_PROJECT_APPEND_MARKER', '[redacted]').replaceAll('PRIVATE_AGENT_APPEND_MARKER', '[redacted]'));
  throw error;
} finally {
  release();
  server.stop(true);
  kill('SIGTERM');
  await Promise.race([processHandle?.exited ?? Promise.resolve(), Bun.sleep(1000)]);
  kill('SIGKILL');
  await writeFile(join(evidence, 'receipts.json'), JSON.stringify(receipts, null, 2));
  const sanitize = value => value.replaceAll('PARENT_PRIVATE_MARKER', '[redacted]').replaceAll('PRIVATE_PROJECT_APPEND_MARKER', '[redacted]').replaceAll('PRIVATE_AGENT_APPEND_MARKER', '[redacted]').replaceAll('local-fixture-only', '[redacted-key]').slice(-8192);
  await writeFile(join(evidence, 'output.json'), JSON.stringify({ stdoutTail: sanitize(hostStdout), stderrTail: sanitize(hostStderr) }, null, 2));
  await rm(scratch, { recursive: true, force: true });
}
