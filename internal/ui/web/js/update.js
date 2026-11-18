// 设置 → ModAVD 更新 (API.md "更新"): version and update state of this root, the update source and the Mirror 酱 CDK,
// a check on request, and 更新: the server's update job downloads and verifies, stops the instances and hands the swap
// to tools\modavd.exe; the manager then closes and is started again by it (update_apply.go), and that manager shows
// what happened (v.handoff). These settings are also `modavd update`'s; AUTO-MAS's own updater uses its own.

import { t, lang, localeView, setPreference, enumComma, paren, colon } from './i18n.js';
import { h, icon, btn, attr, setText, showError } from './dom.js';
import { S, update, subscribe } from './store.js';
import { api } from './api.js';
import { openDialog, toast, toastError } from './ui.js';
import { openDiagnostics, activeJob, progressText } from './diagnostics.js';
import { onJob, notifyJob } from './actions.js';
import { quoted } from './format.js';

const clock = (ms) => new Date(ms).toLocaleString(lang(), { hour12: false });
const ver = (v) => (v ? v.replace(/^v/, '') : '');
const PHASE = { idle: '', preparing: t('正在准备新版'), staged: t('新版已下载并核对好，待安装'), swapping: t('正在换上新版') };
const NOTE = t('点「更新」会先下载并核对新版（这时实例照常用），再正常关掉所有实例，管理器关闭、换上新版，换好后自动重新打开。用 AUTO-MAS 的也可以在 AUTO-MAS 里更新，它用 AUTO-MAS 设置里的更新源和 CDK；这里的更新源和 CDK 用于这里和命令行 modavd update。');
const NOTE_CLI = t('这个管理器不能自己更新：关掉所有实例和管理器后运行 tools\\modavd.exe update install。用 AUTO-MAS 的也可以在 AUTO-MAS 里更新，它用 AUTO-MAS 设置里的更新源和 CDK。');

export function loadUpdate() {
  return api.get('/api/update').then(onUpdate).catch(() => {});
}

// GET /api/update, the "update" event and every answer of the settings API.
export function onUpdate(v) {
  S.update = v;
  showHandoff(v.handoff);
  update();
}

// The server grants the notification to one page across all manager windows.
const handoffClaims = new Set();
async function showHandoff(x) {
  if (!x || !x.id || handoffClaims.has(x.id)) return;
  handoffClaims.add(x.id);
  try {
    const claimed = await api.post('/api/update/handoff/take', { id: x.id });
    if (!claimed) return;
    if (claimed.ok) toast({ title: claimed.updated ? t('已更新到 {version}', { version: ver(claimed.version) }) : (claimed.message || t('已经是最新版本')), timeout: 9000 });
    else toast({ tone: 'err', title: t('新版没有换上'), text: (claimed.error && claimed.error.message) || claimed.message, advice: claimed.error && claimed.error.advice, timeout: 15000 });
  } catch {
    handoffClaims.delete(x.id);
  }
}

// 更新 may be clicked: this manager can come back after the swap, the root can be updated, a check found a newer
// version (or one is already downloaded), and no update is running.
const installable = (v) => !!(v && v.can_install && v.updatable && (v.update_available || v.prepared_version)
  && !v.checking && !activeJob('update'));

function installTitle(v) {
  if (!v) return '';
  if (activeJob('update')) return t('正在更新');
  if (!v.can_install) return t('这个管理器不能自己更新');
  if (!v.updatable) return v.not_updatable_reason;
  if (!v.update_available && !v.prepared_version) return v.checked ? t('已经是最新版本') : t('先点「检查更新」');
  return '';
}

