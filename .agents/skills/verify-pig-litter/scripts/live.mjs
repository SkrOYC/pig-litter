#!/usr/bin/env bun
import { access, mkdtemp, mkdir, readFile, writeFile, readdir, rename, rm } from 'node:fs/promises';
import { readFileSync } from 'node:fs';
import { createConnection } from 'node:net';
import { tmpdir } from 'node:os';
import { dirname, isAbsolute, join, resolve } from 'node:path';
import { randomUUID } from 'node:crypto';

const repository = resolve(import.meta.dir, '../../../..');
const diagnosticText = 'Use litter_list to discover named agents and children. Use litter_spawn to start a background child, then inspect, message, stop, or wait by ID.';
export const liveWidgetPattern = /^ *Litter [1-9]\d* (?:active|live)\b/m;
const controlTools = ['litter_spawn', 'litter_list', 'litter_inspect', 'litter_message', 'litter_stop', 'litter_wait'];
const retainedOutcomeStates = new Set(['completed', 'partial', 'failed', 'unavailable', 'stopped', 'paused']);

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
 else if (kind !== 'plain') args.push('--piglet', config.piglet);
 if (kind !== 'plain') args.push('--tools', ['read', 'write', 'edit', 'bash', ...controlTools].join(','));
 if (rpc) args.push('--mode', 'rpc', '--no-session');
 return args;
}

export function sessionEvidence(text) {
 const records = text.split('\n').filter(Boolean).map(line => JSON.parse(line));
 const calls = [];
 const results = [];
 const assistants = [];
 for (const [position, entry] of records.entries()) {
  const message = entry.type === 'message' ? entry.message : undefined;
  if (message?.role === 'assistant') {
   assistants.push({ provider: message.provider, model: message.model, stopReason: message.stopReason, entryId: entry.id, position });
   for (const block of message.content ?? []) if (block.type === 'toolCall') calls.push({ id: block.id, name: block.name, arguments: block.arguments, entryId: entry.id, position });
  }
  if (message?.role === 'toolResult') {
   const content = typeof message.content === 'string' ? message.content : (message.content ?? []).filter(block => block.type === 'text').map(block => block.text).join('\n');
   let value;
   let parseError;
   if (controlTools.includes(message.toolName)) {
    try { value = JSON.parse(content); } catch { parseError = 'Child control result is not JSON.'; }
   }
   results.push({ id: message.toolCallId, name: message.toolName, isError: message.isError, value, parseError, errorText: parseError && content.slice(0, 8192), entryId: entry.id, position });
  }
 }
 return { header: records.find(entry => entry.type === 'session'), modelChanges: records.filter(entry => entry.type === 'model_change').map(entry => ({ provider: entry.provider, model: entry.modelId })), calls, results, assistants };
}

export function sourceIdentity(requested, cwd, source) {
 ensure(source && typeof source.source === 'string' && source.source.length > 0, `Selected Piglet ${requested} did not report its source path.`);
 const localPath = isAbsolute(requested) || requested.includes('/') || requested.endsWith('.yaml') || requested.endsWith('.yml');
 const resolved = resolve(cwd, source.source);
 if (localPath) ensure(resolved === resolve(cwd, requested), `PiG resolved ${requested} to a different Piglet source at ${resolved}.`);
 else ensure(source.name === requested, `PiG resolved ${requested} as ${source.name ?? 'an unnamed Piglet'}.`);
 return { ...source, resolvedPath: resolved };
}

function assertIdOnlyControls(evidence) {
 const allowed = {
  litter_inspect: ['id'],
  litter_wait: ['id', 'timeoutMs'],
  litter_message: ['id', 'text', 'mode', 'resume'],
  litter_stop: ['id'],
 };
 for (const call of evidence.calls) {
  const keys = allowed[call.name];
  if (!keys) continue;
  ensure(Object.keys(call.arguments ?? {}).every(key => keys.includes(key)), `${call.name} used an unsupported control field.`);
  ensure(typeof call.arguments.id === 'string' && call.arguments.id.length > 0, `${call.name} did not identify a child by ID.`);
 }
 for (const call of evidence.calls.filter(call => call.name === 'litter_spawn')) ensure(!('generation' in (call.arguments ?? {})), 'Child spawn supplied a generation target.');
}

export function currentInspection(evidence, id) {
 assertIdOnlyControls(evidence);
 const result = evidence.results.findLast(result => {
  const call = evidence.calls.find(call => call.id === result.id && call.name === 'litter_inspect' && call.arguments.id === id);
  return call && result.name === call.name;
 });
 if (!result) return undefined;
 if (result.isError) throw new Error('ID-only inspection failed.');
 ensure(Object.keys(result.value ?? {}).sort().join(',') === 'child,handbackTruncated,operation,outcome,state', 'Inspection exposed fields beyond current status, child, and latest outcome.');
 ensure(result.value.operation === 'litter_inspect' && result.value.handbackTruncated === false, 'Inspection handback is misidentified or truncated.');
 ensure(result.value.child?.id === id && !('generation' in result.value.child), 'Inspection did not return the requested ID-only child.');
 if (result.value.outcome) ensure(result.value.outcome.run?.id === id && Object.keys(result.value.outcome.run).join(',') === 'id', 'Inspection outcome did not match the requested ID.');
 return { result, child: result.value.child, outcome: result.value.outcome };
}

