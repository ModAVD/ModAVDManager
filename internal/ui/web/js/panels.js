// 电脑检查 panel, 活动 drawer and the not-ready banner.

import { h, icon, btn, keyed, showError, setText, attr } from './dom.js';
import { api } from './api.js';
import { S, update, subscribe, instance, jobsSorted, runningCount } from './store.js';
import { openDialog } from './ui.js';
import { KIND, KIND_ICON, STATE, STATE_TONE, clock, dur, label, parseTime, pct, stripInst, stopMethod } from './format.js';
import * as A from './actions.js';

const advice = (text) => h('div', { class: 'advice' }, icon('bulb'), h('span', { text }));
const folderBtn = (cls) => btn(cls, [icon('folder'), '打开目录'], () => A.openFolder('root'));

// ---- check
export async function runCheck(refresh) {
  if (S.checking) return;
  S.checking = true;
  S.checkError = null;
  update();
  try {
    S.check = await api.get('/api/check' + (refresh ? '?refresh=1' : ''));
    S.checkedAt = Date.now();
  } catch (e) {
    S.checkError = e;
  }
  S.checking = false;
  update();
}

export function checkSummary(c) {
  if (!c) return null;
  const items = c.items || [];
  const blockers = items.filter((x) => x.blocking && x.ok === false);
  const hints = items.filter((x) => (x.ok === false && !x.blocking) || x.ok == null);
  const tone = !c.ready || blockers.length ? 'err' : hints.length ? 'warn' : 'ok';
  return { tone, text: { err: '不能开机', warn: '有提示', ok: '就绪' }[tone], blockers, hints, n: items.length };
}

// ok / not ok / not checked; a failed item that does not block the boot is only a hint
function itemView(it) {
  if (it.ok === false && !it.blocking) return ['warn', 'alert', '提示'];
  return it.ok ? ['ok', 'ok', '通过'] : it.ok === false ? ['err', 'bad', '未通过'] : ['idle', 'unknown', '未查'];
}

// what needs doing first: blocking failures, then hints, then not checked, then passed (server order within each)
const RANK = (it) => (it.ok === false ? (it.blocking ? 0 : 1) : it.ok == null ? 2 : 3);

function checkRow() {
  const r = { ic: h('span', { class: 'ci-ic' }), title: h('span'), tag: h('span', { class: 'tag', text: '不影响开机' }), reason: h('div', { class: 'ci-reason' }), adv: h('div'), state: h('span', { class: 'ci-state' }) };
  r.el = h('li', { class: 'check-item' }, r.ic, h('div', null, h('div', { class: 'ci-title' }, r.title, r.tag), r.reason, r.adv), r.state);
  return r;
}

function fillCheckRow(r, it) {
  const [tone, ic, word] = itemView(it);
  r.el.className = 'check-item tone-' + tone;
  if (r.icn !== ic) r.ic.replaceChildren(icon((r.icn = ic)));
  setText(r.title, it.title);
  r.tag.hidden = it.blocking;
  setText(r.reason, it.reason);
  const a = it.ok !== true && it.advice ? it.advice : '';
  if (r.a !== a) r.adv.replaceChildren((r.a = a) ? advice(a) : '');
  setText(r.state, word);
}

let checkCtl = null;

