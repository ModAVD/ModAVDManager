// New instance, settings drawer, delete confirmation.

import { h, icon, btn, ibtn, showError, attr } from './dom.js';
import { api } from './api.js';
import { S, update, subscribe, instance, lastJob } from './store.js';
import { openDialog, toast } from './ui.js';
import { quoted, label, ownTitle, defaultName, uptime, fmtBytes } from './format.js';
import * as A from './actions.js';

const MEM = [[3072, '3 GB'], [4096, '4 GB'], [5120, '5 GB'], [6144, '6 GB']];
const CPU = [[2, '2 核'], [4, '4 核'], [6, '6 核']];
const FPS_MIN = 1;
const FPS_MAX = 60;
const RESOLUTION = [[1080, '1080p'], [720, '720p']];
const INDEXES = [0, 1, 2, 3, 4, 6, 7, 8, 9];
const MIN12 = '约 1⁠–⁠2 分钟';
const CREATE_TIME = '一般要几分钟';
const settingsOpen = new Map();
let seq = 0;

// the form parts the other dialogs (proxy.js) use too
export const hint = (text, id) => h('p', { class: 'f-hint', text, id });
export const cell = (...kids) => h('div', { class: 'f-ctl' }, kids);
export const inputRow = (text, el, ...more) => [h('label', { class: 'f-label', for: (el.id ? el : el.querySelector('input')).id, text }), cell(el, ...more)];

function segRow(text, name, opts, value, ...more) {
  const id = 'seg' + ++seq;
  return [h('span', { class: 'f-label', id, text }),
    cell(h('div', { class: 'seg', role: 'radiogroup', 'aria-labelledby': id },
      opts.map(([v, t]) => h('label', null, h('input', { type: 'radio', name, value: String(v), checked: v === value }), h('span', { text: t })))), ...more)];
}

// Data disk (ModAVD#153): 自动扩盘 (the default: 256 GB from the start, the host only pays for what is written) or a
// size the user sets; the size box shows only for the latter. minGB: the smallest size (16, or the current size in
// the settings: it can only grow). value(quiet) is {auto: true}, {auto: false, gb}, or null after showing the error
// (quiet: no error shown).
const DISK_AUTO_GB = 256;
const DISK_MAX_GB = 512;
function diskField(prefix, mode, gb, minGB) {
  const id = prefix + '-disk';
  const labelId = id + '-l';
  const startAuto = mode !== 'manual';
  const num = h('input', { class: 'input', id, type: 'number', min: String(minGB), max: String(DISK_MAX_GB), step: '1',
    value: String(startAuto ? Math.max(minGB, 64) : gb), inputmode: 'numeric', 'aria-describedby': id + '-h', 'aria-label': '数据盘大小' });
  const size = h('span', { class: 'with-suffix' }, num, h('span', { class: 'suffix', text: 'GB' }));
  const hintEl = hint('', id + '-h');
  const err = h('p', { class: 'f-err', id: id + '-e', role: 'alert', hidden: true });
  const radios = [['auto', '自动扩盘'], ['manual', '指定大小']].map(([v]) =>
    h('input', { type: 'radio', name: prefix + '-diskmode', value: v, checked: (v === 'auto') === startAuto }));
  const seg = h('div', { class: 'seg', role: 'radiogroup', 'aria-labelledby': labelId },
    radios.map((r, k) => h('label', null, r, h('span', { text: k === 0 ? '自动扩盘' : '指定大小' }))));
  const isAuto = () => radios[0].checked;
  const clear = () => {
    if (err.hidden) return;
    err.hidden = true;
    hintEl.hidden = false;
    attr(num, 'aria-invalid', null);
    attr(num, 'aria-describedby', id + '-h');
  };
  const sync = () => {
    size.hidden = isAuto();
    hintEl.textContent = isAuto() ? `一开始就是 ${DISK_AUTO_GB} GB，按实际写入占电脑硬盘，不用管大小`
      : `${minGB}–${DISK_MAX_GB} GB，以后只能改大；快满时会提醒`;
    clear();
  };
  seg.addEventListener('change', sync);
  num.addEventListener('input', clear);
  sync();
  const value = (quiet) => {
    if (isAuto()) return { auto: true };
    const n = Number(num.value);
    if (num.value.trim() && Number.isInteger(n) && n >= minGB && n <= DISK_MAX_GB) return { auto: false, gb: n };
    if (quiet) return null;
    err.replaceChildren(icon('alert'), h('span', { text: `要填 ${minGB}–${DISK_MAX_GB} 之间的整数（单位 GB）` }));
    err.hidden = false;
    hintEl.hidden = true;
    attr(num, 'aria-invalid', true);
    attr(num, 'aria-describedby', id + '-e');
    num.focus();
    return null;
  };
  return { rows: [h('span', { class: 'f-label', id: labelId, text: '数据盘' }), cell(seg, size, hintEl, err)], value };
}

