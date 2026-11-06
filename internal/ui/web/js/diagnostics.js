// 设置 → 组件与问题反馈。所有操作都走 API 任务，页面关闭不会中断后台任务。

import { h, icon, btn, attr, setText, showError } from './dom.js';
import { S, update, subscribe } from './store.js';
import { api } from './api.js';
import { openDialog, toast, toastError, copyText } from './ui.js';
import { openCheck } from './panels.js';

const PRIVACY = '隐私提示：问题包只保存在本机，不会自动上传。\r\n- 里面有：summary.txt（电脑和模拟器的情况）、模拟器日志、实例配置、AUTO-MAS 日志（mas 文件夹）；有实例在运行时还有现场截图和安卓状态（live 文件夹）；模拟器近 7 天崩溃过的话还有崩溃转储（crash 文件夹）。不收磁盘镜像，不收 AUTO-MAS 的配置和账号数据，也不会有意收集账号密码。\r\n- 这些文件里都有本机路径，路径里可能有 Windows 用户名；截图和 AUTO-MAS 日志里可能有游戏账号名。\r\n- 可以公开：在 GitHub Issue（https://github.com/ModAVD/ModAVD/issues）里贴 summary.txt 和日志，或者上传删掉 crash 文件夹后的问题包。上传前看一眼，不想公开的内容先删掉。\r\n- 不要公开：crash 文件夹（崩溃转储，含模拟器进程的线程栈和少量内存片段）。带 crash 文件夹的完整问题包请发邮件到 qiyinxi@foxmail.com，邮件里写上 Issue 编号。';

export function loadDiagnostics() {
  return api.get('/api/components').then((v) => { S.components = v; S.componentsError = null; update(); }).catch((e) => {
    S.componentsError = e;
    update();
    throw e;
  });
}

let ctl = null;

const active = (kind) => [...S.jobs.values()].filter((j) => j.kind === kind && j.state === 'running').sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at))[0] || null;
const latest = (kind) => [...S.jobs.values()].filter((j) => j.kind === kind).sort((a, b) => Date.parse(b.created_at) - Date.parse(a.created_at))[0] || null;

function startJob(p) {
  p.then((j) => { S.watch.add(j.id); S.jobs.set(j.id, j); update(); }).catch(toastError);
}

function cancel(id) {
  api.post(`/api/jobs/${encodeURIComponent(id)}/cancel`).then((j) => { S.jobs.set(j.id, j); update(); }).catch(toastError);
}

export function openComponentFetch() {
  Promise.all([api.get('/api/components/license'), api.get('/api/components/source')])
    .then(([v, saved]) => openLicense(v.license, v.license_source, saved)).catch(toastError);
}

function openLicense(license, source, saved = {}) {
  const text = h('pre', { class: 'loglines diag-license', tabindex: '0', text: license || '后台没有返回许可协议全文。' });
  const note = h('p', { class: 'f-hint', text: `取自：${source || '未知镜像'}` });
  const choices = [['auto', '自动'], ['googledownloads_cn', 'googledownloads_cn'], ['tencent', 'tencent'], ['google', 'google'], ['custom', '自定义地址']];
  const select = h('select', { class: 'input', 'aria-label': '组件下载源' }, ...choices.map(([value, label]) => h('option', { value, text: label })));
  select.value = saved.source || 'auto';
  const custom = h('input', { class: 'input', placeholder: 'https://example/AndroidSDK/', value: (saved.mirror_urls || []).join(',') });
  const customRow = h('div', { class: 'f-ctl' }, custom);
  const updateCustom = () => { customRow.hidden = select.value !== 'custom'; };
  select.addEventListener('change', updateCustom);
  updateCustom();
  const sourceRow = h('div', { class: 'fields' }, h('label', { class: 'f-label', text: '下载源' }), h('div', { class: 'f-ctl' }, select), customRow);
  const box = h('div', { class: 'diag-license-body' }, text, note, sourceRow);
  const agree = btn('btn btn-primary', '同意并下载', () => {
    agree.disabled = true;
    const selected = select.value;
    const urls = selected === 'custom' ? custom.value.split(',').map((x) => x.trim()).filter(Boolean) : [];
    if (selected === 'custom' && !urls.length) {
      agree.disabled = false;
      toast({ tone: 'err', title: '需要自定义地址', text: '请填写至少一个 HTTP(S) 下载目录。' });
      return;
    }
    const body = { source: selected, mirror_urls: urls };
    api.put('/api/components/source', body).then(() => startJob(api.post('/api/components/fetch', { accept_license: true, ...body })))
      .then(() => d.close()).catch((e) => { agree.disabled = false; toastError(e); });
  });
  const d = openDialog({ title: 'Android 软件开发套件许可协议', sub: '请完整阅读后再决定是否下载组件', cls: 'dlg-update', body: box,
    actions: { left: btn('btn btn-ghost', '不同意', () => d.close()), cancel: false, ok: agree }, light: true });
}

