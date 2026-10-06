import assert from 'node:assert/strict';
import { test } from 'node:test';
import { parseConfig } from './config.mjs';
import { SessionTree, TreeError } from './session-tree.mjs';

const scout = { role: 'scout', instructions: 'Read.', canDelegate: true };
const worker = { role: 'worker', instructions: 'Write.', canDelegate: true };
const history = () => ({ getEntries: () => [{ type: 'message', id: 'one' }] });
const request = (agent = scout, extra = {}) => ({ agent, model: 'fixture/child', tools: agent.role === 'scout' ? ['read'] : ['read', 'write'], cwd: '/fixture', history: history(), ...extra });

test('admission counts all descendants, rejects depth and conflicting writers without a queue', () => {
  const tree = new SessionTree(parseConfig('concurrency: 2\ndepth: 2\n'));
  const first = tree.admit(request());
  const second = tree.admit(request(scout, { parentId: first.id }));
  assert.equal(second.depth, 2);
  assert.equal(tree.live, 2);
  assert.throws(() => tree.admit(request()), error => error instanceof TreeError && error.code === 'rejected');
  const zero = new SessionTree(parseConfig('depth: 0\n'));
  assert.throws(() => zero.admit(request()), /depth limit/);
  const writers = new SessionTree(parseConfig(''));
  writers.admit(request(worker));
  assert.throws(() => writers.admit(request(worker)), /writer conflict/);
});

test('wait timeout leaves a live child; stop joins once and retains an empty parent mailbox', async () => {
  const delivered = [];
  const tree = new SessionTree(parseConfig(''), { onCompletion: outcome => delivered.push(outcome) });
  const child = tree.admit(request());
  await assert.rejects(tree.wait(child, { timeoutMs: 1 }), error => error.code === 'wait_timeout');
  assert.equal(tree.list()[0].state, 'starting');
  const stopped = await tree.stop(child);
  assert.equal(stopped.stoppedCount, 1);
  assert.equal(stopped.outcome.state, 'stopped');
  assert.deepEqual(tree.inspect(child.id).mailbox, []);
  assert.equal((await tree.stop(child)).stoppedCount, 0);
  await Promise.resolve();
  assert.equal(delivered.length, 1);
});

test('resume retains ID and history, increments generation, and rejects stale refs', async () => {
  const tree = new SessionTree(parseConfig(''));
  const child = tree.admit(request());
  await tree.stop(child);
  const resumed = await tree.message(child, { mode: 'resume', text: 'continue' });
  assert.equal(resumed.id, child.id);
  assert.equal(resumed.generation, child.generation + 1);
  assert.throws(() => tree.current(child), error => error.code === 'rejected');
  assert.equal(tree.inspect(child.id, { transcript: true }).entries[0].id, 'one');
  await tree.close();
  assert.equal(tree.live, 0);
});

test('running child messages stay ordered and settlement releases capacity after disposal', async () => {
  const tree = new SessionTree(parseConfig('concurrency: 1\n'));
  const child = tree.admit(request());
  const calls = [];
  let settle;
  const completion = new Promise(resolve => { settle = resolve; });
  let releaseDispose;
  const disposal = new Promise(resolve => { releaseDispose = resolve; });
  const running = tree.activate(child, async () => ({
    session: { steer: async text => calls.push(`steer:${text}`), followUp: async text => calls.push(`follow:${text}`), abort: async () => {} },
    completion,
    dispose: async () => { calls.push('dispose'); await disposal; },
  }));
  await tree.message(child, { mode: 'steer', text: 'first' });
  await tree.message(child, { mode: 'follow_up', text: 'second' });
  settle({ state: 'completed', text: 'done', usage: { input: 3, output: 2, totalTokens: 5 } });
  await Promise.resolve();
  assert.equal(tree.live, 1);
  releaseDispose();
  const outcome = await running;
  assert.deepEqual(calls, ['steer:first', 'follow:second', 'dispose']);
  assert.equal(outcome.usage.totalTokens, 5);
  assert.equal(tree.live, 0);
});

