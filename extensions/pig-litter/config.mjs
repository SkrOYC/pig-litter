import { open } from 'node:fs/promises';
import { constants } from 'node:fs';
import { join } from 'node:path';
import YAML from 'yaml';

const maximumBytes = 32 * 1024;
const limits = Object.freeze({
  concurrency: [1, 16, 4],
  depth: [0, 8, 2],
  max_records: [1, 4096, 64],
  max_history_bytes: [1024, 64 * 1024 * 1024, 1024 * 1024],
  max_result_bytes: [1, 1024 * 1024, 8192],
  max_mailbox: [1, 1024, 8],
  max_run_millis: [1000, 60 * 60 * 1000, 120000],
  max_turns: [1, 128, 20],
});
const roleTools = Object.freeze({ scout: ['read', 'ls'], worker: ['read', 'ls', 'write', 'edit'] });
const bundledAgents = Object.freeze({
  scout: { role: 'scout', instructions: 'Inspect files for the task. Do not change files. Report relevant paths and uncertainty.', tools: ['read'], can_delegate: true, model: '' },
  worker: { role: 'worker', instructions: 'Complete the task with permitted file tools. Preserve unrelated changes. Report changes and checks.', tools: ['read', 'write', 'edit'], can_delegate: true, model: '' },
});

function mapping(value, label) {
  if (value === null || typeof value !== 'object' || Array.isArray(value)) throw new Error(`${label} must be a mapping`);
  return value;
}

function agentDefinition(name, raw) {
  if (!/^[a-z][a-z0-9_-]{0,63}$/.test(name)) throw new Error(`invalid agent name ${name}`);
  const source = mapping(raw, `agents.${name}`);
  const fallback = bundledAgents[name] ?? {};
  for (const key of Object.keys(source)) {
    if (!['role', 'instructions', 'tools', 'can_delegate', 'model'].includes(key)) throw new Error(`agents.${name} has unknown setting ${key}`);
  }
  const field = key => Object.hasOwn(source, key) ? source[key] : fallback[key];
  const role = field('role');
  if (!Object.hasOwn(roleTools, role) || (fallback.role && role !== fallback.role)) throw new Error(`agents.${name} has invalid role`);
  const instructions = field('instructions');
  if (typeof instructions !== 'string' || !instructions.trim() || Buffer.byteLength(instructions) > 4096) throw new Error(`agents.${name} needs 1 to 4096 bytes of instructions`);
  const tools = field('tools');
  if (!Array.isArray(tools) || tools.some(tool => typeof tool !== 'string' || !roleTools[role].includes(tool)) || new Set(tools).size !== tools.length) {
    throw new Error(`agents.${name} tools must be distinct ${role} file tools`);
  }
  const canDelegate = Object.hasOwn(source, 'can_delegate') ? source.can_delegate : fallback.can_delegate;
  if (typeof canDelegate !== 'boolean') throw new Error(`agents.${name} can_delegate must be a boolean`);
  const model = Object.hasOwn(source, 'model') ? source.model : fallback.model ?? '';
  if (typeof model !== 'string' || (model && !/^[^/\s]+\/[^\s]+$/.test(model))) throw new Error(`agents.${name} model must be an exact provider/model`);
  return Object.freeze({ role, instructions, tools: Object.freeze([...tools]), canDelegate, model });
}

export function parseConfig(source) {
  if (Buffer.byteLength(source) > maximumBytes) throw new Error('litter.yaml exceeds 32768 bytes');
  const document = YAML.parseDocument(source, { uniqueKeys: true, strict: true, merge: false });
  if (document.errors.length) throw new Error(`litter.yaml: ${document.errors[0].message}`);
  if (source.trim() && document.contents == null) throw new Error('litter.yaml must contain one mapping');
  YAML.visit(document, { Alias() { throw new Error('litter.yaml does not permit aliases'); } });
  const content = document.toJS();
  if (source.trim() && content == null) throw new Error('litter.yaml must contain one mapping');
  const raw = mapping(content ?? {}, 'litter.yaml');
  for (const key of Object.keys(raw)) {
    if (key !== 'agents' && !Object.hasOwn(limits, key)) throw new Error(`litter.yaml has unknown setting ${key}`);
  }
  const config = Object.fromEntries(Object.entries(limits).map(([key, [, , fallback]]) => [key, fallback]));
  for (const [key, [minimum, maximum]] of Object.entries(limits)) {
    if (!Object.hasOwn(raw, key)) continue;
    const value = raw[key];
    if (!Number.isSafeInteger(value) || value < minimum || value > maximum) {
      throw new Error(`litter.yaml ${key} must be an integer from ${minimum} to ${maximum}`);
    }
    config[key] = value;
  }
  if (config.max_records < config.concurrency) throw new Error('max_records must be at least concurrency');
  if (config.max_history_bytes < config.max_result_bytes) throw new Error('max_history_bytes must be at least max_result_bytes');
  const rawAgents = raw.agents === undefined ? {} : mapping(raw.agents, 'agents');
  const agents = {};
  for (const name of new Set([...Object.keys(bundledAgents), ...Object.keys(rawAgents)])) {
    agents[name] = agentDefinition(name, Object.hasOwn(rawAgents, name) ? rawAgents[name] : {});
  }
  return Object.freeze({ ...config, agents: Object.freeze(agents) });
}

export async function loadConfig(cwd) {
  let file;
  try { file = await open(join(cwd, 'litter.yaml'), constants.O_RDONLY | constants.O_NONBLOCK); }
  catch (error) {
    if (error?.code === 'ENOENT') return parseConfig('');
    throw error;
  }
  try {
    const status = await file.stat();
    if (!status.isFile()) throw new Error('litter.yaml must be a regular file');
    if (status.size > maximumBytes) throw new Error('litter.yaml exceeds 32768 bytes');
    const buffer = Buffer.alloc(maximumBytes + 1);
    let total = 0;
    while (total < buffer.length) {
      const { bytesRead } = await file.read(buffer, total, buffer.length - total, total);
      if (bytesRead === 0) break;
      total += bytesRead;
    }
    if (total > maximumBytes) throw new Error('litter.yaml exceeds 32768 bytes');
    return parseConfig(buffer.toString('utf8', 0, total));
  } finally { await file.close(); }
}
