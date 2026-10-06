export class TreeError extends Error {
  constructor(code, message) {
    super(message);
    this.code = code;
  }
}

const terminal = new Set(['completed', 'partial', 'failed', 'stopped', 'unavailable']);

function deferred() {
  let resolve;
  const promise = new Promise(done => { resolve = done; });
  return { promise, resolve };
}

function bounded(text, maximum) {
  const source = String(text ?? '');
  if (Buffer.byteLength(source) <= maximum) return { text: source, truncated: false };
  const characters = [];
  let bytes = 0;
  for (const character of source) {
    const size = Buffer.byteLength(character);
    if (bytes + size > maximum) break;
    characters.push(character);
    bytes += size;
  }
  return { text: characters.join(''), truncated: true };
}

function usageOf(value) {
  const source = value ?? {};
  const usage = {};
  for (const key of ['input', 'output', 'cacheRead', 'cacheWrite', 'totalTokens', 'cost']) {
    const number = source[key] ?? 0;
    usage[key] = Number.isFinite(number) && number >= 0 ? number : 0;
  }
  return usage;
}

function previewOutcome(outcome, maximum) {
  if (!outcome) return undefined;
  const text = bounded(outcome.text, maximum);
  const error = bounded(outcome.error, 256);
  return { ...outcome, text: text.text, error: error.text || undefined, handbackTruncated: text.truncated || error.truncated };
}

function newRun(generation) {
  const finished = deferred();
  return { generation, state: 'starting', controller: new AbortController(), finished, started: false, session: undefined, task: undefined, outcome: undefined, messages: [], pendingCount: 0, operation: Promise.resolve() };
}

export class SessionTree {
  constructor(config, { ownerSessionId, onCompletion, onStateChange } = {}) {
    this.config = config;
    this.ownerSessionId = ownerSessionId;
    this.onCompletion = onCompletion;
    this.onStateChange = onStateChange;
    this.records = new Map();
    this.names = new Map();
    this.sequence = 0;
    this.identity = randomUUID();
    this.live = 0;
    this.epoch = 1;
    this.closed = false;
    this.rootPending = new Set();
    this.deliveryErrors = [];
  }

  assertOpen() {
    if (this.closed) throw new TreeError('unavailable', 'the owning Session is closed');
  }

  record(id) {
    const record = this.records.get(id);
    if (!record) throw new TreeError('rejected', 'unknown child ID');
    return record;
  }

  current(ref) {
    const record = this.record(ref.id);
    if (record.run.generation !== ref.generation) throw new TreeError('rejected', 'stale child generation');
    return record;
  }

  owned(callerId, id) {
    const record = this.record(id);
    if (this.belongsTo(callerId, record)) return record;
    throw new TreeError('permission_denied', 'child is outside the caller subtree');
  }

  belongsTo(callerId, record) {
    if (!callerId) return true;
    for (let parent = record.parentId; parent; parent = this.records.get(parent)?.parentId) {
      if (parent === callerId) return true;
    }
    return false;
  }

  snapshot(record) {
    return { id: record.id, generation: record.run.generation, parent: record.parentId, depth: record.depth, name: record.name, type: record.type, model: record.model, state: record.run.state };
  }