export function pausedOutcome(evidence, id) {
 assertIdOnlyControls(evidence);
 const result = evidence.results.findLast(result => {
  const call = evidence.calls.find(call => call.id === result.id && call.name === 'litter_stop' && call.arguments.id === id);
  return call && result.name === call.name;
 });
 if (!result) return undefined;
 if (result.isError) throw new Error('ID-only pause failed.');
 ensure(Object.keys(result.value ?? {}).sort().join(',') === 'handbackTruncated,operation,outcome,state,stoppedCount', 'Pause handback contains unexpected fields.');
 ensure(result.value.operation === 'litter_stop' && result.value.handbackTruncated === false, 'Pause handback is misidentified or truncated.');
 ensure(Number.isInteger(result.value.stoppedCount) && result.value.stoppedCount >= 0, 'Pause handback omitted its nonnegative newly paused active-turn count.');
 ensure(retainedOutcomeStates.has(result.value.outcome?.state), 'Pause did not retain a terminal outcome.');
 ensure(result.value.state === result.value.outcome.state, 'Pause status and latest outcome disagree.');
 ensure(result.value.outcome.run?.id === id && Object.keys(result.value.outcome.run).join(',') === 'id', 'Paused outcome did not match the requested ID.');
 return { result, outcome: result.value.outcome };
}

export function pausedRetention(evidence, id, cancelledCallId) {
 const inspection = currentInspection(evidence, id);
 if (!inspection) return undefined;
 const waitCall = evidence.calls.find(call => call.id === cancelledCallId && call.name === 'litter_wait' && call.arguments?.id === id);
 const cancelled = evidence.results.find(result => result.id === cancelledCallId && result.name === 'litter_wait');
 if (!waitCall || !cancelled) return undefined;
 ensure(cancellationAcknowledged(evidence, cancelledCallId), 'Escape did not cancel the pending child wait.');
 ensure(waitCall.position < cancelled.position && cancelled.position < inspection.result.position, 'Paused inspection did not follow the cancelled wait.');
 const admission = evidence.results.findLast(result => result.name === 'litter_spawn' && result.value?.child?.id === id);
 ensure(admission && !admission.isError && admission.value.operation === 'litter_spawn' && admission.value.handbackTruncated === false && evidence.calls.some(call => call.id === admission.id && call.name === 'litter_spawn'), 'Paused inspection did not retain the admitted child ID.');
 ensure(!evidence.calls.some(call => call.name === 'litter_message' && call.arguments?.id === id && call.arguments.resume === true && call.position < inspection.result.position), 'Child resumed before its paused status was inspected.');
 ensure(inspection.result.value.state === 'paused' && inspection.child.state === 'paused' && inspection.outcome?.state === 'paused', 'Escape did not retain the active child as paused.');
 ensure(inspection.outcome.run?.id === id && Object.keys(inspection.outcome.run).join(',') === 'id', 'Paused inspection changed the child session ID.');
 return { cancelled, inspection, child: inspection.child, outcome: inspection.outcome };
}

export function configuredAgents(evidence) {
 const calls = evidence.calls.filter(call => call.name === 'litter_list');
 if (!calls.length) return undefined;
 ensure(calls.length === 1, 'Parent must discover agent definitions once.');
 const result = evidence.results.find(result => result.id === calls[0].id && result.name === 'litter_list');
 if (!result) return undefined;
 ensure(!result.isError, 'Agent discovery failed.');
 const definitions = result.value?.availableAgentTypes;
 ensure(Array.isArray(definitions), 'Agent discovery did not return available agent types.');
 function requireAgent(name, role, tool, leaf = false) {
  const agent = definitions.find(agent => agent.name === name);
  ensure(agent && agent.role === role, `Agent ${name} is not configured as ${role}.`);
  if (leaf) ensure(agent.canDelegate === false, `Agent ${name} must have delegation disabled.`);
  if (tool) ensure(agent.tools?.includes(tool), `Agent ${name} does not expose ${tool}.`);
  return { name: agent.name, role: agent.role, canDelegate: agent.canDelegate, tools: agent.tools };
 }
 return { scout: requireAgent('scout', 'scout', 'read', true), worker: requireAgent('worker', 'worker', 'bash') };
}

