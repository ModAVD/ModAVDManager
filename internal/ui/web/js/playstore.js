// 补全 / 移除 Google Play 商店 on one instance (API.md "Google Play 商店"): asked first, since the job boots the
// instance cold once; then its card shows the progress and a toast the outcome. 补全 needs the root's five files:
// without them it goes to 电脑检查's Google Play 商店服务 (下载 or 使用我自己的文件) instead. 复制 GSF ID reads the
// running instance's GSF ID for registering it with Google.

import { h, icon, btn, ibtn } from './dom.js';
import { api } from './api.js';
import { S, instance } from './store.js';
import { openDialog, toast, toastError, copyText } from './ui.js';
import { quoted } from './format.js';
import { loadDiagnostics } from './diagnostics.js';
import { openCheck } from './panels.js';
import * as A from './actions.js';

// where a GSF ID is registered (emu.PlayStoreUncertifiedURL; the documentation site's step 6)
export const UNCERTIFIED = 'https://www.google.com/android/uncertified/';

const asking = new Set(); // instances with the question on screen
const reading = new Set(); // instances whose GSF ID is being read

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

// 复制 GSF ID (API.md GET playstore-gsfid): the running instance's GSF ID, the decimal one copied at once and both
// shown with where to register them. No ID yet is a hint to wait, not an error: GSF gets one once it is online.
export async function openGSFID(i) {
  if (!instance(i) || reading.has(i)) return;
  reading.add(i);
  let r;
  try {
    r = await api.get(`/api/instances/${i}/playstore-gsfid`);
  } catch (e) {
    if (e && e.details && e.details.reason === 'no_gsf_id') {
      toast({ tone: 'info', title: '还没有 GSF ID', text: '实例里的 Google 服务框架联网登记后才有：开着网等一两分钟，再试一次。', timeout: 8000 });
    } else toastError(e);
    return;
  } finally {
    reading.delete(i);
  }
  const copied = await copyText(r.decimal);
  const copy = (text, what) => ibtn('copy', '复制' + what, async () => { if (await copyText(text)) toast({ title: '已复制', text }); });
  const ctl = openDialog({
    title: 'GSF ID', sub: `${quoted(instance(i), i)}的 GSF ID：登记以后，这台实例上的 Google 应用才能用`, cls: 'dlg-gsf', light: true,
    body: h('div', { class: 'dlg-body' },
      h('div', { class: 'note-box tone-' + (copied ? 'ok' : 'info') }, icon(copied ? 'ok' : 'info'),
        h('span', { text: copied ? '已把十进制的 GSF ID 复制到剪贴板。' : '没能自动复制：用右边的按钮复制，或者选中后复制。' })),
      h('dl', { class: 'kv' },
        h('dt', { text: '十进制' }), h('dd', null, h('span', { class: 'v mono', text: r.decimal }), copy(r.decimal, '十进制的 GSF ID')),
        h('dt', { text: '十六进制' }), h('dd', null, h('span', { class: 'v mono', text: r.hex }), copy(r.hex, '十六进制的 GSF ID'))),
      h('ol', { class: 'gsf-steps' },
        h('li', null, '在电脑的浏览器里打开 ', h('span', { class: 'mono', text: UNCERTIFIED }), copy(UNCERTIFIED, '登记页的网址')),
        h('li', { text: '用要在实例里登录的那个 Google 账号登录，填入 GSF ID（登记页要哪种写法就填哪种），提交。' }),
        h('li', { text: '等一会儿让登记生效，把实例关机再开机，再去登录。' })),
      h('p', { class: 'gsf-note', text: '每台实例的 GSF ID 不一样，每台都要登记；清除「Google 服务框架」的数据或者重建实例以后会变，要重新登记。' })),
    actions: { cancel: false, ok: btn('btn btn-primary', '好', () => ctl.close()) },
  });
  return ctl;
}
