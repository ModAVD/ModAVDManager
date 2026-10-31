// New instance, settings drawer, delete confirmation.

import { h, icon, btn, ibtn, showError, attr } from './dom.js';
import { api } from './api.js';
import { S, update, subscribe, instance, lastJob } from './store.js';
import { openDialog, toast } from './ui.js';
import { quoted, label, uptime } from './format.js';
import * as A from './actions.js';

const MEM = [[3072, '3 GB'], [4096, '4 GB'], [5120, '5 GB'], [6144, '6 GB']];
const CPU = [[2, '2 核'], [4, '4 核'], [6, '6 核']];
const FPS = [[30, '30 Hz'], [60, '60 Hz']];
const INDEXES = [0, 1, 2, 3, 4, 6, 7, 8, 9];
const MIN12 = '约 1⁠–⁠2 分钟';
const settingsOpen = new Map();
let seq = 0;

const hint = (text, id) => h('p', { class: 'f-hint', text, id });
const cell = (...kids) => h('div', { class: 'f-ctl' }, kids);
const inputRow = (text, el, ...more) => [h('label', { class: 'f-label', for: (el.id ? el : el.querySelector('input')).id, text }), cell(el, ...more)];

function segRow(text, name, opts, value, ...more) {
  const id = 'seg' + ++seq;
  return [h('span', { class: 'f-label', id, text }),
    cell(h('div', { class: 'seg', role: 'radiogroup', 'aria-labelledby': id },
      opts.map(([v, t]) => h('label', null, h('input', { type: 'radio', name, value: String(v), checked: v === value }), h('span', { text: t })))), ...more)];
}

const errorBox = () => h('div', { class: 'form-error', role: 'alert', hidden: true });

function setBusy(b, busy, text) {
  if (!b.idle) b.idle = [...b.childNodes];
  attr(b, 'aria-busy', busy || null);
  attr(b, 'aria-disabled', busy || null);
  b.replaceChildren(...(busy ? [h('span', { class: 'spin' }), h('span', { text })] : b.idle));
}

// Runs one submit: busy button, the API call, then close + toast, or the error inside the dialog.
async function submit(ctl, b, text, box, work, onFail) {
  if (ctl.busy || b.getAttribute('aria-disabled') === 'true') return;
  ctl.busy = true;
  box.hidden = true;
  setBusy(b, true, text);
  try {
    const msg = await work();
    update();
    ctl.busy = false;
    ctl.close();
    toast(msg);
  } catch (e) {
    ctl.busy = false;
    setBusy(b, false);
    if (onFail) onFail();
    showError(box, e);
  }
}

// ---- create
export function openCreate() {
  if (!S.list) return;
  const used = new Set(S.list.instances.map((x) => x.index));
  const free = INDEXES.filter((i) => !used.has(i));
  if (!free.length) {
    toast({ tone: 'info', title: '已经有 9 台实例了', text: '最多 9 台（编号 0–9，5 除外）。删掉不用的实例再新建。' });
    return;
  }
  const title = h('input', { class: 'input', id: 'c-title', maxlength: '40', autocomplete: 'off', placeholder: '比如：明日方舟 · 主号' });
  const titleHint = hint('');
  const disk = h('input', { class: 'input', id: 'c-disk', type: 'number', min: '16', max: '512', step: '1', value: '64', inputmode: 'numeric', 'aria-describedby': 'c-disk-h' });
  // the error takes the hint's line (both one line), so the dialog does not move when it shows
  const diskHint = hint('按实际写入增长。16–512 GB，建好后不能改', 'c-disk-h');
  const diskErr = h('p', { class: 'f-err', id: 'c-disk-e', role: 'alert', hidden: true });
  const idx = h('select', { class: 'select', id: 'c-index' }, h('option', { value: '', text: `自动（编号 ${free[0]}）` }),
    free.map((i) => h('option', { value: String(i), text: `编号 ${i}` })));
  const idxHint = hint('');
  const sync = () => {
    const i = idx.value === '' ? free[0] : Number(idx.value);
    titleHint.textContent = `不填就显示 mas_${i}，以后可以随时改`;
    idxHint.replaceChildren('编号决定端口，脚本连 ', h('b', { text: `127.0.0.1:${20001 + 10 * i}` }));
  };
  idx.addEventListener('change', sync);
  sync();
  disk.addEventListener('input', () => {
    if (diskErr.hidden) return;
    diskErr.hidden = true;
    diskHint.hidden = false;
    attr(disk, 'aria-invalid', null);
    attr(disk, 'aria-describedby', 'c-disk-h');
  });
  const box = errorBox();
  const ok = h('button', { class: 'btn btn-primary', type: 'submit' }, icon('plus'), h('span', { text: '新建' }));
  const ctl = openDialog({
    title: '新建实例', sub: '每台实例是一台独立的安卓模拟器，可以各登录一个游戏账号。', cls: 'dlg-create', form: true, focus: '#c-title',
    body: h('div', { class: 'dlg-body' }, h('div', { class: 'fields' },
      inputRow('名称', title, titleHint),
      segRow('内存', 'memory', MEM, 6144),
      segRow('CPU', 'cpu', CPU, 6),
      segRow('帧率', 'fps', FPS, 60, hint('30 更省资源')),
      inputRow('数据盘上限', h('span', { class: 'with-suffix' }, disk, h('span', { class: 'suffix', text: 'GB' })), diskHint, diskErr),
      inputRow('编号', idx, idxHint)), box),
    actions: { ok },
    onSubmit() {
      const f = ctl.form.elements;
      const n = Number(disk.value);
      if (!disk.value.trim() || !Number.isInteger(n) || n < 16 || n > 512) {
        diskErr.replaceChildren(icon('alert'), h('span', { text: '要填 16–512 之间的整数（单位 GB）' }));
        diskErr.hidden = false;
        diskHint.hidden = true;
        attr(disk, 'aria-invalid', true);
        attr(disk, 'aria-describedby', 'c-disk-e');
        disk.focus();
        return;
      }
      submit(ctl, ok, '正在新建…', box, async () => {
        const inst = await api.post('/api/instances', {
          title: title.value.trim() || undefined, memory_mb: Number(f.memory.value), cpu: Number(f.cpu.value),
          fps: Number(f.fps.value), data_partition_gb: n, index: idx.value === '' ? undefined : Number(idx.value),
        }, { headers: { Prefer: 'respond-async' } });
        if (inst && inst.kind === 'create') return { title: '正在新建实例', text: '后台会无头开机、完成初始化并正常关机；可在活动面板查看进度。' };
        if (!S.list.instances.some((x) => x.index === inst.index)) S.list.instances.push(inst);
        return { title: `已新建${quoted(inst)}`, text: `adb 地址 ${inst.adb_address}。首次开机会做初始化，${MIN12}。` };
      });
    },
  });
}

