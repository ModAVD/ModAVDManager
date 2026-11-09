// 设置 → ModAVD 更新 (API.md "更新"): version and update state of this root, the update source and the Mirror 酱 CDK,
// a check on request, and 更新: the server's update job downloads and verifies, stops the instances and hands the swap
// to tools\modavd.exe; the manager then closes and is started again by it (update_apply.go), and that manager shows
// what happened (v.handoff). These settings are also `modavd update`'s; AUTO-MAS's own updater uses its own.

import { h, icon, btn, attr, setText, showError } from './dom.js';
import { S, update, subscribe } from './store.js';
import { api } from './api.js';
import { openDialog, toast, toastError } from './ui.js';
import { openDiagnostics, activeJob, progressText } from './diagnostics.js';
import { onJob, notifyJob } from './actions.js';

const clock = (t) => new Date(t).toLocaleString('zh-CN', { hour12: false });
const ver = (v) => (v ? v.replace(/^v/, '') : '');
const PHASE = { idle: '', preparing: '正在准备新版', staged: '新版已下载并核对好，待安装', swapping: '正在换上新版' };
const NOTE = '点「更新」会先下载并核对新版（这时实例照常用），再正常关掉所有实例，管理器关闭、换上新版，换好后自动重新打开。'
  + '用 AUTO-MAS 的也可以在 AUTO-MAS 里更新，它用 AUTO-MAS 设置里的更新源和 CDK；这里的更新源和 CDK 用于这里和命令行 modavd update。';
const NOTE_CLI = '这个管理器不能自己更新：关掉所有实例和管理器后运行 tools\\modavd.exe update install。'
  + '用 AUTO-MAS 的也可以在 AUTO-MAS 里更新，它用 AUTO-MAS 设置里的更新源和 CDK。';

export function loadUpdate() {
  return api.get('/api/update').then(onUpdate).catch(() => {});
}

// GET /api/update, the "update" event and every answer of the settings API.
export function onUpdate(v) {
  S.update = v;
  showHandoff(v.handoff);
  update();
}

// What the CLI did after the last 更新 (this manager is the one it started again): told once per page.
let handoffShown = '';
function showHandoff(x) {
  if (!x || handoffShown === x.at) return;
  handoffShown = x.at;
  if (x.ok) toast({ title: x.updated ? `已更新到 ${ver(x.version)}` : (x.message || '已经是最新版本'), timeout: 9000 });
  else toast({ tone: 'err', title: '新版没有换上', text: (x.error && x.error.message) || x.message, advice: x.error && x.error.advice, timeout: 15000 });
}

// 更新 may be clicked: this manager can come back after the swap, the root can be updated, a check found a newer
// version (or one is already downloaded), and no update is running.
const installable = (v) => !!(v && v.can_install && v.updatable && (v.update_available || v.prepared_version)
  && !v.checking && !activeJob('update'));

function installTitle(v) {
  if (!v) return '';
  if (activeJob('update')) return '正在更新';
  if (!v.can_install) return '这个管理器不能自己更新';
  if (!v.updatable) return v.not_updatable_reason;
  if (!v.update_available && !v.prepared_version) return v.checked ? '已经是最新版本' : '先点「检查更新」';
  return '';
}

