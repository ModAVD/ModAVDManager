// 画面 viewer (about 2 frames a second, click = tap) and the log viewer.

import { h, icon, btn, setText, attr } from './dom.js';
import { api } from './api.js';
import { S, subscribe, instance, rootBlocked } from './store.js';
import { openDialog } from './ui.js';
import { label } from './format.js';
import * as A from './actions.js';

const sleep = (ms) => new Promise((r) => setTimeout(r, ms));

export function openViewer(i) {
  const inst = instance(i);
  if (!inst) return;
  const img = h('img', { class: 'vshot', alt: `${label(inst)} 的当前画面`, draggable: 'false' });
  // not live: the card's placeholder (number tile on the dot pattern), what is going on, and 开机 when it is off
  const phMsg = h('span');
  const phLead = h('span', { class: 'vph-lead' });
  const phSub = h('p', { class: 'vph-sub' });
  const startBtn = btn('btn btn-tonal', [icon('power'), '开机'], () => { if (startBtn.getAttribute('aria-disabled') !== 'true') A.start(i); });
  const ph = h('div', { class: 'ph vph' }, h('span', { class: 'ph-tile' }, h('span', { class: 'ph-num', text: String(i) })),
    h('p', { class: 'vph-text' }, phLead, phMsg), phSub, startBtn);
  const frame = h('div', { class: 'viewer-frame off', role: 'group', 'aria-label': `「${label(inst)}」的画面` }, img, ph);
  const dot = h('span', { class: 'live-dot off' });
  const status = h('span');
  const keys = [['back', '返回'], ['home', '主页'], ['recents', '最近任务']].map(([k, t]) =>
    btn('btn btn-secondary btn-sm', [icon(k), t], () => { if (!frame.classList.contains('off')) A.input(i, { type: 'key', key: k }); }));
  const ctl = openDialog({
    title: `「${label(inst)}」的画面`, sub: '点按画面即在模拟器上点按 · Esc 关闭', cls: 'dlg-viewer', light: true,
    tools: h('div', { class: 'viewer-tools' }, h('span', { class: 'viewer-status' }, dot, status), h('div', { class: 'navkeys' }, keys)),
    body: h('div', { class: 'dlg-body' }, frame),
  });
  frame.setAttribute('aria-describedby', ctl.subEl.id);

  let alive = true;
  let url = null;
  let state = '';
  // s: live | wait | off | boot | error
  const setState = (s, text = '', sub = '') => {
    const blocked = s === 'off' && rootBlocked();
    if (state === s + text + sub + blocked) return;
    state = s + text + sub + blocked;
    const live = s === 'live';
    frame.classList.toggle('off', !live);
    ph.hidden = live;
    img.hidden = !live;
    dot.classList.toggle('off', !live);
    setText(status, live ? '实时 · 约 2 帧/秒' : s === 'wait' ? '正在取画面…' : '没有画面');
    for (const b of keys) attr(b, 'aria-disabled', !live || null);
    phLead.replaceChildren(s === 'wait' || s === 'boot' ? h('span', { class: 'spin' }) : s === 'error' ? icon('alert') : '');
    setText(phMsg, text);
    setText(phSub, blocked ? '这个目录现在还不能开机，先按主页上方的提示处理' : sub);
    phSub.hidden = !phSub.textContent;
    startBtn.hidden = s !== 'off' || text !== OFF;
    attr(startBtn, 'aria-disabled', blocked || null);
  };
  const OFF = '实例没有开机';
  setState('wait', '正在取画面…');

  (async () => {
    let fail = 0;
    while (alive) {
      const t0 = performance.now();
      let wait = 500;
      const cur = instance(i);
      if (!cur) { setState('off', '这台实例已经不在了'); break; }
      if (cur.status === 'starting' || S.pending[i] === 'start') setState('boot', '正在开机，开好后这里会显示画面');
      else if (cur.status !== 'online') setState('off', OFF, '开机后这里会显示实时画面');
      else if (document.visibilityState === 'visible') {
        let u = null;
        try {
          const res = await api.get(`/api/instances/${i}/screenshot`, { raw: true, signal: AbortSignal.timeout(8000) });
          u = URL.createObjectURL(await res.blob());
          const im = new Image();
          im.src = u;
          await im.decode();
          fail = 0;
          if (alive) {
            img.src = u;
            if (url) URL.revokeObjectURL(url);
            url = u;
            u = null;
            setState('live');
          }
        } catch (e) {
          if (e.status === 409) setState('off', OFF, '开机后这里会显示实时画面');
          else {
            fail++;
            if (e.name === 'TimeoutError') setState('error', '取画面超时', '模拟器可能卡住了，过一会儿会再试');
            else setState('error', e.message || '取不到画面', e.advice);
          }
        }
        if (u) URL.revokeObjectURL(u);
        // every frame is an adb screencap: slower while the window is in the background, back off while it fails
        if (!document.hasFocus()) wait = 2000;
        if (fail) wait = Math.min(10000, 1000 * 2 ** fail);
      }
      await sleep(Math.max(150, wait - (performance.now() - t0)));
    }
  })();

  frame.addEventListener('click', (e) => {
    if (frame.classList.contains('off')) return;
    const r = frame.getBoundingClientRect();
    const cfg = (instance(i) || inst).config || {};
    const fx = (e.clientX - r.left) / r.width;
    const fy = (e.clientY - r.top) / r.height;
    if (fx < 0 || fx > 1 || fy < 0 || fy > 1) return;
    const rip = h('span', { class: 'ripple' });
    rip.style.left = fx * 100 + '%';
    rip.style.top = fy * 100 + '%';
    frame.append(rip);
    setTimeout(() => rip.remove(), 600);
    // the API takes pixels of the 1280×720 display
    A.input(i, { type: 'tap', x: Math.round(fx * ((cfg.width || 1280) - 1)), y: Math.round(fy * ((cfg.height || 720) - 1)) });
  });
  const un = subscribe(() => {
    const cur = instance(i);
    if (!cur) return;
    setText(ctl.titleEl, `「${label(cur)}」的画面`);
    attr(frame, 'aria-label', `「${label(cur)}」的画面`);
    attr(img, 'alt', `${label(cur)} 的当前画面`);
  });
  ctl.onClose.push(() => { alive = false; un(); if (url) URL.revokeObjectURL(url); });
}

