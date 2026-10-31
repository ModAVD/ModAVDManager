// Entry: theme, the optional mock backend (?mock=…), first load, the event stream and the top-level render.

import { h, icon, btn, setText, attr, hydrateIcons } from './dom.js';
import { S, update, subscribe, runningCount } from './store.js';
import { api, connectEvents } from './api.js';
import { initUI } from './ui.js';
import { initGrid, renderGrid, viewState, setView } from './cards.js';
import { openCreate, openSettings, openDelete } from './forms.js';
import { runCheck, checkSummary, openCheck, openActivity, renderBanner } from './panels.js';
import { openViewer, openLogs } from './viewer.js';
import * as A from './actions.js';

const params = new URLSearchParams(location.search);
const $ = (id) => document.getElementById(id);
const bootAt = Date.now();

const load = (k) => { try { return localStorage.getItem(k); } catch { return null; } }; // storage blocked: defaults
const save = (k, v) => { try { localStorage.setItem(k, v); } catch { /* storage blocked: not remembered */ } };

// A row of icon radios (roving tabindex, arrow keys move and pick). apply(v) returns the value it settled on.
function radioRow(el, apply) {
  const btns = [...el.children];
  const set = (v, remember) => {
    v = apply(v, remember);
    for (const b of btns) {
      b.setAttribute('aria-checked', b.dataset.v === v);
      b.tabIndex = b.dataset.v === v ? 0 : -1;
    }
  };
  for (const b of btns) b.addEventListener('click', () => set(b.dataset.v, true));
  el.addEventListener('keydown', (e) => {
    const d = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[e.key];
    if (!d) return;
    e.preventDefault();
    const next = btns[(btns.findIndex((b) => b.tabIndex === 0) + d + btns.length) % btns.length];
    set(next.dataset.v, true);
    next.focus();
  });
  return set;
}

// ---- theme: 系统 / 浅色 / 深色, remembered in this browser profile (?theme= for screenshots, not remembered)
hydrateIcons(document.body);
const setTheme = radioRow($('themeSeg'), (t, remember) => {
  if (t !== 'light' && t !== 'dark') t = 'system';
  if (t === 'system') delete document.documentElement.dataset.theme;
  else document.documentElement.dataset.theme = t;
  if (remember) save('modavd.theme', t);
  return t;
});
setTheme(params.get('theme') || load('modavd.theme'), false);

// ---- mock backend for design work; never shipped to users (the server does not serve /dev/)
if (params.get('mock')) {
  try {
    (await import('/dev/mock.js')).install(params.get('mock'), params);
  } catch (e) {
    console.error('mock backend unavailable', e);
  }
}

// ---- wiring
initUI();
initGrid($('grid'));
// ---- 卡片 / 列表 (?view= for screenshots, not remembered)
radioRow($('viewSeg'), (v, remember) => {
  v = v === 'list' ? 'list' : 'card';
  setView(v);
  if (remember) save('modavd.view', v);
  return v;
})(params.get('view') || load('modavd.view'), false);
$('rootChip').onclick = () => A.openFolder('root');
$('checkChip').onclick = openCheck;
$('actBtn').onclick = openActivity;
$('createBtn').onclick = $('emptyCreate').onclick = openCreate;

const failBox = $('failState');
const sigs = {};
const changed = (k, v) => (sigs[k] === v ? false : ((sigs[k] = v), true));
let opened = false;