// ---- settings
// field: 'memory' focuses the memory choice (the card's 「调小内存」 link)
export function openSettings(i, field) {
  const inst0 = instance(i);
  if (!inst0 || settingsOpen.has(i)) return;
  const cfg = inst0.config || {};
  const initTitle = inst0.title && inst0.title !== inst0.name ? inst0.title : '';
  const title = h('input', { class: 'input', id: 's-title', maxlength: '40', autocomplete: 'off', value: initTitle, placeholder: inst0.name });
  const runNote = h('div', { class: 'note-box tone-warn', role: 'status' }, icon('info'), h('span', { text: '实例运行中，改动在下次开机生效。' }));
  const kv = h('dl', { class: 'kv' });
  const box = errorBox();
  const save = h('button', { class: 'btn btn-primary', type: 'submit', 'aria-disabled': 'true' }, h('span', { text: '保存' }));
  const del = btn('btn btn-danger-ghost left', [icon('trash'), '删除…'], () => { if (!instance(i).running) openDelete(i); });
  const ctl = openDialog({
    title: '设置', sub: `${label(inst0)} · ${inst0.name}`, cls: 'drawer', form: true,
    focus: field === 'memory' ? 'input[name="memory"]:checked' : '#s-title',
    body: h('div', { class: 'dlg-body' }, runNote,
      h('h3', { class: 'sec-title', text: '可以修改' }),
      h('div', { class: 'fields' },
        inputRow('名称', title, hint(`不填就显示 ${inst0.name}`)),
        segRow('内存', 'memory', MEM, cfg.memory_mb),
        segRow('CPU', 'cpu', CPU, cfg.cpu),
        segRow('帧率', 'fps', FPS, cfg.fps, hint('30 更省资源'))),
      box,
      h('h3', { class: 'sec-title', text: '实例信息' }), kv),
    actions: { left: del, ok: save },
    onSubmit: () => submit(ctl, save, '正在保存…', box, async () => {
      const r = await api.patch(`/api/instances/${i}`, changes());
      const k = S.list.instances.findIndex((x) => x.index === i);
      if (k >= 0 && r.instance) S.list.instances[k] = r.instance;
      return r.applies_next_boot ? { title: '已保存，下次开机生效', text: '实例正在运行，这次开机还是原来的设置。' } : { title: '已保存' };
    }, dirty),
  });
  settingsOpen.set(i, ctl);

  function changes() {
    const f = ctl.form.elements;
    const out = {};
    if (title.value.trim() !== initTitle) out.title = title.value.trim();
    for (const [k, key] of [['memory', 'memory_mb'], ['cpu', 'cpu'], ['fps', 'fps']]) {
      const v = f[k].value === '' ? null : Number(f[k].value);
      if (v != null && v !== cfg[key]) out[key] = v;
    }
    return out;
  }
  function dirty() {
    if (!ctl.busy) attr(save, 'aria-disabled', Object.keys(changes()).length ? null : true);
  }
  ctl.form.addEventListener('input', dirty);
  ctl.form.addEventListener('change', dirty);

  let sig = '';
  const render = () => {
    const inst = instance(i);
    if (!inst) { ctl.close(); return; }
    runNote.hidden = !inst.running;
    attr(del, 'aria-disabled', inst.running || null);
    del.title = inst.running ? '先关机才能删除' : '';
    const rows = infoRows(inst);
    const s = JSON.stringify(rows.map((r) => r.slice(0, 3)));
    if (s === sig) return;
    sig = s;
    kv.replaceChildren(...rows.flatMap(([k, v, extra, act]) => [h('dt', { text: k }),
      h('dd', null, h('span', { class: extra === 'mono' ? 'v mono' : 'v' }, v, extra && extra !== 'mono' && h('span', { class: 'dim', text: extra })), act)]));
  };
  const un = subscribe(render);
  render();
  ctl.onClose.push(() => { un(); settingsOpen.delete(i); });
}