  admit({ parentId, agentName, agent, name, model, tools, cwd, history, task, authority }) {
    this.assertOpen();
    const parent = parentId ? this.record(parentId) : undefined;
    if (parent && (!parent.canDelegate || !['starting', 'running'].includes(parent.run.state))) throw new TreeError('permission_denied', 'the parent cannot delegate');
    const depth = parent ? parent.depth + 1 : 1;
    if (depth > this.config.depth) throw new TreeError('rejected', 'child depth limit reached');
    if (this.live >= this.config.concurrency) throw new TreeError('rejected', 'live child capacity reached');
    if (this.records.size >= this.config.max_records) throw new TreeError('rejected', 'retained child limit reached');
    if (parent) {
      const pending = [...this.records.values()].filter(record => record.parentId === parent.id && !terminal.has(record.run.state)).length;
      if (parent.pendingCompletions.size + pending >= this.config.max_mailbox) throw new TreeError('rejected', 'parent mailbox capacity reached');
    } else if (this.rootPending.size >= this.config.max_mailbox) throw new TreeError('rejected', 'root completion capacity reached');
    const roleTools = agent.role === 'scout' ? ['read', 'ls'] : ['read', 'ls', 'write', 'edit'];
    if (tools.some(tool => !roleTools.includes(tool)) || parent && tools.some(tool => !parent.tools.includes(tool))) throw new TreeError('permission_denied', 'child tools exceed the caller ceiling');
    if (parent?.readOnly && agent.role !== 'scout') throw new TreeError('permission_denied', 'read-only parent cannot delegate a worker');
    const readOnly = agent.role === 'scout' || !tools.some(tool => tool === 'write' || tool === 'edit');
    if (!readOnly && [...this.records.values()].some(record => record.cwd === cwd && !record.readOnly && !terminal.has(record.run.state))) {
      throw new TreeError('rejected', 'writer conflict in the shared working directory');
    }
    const id = `${this.identity}:${++this.sequence}`;
    const label = name || id;
    const nameKey = `${parentId ?? 'main'}\0${label}`;
    if (this.names.has(nameKey)) throw new TreeError('rejected', 'child name already belongs to this owner');
    const record = { id, parentId: parentId ?? null, parentGeneration: parent?.run.generation, depth, name: label, type: agentName ?? agent.role, instructions: agent.instructions, task, authority, canDelegate: agent.canDelegate, readOnly, tools: [...tools], model, cwd, history, historyLost: false, pendingCompletions: new Set(), run: newRun(1) };
    record.run.history = history;
    this.records.set(id, record);
    this.names.set(nameKey, id);
    this.live++;
    if (!parent) this.rootPending.add(`${id}:1`);
    this.onStateChange?.();
    return this.snapshot(record);
  }

  activate(ref, create) {
    this.assertOpen();
    const record = this.current(ref);
    const run = record.run;
    if (run.started || run.state !== 'starting') throw new TreeError('rejected', 'child activation is no longer pending');
    run.started = true;
    record.previousRun = undefined;
    run.timer = setTimeout(() => {
      run.deadline = true;
      run.controller.abort();
      void run.session?.abort();
    }, this.config.max_run_millis);
    run.task = this.run(record, run, create);
    return run.task;
  }

  cancelPending(ref) {
    const record = this.current(ref);
    if (record.run.started || record.run.state !== 'starting') return false;
    record.run.controller.abort();
    record.run.outcome = Object.freeze({ run: ref, state: 'stopped', model: record.model, text: '', error: 'spawn admission ended before the tool result was acknowledged', truncated: false, usage: usageOf() });
    record.run.state = 'stopped';
    record.run.finished.resolve(record.run.outcome);
    this.live--;
    this.rootPending.delete(`${record.id}:${record.run.generation}`);
    this.onStateChange?.();
    if (record.previousRun) {
      record.run = record.previousRun;
      record.previousRun = undefined;
      record.resumeText = undefined;
      return true;
    }
    return true;
  }

  retireHistory(record, finishingRun) {
    record.historyLost = true;
    record.history = undefined;
    const run = record.run;
    if (run === finishingRun) return;
    if (run.state === 'starting' && !run.started) {
      this.cancelPending({ id: record.id, generation: run.generation });
    } else if (!terminal.has(run.state) && run.state !== 'stopping') {
      run.state = 'stopping';
      run.controller.abort();
      void run.session?.abort();
      this.onStateChange?.();
    }
  }

  async run(record, run, create) {
    let outcome;
    let dispose;
    try {
      if (run.controller.signal.aborted) throw new TreeError('cancelled', 'child stopped before activation');
      const owned = await create(record, run.controller.signal, run);
      run.session = owned.session;
      dispose = owned.dispose ?? (() => owned.session?.dispose());
      if (run.controller.signal.aborted) await owned.session?.abort();
      else {
        run.state = 'running';
        this.onStateChange?.();
        for (const message of run.messages.splice(0)) {
          if (run.controller.signal.aborted) break;
          await this.deliver(run, message);
        }
      }
      const result = await owned.completion;
      outcome = { state: result.state, text: result.text, error: result.error, usage: result.usage, model: record.model, run: { id: record.id, generation: run.generation } };
    } catch (error) {
      outcome = { state: run.controller.signal.aborted ? 'stopped' : 'failed', text: '', error: error instanceof Error ? error.message : String(error), model: record.model, run: { id: record.id, generation: run.generation } };
    } finally {
      try { await dispose?.(); }
      catch (error) {
        outcome = { ...outcome, state: 'failed', error: `child cleanup failed: ${error instanceof Error ? error.message : String(error)}` };
      }
      run.session = undefined;
      if (record.history && Buffer.byteLength(JSON.stringify(record.history.getEntries())) > this.config.max_history_bytes) {
        this.retireHistory(record, run);
        outcome = { ...outcome, state: 'partial', error: 'retained child history exceeded the configured limit; resume unavailable' };
      }
      this.finish(record, run, outcome);
    }
    return run.outcome;
  }