export function completedDelegation(evidence, type, model, token, { cwd, operation = 'wait', afterPosition = -1 } = {}) {
 assertIdOnlyControls(evidence);
 const call = evidence.calls.findLast(call => call.name === 'litter_spawn' && call.arguments?.type === type);
 const admission = evidence.results.findLast(result => result.id === call?.id && result.name === 'litter_spawn');
 if (!call || !admission) return undefined;
 if (evidence.calls.some(call => !controlTools.includes(call.name))) throw new Error('Parent called another tool; this does not prove child delegation.');
 if (call.arguments.model) throw new Error('The child call set a model instead of testing parent model inheritance.');
 ensure(typeof call.arguments.task === 'string' && !call.arguments.task.includes(token), 'Private file token was included in the parent child task.');
 if (admission.isError || !admission.value?.child?.id) throw new Error(`Child ${type} admission returned ${admission.value?.state ?? 'an invalid result'}.`);
 ensure(Object.keys(admission.value ?? {}).sort().join(',') === 'child,handbackTruncated,operation,state', 'Spawn handback contains unexpected fields.');
 ensure(admission.value.operation === 'litter_spawn' && admission.value.handbackTruncated === false, 'Spawn handback is misidentified or truncated.');
 const child = admission.value.child;
 if ('generation' in child) throw new Error('Child admission exposed a generation instead of an ID-only control target.');
 if (child.type !== type || child.model !== model) throw new Error('Child type or effective model does not match the selected run.');
 if (cwd) {
  ensure(call.arguments.cwd === cwd, 'Child spawn did not receive the required explicit working directory.');
  ensure(child.cwd === cwd, 'Child snapshot did not preserve its explicit working directory.');
 }
 const result = evidence.results.findLast(result => {
  if (result.position <= afterPosition) return false;
  const matchingCall = evidence.calls.find(call => call.id === result.id && call.name === `litter_${operation}` && call.arguments?.id === child.id);
  if (!matchingCall || matchingCall.position <= afterPosition) return false;
  const outcome = result.value?.outcome;
  return result.name === matchingCall.name && outcome?.run?.id === child.id && !('generation' in outcome.run);
 });
 if (!result) return undefined;
 const outcome = result.value.outcome;
 if (result.isError || outcome.state !== 'completed') throw new Error(`Child ${type} returned ${outcome.state ?? 'an invalid result'}.`);
 if (Object.keys(outcome.run ?? {}).join(',') !== 'id') throw new Error('Child outcome run identity contains fields beyond its ID.');
 if (operation === 'wait') {
  ensure(Object.keys(result.value).sort().join(',') === 'handbackTruncated,operation,outcome,state', 'Wait handback contains unexpected fields.');
  ensure(result.value.operation === 'litter_wait' && result.value.handbackTruncated === false, 'Wait handback is misidentified or truncated.');
 }
 if (operation === 'inspect') {
  ensure(Object.keys(result.value).sort().join(',') === 'child,handbackTruncated,operation,outcome,state', 'Inspection returned fields beyond current status, child, and latest outcome.');
  ensure(result.value.operation === 'litter_inspect' && result.value.handbackTruncated === false, 'Inspection handback is misidentified or truncated.');
 }
 if (result.value.state !== outcome.state) throw new Error('Child status and latest outcome disagree.');
 if (outcome.model !== model) throw new Error('Child outcome model does not match the selected run.');
 if (Buffer.byteLength(outcome.text ?? '') > 50000 || !outcome.text?.includes(token)) throw new Error('Child handback lacks the expected file token or exceeds the report limit.');
 if (typeof outcome.truncated !== 'boolean' || outcome.truncated) throw new Error('Child handback was truncated or omitted its truncation status.');
 if (!outcome.usage || typeof outcome.usage !== 'object') throw new Error('Child outcome omitted usage.');
 if (outcome.error) throw new Error('Child outcome reported an error.');
 if (outcome.reportProvenance !== 'child_assistant') throw new Error('Child handback does not identify its report provenance.');
 if (evidence.assistants.some(message => `${message.provider}/${message.model}` !== model)) throw new Error('Parent assistant used a different model than the doctor observed.');
 if (evidence.assistants.at(-1)?.stopReason !== 'stop') return undefined;
 const resultCall = evidence.calls.find(call => call.id === result.id);
 return { call, admission, result, resultCall, child, outcome };
}

export function cancellationAcknowledged(evidence, callId) {
 return evidence.results.some(result => result.id === callId && (result.value?.state === 'cancelled' || result.isError && /abort|cancel/i.test(result.errorText ?? result.value?.report ?? '')));
}

export function resumedRecall(evidence, scout, model, token, afterPosition = -1) {
 assertIdOnlyControls(evidence);
 const message = evidence.results.findLast(result => {
  if (result.position <= afterPosition) return false;
  const call = evidence.calls.find(call => call.id === result.id && call.name === 'litter_message' && call.arguments?.id === scout.child.id && call.arguments.resume === true);
  if (!call || call.position <= afterPosition || result.isError || result.value?.delivery !== 'resumed') return false;
  ensure(typeof call.arguments.text === 'string' && !call.arguments.text.includes(token), 'Resume message supplied the private token instead of testing retained recall.');
  return result.value.child?.id === scout.child.id && !('generation' in result.value.child);
 });
 if (!message) return undefined;
 ensure(Object.keys(message.value).sort().join(',') === 'child,delivery,handbackTruncated,operation,state', 'Resume acknowledgement contains unexpected fields.');
 ensure(message.value.operation === 'litter_message' && message.value.handbackTruncated === false, 'Resume acknowledgement is misidentified or truncated.');
 const messageCall = evidence.calls.find(call => call.id === message.id);
 const completed = completedDelegation(evidence, 'scout', model, token, { afterPosition: message.position });
 if (!completed) return undefined;
 ensure(completed.resultCall.position > message.position && completed.result.position > message.position, 'Resumed completion arrived before its resume acknowledgement.');
 ensure(messageCall?.position < completed.resultCall.position, 'Resumed wait started before its resume acknowledgement.');
 return { message, ...completed };
}

function ensure(condition, message) { if (!condition) throw new Error(message); }
const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

export function parseProcessStat(text) {
 const match = text.match(/^(\d+) \(([\s\S]*)\) ([\s\S]*)$/);
 const fields = match?.[3].trim().split(/\s+/);
 ensure(fields?.length >= 20 && /^\d+$/.test(fields[19]), 'Malformed Linux process identity.');
 return { pid: Number(match[1]), parent: Number(fields[1]), group: Number(fields[2]), state: fields[0], startTicks: fields[19], command: match[2] };
}

