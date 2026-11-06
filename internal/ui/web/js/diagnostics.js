// 补全组件: the license dialog (from 电脑检查's 组件 item, the banner and its retry), and 设置 → 组件与问题反馈:
// component status, 收集问题信息, 复制配置信息. Everything runs as API jobs; closing a dialog does not stop them.

import { h, icon, btn, setText, showError, attr } from './dom.js';
import { S, update, subscribe, newestFirst } from './store.js';
import { api } from './api.js';
import { openDialog, toast, toastError, copyText } from './ui.js';
import { openCheck } from './panels.js';
import { fmtBytes } from './format.js';
import * as A from './actions.js';

// before a package exists; afterwards the panel shows the note that came with it (what is inside, what may be shared)
const PRIVACY_BEFORE = '问题包只保存在这台电脑上，不会自动上传。生成以后，这里会写明包里有什么、哪些可以公开、哪些不要公开。';

const SOURCES = [['auto', '自动'], ['googledownloads_cn', 'googledownloads_cn'], ['tencent', 'tencent'], ['google', 'google'], ['custom', '自定义地址']];

export function loadDiagnostics() {
  return api.get('/api/components').then((v) => { S.components = v; S.componentsError = null; update(); }).catch((e) => {
    S.componentsError = e;
    update();
    throw e;
  });
}

export const activeJob = (kind) => [...S.jobs.values()].filter((j) => j.kind === kind && j.state === 'running').sort(newestFirst)[0] || null;
export const latestJob = (kind) => [...S.jobs.values()].filter((j) => j.kind === kind).sort(newestFirst)[0] || null;

// The progress line of a components / collect-info job: the backend's own line when there is one (a download's
// already has the percentage, the sizes, the speed and the time left), else the step with the numbers.
export function progressText(j) {
  const x = j && j.progress;
  if (!x) return '正在准备…';
  const m = (x.message || '').trim().replace(/\s{2,}/g, '  ');
  if (m) return m;
  const bits = [x.name || x.step || '进行中'];
  if (x.total > 0) {
    const percent = Math.max(0, Math.min(100, Math.round((x.done || 0) * 100 / x.total)));
    bits.push(`${percent}%（${Math.round((x.done || 0) / 1048576)} / ${Math.round(x.total / 1048576)} MB）`);
  }
  return bits.join(' · ');
}

// a job just started from this window: its toast comes when it ends (the stream may have been faster than the POST)
function startJob(p) {
  return p.then((j) => {
    const seen = S.jobs.get(j.id);
    if (seen && seen.state !== 'running') A.notifyJob(seen);
    else {
      S.watch.add(j.id);
      A.onJob(j);
    }
    return j;
  });
}

const splitURLs = (v) => v.split(',').map((x) => x.trim()).filter(Boolean);

function licenseURL(source, urls) {
  const q = new URLSearchParams({ source });
  for (const u of urls) q.append('mirror_urls', u);
  return `/api/components/license?${q}`;
}

let licenseCtl = null;

// ---- Google Play 商店服务 (API.md "Google Play 商店"): the root's five files of the optional store, which an instance's
// 「补全 Google Play 商店」 puts in. 补全组件 downloads them when its box is ticked (Google's Play image, only the five
// files are kept); 「使用我自己的文件…」 imports that image's zip or the five files the user already has.
const STORE_FROM = { download: '从 Google 官方下载', import: '用自己的文件导入' };
const MAX_IMPORT = 8; // the server's limit: the five files and the zip, with room for one given twice
let upload = null; // {n, bytes} while an import's files are being handed to the server

export const storeStatus = () => (S.components && S.components.play_store) || null;

function storeReadyText(st) {
  const bits = [STORE_FROM[st.source], st.version && `r${st.version}`].filter(Boolean);
  return `Google Play 商店服务已就绪${bits.length ? `（${bits.join('，')}）` : ''}`;
}

const refusal = (message, advice) => Object.assign(new Error(message), { advice });

