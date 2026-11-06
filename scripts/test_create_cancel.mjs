// Run: node --experimental-vm-modules --test scripts/test_create_cancel.mjs
// Execute the actual card menu, settings and actions, stubbing only their browser / HTTP dependencies.
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
    'panels.js': { openActivity: noop, runCheck: noop },
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

// Small browser boundary for the actual settings submit and card update paths. No emulator or network is used.
class Element {
  constructor(tag) {
    this.tagName = tag.toUpperCase();
    this.childNodes = [];
    this.attrs = new Map();
    this.events = new Map();
    this.dataset = {};
    this.style = {};
    this.checked = false;
    this.disabled = false;
    this.value = '';
    this.className = '';
    this.classList = {
      toggle: (name, on) => {
        const names = new Set(this.className.split(' ').filter(Boolean));
        if (on) names.add(name); else names.delete(name);
        this.className = [...names].join(' ');
      },
      add: name => this.classList.toggle(name, true),
      remove: name => this.classList.toggle(name, false),
    };
  }
  append(...kids) {
    for (const kid of kids.flat(Infinity)) if (kid != null && kid !== false) this.childNodes.push(kid);
  }
  replaceChildren(...kids) { this.childNodes = []; this.append(...kids); }
  get textContent() { return this.childNodes.map(x => x instanceof Element ? x.textContent : String(x)).join(''); }
  set textContent(text) { this.childNodes = [String(text)]; }
  setAttribute(key, value) {
    this.attrs.set(key, String(value));
    if (['id', 'name', 'type', 'class'].includes(key)) this[key === 'class' ? 'className' : key] = String(value);
  }
  getAttribute(key) { return this.attrs.get(key) ?? null; }
  hasAttribute(key) { return this.attrs.has(key); }
  removeAttribute(key) { this.attrs.delete(key); }
  addEventListener(type, fn) { this.events.set(type, [...(this.events.get(type) || []), fn]); }
  dispatch(type) { for (const fn of this.events.get(type) || []) fn(); }
  querySelector(selector) { return descendants(this).find(x => selector.startsWith('#') ? x.id === selector.slice(1) : x.tagName.toLowerCase() === selector) || null; }
  focus() {}
}

function descendants(root) {
  return root.childNodes.flatMap(x => x instanceof Element ? [x, ...descendants(x)] : []);
}

