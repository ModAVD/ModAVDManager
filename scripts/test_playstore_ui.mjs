// Run: node --experimental-vm-modules --test scripts/test_playstore_ui.mjs
// The Google Play Store in the manager's page (ModAVD/ModAVD#125): the instance menu's 补全 / 移除, the question before
// the job, the way to the root's files when they are missing, 补全组件's option and 「使用我自己的文件」, and a create
// without the store. Runs the page's own modules, stubbing only their browser / HTTP neighbours.
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import { test } from 'node:test';
import vm from 'node:vm';

const base = new URL('../internal/ui/web/js/', import.meta.url);
const noop = () => {};
const turn = () => new Promise(resolve => setImmediate(resolve));
// values made inside the page's realm compared by their content
const plain = x => JSON.parse(JSON.stringify(x));
const turns = async (n = 6) => { for (let k = 0; k < n; k++) await turn(); };

// A small DOM: what h(), the dialogs and the form code touch.
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
    this.hidden = false;
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
    for (const kid of kids.flat(Infinity)) if (kid != null && kid !== false && kid !== '') this.childNodes.push(kid);
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
  dispatch(type) { return Promise.all((this.events.get(type) || []).map(fn => fn())); }
  querySelector(selector) { return all(this).find(x => selector.startsWith('#') ? x.id === selector.slice(1) : x.tagName.toLowerCase() === selector) || null; }
  focus() {}
  click() { this.clicked = (this.clicked || 0) + 1; }
}
const all = root => root.childNodes.flatMap(x => x instanceof Element ? [x, ...all(x)] : []);
const byText = (root, text) => all(root).find(x => x.tagName === 'BUTTON' && x.textContent.includes(text));

function h(tag, props, ...kids) {
  const el = new Element(tag);
  for (const [key, value] of Object.entries(props || {})) {
    if (key === 'checked') el.checked = !!value;
    else if (value == null || value === false) continue;
    else if (key === 'text') el.textContent = value;
    else if (key === 'value') el.value = value;
    else if (key === 'hidden') el.hidden = value;
    else if (key.startsWith('on') && typeof value === 'function') el.addEventListener(key.slice(2), value);
    else el.setAttribute(key, value === true ? '' : value);
  }
  el.append(...kids);
  return el;
}
const dom = {
  h, icon: () => h('svg'), btn: (cls, kids, fn, more) => h('button', { class: cls, onclick: fn, ...more }, kids),
  ibtn: (ic, text, fn) => h('button', { onclick: fn, 'aria-label': text }), keyed: noop, add: noop, hydrateIcons: noop,
  attr: (el, key, value) => value == null || value === false ? el.removeAttribute(key) : el.setAttribute(key, value),
  setText: (el, text) => { el.textContent = text == null ? '' : text; },
  showError: (el, e) => { el.textContent = e.message + (e.advice ? ' / ' + e.advice : ''); el.hidden = false; },
};