// The confirmation, then the update job (its end: actions.js notify, which closes this window on a hand-off). With
// instances running, a second confirmation names them: the update shuts them all down normally (as 关机 does).
function confirmInstall(v) {
  const target = ver(v.prepared_version || v.latest_version);
  const runningList = ((S.list && S.list.instances) || []).filter((x) => x.running);
  const running = runningList.length;
  const body = running
    ? t('先下载并核对新版，这时实例照常用；然后正常关掉正在运行的 {n} 台实例（安卓正常关机），管理器关闭、换上新版，换好后自动重新打开管理器。', { n: running })
    : t('先下载并核对新版，这时实例照常用（现在没有实例在运行）；然后管理器关闭、换上新版，换好后自动重新打开管理器。');
  const apply = async () => {
    try {
      // what the user just agreed to shut down: one started while the download runs makes the update stop and ask again
      const j = await api.post('/api/update/apply', { confirmed: runningList.map((x) => x.index) });
      const seen = S.jobs.get(j.id);
      if (seen && seen.state !== 'running') notifyJob(seen);
      else {
        S.watch.add(j.id);
        onJob(j);
      }
    } catch (e) { toastError(e); }
  };
  const confirmStop = () => {
    const names = runningList.map((x) => quoted(x, x.index)).join(lang() === 'en' ? ', ' : '');
    const stop = btn('btn btn-primary', [icon('stop'), t('关机并更新')], () => { second.close(); apply(); });
    const second = openDialog({
      title: t('要关掉 {n} 台实例', { n: running }), light: true,
      body: h('div', { class: 'dlg-body' },
        h('p', { text: t('更新会正常关掉{names}，和点「关机」一样。实例里正在跑的游戏和任务会中断，更新完要自己再开机。', { names }) })),
      actions: { ok: stop },
    });
  };
  const go = btn('btn btn-primary', [icon('download'), t('更新')], () => {
    dlg.close();
    if (running) confirmStop();
    else apply();
  });
  const dlg = openDialog({
    title: t('更新到 {target}', { target }), sub: t('ModAVD 整包：模拟器、组件、脚本和管理器'), light: true,
    body: h('div', { class: 'dlg-body' },
      h('p', { text: body }),
      h('p', { class: 'f-hint', text: t('实例的数据（avd 文件夹）和设置都不动。正在新建的实例要先等它建完；用 modavd mcp 的 AI 助手等程序也要先关掉。') })),
    actions: { ok: go },
  });
}

