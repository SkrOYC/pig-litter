import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtemp, rm, symlink, writeFile } from 'node:fs/promises';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { loadConfig, parseConfig } from './config.mjs';

test('strict project limits parse with defaults and inline comments', () => {
  const config = parseConfig('concurrency: 3 # active descendants\ndepth: 0\nmax_records: 5\nmax_mailbox: 8\n');
  assert.equal(config.concurrency, 3);
  assert.equal(config.depth, 0);
  assert.equal(config.max_records, 5);
  assert.equal(config.max_mailbox, 8);
  assert.deepEqual(config.agents.scout.tools, ['read']);
});

test('duplicates, unknown keys, nonfinite values, and cross-field violations fail', () => {
  for (const source of [
    'concurrency: 2\nconcurrency: 3', 'worktrees: 1', 'depth: NaN', 'depth: .inf',
    'depth: 1.5', 'concurrency: 9007199254740993', 'concurrency: 5\nmax_records: 4',
    'max_history_bytes: 1024\nmax_result_bytes: 2048', 'concurrency: 0', '---',
    'depth: 2\nextra: 1', 'agents: {scout: {role: scout, role: worker}}',
    'agents: {scout: {tools: [write]}}', 'agents: {custom: {role: worker, can_delegate: true}}',
    'agents: &a {scout: {role: scout}}\ncopy: *a',
  ]) {
    assert.throws(() => parseConfig(source), Error, source);
  }
  assert.throws(() => parseConfig('x'.repeat(32769)), /exceeds 32768 bytes/);
});

test('named agents retain only their role ceiling', () => {
  const config = parseConfig('agents:\n  reviewer:\n    role: scout\n    tools: [read]\n    instructions: Review the given file.\n    can_delegate: false\n    model: fixture/reviewer\n');
  assert.deepEqual(config.agents.reviewer, { role: 'scout', tools: ['read'], instructions: 'Review the given file.', canDelegate: false, model: 'fixture/reviewer' });
});

test('config file reads stop at the byte bound and reject nonregular input', async () => {
  const directory = await mkdtemp(join(tmpdir(), 'litter-config-'));
  try {
    await writeFile(join(directory, 'litter.yaml'), 'x'.repeat(32769));
    await assert.rejects(loadConfig(directory), /exceeds 32768 bytes/);
    if (process.platform !== 'win32') {
      await rm(join(directory, 'litter.yaml'));
      await symlink('/dev/null', join(directory, 'litter.yaml'));
      await assert.rejects(loadConfig(directory), /regular file/);
    }
  } finally { await rm(directory, { recursive: true, force: true }); }
});
