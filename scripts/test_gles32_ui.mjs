// Run: node --experimental-vm-modules --test scripts/test_gles32_ui.mjs
// 报告 GLES 3.2（实验）in the manager's page (ModAVD/ModAVD#131): the instance menu's item, the question before the job,
// the job's toast and texts. Runs the page's own modules, stubbing only their browser / HTTP neighbours.
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
    'playstore.js': { openPlayStore: noop, openGSFID: noop },
    'gles32.js': { openGLES32: (i, action) => calls.push([i, action]), GLES32_NOTE: '只改报告的版本' },
  });
  for (const j of jobs) m.S.jobs.set(j.id, j);
  const items = m.mod.menuItems({ i: 3, inst: { index: 3, status: 'offline', running: false, initialized: true, bridge: 'installed', ...inst } }).filter(Boolean);
  return { items, calls, gl: items.find(x => /GLES 3\.2/.test(x.label || '')) };
}

test('an instance with the switch off offers 开启, which asks gles32.js to turn it on; it sits after the store items', async () => {
  const { items, gl, calls } = await menuFor({});
  assert.equal(items.filter(x => /GLES 3\.2/.test(x.label || '')).length, 1);
  assert.equal(gl.label, '开启报告 GLES 3.2（实验）…');
  assert.ok(!gl.disabled);
  assert.match(gl.hint, /渲染仍是 3\.1/);
  assert.ok(items.indexOf(gl) > items.findIndex(x => /Google Play 商店/.test(x.label || '')));
  gl.run();
  assert.deepEqual(calls, [[3, 'on']]);
});

test('an instance with the switch on offers 关闭 instead', async () => {
  const { gl, calls } = await menuFor({ gles32: true, status: 'online', running: true });
  assert.equal(gl.label, '关闭报告 GLES 3.2（实验）…');
  assert.ok(!gl.disabled);
  gl.run();
  assert.deepEqual(calls, [[3, 'off']]);
});

test('without the execution bridge 开启 is off and says why; 关闭 still works', async () => {
  const off = await menuFor({ bridge: 'off' });
  assert.ok(off.gl.disabled);
  assert.equal(off.gl.hint, '要执行桥');
  const had = await menuFor({ bridge: 'off', gles32: true });
  assert.ok(!had.gl.disabled);
});

test('the item waits while the instance is being created, set up or busy with a job', async () => {
  for (const [inst, jobs] of [[{ status: 'creating', running: true }, []], [{ status: 'create_failed' }, []],
    [{}, [{ id: 'j9', kind: 'disk-compact', index: 3, state: 'running' }]],
    [{ gles32: true }, [{ id: 'j8', kind: 'gles32-off', index: 3, state: 'running' }]]]) {
    const { gl } = await menuFor(inst, jobs);
    assert.ok(gl.disabled, JSON.stringify(inst));
  }
});

// ---- the question (gles32.js)

async function dialog() {
  const r = { dialogs: [], errors: [], posts: [] };
  const m = await load('gles32.js', {
    'dom.js': dom,
    'ui.js': {
      openDialog: o => {
        const ctl = { options: o, onClose: [], closed: false, close() { if (!this.closed) { this.closed = true; for (const fn of this.onClose) fn(); } } };
        r.dialogs.push(ctl);
        return ctl;
      },
      toastError: e => r.errors.push(e),
    },
    'actions.js': { gles32: (i, action, onErr) => r.posts.push({ i, action, onErr }) },
  });
  m.S.list = { instances: [{ index: 3, name: 'mas_3', title: '明日方舟', status: 'online', running: true }, { index: 4, name: 'mas_4', title: '星铁', status: 'offline', running: false, play_store: true }] };
  return Object.assign(r, { open: m.mod.openGLES32, S: m.S });
}

test('开启 asks first, says it is an experiment and boots cold, and only then starts the job', async () => {
  const p = await dialog();
  p.open(3, 'on');
  const d = p.dialogs[0];
  assert.equal(d.options.title, '开启「报告 GLES 3.2」？');
  const text = d.options.body.textContent;
  assert.match(text, /实验功能/);
  assert.match(text, /实际渲染仍是 3\.1/);
  assert.match(text, /冷启动一次/);
  assert.match(text, /会先关机/); // it is running
  assert.ok(!/清除数据/.test(text)); // no store on it
  assert.equal(p.posts.length, 0);
  p.open(3, 'on'); // a second click while it asks opens nothing more
  assert.equal(p.dialogs.length, 1);
  d.options.onSubmit();
  assert.equal(d.closed, true);
  assert.equal(p.posts.length, 1);
  assert.deepEqual(plain({ i: p.posts[0].i, action: p.posts[0].action }), { i: 3, action: 'on' });
  p.open(3, 'on'); // asking again is possible once it closed
  assert.equal(p.dialogs.length, 2);
});