// ---- logs
const LOGCAT_LVL = /^\S+\s+\S+\s+\d+\s+\d+\s+([VDIWEF])\s/;

function lineClass(kind, line) {
  if (kind === 'logcat') {
    const m = LOGCAT_LVL.exec(line);
    return m ? { E: 'error', F: 'error', W: 'warn', V: 'dim', D: 'dim' }[m[1]] || '' : '';
  }
  return /^(ERROR|FATAL|PANIC)\b/.test(line) ? 'error' : /^WARNING\b/.test(line) ? 'warn' : '';
}

export function openLogs(i, kind0) {
  const inst = instance(i);
  if (!inst) return;
  let kind = kind0 === 'logcat' ? 'logcat' : 'emulator';
  let lines = 300;
  let follow = true;
  let text = null;
  let seq = 0;

  const tabs = [['emulator', '模拟器'], ['logcat', 'logcat']].map(([k, t]) =>
    btn('tab', t, () => setKind(k), { role: 'tab', id: `lt-${k}`, 'aria-controls': 'logview' }));
  const tablist = h('div', { class: 'tabs', role: 'tablist', 'aria-label': '日志种类' }, tabs);
  tablist.addEventListener('keydown', (e) => {
    if (e.key !== 'ArrowLeft' && e.key !== 'ArrowRight') return;
    e.preventDefault();
    setKind(kind === 'emulator' ? 'logcat' : 'emulator');
    tabs[kind === 'emulator' ? 0 : 1].focus();
  });
  const sel = h('select', { class: 'select', 'aria-label': '显示的行数' }, [300, 1000, 3000].map((n) => h('option', { value: String(n), text: `最近 ${n} 行` })));
  sel.addEventListener('change', () => { lines = Number(sel.value); load(true); });
  const chk = h('input', { type: 'checkbox', checked: true });
  chk.addEventListener('change', () => { follow = chk.checked; if (follow) load(true); });
  const view = h('pre', { class: 'logview', id: 'logview', role: 'tabpanel', tabindex: '0' });
  // scrolling up to read stops 实时跟随; scrolling back to the end turns it on again
  view.addEventListener('scroll', () => {
    if (text == null) return;
    const atEnd = view.scrollHeight - view.scrollTop - view.clientHeight <= 32;
    if (atEnd !== follow) follow = chk.checked = atEnd; // the next 3 s tick reloads
  }, { passive: true });
  // a selection inside the log survives: no refresh while there is one
  const selecting = () => { const s = getSelection(); return !!s && !s.isCollapsed && view.contains(s.anchorNode); };
  const ctl = openDialog({
    title: `「${label(inst)}」的日志`, sub: `${inst.name} · ${inst.serial}`, cls: 'dlg-logs', light: true,
    body: h('div', { class: 'dlg-body' },
      h('div', { class: 'logs-bar' }, tablist, h('span', { class: 'spacer' }), sel,
        h('label', { class: 'checkbox' }, chk, h('span', { text: '实时跟随' })), btn('btn btn-secondary', [icon('refresh'), '刷新'], () => load(true))),
      view),
    foot: h('footer', { class: 'dlg-foot' }, h('span', { class: 'foot-note', text: '最新那份日志的末尾。完整日志在日志目录里。' }),
      btn('btn btn-ghost', [icon('folder'), '打开日志目录'], () => A.openFolder('logs'))),
  });

  function setKind(k) {
    kind = k;
    for (const t of tabs) {
      attr(t, 'aria-selected', t.id === `lt-${k}`);
      t.tabIndex = t.id === `lt-${k}` ? 0 : -1;
    }
    attr(view, 'aria-labelledby', `lt-${k}`);
    text = null;
    load(true);
  }

  function showState(ic, msg, sub) {
    text = null;
    view.replaceChildren(h('div', { class: 'log-state' }, ic === 'spin' ? h('span', { class: 'spin' }) : icon(ic), h('div', { text: msg }), sub && h('div', { class: 'muted', text: sub })));
  }

  async function load(manual) {
    const my = ++seq;
    // Keep a selected log line intact.  The next interval (or a manual refresh after
    // the selection is cleared) will fetch the newest tail without replacing it mid-read.
    if (text != null && selecting()) return;
    if (manual && text == null) showState('spin', '正在读取日志…');
    try {
      const t = await api.get(`/api/instances/${i}/log?kind=${kind}&lines=${lines}`);
      if (my !== seq || t === text) return;
      const atEnd = view.scrollHeight - view.scrollTop - view.clientHeight < 32;
      if (!t.trim()) { showState('file', '日志是空的'); return; }
      text = t;
      view.replaceChildren(...t.replace(/\n$/, '').split('\n').map((l) => h('span', { class: 'll ' + lineClass(kind, l), text: l })));
      if (follow || atEnd || manual) view.scrollTop = view.scrollHeight;
    } catch (e) {
      if (my !== seq) return;
      if (e.status === 404) showState('file', kind === 'logcat' ? '还没有 logcat 日志' : '还没有模拟器日志', '实例开过一次机才会有日志。');
      else showState('alert', e.message, e.advice);
    }
  }

  setKind(kind);
  const timer = setInterval(() => { if (follow && document.visibilityState === 'visible' && !selecting()) load(); }, 3000);
  ctl.onClose.push(() => { clearInterval(timer); seq++; });
}
