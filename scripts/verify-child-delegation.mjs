import { mkdtemp, mkdir, writeFile, readFile, readdir, rm } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join, resolve } from 'node:path';
import assert from 'node:assert/strict';

const root = resolve(import.meta.dir, '..');
const scratch = await mkdtemp(join(tmpdir(), 'pig-litter-child-'));
const evidence = join(root, '.pstack/evidence/child-delegation', new Date().toISOString().replaceAll(':', '-'));
await mkdir(evidence, { recursive: true });
const cwd = join(scratch, 'workspace');
const home = join(scratch, 'pig-home');
await mkdir(join(home, 'agent'), { recursive: true });
await mkdir(cwd);
await writeFile(join(cwd, 'input.txt'), 'fixture read marker\n');
const env = {
 PATH: process.env.PATH, HOME: process.env.HOME,
 PIG_HOME: home, PIG_CODING_AGENT_DIR: join(home, 'agent'), PIG_USE_PI_DIRS: '0',
 PIG_OFFLINE: '1', PI_OFFLINE: '1', PI_SKIP_VERSION_CHECK: '1',
 GOPROXY: 'off', GOENV: 'off', GOCACHE: join(scratch, 'go-cache'), GOMODCACHE: join(scratch, 'go-mod-cache'),
 TERM: 'xterm-256color', LANG: 'C.UTF-8',
};
const requests = [];
const outcomes = [];
const tmuxSocket = join(scratch, 'tmux.sock');
let cancelRequest;

function reply(model, text, calls = [], reason = 'stop') {
 const delta = calls.length ? { role: 'assistant', tool_calls: calls.map(([name, args], index) => ({ index, id: `call_${index}`, type: 'function', function: { name, arguments: JSON.stringify(args) } })) } : { role: 'assistant', content: text };
 const base = { id: 'fixture', object: 'chat.completion.chunk', created: 1, model };
 return new Response(`data: ${JSON.stringify({ ...base, choices: [{ index: 0, delta, finish_reason: null }] })}\n\ndata: ${JSON.stringify({ ...base, choices: [{ index: 0, delta: {}, finish_reason: calls.length ? 'tool_calls' : reason }], usage: { prompt_tokens: 10, completion_tokens: 5, total_tokens: 15 } })}\n\ndata: [DONE]\n\n`, { headers: { 'Content-Type': 'text/event-stream' } });
}

const server = Bun.serve({ hostname: '127.0.0.1', port: 0, idleTimeout: 0, async fetch(req) {
 const body = await req.json();
 requests.push(body);
 const tools = body.messages.filter(message => message.role === 'tool');
 const userContent = body.messages.findLast(message => message.role === 'user')?.content ?? '';
 const user = typeof userContent === 'string' ? userContent : userContent.filter(block => block.type === 'text').map(block => block.text).join('\n');
 const model = body.model;
 if (model.startsWith('parent_')) {
  if (tools.length) {
   for (const tool of tools) outcomes.push({ scenario: model, result: JSON.parse(tool.content) });
   return reply(model, 'parent complete');
  }
  const scenario = model.slice('parent_'.length);
  const call = { type: scenario === 'worker' || scenario === 'headless' || scenario === 'cancel' ? 'worker' : 'scout', task: 'Read input.txt and report fixture read marker.', model: 'fixture/scout' };
  if (scenario === 'worker') Object.assign(call, { task: 'Write output.txt with fixture write marker and read it back.', model: 'fixture/worker' });
  if (scenario === 'headless') Object.assign(call, { model: 'fixture/worker' });
  if (scenario === 'missing') call.model = 'fixture/missing';
  if (scenario === 'error') call.model = 'fixture/error';
  if (scenario === 'timeout' || scenario === 'cancel') Object.assign(call, { task: `${scenario} fixture. Never write files.`, model: 'fixture/hang', timeoutMs: scenario === 'timeout' ? 1000 : 10000 });
  if (scenario === 'busy') return reply(model, '', [
   ['pig_litter_agent', { type: 'scout', task: 'busy fixture. Never write files.', model: 'fixture/hang', timeoutMs: 1000 }],
   ['pig_litter_agent', call],
  ]);
  return reply(model, '', [['pig_litter_agent', call]]);
 }
 if (model === 'error') return new Response(JSON.stringify({ error: { message: 'fixture provider failure', type: 'fixture_error' } }), { status: 400, headers: { 'Content-Type': 'application/json' } });
 if (model === 'hang') {
  const record = { task: user, disconnected: false };
  if (user.includes('cancel fixture.')) cancelRequest = record;
  req.signal.addEventListener('abort', () => { record.disconnected = true; });
  return await new Promise(resolve => setTimeout(() => resolve(reply(model, '', [['write', { path: 'after-stop.txt', content: 'must never be written' }]])), 30000).unref());
 }
 if (model === 'scout') {
  if (!tools.length) return reply(model, '', [['read', { path: 'input.txt' }]]);
  assert.match(tools.at(-1).content, /fixture read marker/);
  return reply(model, `fixture read marker\n${'bounded report '.repeat(900)}`);
 }
 if (model === 'worker') {
  if (!tools.length) return reply(model, '', [['write', { path: 'output.txt', content: 'fixture write marker\n' }]]);
  if (tools.length === 1) return reply(model, '', [['read', { path: 'output.txt' }]]);
  assert.match(tools.at(-1).content, /fixture write marker/);
  return reply(model, 'fixture write marker verified');
 }
 throw new Error(`unexpected fixture model ${model} ${user}`);
}});