// ---- render
function render() {
  const root = (S.info && S.info.root) || S.list;
  const list = S.list;
  if (root) {
    const ver = root.version;
    $('ver').hidden = !ver;
    setText($('ver'), ver);
    $('ver').title = `模拟器 ${ver || '版本未知'}${root.build ? `（mas-${root.build}）` : ''}${S.info ? ` · 管理器 ${S.info.version}` : ''}`;
    if (changed('root', root.root)) {
      setText($('rootPath'), root.root);
      $('rootChip').title = `在资源管理器里打开 ${root.root}`;
    }
  }

  const sum = checkSummary(S.check);
  const st = $('checkState');
  if (changed('check', S.checking && !S.check ? 'busy' : sum ? sum.text : S.checkError ? 'fail' : '')) {
    if (sum) { st.className = 'st tone-' + sum.tone; st.textContent = sum.text; }
    else if (S.checking) { st.className = 'st loading'; st.replaceChildren(h('span', { class: 'spin' }), '检查中'); }
    else if (S.checkError) { st.className = 'st tone-idle'; st.textContent = '没查成'; }
    $('checkChip').setAttribute('aria-label', '电脑检查：' + st.textContent);
  }

  const n = runningCount();
  $('actBadge').hidden = !n;
  setText($('actBadge'), n);
  $('actBtn').setAttribute('aria-label', n ? `活动，${n} 个任务进行中` : '活动');

  renderBanner($('banner'));

  const none = !!list && !list.instances.length;
  const failed = !list && S.loadError;
  if (list) {
    setText($('instCount'), `${list.instances.length} 台`);
    const c = { online: 0, starting: 0, stopping: 0, offline: 0, creating: 0, createfail: 0 };
    for (const inst of list.instances) c[viewState(inst).key.replace('error', 'offline')]++;
    const parts = [['ok', c.online, '运行中'], ['warn', c.starting, '开机中'], ['warn', c.creating, '新建中'], ['warn', c.stopping, '关机中'], ['err', c.createfail, '新建失败'], ['idle', c.offline, '已关机']].filter((p) => p[1]);
    if (changed('sum', parts.join('|'))) $('summary').replaceChildren(...parts.map(([tone, k, t]) => h('span', { class: 'sum-item tone-' + tone, text: `${k} ${t}` })));
    const full = list.instances.length >= 9;
    attr($('createBtn'), 'aria-disabled', full || null);
    $('createBtn').title = full ? '最多 9 台实例（编号 0–9，5 除外）' : '';
  }
  $('createBtn').hidden = none || !!failed;
  $('viewSeg').hidden = none || !!failed;
  $('summary').hidden = none;
  $('grid').hidden = !!failed || none;
  renderGrid();
  if (failed && failBox.err !== S.loadError) {
    failBox.err = S.loadError;
    failBox.replaceChildren(h('span', { class: 'banner-ic' }, icon('alert')), h('h2', { text: S.loadError.message }),
      S.loadError.advice && h('p', { text: S.loadError.advice }), btn('btn btn-secondary', [icon('refresh'), '重试'], A.loadInstances));
  }
  $('emptyState').hidden = !none;
  failBox.hidden = !failed;
  $('emptyBox').hidden = !none && !failed;

  // the stream is down: a quiet bar first; after 10 s say that what is shown may be old, dim it, offer a retry
  const lost = S.conn === 'lost' || S.conn === 'auth' || (S.conn === 'connecting' && Date.now() - bootAt > 4000);
  const since = S.lostAt || bootAt;
  const stale = lost && S.conn !== 'auth' && Date.now() - since > 10000;
  $('reconnect').hidden = !lost;
  $('reconnect').classList.toggle('auth', S.conn === 'auth');
  $('reconnect').classList.toggle('stale', stale);
  setText($('reconnectText'), S.conn === 'auth' ? '和后台的连接已失效，请关掉窗口后重新打开 ModAVD'
    : stale ? '后台没有回应，显示的可能不是最新状态' : '正在重新连接…');
  $('reconnectBtn').hidden = !stale;
  $('grid').classList.toggle('stale', stale && !!list);

  // ?open=create | check | activity | settings:<i> | delete:<i> | viewer:<i> | logs:<i>  (deep links, screenshots)
  if (!opened && list) {
    opened = true;
    const [k, a] = (params.get('open') || '').split(':');
    const i = Number(a);
    const fn = { create: openCreate, check: openCheck, activity: openActivity, settings: () => openSettings(i), delete: () => openDelete(i),
      viewer: () => openViewer(i), logs: () => openLogs(i, params.get('kind')) }[k];
    if (fn) setTimeout(fn, 30);
  }
}

subscribe(render);
update();
setTimeout(update, 4100);

// ---- data
const loadJobs = () => api.get('/api/jobs').then((r) => { for (const j of r.jobs || []) A.onJob(j); }).catch(() => {});
api.get('/api/info').then((info) => { S.info = info; update(); }).catch(() => {});
A.loadInstances();
loadJobs();
runCheck(false);

const probe = () => api.get('/api/info').then((info) => { S.info = info; }).catch((e) => { if (e.status === 401) { S.conn = 'auth'; update(); } });
let staleTimer = 0;
const events = connectEvents({
  open() {
    if (S.conn === 'lost' || S.conn === 'auth') loadJobs(); // job changes while the stream was down
    S.conn = 'open';
    S.lostAt = 0;
    clearTimeout(staleTimer);
    update();
  },
  lost(closed) {
    if (S.conn === 'auth') return;
    S.conn = 'lost';
    if (!S.lostAt) {
      S.lostAt = Date.now();
      clearTimeout(staleTimer);
      staleTimer = setTimeout(update, 10100);
    }
    update();
    if (closed) probe();
  },
  instances(d) {
    S.list = d;
    S.loadError = null;
    update();
  },
  job: A.onJob,
  log(l) {
    S.logs.push(l);
    if (S.logs.length > 600) S.logs.splice(0, 100);
    update();
  },
});

// 重新连接: ask the server once and open a new stream right away; the 10 s grace starts over
$('reconnectBtn').onclick = () => {
  if (S.conn === 'auth') return;
  S.lostAt = Date.now();
  clearTimeout(staleTimer);
  staleTimer = setTimeout(update, 10100);
  update();
  probe();
  events.reconnect();
};