// The chip: 设置, with a dot when a check found a newer ModAVD.
export function renderUpdateChip(chip, badge) {
  const v = S.update;
  const fresh = !!(v && v.update_available);
  badge.hidden = !fresh;
  chip.title = fresh ? t('有新版本 ModAVD {version}', { version: ver(v.latest_version) }) : t('设置');
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
    h('label', null, radios[0], h('span', { text: 'GitHub' })), h('label', null, radios[1], h('span', { text: t('Mirror 酱') })));
  const cdk = h('input', { type: 'password', id: 'upd-cdk', class: 'input', autocomplete: 'off', spellcheck: 'false', maxlength: '256' });
  const cdkState = h('p', { class: 'f-hint' });
  const save = btn('btn btn-secondary', t('保存 CDK'), () => cdk.value.trim() && post({ cdk: cdk.value.trim() }, t('已保存 CDK')));
  const clear = btn('btn btn-ghost', t('清除'), () => post({ cdk: '' }, t('已清除 CDK')));
  const componentSource = h('select', { class: 'input', id: 'upd-comp' },
    ...[['auto', t('自动')], ['googledownloads_cn', 'googledownloads_cn'], ['tencent', 'tencent'], ['google', 'google'], ['custom', t('自定义地址')]]
      .map(([value, text]) => h('option', { value, text })));
  const componentURLs = h('input', { class: 'input', placeholder: 'https://example/AndroidSDK/', spellcheck: 'false', 'aria-label': t('自定义下载地址') });
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
      toast({ tone: 'err', title: t('需要自定义地址'), text: t('请填写至少一个 HTTP(S) 下载目录。') });
      return;
    }
    try {
      const v = await api.put('/api/components/source', { source, mirror_urls: urls });
      showSaved(v);
      toast({ title: t('已保存组件下载源'), text: source === 'custom' ? (v.mirror_urls || []).join(enumComma()) : source === 'auto' ? t('自动') : source });
    } catch (e) {
      toastError(e);
      if (source !== 'custom') { componentSource.value = savedSource; componentURLsRow.hidden = savedSource !== 'custom'; }
    }
  }
  const componentSave = btn('btn btn-secondary', t('保存'), saveComponentSource);
  const componentURLsRow = h('div', { class: 'upd-cdk upd-urls' }, componentURLs, componentSave);
  componentSource.addEventListener('change', () => {
    componentURLsRow.hidden = componentSource.value !== 'custom';
    if (componentSource.value !== 'custom') saveComponentSource();
    else componentURLs.focus();
  });
  componentURLs.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); saveComponentSource(); } });
  // the manager's language (GET / PUT /api/locale): 跟随系统 / the languages on offer; a change is saved and the page reloads
  const loc = localeView();
  const langName = (code) => ((loc.languages || []).find((x) => x.code === code) || {}).name || code;
  const langSelect = h('select', { class: 'input', id: 'upd-lang' },
    h('option', { value: 'auto', text: t('跟随系统（{language}）', { language: langName(loc.system) }) }),
    ...(loc.languages || []).map((x) => h('option', { value: x.code, text: x.name })));
  langSelect.value = loc.preference || 'auto';
  langSelect.addEventListener('change', () => {
    langSelect.disabled = true;
    setPreference(langSelect.value).then(() => location.reload()).catch((e) => {
      langSelect.disabled = false;
      langSelect.value = loc.preference || 'auto';
      toastError(e);
    });
  });
  const fields = h('div', { class: 'fields' },
    h('label', { class: 'f-label', for: 'upd-lang', text: t('语言') }), h('div', { class: 'f-ctl' }, langSelect),
    h('span', { class: 'f-label', id: 'upd-src-l', text: t('更新源') }), h('div', { class: 'f-ctl' }, seg),
    h('label', { class: 'f-label', for: 'upd-cdk', text: t('Mirror 酱 CDK') }),
    h('div', { class: 'f-ctl' }, h('div', { class: 'upd-cdk' }, cdk, save, clear), cdkState),
    h('label', { class: 'f-label', for: 'upd-comp', text: t('组件下载源') }), h('div', { class: 'f-ctl' }, componentSource, componentURLsRow,
      h('p', { class: 'f-hint', text: t('「补全组件」和 modavd components fetch 都从这里选的源下载；选了就保存。') })));
  const note = h('p', { class: 'f-hint' });
  const diagBtn = btn('btn btn-secondary', [icon('shield'), t('组件与问题反馈')], () => { ctl.close(); openDiagnostics(); });
  const when = h('span', { class: 'foot-note' });
  const checkBtn = btn('btn btn-secondary', null, () => run(api.post('/api/update/check')));
  const installBtn = btn('btn btn-primary', null, () => installable(S.update) && confirmInstall(S.update));
  for (const r of radios) r.addEventListener('change', () => r.checked && post({ source: r.value }, r.value === 'github' ? t('改用 GitHub 更新') : t('改用 Mirror 酱更新')));

  ctl = openDialog({
    title: t('设置'), sub: t('ModAVD 更新：模拟器、组件、脚本和管理器整包更新'), cls: 'dlg-update', light: true,
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
    const rows = [[t('当前版本'), ver(v.current_version) ? ver(v.current_version) + (v.current_build ? paren(v.current_build) : '') : t('未知')]];
    if (!v.updatable) rows.push([t('不能更新'), v.not_updatable_reason]);
    if (v.checked) rows.push([t('最新版本'), ver(v.latest_version) + paren(v.latest_source === 'mirrorchyan' ? t('Mirror 酱') : 'GitHub')]);
    if (v.update_available && v.required_automas) rows.push([t('要求 AUTO-MAS'), t('{version} 或更新（用 AUTO-MAS 的才要）', { version: v.required_automas })]);
    const phase = PHASE[v.phase] || '';
    if (phase) rows.push([t('状态'), v.prepared_version ? phase + colon() + ver(v.prepared_version) : phase]);
    const lr = v.last_result;
    if (v.handoff && !v.handoff.ok) rows.push([t('上次'), t('没换上：{message}', { message: v.handoff.message })]);
    else if (lr && lr.action) rows.push([t('上次'), lr.ok ? t('已更新到 {version}', { version: ver(lr.version) }) : t('没成功：{reason}', { reason: lr.error || t('原因未知') })]);
    kv.replaceChildren(...rows.flatMap(([k, val]) => [h('dt', { text: k }), h('dd', null, h('span', { class: 'v', text: val }))]));
    const job = activeJob('update');
    const line = job ? progressText(job) : (v.message || '');
    setText(msg, line);
    msg.hidden = !line;
    setText(note, v.can_install ? NOTE : NOTE_CLI);
    if (v.error && !v.checking) { showError(box, v.error); box.hidden = false; }
    for (const r of radios) r.checked = r.value === (v.source || 'github');
    setText(cdkState, v.cdk_set ? t('已填（加密保存在本机，只有这个 Windows 用户能读）') : v.source === 'mirrorchyan' ? t('没填：没有 CDK 时只能从 GitHub 下载') : t('选 Mirror 酱时才用到'));
    cdk.placeholder = v.cdk_set ? t('已填，输入新的会替换') : t('粘贴 CDK');
    clear.hidden = !v.cdk_set;
    componentURLsRow.hidden = componentSource.value !== 'custom';
    setText(when, v.checked_at ? t('检查于 {time}', { time: clock(v.checked_at) }) : '');
    checkBtn.replaceChildren(v.checking ? h('span', { class: 'spin' }) : icon('refresh'), v.checking ? t('正在检查…') : t('检查更新'));
    checkBtn.disabled = !!v.checking || !!job;
    attr(checkBtn, 'aria-busy', v.checking || null);
    installBtn.replaceChildren(job ? h('span', { class: 'spin' }) : icon('download'), job ? t('正在更新…') : t('更新'));
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
