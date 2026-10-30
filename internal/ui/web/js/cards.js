// Instance cards: keyed by index, built once and patched in place (an update never loses the thumbnail).
// The same elements make both views: cards in a grid, or 56px rows in one surface (.grid.is-list, CSS only).

import { h, icon, btn, ibtn, keyed, setText, attr } from './dom.js';
import { S, lastJob, rootBlocked } from './store.js';
import { label, uptime, stripInst, pct, cfgText } from './format.js';
import * as A from './actions.js';
import { openMenu } from './ui.js';
import { openSettings, openDelete } from './forms.js';
import { openViewer, openLogs } from './viewer.js';
import { openActivity } from './panels.js';

const cards = new Map();
let grid;
let io;
let skeleton = null;
let view = 'card';
const $apk = () => document.getElementById('apkInput');
const BLOCKED = '这个目录现在还不能开机，先按上方提示处理';

export function initGrid(el) {
  grid = el;
  io = new IntersectionObserver((es) => {
    for (const e of es) if (e.target.card) e.target.card.visible = e.isIntersecting;
    tick();
  }, { rootMargin: '120px' });
  setInterval(tick, 1000);
  addEventListener('focus', tick);
  document.addEventListener('visibilitychange', tick);
  $apk().addEventListener('change', () => {
    const f = $apk().files[0];
    $apk().value = '';
    if (f) A.install(Number($apk().dataset.index), f);
  });
}

export const getView = () => view;

export function setView(v) {
  v = v === 'list' ? 'list' : 'card';
  if (v === view && grid.classList.contains('is-list') === (v === 'list')) return;
  view = v;
  grid.classList.toggle('is-list', v === 'list');
  if (skeleton) {
    for (const s of skeleton) s.remove();
    skeleton = null;
  }
  renderGrid();
  tick();
}

function pickApk(i) {
  $apk().dataset.index = String(i);
  $apk().click();
}

// The 开机 button while the root cannot boot: show where the reason is instead of starting a job that fails.
function goBanner() {
  const b = document.getElementById('banner');
  if (!b || b.hidden) return;
  b.scrollIntoView({ block: 'start', behavior: matchMedia('(prefers-reduced-motion: reduce)').matches ? 'auto' : 'smooth' });
  const f = document.getElementById('bannerCheck');
  if (f) f.focus({ preventScroll: true });
}

// What a card shows: the instance status refined by the job running on it.
export function viewState(inst) {
  const i = inst.index;
  const job = lastJob(i, true);
  const kind = (job && job.kind) || S.pending[i];
  let key = 'offline';
  if (kind === 'stop' || inst.busy === 'stopping') key = 'stopping';
  else if (kind === 'start' || kind === 'restart' || inst.status === 'starting' || inst.busy === 'starting') key = 'starting';
  else if (inst.status === 'online') key = 'online';
  let failed = null;
  if (!kind) {
    const lj = lastJob(i);
    if (lj && lj.state === 'failed' && !S.dismissed.has(lj.id)) failed = lj;
  }
  if (failed && key === 'offline' && (failed.kind === 'start' || failed.kind === 'restart')) key = 'error';
  return { key, job, kind, failed };
}

const PILL = { online: ['运行中', 'ok'], starting: ['开机中', 'warn'], stopping: ['关机中', 'warn'], offline: ['已关机', 'idle'], error: ['开机失败', 'err'] };
const FAIL = { start: '开机失败', restart: '重启失败', stop: '关机失败', install: '安装失败' };

function skeletonFor(v) {
  const sk = (cls) => h('div', { class: 'sk ' + cls });
  if (v === 'list') {
    return [0, 1, 2].map(() => h('div', { class: 'card skel', 'aria-hidden': 'true' },
      sk('sk-pill'), h('div', { class: 'sk-titles' }, sk('sk-title'), sk('sk-sub')), sk('sk-cfg'), sk('sk-addr'), sk('sk-btn')));
  }
  return [0, 1, 2].map(() => h('div', { class: 'card skel', 'aria-hidden': 'true' }, h('div', { class: 'screen sk' }),
    h('div', { class: 'card-body' }, sk('sk-title'), sk('sk-sub'), sk('sk-adb')), h('div', { class: 'card-actions' }, sk('sk-btn'))));
}