// importPlayStore hands the picked files to the server (each one a "file" part, like an APK upload) and starts the
// import job; its toast comes when it ends. The size and hash checks are the server's.
export async function importPlayStore(files) {
  files = [...files];
  if (!files.length) return null;
  if (activeJob('components') || activeJob('playstore-import') || upload) throw refusal('组件正在补全或导入', '等它做完再导入');
  if (files.length > MAX_IMPORT) throw refusal(`一次最多选 ${MAX_IMPORT} 个文件`, '只要 Google Play 版镜像的压缩包 x86_64-34_r14.zip，或者商店的五个文件');
  const empty = files.find((f) => !f.size);
  if (empty) throw refusal(`${empty.name} 是空文件`, '换成 Google Play 版镜像的压缩包，或者商店的五个文件');
  const fd = new FormData();
  for (const f of files) fd.append('file', f, f.name);
  upload = { n: files.length, bytes: files.reduce((n, f) => n + f.size, 0) };
  update();
  try {
    return await startJob(api.post('/api/components/import-playstore', fd));
  } finally {
    upload = null;
    update();
  }
}

// storeBox: the store's part of 补全组件 (mode 'fetch': a box to tick, checked to start with when asked) and of
// 电脑检查 (mode 'check': its own row with 下载…). Both offer 「使用我自己的文件…」 and show an import that runs or
// failed. fillStoreBox updates it from S; b.wanted() says whether the fetch should take the store along.
export function storeBox(mode, checked) {
  const b = { mode, sig: null };
  // the picker lives inside the dialog: the rest of the page is inert while a dialog is open
  b.input = h('input', { type: 'file', multiple: true, accept: '.zip,.apk,.der', hidden: true, tabindex: '-1', 'aria-hidden': 'true' });
  b.input.addEventListener('change', () => {
    const files = [...b.input.files];
    b.input.value = '';
    b.local = null;
    importPlayStore(files).catch((e) => { b.local = e; b.sig = null; fillStoreBox(b); });
    fillStoreBox(b);
  });
  b.own = btn('btn btn-ghost btn-sm', [icon('folder'), '使用我自己的文件…'], () => { if (b.own.getAttribute('aria-disabled') !== 'true') b.input.click(); });
  b.state = h('p', { class: 'store-state tone-ok', hidden: true });
  b.text = h('span');
  b.cancel = btn('btn btn-ghost btn-sm', '取消', () => b.id && A.cancelJob(b.id));
  b.running = h('div', { class: 'diag-running', role: 'status', hidden: true }, h('span', { class: 'spin' }), b.text, b.cancel);
  b.err = h('div', { class: 'form-error', role: 'alert', hidden: true });
  if (mode === 'fetch') {
    b.box = h('input', { type: 'checkbox', id: 'lic-store', checked: !!checked, 'aria-describedby': 'lic-store-h' });
    b.choose = h('div', { class: 'store-choose' },
      h('label', { class: 'checkbox' }, b.box, h('span', { text: '同时补全 Google Play 商店服务（约 1.5 GB，可选）' })),
      h('p', { class: 'f-hint', id: 'lic-store-h', text: '勾上就一起从 Google 官方下载 Google Play 版系统镜像（同样受这份协议约束），只留商店的五个文件；新建的实例不带商店，要用的实例在「更多操作」里补全。已经有镜像压缩包 x86_64-34_r14.zip 或那五个文件的，直接导入，不用下载：' }),
      h('div', { class: 'store-own' }, b.own));
    b.el = h('div', { class: 'store-opt' }, b.choose, b.state, b.running, b.err, b.input);
  } else {
    b.pill = h('span', { class: 'pill tone-idle', text: '没有' });
    b.get = btn('btn btn-secondary btn-sm', [icon('download'), '下载…'], () => { if (b.get.getAttribute('aria-disabled') !== 'true') openComponentFetch({ playStore: true }); });
    b.choose = h('div', { class: 'diag-actions' }, b.get, b.own);
    b.el = h('section', { class: 'store-box', 'aria-labelledby': 'store-box-t', tabindex: '-1' },
      h('div', { class: 'store-head' }, h('strong', { id: 'store-box-t', text: 'Google Play 商店服务' }), h('span', { class: 'tag', text: '可选' }), h('span', { class: 'spacer' }), b.pill),
      h('p', { class: 'store-note', text: '给实例补全 Google Play 商店要用。从 Google 官方下载 Google Play 版系统镜像（约 1.5 GB），只留商店的五个文件；或者用自己的镜像压缩包 x86_64-34_r14.zip、那五个文件导入。新建的实例不带商店。' }),
      b.state, b.running, b.err, b.choose, b.input);
  }
  b.wanted = () => mode === 'fetch' && !!b.box.checked && !b.box.disabled && !(storeStatus() || {}).ready;
  fillStoreBox(b);
  return b;
}

