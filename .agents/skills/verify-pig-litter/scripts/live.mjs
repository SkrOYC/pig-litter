#!/usr/bin/env bun
import { mkdtemp, mkdir, readFile, writeFile, readdir, rm, symlink } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { dirname, join, resolve } from 'node:path';
import { randomUUID } from 'node:crypto';

const repository = resolve(import.meta.dir, '../../../..');
const statusText = 'foreground scout and worker';
const diagnosticText = 'Pig Litter ready. Use pig_litter_agent with scout or worker. Foreground only.';

export function configuration(args, env = process.env) {
 const config = { pigBinary: 'pig', piglet: 'pig-litter', model: undefined, pigHome: env.PIG_HOME, case: 'selected', evidencePath: join(repository, '.pstack/evidence/pig-litter-live'), timeoutMs: 180000, commandTimeoutMs: 120000, doctorOnly: false };
 const options = { '--pig-bin': 'pigBinary', '--piglet': 'piglet', '--model': 'model', '--pig-home': 'pigHome', '--case': 'case', '--evidence-dir': 'evidencePath', '--timeout-ms': 'timeoutMs', '--command-timeout-ms': 'commandTimeoutMs' };
 for (let index = 0; index < args.length; index++) {
  const arg = args[index];
  if (arg === 'doctor') { config.doctorOnly = true; continue; }
  const key = options[arg];
  if (!key || !args[index + 1] || args[index + 1].startsWith('--')) throw new Error(`Invalid argument ${arg}. Use --help for runtime options.`);
  const value = args[++index];
  config[key] = key.endsWith('Ms') ? Number(value) : value;
 }
 if (!['selected', 'direct', 'plain', 'cancel', 'all'].includes(config.case)) throw new Error('Choose selected, direct, plain, cancel, or all for --case.');
 for (const key of ['timeoutMs', 'commandTimeoutMs']) if (!Number.isInteger(config[key]) || config[key] < 1000 || config[key] > 600000) throw new Error(`${key} must be an integer from 1000 to 600000.`);
 if (config.model && !/^[^/\s]+\/.+/.test(config.model)) throw new Error('--model must be an exact provider/model.');
 return config;
}

export function environment(config, inherited) {
 const env = { ...inherited, TERM: 'xterm-256color' };
 delete env.PIG_PIGLET_NAME;
 delete env.PIG_PIGLET_PATH;
 if (config.pigHome) env.PIG_HOME = resolve(config.pigHome);
 env.PATH = `${dirname(config.pigBinary)}:${env.PATH ?? ''}`;
 return env;
}

export function launchArgs(config, kind, sessionDir, rpc = false) {
 const args = ['--offline', '--approve', '--session-dir', sessionDir];
 if (kind !== 'plain') args.push('--no-skills', '--no-prompt-templates', '--no-themes', '--no-context-files');
 if (config.model) args.push('--model', config.model);
 if (kind === 'direct') args.push('-e', join(repository, 'extensions/pig-litter'));
 else if (kind !== 'plain') args.push('--piglet', config.piglet, '--no-builtin-tools');
 if (rpc) args.push('--mode', 'rpc', '--no-session');
 return args;
}

export function sessionEvidence(text) {
 const records = text.split('\n').filter(Boolean).map(line => JSON.parse(line));
 const calls = [];
 const results = [];
 const assistants = [];
 for (const entry of records) {
  const message = entry.type === 'message' ? entry.message : undefined;
  if (message?.role === 'assistant') {
   assistants.push({ provider: message.provider, model: message.model, stopReason: message.stopReason, entryId: entry.id });
   for (const block of message.content ?? []) if (block.type === 'toolCall') calls.push({ id: block.id, name: block.name, arguments: block.arguments, entryId: entry.id });
  }
  if (message?.role === 'toolResult') {
   const content = typeof message.content === 'string' ? message.content : (message.content ?? []).filter(block => block.type === 'text').map(block => block.text).join('\n');
   let value;
   let parseError;
   if (message.toolName === 'pig_litter_agent') {
    try { value = JSON.parse(content); } catch { parseError = 'Delegation tool result is not JSON.'; }
   }
   results.push({ id: message.toolCallId, name: message.toolName, isError: message.isError, value, parseError, errorText: parseError && content.slice(0, 8192), entryId: entry.id });
  }
 }
 return { header: records.find(entry => entry.type === 'session'), modelChanges: records.filter(entry => entry.type === 'model_change').map(entry => ({ provider: entry.provider, model: entry.modelId })), calls, results, assistants };
}