export function renderGrid() {
  if (!S.list) {
    if (!skeleton) {
      skeleton = skeletonFor(view);
      grid.replaceChildren(...skeleton);
      grid.setAttribute('aria-busy', 'true');
    }
    return;
  }
  if (skeleton) {
    for (const s of skeleton) s.remove();
    skeleton = null;
    grid.removeAttribute('aria-busy');
  }
  keyed(grid, cards, [...S.list.instances].sort((a, b) => a.index - b.index), (x) => x.index, (x) => makeCard(x.index), updateCard);
}

function makeCard(i) {
  const c = { i, url: null, next: 0, fail: 0, visible: true };
  const view = () => { if (c.inst.status === 'online') openViewer(i); };
  const menu = () => openMenu(c.more, menuItems(c));
  c.img = h('img', { class: 'shot', alt: '', draggable: 'false' });
  c.phText = h('span', { class: 'ph-text' });
  c.progMsg = h('span', { class: 'prog-msg' });
  c.progFill = h('i');
  c.progBar = h('div', { class: 'bar' }, c.progFill);
  // no live region here: a boot reports every 2.5 s; the toast at the end says how it went
  c.peek = btn('peek', h('span', { class: 'peek-chip' }, icon('eye'), '打开画面'), view);
  c.screen = h('div', { class: 'screen' },
    h('div', { class: 'ph' }, h('span', { class: 'ph-tile' }, h('span', { class: 'ph-num', text: String(i) })), c.phText),
    c.img,
    h('div', { class: 'prog' }, h('div', { class: 'prog-row' }, h('span', { class: 'spin' }), c.progMsg), c.progBar),
    c.peek);
  c.title = h('h2', { class: 'card-title', id: `card-t-${i}` });
  c.sub = h('div', { class: 'card-sub' });
  c.lmsg = h('div', { class: 'card-sub card-lmsg' });
  c.cfg = h('div', { class: 'card-cfg', title: '内存 · CPU 核数 · 帧率 · 数据盘上限（按实际写入增长）' });
  c.pillText = h('span');
  c.pill = h('span', { class: 'pill' }, c.pillText);
  c.adb = h('span', { class: 'adb-addr' });
  c.copy = ibtn('copy', '复制 adb 地址', () => A.copy(c.inst.adb_address));
  c.notes = h('div', { class: 'notes' });
  c.primary = btn('btn btn-power', null, () => {
    const m = c.primary.dataset.mode;
    if (m === 'blocked') goBanner();
    else if (m === 'start') A.start(i);
    else if (m === 'stop') A.stop(i);
  });
  c.view = btn('btn btn-ghost act-view', [icon('screen'), '画面'], view);
  c.more = btn('icon-btn act-more', icon('more'), menu, {
    'aria-haspopup': 'menu', 'aria-expanded': 'false',
    onkeydown: (e) => { if (e.key === 'ArrowDown') { e.preventDefault(); menu(); } },
  });
  // list view only: the boot progress as a hairline along the row's bottom edge
  c.lfill = h('i');
  c.lbar = h('div', { class: 'bar card-lbar', 'aria-hidden': 'true' }, c.lfill);
  c.el = h('article', { class: 'card', 'aria-labelledby': `card-t-${i}` }, c.screen,
    h('div', { class: 'card-body' },
      h('div', { class: 'card-head' }, h('div', { class: 'card-titles' }, c.title, c.sub, c.lmsg), c.pill),
      c.cfg,
      h('div', { class: 'adb' }, h('span', { class: 'adb-label', text: 'ADB' }), c.adb, c.copy),
      c.notes),
    h('div', { class: 'card-actions' }, c.primary, c.view, h('span', { class: 'spacer' }), c.more),
    c.lbar);
  c.el.card = c;
  c.gone = () => { io.unobserve(c.el); dropShot(c); };
  io.observe(c.el);
  return c;
}