// Frame rate 1–60 (ModAVD#180): a number box named fps (what the form sends) and a slider moving with it.
// check() returns the value, or shows the error under the field and returns null.
function fpsField(prefix, value) {
  const id = prefix + '-fps';
  const v0 = Number.isInteger(value) && value >= FPS_MIN && value <= FPS_MAX ? value : FPS_MAX;
  const num = h('input', { class: 'input', id, name: 'fps', type: 'number', min: String(FPS_MIN), max: String(FPS_MAX), step: '1', value: String(v0), inputmode: 'numeric', 'aria-describedby': id + '-h' });
  const range = h('input', { type: 'range', min: String(FPS_MIN), max: String(FPS_MAX), step: '1', value: String(v0), 'aria-label': '帧率', tabindex: '-1' });
  const hintEl = hint(`${FPS_MIN}–${FPS_MAX} 的整数，越低越省资源`, id + '-h');
  const err = h('p', { class: 'f-err', id: id + '-e', role: 'alert', hidden: true });
  const clear = () => {
    if (err.hidden) return;
    err.hidden = true;
    hintEl.hidden = false;
    attr(num, 'aria-invalid', null);
    attr(num, 'aria-describedby', id + '-h');
  };
  // the slider's input event bubbles to the form after this, so the settings drawer sees the new number
  range.addEventListener('input', () => { num.value = range.value; clear(); });
  num.addEventListener('input', () => {
    const n = Number(num.value);
    if (num.value.trim() && Number.isInteger(n) && n >= FPS_MIN && n <= FPS_MAX) range.value = String(n);
    clear();
  });
  const check = () => {
    const n = Number(num.value);
    if (num.value.trim() && Number.isInteger(n) && n >= FPS_MIN && n <= FPS_MAX) return n;
    err.replaceChildren(icon('alert'), h('span', { text: `要填 ${FPS_MIN}–${FPS_MAX} 之间的整数` }));
    err.hidden = false;
    hintEl.hidden = true;
    attr(num, 'aria-invalid', true);
    attr(num, 'aria-describedby', id + '-e');
    num.focus();
    return null;
  };
  const ctl = h('div', { class: 'fps-ctl' }, h('span', { class: 'with-suffix' }, num, h('span', { class: 'suffix', text: 'Hz' })), range);
  return { rows: inputRow('帧率', ctl, hintEl, err), check };
}

export const errorBox = () => h('div', { class: 'form-error', role: 'alert', hidden: true });

export function setBusy(b, busy, text) {
  if (!b.idle) b.idle = [...b.childNodes];
  attr(b, 'aria-busy', busy || null);
  attr(b, 'aria-disabled', busy || null);
  b.replaceChildren(...(busy ? [h('span', { class: 'spin' }), h('span', { text })] : b.idle));
}

