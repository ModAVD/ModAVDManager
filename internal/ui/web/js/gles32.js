// 开启 / 关闭「报告 GLES 3.2（实验）」 on one instance (API.md "报告 GLES 3.2"): asked first, since the job boots the
// instance cold once; then its card shows the progress and a toast the outcome. It only changes the OpenGL ES version
// the guest reports (so Google Play lets apps that list GLES 3.2 in); the rendering stays 3.1.

import { t, tNode } from './i18n.js';
import { h, icon } from './dom.js';
import { instance } from './store.js';
import { openDialog, toastError } from './ui.js';
import { quoted } from './format.js';
import * as A from './actions.js';

// what the switch does and does not do, shown on the menu item's hint and in the question
export const GLES32_NOTE = t('实验功能：只改报告的版本，实际渲染仍是 3.1。走 Vulkan 的游戏能用；真要 GLES 3.2 渲染的应用会装得上、打不开。');

const asking = new Set(); // instances with the question on screen

// action: on | off
export function openGLES32(i, action) {
  if (!instance(i) || asking.has(i)) return;
  const on = action === 'on';
  const inst = instance(i);
  const who = quoted(inst, i);
  const desc = 'gl-desc-' + i;
  const how = inst.running
    ? t('它正开着：会先关机，做完按原来的方式开回来。')
    : t('它现在关着：会在后台开机，做完再关上。');
  const ok = h('button', { class: 'btn btn-primary', type: 'submit' }, icon(on ? 'plus' : 'x'), h('span', { text: on ? t('开启') : t('关闭') }));
  asking.add(i);
  const ctl = openDialog({
    title: on ? t('开启「报告 GLES 3.2」？') : t('关闭「报告 GLES 3.2」？'), cls: 'dlg-store', form: true, desc,
    body: h('div', { class: 'dlg-body' },
      h('p', { id: desc, class: 'store-q' }, on
        ? tNode(t('让{who}把 OpenGL ES 版本报成 3.2（原来是 3.1），Google Play 就不会再把要求 GLES 3.2 的游戏判成不兼容。'), { who: h('strong', { text: who }) })
        : tNode(t('让{who}把 OpenGL ES 版本改回报 3.1。'), { who: h('strong', { text: who }) })),
      h('div', { class: 'note-box tone-warn' }, icon('alert'), h('span', { text: GLES32_NOTE })),
      inst.play_store && h('div', { class: 'note-box tone-info' }, icon('info'),
        h('span', { text: t('Play 商店会记着旧的设备信息：改完后到实例里 设置 → 应用 → Google Play 商店 → 存储，清除数据（账号不会退出），它才会按新版本重新判断兼容。') })),
      h('div', { class: 'note-box tone-info' }, icon('info'), h('span', { text: t('要冷启动一次，几分钟，期间这台实例不能用。{how}', { how }) }))),
    actions: { ok },
    onSubmit() {
      ctl.close();
      A.gles32(i, action, toastError);
    },
  });
  ctl.onClose.push(() => asking.delete(i));
}
