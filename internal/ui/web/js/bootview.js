// The boot screen of the viewer: the brand avatar on the brand brown with a progress bar and a line saying what is
// being done (the same picture the emulator's window gets, internal/bootscreen; the bar and the numbers are
// bootscreen.js). The page only shows it while an instance boots; the picture is assets/boot-avatar.png.

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
  const title = h('div', { class: 'vboot-title' });
  const fill = h('div', { class: 'vboot-fill' });
  const pct = h('div', { class: 'vboot-pct' });
  const el = h('div', { class: 'vboot', hidden: true, role: 'status' }, avatar, title, h('div', { class: 'vboot-bar' }, fill), pct);
  return {
    el,
    update(inst) {
      const f = bootFraction(inst);
      const stages = STAGE();
      setText(title, stages[bootStage(inst)] || stages.tune);
      fill.style.width = (f * 100).toFixed(1) + '%';
      setText(pct, Math.round(f * 100) + '%');
    },
  };
}