// The confirmation, then the update job (its end: actions.js notify, which closes this window on a hand-off).
function confirmInstall(v) {
  const target = ver(v.prepared_version || v.latest_version);
  const running = ((S.list && S.list.instances) || []).filter((x) => x.running).length;
  const stops = running ? `正常关掉正在运行的 ${running} 台实例（安卓正常关机）` : '（现在没有实例在运行）';
  const go = btn('btn btn-primary', [icon('download'), '更新'], async () => {
    dlg.close();
    try {
      const j = await api.post('/api/update/apply', {});
      const seen = S.jobs.get(j.id);
      if (seen && seen.state !== 'running') notifyJob(seen);
      else {
        S.watch.add(j.id);
        onJob(j);
      }
    } catch (e) { toastError(e); }
  });
  const dlg = openDialog({
    title: `更新到 ${target}`, sub: 'ModAVD 整包：模拟器、组件、脚本和管理器', light: true,
    body: h('div', { class: 'dlg-body' },
      h('p', { text: `先下载并核对新版，这时实例照常用；然后${stops}，管理器关闭、换上新版，换好后自动重新打开管理器。` }),
      // the manager installs as a person does, without the AUTO-MAS check the CLI does for AUTO-MAS
      v.required_automas && h('div', { class: 'note-box tone-warn', role: 'status' }, icon('alert'),
        h('span', { text: `这一版要求 AUTO-MAS ${v.required_automas} 或更新。用 AUTO-MAS 的，先把 AUTO-MAS 更新到这个版本，再更新这里。` })),
      h('p', { class: 'f-hint', text: '实例的数据（avd 文件夹）和设置都不动。正在新建的实例要先等它建完；用 modavd mcp 的 AI 助手等程序也要先关掉。' })),
    actions: { ok: go },
  });
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
  const componentSource = h('select', { class: 'input', id: 'upd-comp' },
    ...[['auto', '自动'], ['googledownloads_cn', 'googledownloads_cn'], ['tencent', 'tencent'], ['google', 'google'], ['custom', '自定义地址']]
      .map(([value, text]) => h('option', { value, text })));
  const componentURLs = h('input', { class: 'input', placeholder: 'https://example/AndroidSDK/', spellcheck: 'false', 'aria-label': '自定义下载地址' });
  // a built-in choice is saved when picked (like 更新源); 自定义地址 when its addresses are saved
  let savedSource = 'auto';
  const showSaved = (v) => {
    savedSource = v.source || 'auto';
    componentSource.value = savedSource;
    componentURLs.value = (v.mirror_urls || []).join(',');
    componentURLsRow.hidden = savedSource !== 'custom';
  };
  async function saveComponentSource() {
    const source = componentSource.value;
    const urls = source === 'custom' ? componentURLs.value.split(',').map((x) => x.trim()).filter(Boolean) : [];
    if (source === 'custom' && !urls.length) {
      toast({ tone: 'err', title: '需要自定义地址', text: '请填写至少一个 HTTP(S) 下载目录。' });
      return;
    }
    try {
      const v = await api.put('/api/components/source', { source, mirror_urls: urls });
      showSaved(v);
      toast({ title: '已保存组件下载源', text: source === 'custom' ? (v.mirror_urls || []).join('、') : source === 'auto' ? '自动' : source });
    } catch (e) {
      toastError(e);
      if (source !== 'custom') { componentSource.value = savedSource; componentURLsRow.hidden = savedSource !== 'custom'; }
    }
  }
  const componentSave = btn('btn btn-secondary', '保存', saveComponentSource);
  const componentURLsRow = h('div', { class: 'upd-cdk upd-urls' }, componentURLs, componentSave);
  componentSource.addEventListener('change', () => {
    componentURLsRow.hidden = componentSource.value !== 'custom';
    if (componentSource.value !== 'custom') saveComponentSource();
    else componentURLs.focus();
  });
  componentURLs.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); saveComponentSource(); } });
  const fields = h('div', { class: 'fields' },
    h('span', { class: 'f-label', id: 'upd-src-l', text: '更新源' }), h('div', { class: 'f-ctl' }, seg),
    h('label', { class: 'f-label', for: 'upd-cdk', text: 'Mirror 酱 CDK' }),
    h('div', { class: 'f-ctl' }, h('div', { class: 'upd-cdk' }, cdk, save, clear), cdkState),
    h('label', { class: 'f-label', for: 'upd-comp', text: '组件下载源' }), h('div', { class: 'f-ctl' }, componentSource, componentURLsRow,
      h('p', { class: 'f-hint', text: '「补全组件」和 modavd components fetch 都从这里选的源下载；选了就保存。' })));
  const note = h('p', { class: 'f-hint' });
  const diagBtn = btn('btn btn-secondary', [icon('shield'), '组件与问题反馈'], () => { ctl.close(); openDiagnostics(); });
  const when = h('span', { class: 'foot-note' });
  const checkBtn = btn('btn btn-secondary', null, () => run(api.post('/api/update/check')));
  const installBtn = btn('btn btn-primary', null, () => installable(S.update) && confirmInstall(S.update));
  for (const r of radios) r.addEventListener('change', () => r.checked && post({ source: r.value }, r.value === 'github' ? '改用 GitHub 更新' : '改用 Mirror 酱更新'));

  ctl = openDialog({
    title: '设置', sub: 'ModAVD 更新：模拟器、组件、脚本和管理器整包更新', cls: 'dlg-update', light: true,
    body: h('div', { class: 'dlg-body' }, msg, box, kv, fields, note, h('div', { class: 'diag-launch' }, diagBtn)),
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
    if (v.update_available && v.required_automas) rows.push(['要求 AUTO-MAS', `${v.required_automas} 或更新（用 AUTO-MAS 的才要）`]);
    const phase = PHASE[v.phase] || '';
    if (phase) rows.push(['状态', v.prepared_version ? `${phase}：${ver(v.prepared_version)}` : phase]);
    const lr = v.last_result;
    if (v.handoff && !v.handoff.ok) rows.push(['上次', `没换上：${v.handoff.message}`]);
    else if (lr && lr.action) rows.push(['上次', lr.ok ? `已更新到 ${ver(lr.version)}` : `没成功：${lr.error || '原因未知'}`]);
    kv.replaceChildren(...rows.flatMap(([k, t]) => [h('dt', { text: k }), h('dd', null, h('span', { class: 'v', text: t }))]));
    const job = activeJob('update');
    const line = job ? progressText(job) : (v.message || '');
    setText(msg, line);
    msg.hidden = !line;
    setText(note, v.can_install ? NOTE : NOTE_CLI);
    if (v.error && !v.checking) { showError(box, v.error); box.hidden = false; }
    for (const r of radios) r.checked = r.value === (v.source || 'github');
    setText(cdkState, v.cdk_set ? '已填（加密保存在本机，只有这个 Windows 用户能读）' : v.source === 'mirrorchyan' ? '没填：没有 CDK 时只能从 GitHub 下载' : '选 Mirror 酱时才用到');
    cdk.placeholder = v.cdk_set ? '已填，输入新的会替换' : '粘贴 CDK';
    clear.hidden = !v.cdk_set;
    componentURLsRow.hidden = componentSource.value !== 'custom';
    setText(when, v.checked_at ? `检查于 ${clock(v.checked_at)}` : '');
    checkBtn.replaceChildren(v.checking ? h('span', { class: 'spin' }) : icon('refresh'), v.checking ? '正在检查…' : '检查更新');
    checkBtn.disabled = !!v.checking || !!job;
    attr(checkBtn, 'aria-busy', v.checking || null);
    installBtn.replaceChildren(job ? h('span', { class: 'spin' }) : icon('download'), job ? '正在更新…' : '更新');
    const can = installable(v);
    installBtn.disabled = !can;
    attr(installBtn, 'aria-disabled', can ? null : 'true');
    attr(installBtn, 'aria-busy', job ? 'true' : null);
    attr(installBtn, 'title', installTitle(v) || null);
  };
  const un = subscribe(render);
  ctl.onClose.push(() => { un(); ctl = null; });
  api.get('/api/components/source').then(showSaved).catch(() => {});
  render();
}