export function openCheck() {
  if (checkCtl) return;
  const sum = h('div', { class: 'check-sum' });
  const box = h('div', { class: 'form-error', role: 'alert', hidden: true });
  const list = h('ul', { class: 'check-list' });
  // when something did not pass, the passed items fold away under one line
  const passList = h('ul', { class: 'check-list' });
  const passText = h('span');
  const passed = h('details', { class: 'passed tone-ok', hidden: true },
    h('summary', null, icon('ok'), passText, h('span', { class: 'spacer' }), icon('chevron')), passList);
  const comps = h('details', { class: 'comps', hidden: true });
  const when = h('span', { class: 'foot-note' });
  const again = btn('btn btn-secondary', null, () => runCheck(true));
  const ctl = openDialog({
    title: '电脑检查', sub: '开机前会自动检查这些项目，没通过的项目会说明原因和处理办法。', cls: 'dlg-check', light: true,
    body: h('div', { class: 'dlg-body' }, sum, box, list, passed, comps),
    foot: h('footer', { class: 'dlg-foot' }, when, folderBtn('btn btn-ghost'), again),
  });
  checkCtl = ctl;
  const rows = new Map();
  const passRows = new Map();
  let sigs = [];

  const render = () => {
    const c = S.check;
    if (sigs[0] !== S.checking) {
      sigs[0] = S.checking;
      again.replaceChildren(S.checking ? h('span', { class: 'spin' }) : icon('refresh'), S.checking ? '正在检查…' : '重新检查');
      attr(again, 'aria-busy', S.checking || null);
      attr(again, 'aria-disabled', S.checking || null);
    }
    setText(when, S.checking ? '要几秒：硬件加速检查和 Vulkan 探测' : S.checkedAt ? `检查于 ${clock(S.checkedAt)}` : '');
    if (S.checkError && !c) showError(box, S.checkError);
    box.hidden = !S.checkError;
    if (!c) {
      if (S.checking && !list.firstChild) {
        sum.replaceChildren(h('div', { class: 'sk sk-row' }));
        list.replaceChildren(...[1, 2, 3, 4, 5, 6, 7].map(() => h('li', { class: 'sk sk-row' })));
        sum.classList.add('skel');
        list.classList.add('skel');
      }
      return;
    }
    if (list.classList.contains('skel')) {
      list.replaceChildren();
      list.classList.remove('skel');
      sum.classList.remove('skel');
    }
    const s = checkSummary(c);
    const [head, line] = {
      err: ['暂时不能开机', `${s.blockers.length} 项必需检查没通过，按下面的建议处理后再检查一次。`],
      warn: ['可以开机，有提示', `${s.hints.length} 项需要留意，不影响开机。`],
      ok: ['这台电脑可以开机', `${s.n} 项检查都通过了。`],
    }[s.tone];
    sum.className = 'check-sum tone-' + s.tone;
    sum.replaceChildren(icon({ ok: 'ok', warn: 'alert', err: 'bad' }[s.tone]), h('div', null, h('strong', { text: head }), h('span', { text: line })));

    const items = [...(c.items || [])].sort((a, b) => RANK(a) - RANK(b));
    const good = items.filter((x) => x.ok === true);
    const fold = good.length < items.length && good.length > 0;
    keyed(list, rows, fold ? items.filter((x) => x.ok !== true) : items, (it) => it.id, checkRow, fillCheckRow);
    keyed(passList, passRows, fold ? good : [], (it) => it.id, checkRow, fillCheckRow);
    passed.hidden = !fold;
    setText(passText, `其余 ${good.length} 项已通过`);

    const cs = c.components || [];
    comps.hidden = !cs.length;
    const sig = JSON.stringify(cs);
    if (sig !== sigs[1]) {
      sigs[1] = sig;
      if (cs.some((x) => !x.installed && !x.optional)) comps.open = true;
      comps.replaceChildren(
        h('summary', null, '组件', h('span', { class: 'muted', text: `${cs.filter((x) => x.installed).length} / ${cs.length} 已安装` }), h('span', { class: 'spacer' }), icon('chevron')),
        ...cs.map((x) => h('div', { class: 'comp-row' },
          h('span', null, x.name, x.optional && h('span', { class: 'muted', text: '（可选）' })),
          h('span', { class: 'v', text: [x.version, x.build].filter(Boolean).join(' · ') }),
          h('span', { class: 'pill tone-' + (x.installed ? 'ok' : x.optional ? 'idle' : 'err'), text: x.installed ? '已安装' : '缺少' }))));
    }
  };
  const un = subscribe(render);
  render();
  ctl.onClose.push(() => { un(); checkCtl = null; });
  if (!S.check && !S.checking) runCheck(false);
}

