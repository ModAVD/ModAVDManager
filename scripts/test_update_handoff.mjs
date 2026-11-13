// Copyright 2026 jinghero
// SPDX-License-Identifier: AGPL-3.0-or-later
// Run: node --experimental-vm-modules --test scripts/test_update_handoff.mjs
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/ui/web/js/update.js', import.meta.url), 'utf8');
const settle = () => new Promise(resolve => setImmediate(resolve));

async function page(post) {
  const notices = [];
  const noop = () => {};
  const stubs = {
    './dom.js': Object.fromEntries(['h', 'icon', 'btn', 'attr', 'setText', 'showError'].map(k => [k, noop])),
    './store.js': { S: {}, update: noop, subscribe: noop },
    './api.js': { api: { post } },
    './ui.js': { openDialog: noop, toast: value => notices.push(value), toastError: noop },
    './diagnostics.js': { openDiagnostics: noop, activeJob: noop, progressText: noop },
    './actions.js': { onJob: noop, notifyJob: noop },
  };
  const context = vm.createContext({});
  const module = new vm.SourceTextModule(source, { context });
  await module.link(specifier => {
    const values = stubs[specifier];
    return new vm.SyntheticModule(Object.keys(values), function() {
      for (const [key, value] of Object.entries(values)) this.setExport(key, value);
    }, { context });
  });
  await module.evaluate();
  return { update: value => module.namespace.onUpdate({ handoff: value }), notices };
}

test('two windows claim once, then repeated events and reloads stay silent', async () => {
  const result = { id: 'a'.repeat(32), at: 'same-second', ok: true, updated: true, version: 'v0.40.0' };
  let pending = result;
  let calls = 0;
  const post = async (path, body) => {
    assert.equal(path, '/api/update/handoff/take');
    assert.equal(body.id, result.id);
    calls++;
    const claimed = pending;
    pending = null;
    return claimed;
  };
  const first = await page(post);
  const second = await page(post);
  first.update(result);
  first.update(result);
  second.update(result);
  await settle();
  assert.equal(first.notices.length + second.notices.length, 1);
  assert.equal(first.notices[0].title, '已更新到 0.40.0');
  assert.equal(calls, 2);
  first.update(result);
  const reloaded = await page(post);
  reloaded.update(result);
  await settle();
  assert.equal(reloaded.notices.length, 0);
});

test('different IDs in the same second show their own success and error', async () => {
  const results = [
    { id: 'a'.repeat(32), at: 'same-second', ok: true, message: 'already current' },
    { id: 'b'.repeat(32), at: 'same-second', ok: false, error: { message: 'busy', advice: 'close manager' } },
  ];
  const p = await page(async (_path, body) => results.find(x => x.id === body.id));
  for (const result of results) p.update(result);
  await settle();
  assert.equal(p.notices.length, 2);
  assert.equal(p.notices[0].title, 'already current');
  assert.equal(p.notices[1].text, 'busy');
  assert.equal(p.notices[1].advice, 'close manager');
});

test('a failed request retries on the next update without showing unclaimed data', async () => {
  const result = { id: 'a'.repeat(32), ok: true, message: 'done' };
  let calls = 0;
  const p = await page(async () => {
    if (++calls === 1) throw new Error('disconnected');
    return result;
  });
  p.update({ ok: true }); // legacy payload has no owner
  p.update(result);
  await settle();
  assert.equal(p.notices.length, 0);
  p.update(result);
  await settle();
  assert.equal(calls, 2);
  assert.equal(p.notices.length, 1);
});
