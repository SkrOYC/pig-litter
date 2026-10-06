import { join } from 'node:path';
import { createAgentSession, DefaultResourceLoader, getAgentDir, ModelRuntime, SessionManager, SettingsManager } from '@earendil-works/pi-coding-agent';
import { stripTerminalSequences, truncateToWidth } from '@earendil-works/pi-tui';
import { loadConfig } from './config.mjs';
import { SessionTree, TreeError } from './session-tree.mjs';

const fileNames = ['read', 'ls', 'write', 'edit'];
const controlNames = ['litter_spawn', 'litter_list', 'litter_inspect', 'litter_message', 'litter_stop', 'litter_wait'];
const object = (required, properties) => ({ type: 'object', additionalProperties: false, required, properties });
const text = (maximum = 16384) => ({ type: 'string', minLength: 1, maxLength: maximum });
const id = text(128);
const generation = { type: 'integer', minimum: 1 };
const schemas = {
  litter_spawn: object(['type', 'task'], { type: { ...text(64), description: 'Named agent from litter_list; starts a background child.' }, task: text(), name: text(128), model: text(256) }),
  litter_list: object([], { offset: { type: 'integer', minimum: 0 }, limit: { type: 'integer', minimum: 1, maximum: 50 }, agentOffset: { type: 'integer', minimum: 0 }, agentLimit: { type: 'integer', minimum: 1, maximum: 50 } }),
  litter_inspect: object(['id'], { id, transcript: { type: 'boolean' }, offset: { type: 'integer', minimum: 0 }, limit: { type: 'integer', minimum: 1, maximum: 50 } }),
  litter_message: object(['id', 'generation', 'text'], { id, generation, text: text(), mode: { type: 'string', enum: ['steer', 'follow_up'] }, resume: { type: 'boolean' } }),
  litter_stop: object(['id', 'generation'], { id, generation }),
  litter_wait: object(['id', 'generation'], { id, generation, timeoutMs: { type: 'integer', minimum: 1, maximum: 300000 } }),
};

function preview(text, limit) {
  if (Buffer.byteLength(text) <= limit) return { text, truncated: false };
  const characters = [];
  let bytes = 0;
  for (const character of text) {
    const size = Buffer.byteLength(character);
    if (bytes + size > limit) break;
    characters.push(character);
    bytes += size;
  }
  return { text: characters.join(''), truncated: true };
}

function result(operation, value, error = false) {
  const outcome = value.outcome;
  let handbackTruncated = false;
  if (outcome?.text) {
    const report = preview(outcome.text, 8192);
    value = { ...value, outcome: { ...outcome, text: report.text } };
    handbackTruncated ||= report.truncated;
  }
  if (typeof value.report === 'string') {
    const report = preview(value.report, 1024);
    value = { ...value, report: report.text };
    handbackTruncated ||= report.truncated;
  }
  return { content: [{ type: 'text', text: JSON.stringify({ operation, ...value, handbackTruncated }) }], isError: error };
}

function errorResult(operation, error) {
  const code = error instanceof TreeError ? error.code : 'failed';
  const state = code === 'wait_timeout' ? 'timed_out' : code === 'cancelled' ? 'cancelled' : code === 'unavailable' ? 'unavailable' : code === 'permission_denied' || code === 'rejected' ? 'rejected' : 'failed';
  return result(operation, { state, code, report: error instanceof Error ? error.message : String(error) }, code !== 'wait_timeout' && code !== 'cancelled');
}

function qualified(model) {
  if (typeof model !== 'string') return '';
  const slash = model.indexOf('/');
  return slash > 0 && slash < model.length - 1 && !/\s/.test(model) ? model : '';
}

function safeLabel(value) {
  if (typeof value !== 'string' || Buffer.byteLength(value) > 128) throw new TreeError('rejected', 'child name must be at most 128 bytes');
  const label = stripTerminalSequences(value).replace(/[\u0000-\u001f\u007f-\u009f]/g, ' ').replace(/\s+/g, ' ').trim();
  if (!label) throw new TreeError('rejected', 'child name must contain visible text');
  return label;
}

function assistantText(message) {
  return (message?.content ?? []).filter(block => block.type === 'text').map(block => block.text).join('');
}

