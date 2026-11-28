// The boot screen of the viewer: the same picture and layout the emulator's window gets (internal/bootscreen, v3.go): the
// character in parts that move a little, the screen she holds filling in with the progress, a thin bar under her with the
// percentage at its right and a line saying what is being done under it. The pictures are the very files of the Go package
// (served at assets/boot/, rig.json says where each part goes and how it moves); the numbers are bootscreen.js. The page only
// shows it while an instance boots (so no eruption: when the instance is ready the page shows the live screen instead).

import { t } from './i18n.js';
import { h, setText } from './dom.js';
import { bootFraction, bootStage } from './bootscreen.js';

// what each stage says; the strings are the ones the overlay uses, so one translation serves both
const STAGE = () => ({
  launch: t('正在启动模拟器…'), android: t('正在等安卓开机…'), vulkan_fix: t('正在挂上客体库补丁…'), tune: t('正在做开机后调优…'),
  ime: t('正在设置输入法…'), first_boot: t('正在做首次初始化…'), sdcard: t('正在等存储就绪…'), boot: t('正在收尾…'),
});

const BASE = 'assets/boot/';
// the reference composition of the artwork: 1280 x 720 around the middle of the frame (layers.json "progressUi")
const REF = { w: 1280, h: 720, avX: 277, avY: -31, rigScale: 0.71, barX: 443, barY: 646, barW: 352, barH: 7, pctGap: 16, pctPx: 18,
  stepDown: 25, stepPx: 16 };

// the artwork, loaded once: rig.json and the pictures it names
let artPromise = null;
function loadArt() {
  if (artPromise) return artPromise;
  const img = (name) => new Promise((resolve, reject) => {
    const el = new Image();
    el.onload = () => resolve(el);
    el.onerror = () => reject(new Error(name));
    el.src = BASE + name;
  });
  artPromise = fetch(BASE + 'rig.json').then((r) => { if (!r.ok) throw new Error('rig.json'); return r.json(); }).then(async (rig) => {
    const parts = rig.parts.slice().sort((a, b) => a.z - b.z);
    const [pics, states] = await Promise.all([
      Promise.all(parts.map((p) => (p.file ? img(p.file) : null))),
      Promise.all(rig.screenStates.map((s) => img(s.file))),
    ]);
    return { rig, parts, pics, states };
  });
  artPromise.catch(() => { artPromise = null; }); // try again at the next show
  return artPromise;
}

// the movement of a part or group at time tMs: turn by theta about the pivot, then move (rig units); the same as the Go code's
function applyMotion(ctx, pivot, m, tMs) {
  const j = m.periodMs > 0 ? Math.sin((2 * Math.PI * tMs) / m.periodMs + (m.phase || 0)) : 0;
  const rot = ((m.rotateDeg || 0) * j * Math.PI) / 180;
  ctx.translate(pivot[0] + (m.translateX || 0) * j, pivot[1] + (m.translateY || 0) * j);
  ctx.rotate(rot);
  ctx.translate(-pivot[0], -pivot[1]);
}

function layoutOf(w, hgt) {
  const s = Math.min(hgt / REF.h, w / 810);
  const cx = w / 2, cy = hgt / 2;
  const at = (rx, ry) => [Math.round(cx + (rx - REF.w / 2) * s), Math.round(cy + (ry - REF.h / 2) * s)];
  const [avX, avY] = at(REF.avX, REF.avY);
  const [barX, barY] = at(REF.barX, REF.barY);
  const barW = Math.max(Math.round(REF.barW * s), 40), barH = Math.max(Math.round(REF.barH * s), 4);
  const pctPx = Math.max(Math.round(REF.pctPx * s), 11), stepPx = Math.max(Math.round(REF.stepPx * s), 11);
  return { s, cx, cy, avX, avY, k: Math.round(1024 * REF.rigScale * s) / 1024, barX, barY, barW, barH, pctPx, stepPx,
    pctX: barX + barW + Math.max(Math.round(REF.pctGap * s), 6), stepY: barY + barH + Math.max(Math.round(REF.stepDown * s), 8) };
}