function updateCard(c, inst) {
  c.inst = inst;
  const v = viewState(inst);
  const live = inst.status === 'online';
  const name = label(inst);
  c.el.dataset.status = v.key;
  setText(c.title, (c.title.title = name));
  const sub = [];
  if (inst.title && inst.title !== inst.name) sub.push(inst.name);
  if (live && inst.started_at) sub.push(uptime(inst.started_at));
  setText(c.sub, sub.join(' · ') || `编号 ${inst.index}`);
  setText(c.cfg, cfgText(inst.config));

  let [pt, tone] = PILL[v.key];
  if (v.key === 'starting' && v.kind === 'restart') pt = '重启中';
  if (v.key === 'error') pt = FAIL[v.failed.kind];
  setText(c.pillText, pt);
  attr(c.pill, 'class', 'pill tone-' + tone);

  setText(c.adb, inst.adb_address);
  attr(c.copy, 'aria-label', `复制 ${inst.adb_address}`);
  c.copy.title = '复制 adb 地址';
  attr(c.more, 'aria-label', `更多操作：${name}`);
  attr(c.view, 'aria-label', `打开「${name}」的画面`);
  attr(c.peek, 'aria-label', `打开「${name}」的画面`);
  renderNotes(c, inst, v);

  if (!live) {
    dropShot(c);
    c.fail = 0;
    c.next = 0;
    c.hold = null;
  }
  const busy = v.key === 'starting' || v.key === 'stopping' || v.kind === 'install';
  c.el.classList.toggle('is-busy', busy);
  c.screen.classList.toggle('has-shot', live && !!c.url);
  c.screen.classList.toggle('busy', busy);
  setText(c.phText, v.key === 'error' ? '上次开机没有成功' : live ? '正在取画面…' : '未开机');
  if (busy) {
    const p = v.job && v.job.progress;
    const msg = (p && stripInst(p.message)) || (v.key === 'stopping' ? '正在关机…' : v.kind === 'install' ? '正在安装 APK…' : '正在开机…');
    setText(c.progMsg, msg);
    setText(c.lmsg, msg);
    const n = pct(p);
    for (const [bar, fill] of [[c.progBar, c.progFill], [c.lbar, c.lfill]]) {
      bar.classList.toggle('indet', n == null);
      fill.style.width = n == null ? '' : n.toFixed(1) + '%';
    }
  }
  attr(c.view, 'aria-disabled', !live);
  c.view.title = live ? '实时画面，可以点按' : '开机后才能看画面';

  const blocked = rootBlocked();
  const mode = v.key === 'starting' ? ['busy', pt] : v.key === 'stopping' ? ['busy', '关机中'] : inst.running ? ['stop', '关机']
    : blocked ? ['blocked', '开机'] : ['start', '开机'];
  const sig = mode.join();
  if (sig !== c.psig) {
    c.psig = sig;
    const b = c.primary;
    b.replaceChildren(mode[0] === 'busy' ? h('span', { class: 'spin' }) : icon('power'), h('span', { text: mode[1] }));
    b.className = 'btn btn-power act-power ' + (mode[0] === 'start' ? 'btn-tonal' : 'btn-secondary');
    b.dataset.mode = mode[0];
    b.title = mode[0] === 'blocked' ? BLOCKED : '';
    attr(b, 'aria-busy', mode[0] === 'busy');
    attr(b, 'aria-disabled', mode[0] === 'busy' || mode[0] === 'blocked');
  }
  if (live && !c.url) tick();
}

function renderNotes(c, inst, v) {
  const notes = [];
  const f = v.failed;
  if (f) {
    const e = f.error || {};
    // 内存不够 is the one failure a setting fixes: offer the setting right there
    const mem = e.code === 'precheck_failed' && /内存/.test(e.message || '');
    notes.push({ tone: 'err', ic: 'bad', text: `${FAIL[f.kind] || '失败'}：${e.message || '原因未知'}`, advice: e.advice, job: f.id, mem });
  }
  if (inst.software_renderer) notes.push({ tone: 'warn', ic: 'alert', text: '软件渲染：显卡驱动不可用，游戏会卡' });
  if (!inst.initialized && !inst.running && v.key !== 'starting') notes.push({ tone: 'info', ic: 'info', text: '首次开机会做初始化，约 1⁠–⁠2 分钟' });
  const sig = JSON.stringify(notes);
  if (sig === c.nsig) return;
  c.nsig = sig;
  c.notes.replaceChildren(...notes.map((n) => {
    const full = n.advice ? `${n.text}\n${n.advice}` : n.text;
    // message and advice clamp separately, so a long message never pushes the advice out of sight
    return h('div', { class: 'note tone-' + n.tone, title: full }, icon(n.ic),
      h('div', { class: 'note-main' },
        h('p', { class: 'note-text', text: n.text }),
        n.advice && h('p', { class: 'note-text note-adv', text: n.advice }),
        n.job && h('div', { class: 'note-links' },
          n.mem && btn('link-btn', '调小内存', () => openSettings(c.i, 'memory')),
          btn('link-btn', '详情', openActivity))),
      n.job && ibtn('x', '不再显示这条', () => { S.dismissed.add(n.job); renderGrid(); }));
  }));
}