const ids = ['scout','worker','error','hang', ...['scout','worker','missing','error','timeout','busy','headless','cancel'].map(value => `parent_${value}`)];
await writeFile(join(home, 'agent/models.json'), JSON.stringify({ providers: { fixture: { api: 'openai-completions', baseUrl: `http://127.0.0.1:${server.port}/v1`, apiKey: 'local-fixture', models: ids.map(id => ({ id, name: id, contextWindow: 100000, maxTokens: 20000 })) } } }));
const base = ['pig', '--no-extensions', '-e', join(root, 'extensions/pig-litter'), '--no-skills', '--no-prompt-templates', '--no-themes', '--no-context-files', '--no-session', '--approve'];
async function run(args) {
 const process = Bun.spawn(args, { cwd, env, stdout: 'pipe', stderr: 'pipe' });
 const timer = setTimeout(() => process.kill(), 60000);
 const [stdout, stderr, code] = await Promise.all([new Response(process.stdout).text(), new Response(process.stderr).text(), process.exited]);
 clearTimeout(timer);
 assert.equal(code, 0, `${args.join(' ')}\n${stderr}\n${stdout}`);
 return stdout;
}
async function tmux(...args) {return await run(['tmux','-S',tmuxSocket,'-f','/dev/null',...args]);}
async function waitFor(check, label, ms = 45000) {
 const deadline = Date.now() + ms;
 while (!(await check())) {assert.ok(Date.now() < deadline, `timed out waiting for ${label}`); await Bun.sleep(100);}
}
async function processTree(parent) {
 const text = await run(['ps', '-eo', 'pid=,ppid=,args=']);
 const rows = text.trim().split('\n').map(line => {
  const match = line.trim().match(/^(\d+)\s+(\d+)\s+(.*)$/);
  return { pid: Number(match[1]), parent: Number(match[2]), command: match[3] };
 });
 const selected = new Set([parent]);
 let changed = true;
 while (changed) {
  changed = false;
  for (const row of rows) if (selected.has(row.parent) && !selected.has(row.pid)) { selected.add(row.pid); changed = true; }
 }
 return rows.filter(row => selected.has(row.pid));
}
function alive(pid) { try { process.kill(pid, 0); return true; } catch { return false; } }
async function interactive(scenario) {
 const name = `fixture-${scenario}`;
 await tmux('new-session','-d','-s',name,'-x','180','-y','50','--',...base,'--model',`fixture/parent_${scenario}`);
 await tmux('set-option','-t',name,'remain-on-exit','on');
 const parent = Number((await tmux('display-message','-pt',name,'#{pane_pid}')).trim());
 await waitFor(async () => (await tmux("capture-pane", "-pt", name)).includes("foreground scout and worker"), "selected TUI ready");
 const tracked = new Map((await processTree(parent)).map(row => [row.pid, row]));
 await tmux('send-keys','-t',name,'-l','Delegate the fixture task. Parent secret must stay private.');
 await tmux('send-keys','-t',name,'Enter');
 if (scenario !== "cancel") await waitFor(() => outcomes.some(item => item.scenario === `parent_${scenario}`), "real child handback");
 else {
  await waitFor(() => cancelRequest !== undefined, 'interactive child provider request');
  const before = await processTree(parent);
  for (const row of before) tracked.set(row.pid, row);
  const children = before.filter(row => row.command.includes('--mode json') && row.pid !== parent);
  assert.ok(children.length > 0, 'live child PiG observed before Escape');
  await writeFile(join(evidence, 'cancel.processes-before.json'), JSON.stringify(before, null, 2));
  await tmux('send-keys','-t',name,'Escape');
  await waitFor(() => cancelRequest.disconnected, 'child HTTP disconnection after Escape', 8000);
  await waitFor(() => children.every(row => !alive(row.pid)), 'cancelled child processes ended', 8000);
  await writeFile(join(evidence, 'cancel.processes-after.json'), JSON.stringify(await processTree(parent), null, 2));
 }
 for (const row of await processTree(parent)) tracked.set(row.pid, row);
 await writeFile(join(evidence, `${scenario}.screen.txt`), await tmux('capture-pane','-pt',name,'-S','-100'));
 await tmux('send-keys','-t',name,'C-d');
 await waitFor(async () => (await tmux('display-message','-pt',name,'#{pane_dead}')).trim() === '1', 'normal PiG exit', 8000);
 const status = (await tmux('display-message','-pt',name,'#{pane_dead_status}|#{pane_dead_signal}')).trim();
 assert.equal(status, '0|', 'normal PiG exit without terminating signal');
 await waitFor(() => [...tracked.keys()].every(pid => !alive(pid)), 'parent and extension processes ended', 8000);
 await writeFile(join(evidence, `${scenario}.process-exit.json`), JSON.stringify({ status, processes: [...tracked.values()], remaining: [...tracked.keys()].filter(alive) }, null, 2));
 await tmux('kill-session','-t',name);
}