function drawBackground(ctx, w, hgt, L) {
  // the artwork's elliptical radial gradient: centre (640, 296), radius (710, 514) on the 1280 x 720 reference
  ctx.save();
  ctx.fillStyle = '#251915';
  ctx.fillRect(0, 0, w, hgt);
  ctx.translate(L.cx, L.cy + (296 - REF.h / 2) * L.s);
  ctx.scale(710 * L.s, 514 * L.s);
  const g = ctx.createRadialGradient(0, 0, 0, 0, 0, 1);
  g.addColorStop(0, 'rgb(82,53,42)'); g.addColorStop(0.48, 'rgb(60,39,31)'); g.addColorStop(1, 'rgb(37,25,21)');
  ctx.fillStyle = g;
  ctx.fillRect(-w, -hgt, 2 * w + 2, 2 * hgt + 2); // far beyond the unit circle: the last stop's colour
  ctx.restore();
}

function drawCharacter(ctx, art, L, tMs, progress) {
  const { rig, parts, pics, states } = art;
  const groupMotion = (name) => {
    const out = [];
    for (let g = rig.groups[name]; g; g = g.parent ? rig.groups[g.parent] : null) {
      if (g.motion && g.motion.periodMs > 0) out.push([g.pivot, g.motion]);
    }
    return out;
  };
  const heads = {};
  for (let i = 0; i < parts.length; i++) {
    const p = parts[i];
    ctx.save();
    ctx.translate(L.avX, L.avY);
    ctx.scale(L.k, L.k);
    // the parents' transforms are applied after the part's own: the outermost first on the canvas
    const par = heads[p.parent] || (heads[p.parent] = groupMotion(p.parent));
    for (let q = par.length - 1; q >= 0; q--) applyMotion(ctx, par[q][0], par[q][1], tMs);
    if (p.file) {
      applyMotion(ctx, p.pivot, p.motion, tMs);
      ctx.drawImage(pics[i], p.origin[0], p.origin[1], p.size[0], p.size[1]);
    } else {
      // the screen: the state the progress is at, cross-faded with the next one
      const ps = rig.screenStates.map((s) => s.p);
      let k = 0;
      while (k + 2 < ps.length && progress >= ps[k + 1]) k++;
      const f = Math.min(Math.max((progress - ps[k]) / (ps[k + 1] - ps[k]), 0), 1);
      ctx.drawImage(states[k], p.origin[0], p.origin[1]);
      if (f > 0.004) { ctx.globalAlpha = f; ctx.drawImage(states[k + 1], p.origin[0], p.origin[1]); }
    }
    ctx.restore();
  }
}

function drawBar(ctx, L, progress, indet, tMs) {
  const { barX: x, barY: y, barW: bw, barH: bh } = L;
  const pill = (px, pw) => { ctx.beginPath(); ctx.roundRect(px, y, pw, bh, bh / 2); ctx.fill(); };
  ctx.save();
  ctx.fillStyle = 'rgba(255,245,229,0.2)';
  pill(x, bw);
  ctx.beginPath(); ctx.roundRect(x, y, bw, bh, bh / 2); ctx.clip();
  ctx.fillStyle = '#f07060';
  let head = 0;
  if (indet) {
    const seg = bw * 0.3;
    const ph = (tMs % 2400) / 2400;
    let pp = 1 - Math.abs(2 * ph - 1);
    pp = pp * pp * (3 - 2 * pp);
    const lo = pp * (bw - seg);
    ctx.fillRect(x + lo, y, seg, bh);
  } else {
    const hi = bw * Math.min(Math.max(progress, 0), 1);
    ctx.fillRect(x, y, hi, bh);
    const period = 1800;
    const band = ((tMs % period) / period) * (hi + bw * 0.18) - bw * 0.09; // the light that runs along the fill
    if (hi > 0) {
      const gr = ctx.createLinearGradient(x + band - bw * 0.09, 0, x + band + bw * 0.09, 0);
      gr.addColorStop(0, 'rgba(255,214,190,0)'); gr.addColorStop(0.5, 'rgba(255,214,190,0.5)'); gr.addColorStop(1, 'rgba(255,214,190,0)');
      ctx.fillStyle = gr;
      ctx.fillRect(x, y, hi, bh);
    }
    head = hi;
  }
  ctx.restore();
  if (!indet && head > 0) { // the warm light at the head of the fill, kept inside the track
    const r1 = Math.max(3 * L.s, 1.5), r2 = Math.max(5 * L.s, 2.5);
    const hx = x + Math.min(Math.max(head, r2), bw - r2), hy = y + bh / 2;
    ctx.fillStyle = 'rgba(255,153,91,0.224)';
    ctx.beginPath(); ctx.arc(hx, hy, r2, 0, 2 * Math.PI); ctx.fill();
    ctx.fillStyle = '#ffbd6d';
    ctx.beginPath(); ctx.arc(hx, hy, r1, 0, 2 * Math.PI); ctx.fill();
  }
}