// Runs one submit: busy button, the API call, then close + toast, or the error inside the dialog.
export async function submit(ctl, b, text, box, work, onFail) {
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
  const title = h('input', { class: 'input', id: 'c-title', maxlength: '40', autocomplete: 'off', placeholder: '明日方舟 · 主号' });
  // the number is always picked by the server (the first free one); the dialog no longer offers a choice
  const titleHint = hint(`不填就显示「${defaultName(free[0])}」，以后可以随时改`);
  const disk = diskField('c', 'auto', 64, 16);
  const fps = fpsField('c', 60);
  const box = errorBox();
  const ok = h('button', { class: 'btn btn-primary', type: 'submit' }, icon('plus'), h('span', { text: '新建' }));
  const ctl = openDialog({
    title: '新建实例', sub: '每台实例是一台独立的安卓模拟器，可以各登录一个游戏账号。', cls: 'dlg-create', form: true, focus: '#c-title',
    body: h('div', { class: 'dlg-body' }, h('div', { class: 'fields' },
      inputRow('名称', title, titleHint),
      segRow('内存', 'memory', MEM, 6144),
      segRow('CPU', 'cpu', CPU, 6),
      fps.rows,
      segRow('分辨率', 'resolution', RESOLUTION, 1080, hint('720p 更省资源')),
      disk.rows), box),
    actions: { ok },
    onSubmit() {
      const f = ctl.form.elements;
      const d = disk.value();
      if (d === null) return;
      const fpsValue = fps.check();
      if (fpsValue === null) return;
      submit(ctl, ok, '正在新建…', box, async () => {
        // a job: the setup boots the instance headless for a few minutes; its card shows the progress
        const job = await api.post('/api/instances', {
          title: title.value.trim() || undefined, memory_mb: Number(f.memory.value), cpu: Number(f.cpu.value),
          // no size: 自动扩盘
          fps: fpsValue, resolution: Number(f.resolution.value), data_partition_gb: d.auto ? undefined : d.gb,
        });
        A.onJob(job);
        S.watch.add(job.id);
        return { title: `正在新建实例 ${job.index}`, text: `后台无头开机做完初始化后关机，${CREATE_TIME}；关掉窗口也会在后台做完。` };
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
  const initTitle = ownTitle(inst0);
  const initDiagnostics = inst0.diagnostics === true;
  const title = h('input', { class: 'input', id: 's-title', maxlength: '40', autocomplete: 'off', value: initTitle, placeholder: defaultName(i) });
  const diagnostics = h('input', { type: 'checkbox', id: 's-diagnostics', checked: initDiagnostics, 'aria-describedby': 's-diagnostics-h' });
  const boss = bossKeyField(inst0.boss_key || '', () => dirty());
  const runNote = h('div', { class: 'note-box tone-warn', role: 'status' }, icon('info'), h('span', { text: '实例运行中，内存、CPU、帧率、分辨率和数据盘在下次开机生效。诊断设置立即生效。' }));
  const kv = h('dl', { class: 'kv' });
  const fps = fpsField('s', cfg.fps);
  const diskAuto0 = cfg.data_disk_mode !== 'manual';
  const diskGB0 = cfg.data_partition_gb || 16;
  // the size can only grow
  const disk = diskField('s', cfg.data_disk_mode, diskGB0, Math.max(16, diskGB0));
  const box = errorBox();
  const save = h('button', { class: 'btn btn-primary', type: 'submit', 'aria-disabled': 'true' }, h('span', { text: '保存' }));
  const del = btn('btn btn-danger-ghost left', [icon('trash'), '删除…'], () => { if (!instance(i).running) openDelete(i); });
  let confirmation = null;
  const ctl = openDialog({
    title: '设置', sub: `${label(inst0)} · ${inst0.name}`, cls: 'drawer', form: true,
    focus: field === 'memory' ? 'input[name="memory"]:checked' : '#s-title',
    body: h('div', { class: 'dlg-body' }, runNote,
      h('h3', { class: 'sec-title', text: '可以修改' }),
      h('div', { class: 'fields' },
        inputRow('名称', title, hint(`不填就显示「${defaultName(i)}」`)),
        segRow('内存', 'memory', MEM, cfg.memory_mb),
        segRow('CPU', 'cpu', CPU, cfg.cpu),
        fps.rows,
        segRow('分辨率', 'resolution', RESOLUTION, cfg.resolution, hint('下次开机生效，自动化工具里的分辨率也要跟着改')),
        disk.rows,
        inputRow('诊断模式', h('span', { class: 'checkbox p-switch' }, diagnostics),
          hint('立即生效。历史日志上限 4 GiB、客体内存最多增加 48 MiB；日志可能包含账号信息，仅保存在本机。', 's-diagnostics-h')),
        boss.row),
      box,
      h('h3', { class: 'sec-title', text: '实例信息' }), kv),
    actions: { left: del, ok: save },
    onSubmit: saveSettings,
  });
  settingsOpen.set(i, ctl);

  function changes() {
    const f = ctl.form.elements;
    const out = {};
    if (title.value.trim() !== initTitle) out.title = title.value.trim();
    if (diagnostics.checked !== initDiagnostics) out.diagnostics = diagnostics.checked;
    if (boss.changed()) out.boss_key = boss.value();
    for (const [k, key] of [['memory', 'memory_mb'], ['cpu', 'cpu'], ['fps', 'fps'], ['resolution', 'resolution']]) {
      const v = f[k].value === '' ? null : Number(f[k].value);
      if (v != null && v !== cfg[key]) out[key] = v;
    }
    // 自动扩盘 or a size; an invalid size is no change yet (saving shows the error)
    const d = disk.value(true);
    if (d && d.auto && !diskAuto0) out.data_disk_auto = true;
    else if (d && !d.auto && (diskAuto0 || d.gb !== diskGB0)) out.data_partition_gb = d.gb;
    return out;
  }
  function dirty() {
    if (!ctl.busy) attr(save, 'aria-disabled', Object.keys(changes()).length ? null : true);
  }
  ctl.form.addEventListener('input', dirty);
  ctl.form.addEventListener('change', dirty);

  async function saveSettings() {
    if (ctl.busy || save.getAttribute('aria-disabled') === 'true') return;
    if (fps.check() === null || disk.value() === null) return;
    const pending = changes();
    if (pending.diagnostics === false) {
      ctl.busy = true;
      diagnostics.disabled = true;
      box.hidden = true;
      setBusy(save, true, '正在读取清理范围…');
      try {
        const preview = await api.get(`/api/instances/${i}/diagnostics/preview`);
        if (ctl.closed) return;
        if (!preview || preview.index !== i || !Number.isSafeInteger(preview.files) || preview.files < 0 || !Number.isSafeInteger(preview.bytes) || preview.bytes < 0) {
          throw new Error('读不到诊断日志清理范围，请稍后重试');
        }
        if (!await confirmDisable(preview)) return;
      } catch (e) {
        showError(box, e);
        return;
      } finally {
        ctl.busy = false;
        diagnostics.disabled = false;
        setBusy(save, false);
        dirty();
      }
      if (ctl.closed) return;
    }
    return submit(ctl, save, '正在保存…', box, async () => {
      const r = await api.patch(`/api/instances/${i}`, pending);
      const k = S.list.instances.findIndex((x) => x.index === i);
      if (k >= 0 && r.instance) S.list.instances[k] = r.instance;
      if (r.boss_key_taken) {
        // saved, but another program has the key: say so (the settings page marks it too)
        return { tone: 'warn', title: '已保存，但老板键用不了', text: `${r.instance.boss_key} 被别的程序占了，在设置里换一个。` };
      }
      if ('diagnostics' in pending) {
        let text = '诊断模式已开启，立即生效。';
        if (!pending.diagnostics) {
          const cleanup = r.diagnostics_cleanup;
          text = cleanup ? `诊断模式已关闭，已清理 ${cleanup.files} 个文件（${fmtBytes(cleanup.bytes)}）。` : '诊断模式已关闭。';
        }
        if (r.applies_next_boot && Object.keys(pending).some((k) => k !== 'diagnostics')) text += '其他设置在下次开机生效。';
        if (r.warnings && r.warnings.length) text += r.warnings.join('；');
        return { title: '已保存', text };
      }
      return r.applies_next_boot ? { title: '已保存，下次开机生效', text: '实例正在运行，这次开机还是原来的设置。' } : { title: '已保存' };
    }, dirty);
  }

  function confirmDisable(preview) {
    return new Promise((resolve) => {
      let accepted = false;
      const desc = 's-diagnostics-off-' + i;
      const ok = h('button', { class: 'btn btn-danger', type: 'submit' }, h('span', { text: '关闭并清理日志' }));
      const confirm = openDialog({
        title: '关闭诊断模式？', form: true, alert: true, desc,
        body: h('div', { class: 'dlg-body' }, h('p', { id: desc, text: `预计清理 ${preview.files} 个日志文件（${fmtBytes(preview.bytes)}）。` })),
        actions: { ok },
        onSubmit() { accepted = true; confirm.close(); },
      });
      confirmation = confirm;
      confirm.onClose.push(() => { confirmation = null; resolve(accepted); });
    });
  }

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
  ctl.onClose.push(() => { if (confirmation) confirmation.close(); un(); settingsOpen.delete(i); });
}

// ---- boss key (ModAVD/ModAVD#101)
// The key a KeyboardEvent.code stands for, as the manager writes it ("1", "A", "F5", "Num0", "PageUp"); '' when a boss
// key cannot use it. The code is the key's place, not its character: Ctrl+Alt+Shift+1 stays "1" on any layout.
const NAMED_KEYS = { Space: 'Space', PageUp: 'PageUp', PageDown: 'PageDown', End: 'End', Home: 'Home', Insert: 'Insert',
  Delete: 'Delete', ArrowLeft: 'Left', ArrowUp: 'Up', ArrowRight: 'Right', ArrowDown: 'Down' };
export function bossKeyName(code) {
  let m;
  if ((m = /^Digit([0-9])$/.exec(code))) return m[1];
  if ((m = /^Key([A-Z])$/.exec(code))) return m[1];
  if ((m = /^F([1-9]|1[0-9]|2[0-4])$/.exec(code))) return 'F' + m[1];
  if ((m = /^Numpad([0-9])$/.exec(code))) return 'Num' + m[1];
  return NAMED_KEYS[code] || '';
}

// bossKeyOf is the key combination of a keydown: { key } ("Ctrl+Alt+Shift+1"), { partial } while only modifiers are
// held, { error } for one a boss key cannot be, or null for keys the field lets through (Tab moves on).
export function bossKeyOf(e) {
  const mods = [e.ctrlKey && 'Ctrl', e.altKey && 'Alt', e.shiftKey && 'Shift'].filter(Boolean);
  if (e.metaKey || e.key === 'Meta' || e.key === 'OS') return { error: 'Win 的组合 Windows 自己留着用，换一个' };
  if (['Control', 'Alt', 'Shift', 'AltGraph'].includes(e.key)) return { partial: mods.join('+') };
  if (e.key === 'Tab' && !e.ctrlKey && !e.altKey) return null;
  const name = bossKeyName(e.code);
  if (!name) return { error: '这个键不能用：要数字、字母、F1–F24、小键盘数字或 Home / End 这类键' };
  if (!mods.length) return { error: '至少要带 Ctrl、Alt、Shift 中的一个' };
  return { key: [...mods, name].join('+') };
}

// bossKeyField is the 老板键 row of the settings drawer: click the field and press the combination; the × clears it.
// A key is tried at once (POST /api/boss-keys/check) and marked when another program has it. Hidden when this
// manager has no boss keys (not Windows).
function bossKeyField(initial, onChange) {
  let value = initial;
  const input = h('input', { class: 'input', id: 's-bosskey', readonly: true, autocomplete: 'off', value: initial,
    placeholder: '无', 'aria-describedby': 's-bosskey-h' });
  const warn = h('span', { class: 'bk-warn', role: 'img', title: '这个键被别的程序占了，换一个', 'aria-label': '这个键被别的程序占了，换一个', hidden: true }, icon('alert'));
  const clear = ibtn('x', '清空老板键（这台不要老板键）', () => set(''));
  const err = h('p', { class: 'f-err', role: 'alert', hidden: true });
  const help = hint('点一下输入框，再按想用的组合，至少带 Ctrl、Alt、Shift 中的一个。按一下藏起这台的窗口，再按一下叫回来；几台用同一个键就一起藏、一起显示。立即生效；关掉管理器窗口后留在托盘，照样能用。', 's-bosskey-h');
  const label = h('label', { class: 'f-label', for: 's-bosskey', text: '老板键' });
  const ctl = cell(h('span', { class: 'bosskey' }, input, clear, warn), err, help);
  label.hidden = ctl.hidden = true;
  let seq = 0;
  const check = async (key) => {
    const n = ++seq;
    if (!key) { warn.hidden = true; return; }
    try {
      const r = await api.post('/api/boss-keys/check', { key });
      if (n === seq) warn.hidden = !r.taken;
    } catch { if (n === seq) warn.hidden = true; }
  };
  function set(key) {
    err.hidden = true;
    value = key;
    input.value = key;
    onChange();
    check(key);
  }
  input.addEventListener('keydown', (e) => {
    const k = bossKeyOf(e);
    if (!k) return;
    e.preventDefault();
    e.stopPropagation();
    if (k.partial !== undefined) { input.value = k.partial ? k.partial + '+…' : value; return; }
    if (k.error) {
      input.value = value;
      err.replaceChildren(icon('alert'), h('span', { text: k.error }));
      err.hidden = false;
      return;
    }
    set(k.key);
  });
  // letting go of the modifiers without a key puts the value back
  input.addEventListener('keyup', (e) => { if (!e.ctrlKey && !e.altKey && !e.shiftKey) input.value = value; });
  input.addEventListener('blur', () => { input.value = value; });
  api.get('/api/boss-keys').then((r) => {
    if (!r || !r.supported) return;
    label.hidden = ctl.hidden = false;
    warn.hidden = !initial || !(r.taken || []).includes(initial);
  }).catch(() => {});
  return { row: [label, ctl], value: () => value, changed: () => value !== initial };
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
    ['数据盘', cfg.data_disk_mode === 'manual' ? `${cfg.data_partition_gb} GB（指定大小）` : `自动扩盘（${cfg.data_partition_gb} GB）`,
      inst.data_space && inst.data_space.host_bytes >= 0 ? `电脑上实际占 ${fmtBytes(inst.data_space.host_bytes)}` : ''],
    ['显示', cfg.width ? `${cfg.width}×${cfg.height} · ${cfg.dpi} dpi` : '—'],
    ['渲染器', inst.renderer || (inst.initialized ? '未知' : '首次开机后才知道'), inst.software_renderer ? '软件渲染（没有可用的显卡驱动）' : ''],
    ['截图共享内存', shm.shm || '—', shm.shm ? (shm.shm_ready ? '已就绪' : '未就绪') : ''],
    ['首次初始化', inst.initialized ? '已完成' : `还没做（首次开机时做，${MIN12}）`],
    // the record in mas-avd.json (InstanceInfo.play_store, play_store_partial); the card's 「更多操作」 menu adds or removes it
    inst.play_store_partial ? ['Google Play 商店', '只装了一部分', '上次补全中途失败，没能全部撤回：在「更多操作」里移除，或者再补全一次']
      : ['Google Play 商店', inst.play_store ? '已补全' : '没有', inst.play_store ? '' : '要用的话在实例的「更多操作」菜单里补全'],
    ['报告 GLES 3.2', inst.gles32 ? '开（实验）' : '关', inst.gles32 ? '只改报的版本，实际渲染仍是 3.1；在「更多操作」里关闭' : '实验功能，要用的话在实例的「更多操作」菜单里开启'],
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
