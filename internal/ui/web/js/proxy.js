// 按应用走代理 (ModAVD/ModAVD#81): the listed apps of an instance go through a proxy, the others direct. The setting
// is the instance's (GET / PUT /api/instances/{i}/proxy); the 代理 mark on a running instance's card is the proxy
// event's (S.proxyMarks), which the server keeps from what the guest says.

import { h, icon, btn, showError, attr, setText } from './dom.js';
import { api } from './api.js';
import { S, update, subscribe, instance } from './store.js';
import { openDialog } from './ui.js';
import { label } from './format.js';
import { hint, cell, errorBox, setBusy, submit } from './forms.js';

const GMS = 'com.google.android.gms';
const MAX_APPS = 64;
// Android's rule (appproxy.ValidPackage): two or more parts, each a letter then letters, digits or _
const PKG = /^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$/;
const KIND = { socks5: 'SOCKS5', http: 'HTTP 代理' };
const TONE_IC = { ok: 'ok', warn: 'alert', err: 'bad', info: 'info' };
const dialogs = new Map();

// 「SOCKS5，UDP 可用」 / 「HTTP 代理：只转 TCP，UDP 不通」: what 检测 found
export function probeText(r) {
  const k = KIND[r.kind] || r.kind;
  if (r.kind === 'http') return { tone: 'warn', text: `${k}：只转 TCP，UDP 不通（换成代理软件的 SOCKS5 或混合端口就都能用）` };
  if (r.udp === 'ok') return { tone: 'ok', text: `${k}，UDP 可用` };
  if (r.udp === 'unsupported') return { tone: 'warn', text: `${k}，但这个代理不转 UDP：名单里的应用只能用 TCP` };
  if (r.udp === 'failed') return { tone: 'warn', text: `${k}，UDP 没试通：${r.udp_error || '原因不明'}` };
  return { tone: 'ok', text: k };
}

// The card's mark: shown while the setting is on with something listed. Running: the server's mark (the guest's
// state); otherwise when it takes effect.
export function markOf(inst) {
  const ap = inst.app_proxy;
  if (!ap || !ap.enabled || !ap.apps) return null;
  if (inst.status === 'online' && !inst.busy) {
    const m = S.proxyMarks[inst.index];
    return m ? { tone: m.tone, text: m.text } : { tone: 'info', text: '正在问客体里的状态' };
  }
  if (inst.running) return { tone: 'info', text: '实例正在开机或关机' };
  return { tone: 'info', text: `${ap.apps} 个应用 → ${ap.server}，下次开机生效` };
}

// What the saved setting does now (the line under the form; edits count once saved).
function stateOf(v, inst) {
  if (!v.enabled) return { tone: 'info', text: '关着：名单里的应用直连' };
  if (!v.apps.length) return { tone: 'info', text: '开着，但名单是空的：没有应用走代理' };
  if (!inst || !inst.running) return { tone: 'info', text: '实例没开：下次开机生效（用管理器或 modavd 开机时）' };
  if (inst.status !== 'online' || inst.busy) return { tone: 'info', text: '实例正在开机：开完马上生效' };
  const m = S.proxyMarks[inst.index] || v.mark;
  return m ? { tone: m.tone, text: m.text } : { tone: 'info', text: '正在问客体里的状态…' };
}