  finish(record, run, raw) {
    if (run.outcome) return run.outcome;
    const state = raw.state === 'completed' && raw.error ? 'partial' : terminal.has(raw.state) ? raw.state : 'failed';
    const report = bounded(raw.text, this.config.max_result_bytes);
    const error = bounded(raw.error, 1024);
    run.outcome = Object.freeze({ run: { id: record.id, generation: run.generation }, state: run.deadline ? (report.text ? 'partial' : 'failed') : run.controller.signal.aborted ? 'stopped' : state, model: record.model, text: report.text, truncated: report.truncated, error: run.deadline ? 'child run time bound reached' : error.text || undefined, usage: usageOf(raw.usage) });
    run.state = run.outcome.state;
    this.onStateChange?.();
    clearTimeout(run.timer);
    this.live--;
    if (record.parentId) {
      const parent = this.records.get(record.parentId);
      if (parent) parent.pendingCompletions.add(`${record.id}:${run.generation}`);
    }
    run.finished.resolve(run.outcome);
    if (!this.closed && this.onCompletion) {
      Promise.resolve().then(() => this.onCompletion(run.outcome, record.parentId)).then(() => {
        if (record.parentId) this.records.get(record.parentId)?.pendingCompletions.delete(`${record.id}:${run.generation}`);
        else this.rootPending.delete(`${record.id}:${run.generation}`);
      }).catch(error => { this.deliveryErrors.push({ run: run.outcome.run, error }); });
    }
    return run.outcome;
  }

  list(callerId) {
    this.assertOpen();
    return [...this.records.values()].filter(record => this.belongsTo(callerId, record)).map(record => this.snapshot(record));
  }

  inspect(id, { transcript = false, offset = 0, limit = 10 } = {}) {
    this.assertOpen();
    const record = this.record(id);
    if (!Number.isSafeInteger(offset) || offset < 0 || !Number.isSafeInteger(limit) || limit < 1 || limit > 50) throw new TreeError('rejected', 'invalid transcript page');
    const entries = transcript ? record.history?.getEntries()?.slice(offset, offset + limit) ?? [] : [];
    const raw = JSON.stringify(entries);
    if (Buffer.byteLength(raw) > Math.min(this.config.max_history_bytes, 32768)) throw new TreeError('rejected', 'transcript page exceeds 32768 bytes');
    const completedChildren = [...this.records.values()].filter(child => child.parentId === record.id && child.run.outcome).map(child => child.run.outcome);
    return { child: this.snapshot(record), outcome: previewOutcome(record.run.outcome, 2048), historyAvailable: !record.historyLost, mailbox: completedChildren.slice(-8).map(outcome => previewOutcome(outcome, 512)), mailboxTotal: completedChildren.length, mailboxMore: completedChildren.length > 8, entries, nextOffset: transcript ? offset + entries.length : 0, more: transcript && (record.history?.getEntries()?.length ?? 0) > offset + entries.length };
  }

  async wait(ref, { timeoutMs = 120000, signal } = {}) {
    this.assertOpen();
    const run = this.current(ref).run;
    if (!Number.isSafeInteger(timeoutMs) || timeoutMs < 1 || timeoutMs > 300000) throw new TreeError('rejected', 'invalid wait timeout');
    if (signal?.aborted) throw new TreeError('cancelled', 'wait cancelled');
    let timer;
    let onAbort;
    const timeout = new Promise((_, reject) => { timer = setTimeout(() => reject(new TreeError('wait_timeout', 'wait timed out')), timeoutMs); });
    const cancellation = signal ? new Promise((_, reject) => {
      onAbort = () => reject(new TreeError('cancelled', 'wait cancelled'));
      signal.addEventListener('abort', onAbort, { once: true });
    }) : new Promise(() => {});
    try { return await Promise.race([run.finished.promise, timeout, cancellation]); }
    finally { clearTimeout(timer); if (signal && onAbort) signal.removeEventListener('abort', onAbort); }
  }

  async deliver(run, message) {
    if (run.controller.signal.aborted) throw new TreeError('cancelled', 'child stopped before message delivery');
    const method = message.mode === 'steer' ? 'steer' : 'followUp';
    await run.session[method](message.text);
  }