export function fillStoreBox(b) {
  const st = storeStatus();
  const ready = !!(st && st.ready);
  const imp = activeJob('playstore-import');
  // 电脑检查 has no other place that shows a fetch outside a failed 组件 item: say it here too
  const run = imp || (b.mode === 'check' ? activeJob('components') : null);
  const last = imp ? null : latestJob('playstore-import');
  const failed = b.local || (last && last.state === 'failed' ? last.error || {} : null);
  b.choose.hidden = ready;
  if (b.mode === 'check') {
    setText(b.pill, ready ? '已就绪' : st ? '没有' : '未知');
    b.pill.className = 'pill tone-' + (ready ? 'ok' : 'idle');
    for (const x of [b.get, b.own]) attr(x, 'aria-disabled', run || upload ? true : null);
  } else {
    b.box.disabled = !!(imp || upload);
    attr(b.own, 'aria-disabled', run || upload ? true : null);
  }
  b.state.hidden = !ready;
  if (ready) setText(b.state, `${storeReadyText(st)}。要用的实例在「更多操作」里补全商店。`);
  b.running.hidden = !(run || upload);
  b.cancel.hidden = !run;
  if (run) {
    b.id = run.id;
    setText(b.text, `${run.kind === 'components' ? '正在补全组件' : '正在导入'}：${progressText(run)}`);
  } else if (upload) setText(b.text, `正在把 ${upload.n} 个文件交给后台（共 ${fmtBytes(upload.bytes)}）…`);
  const sig = failed && !ready && !run && !upload ? `${failed.message}|${failed.advice}` : '';
  if (b.sig !== sig) {
    b.sig = sig;
    if (sig) showError(b.err, { message: `没有导入：${failed.message || '原因未知'}`, advice: A.importAdvice(failed) });
    b.err.hidden = !sig;
  }
}

