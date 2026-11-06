// 补全 / 移除 Google Play 商店 on one instance (API.md "Google Play 商店"): asked first, since the job boots the
// instance cold once; then its card shows the progress and a toast the outcome. 补全 needs the root's five files:
// without them it goes to 电脑检查's Google Play 商店服务 (下载 or 使用我自己的文件) instead.

import { h, icon } from './dom.js';
import { S, instance } from './store.js';
import { openDialog, toast, toastError } from './ui.js';
import { quoted } from './format.js';
import { loadDiagnostics } from './diagnostics.js';
import { openCheck } from './panels.js';
import * as A from './actions.js';

const asking = new Set(); // instances with the question on screen

// action: install | uninstall
export async function openPlayStore(i, action) {
  if (!instance(i) || asking.has(i)) return;
  const install = action === 'install';
  if (install) {
    // the root's files as 组件 says now (another window may have imported them; the server checks their hashes itself)
    asking.add(i);
    await loadDiagnostics().catch(() => {});
    asking.delete(i);
    const st = S.components && S.components.play_store;
    if (st && !st.ready) {
      needFiles();
      return;
    }
  }
  const inst = instance(i);
  if (!inst) return;
  const who = quoted(inst, i);
  const desc = 'ps-desc-' + i;
  const how = inst.running
    ? '它正开着：会先关机，做完按原来的方式开回来。'
    : '它现在关着：会在后台开机，做完再关上。';
  const ok = h('button', { class: install ? 'btn btn-primary' : 'btn btn-danger', type: 'submit' }, icon(install ? 'plus' : 'x'),
    h('span', { text: install ? '补全' : '移除' }));
  asking.add(i);
  const ctl = openDialog({
    title: install ? '补全 Google Play 商店？' : '移除 Google Play 商店？', cls: 'dlg-store', form: true, desc,
    body: h('div', { class: 'dlg-body' },
      h('p', { id: desc, class: 'store-q' }, install
        ? ['把这个目录里的 Google Play 商店（商店、Play 服务、GSF 和合作伙伴设置）装进', h('strong', { text: who }), '，藏起系统原来的占位应用。']
        : ['从', h('strong', { text: who }), '里移除 Google Play 商店：卸掉 Google Play 装的商店和 Play 服务更新，删掉商店的文件，换回系统原来的占位应用。']),
      h('div', { class: 'note-box tone-info' }, icon('info'), h('span', { text: `要冷启动一次，几分钟，期间这台实例不能用。${how}` }))),
    actions: { ok },
    onSubmit() {
      ctl.close();
      A.playStore(i, action, install ? installRefused : toastError);
    },
  });
  ctl.onClose.push(() => asking.delete(i));
}

// The server's answer before the job (API.md: 422 not_ready with details.reason): the files are missing or damaged,
// so go and get them; anything else (no execution bridge, arm64, busy) is a toast with its own advice.
function installRefused(e) {
  if (e && e.details && e.details.reason === 'playstore_files_missing') {
    loadDiagnostics().catch(() => {});
    needFiles();
    return;
  }
  toastError(e);
}

function needFiles() {
  openCheck({ playStore: true });
  toast({ tone: 'info', title: '先补全 Google Play 商店服务', text: '这个目录还没有商店的五个文件：在下面下载，或者用自己的文件导入，再给实例补全。', timeout: 8000 });
}