function infoRows(inst) {
  const p = inst.ports || {};
  const cfg = inst.config || {};
  const shm = inst.screencap || {};
  const port = S.list && S.list.adb_server_port;
  return [
    ['编号', `${inst.index}（${inst.name}）`],
    ['状态', inst.status === 'online' ? `运行中 · PID ${inst.pid} · ${uptime(inst.started_at)}` : inst.status === 'starting' ? `开机中 · PID ${inst.pid}` : '已关机'],
    ['adb 地址', inst.adb_address, 'mono', ibtn('copy', '复制 adb 地址', () => A.copy(inst.adb_address))],
    ['序列号', inst.serial, port ? `私有 adb server ${port} 上` : ''],
    ['端口', `控制台 ${p.console} · adb ${p.adb} · gRPC ${p.grpc}`],
    ['数据盘上限', `${cfg.data_partition_gb} GB`, '按实际写入增长，建好后不能改'],
    ['显示', cfg.width ? `${cfg.width}×${cfg.height} · ${cfg.dpi} dpi` : '—'],
    ['渲染器', inst.renderer || (inst.initialized ? '未知' : '首次开机后才知道'), inst.software_renderer ? '软件渲染（没有可用的显卡驱动）' : ''],
    ['截图共享内存', shm.shm || '—', shm.shm ? (shm.shm_ready ? '已就绪' : '未就绪') : ''],
    ['首次初始化', inst.initialized ? '已完成' : `还没做（首次开机时做，${MIN12}）`],
    ['路径', inst.path, 'mono', ibtn('folder', '打开实例目录', () => A.openFolder('instance', inst.index))],
  ];
}

// ---- delete
export function openDelete(i) {
  const inst = instance(i);
  if (!inst) return;
  const who = quoted(inst);
  const descId = 'd-desc-' + i;
  const chk = h('input', { type: 'checkbox', id: 'd-ok' });
  // running: say so and offer the 关机 right here (the note hides by itself once the instance is off)
  const stopBtn = h('button', { class: 'btn btn-secondary btn-sm', type: 'button' });
  stopBtn.addEventListener('click', () => { if (stopBtn.getAttribute('aria-disabled') !== 'true') A.stop(i); });
  const runNote = h('div', { class: 'note-box tone-warn has-act', role: 'status' }, icon('alert'),
    h('span', { class: 'nb-text', text: '实例正在运行，先关机才能删除。' }), stopBtn);
  const box = errorBox();
  const del = h('button', { class: 'btn btn-danger', type: 'submit', 'aria-disabled': 'true' }, icon('trash'), h('span', { text: '永久删除' }));
  const ctl = openDialog({
    title: '删除实例？', cls: 'dlg-delete', form: true, focus: '#d-ok', alert: true, desc: descId,
    lead: h('span', { class: 'dlg-lead tone-err' }, icon('trash')),
    body: h('div', { class: 'dlg-body' },
      h('p', { class: 'danger-text', id: descId }, h('strong', { text: who }), '会连同里面装的游戏、登录状态和所有数据一起永久删除，不能恢复。'),
      h('div', { class: 'path-box', text: inst.path }), runNote,
      h('label', { class: 'checkbox danger' }, chk, h('span', { text: '我明白，所有数据都会删除' })), box),
    actions: { ok: del },
    onSubmit() {
      if (!chk.checked) chk.focus();
      submit(ctl, del, '正在删除…', box, async () => {
        await api.del(`/api/instances/${i}?confirm=1`);
        S.list.instances = S.list.instances.filter((x) => x.index !== i);
        const s = settingsOpen.get(i);
        if (s) s.close();
        return { title: `已删除${who}` };
      }, sync);
    },
  });
  let stopSig = '';
  function sync() {
    const cur = instance(i);
    if (!cur) return;
    runNote.hidden = !cur.running;
    const job = lastJob(i, true);
    const stopping = S.pending[i] === 'stop' || cur.busy === 'stopping' || (job && job.kind === 'stop');
    if (stopSig !== String(stopping)) {
      stopSig = String(stopping);
      stopBtn.replaceChildren(...(stopping ? [h('span', { class: 'spin' }), h('span', { text: '正在关机…' })] : [icon('power'), h('span', { text: '关机' })]));
      attr(stopBtn, 'aria-busy', stopping || null);
      attr(stopBtn, 'aria-disabled', stopping || null);
    }
    if (!ctl.busy) attr(del, 'aria-disabled', chk.checked && !cur.running ? null : true);
  }
  chk.addEventListener('change', sync);
  ctl.onClose.push(subscribe(sync));
  sync();
}