export function openProxy(i) {
  const inst0 = instance(i);
  if (!inst0) return;
  if (dialogs.has(i)) return;
  let v = null; // the setting as loaded / saved
  let apps = []; // the list being edited, in its order
  let extra = []; // every name listed or typed in this dialog: an unticked one stays, to tick again
  let pkgs = null; // the guest's apps (GET packages), null until listed
  let pkgErr = null;
  let pkgFor = ''; // started_at of the boot pkgs came from
  let stateSig = '';

  const on = h('input', { type: 'checkbox', id: 'p-on' });
  const server = h('input', { class: 'input p-mono', id: 'p-server', autocomplete: 'off', spellcheck: 'false',
    placeholder: 'socks5://127.0.0.1:7890', 'aria-describedby': 'p-server-h' });
  const probeLine = h('p', { class: 'f-hint p-probe', role: 'status', hidden: true });
  const suggestBtn = btn('btn btn-secondary btn-sm', '从系统代理读取', () => suggest());
  const probeBtn = btn('btn btn-secondary btn-sm', '检测', () => probe());
  const list = h('div', { class: 'p-apps', role: 'group', 'aria-labelledby': 'p-apps-t' });
  const listNote = h('p', { class: 'f-hint' });
  const add = h('input', { class: 'input p-mono', id: 'p-add', autocomplete: 'off', spellcheck: 'false', placeholder: '包名，比如 com.example.game', 'aria-label': '手填包名' });
  const addErr = h('p', { class: 'f-err', role: 'alert', hidden: true });
  const count = h('p', { class: 'f-hint p-count' });
  const gms = h('div', { class: 'note-box tone-info has-act p-gms' }, icon('info'),
    h('span', { class: 'nb-text', text: '海外游戏用 Google 登录的，把「Google Play 服务」（com.google.android.gms）也加进名单。' }),
    btn('btn btn-secondary btn-sm', '加进名单', () => { addApp(GMS); renderApps(); }));
  const state = h('div', { class: 'note-box p-state', role: 'status' });
  const box = errorBox();
  const form = h('div', { class: 'p-form', hidden: true },
    h('div', { class: 'fields' },
      h('span', { class: 'f-label', text: '开关' }),
      cell(h('label', { class: 'checkbox p-switch' }, on, h('span', { text: '名单里的应用走代理' }))),
      h('label', { class: 'f-label', for: 'p-server', text: '代理地址' }),
      cell(server, h('div', { class: 'p-btns' }, suggestBtn, probeBtn), probeLine,
        hint('最好用代理软件的 SOCKS5 或混合端口（UDP 也能转）。127.0.0.1 会记成实例里看到的 10.0.2.2。', 'p-server-h'))),
    h('h3', { class: 'sec-title', id: 'p-apps-t', text: '应用名单' }),
    list, listNote,
    h('div', { class: 'p-add' }, add, btn('btn btn-secondary btn-sm', '添加', () => addTyped())), addErr, count,
    gms,
    h('h3', { class: 'sec-title', text: '现在' }), state);
  const loading = h('div', { class: 'p-loading' }, h('span', { class: 'spin' }), h('span', { text: '正在读设置…' }));
  const save = h('button', { class: 'btn btn-primary', type: 'submit', 'aria-disabled': 'true' }, h('span', { text: '保存' }));

  const ctl = openDialog({
    title: '按应用走代理', sub: `${label(inst0)} · ${inst0.name}`, cls: 'drawer dlg-proxy', form: true, focus: '#p-on',
    body: h('div', { class: 'dlg-body' },
      h('p', { class: 'p-intro', text: '名单里的应用走代理，其余照常直连。代理连不上时名单里的应用断网，不会改走直连。' }),
      loading, form, box),
    actions: { ok: save },
    onSubmit() {
      if (!v) return;
      if (apps.length > MAX_APPS) {
        showError(box, { message: `名单里最多 ${MAX_APPS} 个应用（现在 ${apps.length} 个）`, advice: '去掉几个再保存' });
        return;
      }
      submit(ctl, save, '正在保存…', box, async () => {
        const body = { enabled: on.checked, apps: [...apps] };
        const typed = server.value.trim();
        if (typed && typed !== v.server) body.server = typed;
        const r = await api.put(`/api/instances/${i}/proxy`, body);
        took(r);
        return savedToast(r);
      });
    },
  });
  dialogs.set(i, ctl);

  // ---- the setting
  async function load() {
    try {
      took(await api.get(`/api/instances/${i}/proxy`));
    } catch (e) {
      loading.hidden = true;
      showError(box, e);
      return;
    }
    on.checked = v.enabled;
    server.value = v.server;
    apps = [...v.apps];
    extra = [...v.apps];
    loading.hidden = true;
    form.hidden = false;
    attr(save, 'aria-disabled', null);
    on.focus({ preventScroll: true });
    render();
  }

  // r: an answer of GET / PUT; the cards follow it at once (the events catch up)
  function took(r) {
    v = r;
    if (r.mark) S.proxyMarks[i] = r.mark;
    else delete S.proxyMarks[i];
    const k = S.list ? S.list.instances.findIndex((x) => x.index === i) : -1;
    if (k >= 0) S.list.instances[k].app_proxy = { enabled: r.enabled, server: r.server, apps: r.apps.length };
    update();
  }

  // ---- the address
  let asking = false;
  async function ask(b, text, work) {
    if (asking) return;
    asking = true;
    setBusy(b, true, text);
    try {
      await work();
    } catch (e) {
      showProbe('err', e.message, e.advice ? [e.advice] : []);
    } finally {
      asking = false;
      setBusy(b, false);
    }
  }
  function showProbe(tone, text, notes) {
    probeLine.hidden = false;
    probeLine.className = 'f-hint p-probe tone-' + tone;
    probeLine.replaceChildren(icon(TONE_IC[tone]), h('span', null, h('span', { text }), ...(notes || []).map((n) => h('span', { class: 'p-note', text: n }))));
  }
  const suggest = () => ask(suggestBtn, '正在读取…', async () => {
    const r = await api.get('/api/proxy/suggest');
    server.value = r.server;
    showProbe('info', '已填入这台电脑的代理设置', r.notes);
    await probeNow(r.notes);
  });
  const probe = () => ask(probeBtn, '正在检测…', () => probeNow([]));
  async function probeNow(before) {
    const typed = server.value.trim();
    if (!typed) {
      showProbe('err', '先填代理地址，或者点「从系统代理读取」');
      server.focus();
      return;
    }
    const r = await api.post('/api/proxy/probe', { server: typed });
    server.value = r.server;
    const t = probeText(r);
    showProbe(t.tone, t.text, [...before, ...r.notes]);
  }

  // ---- the list
  function addApp(p) {
    if (!apps.includes(p)) apps.push(p);
    if (!extra.includes(p)) extra.push(p);
  }
  function addTyped() {
    const p = add.value.trim();
    addErr.hidden = true;
    attr(add, 'aria-invalid', null);
    if (!p) return;
    if (!PKG.test(p)) {
      addErr.replaceChildren(icon('alert'), h('span', { text: `「${p}」不是安卓包名：要像 com.example.game 这样，几段字母开头的名字用点连起来` }));
      addErr.hidden = false;
      attr(add, 'aria-invalid', true);
      add.focus();
      return;
    }
    addApp(p);
    add.value = '';
    renderApps();
  }
  // Enter in the box adds the name; it does not save the dialog
  add.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      addTyped();
    }
  });
  add.addEventListener('input', () => { addErr.hidden = true; attr(add, 'aria-invalid', null); });

  let appsSig = '';
  function renderApps() {
    const known = new Map((pkgs || []).map((p) => [p.package, p]));
    // the guest's apps, then the listed (or once listed) ones it does not have
    const rows = [...(pkgs || []), ...extra.filter((p) => !known.has(p)).map((p) => ({ package: p, name: '', missing: !!pkgs }))];
    const sig = JSON.stringify([rows.map((r) => r.package), apps]);
    if (sig !== appsSig) {
      appsSig = sig;
      list.replaceChildren(...rows.map((p) => {
        const chk = h('input', { type: 'checkbox', checked: apps.includes(p.package), 'aria-label': p.name ? `${p.name}（${p.package}）` : p.package });
        chk.addEventListener('change', () => {
          if (chk.checked) addApp(p.package);
          else apps = apps.filter((x) => x !== p.package);
          renderApps();
        });
        return h('label', { class: 'checkbox p-app' }, chk,
          h('span', { class: 'p-app-main' },
            h('span', { class: 'p-app-name', text: p.name || p.package }),
            p.name && h('span', { class: 'p-app-pkg', text: p.package })),
          p.system && h('span', { class: 'tag', text: '系统' }),
          p.missing && h('span', { class: 'tag', text: '没装', title: '装上就生效' }));
      }));
      if (!rows.length) list.replaceChildren(h('p', { class: 'p-empty', text: pkgs ? '实例里没有装别的应用' : '名单是空的' }));
    }
    setText(count, apps.length > MAX_APPS ? `已选 ${apps.length} 个，最多 ${MAX_APPS} 个` : `已选 ${apps.length} 个`);
    count.classList.toggle('tone-err', apps.length > MAX_APPS);
    gms.hidden = apps.includes(GMS);
  }

  // the guest's apps: once per boot, when it runs
  async function loadPackages(inst) {
    pkgFor = inst.started_at || 'on';
    pkgErr = null;
    setText(listNote, '正在读实例里装的应用…');
    try {
      const r = await api.get(`/api/instances/${i}/packages`);
      pkgs = r.packages;
    } catch (e) {
      pkgErr = e;
      pkgFor = '';
    }
    render();
  }

  function render() {
    if (!v || ctl.closed) return;
    const inst = instance(i);
    if (!inst) { ctl.close(); return; }
    const up = inst.status === 'online';
    if (up && pkgFor !== (inst.started_at || 'on') && !pkgErr) loadPackages(inst);
    if (!up) { pkgs = null; pkgFor = ''; pkgErr = null; }
    setText(listNote, pkgErr ? `列不出实例里装的应用：${pkgErr.message}。可以手填包名。`
      : !up ? '实例没开：开机后这里会列出装了哪些应用；现在可以手填包名。'
        : pkgs ? '打勾的应用走代理；「没装」的装上就生效。' : '正在读实例里装的应用…');
    renderApps();
    const s = stateOf(v, inst);
    if (stateSig === s.tone + s.text) return; // a status region: changed text only
    stateSig = s.tone + s.text;
    state.className = 'note-box p-state tone-' + s.tone;
    state.replaceChildren(icon(TONE_IC[s.tone]), h('span', { class: 'nb-text', text: s.text }));
  }

  ctl.onClose.push(subscribe(render));
  ctl.onClose.push(() => dialogs.delete(i));
  load();
}

function savedToast(r) {
  const text = r.notes.join('；') || undefined;
  switch (r.applied) {
    case 'now':
      if (r.mark && r.mark.tone === 'err') return { tone: 'err', title: '已保存，但没生效', text: r.mark.text };
      return { title: '已生效', text };
    case 'failed':
      return { tone: 'err', title: '已保存，但没生效', text: r.error };
    case 'booting':
      return { title: '已保存，开完机马上生效', text };
    case 'next_boot':
      return { title: '已保存，下次开机生效', text };
    default:
      return { title: '已关，名单里的应用直连', text };
  }
}