// 补全组件: pick the source, read the license that source serves, agree, the job starts. The dialog opens at once
// with the saved source, so a source that cannot be reached shows its error here and another one can be picked.
// opts.playStore ticks 「同时补全 Google Play 商店服务」 to start with (电脑检查's 下载…). Called as a click handler
// too (opts is the event then).
export function openComponentFetch(opts) {
  if (licenseCtl) return;
  const running = activeJob('components') || activeJob('playstore-import');
  if (running) {
    toast({ tone: 'info', title: running.kind === 'components' ? '组件正在补全' : '正在导入 Google Play 商店的文件', text: progressText(running) });
    return;
  }
  const select = h('select', { class: 'input', id: 'lic-src' }, ...SOURCES.map(([value, text]) => h('option', { value, text })));
  const custom = h('input', { class: 'input', id: 'lic-urls', placeholder: 'https://example/AndroidSDK/', spellcheck: 'false', autocomplete: 'off' });
  const customLabel = h('label', { class: 'f-label', for: 'lic-urls', text: '自定义地址' });
  const customCtl = h('div', { class: 'f-ctl' }, custom,
    h('p', { class: 'f-hint', text: '一个或几个 HTTP(S) 目录地址，用逗号分开，按顺序试。不管从哪里下载，文件都按管理器里写好的校验值核对。' }));
  const fields = h('div', { class: 'fields' },
    h('label', { class: 'f-label', for: 'lic-src', text: '下载源' }), h('div', { class: 'f-ctl' }, select), customLabel, customCtl);
  const status = h('div', { class: 'form-error', role: 'alert', hidden: true });
  const note = h('p', { class: 'f-hint diag-license-from' });
  const text = h('pre', { class: 'loglines diag-license', tabindex: '0' });

  let shown = null; // {sig, sha}: the text on screen, read from that source
  let seq = 0;
  const pick = () => {
    const source = select.value;
    const urls = source === 'custom' ? splitURLs(custom.value) : [];
    return { source, urls, sig: `${source}|${urls.join(',')}` };
  };
  const fail = (e) => {
    showError(status, e);
    status.append(btn('btn btn-ghost btn-sm', [icon('refresh'), '重新获取'], () => { shown = null; load(); }));
  };
  const load = async () => {
    const p = pick();
    customLabel.hidden = customCtl.hidden = p.source !== 'custom';
    if (shown && shown.sig === p.sig) return;
    const n = ++seq;
    shown = null;
    agree.disabled = true;
    status.hidden = true;
    text.hidden = true;
    if (p.source === 'custom' && !p.urls.length) {
      setText(note, '填好自定义地址后，这里显示从它取到的许可协议全文。');
      return;
    }
    setText(note, '正在从这个下载源取许可协议…');
    try {
      const v = await api.get(licenseURL(p.source, p.urls));
      if (n !== seq) return; // another choice was made meanwhile
      if (!v.license || !v.license_sha256) throw new Error('后台没有返回许可协议全文');
      setText(text, v.license);
      text.hidden = false;
      text.scrollTop = 0;
      setText(note, `下面是从 ${v.license_source || '这个下载源'} 取到的全文。同意后从同一个下载源下载；到时取到的协议和这份不一样，就什么也不下。`);
      shown = { sig: p.sig, sha: v.license_sha256 };
      agree.disabled = false;
    } catch (e) {
      if (n !== seq) return;
      setText(note, '没有取到这个下载源的许可协议，可以换一个下载源。');
      fail(e);
    }
  };
  const agree = btn('btn btn-primary', '同意并下载', async () => {
    const p = pick();
    if (!shown || shown.sig !== p.sig) { load(); return; } // the choice changed after the text was read
    status.hidden = true;
    // an import of the store's files holds the same lock: the server would answer busy
    if (activeJob('playstore-import') || upload) {
      showError(status, { message: '正在导入 Google Play 商店的文件', advice: '等它做完再下载' });
      return;
    }
    agree.disabled = true;
    const body = { source: p.source, mirror_urls: p.urls };
    try {
      await api.put('/api/components/source', body);
      const fetchBody = { accept_license: true, ...body, license_sha256: shown.sha };
      if (store.wanted()) fetchBody.play_store = true;
      await startJob(api.post('/api/components/fetch', fetchBody));
      d.close();
    } catch (e) {
      agree.disabled = false;
      fail(e);
    }
  });
  select.addEventListener('change', load);
  custom.addEventListener('change', load);
  custom.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); load(); } });

  // the optional Google Play Store, under the license it is fetched under too
  const store = storeBox('fetch', opts && opts.playStore === true);
  const d = openDialog({
    title: 'Android 软件开发套件许可协议', sub: '组件由 Google 提供。先选下载源，读完从它取到的许可协议再决定。', cls: 'dlg-update', light: true,
    body: h('div', { class: 'dlg-body diag-license-body' }, fields, status, note, text, store.el),
    actions: { left: btn('btn btn-ghost', '不同意', () => d.close()), cancel: false, ok: agree },
  });
  licenseCtl = d;
  const un = subscribe(() => fillStoreBox(store));
  d.onClose.push(() => {
    licenseCtl = null;
    seq++;
    un();
  });
  loadDiagnostics().catch(() => {});
  agree.disabled = true;
  api.get('/api/components/source').catch(() => ({})).then((saved) => {
    if (licenseCtl !== d) return; // closed meanwhile
    select.value = SOURCES.some(([v]) => v === saved.source) ? saved.source : 'auto';
    custom.value = (saved.mirror_urls || []).join(',');
    load();
  });
}