try {
 for (const scenario of ["scout","missing","error","timeout","busy","worker"]) await interactive(scenario);
 for (const scenario of ["headless"]) {
  const stdout = await run([...base, '--model', `fixture/parent_${scenario}`, '--mode','json','--print','Parent secret must stay private. Delegate the fixture task.']);
  await writeFile(join(evidence, `${scenario}.jsonl`), stdout);
 }

 await interactive('cancel');
 const expected = { parent_scout:'completed', parent_missing:'unavailable', parent_error:'failed', parent_timeout:'stopped', parent_headless:'unavailable', parent_worker:'completed' };
 for (const [scenario,state] of Object.entries(expected)) assert.equal(outcomes.find(item => item.scenario === scenario)?.result.state, state, scenario);
 const scout = outcomes.find(item => item.scenario === 'parent_scout').result;
 const busy = outcomes.filter(item => item.scenario === 'parent_busy');
 assert.equal(busy.length, 2);
 assert.equal(busy.filter(item => item.result.state === 'rejected').length, 1, 'only one parallel call launches a child');
 assert.equal(scout.model, 'fixture/scout');
 assert.equal(scout.reportTruncated, true);
 assert.ok(Buffer.byteLength(scout.report) <= 8192);
 assert.equal(scout.usage.totalTokens, 30);
 assert.equal(await readFile(join(cwd,'output.txt'),'utf8'), 'fixture write marker\n');
 await assert.rejects(readFile(join(cwd,'after-stop.txt')));
 const scoutRequest = requests.find(request => request.model === 'scout');
 const workerRequest = requests.find(request => request.model === 'worker');
 assert.deepEqual(scoutRequest.tools.map(tool => tool.function.name), ['read','ls']);
 assert.deepEqual(workerRequest.tools.map(tool => tool.function.name).sort(), ['read','write','edit','ls'].sort());
 assert.match(scoutRequest.messages[0].content, /Pig Litter's scout/);
 assert.match(workerRequest.messages[0].content, /Pig Litter's worker/);
 for (const request of requests.filter(request => ['scout','worker','hang','error'].includes(request.model))) assert.ok(!JSON.stringify(request.messages).includes('Parent secret must stay private'));
 for (const item of outcomes) assert.ok(!('details' in item.result));
 const stateFiles = await readdir(home, { recursive: true });
 assert.deepEqual(stateFiles.filter(path => path.includes('sessions') && path.endsWith('.jsonl')), [], 'no persisted parent or child session');
 await writeFile(join(evidence,'results.json'), JSON.stringify({ passed: true, outcomes, cancellation: cancelRequest, assertions: ['real selected Go tool', 'fresh child messages', 'distinct prompts/models/tool lists', 'real read/write/readback', 'bounded parent handback', 'provider failure', 'missing model', 'timeout', 'interactive cancellation', 'headless delegation unavailable'] }, null, 2));
 console.log(`Child delegation passed. Evidence ${evidence}`);
} catch (error) {
 await writeFile(join(evidence,'failure.txt'), String(error.stack ?? error));
 throw error;
} finally {
 await writeFile(join(evidence,'requests.json'), JSON.stringify(requests,null,2));
 await writeFile(join(evidence,'outcomes.json'), JSON.stringify(outcomes,null,2));
 await tmux('kill-server').catch(() => {});
 server.stop(true);
 await rm(scratch,{recursive:true,force:true});
}