test('an unacknowledged root spawn retains a stopped ID without launching', () => {
  const tree = new SessionTree(parseConfig('concurrency: 1\n'));
  const pending = tree.admit(request(scout, { name: 'retry' }));
  assert.equal(tree.cancelPending(pending), true);
  assert.equal(tree.live, 0);
  assert.equal(tree.list()[0].state, 'stopped');
  assert.equal(tree.inspect(pending.id).outcome.state, 'stopped');
  assert.throws(() => tree.activate(pending, async () => { throw new Error('must not run'); }), /activation is no longer pending/);
  assert.throws(() => tree.admit(request(scout, { name: 'retry' })), /name already belongs/);
  assert.equal(tree.admit(request(scout, { name: 'next' })).name, 'next');
});

test('new root trees never reuse an ID and report truncation preserves Unicode', () => {
  const config = parseConfig('max_result_bytes: 5\nmax_history_bytes: 1024\n');
  const first = new SessionTree(config);
  const second = new SessionTree(config);
  const a = first.admit(request());
  const b = second.admit(request());
  assert.notEqual(a.id, b.id);
  first.finish(first.record(a.id), first.record(a.id).run, { state: 'completed', text: 'ééé', usage: {} });
  assert.equal(first.record(a.id).run.outcome.text, 'éé');
  assert.equal(first.record(a.id).run.outcome.truncated, true);
});

test('subtree stop signals descendants before awaiting parent abort and concurrent stop joins', async () => {
  const tree = new SessionTree(parseConfig(''));
  const parent = tree.admit(request());
  const child = tree.admit(request(scout, { parentId: parent.id }));
  const order = [];
  let releaseParent;
  let releaseChild;
  const parentCompletion = new Promise(resolve => { releaseParent = resolve; });
  const childCompletion = new Promise(resolve => { releaseChild = resolve; });
  tree.activate(parent, async () => ({ session: { abort: async () => { order.push('parent-abort'); await new Promise(resolve => setTimeout(resolve, 1)); releaseParent({ state: 'stopped' }); }, dispose() {} }, completion: parentCompletion }));
  tree.activate(child, async () => ({ session: { abort: async () => { order.push('child-abort'); releaseChild({ state: 'stopped' }); }, dispose() {} }, completion: childCompletion }));
  await Promise.resolve();
  const stopping = tree.stop(parent);
  const repeated = tree.stop(parent);
  const [first, second] = await Promise.all([stopping, repeated]);
  assert.equal(first.stoppedCount, 2);
  assert.equal(second.stoppedCount, 0);
  assert.equal(second.outcome.state, 'stopped');
  assert.deepEqual(order, ['parent-abort', 'child-abort']);
});

test('mailbox admission reserves capacity and pending messages have a byte/count bound', async () => {
  const tree = new SessionTree(parseConfig('max_mailbox: 1\n'));
  const parent = tree.admit(request());
  const child = tree.admit(request(scout, { parentId: parent.id }));
  assert.throws(() => tree.admit(request(scout, { parentId: parent.id })), /mailbox capacity/);
  await tree.message(child, { mode: 'steer', text: 'first' });
  await assert.rejects(tree.message(child, { mode: 'steer', text: 'second' }), /message bound/);
  await assert.rejects(tree.message(parent, { mode: 'steer', text: 'x'.repeat(16385) }), /message bound/);
  await tree.close();
  assert.equal(tree.live, 0);
});

test('a child sees only descendants in its owned subtree', () => {
  const tree = new SessionTree(parseConfig(''));
  const parent = tree.admit(request());
  const sibling = tree.admit(request());
  const grandchild = tree.admit(request(scout, { parentId: parent.id }));
  assert.equal(tree.owned(parent.id, grandchild.id).id, grandchild.id);
  assert.deepEqual(tree.list(parent.id).map(child => child.id), [grandchild.id]);
  assert.throws(() => tree.owned(parent.id, parent.id), error => error.code === 'permission_denied');
  assert.throws(() => tree.owned(parent.id, sibling.id), error => error.code === 'permission_denied');
});