// load(entry, stubs): the page module entry with every module named in stubs replaced (the others are the real ones).
async function load(entry, stubs, context = {}) {
  const ctx = vm.createContext({ requestAnimationFrame: noop, setTimeout, clearTimeout, FormData, File, URLSearchParams, console, ...context });
  const cache = new Map();
  const sources = new Map(); // name -> the file's text (a promise), read once
  async function get(name) {
    if (cache.has(name)) return cache.get(name);
    const values = typeof stubs[name] === 'function' ? stubs[name](cache) : stubs[name];
    const extra = name === 'cards.js' ? '\nexport { menuItems };' : '';
    let text = '';
    if (!values) {
      if (!sources.has(name)) sources.set(name, readFile(new URL(name, base), 'utf8'));
      text = await sources.get(name);
      // two modules importing this one while its file was read: one instance (the page's S is shared that way)
      if (cache.has(name)) return cache.get(name);
    }
    const mod = values
      ? new vm.SyntheticModule(Object.keys(values), function() {
        for (const [k, v] of Object.entries(values)) this.setExport(k, v);
      }, { context: ctx })
      : new vm.SourceTextModule(text + extra, { context: ctx });
    cache.set(name, mod);
    await mod.link(specifier => get(specifier.replace(/^\.\//, '')));
    return mod;
  }
  const mod = await get(entry);
  await mod.evaluate();
  const ns = name => cache.get(name)?.namespace;
  return { mod: mod.namespace, ns, S: ns('store.js')?.S };
}

// ---- the instance menu (cards.js)

async function menuFor(inst, jobs = []) {
  const calls = [];
  const m = await load('cards.js', {
    'api.js': { api: {} },
    'dom.js': dom,
    'format.js': Object.fromEntries(['label', 'uptime', 'stripInst', 'pct', 'cfgText', 'quoted', 'dur', 'diskInfoText', 'diskTrimText', 'diskCompactText', 'playStoreText', 'gles32Text'].map(k => [k, noop])),
    'ui.js': { toast: noop, toastError: noop, copyText: noop, openMenu: noop },
    'forms.js': { openSettings: noop, openDelete: noop },
    'proxy.js': { openProxy: noop, markOf: noop },
    'viewer.js': { openViewer: noop, openLogs: noop },
    'panels.js': { openActivity: noop, runCheck: noop },
    'playstore.js': { openPlayStore: (i, action) => calls.push([i, action]), openGSFID: i => calls.push([i, 'gsfid']) },
    'gles32.js': { openGLES32: noop, GLES32_NOTE: '' },
  });
  for (const j of jobs) m.S.jobs.set(j.id, j);
  const items = m.mod.menuItems({ i: 3, inst: { index: 3, status: 'offline', running: false, initialized: true, bridge: 'installed', ...inst } }).filter(Boolean);
  const named = label => items.find(x => x.label === label);
  return { items, calls, named, store: items.find(x => /Google Play 商店/.test(x.label || '')) };
}

test('an instance without the store offers 补全 Google Play 商店, which asks playstore.js to install', async () => {
  const { items, store, calls } = await menuFor({});
  assert.equal(items.filter(x => /Google Play 商店/.test(x.label || '')).length, 1);
  assert.equal(store.label, '补全 Google Play 商店…');
  assert.ok(!store.disabled);
  store.run();
  assert.deepEqual(calls, [[3, 'install']]);
});

test('an instance with the store offers 移除 Google Play 商店 instead', async () => {
  const { store, calls } = await menuFor({ play_store: true, status: 'online', running: true });
  assert.equal(store.label, '移除 Google Play 商店…');
  assert.ok(!store.disabled);
  store.run();
  assert.deepEqual(calls, [[3, 'uninstall']]);
});

test('without the execution bridge 补全 is off and says why; removing still works', async () => {
  const off = await menuFor({ bridge: 'off' });
  assert.ok(off.store.disabled);
  assert.equal(off.store.hint, '要执行桥');
  const had = await menuFor({ bridge: 'off', play_store: true });
  assert.ok(!had.store.disabled);
});

test('with the store 复制 GSF ID reads it from the running instance; off it waits for a boot', async () => {
  const on = await menuFor({ play_store: true, status: 'online', running: true });
  const gsf = on.named('复制 GSF ID');
  assert.ok(gsf && !gsf.disabled);
  assert.equal(on.items.indexOf(gsf), on.items.indexOf(on.named('移除 Google Play 商店…')) + 1);
  gsf.run();
  assert.deepEqual(on.calls, [[3, 'gsfid']]);
  const off = await menuFor({ play_store: true });
  assert.ok(off.named('复制 GSF ID').disabled);
  assert.equal(off.named('复制 GSF ID').hint, '开机后可用');
  const busy = await menuFor({ play_store: true, status: 'online', running: true }, [{ id: 'j9', kind: 'install', index: 3, state: 'running' }]);
  assert.ok(busy.named('复制 GSF ID').disabled);
  const none = await menuFor({ status: 'online', running: true });
  assert.equal(none.named('复制 GSF ID'), undefined);
});

test('after an install that failed half way (partial) both 补全 and 移除 are offered', async () => {
  const { items, named, calls } = await menuFor({ play_store_partial: true });
  assert.deepEqual(plain(items.filter(x => /Google Play 商店/.test(x.label || '')).map(x => x.label)), ['补全 Google Play 商店…', '移除 Google Play 商店…']);
  assert.ok(!named('补全 Google Play 商店…').disabled && !named('移除 Google Play 商店…').disabled);
  assert.equal(named('复制 GSF ID'), undefined);
  named('移除 Google Play 商店…').run();
  assert.deepEqual(calls, [[3, 'uninstall']]);
  const noBridge = await menuFor({ play_store_partial: true, bridge: 'off' });
  assert.ok(noBridge.named('补全 Google Play 商店…').disabled);
  assert.ok(!noBridge.named('移除 Google Play 商店…').disabled);
});

test('the store items wait while the instance is being created, set up or busy with a job', async () => {
  for (const [inst, jobs] of [[{ status: 'creating', running: true }, []], [{ status: 'create_failed' }, []],
    [{}, [{ id: 'j9', kind: 'disk-compact', index: 3, state: 'running' }]],
    [{ play_store: true, status: 'online', running: true }, [{ id: 'j8', kind: 'playstore-uninstall', index: 3, state: 'running' }]]]) {
    const { store } = await menuFor(inst, jobs);
    assert.ok(store.disabled, JSON.stringify(inst));
  }
});

// ---- the question and the way to the files (playstore.js)

async function playstore({ components, loaded, gsfid } = {}) {
  const r = { dialogs: [], toasts: [], errors: [], checks: [], posts: [], gets: [], copied: [], loads: 0 };
  let S;
  const m = await load('playstore.js', {
    'dom.js': dom,
    'api.js': { api: { get: async path => { r.gets.push(path); if (gsfid instanceof Error) throw gsfid; return gsfid; } } },
    'ui.js': {
      openDialog: o => {
        const ctl = { options: o, onClose: [], closed: false, close() { if (!this.closed) { this.closed = true; for (const fn of this.onClose) fn(); } } };
        r.dialogs.push(ctl);
        return ctl;
      },
      toast: t => r.toasts.push(t), toastError: e => r.errors.push(e),
      copyText: async t => { r.copied.push(t); return true; },
    },
    'diagnostics.js': { loadDiagnostics: async () => { r.loads++; if (loaded) S.components = loaded; } },
    'panels.js': { openCheck: o => r.checks.push(o) },
    'actions.js': { playStore: (i, action, onErr) => r.posts.push({ i, action, onErr }) },
  });
  S = m.S;
  S.list = { instances: [{ index: 3, name: 'mas_3', title: '明日方舟', status: 'online', running: true }] };
  S.components = components ?? null;
  return Object.assign(r, { open: m.mod.openPlayStore, gsf: m.mod.openGSFID, S });
}
const files = ready => ({ play_store: { ready, source: ready ? 'download' : '', files: [] } });

test('补全 without the root\'s five files goes to 电脑检查 and starts nothing', async () => {
  const p = await playstore({ components: files(false) });
  await p.open(3, 'install');
  assert.equal(p.dialogs.length, 0);
  assert.equal(p.posts.length, 0);
  assert.deepEqual(plain(p.checks), [{ playStore: true }]);
  assert.match(p.toasts[0].title, /先补全 Google Play 商店服务/);
});

test('补全 reads the files\' state first (the page may not have it, or an old one)', async () => {
  const p = await playstore({ components: null, loaded: files(true) });
  await p.open(3, 'install');
  assert.equal(p.loads, 1);
  assert.equal(p.dialogs.length, 1);
});

test('补全 asks first, says it boots cold, and only then starts the job', async () => {
  const p = await playstore({ components: files(true) });
  await p.open(3, 'install');
  const d = p.dialogs[0];
  assert.equal(d.options.title, '补全 Google Play 商店？');
  assert.match(d.options.body.textContent, /冷启动一次/);
  assert.match(d.options.body.textContent, /会先关机/); // it is running
  assert.equal(p.posts.length, 0);
  await p.open(3, 'install'); // a second click while it asks opens nothing more
  assert.equal(p.dialogs.length, 1);
  d.options.onSubmit();
  assert.equal(d.closed, true);
  assert.equal(p.posts.length, 1);
  assert.equal(p.posts[0].action, 'install');
});

test('the server saying the files are missing leads to them; other refusals are a toast', async () => {
  const p = await playstore({ components: files(true) });
  await p.open(3, 'install');
  p.dialogs[0].options.onSubmit();
  const onErr = p.posts[0].onErr;
  onErr({ code: 'not_ready', message: '没有五个文件', details: { reason: 'playstore_files_missing' } });
  assert.deepEqual(plain(p.checks), [{ playStore: true }]);
  assert.equal(p.errors.length, 0);
  const noBridge = { code: 'not_ready', message: '没有执行桥', details: { reason: 'no_bridge' } };
  onErr(noBridge);
  assert.deepEqual(p.errors, [noBridge]);
  assert.equal(p.checks.length, 1);
});

test('移除 asks with its own words and does not need the files', async () => {
  const p = await playstore({ components: files(false) });
  await p.open(3, 'uninstall');
  assert.equal(p.checks.length, 0);
  const d = p.dialogs[0];
  assert.equal(d.options.title, '移除 Google Play 商店？');
  assert.match(d.options.body.textContent, /换回系统原来的占位应用/);
  d.options.onSubmit();
  assert.equal(p.posts[0].action, 'uninstall');
});

test('复制 GSF ID copies the decimal ID and shows both spellings with where to register them', async () => {
  const p = await playstore({ gsfid: { index: 3, decimal: '3881793548573364239', hex: '35dee6aa721e500f' } });
  await p.gsf(3);
  assert.deepEqual(p.gets, ['/api/instances/3/playstore-gsfid']);
  assert.deepEqual(p.copied, ['3881793548573364239']);
  const d = p.dialogs[0];
  const text = d.options.body.textContent;
  for (const want of ['已把十进制的 GSF ID 复制到剪贴板', '3881793548573364239', '35dee6aa721e500f', 'https://www.google.com/android/uncertified/', '关机再开机']) {
    assert.ok(text.includes(want), want);
  }
  assert.match(d.options.sub, /「明日方舟」/);
  // each spelling (and the page's address) has its own copy button
  const copies = all(d.options.body).filter(x => /^复制/.test(x.getAttribute('aria-label') || ''));
  assert.equal(copies.length, 3);
  await copies[1].dispatch('click');
  assert.equal(p.copied.at(-1), '35dee6aa721e500f');
  await d.options.actions.ok.dispatch('click');
  assert.equal(d.closed, true);
});

test('复制 GSF ID without an ID yet says to wait online; other errors are the usual toast', async () => {
  const none = Object.assign(new Error('实例 3 还没有 GSF ID'), { code: 'not_ready', details: { reason: 'no_gsf_id' } });
  const p = await playstore({ gsfid: none });
  await p.gsf(3);
  assert.equal(p.dialogs.length, 0);
  assert.equal(p.copied.length, 0);
  assert.equal(p.toasts[0].tone, 'info');
  assert.match(p.toasts[0].title, /还没有 GSF ID/);
  assert.match(p.toasts[0].text, /等一两分钟/);
  assert.equal(p.errors.length, 0);
  const off = Object.assign(new Error('实例 3 没有开机'), { code: 'busy', details: {} });
  const q = await playstore({ gsfid: off });
  await q.gsf(3);
  assert.deepEqual(q.errors, [off]);
  assert.equal(q.dialogs.length, 0);
});

// ---- the jobs (actions.js)

async function actions(post) {
  const r = { toasts: [], requests: [] };
  const m = await load('actions.js', {
    'api.js': { api: {
      post: async (path, body) => { r.requests.push(['POST', path, body]); return post ? post(path) : { id: 'j5', kind: 'playstore-install', index: 3, state: 'running' }; },
      get: async path => { r.requests.push(['GET', path]); return { root: 'X', components: [], play_store: { ready: true } }; },
    } },
    'ui.js': { toast: t => r.toasts.push(t), toastError: e => r.toasts.push({ error: e }), copyText: noop },
    'panels.js': { openActivity: noop, runCheck: noop },
  });
  m.S.list = { instances: [{ index: 3, name: 'mas_3', title: 'mas_3' }] };
  return { ...r, A: m.mod, S: m.S };
}

test('补全 and 移除 post an empty body to their endpoints and watch the job', async () => {
  const a = await actions();
  await a.A.playStore(3, 'install', noop);
  await a.A.playStore(3, 'uninstall', noop);
  assert.deepEqual(plain(a.requests.map(x => x.slice(0, 3))), [['POST', '/api/instances/3/playstore-install', {}], ['POST', '/api/instances/3/playstore-uninstall', {}]]);
  assert.ok(a.S.watch.has('j5'));
});

test('a refusal before the job goes to the caller\'s handler, not the generic toast', async () => {
  const a = await actions(() => { throw Object.assign(new Error('没有五个文件'), { details: { reason: 'playstore_files_missing' } }); });
  const seen = [];
  await a.A.playStore(3, 'install', e => seen.push(e.details.reason));
  assert.deepEqual(seen, ['playstore_files_missing']);
  assert.equal(a.toasts.length, 0);
});

test('a refused import says what to give instead; the server\'s own advice wins', async () => {
  const a = await actions();
  assert.match(a.A.importAdvice({ code: 'components_rejected', details: { reason: 'mismatch' } }), /重新下载/);
  assert.match(a.A.importAdvice({ code: 'components_rejected', details: { reason: 'unknown_file' } }), /x86_64-34_r14\.zip/);
  assert.equal(a.A.importAdvice({ code: 'components_rejected', advice: '把五个文件一起给出', details: { reason: 'incomplete' } }), '把五个文件一起给出');
  a.S.watch.add('j7');
  a.A.onJob({ id: 'j7', kind: 'playstore-import', index: -1, state: 'running', created_at: '' });
  a.A.onJob({ id: 'j7', kind: 'playstore-import', index: -1, state: 'failed', created_at: '', error: { code: 'components_rejected', message: 'a.apk 的 SHA-256 对不上', advice: '', details: { reason: 'mismatch' } } });
  assert.equal(a.toasts[0].tone, 'err');
  assert.match(a.toasts[0].title, /导入 Google Play 商店的文件没有成功/);
  assert.match(a.toasts[0].advice, /重新下载/);
  await turns();
  assert.ok(a.requests.some(x => x[1] === '/api/components'), 'the store state is read again');
});

test('a finished install says the instance booted cold and what the store needs next', async () => {
  const a = await actions();
  a.S.watch.add('j6');
  a.A.onJob({ id: 'j6', kind: 'playstore-install', index: 3, state: 'running', created_at: '' });
  a.A.onJob({ id: 'j6', kind: 'playstore-install', index: 3, state: 'done', created_at: '',
    result: { index: 3, action: 'install', state: 'installed', installed: true, restarted: true, stopped: false, warnings: [] } });
  assert.match(a.toasts[0].title, /已补全 Google Play 商店/);
  assert.match(a.toasts[0].text, /冷启动过一次/);
  // it points at 复制 GSF ID and the page, not at the documentation's script (instances it installs have none)
  assert.match(a.toasts[0].text, /「复制 GSF ID」/);
  assert.match(a.toasts[0].text, /https:\/\/www\.google\.com\/android\/uncertified\//);
  assert.doesNotMatch(a.toasts[0].text, /文档/);
});

// ---- 补全组件's option and 使用我自己的文件 (diagnostics.js)

async function diagnostics({ store = false, get } = {}) {
  const r = { posts: [], puts: [], dialogs: [], toasts: [], jobs: [] };
  const m = await load('diagnostics.js', {
    'dom.js': dom,
    'api.js': { api: {
      get: async path => get?.(path) ?? (path.startsWith('/api/components/license') ? { license: 'TERMS', license_source: 'mock', license_sha256: 'ab'.repeat(32) }
        : path === '/api/components' ? { root: 'X', components: [], play_store: { ready: store, source: store ? 'import' : '', version: store ? '14' : '', files: [] } } : {}),
      post: async (path, body) => { r.posts.push({ path, body }); return { id: 'j' + r.posts.length, kind: path.endsWith('fetch') ? 'components' : 'playstore-import', index: -1, state: 'running' }; },
      put: async (path, body) => { r.puts.push({ path, body }); return {}; },
    } },
    'ui.js': {
      openDialog: o => {
        const ctl = { options: o, onClose: [], closed: false, close() { if (!this.closed) { this.closed = true; for (const fn of this.onClose) fn(); } } };
        r.dialogs.push(ctl);
        return ctl;
      },
      toast: t => r.toasts.push(t), toastError: noop, copyText: noop,
    },
    'panels.js': { openCheck: noop },
    'actions.js': { onJob: j => r.jobs.push(j), notifyJob: noop, cancelJob: noop, importAdvice: e => e.advice || '' },
  });
  return { ...r, D: m.mod, S: m.S };
}
const file = (name, size = 3) => new File(['x'.repeat(size)], name);

test('使用我自己的文件 sends each picked file as a "file" part and watches the import', async () => {
  const d = await diagnostics();
  const j = await d.D.importPlayStore([file('x86_64-34_r14.zip'), file('Phonesky.apk')]);
  assert.equal(d.posts.length, 1);
  assert.equal(d.posts[0].path, '/api/components/import-playstore');
  assert.ok(d.posts[0].body instanceof FormData);
  assert.deepEqual(d.posts[0].body.getAll('file').map(f => f.name), ['x86_64-34_r14.zip', 'Phonesky.apk']);
  assert.equal(j.kind, 'playstore-import');
  assert.ok(d.S.watch.has(j.id));
});

test('an import the server would refuse anyway is stopped before the upload', async () => {
  const d = await diagnostics();
  await assert.rejects(d.D.importPlayStore(Array.from({ length: 9 }, (_, k) => file(`f${k}.apk`))), /最多选 8 个/);
  await assert.rejects(d.D.importPlayStore([file('Phonesky.apk'), file('empty.apk', 0)]), /empty\.apk 是空文件/);
  d.S.jobs.set('j1', { id: 'j1', kind: 'components', index: -1, state: 'running', created_at: '' });
  await assert.rejects(d.D.importPlayStore([file('Phonesky.apk')]), /正在补全或导入/);
  assert.equal(d.posts.length, 0);
});

test('the option is off unless asked for, and moot once the files are there', async () => {
  const d = await diagnostics();
  assert.equal(d.D.storeBox('fetch', false).wanted(), false);
  const on = d.D.storeBox('fetch', true);
  assert.equal(on.wanted(), true);
  d.S.components = { play_store: { ready: true, source: 'download', version: '14' } };
  d.D.fillStoreBox(on);
  assert.equal(on.wanted(), false);
  assert.equal(on.choose.hidden, true);
  assert.match(on.state.textContent, /已就绪（从 Google 官方下载，r14）/);
});

async function agreeIn(d) {
  d.D.openComponentFetch({ playStore: true });
  await turns();
  const dlg = d.dialogs[0];
  await dlg.options.actions.ok.dispatch('click');
  await turns();
  return d.posts.find(x => x.path === '/api/components/fetch');
}

test('补全组件 with the option ticked and no files of the user\'s own fetches the store along', async () => {
  const d = await diagnostics();
  const fetch = await agreeIn(d);
  assert.equal(fetch.body.accept_license, true);
  assert.equal(fetch.body.play_store, true);
});

test('补全组件 leaves the store out when it is not ticked or its files are there already', async () => {
  const d = await diagnostics();
  d.D.openComponentFetch();
  await turns();
  await d.dialogs[0].options.actions.ok.dispatch('click');
  await turns();
  assert.equal('play_store' in d.posts.find(x => x.path === '/api/components/fetch').body, false);
  const ready = await diagnostics({ store: true });
  assert.equal('play_store' in (await agreeIn(ready)).body, false);
});

test('补全组件 waits while an import of the store\'s files runs', async () => {
  const d = await diagnostics();
  d.D.openComponentFetch({ playStore: true });
  await turns();
  d.S.jobs.set('j3', { id: 'j3', kind: 'playstore-import', index: -1, state: 'running', created_at: '' });
  await d.dialogs[0].options.actions.ok.dispatch('click');
  await turns();
  assert.equal(d.posts.length, 0);
  assert.match(d.dialogs[0].options.body.textContent, /正在导入 Google Play 商店的文件/);
});

// ---- 新建 without the store (forms.js)

test('新建 has no Google Play Store choice and never sends play_store', async () => {
  const posts = [];
  const dialogs = [];
  const m = await load('forms.js', {
    'dom.js': dom,
    'api.js': { api: { post: async (path, body) => { posts.push({ path, body }); return { id: 'j1', kind: 'create', index: 0, state: 'running' }; } } },
    'ui.js': {
      openDialog: o => {
        const form = h('form', null, o.body, o.actions?.ok);
        const inputs = all(form).filter(x => x.tagName === 'INPUT');
        form.elements = {};
        for (const name of new Set(inputs.filter(x => x.name).map(x => x.name))) {
          Object.defineProperty(form.elements, name, { get: () => ({ value: inputs.find(x => x.name === name && x.checked)?.value ?? '' }) });
        }
        const ctl = { form, options: o, onClose: [], close() { this.closed = true; } };
        dialogs.push(ctl);
        return ctl;
      },
      toast: noop,
    },
    'actions.js': { onJob: noop, copy: noop, openFolder: noop, stop: noop },
  });
  m.S.list = { instances: [], port_base: 20000 };
  m.mod.openCreate();
  const form = dialogs[0].form;
  assert.equal(all(form).filter(x => x.type === 'checkbox').length, 0);
  assert.doesNotMatch(dialogs[0].options.body.textContent, /Google Play/);
  form.querySelector('#c-disk').value = '64';
  await dialogs[0].options.onSubmit();
  await turns();
  assert.equal(posts.length, 1);
  assert.equal(posts[0].path, '/api/instances');
  assert.equal('play_store' in posts[0].body, false);
});