export function createBootScreen() {
  const canvas = h('canvas', { class: 'vboot-canvas', 'aria-hidden': 'true' });
  // the text for screen readers; what the eye reads is drawn on the canvas
  const step = h('div', { class: 'vboot-sr' });
  const pct = h('div', { class: 'vboot-sr' });
  const el = h('div', { class: 'vboot', hidden: true, role: 'status' }, canvas, pct, step);
  const ctx = canvas.getContext('2d');
  const reduced = typeof matchMedia === 'function' && matchMedia('(prefers-reduced-motion: reduce)').matches;
  let art = null, want = 0, shown = 0, indet = true, stepText = '', pctText = '…', started = 0, last = 0, raf = 0, timer = 0;

  const draw = (now) => {
    raf = 0;
    if (el.hidden || !canvas.isConnected) return;
    const dpr = Math.min(window.devicePixelRatio || 1, 2);
    const cw = Math.max(Math.round(canvas.clientWidth * dpr), 1), ch = Math.max(Math.round(canvas.clientHeight * dpr), 1);
    if (canvas.width !== cw || canvas.height !== ch) { canvas.width = cw; canvas.height = ch; }
    const dt = last ? now - last : 0;
    last = now;
    // the shown percentage moves toward the real one quickly at first and never back (bootscreen.Ease)
    if (!indet && want > shown) shown += (want - shown) * (1 - Math.exp(-dt / 600));
    const tMs = reduced ? 0 : now - started;
    const L = layoutOf(cw, ch);
    drawBackground(ctx, cw, ch, L);
    if (art) drawCharacter(ctx, art, L, tMs, indet ? 0 : shown);
    drawBar(ctx, L, shown, indet, tMs);
    ctx.textBaseline = 'middle';
    ctx.font = `600 ${L.pctPx}px system-ui, sans-serif`;
    ctx.fillStyle = '#ffeddb';
    ctx.textAlign = 'left';
    ctx.fillText(indet ? '…' : Math.round(shown * 100) + '%', L.pctX, L.barY + L.barH / 2);
    ctx.font = `${L.stepPx}px system-ui, sans-serif`;
    ctx.fillStyle = '#eed7c5';
    ctx.textAlign = 'center';
    ctx.fillText(stepText, L.cx, L.stepY, Math.min(cw - 8, L.barW * 1.6));
    // the movement is slow: 15 frames a second are plenty (and the page is only showing while an instance boots)
    if (!reduced && !el.hidden) timer = setTimeout(() => { timer = 0; kick(); }, 66);
  };
  const kick = () => {
    if (!raf && !timer && !el.hidden) raf = requestAnimationFrame(draw);
  };
  new MutationObserver(() => {
    if (!el.hidden) {
      if (!started) started = performance.now();
      loadArt().then((a) => { art = a; kick(); }).catch(() => {});
      kick();
    }
  }).observe(el, { attributes: true, attributeFilter: ['hidden'] });

  return {
    el,
    update(inst) {
      const f = bootFraction(inst);
      const stages = STAGE();
      stepText = stages[bootStage(inst)] || stages.tune;
      indet = f == null;
      if (!indet) want = f;
      pctText = f == null ? '…' : Math.round(f * 100) + '%';
      setText(step, stepText);
      setText(pct, pctText);
      if (reduced) { shown = indet ? 0 : want; draw(performance.now()); }
      else kick();
    },
  };
}