test('concurrent close callers wait for the same owned cleanup', async () => {
  const tree = new SessionTree(parseConfig(''));
  const child = tree.admit(request());
  let finish;
  const completion = new Promise(resolve => { finish = resolve; });
  let release;
  const disposal = new Promise(resolve => { release = resolve; });
  tree.activate(child, async () => ({ session: { abort: async () => finish({ state: 'stopped' }) }, completion, dispose: async () => disposal }));
  await Promise.resolve();
  const first = tree.close();
  const second = tree.close();
  let secondReturned = false;
  second.then(() => { secondReturned = true; });
  await Promise.resolve();
  assert.equal(secondReturned, false);
  release();
  await Promise.all([first, second]);
  assert.equal(secondReturned, true);
  assert.equal(tree.live, 0);
});

test('an explicit transcript page cannot flood the model context', () => {
  const tree = new SessionTree(parseConfig(''));
  const child = tree.admit(request(scout, { history: { getEntries: () => [{ type: 'message', text: 'x'.repeat(33000) }] } }));
  assert.throws(() => tree.inspect(child.id, { transcript: true, limit: 1 }), /32768 bytes/);
  assert.deepEqual(tree.inspect(child.id).entries, []);
});

test('inspection keeps mailbox and outcome previews separate from retained reports', () => {
  const tree = new SessionTree(parseConfig('max_mailbox: 12\n'));
  const parent = tree.admit(request());
  const record = tree.record(parent.id);
  const text = 'r'.repeat(5000);
  for (let index = 0; index < 9; index++) {
    const child = tree.admit(request(scout, { parentId: parent.id, name: `child-${index}` }));
    tree.finish(tree.record(child.id), tree.record(child.id).run, { state: 'completed', text });
  }
  tree.finish(record, record.run, { state: 'completed', text });
  const inspected = tree.inspect(parent.id);
  assert.equal(inspected.outcome.text.length, 2048);
  assert.equal(inspected.outcome.handbackTruncated, true);
  assert.equal(record.run.outcome.text.length, 5000);
  assert.equal(inspected.mailbox.length, 8);
  assert.equal(inspected.mailbox[0].text.length, 512);
  assert.equal(inspected.mailboxTotal, 9);
  assert.equal(inspected.mailboxMore, true);
});

test('a full pending parent mailbox rejects resume before another completion can overflow it', async () => {
  const tree = new SessionTree(parseConfig('max_mailbox: 2\n'));
  const parent = tree.admit(request());
  const first = tree.admit(request(scout, { parentId: parent.id, name: 'first' }));
  tree.finish(tree.record(first.id), tree.record(first.id).run, { state: 'completed', text: 'first' });
  tree.admit(request(scout, { parentId: parent.id, name: 'second' }));
  await assert.rejects(tree.message(first, { mode: 'resume', text: 'again' }), /mailbox capacity/);
});

test('oversized retained history is dropped after cleanup and cannot be resumed', async () => {
  let entries = [{ type: 'message', text: 'short' }];
  const tree = new SessionTree(parseConfig('max_history_bytes: 1024\nmax_result_bytes: 512\n'));
  const child = tree.admit(request(scout, { history: { getEntries: () => entries } }));
  const running = tree.activate(child, async () => ({
    session: { abort: async () => {}, dispose() {} },
    completion: Promise.resolve({ state: 'completed', text: 'response' }),
    dispose: () => { entries = [{ type: 'message', text: 'x'.repeat(2048) }]; },
  }));
  const outcome = await running;
  assert.equal(outcome.state, 'partial');
  assert.equal(tree.inspect(child.id).historyAvailable, false);
  assert.deepEqual(tree.inspect(child.id, { transcript: true }).entries, []);
  await assert.rejects(tree.message(child, { mode: 'resume', text: 'continue' }), /history is unavailable/);
});

