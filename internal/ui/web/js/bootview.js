// The boot screen of the viewer: the brand avatar on the brand brown with a progress bar, the percentage at its right
// and a line saying what is being done under it (the same picture and layout the emulator's window gets,
// internal/bootscreen; the numbers are bootscreen.js). The page only shows it while an instance boots; the picture is
// assets/boot-avatar.png.

import { t } from './i18n.js';
import { h, setText } from './dom.js';
import { bootFraction, bootStage } from './bootscreen.js';

// what each stage says; the strings are the ones the overlay uses, so one translation serves both
const STAGE = () => ({
  launch: t('正在启动模拟器…'), android: t('正在等安卓开机…'), vulkan_fix: t('正在挂上客体库补丁…'), tune: t('正在做开机后调优…'),
  ime: t('正在设置输入法…'), first_boot: t('正在做首次初始化…'), sdcard: t('正在等存储就绪…'), boot: t('正在收尾…'),
});

export function createBootScreen() {
  const avatar = h('img', { class: 'vboot-avatar', src: 'assets/boot-avatar.png', alt: '', draggable: 'false' });
  const fill = h('div', { class: 'vboot-fill' });
  const bar = h('div', { class: 'vboot-bar' }, fill);
  const pct = h('div', { class: 'vboot-pct' });
  const step = h('div', { class: 'vboot-step' });
  const el = h('div', { class: 'vboot', hidden: true, role: 'status' }, avatar, h('div', { class: 'vboot-row' }, bar, pct), step);
  return {
    el,
    update(inst) {
      const f = bootFraction(inst);
      const stages = STAGE();
      setText(step, stages[bootStage(inst)] || stages.tune);
      bar.classList.toggle('indet', f == null);
      fill.style.width = f == null ? '' : (f * 100).toFixed(1) + '%';
      setText(pct, f == null ? '…' : Math.round(f * 100) + '%');
    },
  };
}