export function openDiagnostics() {
  if (ctl) return;
  const componentBox = h('div', { class: 'diag-components' });
  const componentError = h('div', { class: 'form-error', role: 'alert', hidden: true });
  const fetchBtn = btn('btn btn-ghost', '去检测页补全', () => { ctl.close(); openCheck(); });
  const compHead = h('div', { class: 'diag-head' }, h('strong', { text: '组件' }), fetchBtn);
  const compNote = h('p', { class: 'f-hint', text: '缺少组件时先查看许可协议；只有亲手点“同意并下载”才会开始下载。' });

  const componentProgressBox = h('div', { class: 'diag-result diag-component-progress' });
  const collectBox = h('div', { class: 'diag-result' });
  const collectBtn = btn('btn btn-secondary', [icon('file'), '收集问题信息'], () => startJob(api.post('/api/diag/collect', {})));
  const configBtn = btn('btn btn-ghost', [icon('copy'), '复制配置信息'], async () => {
    try {
      const v = await api.get('/api/diag/report-config');
      const text = v.text || '';
      const copied = await copyText(text);
      openConfig(text, copied);
    } catch (e) { toastError(e); }
  });
  const feedbackHead = h('div', { class: 'diag-head' }, h('strong', { text: '问题反馈' }), h('div', { class: 'diag-actions' }, collectBtn, configBtn));
  const privacy = h('p', { class: 'f-hint diag-privacy', text: PRIVACY });

  ctl = openDialog({ title: '组件与问题反馈', sub: '组件、问题信息和配置信息', cls: 'dlg-update', light: true,
    body: h('div', { class: 'dlg-body' }, compHead, compNote, componentError, componentBox, componentProgressBox, feedbackHead, collectBox, privacy) });

  const render = () => {
    const c = S.components;
    if (c && Array.isArray(c.components)) {
      componentBox.replaceChildren(...c.components.map((x) => h('div', { class: 'comp-row' },
        h('span', null, x.name || x.id, x.optional && h('span', { class: 'muted', text: '（可选）' })),
        h('span', { class: 'v', text: x.version || x.build || '未知版本' }),
        h('span', { class: x.installed ? 'tone-ok' : x.optional ? 'tone-idle' : 'tone-err', text: x.installed ? '已安装' : x.optional ? '未安装' : '缺少' }))));
      fetchBtn.hidden = !c.components.some((x) => !x.optional && !x.installed);
    }
    if (S.componentsError) { showError(componentError, S.componentsError); componentError.hidden = false; }
    else componentError.hidden = true;
    const fj = active('components');
    const cj = active('collect-info');
    fetchBtn.disabled = !!fj;
    collectBtn.disabled = !!cj;
    const p = (j) => {
      if (!j || !j.progress) return '';
      const x = j.progress;
      const bits = [x.message || x.step || '进行中'];
      if (x.total > 0) {
        const percent = Math.max(0, Math.min(100, Math.round((x.done || 0) * 100 / x.total)));
        const doneMB = Math.round((x.done || 0) / 1048576);
        const totalMB = Math.round(x.total / 1048576);
        bits.push(`${percent}%（${doneMB} / ${totalMB} MB）`);
      }
      if (x.speed > 0) bits.push(`${(x.speed / 1048576).toFixed(1)} MB/s`);
      if (x.remaining > 0) bits.push(`剩余 ${Math.ceil(x.remaining)} 秒`);
      return bits.join(' · ');
    };
    const componentRows = [];
    const componentJob = latest('components');
    if (fj) componentRows.push(h('p', { class: 'diag-running' }, h('span', { class: 'spin' }), `补全组件：${p(fj)}`, btn('btn btn-ghost', '取消', () => cancel(fj.id))));
    else if (componentJob?.state === 'done') {
      const doneID = componentJob.id;
      if (ctl.refreshedComponentsJob !== doneID && ctl.refreshingComponentsJob !== doneID) {
        ctl.refreshingComponentsJob = doneID;
        loadDiagnostics().catch(() => {}).finally(() => {
          if (ctl) {
            ctl.refreshedComponentsJob = doneID;
            update();
          }
        });
      }
      componentRows.push(h('p', { class: 'tone-ok', text: ctl.refreshedComponentsJob === doneID ? '组件补全完成' : '组件补全完成，正在刷新组件状态…' }));
    }
    else if (componentJob?.state === 'failed') componentRows.push(h('p', { class: 'tone-err', text: componentJob.error?.message || '组件补全失败。' }));
    else if (componentJob?.state === 'canceled') componentRows.push(h('p', { class: 'tone-idle', text: '已取消' }));
    componentProgressBox.replaceChildren(...componentRows);

    const rows = [];
    if (cj) rows.push(h('p', { class: 'diag-running' }, h('span', { class: 'spin' }), `收集问题信息：${p(cj)}`, btn('btn btn-ghost', '取消', () => cancel(cj.id))));
    const done = latest('collect-info');
    if (!cj && done?.state === 'done' && done.result) {
      rows.push(h('p', { class: 'diag-path', text: `问题包：${done.result.zip_path || '已生成'}` }),
        h('div', { class: 'diag-actions' }, btn('btn btn-ghost', '在资源管理器里显示', () => api.post('/api/open', { what: 'path', path: done.result.zip_path }).then(() => toast({ title: '已打开资源管理器' })).catch(toastError))));
      if (done.result.sections?.length) rows.push(h('ul', { class: 'diag-sections' }, ...done.result.sections.map((x) => h('li', { class: x.error ? 'tone-err' : 'tone-ok', text: `${x.name}：${x.error || '完成'}` }))));
    } else if (!cj && done?.state === 'failed') rows.push(h('p', { class: 'tone-err', text: done.error?.message || '收集问题信息失败。' }));
    else if (!cj && done?.state === 'canceled') rows.push(h('p', { class: 'tone-idle', text: '已取消' }));
    collectBox.replaceChildren(...rows);
  };
  const un = subscribe(render);
  ctl.onClose.push(() => { un(); ctl = null; });
  loadDiagnostics().catch(() => {});
  render();
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