function outcomeFrom(session, firstMessage, firstStats, failure, stopped, limitHit) {
  const recent = session.messages.slice(firstMessage);
  const assistant = recent.filter(message => message.role === 'assistant').at(-1);
  const stats = session.getSessionStats();
  let state = stopped ? 'stopped' : assistant?.stopReason === 'stop' ? 'completed' : assistant?.stopReason === 'length' ? 'partial' : 'failed';
  if (failure && state === 'completed') state = 'partial';
  if (limitHit && state !== 'stopped') state = assistant ? 'partial' : 'failed';
  const delta = key => Math.max(0, stats.tokens[key] - firstStats.tokens[key]);
  return {
    state, text: assistantText(assistant), error: failure || assistant?.errorMessage,
    usage: { input: delta('input'), output: delta('output'), cacheRead: delta('cacheRead'), cacheWrite: delta('cacheWrite'), totalTokens: delta('total'), cost: Math.max(0, stats.cost - firstStats.cost) },
  };
}

export default function pigLitter(pi) {
  let ownerEpoch = 0;
  let owner;
  let initialization = Promise.resolve();
  let retirement = Promise.resolve();
  let pendingInit;
  const rootAcks = new Map();

  function current(root) {
    if (owner !== root || root.epoch !== ownerEpoch || root.tree.closed) throw new TreeError('unavailable', 'the root Session retired');
    if (root.context.sessionManager.getSessionId() !== root.sessionId) throw new TreeError('unavailable', 'the root Session changed');
  }

  function updateWidget(root) {
    try {
      if (root.tree.closed || !root.context.hasUI) return;
      const children = root.tree.list();
      const live = children.filter(child => ['starting', 'running', 'stopping'].includes(child.state)).length;
      const lines = [truncateToWidth(`Litter ${live} live ${children.length - live} kept`, 29, '')];
      for (const child of children.slice(-2)) lines.push(truncateToWidth(`${child.name} ${child.state}`, 29, ''));
      root.context.ui.setWidget('pig-litter', lines);
    } catch (error) {
      root.widgetError = error instanceof Error ? error.message : String(error);
    }
  }

  function canonical(root, name, context = root.context) {
    current(root);
    const callable = context.tools.find(tool => tool.name === name);
    const registered = pi.getAllTools().find(tool => tool.name === name);
    const source = registered?.sourceInfo;
    if (!callable || source?.source !== 'builtin' || source.path !== `builtin:${name}`) throw new TreeError('permission_denied', `canonical ${name} is unavailable`);
    const original = root.canonical.get(name);
    if (!original || JSON.stringify(callable.parameters) !== original.schema || JSON.stringify(source) !== original.source) {
      throw new TreeError('permission_denied', `canonical ${name} changed`);
    }
    return callable;
  }

  function modelFor(root, name) {
    current(root);
    const exact = qualified(name);
    if (!exact) throw new TreeError('unavailable', 'an exact provider/model is required');
    const [provider, ...parts] = exact.split('/');
    const modelId = parts.join('/');
    if (root.context.modelRegistry.getRegisteredProviderIds().includes(provider)) {
      throw new TreeError('unavailable', 'an extension-registered parent provider cannot be reproduced in the independent child');
    }
    const available = root.context.modelRegistry.getAvailable().some(model => model.provider === provider && model.id === modelId);
    const scoped = root.context.scopedModels ?? [];
    const permitted = scoped.length === 0 || scoped.some(item => (item.model ?? item).provider === provider && (item.model ?? item).id === modelId);
    const model = root.modelRuntime.getModel(provider, modelId);
    const independentAvailable = root.modelRuntime.getAvailableSnapshot().some(candidate => candidate.provider === provider && candidate.id === modelId);
    if (!available || !permitted || !model || !root.modelRuntime.hasConfiguredAuth(provider) || !independentAvailable) throw new TreeError('unavailable', 'exact model is unavailable to the root and stock SDK');
    return model;
  }

  async function refreshModels(root) {
    const refreshed = (root.modelRefresh ?? Promise.resolve()).then(() => root.modelRuntime.refresh({ allowNetwork: false }));
    root.modelRefresh = refreshed.catch(() => {});
    await refreshed;
    current(root);
  }

  async function ensureRoot(context) {
    const sessionId = context.sessionManager.getSessionId();
    if (owner?.sessionId === sessionId && !owner.tree.closed) { current(owner); return owner; }
    if (pendingInit?.sessionId === sessionId && pendingInit.epoch === ownerEpoch) return pendingInit.promise;
    const expectedEpoch = ownerEpoch;
    const promise = initialization.then(async () => {
      await retirement;
      if (owner?.sessionId === sessionId && !owner.tree.closed) { current(owner); return owner; }
      if (owner) { const previous = owner; owner = undefined; await previous.tree.close(); }
      if (ownerEpoch !== expectedEpoch) throw new TreeError('unavailable', 'the root Session changed during initialization');
      const trusted = await context.isProjectTrusted();
      if (trusted !== true) throw new TreeError('permission_denied', 'Pig Litter requires a trusted project');
      const cwd = context.cwd;
      if (!cwd) throw new TreeError('unavailable', 'the project directory is unavailable');
      const config = await loadConfig(cwd);
      const agentDir = getAgentDir();
      const modelRuntime = await ModelRuntime.create({ authPath: join(agentDir, 'auth.json'), modelsPath: join(agentDir, 'models.json'), allowModelNetwork: false });
      const canonicalTools = new Map();
      for (const name of fileNames) {
        const registered = pi.getAllTools().find(tool => tool.name === name);
        const source = registered?.sourceInfo;
        if (source?.source === 'builtin' && source.path === `builtin:${name}`) canonicalTools.set(name, { schema: JSON.stringify(registered.parameters), source: JSON.stringify(source) });
      }
      const root = { epoch: ownerEpoch, context, sessionId, cwd, agentDir, config, modelRuntime, canonical: canonicalTools, modelRefresh: Promise.resolve(), tree: undefined };
      root.tree = new SessionTree(config, { ownerSessionId: sessionId, onStateChange: () => updateWidget(root), onCompletion: async (outcome, parentId) => {
        current(root);
        const content = JSON.stringify({ id: outcome.run.id, generation: outcome.run.generation, state: outcome.state, model: outcome.model, report: outcome.text, error: outcome.error, truncated: outcome.truncated });
        const details = { childId: outcome.run.id, generation: outcome.run.generation, state: outcome.state };
        const message = { customType: 'litter_completion', content, display: false, details };
        if (parentId) {
          const parent = root.tree.records.get(parentId);
          if (!parent) throw new TreeError('unavailable', 'the rightful parent history is unavailable');
          if (parent.run.session && ['starting', 'running', 'stopping'].includes(parent.run.state)) {
            await parent.run.session.sendCustomMessage(message, { triggerTurn: false });
          } else {
            if (!parent.history) throw new TreeError('unavailable', 'parent retained history is unavailable');
            parent.history.appendCustomMessageEntry(message.customType, content, false, details);
            if (Buffer.byteLength(JSON.stringify(parent.history.getEntries())) > root.config.max_history_bytes) {
              root.tree.retireHistory(parent);
              throw new TreeError('unavailable', 'parent retained history cannot fit the child completion');
            }
          }
          return;
        }
        pi.sendMessage(message, root.context.isIdle() ? { triggerTurn: false } : { deliverAs: 'followUp' });
        updateWidget(root);
      } });
      if (ownerEpoch !== expectedEpoch || context.sessionManager.getSessionId() !== sessionId) {
        await root.tree.close();
        throw new TreeError('unavailable', 'the root Session changed during initialization');
      }
      owner = root;
      return root;
    });
    initialization = promise.catch(() => {});
    pendingInit = { sessionId, epoch: expectedEpoch, promise };
    try { return await promise; }
    finally { if (pendingInit?.promise === promise) pendingInit = undefined; }
  }

  function fileTool(root, record, name) {
    const authority = record.authority;
    if (!authority || authority.sessionId !== root.sessionId || authority.epoch !== root.epoch) throw new TreeError('unavailable', 'child authority retired');
    const metadata = canonical(root, name, authority.context);
    return { name, label: name, description: `Use the parent host's ${name} tool`, parameters: structuredClone(metadata.parameters), async execute(_id, args, signal) {
      if (authority.sessionId !== root.sessionId || authority.epoch !== ownerEpoch || authority.context.sessionManager.getSessionId() !== root.sessionId) throw new TreeError('unavailable', 'child authority retired');
      if (await authority.context.isProjectTrusted() !== true) throw new TreeError('permission_denied', 'project trust was revoked');
      canonical(root, name, authority.context);
      if (!record.tools.includes(name) || signal?.aborted) throw new TreeError('permission_denied', `${name} is unavailable to this child`);
      const outcome = await authority.executeTool(name, args, { signal });
      return { ...outcome.result, isError: outcome.isError || outcome.result?.isError === true };
    } };
  }

  function controls(root, record, acks) {
    return controlNames.map(name => ({ name, label: name, description: `${name} within this child tree`, parameters: schemas[name], async execute(callId, args, signal) {
      try {
        current(root);
        if (!record.canDelegate && name === 'litter_spawn') throw new TreeError('permission_denied', 'this agent cannot delegate');
        const value = await operation(root, name, args, signal, record.id, callId, acks);
        return result(name, value);
      } catch (error) { return errorResult(name, error); }
    } }));
  }

  async function childSession(root, record, signal, run) {
    current(root);
    const manager = run.history;
    const retained = () => {
      if (!manager || record.history !== manager || record.historyLost) throw new TreeError('unavailable', 'retained child history was retired before construction');
    };
    retained();
    await refreshModels(root);
    retained();
    const model = modelFor(root, record.model);
    const trusted = await root.context.isProjectTrusted();
    current(root);
    retained();
    if (trusted !== true) throw new TreeError('permission_denied', 'project trust was revoked');
    const settingsManager = SettingsManager.inMemory({}, { projectTrusted: trusted });
    const loader = new DefaultResourceLoader({ cwd: root.cwd, agentDir: root.agentDir, settingsManager, noExtensions: true, noSkills: true, noPromptTemplates: true, noThemes: true, noContextFiles: true, systemPromptOverride: () => record.instructions, appendSystemPrompt: [] });
    let session;
    let unsubscribe;
    const onAbort = () => { void session?.abort(); };
    signal.addEventListener('abort', onAbort, { once: true });
    try {
      if (signal.aborted) throw new TreeError('cancelled', 'child stopped during construction');
      await loader.reload();
      current(root);
      retained();
      if (signal.aborted) throw new TreeError('cancelled', 'child stopped during construction');
      const acks = new Map();
      const customTools = [...record.tools.map(name => fileTool(root, record, name)), ...(record.canDelegate ? controls(root, record, acks) : [])];
      retained();
      ({ session } = await createAgentSession({ cwd: root.cwd, agentDir: root.agentDir, modelRuntime: root.modelRuntime, model, sessionManager: manager, settingsManager, resourceLoader: loader, noTools: 'builtin', tools: customTools.map(tool => tool.name), customTools }));
      current(root);
      retained();
      if (signal.aborted) throw new TreeError('cancelled', 'child stopped during construction');
      await session.bindExtensions({ mode: 'print' });
      current(root);
      retained();
      if (signal.aborted) throw new TreeError('cancelled', 'child stopped during construction');
      let failure = '';
      let limitHit = false;
      let turns = 0;
      unsubscribe = session.subscribe(event => {
        if (event.type === 'turn_start' && ++turns > root.config.max_turns) { failure = 'child turn limit reached'; limitHit = true; void session.abort(); }
        if (event.type === 'tool_execution_end' && event.isError) failure ||= `tool ${event.toolName} failed`;
        if (event.type === 'message_end') {
          if (record.history !== manager || record.historyLost) { failure = 'retained child history was retired'; limitHit = true; void session.abort(); }
          else if (Buffer.byteLength(JSON.stringify(manager.getEntries())) > root.config.max_history_bytes) { failure = 'child history limit reached'; limitHit = true; void session.abort(); }
          if (event.message?.role === 'toolResult') settleAck(acks, event.message.toolCallId, event.message.isError);
        }
      });
      const firstMessage = session.messages.length;
      const firstStats = session.getSessionStats();
      const completion = (async () => {
        try { await session.prompt(record.resumeText ?? `Task:\n${record.task}`); }
        catch (error) { failure ||= error instanceof Error ? error.message : String(error); }
        if (record.history !== manager || record.historyLost) { failure ||= 'retained child history was retired'; limitHit = true; }
        else if (Buffer.byteLength(JSON.stringify(manager.getEntries())) > root.config.max_history_bytes) { failure ||= 'child history limit reached'; limitHit = true; }
        return outcomeFrom(session, firstMessage, firstStats, failure, signal.aborted, limitHit);
      })();
      return { session, completion, dispose: () => { signal.removeEventListener('abort', onAbort); for (const pending of acks.values()) pending.cancel(); unsubscribe?.(); session.dispose(); } };
    } catch (error) {
      signal.removeEventListener('abort', onAbort);
      unsubscribe?.();
      if (session) { try { await session.abort(); } finally { session.dispose(); } }
      throw error;
    }
  }

  function settleAck(acks, callId, isError) {
    const pending = acks.get(callId);
    if (!pending) return;
    acks.delete(callId);
    pending.signal?.removeEventListener('abort', pending.cancel);
    if (isError || pending.signal?.aborted) pending.tree.cancelPending(pending.ref);
    else {
      try { void pending.tree.activate(pending.ref, (record, signal, run) => childSession(pending.root, record, signal, run)); }
      catch (error) {
        try { pending.tree.cancelPending(pending.ref); }
        catch { pending.root.activationError = error instanceof Error ? error.message : String(error); }
      }
    }
  }

  function deferActivation(root, acks, callId, ref, signal) {
    const pending = { root, tree: root.tree, ref, signal, cancel: undefined };
    pending.cancel = () => {
      if (!acks.delete(callId)) return;
      signal?.removeEventListener('abort', pending.cancel);
      try { root.tree.cancelPending(ref); }
      catch (error) { root.activationError = error instanceof Error ? error.message : String(error); }
    };
    signal?.addEventListener('abort', pending.cancel, { once: true });
    acks.set(callId, pending);
    if (signal?.aborted) pending.cancel();
  }

  async function operation(root, name, args, signal, parentId, callId, acks, toolContext) {
    current(root);
    const tree = root.tree;
    if (name === 'litter_spawn') {
      if (await (toolContext ?? root.context).isProjectTrusted() !== true) throw new TreeError('permission_denied', 'project trust was revoked');
      current(root);
      const agent = root.config.agents[args.type];
      if (!agent || typeof args.task !== 'string' || !args.task.trim() || Buffer.byteLength(args.task) > 16384) throw new TreeError('rejected', 'choose a named agent and a bounded task');
      if (Buffer.byteLength(args.task) + Buffer.byteLength(agent.instructions) + 512 > root.config.max_history_bytes) throw new TreeError('rejected', 'task would exceed the retained history limit');
      const parent = parentId ? tree.record(parentId) : undefined;
      const authority = parent?.authority ?? { context: toolContext, executeTool: toolContext?.executeTool, sessionId: root.sessionId, epoch: root.epoch };
      if (!authority.executeTool) throw new TreeError('unavailable', 'root tool execution bridge is unavailable');
      if (agent.model && args.model && args.model !== agent.model) throw new TreeError('permission_denied', 'the named agent has a fixed model');
      const model = agent.model || args.model || parent?.model || `${root.context.model?.provider}/${root.context.model?.id}`;
      await refreshModels(root);
      modelFor(root, model);
      const ceiling = parent?.tools ?? fileNames;
      const tools = [...agent.tools];
      if (tools.some(tool => !ceiling.includes(tool))) throw new TreeError('permission_denied', 'the named agent exceeds the caller tool ceiling');
      for (const tool of tools) canonical(root, tool);
      const snapshot = tree.admit({ parentId, agentName: args.type, agent, name: args.name === undefined ? undefined : safeLabel(args.name), model, tools, cwd: root.cwd, history: SessionManager.inMemory(root.cwd), task: args.task, authority });
      deferActivation(root, acks, callId, { id: snapshot.id, generation: snapshot.generation }, signal);
      return { state: snapshot.state, child: snapshot };
    }
    if (name === 'litter_list') {
      const allChildren = tree.list(parentId);
      const offset = args.offset ?? 0;
      const limit = args.limit ?? 20;
      const allAgents = Object.entries(root.config.agents).sort(([left], [right]) => left.localeCompare(right));
      const agentOffset = args.agentOffset ?? 0;
      const agentLimit = args.agentLimit ?? 20;
      for (const [value, maximum] of [[offset, 1000000], [agentOffset, 1000000], [limit, 50], [agentLimit, 50]]) {
        if (!Number.isSafeInteger(value) || value < (maximum === 50 ? 1 : 0) || value > maximum) throw new TreeError('rejected', 'invalid list page');
      }
      const availableAgentTypes = allAgents.slice(agentOffset, agentOffset + agentLimit).map(([name, agent]) => ({ name, role: agent.role, description: Array.from(agent.instructions).slice(0, 160).join(''), tools: agent.tools, model: agent.model || undefined, canDelegate: agent.canDelegate }));
      return { state: 'listed', children: allChildren.slice(offset, offset + limit), nextOffset: offset + Math.min(limit, Math.max(0, allChildren.length - offset)), more: allChildren.length > offset + limit, totalChildren: allChildren.length, availableAgentTypes, nextAgentOffset: agentOffset + availableAgentTypes.length, moreAgentTypes: allAgents.length > agentOffset + agentLimit, totalAgentTypes: allAgents.length };
    }
    if (name === 'litter_inspect') {
      tree.owned(parentId, args.id);
      const inspected = tree.inspect(args.id, { transcript: args.transcript === true, offset: args.offset ?? 0, limit: args.limit ?? 10 });
      return { state: inspected.child.state, ...inspected };
    }
    const ref = { id: args.id, generation: args.generation };
    tree.owned(parentId, ref.id);
    if (name === 'litter_wait') {
      const outcome = await tree.wait(ref, { timeoutMs: args.timeoutMs ?? Math.min(root.config.max_run_millis, 300000), signal });
      return { state: outcome.state, outcome };
    }
    if (name === 'litter_stop') {
      const stopped = await tree.stop(ref);
      return { state: stopped.outcome?.state ?? 'stopping', ...stopped };
    }
    if (name === 'litter_message') {
      const mode = args.resume === true ? 'resume' : args.mode ?? 'steer';
      if (mode === 'resume') {
        if (await (toolContext ?? root.context).isProjectTrusted() !== true) throw new TreeError('permission_denied', 'project trust was revoked');
        current(root);
        await refreshModels(root);
        modelFor(root, tree.current(ref).model);
      }
      const child = await tree.message(ref, { mode, text: args.text });
      if (mode === 'resume') deferActivation(root, acks, callId, { id: child.id, generation: child.generation }, signal);
      return { state: child.state, child, delivery: mode === 'resume' ? 'resumed' : 'queued' };
    }
    throw new TreeError('rejected', 'unknown child operation');
  }

  pi.on('message_end', event => {
    if (event.message?.role === 'toolResult') settleAck(rootAcks, event.message.toolCallId, event.message.isError);
  });
  const retire = () => {
    ownerEpoch++;
    for (const pending of rootAcks.values()) pending.cancel();
    if (owner) {
      const previous = owner;
      owner = undefined;
      try { if (previous.context.hasUI) previous.context.ui.setWidget('pig-litter', undefined); }
      catch (error) { previous.widgetError = error instanceof Error ? error.message : String(error); }
      retirement = Promise.all([retirement, previous.tree.close()]).then(() => {});
    }
    return retirement;
  };
  pi.on('session_start', retire);
  pi.on('session_shutdown', retire);
  for (const name of controlNames) {
    const descriptions = {
      litter_spawn: 'Start a background child. Use litter_list to discover named agent types.',
      litter_list: 'List available agent types and a bounded page of owned children.',
      litter_inspect: 'Inspect one owned child with an optional bounded transcript page.',
      litter_message: 'Steer a live child or explicitly resume a settled generation.',
      litter_stop: 'Stop an owned child subtree and report newly stopped runs.',
      litter_wait: 'Wait for an exact child generation without stopping it on timeout.',
    };
    pi.registerTool({ name, label: name, description: descriptions[name], executionMode: 'parallel', parameters: schemas[name], async execute(callId, args, signal, _update, context) {
      try {
        const root = await ensureRoot(context);
        const value = await operation(root, name, args, signal, null, callId, rootAcks, context);
        return result(name, value);
      } catch (error) { return errorResult(name, error); }
    } });
  }
  pi.registerCommand('pig-litter', { description: 'Show Pig Litter child controls.', handler: async (_args, ctx) => {
    ctx.ui.notify('Use litter_list to discover available agent types and children. Use litter_spawn to start one in the background, then inspect, message, stop, or wait by ID.', 'info');
  } });
}