function menuItems(c) {
  const inst = c.inst;
  const i = c.i;
  const v = viewState(inst);
  const busy = v.key === 'starting' || v.key === 'stopping';
  const running = inst.running || v.key === 'starting';
  const vis = !!S.win[i];
  const blocked = rootBlocked();
  return [
    { label: '带窗口开机', icon: 'window', disabled: running || busy || blocked, hint: blocked && !running ? '目录未就绪' : '', run: () => A.start(i, true) },
    { label: '重启', icon: 'refresh', disabled: !inst.running || busy || blocked, hint: blocked && inst.running ? '目录未就绪' : '', run: () => A.restart(i) },
    v.key === 'starting' && { label: '关机', icon: 'power', run: () => A.stop(i) },
    { label: '强制关机', icon: 'bolt', disabled: !running || v.key === 'stopping', run: () => A.stop(i, true) },
    { sep: true },
    { label: vis ? '隐藏窗口' : '显示窗口', icon: vis ? 'eyeOff' : 'window', disabled: !inst.running, run: () => A.setWindow(i, !vis) },
    { label: '安装 APK…', icon: 'download', disabled: inst.status !== 'online' || v.kind === 'install', run: () => pickApk(i) },
    { label: '查看日志', icon: 'file', run: () => openLogs(i) },
    { label: '打开目录', icon: 'folder', run: () => A.openFolder('instance', i) },
    { sep: true },
    { label: '设置…', icon: 'sliders', run: () => openSettings(i) },
    { label: '删除…', icon: 'trash', danger: true, disabled: running || busy, hint: running || busy ? '先关机' : '', run: () => openDelete(i) },
  ];
}

// ---- thumbnails: about every 5 s, only while the card view is on screen, the window has focus and no dialog covers
// the grid (the first frame too). Every one is an `adb exec-out screencap`, so a failing card backs off up to 30 s and
// a 409 (not booted after all) waits for the next instances event.
function tick() {
  if (view !== 'card') return;
  if (document.visibilityState !== 'visible' || !document.hasFocus() || document.querySelector('dialog[open]')) return;
  const now = Date.now();
  for (const c of cards.values()) {
    if (!c.inst || c.inst.status !== 'online' || c.inflight || !c.visible) continue;
    if (c.hold && c.hold === S.list) continue;
    if (now >= c.next) shoot(c);
  }
}

async function shoot(c) {
  c.inflight = true;
  c.hold = null;
  let url = null;
  let ok = false;
  try {
    const res = await fetch(`/api/instances/${c.i}/screenshot`, { credentials: 'same-origin', cache: 'no-store', signal: AbortSignal.timeout(8000) });
    if (res.status === 409) {
      c.hold = S.list;
      ok = true;
      return;
    }
    if (!res.ok) return;
    url = URL.createObjectURL(await res.blob());
    // decode off screen first: the swap never shows a blank frame, and a broken image counts as a failure
    const im = new Image();
    im.src = url;
    await im.decode();
    ok = true;
    if (c.inst.status !== 'online' || !c.el.isConnected) return;
    const old = c.url;
    c.img.src = c.url = url;
    url = null;
    if (old) URL.revokeObjectURL(old);
    c.screen.classList.add('has-shot');
  } catch {
    // timeout, network or decode error: back off below
  } finally {
    if (url) URL.revokeObjectURL(url);
    c.inflight = false;
    if (ok) {
      c.fail = 0;
      c.next = Date.now() + 5000;
    } else {
      c.fail++;
      c.next = Date.now() + Math.min(30000, 2000 * 2 ** c.fail);
    }
  }
}

function dropShot(c) {
  if (!c.url) return;
  URL.revokeObjectURL(c.url);
  c.url = null;
  c.img.removeAttribute('src');
  c.screen.classList.remove('has-shot');
}