  async message(ref, message) {
    this.assertOpen();
    const record = this.current(ref);
    if (message.mode === 'resume') return this.resume(record, message.text);
    if (!['steer', 'follow_up'].includes(message.mode) || !message.text?.trim()) throw new TreeError('rejected', 'invalid child message');
    const run = record.run;
    if (!['starting', 'running'].includes(run.state)) throw new TreeError('rejected', 'child is not live');
    const sdkQueued = run.session?.pendingMessageCount ?? 0;
    if (Buffer.byteLength(message.text) > 16 * 1024 || run.messages.length + run.pendingCount + sdkQueued >= this.config.max_mailbox) throw new TreeError('rejected', 'child message bound reached');
    if (run.session && run.session.isStreaming === false) throw new TreeError('rejected', 'child is settling and cannot accept a message');
    if (!run.session) run.messages.push(message);
    else {
      run.pendingCount++;
      const accepted = run.operation.then(() => this.deliver(run, message));
      run.operation = accepted.then(() => { run.pendingCount--; }, () => { run.pendingCount--; });
      await accepted;
    }
    return this.snapshot(record);
  }

  resume(record, text) {
    if (!text?.trim() || !terminal.has(record.run.state)) throw new TreeError('rejected', 'resume needs a settled child and text');
    if (record.historyLost || !record.history) throw new TreeError('unavailable', 'retained history is unavailable for resume');
    if (Buffer.byteLength(JSON.stringify(record.history.getEntries())) + Buffer.byteLength(text) + 512 > this.config.max_history_bytes) throw new TreeError('rejected', 'resume would exceed the retained history limit');
    if (this.live >= this.config.concurrency) throw new TreeError('rejected', 'live child capacity reached');
    if (record.parentId) {
      const parent = this.records.get(record.parentId);
      const pending = [...this.records.values()].filter(other => other.parentId === record.parentId && !terminal.has(other.run.state)).length;
      if (!parent || parent.pendingCompletions.size + pending >= this.config.max_mailbox) throw new TreeError('rejected', 'parent mailbox capacity reached');
    } else if (this.rootPending.size >= this.config.max_mailbox) throw new TreeError('rejected', 'root completion capacity reached');
    if (!record.readOnly && [...this.records.values()].some(other => other !== record && !other.readOnly && other.cwd === record.cwd && !terminal.has(other.run.state))) {
      throw new TreeError('rejected', 'writer conflict in the shared working directory');
    }
    record.previousRun = record.run;
    record.run = newRun(record.run.generation + 1);
    record.run.history = record.history;
    record.resumeText = text;
    this.live++;
    if (!record.parentId) this.rootPending.add(`${record.id}:${record.run.generation}`);
    this.onStateChange?.();
    return this.snapshot(record);
  }

  async stop(ref) {
    this.assertOpen();
    const target = this.current(ref);
    const targetRun = target.run;
    const descendants = [target, ...[...this.records.values()].filter(record => {
      for (let parent = record.parentId; parent; parent = this.records.get(parent)?.parentId) if (parent === target.id) return true;
      return false;
    })];
    const captured = descendants.map(record => ({ record, run: record.run }));
    const pending = captured.filter(({ run }) => !terminal.has(run.state));
    const live = pending.filter(({ run }) => run.state !== 'stopping');
    for (const { record, run } of live) {
      run.state = 'stopping';
      this.onStateChange?.();
      run.controller.abort();
      if (!run.started) this.finish(record, run, { state: 'stopped', text: '', model: record.model });
    }
    await Promise.allSettled(live.map(({ run }) => run.session?.abort()));
    await Promise.all(pending.map(({ run }) => run.finished.promise));
    return { outcome: targetRun.outcome, stoppedCount: live.length };
  }

  async close() {
    if (this.closePromise) return this.closePromise;
    this.closed = true;
    this.epoch++;
    this.closePromise = (async () => {
      const live = [...this.records.values()].filter(record => !terminal.has(record.run.state));
      for (const record of live) {
        record.run.state = 'stopping';
        this.onStateChange?.();
        record.run.controller.abort();
        if (!record.run.started) this.finish(record, record.run, { state: 'stopped', text: '' });
      }
      await Promise.allSettled(live.map(record => record.run.session?.abort()));
      await Promise.all(live.map(record => record.run.finished.promise));
    })();
    return this.closePromise;
  }
}
import { randomUUID } from 'node:crypto';