// ---- banner: the root cannot boot (a blocking check failed or components are missing)
export function renderBanner(el) {
  const c = S.check;
  const sig = c && !c.ready ? JSON.stringify([c.items, c.components]) : '';
  if (el.sig === sig) return;
  el.sig = sig;
  el.hidden = !sig;
  if (!sig) { el.replaceChildren(); return; }
  const blockers = c.items.filter((x) => x.blocking && x.ok === false);
  const first = blockers[0];
  const missing = (c.components || []).filter((x) => !x.installed && !x.optional);
  el.replaceChildren(h('div', { class: 'banner', role: 'status' },
    h('span', { class: 'banner-ic' }, icon('alert')),
    h('div', { class: 'banner-main' },
      h('h2', { text: '这个目录现在还不能开机' }),
      first && h('p', { text: first.reason }),
      first && first.advice && advice(first.advice),
      missing.length && !(first && first.id === 'components') && h('div', { class: 'banner-missing' },
        h('span', { class: 'muted', text: '缺少的组件：' }), missing.map((m) => h('span', { class: 'chip-s', text: m.name }))),
      blockers.length > 1 && h('p', { class: 'banner-more', text: `另外还有 ${blockers.length - 1} 项必需检查没通过，详情见电脑检查。` }),
      h('div', { class: 'banner-actions' }, btn('btn btn-secondary', [icon('shield'), '查看电脑检查'], openCheck, { id: 'bannerCheck' }), folderBtn('btn btn-ghost')))));
}

// ---- activity
let actCtl = null;

export function openActivity() {
  if (actCtl) return;
  const list = h('ul', { class: 'jobs' });
  const empty = h('div', { class: 'act-empty' }, icon('pulse'), h('p', { text: '还没有任务' }),
    h('p', { class: 'muted', text: '开机、关机、重启和安装 APK 都会显示在这里。' }));
  const logBox = h('div', { class: 'loglines', role: 'log', 'aria-label': '最近的消息' });
  const ctl = openDialog({
    title: '活动', sub: ' ', cls: 'drawer', light: true,
    body: h('div', { class: 'dlg-body' }, list, empty, h('h3', { class: 'sec-title', text: '消息' }), logBox),
  });
  actCtl = ctl;
  const rows = new Map();
  let shown = 0;
  let first = null;

  const render = () => {
    const n = runningCount();
    setText(ctl.subEl, n ? `${n} 个任务进行中` : '没有进行中的任务');
    const jobs = jobsSorted();
    empty.hidden = jobs.length > 0;
    keyed(list, rows, jobs, (j) => j.id, jobRow, updateJob);
    // messages: append what is new; rebuild when the buffer was trimmed at the front
    const logs = S.logs;
    if (logs[0] !== first || logs.length < shown) { logBox.replaceChildren(); shown = 0; first = logs[0]; }
    if (logs.length !== shown) {
      const atEnd = logBox.scrollHeight - logBox.scrollTop - logBox.clientHeight < 24;
      logBox.append(...logs.slice(Math.max(shown, logs.length - 200)).map(logLine));
      while (logBox.childNodes.length > 200) logBox.firstChild.remove();
      shown = logs.length;
      if (atEnd) logBox.scrollTop = logBox.scrollHeight;
    }
    for (const r of rows.values()) fillJobLog(r);
  };
  const un = subscribe(render);
  render();
  logBox.scrollTop = logBox.scrollHeight;
  ctl.onClose.push(() => { un(); actCtl = null; });
}

const logLine = (l) => h('span', { class: 'll ' + (l.level === 'error' || l.level === 'warn' ? l.level : '') },
  h('span', { class: 'll-t', text: clock(l.time) }), l.msg);

function jobRow(j) {
  const r = { id: j.id };
  r.title = h('div', { class: 'job-title' });
  r.meta = h('div', { class: 'job-meta' });
  r.why = h('div', { class: 'job-why', hidden: true });
  r.pill = h('span', { class: 'pill' });
  r.msg = h('span');
  r.pctEl = h('span');
  r.fill = h('i');
  r.bar = h('div', { class: 'bar' }, r.fill);
  r.prog = h('div', { class: 'job-prog' }, h('div', { class: 'job-msg' }, r.msg, r.pctEl), r.bar);
  r.err = h('div', { class: 'job-err' });
  r.warn = h('ul', { class: 'job-warn' });
  r.res = h('div', { class: 'job-res' });
  r.acts = h('div', { class: 'job-acts' }, btn('btn btn-secondary btn-sm', '取消等待', () => A.cancelJob(j.id)),
    h('span', { class: 'muted', text: '只停止等待，模拟器会继续开机。' }));
  r.logs = h('div', { class: 'loglines' });
  r.sum = h('summary');
  r.det = h('details', null, r.sum, r.logs);
  r.det.addEventListener('toggle', () => fillJobLog(r));
  r.el = h('li', { class: 'job' },
    h('div', { class: 'job-head' }, h('span', { class: 'job-ic' }, icon(KIND_ICON[j.kind] || 'pulse')), h('div', { class: 'job-titles' }, r.title, r.meta, r.why), r.pill),
    r.prog, r.err, r.warn, r.res, r.acts, r.det);
  return r;
}