export function completedDelegation(evidence, type, model, token) {
 const call = evidence.calls.findLast(call => call.name === 'pig_litter_agent' && call.arguments?.type === type);
 const result = evidence.results.find(result => result.id === call?.id);
 if (!call || !result) return undefined;
 if (evidence.calls.some(call => call.name !== 'pig_litter_agent')) throw new Error('Parent called another tool; this does not prove child delegation.');
 if (call.arguments.model) throw new Error('The child call set a model instead of testing parent model inheritance.');
 if (result.isError || result.value?.state !== 'completed') throw new Error(`Child ${type} returned ${result.value?.state ?? 'an invalid result'}.`);
 if (result.value.type !== type || result.value.model !== model) throw new Error('Child type or effective model does not match the selected run.');
 if (Buffer.byteLength(result.value.report ?? '') > 8192 || !result.value.report?.includes(token)) throw new Error('Child handback lacks the expected file token or exceeds the report limit.');
 if (evidence.assistants.some(message => `${message.provider}/${message.model}` !== model)) throw new Error('Parent assistant used a different model than the doctor observed.');
 if (evidence.assistants.at(-1)?.stopReason !== 'stop') return undefined;
 return { call, result };
}

export function cancellationAcknowledged(evidence, callId) {
 return evidence.results.some(result => result.id === callId && (result.value?.state === 'stopped' || result.isError && /abort|cancel/i.test(result.errorText ?? result.value?.report ?? '')))
  || evidence.assistants.at(-1)?.stopReason === 'aborted';
}

function ensure(condition, message) { if (!condition) throw new Error(message); }
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

