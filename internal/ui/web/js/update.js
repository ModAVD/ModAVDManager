// 设置 → ModAVD 更新 (API.md "更新"): version and update state of this root, the update source and the Mirror 酱 CDK,
// a check on request. Installing is not offered here yet (AUTO-MAS installs, through `modavd update`, with these
// settings); the button is shown and disabled.

import { h, icon, btn, attr, setText, showError } from './dom.js';
import { S, update, subscribe } from './store.js';
import { api } from './api.js';
import { openDialog, toast, toastError } from './ui.js';

const clock = (t) => new Date(t).toLocaleString('zh-CN', { hour12: false });
const ver = (v) => (v ? v.replace(/^v/, '') : '');
const PHASE = { idle: '', preparing: '正在准备新版', staged: '新版已下载并核对好，待安装', swapping: '正在换上新版' };

export function loadUpdate() {
  return api.get('/api/update').then(onUpdate).catch(() => {});
}

// GET /api/update, the "update" event and every answer of the settings API.
export function onUpdate(v) {
  S.update = v;
  update();
}

// The chip: 设置, with a dot when a check found a newer ModAVD.
export function renderUpdateChip(chip, badge) {
  const v = S.update;
  const fresh = !!(v && v.update_available);
  badge.hidden = !fresh;
  chip.title = fresh ? `有新版本 ModAVD ${ver(v.latest_version)}` : '设置';
  chip.setAttribute('aria-label', chip.title);
}

let ctl = null;

export function openUpdate() {
  if (ctl) return;
  const box = h('div', { class: 'form-error', role: 'alert', hidden: true });
  const kv = h('dl', { class: 'kv' });
  const msg = h('p', { class: 'upd-msg' });
  const radios = ['github', 'mirrorchyan'].map((value) => h('input', { type: 'radio', name: 'upd-src', value }));
  const seg = h('div', { class: 'seg', role: 'radiogroup', 'aria-labelledby': 'upd-src-l' },
    h('label', null, radios[0], h('span', { text: 'GitHub' })), h('label', null, radios[1], h('span', { text: 'Mirror 酱' })));
  const cdk = h('input', { type: 'password', id: 'upd-cdk', class: 'input', autocomplete: 'off', spellcheck: 'false', maxlength: '256' });
  const cdkState = h('p', { class: 'f-hint' });
  const save = btn('btn btn-secondary', '保存 CDK', () => cdk.value.trim() && post({ cdk: cdk.value.trim() }, '已保存 CDK'));
  const clear = btn('btn btn-ghost', '清除', () => post({ cdk: '' }, '已清除 CDK'));
  const fields = h('div', { class: 'fields' },
    h('span', { class: 'f-label', id: 'upd-src-l', text: '更新源' }), h('div', { class: 'f-ctl' }, seg),
    h('label', { class: 'f-label', for: 'upd-cdk', text: 'Mirror 酱 CDK' }),
    h('div', { class: 'f-ctl' }, h('div', { class: 'upd-cdk' }, cdk, save, clear), cdkState));
  const note = h('p', { class: 'f-hint', text: '更新现在由 AUTO-MAS 发起（它调用 modavd update），会按这里选的更新源和 CDK 下载；这里先不能直接更新。' });
  const when = h('span', { class: 'foot-note' });
  const checkBtn = btn('btn btn-secondary', null, () => run(api.post('/api/update/check')));
  const installBtn = btn('btn btn-primary', [icon('download'), '更新'], null,
    { disabled: true, 'aria-disabled': 'true', title: '暂时由 AUTO-MAS 发起更新' });
  for (const r of radios) r.addEventListener('change', () => r.checked && post({ source: r.value }, r.value === 'github' ? '改用 GitHub 更新' : '改用 Mirror 酱更新'));

  ctl = openDialog({
    title: '设置', sub: 'ModAVD 更新：模拟器、组件、脚本和管理器整包更新', cls: 'dlg-update', light: true,
    body: h('div', { class: 'dlg-body' }, msg, box, kv, fields, note),
    foot: h('footer', { class: 'dlg-foot' }, when, checkBtn, installBtn),
  });

  function run(p) {
    box.hidden = true;
    return p.then(onUpdate).catch((e) => { showError(box, e); box.hidden = false; toastError(e); });
  }
  function post(body, done) {
    run(api.post('/api/update/settings', body)).then(() => {
      if (box.hidden) toast({ title: done });
      if ('cdk' in body) cdk.value = '';
    });
  }

  const render = () => {
    const v = S.update;
    if (!v) return;
    const rows = [['当前版本', ver(v.current_version) ? `${ver(v.current_version)}${v.current_build ? `（${v.current_build}）` : ''}` : '未知']];
    if (!v.updatable) rows.push(['不能更新', v.not_updatable_reason]);
    if (v.checked) rows.push(['最新版本', `${ver(v.latest_version)}（${v.latest_source === 'mirrorchyan' ? 'Mirror 酱' : 'GitHub'}）`]);
    const phase = PHASE[v.phase] || '';
    if (phase) rows.push(['状态', v.prepared_version ? `${phase}：${ver(v.prepared_version)}` : phase]);
    const lr = v.last_result;
    if (lr && lr.action) rows.push(['上次', lr.ok ? `已更新到 ${ver(lr.version)}` : `没成功：${lr.error || '原因未知'}`]);
    kv.replaceChildren(...rows.flatMap(([k, t]) => [h('dt', { text: k }), h('dd', null, h('span', { class: 'v', text: t }))]));
    setText(msg, v.message || '');
    msg.hidden = !v.message;
    if (v.error && !v.checking) { showError(box, v.error); box.hidden = false; }
    for (const r of radios) r.checked = r.value === (v.source || 'github');
    setText(cdkState, v.cdk_set ? '已填（加密保存在本机，只有这个 Windows 用户能读）' : v.source === 'mirrorchyan' ? '没填：没有 CDK 时只能从 GitHub 下载' : '选 Mirror 酱时才用到');
    cdk.placeholder = v.cdk_set ? '已填，输入新的会替换' : '粘贴 CDK';
    clear.hidden = !v.cdk_set;
    setText(when, v.checked_at ? `检查于 ${clock(v.checked_at)}` : '');
    checkBtn.replaceChildren(v.checking ? h('span', { class: 'spin' }) : icon('refresh'), v.checking ? '正在检查…' : '检查更新');
    checkBtn.disabled = !!v.checking;
    attr(checkBtn, 'aria-busy', v.checking || null);
  };
  const un = subscribe(render);
  ctl.onClose.push(() => { un(); ctl = null; });
  render();
}