let ctl = null;

export function openDiagnostics() {
  if (ctl) return;
  const componentBox = h('div', { class: 'diag-components' });
  const componentError = h('div', { class: 'form-error', role: 'alert', hidden: true });
  const toCheck = btn('btn btn-ghost', [icon('shield'), '去电脑检查补全'], () => { ctl.close(); openCheck(); });
  const compHead = h('div', { class: 'diag-head' }, h('strong', { text: '组件' }), toCheck);
  const compNote = h('p', { class: 'f-hint', text: '缺组件时，在「电脑检查」的组件一项点「补全组件」：先选下载源、读许可协议，同意后才下载。可选的 Google Play 商店服务也在「电脑检查」里补全。' });
  const componentState = h('p', { class: 'diag-state', hidden: true });

  const collectBtn = btn('btn btn-secondary', [icon('file'), '收集问题信息'], () => startJob(api.post('/api/diag/collect', {})).catch(toastError));
  const configBtn = btn('btn btn-ghost', [icon('copy'), '复制配置信息'], async () => {
    try {
      const v = await api.get('/api/diag/report-config');
      const text = v.text || '';
      openConfig(text, await copyText(text));
    } catch (e) { toastError(e); }
  });
  const feedbackHead = h('div', { class: 'diag-head' }, h('strong', { text: '问题反馈' }), h('div', { class: 'diag-actions' }, collectBtn, configBtn));
  let collectID = '';
  const collectText = h('span');
  const collectRunning = h('p', { class: 'diag-running', hidden: true }, h('span', { class: 'spin' }), collectText,
    btn('btn btn-ghost btn-sm', '取消', () => collectID && A.cancelJob(collectID)));
  const collectResult = h('div', { class: 'diag-result' });
  let collectSig = null;
  const privacy = h('p', { class: 'f-hint diag-privacy', text: PRIVACY_BEFORE });

  ctl = openDialog({ title: '组件与问题反馈', sub: '组件状态、收集问题信息和复制配置信息', cls: 'dlg-update', light: true,
    body: h('div', { class: 'dlg-body' }, compHead, compNote, componentError, componentBox, componentState, feedbackHead, collectRunning, collectResult, privacy) });

  let compSig = null;
  const render = () => {
    const c = S.components;
    const sig = c && Array.isArray(c.components) ? JSON.stringify(c.components) : '';
    if (sig !== compSig) {
      compSig = sig;
      componentBox.replaceChildren(...(c && Array.isArray(c.components) ? c.components : []).map((x) => h('div', { class: 'comp-row' },
        h('span', null, x.name || x.id, x.optional && h('span', { class: 'muted', text: '（可选）' })),
        h('span', { class: 'v', text: x.version || x.build || '' }),
        h('span', { class: 'pill tone-' + (x.installed ? 'ok' : x.optional ? 'idle' : 'err'), text: x.installed ? '已安装' : x.optional ? '未安装' : '缺少' }))));
    }
    if (S.componentsError) showError(componentError, S.componentsError);
    componentError.hidden = !S.componentsError;

    const fj = activeJob('components');
    const last = fj || latestJob('components');
    const missing = !!(c && Array.isArray(c.components) && c.components.some((x) => !x.optional && !x.installed));
    toCheck.hidden = !missing && !fj && last?.state !== 'failed';
    let state = '';
    let tone = '';
    if (fj) state = `正在补全组件：${progressText(fj)}`;
    else if (last?.state === 'done') {
      // the list above is from before: read it again once per finished job
      if (ctl.refreshedComponentsJob !== last.id && ctl.refreshingComponentsJob !== last.id) {
        ctl.refreshingComponentsJob = last.id;
        loadDiagnostics().catch(() => {}).finally(() => { if (ctl) { ctl.refreshedComponentsJob = last.id; update(); } });
      }
      [state, tone] = ['组件补全完成。', 'tone-ok'];
    } else if (last?.state === 'failed') [state, tone] = [`上次补全组件没有成功：${last.error?.message || '原因未知'}`, 'tone-err'];
    else if (last?.state === 'canceled') [state, tone] = ['上次补全组件已取消。', 'tone-idle'];
    setText(componentState, state);
    componentState.className = 'diag-state ' + tone;
    componentState.hidden = !state;

    const cj = activeJob('collect-info');
    collectBtn.disabled = !!cj;
    collectRunning.hidden = !cj;
    if (cj) {
      collectID = cj.id;
      setText(collectText, `收集问题信息：${progressText(cj)}`);
    }
    const done = cj ? null : latestJob('collect-info');
    const rsig = done ? `${done.id}:${done.state}` : '';
    if (rsig !== collectSig) {
      collectSig = rsig;
      collectResult.replaceChildren(...collectRows(done));
      setText(privacy, done?.state === 'done' && done.result?.privacy_note ? done.result.privacy_note : PRIVACY_BEFORE);
    }
  };
  const un = subscribe(render);
  ctl.onClose.push(() => { un(); ctl = null; });
  loadDiagnostics().catch(() => {});
  render();
}