export async function drive(config) {
 config = { ...config, pigBinary: Bun.which(config.pigBinary) ?? resolve(config.pigBinary), evidencePath: resolve(config.evidencePath) };
 const scratch = await mkdtemp(join(tmpdir(), 'pig-litter-live-'));
 const childBinDirectory = join(scratch, 'bin');
 await mkdir(childBinDirectory);
 await symlink(config.pigBinary, join(childBinDirectory, 'pig'));
 const evidencePath = join(config.evidencePath, `${new Date().toISOString().replaceAll(':', '-')}-${randomUUID().slice(0, 8)}`);
 await mkdir(evidencePath, { recursive: true });
 const env = environment(config, process.env);
 env.PATH = `${childBinDirectory}:${env.PATH}`;
 const socket = join(scratch, 'tmux.sock');
 const owned = new Map();
 const panes = [];
 const report = { status: 'running', configuration: { ...config, model: config.model ?? 'inherited' }, evidencePath, cases: [], cleanup: { remaining: [], scratchRemoved: false } };
 let active;
 let interrupted;
 const pendingCommands = new Set();
 const interrupt = signal => { interrupted = signal; for (const child of pendingCommands) child.kill(); };
 const interruptHandler = () => interrupt('SIGINT');
 const terminateHandler = () => interrupt('SIGTERM');
 process.on('SIGINT', interruptHandler);
 process.on('SIGTERM', terminateHandler);

 async function command(args, cwd = scratch) {
  const child = Bun.spawn(args, { cwd, env, stdin: 'ignore', stdout: 'pipe', stderr: 'pipe' });
  pendingCommands.add(child);
  const timer = setTimeout(() => child.kill(), config.commandTimeoutMs);
  try {
   const [stdout, stderr, code] = await Promise.all([new Response(child.stdout).text(), new Response(child.stderr).text(), child.exited]);
   ensure(stdout.length + stderr.length <= 2 * 1024 * 1024, 'Metadata command output exceeds 2 MiB.');
   ensure(code === 0, `${args[0]} ${args[1] ?? ''} failed with exit ${code}. Check the selected PiG configuration root and installed source.`);
   return stdout;
  } finally { clearTimeout(timer); pendingCommands.delete(child); }
 }
 const tmux = (...args) => command(['tmux', '-S', socket, '-f', '/dev/null', ...args]);
 async function processes(root) {
  const output = await command(['ps', '-eo', 'pid=,ppid=,lstart=,args=']);
  const rows = output.split('\n').flatMap(line => {
   const match = line.trim().match(/^(\d+)\s+(\d+)\s+(\w{3}\s+\w{3}\s+\d+\s+[\d:]+\s+\d{4})\s+(.*)$/);
   return match ? [{ pid: Number(match[1]), parent: Number(match[2]), started: match[3], command: match[4] }] : [];
  });
  const selected = new Set([root]);
  let changed = true;
  while (changed) { changed = false; for (const row of rows) if (selected.has(row.parent) && !selected.has(row.pid)) { selected.add(row.pid); changed = true; } }
  const tree = rows.filter(row => selected.has(row.pid));
  for (const row of tree) owned.set(`${row.pid}:${row.started}`, row);
  return tree;
 }
 async function alive(row) {
  const text = await command(['ps', '-eo', 'pid=,lstart=']);
  return text.split('\n').some(line => line.trim().replace(/\s+/g, ' ') === `${row.pid} ${row.started}`.replace(/\s+/g, ' '));
 }
 async function wait(check, label, timeout = config.timeoutMs) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
   ensure(!interrupted, `Drive interrupted by ${interrupted}.`);
   if (active) {
    const tree = await processes(active.pid);
    active.processes = [...new Map([...active.processes, ...tree].map(row => [`${row.pid}:${row.started}`, row])).values()];
   }
   const value = await check();
   if (value) return value;
   await sleep(100);
  }
  throw new Error(`Timed out waiting for ${label}.`);
 }
 async function rpcProbe(args, cwd) {
  const child = Bun.spawn([config.pigBinary, ...args], { cwd, env, stdin: 'pipe', stdout: 'pipe', stderr: 'pipe' });
  pendingCommands.add(child);
  const responses = new Map();
  let pending = '';
  let bytes = 0;
  const stderr = new Response(child.stderr).text();
  const pump = (async () => {
   for await (const chunk of child.stdout) {
    bytes += chunk.length;
    ensure(bytes <= 2 * 1024 * 1024, 'Doctor RPC output exceeds 2 MiB.');
    pending += new TextDecoder().decode(chunk);
    const lines = pending.split('\n'); pending = lines.pop();
    for (const line of lines) {
     let record; try { record = JSON.parse(line); } catch { continue; }
     if (record.type === 'response') responses.set(record.id, record);
    }
   }
  })();
  for (const type of ['get_state', 'get_available_models', 'get_commands']) child.stdin.write(`${JSON.stringify({ id: type, type })}\n`);
  try {
   await wait(async () => { await processes(child.pid); return responses.size === 3; }, 'selected runtime doctor responses', config.commandTimeoutMs);
   for (const response of responses.values()) ensure(response.success, `Doctor RPC ${response.command} failed.`);
   return Object.fromEntries([...responses].map(([id, response]) => [id, response.data]));
  } finally {
   child.stdin.end();
   const timer = setTimeout(() => child.kill(), 3000);
   await child.exited; clearTimeout(timer);
   pendingCommands.delete(child);
   await pump; await stderr;
  }
 }
 async function cleanup() {
  for (const pane of panes) {
   const pid = Number(await tmux('display-message', '-pt', pane, '#{pane_pid}').catch(() => '0'));
   if (pid) await processes(pid).catch(() => {});
  }
  await tmux('kill-server').catch(() => {});
  for (const row of owned.values()) if (await alive(row)) { try { process.kill(row.pid, 'SIGTERM'); } catch {} }
  await sleep(300);
  for (const row of owned.values()) if (await alive(row)) { try { process.kill(row.pid, 'SIGKILL'); } catch {} }
  await sleep(100);
  report.cleanup.remaining = (await Promise.all([...owned.values()].map(async row => await alive(row) ? row : undefined))).filter(Boolean);
 }
 async function doctor(kind, workspace, sessions) {
  const version = (await command([config.pigBinary, '--version'], workspace)).trim();
  await command(['tmux', '-V']);
  const status = JSON.parse(await command([config.pigBinary, 'status', '--json', '--no-input'], workspace));
  const source = kind === 'plain' || kind === 'direct' ? undefined : JSON.parse(await command([config.pigBinary, 'piglet', 'show', config.piglet, '--json'], workspace));
  const runtime = await rpcProbe(launchArgs(config, kind, sessions, true), workspace);
  const model = runtime.get_state?.model;
  const selected = `${model?.provider}/${model?.id}`;
  ensure(model?.provider && model?.id, `No model selected under PiG home ${status.paths?.home}. Supply --model or check inherited settings.`);
  ensure(!config.model || config.model === selected, 'Selected runtime model differs from --model.');
  ensure(runtime.get_available_models?.models?.some(model => `${model.provider}/${model.id}` === selected), `Model ${selected} is unavailable under ${status.paths?.home}. Check PIG_HOME and any inherited agent-directory override.`);
  const auth = JSON.parse(await command([config.pigBinary, 'auth', 'check', '--model', selected, '--json', '--no-refresh'], workspace));
  ensure(auth.status === 'ready', `Model readiness is ${auth.status} under ${status.paths?.home}. Check the active configuration root and agent directory.`);
  const commands = (runtime.get_commands?.commands ?? []).map(command => command.name);
  ensure(kind === 'plain' || commands.includes('pig-litter'), 'Selected runtime did not register /pig-litter.');
  return { version, paths: status.paths, model: selected, commands, source: source && { name: source.name, path: source.source, extensions: source.piglet?.extensions?.map(extension => ({ name: extension.name, origins: extension.origins })) }, auth: { status: auth.status, provider: auth.provider } };
 }
 async function sessionProjection(sessionDir) {
  const files = (await readdir(sessionDir, { recursive: true }).catch(() => [])).filter(path => path.endsWith('.jsonl'));
  if (files.length !== 1) return undefined;
  const path = join(sessionDir, files[0]);
  const text = await readFile(path, 'utf8');
  if (!text.endsWith('\n')) return undefined;
  return { path, text, evidence: sessionEvidence(text) };
 }
 async function screen(name, label) {
  const text = await tmux('capture-pane', '-pt', name, '-S', '-200');
  await writeFile(join(evidencePath, `${name}.${label}.txt`), text);
  return text;
 }
 async function send(name, text) {
  active.actions.push({ text, at: new Date().toISOString() });
  await tmux('send-keys', '-t', name, '-l', text);
  await tmux('send-keys', '-t', name, 'Enter');
 }
 async function runCase(kind) {
  const workspace = join(scratch, kind, 'workspace');
  const sessions = join(scratch, kind, 'sessions');
  await mkdir(workspace, { recursive: true }); await mkdir(sessions, { recursive: true });
  const receipt = { name: kind, status: 'running', actions: [], processes: [], workspace, sessionDir: sessions };
  report.cases.push(receipt);
  receipt.doctor = await doctor(kind, workspace, sessions);
  if (config.doctorOnly) { receipt.status = 'passed'; return; }
  const args = launchArgs(config, kind, sessions);
  receipt.arguments = args;
  await tmux('new-session', '-d', '-s', kind, '-c', workspace, '-x', '180', '-y', '50', '--', config.pigBinary, ...args);
  panes.push(kind);
  await tmux('set-option', '-t', kind, 'remain-on-exit', 'on');
  receipt.pid = Number((await tmux('display-message', '-pt', kind, '#{pane_pid}')).trim());
  active = receipt;
  await wait(async () => {
   ensure((await tmux('display-message', '-pt', kind, '#{pane_dead}')).trim() === '0', 'PiG exited during startup.');
   const text = await screen(kind, 'startup');
   return kind === 'plain' ? text.includes(receipt.doctor.model.split('/').at(-1)) : text.includes(statusText);
  }, 'healthy TUI startup', config.commandTimeoutMs);
  if (kind === 'plain') {
   const text = await screen(kind, 'plain');
   ensure(!text.includes(statusText) && !text.includes(diagnosticText), 'Plain PiG unexpectedly selected Pig Litter through ordinary discovery.');
  } else {
   await send(kind, '/pig-litter');
   await wait(async () => (await screen(kind, 'diagnostic')).includes(diagnosticText), '/pig-litter notification', 10000);
  }
  if (kind === 'selected' || kind === 'cancel') {
   const inputToken = `scout-${randomUUID()}`;
   await writeFile(join(workspace, 'input.txt'), `${inputToken}\n`);
   const type = kind === 'cancel' ? 'worker' : 'scout';
   const task = kind === 'cancel' ? 'Read input.txt repeatedly and report its token. Do not write files.' : 'Read input.txt and report its complete token. Do not change files.';
   await send(kind, `Call pig_litter_agent exactly once with type ${type} and task ${JSON.stringify(task)}. Omit model so the child inherits your model. Do not use another tool.`);
  if (kind === 'cancel') {
    const children = await wait(async () => { const tree = await processes(receipt.pid); return tree.filter(row => row.command.includes('--mode json') && row.command.includes('--no-session')).length ? tree.filter(row => row.command.includes('--mode json') && row.command.includes('--no-session')) : undefined; }, 'live child before Escape');
    receipt.cancelledChildren = children;
    const before = await sessionProjection(sessions);
    receipt.cancelledCall = before?.evidence.calls.findLast(call => call.name === 'pig_litter_agent');
    ensure(receipt.cancelledCall, 'Live child lacks an actual parent delegation toolCall record.');
    receipt.actions.push({ key: 'Escape', at: new Date().toISOString() });
    await tmux('send-keys', '-t', kind, 'Escape');
    await wait(async () => !(await Promise.all(children.map(alive))).some(Boolean), 'cancelled child process exit', 10000);
    const beforeDiagnostic = await screen(kind, 'before-responsiveness');
    const notificationCount = beforeDiagnostic.split(diagnosticText).length - 1;
    await send(kind, '/pig-litter');
    await wait(async () => (await screen(kind, 'after-cancel')).split(diagnosticText).length - 1 > notificationCount, 'new notification after cancellation', 10000);
    const after = await wait(async () => { const session = await sessionProjection(sessions); return session && cancellationAcknowledged(session.evidence, receipt.cancelledCall.id) ? session : undefined; }, 'recorded parent cancellation acknowledgement', 10000);
    receipt.cancellation = { callId: receipt.cancelledCall.id, childExitObserved: true, newNotificationObserved: true, results: after?.evidence.results.filter(result => result.id === receipt.cancelledCall.id), assistantStopReasons: after?.evidence.assistants.map(message => message.stopReason), files: await readdir(workspace) };
    ensure(receipt.cancellation.files.length === 1 && receipt.cancellation.files[0] === 'input.txt', 'Cancellation task unexpectedly wrote workspace files.');
   } else {
    const scout = await wait(async () => { const session = await sessionProjection(sessions); return session && completedDelegation(session.evidence, 'scout', receipt.doctor.model, inputToken); }, 'completed scout handback');
    receipt.scout = scout;
    const outputToken = `worker-${randomUUID()}`;
    await send(kind, `Call pig_litter_agent exactly once with type worker and task ${JSON.stringify(`Write output.txt containing exactly ${outputToken} followed by a newline, then read it back and report that token.`)}. Omit model so the child inherits your model. Do not use another tool.`);
    receipt.worker = await wait(async () => { const session = await sessionProjection(sessions); return session && completedDelegation(session.evidence, 'worker', receipt.doctor.model, outputToken); }, 'completed worker handback');
    ensure(await readFile(join(workspace, 'output.txt'), 'utf8') === `${outputToken}\n`, 'Actual worker file content differs from the requested marker.');
    ensure(receipt.processes.some(row => row.command.includes('--mode json') && row.command.includes('--tools read,ls')), 'No live scout child process was observed.');
    ensure(receipt.processes.some(row => row.command.includes('--mode json') && row.command.includes('--tools read,write,edit,ls')), 'No live worker child process was observed.');
    receipt.fileEffects = { input: inputToken, output: outputToken };
   }
  }
  const session = await sessionProjection(sessions);
  if (session) {
   ensure(session.evidence.header?.cwd === workspace, 'Parent session provenance points outside the scratch workspace.');
   receipt.sessionEvidence = session.evidence;
   await writeFile(join(evidencePath, `${kind}.session.jsonl`), session.text);
  }
  await screen(kind, 'before-exit');
  receipt.actions.push({ key: 'Ctrl+D', at: new Date().toISOString() });
  await tmux('send-keys', '-t', kind, 'C-d');
  await wait(async () => (await tmux('display-message', '-pt', kind, '#{pane_dead}')).trim() === '1', 'normal PiG exit', 10000);
  receipt.exit = (await tmux('display-message', '-pt', kind, '#{pane_dead_status}|#{pane_dead_signal}')).trim();
  ensure(receipt.exit === '0|', 'PiG did not exit normally with status zero.');
  await wait(async () => !(await Promise.all(receipt.processes.map(alive))).some(Boolean), 'parent and extension process exit', 10000);
  await tmux('kill-session', '-t', kind);
  receipt.status = 'passed';
  active = undefined;
 }
 try {
  const cases = config.case === 'all' ? ['selected', 'direct', 'plain'] : [config.case];
  for (const kind of cases) await runCase(kind);
  report.status = 'passed';
 } catch (error) {
  report.status = report.cases.at(-1)?.doctor ? 'failed' : 'blocked';
  if (report.cases.length) report.cases.at(-1).status = report.status;
  report.failure = String(error.message);
  if (active) {
   await screen(active.name, 'failure').catch(() => {});
   const paths = (await readdir(active.sessionDir, { recursive: true }).catch(() => [])).filter(path => path.endsWith('.jsonl'));
   for (let index = 0; index < paths.length; index++) await writeFile(join(evidencePath, `${active.name}.failure-${index}.session.jsonl`), await readFile(join(active.sessionDir, paths[index])));
   const projection = await sessionProjection(active.sessionDir).catch(() => undefined);
   if (projection) await writeFile(join(evidencePath, `${active.name}.failure.session.jsonl`), projection.text);
   await cleanup().catch(error => { report.cleanup.failure = error.message; });
   active = undefined;
   const failed = report.cases.at(-1);
   if (!interrupted && !report.cleanup.remaining.length) {
    try { report.doctorAfterFailure = await doctor(failed.name, failed.workspace, join(scratch, 'failure-doctor')); } catch (error) { report.doctorAfterFailure = { status: 'failed', reason: error.message }; }
   }
  }
 } finally {
  await cleanup().catch(error => { report.cleanup.failure = error.message; report.status = 'failed'; });
  if (report.cleanup.remaining.length) report.status = 'failed';
  if (!report.cleanup.remaining.length && !report.cleanup.failure) {
   await rm(scratch, { recursive: true, force: true });
   report.cleanup.scratchRemoved = true;
  }
  await writeFile(join(evidencePath, 'run.json'), JSON.stringify(report, null, 2));
  process.off('SIGINT', interruptHandler);
  process.off('SIGTERM', terminateHandler);
 }
 console.log(`${report.status} ${evidencePath}`);
 if (report.status !== 'passed') process.exitCode = 1;
 return report;
}

if (import.meta.main) {
 if (process.argv.includes('--help')) console.log('Usage: live.mjs [doctor] [--pig-bin PATH] [--piglet NAME|PATH] [--model PROVIDER/MODEL] [--pig-home PATH] [--case selected|direct|plain|cancel|all] [--evidence-dir PATH] [--timeout-ms N] [--command-timeout-ms N]\nInherits PiG configuration and credentials. Doctor performs metadata/build probes without a model request. Selected and cancel cases make real model requests.');
 else await drive(configuration(process.argv.slice(2)));
}