test('SDK queued steering counts against the mailbox after enqueue resolves', async () => {
  const tree = new SessionTree(parseConfig('max_mailbox: 1\n'));
  const child = tree.admit(request());
  let queued = 0;
  let release;
  const completion = new Promise(resolve => { release = resolve; });
  const running = tree.activate(child, async () => ({
    session: { isStreaming: true, get pendingMessageCount() { return queued; }, steer: async () => { queued++; }, abort: async () => {} },
    completion,
    dispose() {},
  }));
  await Promise.resolve();
  await tree.message(child, { mode: 'steer', text: 'first' });
  await assert.rejects(tree.message(child, { mode: 'steer', text: 'second' }), /message bound/);
  release({ state: 'completed', text: 'done' });
  await running;
});

test('one rejected SDK enqueue does not poison the next live message', async () => {
  const tree = new SessionTree(parseConfig(''));
  const child = tree.admit(request());
  let attempts = 0;
  let release;
  const completion = new Promise(resolve => { release = resolve; });
  const running = tree.activate(child, async () => ({
    session: { isStreaming: true, get pendingMessageCount() { return 0; }, steer: async () => { if (++attempts === 1) throw new Error('first rejected'); }, abort: async () => {} },
    completion,
    dispose() {},
  }));
  await Promise.resolve();
  await assert.rejects(tree.message(child, { mode: 'steer', text: 'first' }), /first rejected/);
  await tree.message(child, { mode: 'steer', text: 'second' });
  assert.equal(attempts, 2);
  release({ state: 'completed', text: 'done' });
  await running;
});

test('stopping a queued message rejects only its caller and keeps the serialization tail handled', async () => {
  const tree = new SessionTree(parseConfig(''));
  const child = tree.admit(request());
  let releaseQueue;
  let finish;
  const queue = new Promise(resolve => { releaseQueue = resolve; });
  const completion = new Promise(resolve => { finish = resolve; });
  const running = tree.activate(child, async () => ({ session: { isStreaming: true, get pendingMessageCount() { return 0; }, steer: async () => {}, abort: async () => finish({ state: 'stopped' }) }, completion, dispose() {} }));
  await Promise.resolve();
  tree.record(child.id).run.operation = queue;
  const delivery = tree.message(child, { mode: 'steer', text: 'queued' });
  const stopped = tree.stop(child);
  releaseQueue();
  await assert.rejects(delivery, /stopped before message delivery/);
  assert.equal((await stopped).outcome.state, 'stopped');
  await running;
  await new Promise(resolve => setImmediate(resolve));
});

test('subtree stop reports the requested generation during a concurrent parent resume', async () => {
  const tree = new SessionTree(parseConfig(''));
  const parent = tree.admit(request());
  const descendant = tree.admit(request(scout, { parentId: parent.id }));
  tree.finish(tree.record(parent.id), tree.record(parent.id).run, { state: 'completed', text: 'first generation' });
  let finish;
  const completion = new Promise(resolve => { finish = resolve; });
  tree.activate(descendant, async () => ({ session: { abort: async () => finish({ state: 'stopped' }) }, completion, dispose() {} }));
  await Promise.resolve();
  const stopping = tree.stop(parent);
  const resumed = await tree.message(parent, { mode: 'resume', text: 'again' });
  const result = await stopping;
  assert.equal(result.outcome.run.generation, 1);
  assert.equal(result.outcome.text, 'first generation');
  assert.equal(resumed.generation, 2);
  await tree.close();
});

test('retiring history before resume activation revokes the pending generation', async () => {
  const tree = new SessionTree(parseConfig(''));
  const first = tree.admit(request());
  tree.finish(tree.record(first.id), tree.record(first.id).run, { state: 'completed', text: 'first' });
  const resumed = await tree.message(first, { mode: 'resume', text: 'second' });
  tree.retireHistory(tree.record(first.id));
  assert.equal(tree.record(first.id).history, undefined);
  assert.equal(tree.record(first.id).historyLost, true);
  assert.equal(tree.list()[0].generation, first.generation);
  assert.throws(() => tree.activate(resumed, async () => { throw new Error('must not create a default durable manager'); }), /stale child generation/);
  await assert.rejects(tree.message(first, { mode: 'resume', text: 'third' }), /history is unavailable/);
});