function collectRows(j) {
  if (!j) return [];
  if (j.state === 'failed') return [h('p', { class: 'tone-err', text: j.error?.message || '收集问题信息没有成功。' })];
  if (j.state === 'canceled') return [h('p', { class: 'tone-idle', text: '已取消收集问题信息。' })];
  const r = j.result;
  if (j.state !== 'done' || !r) return [];
  const rows = [h('p', { class: 'diag-path', text: `问题包：${r.zip_path || '已生成'}${r.zip_size ? `（${r.zip_size}）` : ''}` }),
    h('div', { class: 'diag-actions' }, btn('btn btn-ghost', [icon('folder'), '在资源管理器里显示'],
      () => api.post('/api/open', { what: 'collect-info', job: j.id }).catch(toastError)))];
  if (r.sections?.length) rows.push(h('ul', { class: 'diag-sections' }, ...r.sections.map((x) => h('li', { class: x.error ? 'tone-err' : 'tone-ok', text: `${x.name}：${x.error || '完成'}` }))));
  else if (r.problems?.length) rows.push(h('p', { class: 'f-hint', text: `有 ${r.problems.length} 节没收集成功，原因写在包里的 summary.txt，照样提交就行。` }));
  if (r.mas_found === false) rows.push(h('p', { class: 'f-hint', text: '没找到 AUTO-MAS 安装目录，反馈时请附上 AUTO-MAS 的 debug 文件夹里的日志。' }));
  return rows;
}

function openConfig(text, copied) {
  const pre = h('pre', { class: 'loglines diag-config', text });
  const status = h('p', { class: copied ? 'tone-ok' : 'tone-err', text: copied ? '已复制到剪贴板。' : '没能自动复制，请点击“复制”按钮或手动选中。' });
  const copy = btn('btn btn-primary', '复制', async () => {
    if (await copyText(text)) toast({ title: '已复制' });
    else toast({ tone: 'err', title: '没能复制', text: '请手动选中配置信息复制。' });
  });
  openDialog({ title: '配置信息', sub: '复制前请检查内容中是否包含不适合公开的信息', cls: 'dlg-update', body: h('div', { class: 'dlg-body' }, status, pre), actions: { ok: copy }, light: true });
}