async function settingsHarness({ diagnostics = true, running = true, get, patch } = {}) {
  const requests = [], dialogs = [], toasts = [], errors = [];
  const noop = () => {};
  const h = (tag, props, ...kids) => {
    const el = new Element(tag);
    for (const [key, value] of Object.entries(props || {})) {
      if (key === 'checked') el.checked = !!value;
      else if (value == null || value === false) continue;
      else if (key === 'text') el.textContent = value;
      else if (key === 'value') el.value = value;
      else if (key.startsWith('on')) el.addEventListener(key.slice(2), value);
      else if (key === 'hidden') el.hidden = value;
      else el.setAttribute(key, value);
    }
    el.append(...kids);
    return el;
  };
  const openDialog = options => {
    const form = h('form', null, options.body, options.actions?.ok);
    const inputs = descendants(form).filter(x => x.tagName === 'INPUT');
    form.elements = {};
    for (const name of new Set(inputs.filter(x => x.name).map(x => x.name))) {
      Object.defineProperty(form.elements, name, { get: () => ({ value: inputs.find(x => x.name === name && x.checked)?.value ?? '' }) });
    }
    const ctl = { form, options, busy: false, onClose: [], closed: false,
      close() { if (!this.closed) { this.closed = true; for (const fn of this.onClose) fn(); } },
    };
    dialogs.push(ctl);
    return ctl;
  };
  const context = vm.createContext({ requestAnimationFrame: noop, document: { visibilityState: 'hidden' } });
  const base = new URL('../internal/ui/web/js/', import.meta.url);
  const cache = new Map();
  let state;
  const stubs = {
    'api.js': { api: {
      get: async path => {
        requests.push({ method: 'GET', path });
        return get ? get() : { index: 3, files: 3, bytes: 6 * 2 ** 20 };
      },
      patch: async (path, body) => {
        requests.push({ method: 'PATCH', path, body: JSON.parse(JSON.stringify(body)) });
        return patch ? patch(body) : {
          instance: { ...state.list.instances[0], diagnostics: body.diagnostics ?? diagnostics },
          applies_next_boot: running,
          diagnostics_cleanup: { files: 2, bytes: 4 * 2 ** 20 },
        };
      },
    } },
    'dom.js': { h, icon: () => h('svg'), btn: (cls, kids, fn, more) => h('button', { class: cls, onclick: fn, ...more }, kids),
      ibtn: (ic, text, fn) => h('button', { onclick: fn, 'aria-label': text }), keyed: noop,
      attr: (el, key, value) => value == null || value === false ? el.removeAttribute(key) : el.setAttribute(key, value),
      setText: (el, text) => { el.textContent = text; },
      showError: (el, error) => { el.textContent = error.message; el.hidden = false; errors.push(error); },
    },
    'ui.js': { openDialog, toast: value => toasts.push(value), openMenu: noop },
    'actions.js': { copy: noop, openFolder: noop, stop: noop, onJob: noop },
    'proxy.js': { openProxy: noop, markOf: () => null },
    'viewer.js': { openViewer: noop, openLogs: noop },
    'panels.js': { openActivity: noop },
  };
  async function load(name) {
    if (cache.has(name)) return cache.get(name);
    const values = stubs[name];
    const mod = values
      ? new vm.SyntheticModule(Object.keys(values), function() {
        for (const [key, value] of Object.entries(values)) this.setExport(key, value);
      }, { context })
      : new vm.SourceTextModule(await readFile(new URL(name, base), 'utf8') +
        (name === 'cards.js' ? '\nexport { updateCard };' : ''), { context });
    cache.set(name, mod);
    await mod.link(specifier => load(specifier.replace(/^\.\//, '')));
    return mod;
  }
  const cards = await load('cards.js');
  await cards.evaluate();
  state = cache.get('store.js').namespace.S;
  const inst = { index: 3, name: 'mas_3', title: 'mas_3', status: running ? 'online' : 'offline', running, initialized: true, diagnostics,
    config: { memory_mb: 6144, cpu: 6, fps: 60, resolution: 1080, data_partition_gb: 64 }, ports: {}, screencap: {} };
  state.list = { instances: [inst] };
  cache.get('forms.js').namespace.openSettings(3);
  const settings = dialogs[0];
  return { requests, dialogs, toasts, errors, settings, state, inst,
    control: id => settings.form.querySelector('#' + id),
    toggle(on) { this.control('s-diagnostics').checked = on; settings.form.dispatch('change'); },
    radio(name, value) {
      for (const el of descendants(settings.form).filter(x => x.name === name)) el.checked = String(el.value) === String(value);
      settings.form.dispatch('change');
    },
    submit: () => settings.options.onSubmit(),
    updateCard(inst) {
      const c = { i: 3, url: null, visible: false, next: 0, fail: 0 };
      for (const key of ['el', 'title', 'sub', 'cfg', 'pillText', 'pill', 'adb', 'copy', 'more', 'view', 'peek', 'pmark', 'notes', 'screen', 'primary', 'phText']) c[key] = h('div');
      cards.namespace.updateCard(c, inst);
      return c;
    },
  };
}

const turn = () => new Promise(resolve => setImmediate(resolve));

test('settings has one diagnostic checkbox with limits, privacy and immediate application', async () => {
  const h = await settingsHarness();
  assert.equal(descendants(h.settings.form).filter(x => x.type === 'checkbox').length, 1);
  assert.equal(h.control('s-diagnostics').checked, true);
  const text = h.settings.options.body.textContent;
  for (const expected of ['历史日志上限 4 GiB', '客体内存最多增加 48 MiB', '账号信息', '仅保存在本机', '诊断设置立即生效']) assert.ok(text.includes(expected), text);
});

test('enabling diagnostics patches a boolean immediately without a deletion preview', async () => {
  const h = await settingsHarness({ diagnostics: false });
  h.toggle(true);
  await h.submit();
  assert.deepEqual(h.requests, [{ method: 'PATCH', path: '/api/instances/3', body: { diagnostics: true } }]);
  assert.equal(h.state.list.instances[0].diagnostics, true);
  assert.match(h.toasts[0].text, /已开启，立即生效/);
  assert.doesNotMatch(h.toasts[0].title + h.toasts[0].text, /下次开机/);
});

test('diagnostic save displays the external capture warning from its result', async () => {
  const h = await settingsHarness({ diagnostics: false, patch: async () => ({
    instance: { index: 3, diagnostics: true }, warnings: ['已有其它程序采集 logcat，诊断模式未接管宿主采集'],
  }) });
  h.toggle(true);
  await h.submit();
  assert.match(h.toasts[0].text, /未接管宿主采集/);
});

test('disabling waits for preview and confirmation, then reports actual cleanup', async () => {
  const h = await settingsHarness();
  h.toggle(false);
  const pending = h.submit();
  await turn();
  assert.deepEqual(h.requests, [{ method: 'GET', path: '/api/instances/3/diagnostics/preview' }]);
  assert.equal(h.control('s-diagnostics').disabled, true);
  const confirm = h.dialogs[1];
  assert.match(confirm.options.body.textContent, /3 个日志文件（6 MB）/);
  assert.equal(confirm.options.body.querySelector('#' + confirm.options.desc).textContent, confirm.options.body.textContent);
  await h.submit(); // a second Save must not start another preview or patch
  assert.equal(h.requests.length, 1);
  confirm.options.onSubmit();
  confirm.options.onSubmit(); // native dialog close has an animation; a second click must be harmless
  await pending;
  assert.deepEqual(h.requests[1], { method: 'PATCH', path: '/api/instances/3', body: { diagnostics: false } });
  assert.match(h.toasts[0].text, /已清理 2 个文件（4 MB）/);
  assert.doesNotMatch(h.toasts[0].text, /3 个|6 MB|下次开机/);
});

test('canceling diagnostic cleanup makes no patch and a retry gets a fresh preview', async () => {
  let previews = 0;
  const h = await settingsHarness({ get: () => ({ index: 3, files: ++previews, bytes: 0 }) });
  h.toggle(false);
  h.radio('memory', 4096);
  const first = h.submit();
  await turn();
  h.dialogs[1].close();
  await first;
  assert.equal(h.requests.filter(x => x.method === 'PATCH').length, 0);
  assert.equal(h.settings.closed, false);
  assert.equal(h.settings.busy, false);
  const retry = h.submit();
  await turn();
  assert.match(h.dialogs[2].options.body.textContent, /2 个日志文件/);
  h.dialogs[2].close();
  await retry;
  assert.equal(h.requests.filter(x => x.method === 'PATCH').length, 0);
  assert.equal(h.toasts.length, 0);
});

test('a failed or malformed preview never permits a diagnostic patch', async () => {
  for (const get of [() => { throw new Error('preview failed'); }, () => null, () => ({ index: 4, files: 1, bytes: 0 }),
    () => ({ index: 3, files: -1, bytes: 0 }), () => ({ index: 3, files: 1, bytes: -1 })]) {
    const h = await settingsHarness({ get });
    h.toggle(false);
    await h.submit();
    assert.equal(h.requests.length, 1);
    assert.equal(h.requests[0].method, 'GET');
    assert.equal(h.dialogs.length, 1);
    assert.equal(h.settings.closed, false);
    assert.equal(h.control('s-diagnostics').disabled, false);
    assert.equal(h.errors.length, 1);
  }
});

test('diagnostics and ordinary settings share one patch with separate application timing', async () => {
  const h = await settingsHarness();
  h.toggle(false);
  h.radio('memory', 4096);
  const pending = h.submit();
  await turn();
  h.dialogs[1].options.onSubmit();
  await pending;
  assert.deepEqual(h.requests[1].body, { diagnostics: false, memory_mb: 4096 });
  assert.match(h.toasts[0].text, /诊断模式已关闭.*其他设置在下次开机生效/);
});

test('ordinary settings omit diagnostics and keep the existing next-boot response', async () => {
  const h = await settingsHarness();
  h.radio('memory', 4096);
  await h.submit();
  assert.deepEqual(h.requests, [{ method: 'PATCH', path: '/api/instances/3', body: { memory_mb: 4096 } }]);
  assert.equal(h.toasts[0].title, '已保存，下次开机生效');
});

test('closing settings while preview is pending cannot open confirmation or patch later', async () => {
  let finish;
  const h = await settingsHarness({ get: () => new Promise(resolve => { finish = resolve; }) });
  h.toggle(false);
  const pending = h.submit();
  await turn();
  h.settings.close();
  finish({ index: 3, files: 1, bytes: 0 });
  await pending;
  assert.equal(h.requests.length, 1);
  assert.equal(h.dialogs.length, 1);
  assert.equal(h.toasts.length, 0);
});

test('closing settings also cancels an open diagnostic confirmation without patching', async () => {
  const h = await settingsHarness();
  h.toggle(false);
  const pending = h.submit();
  await turn();
  h.settings.close();
  await pending;
  assert.equal(h.dialogs[1].closed, true);
  assert.equal(h.requests.length, 1);
  assert.equal(h.toasts.length, 0);
});

test('a failed diagnostic patch preserves settings and retries from a fresh preview', async () => {
  const h = await settingsHarness({ patch: () => { throw new Error('patch failed'); } });
  h.toggle(false);
  const pending = h.submit();
  await turn();
  h.dialogs[1].options.onSubmit();
  await pending;
  assert.equal(h.settings.closed, false);
  assert.equal(h.state.list.instances[0].diagnostics, true);
  assert.equal(h.errors[0].message, 'patch failed');
  assert.equal(h.toasts.length, 0);
  const retry = h.submit();
  await turn();
  assert.deepEqual(h.requests.map(x => x.method), ['GET', 'PATCH', 'GET']);
  h.dialogs[2].close();
  await retry;
  assert.equal(h.requests.filter(x => x.method === 'PATCH').length, 1);
});

test('cards add only a short diagnostics mark and retain their normal subtitle', async () => {
  const h = await settingsHarness();
  for (const status of ['online', 'offline']) {
    const inst = { ...h.inst, status, running: status === 'online' };
    assert.equal(h.updateCard(inst).sub.textContent, '编号 3 · 诊断模式');
    assert.equal(h.updateCard({ ...inst, diagnostics: false }).sub.textContent, '编号 3');
    assert.equal(h.updateCard({ ...inst, diagnostics: undefined }).sub.textContent, '编号 3');
  }
});