export function readOwnedProcess(pid) {
 try { return parseProcessStat(readFileSync(`/proc/${pid}/stat`, 'utf8')); }
 catch (error) { if (['ENOENT', 'ESRCH'].includes(error.code)) return undefined; throw error; }
}

function sameProcess(owner, current) { return current?.pid === owner.pid && current.startTicks === owner.startTicks; }

export function signalOwnedProcess(owner, signal, { read = readOwnedProcess, send = process.kill } = {}) {
 if (!sameProcess(owner, read(owner.pid))) return false;
 try { send(owner.pid, signal); return true; }
 catch (error) { if (error.code === 'ESRCH' && !sameProcess(owner, read(owner.pid))) return false; throw error; }
}

async function processRows() {
 const pids = (await readdir('/proc')).filter(name => /^\d+$/.test(name));
 return pids.flatMap(pid => { const row = readOwnedProcess(Number(pid)); return row ? [row] : []; });
}

function socketListening(socket) {
 return new Promise((resolve, reject) => {
  const connection = createConnection(socket);
  connection.once('connect', () => { connection.destroy(); resolve(true); });
  connection.once('error', error => { if (['ENOENT', 'ECONNREFUSED'].includes(error.code)) resolve(false); else reject(error); });
  connection.setTimeout(100, () => { connection.destroy(); reject(new Error('Private tmux socket inspection timed out.')); });
 });
}

export async function launchTmuxSession({ launch, create, read = readOwnedProcess }) {
 launch.attempted = true;
 const response = (await create()).trim();
 ensure(/^[1-9]\d*\|[1-9]\d*$/.test(response), 'Private tmux launch did not return server and pane PIDs.');
 [launch.serverPID, launch.panePID] = response.split('|').map(Number);
 launch.server = read(launch.serverPID);
 ensure(launch.server, 'Private tmux server exited before its process identity was captured.');
 launch.process = read(launch.panePID);
 ensure(launch.process, 'PiG exited before its process identity was captured.');
 return launch;
}

export async function cleanupOwnedProcesses({ processes = [], server, launchAttempted = Boolean(server), socket, killServer, read = readOwnedProcess, send = process.kill, socketAlive = () => socketListening(socket), timeoutMs = 2000 }) {
 const rows = [...new Map([...processes, ...(server ? [server] : [])].map(row => [`${row.pid}:${row.startTicks}`, row])).values()];
 const failures = new Set();
 const result = { remaining: rows, serverExited: server ? false : launchAttempted ? null : true, socketClosed: false, failures: [], killServer: undefined };
 if (launchAttempted && !server) failures.add('Private tmux server identity was not captured after a launch attempt.');
 function alive(row) {
  try { return sameProcess(row, read(row.pid)); }
  catch (error) { failures.add(`process ${row.pid}: ${error.message}`); return true; }
 }
 async function inspect() {
  result.remaining = rows.filter(alive);
  result.serverExited = server ? !alive(server) : launchAttempted ? null : true;
  try { result.socketClosed = !await socketAlive(); }
  catch (error) { failures.add(`tmux socket: ${error.message}`); result.socketClosed = false; }
 }
 await inspect();
 if (launchAttempted && (!server || !result.serverExited || !result.socketClosed)) {
  try { await killServer(); }
  catch (error) {
   await inspect();
   const benignPriorExit = result.serverExited === true && result.socketClosed;
   result.killServer = { error: error.message, benignPriorExit };
   if (!benignPriorExit) failures.add(`kill-server: ${error.message}`);
  }
 } else if (!result.socketClosed) failures.add('Private tmux socket is live without its captured server identity.');
 for (const signal of ['SIGTERM', 'SIGKILL']) {
  for (const row of rows) {
   try { signalOwnedProcess(row, signal, { read, send }); }
   catch (error) { failures.add(`${signal} ${row.pid}: ${error.message}`); }
  }
  if (signal === 'SIGTERM') await sleep(75);
 }
 const deadline = Date.now() + timeoutMs;
 do {
  await inspect();
  if (!result.remaining.length && result.socketClosed) break;
  await sleep(20);
 } while (Date.now() < deadline);
 if (result.remaining.length) failures.add(`Owned processes still running: ${result.remaining.map(row => row.pid).join(', ')}.`);
 if (!result.socketClosed) failures.add('Private tmux socket is still live.');
 result.failures = [...failures];
 return result;
}

export async function saveFailureArtifacts({ name, sessionDir, evidencePath, capture, list = readdir, read = readFile, save = writeFile }) {
 const failures = [];
 try { await capture(); } catch (error) { failures.push(`failure screen: ${error.message}`); }
 let paths;
 try { paths = (await list(sessionDir, { recursive: true })).filter(path => path.endsWith('.jsonl')); }
 catch (error) { failures.push(`native log listing: ${error.message}`); return failures; }
 for (const [index, path] of paths.entries()) {
  try { await save(join(evidencePath, `${name}.failure-${index}.session.jsonl`), await read(join(sessionDir, path))); }
  catch (error) { failures.push(`${path}: ${error.message}`); }
 }
 return failures;
}