test('an instance with the Play Store is told to clear the store\'s data; a closed one is booted in the background', async () => {
  const p = await dialog();
  p.open(4, 'on');
  const text = p.dialogs[0].options.body.textContent;
  assert.match(text, /Google Play 商店 → 存储，清除数据/);
  assert.match(text, /账号不会退出/);
  assert.match(text, /后台开机/);
});

test('关闭 asks with its own words; a refusal of the server is a toast', async () => {
  const p = await dialog();
  p.open(3, 'off');
  const d = p.dialogs[0];
  assert.equal(d.options.title, '关闭「报告 GLES 3.2」？');
  d.options.onSubmit();
  assert.equal(p.posts[0].action, 'off');
  const e = { code: 'not_ready', message: '没有执行桥', details: { reason: 'gles32_no_bridge' } };
  p.posts[0].onErr(e);
  assert.deepEqual(p.errors, [e]);
});

// ---- the outcome (actions.js, format.js)

async function actions() {
  const r = { toasts: [], posts: [] };
  const m = await load('actions.js', {
    'api.js': { api: { post: async (path, body) => { r.posts.push({ path, body }); return { id: 'j1', kind: 'gles32-on', state: 'running', index: 3 }; }, get: async () => ({}) } },
    'ui.js': { toast: t => r.toasts.push(t), toastError: e => r.toasts.push({ error: e }), copyText: noop },
    'panels.js': { openActivity: noop, runCheck: noop },
  });
  m.S.list = { instances: [{ index: 3, name: 'mas_3', title: '明日方舟', status: 'online', running: true }] };
  return Object.assign(r, { mod: m.mod, S: m.S });
}

test('gles32 posts to gles32-on / gles32-off as an instance job', async () => {
  const a = await actions();
  await a.mod.gles32(3, 'on');
  await a.mod.gles32(3, 'off');
  assert.deepEqual(plain(a.posts.map(x => x.path)), ['/api/instances/3/gles32-on', '/api/instances/3/gles32-off']);
});

test('a finished 开启 says what the guest reports, and the store note when there is one', async () => {
  const a = await actions();
  const note = 'Play 商店会记着旧的设备信息；要它按新版本重新判断兼容，到 设置 → 应用 → Google Play 商店 → 存储，清除数据（账号不会退出）';
  a.mod.notifyJob({ id: 'j1', kind: 'gles32-on', index: 3, state: 'done', result: { state: 'on', enabled: true, prop: '196610', restarted: true, stopped: false, notes: [note] } });
  const t = a.toasts[0];
  assert.equal(t.tone, 'ok');
  assert.match(t.title, /开启报告 GLES 3\.2了/);
  assert.match(t.text, /ro\.opengles\.version=196610/);
  assert.match(t.text, /冷启动过一次/);
  assert.match(t.text, /清除数据/);
});

test('a 开启 that did not take is a warning with the activity link; a failed job names its kind', async () => {
  const a = await actions();
  a.mod.notifyJob({ id: 'j2', kind: 'gles32-on', index: 3, state: 'done', result: { state: 'pending-on', prop: '196609', stopped: true, notes: [] } });
  assert.equal(a.toasts[0].tone, 'warn');
  assert.match(a.toasts[0].text, /还没有生效/);
  assert.ok(a.toasts[0].action);
  a.mod.notifyJob({ id: 'j3', kind: 'gles32-off', index: 3, state: 'failed', error: { message: '冷启动后对不上', advice: '再看一次' } });
  assert.equal(a.toasts[1].tone, 'err');
  assert.match(a.toasts[1].title, /关闭报告 GLES 3\.2没有成功/);
  a.mod.notifyJob({ id: 'j4', kind: 'gles32-off', index: 3, state: 'done', result: { state: 'off', prop: '196609', restarted: true, notes: [] } });
  assert.equal(a.toasts[2].tone, 'ok');
  assert.match(a.toasts[2].text, /改回报 GLES 3\.1/);
});