function updateJob(r, j) {
  const inst = instance(j.index);
  const name = inst ? label(inst) : `实例 ${j.index}`;
  if (r.name !== name) {
    r.name = name;
    r.title.replaceChildren(`${KIND[j.kind] || j.kind}「${name}」`, inst && name !== inst.name && h('span', { class: 'muted', text: inst.name }));
  }
  const running = j.state === 'running';
  const t0 = parseTime(j.created_at);
  const took = t0 && ((parseTime(j.finished_at) || Date.now()) - t0) / 1000;
  setText(r.meta, clock(j.created_at) + (took ? ` · ${running ? '已进行' : '用时'} ${dur(took)}` : ''));
  setText(r.pill, STATE[j.state] || j.state);
  attr(r.pill, 'class', 'pill tone-' + (STATE_TONE[j.state] || 'idle'));

  r.prog.hidden = !running;
  if (running) {
    const p = j.progress;
    const n = pct(p);
    setText(r.msg, (p && stripInst(p.message)) || (j.kind === 'stop' ? '正在关机…' : j.kind === 'install' ? '正在安装…' : '进行中…'));
    setText(r.pctEl, n == null ? '' : Math.round(n) + '%');
    r.bar.classList.toggle('indet', n == null);
    r.fill.style.width = n == null ? '' : n.toFixed(1) + '%';
  }
  r.acts.hidden = !(running && (j.kind === 'start' || j.kind === 'restart'));

  // 已取消: say why (stopped waiting, or a 关机 cut the boot short)
  const why = j.state === 'canceled' && j.error ? j.error.message || '' : '';
  setText(r.why, why);
  r.why.hidden = !why;

  const e = j.state === 'failed' && j.error;
  r.err.hidden = !e;
  const esig = e ? e.message + e.advice : '';
  if (r.esig !== esig) {
    r.esig = esig;
    r.err.replaceChildren(e ? h('strong', { text: e.message || '没有成功' }) : '', e && e.advice ? advice(e.advice) : '');
  }
  const warns = [...new Set([...(j.warnings || []), ...((j.result && j.result.warnings) || [])])];
  r.warn.hidden = !warns.length;
  if (r.wsig !== warns.join()) {
    r.wsig = warns.join();
    r.warn.replaceChildren(...warns.map((w) => h('li', null, icon('alert'), h('span', { text: w }))));
  }
  const res = j.state === 'done' ? resultText(j) : '';
  setText(r.res, res);
  r.res.hidden = !res;

  let n = 0;
  for (const l of S.logs) if (l.job_id === j.id) n++;
  r.det.hidden = !n;
  if (r.n !== n) r.sum.replaceChildren(`过程（${(r.n = n)} 条）`, icon('chevron'));
}

function fillJobLog(r) {
  if (!r.det.open) return;
  const lines = S.logs.filter((l) => l.job_id === r.id);
  if (r.logs.childNodes.length === lines.length) return;
  r.logs.replaceChildren(...lines.map(logLine));
  r.logs.scrollTop = r.logs.scrollHeight;
}

function resultText(j) {
  const r = j.result;
  if (!r) return '';
  if (j.kind === 'stop') return `${stopMethod(r.method)}${r.seconds ? `，${dur(r.seconds)}` : ''}。`;
  if (j.kind === 'install') return `已装好 ${r.package || r.apk || 'APK'}${r.output ? `（${r.output.trim()}）` : ''}`;
  if (!r.launched) return '模拟器本来就在运行，只重做了开机后的设置。';
  return [`安卓开机 ${dur(r.boot_seconds || 0)}，共 ${dur(r.total_seconds || 0)}`, r.first_boot && '完成了首次初始化',
    r.zygote_restarted && '装了 Vulkan 补丁（安卓框架重启过一次）'].filter(Boolean).join('，') + '。';
}