export async function finalizeArtifacts(report, { scratch, evidencePath, save = writeFile, remove = rm }) {
 report.cleanup.retainedScratchPath = `${scratch}.retained`;
 report.cleanup.retainedScratchRemoved = false;
 if (report.cleanup.remaining.length || report.cleanup.failures.length || report.cleanup.evidenceFailures.length) report.status = 'failed';
 const persist = async (value = report) => {
  try { await save(join(evidencePath, 'run.json'), JSON.stringify(value, null, 2)); return true; }
  catch (error) { report.cleanup.evidenceFailures.push(`run.json: ${error.message}`); report.status = 'failed'; return false; }
 };
 if (!await persist(report.status === 'passed' ? { ...report, status: 'finalizing' } : report) || report.status !== 'passed') return;
 try { await rename(scratch, report.cleanup.retainedScratchPath); report.cleanup.scratchRemoved = true; }
 catch (error) { report.cleanup.failures.push(`scratch retention: ${error.message}`); report.status = 'failed'; await persist(); return; }
 if (!await persist()) return;
 try { await remove(report.cleanup.retainedScratchPath, { recursive: true, force: true }); report.cleanup.retainedScratchRemoved = true; }
 catch (error) { report.cleanup.failures.push(`retained scratch removal: ${error.message}`); report.status = 'failed'; await persist(); }
}

export async function runCommand(args, { cwd, env, timeoutMs, signal, input, onStdout, onStart, maxOutputBytes = 2 * 1024 * 1024 } = {}) {
 ensure(process.platform === 'linux', 'The live driver requires Linux /proc process identities.');
 const child = Bun.spawn(args, { cwd, env, detached: true, stdin: input ? 'pipe' : 'ignore', stdout: 'pipe', stderr: 'pipe' });
 onStart?.(child);
 const readers = [child.stdout.getReader(), child.stderr.getReader()];
 let bytes = 0;
 let deadlineTimer;
 let rejectDeadline;
 const deadline = new Promise((_, reject) => { rejectDeadline = reject; deadlineTimer = setTimeout(() => reject(new Error(`Command timed out after ${timeoutMs} ms.`)), timeoutMs); });
 const abort = () => rejectDeadline(new Error('Command interrupted.'));
 signal?.addEventListener('abort', abort, { once: true });
 async function consume(reader, stdout) {
  const decoder = new TextDecoder();
  let text = '';
  while (true) {
   const chunk = await reader.read();
   if (chunk.done) return text + decoder.decode();
   bytes += chunk.value.length;
   ensure(bytes <= maxOutputBytes, 'Command output exceeds its byte limit.');
   const decoded = decoder.decode(chunk.value, { stream: true });
   text += decoded;
   if (stdout) onStdout?.(decoded, child);
  }
 }
 function members() {
  const snapshot = Bun.spawnSync(['ps', '-eo', 'pid=,ppid=,pgid=,stat='], { timeout: 500, maxBuffer: 2 * 1024 * 1024 });
  ensure(snapshot.exitCode === 0, 'Could not inspect the owned command process group.');
  return snapshot.stdout.toString().trim().split('\n').flatMap(line => {
   const [pid, , group] = line.trim().split(/\s+/);
   if (Number(group) !== child.pid) return [];
   const row = readOwnedProcess(Number(pid));
   return row?.group === child.pid ? [row] : [];
  });
 }
 async function teardown() {
  try {
  for (let round = 0; round < 3; round++) {
   const tree = members();
   if (!tree.length) break;
   const leaves = tree.filter(row => row.pid !== child.pid && !tree.some(other => other.parent === row.pid));
   if (!leaves.length) break;
   for (const row of leaves) signalOwnedProcess(row, round === 0 ? 'SIGTERM' : 'SIGKILL');
   await sleep(75);
  }
  for (const row of members()) signalOwnedProcess(row, 'SIGTERM');
  if (members().length) await sleep(75);
  } finally {
   for (const row of members()) signalOwnedProcess(row, 'SIGKILL');
   await Promise.race([Promise.allSettled([child.exited, ...readers.map(reader => reader.cancel())]), sleep(500)]);
  }
  const remaining = members();
  ensure(!remaining.length, `Owned command group still has processes: ${remaining.map(row => row.pid).join(', ')}.`);
 }
 const operation = Promise.all([consume(readers[0], true), consume(readers[1], false), child.exited]);
 try {
  if (input) child.stdin.write(input);
  if (signal?.aborted) abort();
  const [stdout, , code] = await Promise.race([operation, deadline]);
  ensure(code === 0, `${args[0]} ${args[1] ?? ''} failed with exit ${code}. Check the selected PiG configuration root and installed source.`);
  return stdout;
 } finally {
  clearTimeout(deadlineTimer);
  signal?.removeEventListener('abort', abort);
  try { child.stdin?.end(); } catch {}
  await teardown();
 }
}

