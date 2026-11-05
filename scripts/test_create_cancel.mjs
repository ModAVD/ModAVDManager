// Run: node --experimental-vm-modules --test scripts/test_create_cancel.mjs
// Execute the actual card menu and actions, stubbing only their browser / HTTP dependencies.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import vm from 'node:vm';

async function harness() {
  const requests = [];
  const context = vm.createContext({ requestAnimationFrame() {} });
  const base = new URL('../internal/ui/web/js/', import.meta.url);
  const cache = new Map();
  const noop = () => {};
  const stubs = {
    'api.js': { api: { post: async (path, body) => {
      requests.push({ path, body });
      return { id: 'j2', kind: 'stop', state: 'running', index: 3 };
    } } },
    'dom.js': Object.fromEntries(['h', 'icon', 'btn', 'ibtn', 'keyed', 'setText', 'attr'].map(k => [k, noop])),
    'format.js': Object.fromEntries(['label', 'uptime', 'stripInst', 'pct', 'cfgText', 'quoted', 'dur', 'diskInfoText', 'diskTrimText', 'diskCompactText'].map(k => [k, noop])),
    'ui.js': { toast: noop, toastError: e => { throw e; }, copyText: noop, openMenu: noop },
    'forms.js': { openSettings: noop, openDelete: noop },
    'proxy.js': { openProxy: noop, markOf: noop },
    'viewer.js': { openViewer: noop, openLogs: noop },
    'panels.js': { openActivity: noop },
  };
  async function load(name) {
    if (cache.has(name)) return cache.get(name);
    const values = stubs[name];
    const mod = values
      ? new vm.SyntheticModule(Object.keys(values), function() {
        for (const [k, v] of Object.entries(values)) this.setExport(k, v);
      }, { context })
      : new vm.SourceTextModule(await readFile(new URL(name, base), 'utf8') +
        (name === 'cards.js' ? '\nexport { menuItems };' : ''), { context });
    cache.set(name, mod);
    await mod.link(specifier => load(specifier.replace(/^\.\//, '')));
    return mod;
  }
  const cards = await load('cards.js');
  await cards.evaluate();
  const state = cache.get('store.js').namespace.S;
  return { cards: cards.namespace, state, requests };
}

test('cancel creation started by another process requests explicit graceful interruption', async () => {
  const h = await harness();
  const menu = h.cards.menuItems({ i: 3, inst: { index: 3, status: 'creating', running: true } });
  await menu.find(x => x.label === '取消新建').run();
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].path, '/api/instances/3/stop');
  assert.equal(h.requests[0].body.cancel_create, true);
  assert.notEqual(h.requests[0].body.force, true);
});

test('cancel this window creation cancels its own job', async () => {
  const h = await harness();
  h.state.jobs.set('j1', { id: 'j1', kind: 'create', index: 3, state: 'running' });
  const menu = h.cards.menuItems({ i: 3, inst: { index: 3, status: 'creating', running: true } });
  await menu.find(x => x.label === '取消新建').run();
  assert.equal(h.requests.length, 1);
  assert.equal(h.requests[0].path, '/api/jobs/j1/cancel');
});

test('ordinary force stop does not opt into canceling creation', async () => {
  const h = await harness();
  const menu = h.cards.menuItems({ i: 3, inst: { index: 3, status: 'online', running: true } });
  await menu.find(x => x.label === '强制关机').run();
  assert.equal(h.requests[0].path, '/api/instances/3/stop');
  assert.equal(h.requests[0].body.force, true);
  assert.notEqual(h.requests[0].body.cancel_create, true);
});