export async function drive(config) {
 config = { ...config, pigBinary: Bun.which(config.pigBinary) ?? resolve(config.pigBinary), evidencePath: resolve(config.evidencePath) };
 const scratch = await mkdtemp(join(tmpdir(), 'pig-litter-live-'));
 const evidencePath = join(config.evidencePath, `${new Date().toISOString().replaceAll(':', '-')}-${randomUUID().slice(0, 8)}`);
 await mkdir(evidencePath, { recursive: true });
 const env = environment(config, process.env);
 const socket = join(scratch, 'tmux.sock');
 const owned = new Map();
 let launch = { attempted: false };
 const report = { status: 'running', configuration: { ...config, model: config.model ?? 'inherited' }, evidencePath, cases: [], cleanup: { remaining: [], failures: [], evidenceFailures: [], attempts: [], scratchRemoved: false } };
 let active;
 let interrupted;
 const pendingCommands = new Set();
 const interrupt = signal => { interrupted = signal; for (const controller of pendingCommands) controller.abort(); };
 const interruptHandler = () => interrupt('SIGINT');
 const terminateHandler = () => interrupt('SIGTERM');
 process.on('SIGINT', interruptHandler);
 process.on('SIGTERM', terminateHandler);

 async function command(args, cwd = scratch) {
  const controller = new AbortController();
  pendingCommands.add(controller);
  try {
   return await runCommand(args, { cwd, env, timeoutMs: config.commandTimeoutMs, signal: controller.signal });
  } finally { pendingCommands.delete(controller); }
 }
 const tmux = (...args) => command(['tmux', '-S', socket, '-f', '/dev/null', ...args]);
 async function processes(root) {
  if (!alive(root)) return [];
  const rows = await processRows();
  if (!alive(root)) return [];
  const selected = new Set([root.pid]);
  let changed = true;
  while (changed) { changed = false; for (const row of rows) if (selected.has(row.parent) && !selected.has(row.pid)) { selected.add(row.pid); changed = true; } }
  const tree = rows.filter(row => selected.has(row.pid));
  for (const row of tree) owned.set(`${row.pid}:${row.startTicks}`, row);
  return tree;
 }
 function alive(row) { return sameProcess(row, readOwnedProcess(row.pid)); }
 async function wait(check, label, timeout = config.timeoutMs) {
  const deadline = Date.now() + timeout;
  while (Date.now() < deadline) {
   ensure(!interrupted, `Drive interrupted by ${interrupted}.`);
   if (active) {
    const tree = await processes(active.process);
    active.processes = [...new Map([...active.processes, ...tree].map(row => [`${row.pid}:${row.startTicks}`, row])).values()];
   }
   const value = await check();
   if (value) return value;
   await sleep(100);
  }
  throw new Error(`Timed out waiting for ${label}.`);
 }
 async function rpcProbe(args, cwd) {
  const controller = new AbortController();
  pendingCommands.add(controller);
  const responses = new Map();
  let pending = '';
  try {
   await runCommand([config.pigBinary, ...args], { cwd, env, timeoutMs: config.commandTimeoutMs, signal: controller.signal,
    input: ['get_state', 'get_available_models', 'get_commands'].map(type => JSON.stringify({ id: type, type }) + '\n').join(''),
    onStdout(chunk, child) {
    pending += chunk;
    const lines = pending.split('\n'); pending = lines.pop();
    for (const line of lines) {
     let record; try { record = JSON.parse(line); } catch { continue; }
     if (record.type === 'response') responses.set(record.id, record);
    }
    if (responses.size === 3) child.stdin.end();
   }
   });
   ensure(responses.size === 3, 'Doctor RPC ended without its metadata responses.');
   for (const response of responses.values()) ensure(response.success, `Doctor RPC ${response.command} failed.`);
   return Object.fromEntries([...responses].map(([id, response]) => [id, response.data]));
  } finally {
   pendingCommands.delete(controller);
  }
 }
 async function cleanup() {
  for (const receipt of report.cases) {
   const root = receipt.process;
   try { if (root && alive(root)) await processes(root); }
   catch (error) { report.cleanup.failures.push(`process snapshot: ${error.message}`); }
  }
  const result = await cleanupOwnedProcesses({ processes: [...owned.values()], server: launch.server, launchAttempted: launch.attempted, socket, killServer: () => tmux('kill-server') });
  report.cleanup.attempts.push(result);
  report.cleanup.launch = { ...launch };
  report.cleanup.remaining = result.remaining;
  report.cleanup.serverExited = result.serverExited;
  report.cleanup.socketClosed = result.socketClosed;
  report.cleanup.failures.push(...result.failures);
 }
 async function doctor(kind, workspace, sessions) {
  const version = (await command([config.pigBinary, '--version'], workspace)).trim();
  await command(['tmux', '-V']);
  const status = JSON.parse(await command([config.pigBinary, 'status', '--json', '--no-input'], workspace));
  const source = kind === 'plain' || kind === 'direct' ? undefined : JSON.parse(await command([config.pigBinary, 'piglet', 'show', config.piglet, '--json'], workspace));
  const selectedSource = source && sourceIdentity(config.piglet, workspace, source);
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
  const resolvedSources = [];
  for (const entry of runtime.get_commands?.commands ?? []) if (entry.name === 'pig-litter') {
   const path = entry.sourceInfo?.path;
   const revision = path && await command(['git', '-C', path, 'rev-parse', 'HEAD']).catch(() => undefined);
   resolvedSources.push({ path, revision: revision?.trim(), sourceInfo: entry.sourceInfo });
  }
  return { version, paths: status.paths, model: selected, commands, resolvedSources, source: selectedSource && { name: selectedSource.name, path: selectedSource.source, resolvedPath: selectedSource.resolvedPath, extensions: selectedSource.piglet?.extensions?.map(extension => ({ name: extension.name, origins: extension.origins })) }, auth: { status: auth.status, provider: auth.provider } };
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
  launch = { attempted: false };
  await launchTmuxSession({ launch, create: () => tmux('new-session', '-d', '-P', '-F', '#{pid}|#{pane_pid}', '-s', kind, '-c', workspace, '-x', '180', '-y', '50', '--', config.pigBinary, ...args) });
  owned.set(`${launch.server.pid}:${launch.server.startTicks}`, launch.server);
  await tmux('set-option', '-t', kind, 'remain-on-exit', 'on');
  receipt.pid = launch.panePID;
  receipt.process = launch.process;
  receipt.processes = await processes(receipt.process);
  active = receipt;
  await wait(async () => {
   ensure((await tmux('display-message', '-pt', kind, '#{pane_dead}')).trim() === '0', 'PiG exited during startup.');
   const text = await screen(kind, 'startup');
   return text.includes(receipt.doctor.model.split('/').at(-1)) && text.includes(workspace);
  }, 'healthy TUI startup', config.commandTimeoutMs);
  if (kind === 'plain') {
   const text = await screen(kind, 'plain');
   receipt.ambientSelection = receipt.doctor.commands.includes('pig-litter');
   ensure(!receipt.ambientSelection && !text.includes(diagnosticText) && !/Litter \d+ (?:active|live|children)\b/.test(text), 'Plain PiG selected Pig Litter through ordinary discovery; inspect the recorded configuration and command inventory.');
  } else {
   await send(kind, '/pig-litter help');
   await wait(async () => (await screen(kind, 'diagnostic')).includes(diagnosticText), '/pig-litter help notification', 10000);
  }
  if (kind === 'selected' || kind === 'cancel') {
   const inputToken = `scout-${randomUUID()}`;
   const inputPath = join(workspace, 'input.txt');
   await writeFile(inputPath, `${inputToken}\n`);
   async function progress(label, check) {
    return await wait(async () => {
     const text = await screen(kind, label);
     if (liveWidgetPattern.test(text)) {
      receipt.liveWidgetObserved = true;
      await writeFile(join(evidencePath, `${kind}.live-widget.txt`), text);
     }
     const session = await sessionProjection(sessions);
     if (!session) return undefined;
     ensure(session.evidence.calls.every(call => controlTools.includes(call.name)), 'Parent called a tool outside the child controls.');
     ensure(session.evidence.assistants.every(message => `${message.provider}/${message.model}` === receipt.doctor.model), 'Parent used a different model.');
     const agents = configuredAgents(session.evidence);
     if (!agents) return undefined;
     receipt.agents = agents;
     return check(session.evidence);
    }, label);
   }
   const onlyControls = 'Use only litter_* controls yourself. Do not call file tools yourself. Omit model in spawn so children inherit your model.';
   if (kind === 'cancel') {
    await send(kind, `Call litter_list once to discover the scout and worker agents. Spawn one scout named cancel-proof with task ${JSON.stringify('Read input.txt in 1000 separate read tool calls. Do not delegate or write. Report the token only after all reads.')} and then call litter_wait with its returned ID and timeoutMs 300000. ${onlyControls}`);
    const before = await progress('before-cancel', evidence => {
     const admission = evidence.results.find(result => result.name === 'litter_spawn' && result.value?.child?.name === 'cancel-proof');
     const call = evidence.calls.find(call => call.name === 'litter_wait' && call.arguments?.id === admission?.value?.child?.id);
     return admission && call && !evidence.results.some(result => result.id === call.id) && receipt.liveWidgetObserved ? { admission, call } : undefined;
    });
    receipt.cancelledCall = before.call;
    receipt.cancelledChild = before.admission.value.child;
    ensure(!('generation' in receipt.cancelledChild), 'Cancellation child snapshot exposed a generation.');
    const spawn = (await sessionProjection(sessions)).evidence.calls.find(call => call.id === before.admission.id);
    ensure(!spawn.arguments.model && receipt.cancelledChild.model === receipt.doctor.model, 'Cancellation child did not inherit the parent model.');
    receipt.actions.push({ key: 'Escape', at: new Date().toISOString() });
    await tmux('send-keys', '-t', kind, 'Escape');
    await progress('cancel-acknowledgement', evidence => cancellationAcknowledged(evidence, before.call.id));
    const beforeDiagnostic = await screen(kind, 'before-responsiveness');
    const notificationCount = beforeDiagnostic.split(diagnosticText).length - 1;
    await send(kind, '/pig-litter help');
    await wait(async () => (await screen(kind, 'after-cancel')).split(diagnosticText).length - 1 > notificationCount, 'new notification after cancellation', 10000);
    const child = receipt.cancelledChild;
    await send(kind, `Inspect child ${child.id} by ID. Report its current status and latest outcome only. Do not resume, wait, or spawn. ${onlyControls}`);
    const pause = await progress('paused-retention', evidence => {
     const retained = pausedRetention(evidence, child.id, before.call.id);
     return retained && evidence.assistants.at(-1)?.stopReason === 'stop' ? retained : undefined;
    });
    receipt.cancellation = { callId: before.call.id, waitCancelled: true, childPaused: true, automaticResumeObserved: false, newNotificationObserved: true, childId: child.id, state: pause.inspection.result.value.state, outcomeState: pause.outcome.state, inspectionPosition: pause.inspection.result.position };
    await send(kind, `Explicitly resume paused child ${child.id} with litter_message resume true and text ${JSON.stringify('The repeated-read exercise is over. Read input.txt once, report its complete token, then stop. Do not repeat the earlier reads, delegate, or write.')}. Then wait for the same ID with litter_wait timeoutMs 120000. Use only its latest status and outcome. ${onlyControls}`);
    const resumed = await progress('resume-progress', evidence => resumedRecall(evidence, { child }, receipt.doctor.model, inputToken, receipt.cancellation.inspectionPosition));
    ensure(resumed.child.id === child.id, 'Explicit resume changed the stable child session ID.');
    receipt.resume = { childId: resumed.child.id, resumeAcknowledged: true, outcome: { state: resumed.outcome.state, model: resumed.outcome.model, truncated: resumed.outcome.truncated, reportProvenance: resumed.outcome.reportProvenance }, fileTokenReturned: resumed.outcome.text.includes(inputToken) };
    receipt.cancellation.files = await readdir(workspace);
    ensure(receipt.cancellation.files.length === 1 && receipt.cancellation.files[0] === 'input.txt', 'Cancellation task unexpectedly wrote workspace files.');
   } else {
    await send(kind, `Call litter_list once to discover agents. Spawn exactly one scout named scout-proof with task ${JSON.stringify('Read input.txt and report its complete token. Do not change files or delegate.')} and wait for its returned ID with litter_wait timeoutMs 120000. Use only its latest status and outcome. ${onlyControls}`);
    const scout = await progress('scout-progress', evidence => {
     const completed = completedDelegation(evidence, 'scout', receipt.doctor.model, inputToken);
     return completed;
    });
    ensure(scout.resultCall.name === 'litter_wait', 'Scout completion did not come from litter_wait.');
    receipt.scout = { child: { id: scout.child.id, type: scout.child.type, model: scout.child.model, state: scout.child.state }, outcome: { state: scout.outcome.state, model: scout.outcome.model, truncated: scout.outcome.truncated, reportProvenance: scout.outcome.reportProvenance } };
    await rm(inputPath);
    await access(inputPath).then(() => { throw new Error('Scout input remained available before resume.'); }, error => { if (error.code !== 'ENOENT') throw error; });
    receipt.scoutInputRemoved = true;
    const workerCwd = join(workspace, "worker's workspace");
    const workerInput = `worker-${randomUUID()}`;
    await mkdir(workerCwd, { recursive: true });
    await writeFile(join(workerCwd, 'private-input.txt'), workerInput);
    await writeFile(join(workerCwd, 'worker-output.txt'), 'Disposable worker output.\n');
    await send(kind, `Spawn exactly one worker named worker-proof with cwd ${JSON.stringify(workerCwd)} and task ${JSON.stringify("Use a bash terminal command only. Run `cat private-input.txt > worker-output.txt && printf '\\n' >> worker-output.txt && cat worker-output.txt`, then include that output verbatim in your report. Do not use file tools or delegate.")} and wait for its returned ID with litter_wait timeoutMs 120000. Use only its latest status and outcome. ${onlyControls}`);
    receipt.worker = await progress('worker-progress', evidence => {
     return completedDelegation(evidence, 'worker', receipt.doctor.model, workerInput, { cwd: workerCwd });
    });
    ensure(receipt.worker.resultCall.name === 'litter_wait', 'Worker completion did not come from litter_wait.');
    ensure(await readFile(join(workerCwd, 'worker-output.txt'), 'utf8') === `${workerInput}\n`, 'Worker output does not contain the private input read by the child.');
    receipt.worker = { child: { id: receipt.worker.child.id, type: receipt.worker.child.type, model: receipt.worker.child.model, cwd: receipt.worker.child.cwd, state: receipt.worker.child.state }, outcome: { state: receipt.worker.outcome.state, model: receipt.worker.outcome.model, truncated: receipt.worker.outcome.truncated, reportProvenance: receipt.worker.outcome.reportProvenance }, fileEffect: { input: 'private-input.txt', output: 'worker-output.txt', verified: true } };
    await send(kind, `Resume the completed scout with litter_message id ${scout.child.id} resume true and text ${JSON.stringify('Report the input.txt token from your retained conversation. Do not read files or delegate again.')}. Then wait for the same ID with litter_wait timeoutMs 120000. Use only its latest status and outcome. ${onlyControls}`);
    receipt.resume = await progress('resume-progress', evidence => resumedRecall(evidence, scout, receipt.doctor.model, inputToken));
    receipt.resume = { childId: receipt.resume.child.id, resumeAcknowledged: true, outcome: { state: receipt.resume.outcome.state, model: receipt.resume.outcome.model, truncated: receipt.resume.outcome.truncated, reportProvenance: receipt.resume.outcome.reportProvenance }, outcomeAfterAcknowledgement: true };
    ensure(receipt.liveWidgetObserved, 'No live child widget was observed.');
    receipt.fileEffects = { scoutInputRemovedBeforeResume: true, workerCwd, workerOutputVerified: true };
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
   report.cleanup.evidenceFailures.push(...await saveFailureArtifacts({ name: active.name, sessionDir: active.sessionDir, evidencePath, capture: () => screen(active.name, 'failure') }));
   await cleanup().catch(error => { report.cleanup.failures.push(error.message); });
   active = undefined;
   const failed = report.cases.at(-1);
   if (!interrupted && !report.cleanup.remaining.length && !report.cleanup.failures.length && !report.cleanup.evidenceFailures.length) {
    try { report.doctorAfterFailure = await doctor(failed.name, failed.workspace, join(scratch, 'failure-doctor')); } catch (error) { report.doctorAfterFailure = { status: 'failed', reason: error.message }; }
   }
  }
 } finally {
  await cleanup().catch(error => { report.cleanup.failures.push(error.message); });
  await finalizeArtifacts(report, { scratch, evidencePath });
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
