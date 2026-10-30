// Toasts, dialogs (native <dialog> with a focus trap), menus, clipboard.

import { h, icon, btn, ibtn } from './dom.js';

let host;
const stack = [];
const later = (fn, ms) => setTimeout(fn, matchMedia('(prefers-reduced-motion: reduce)').matches ? 0 : ms);

export function initUI() {
  host = document.getElementById('toasts');
}

// A modal dialog makes the rest of the page inert, so the toasts live inside the topmost dialog.
function placeHost() {
  const parent = stack.length ? stack[stack.length - 1].el : document.body;
  if (host.parentNode !== parent) parent.append(host);
}

// ---- toasts
export function toast({ tone = 'ok', title, text, advice, action, timeout }) {
  let timer = 0;
  const gone = () => {
    clearTimeout(timer);
    if (el.classList.contains('out')) return;
    el.classList.add('out');
    later(() => el.remove(), 170);
  };
  const arm = (ms) => { clearTimeout(timer); timer = setTimeout(gone, ms); };
  const el = h('div', { class: `toast tone-${tone}`, role: tone === 'err' ? 'alert' : 'status' },
    h('span', { class: 'toast-ic' }, icon({ ok: 'ok', err: 'bad', warn: 'alert' }[tone] || 'info')),
    h('div', { class: 'toast-body' },
      h('div', { class: 'toast-title', text: title }),
      text && h('div', { class: 'toast-text', text }),
      advice && h('div', { class: 'advice' }, icon('bulb'), h('span', { text: advice })),
      action && btn('link-btn', action.label, () => { gone(); action.run(); })),
    ibtn('x', '关闭提示', () => gone()),
  );
  el.addEventListener('pointerenter', () => clearTimeout(timer));
  el.addEventListener('focusin', () => clearTimeout(timer));
  el.addEventListener('pointerleave', () => arm(2500));
  host.append(el);
  while (host.children.length > 4) host.firstElementChild.remove();
  arm(timeout || (tone === 'err' ? 10000 : advice ? 7000 : 3200));
}

export function toastError(e) {
  if (!e || e.name === 'AbortError') return;
  if (e.name === 'TimeoutError') toast({ tone: 'err', title: '后台太久没有回应', advice: '稍后再试一次' });
  else toast({ tone: 'err', title: e.message || String(e), advice: e.advice });
}

// ---- dialogs
const FOCUSABLE = 'button, [href], input, select, textarea, summary, [tabindex]:not([tabindex="-1"])';
let seq = 0;

const focusables = (root) => [...root.querySelectorAll(FOCUSABLE)].filter((n) => !n.disabled && n.tabIndex !== -1
  && n.getClientRects().length && !(n.type === 'radio' && !n.checked));

// o: {title, sub, desc, body, foot | actions: {left, ok}, cls, lead, tools, form, onSubmit, light, focus, alert}
// desc: id of the element that describes the dialog (default: the sub line). tools: extra controls in the header row.
// Returns {el, form, titleEl, subEl, busy, onClose: [], close()}.
export function openDialog(o) {
  const id = 'dlg' + ++seq;
  const titleEl = h('h2', { class: 'dlg-title', id: id + 't', text: o.title });
  const subEl = h('p', { class: 'dlg-sub', id: id + 's', text: o.sub, hidden: !o.sub });
  const el = h('dialog', {
    class: 'dlg ' + (o.cls || ''), role: o.alert ? 'alertdialog' : 'dialog', 'aria-modal': 'true',
    'aria-labelledby': id + 't', 'aria-describedby': o.desc || (o.sub && id + 's'),
  }, h('header', { class: 'dlg-head' }, o.lead, h('div', { class: 'dlg-titles' }, titleEl, subEl), o.tools, ibtn('x', '关闭', () => close(), 'icon-btn dlg-x')));
  const parts = [o.body, o.actions ? h('footer', { class: 'dlg-foot' }, o.actions.left, btn('btn btn-ghost', '取消', () => close()), o.actions.ok) : o.foot];
  let form = null;
  if (o.form) {
    el.append((form = h('form', { class: 'dlg-form', novalidate: true }, parts)));
    form.addEventListener('submit', (e) => { e.preventDefault(); o.onSubmit(); });
  } else el.append(...parts.filter(Boolean));
  document.body.append(el);

  const opener = document.activeElement;
  const ctl = { el, form, titleEl, subEl, busy: false, onClose: [], close };
  el.addEventListener('cancel', (e) => { e.preventDefault(); if (!ctl.busy) close(); });
  el.addEventListener('keydown', (e) => {
    if (e.key !== 'Tab') return;
    const list = focusables(el);
    const first = list[0];
    const last = list[list.length - 1];
    if (!first) return;
    if (e.shiftKey ? document.activeElement === first || document.activeElement === el : document.activeElement === last) {
      e.preventDefault();
      (e.shiftKey ? last : first).focus();
    }
  });
  let down = false;
  el.addEventListener('pointerdown', (e) => { down = e.target === el; });
  el.addEventListener('click', (e) => {
    if (o.light && down && e.target === el && !ctl.busy) close();
    down = false;
  });
  el.showModal();
  stack.push(ctl);
  placeHost();
  // forms focus their first field; panels focus the dialog itself (Tab then walks into it)
  let f = o.focus && el.querySelector(o.focus);
  if (!f && o.form) f = focusables(el.querySelector('.dlg-body'))[0];
  if (!f) {
    el.tabIndex = -1;
    f = el;
  }
  f.focus({ preventScroll: true });

  function close() {
    if (ctl.closed) return;
    ctl.closed = true;
    for (const fn of ctl.onClose) {
      try { fn(); } catch (err) { console.error(err); }
    }
    stack.splice(stack.indexOf(ctl), 1);
    placeHost();
    el.classList.add('closing');
    later(() => {
      el.close();
      el.remove();
      if (opener && opener.isConnected && opener.focus) opener.focus({ preventScroll: true });
    }, 140);
  }
  return ctl;
}

// ---- menus: [{label, icon, run, disabled, danger, hint} | {sep: true} | falsy]
let menu = null;

function closeMenu(refocus) {
  if (!menu) return;
  const m = menu;
  menu = null;
  m.ac.abort();
  m.el.remove();
  m.anchor.setAttribute('aria-expanded', 'false');
  if (refocus) m.anchor.focus();
}

export function openMenu(anchor, items) {
  const again = menu && menu.anchor === anchor;
  closeMenu(again);
  if (again) return;
  const el = h('div', { class: 'menu', role: 'menu' });
  const btns = [];
  for (const it of items) {
    if (!it) continue;
    if (it.sep) {
      if (btns.length && el.lastChild.tagName === 'BUTTON') el.append(h('div', { class: 'menu-sep', role: 'separator' }));
      continue;
    }
    const b = btn('menu-item' + (it.danger ? ' danger' : ''), [it.icon && icon(it.icon), h('span', { text: it.label }), it.hint && h('span', { class: 'menu-hint', text: it.hint })],
      () => { if (!it.disabled) { closeMenu(true); it.run(); } }, { role: 'menuitem', tabindex: '-1', 'aria-disabled': it.disabled && 'true' });
    el.append(b);
    btns.push(b);
  }
  if (el.lastChild.tagName !== 'BUTTON') el.lastChild.remove();
  document.body.append(el);

  const r = anchor.getBoundingClientRect();
  let top = r.bottom + 6;
  if (top + el.offsetHeight > innerHeight - 8) {
    top = Math.max(8, r.top - el.offsetHeight - 6);
    el.classList.add('up');
  }
  el.style.left = Math.max(8, Math.min(r.right - el.offsetWidth, innerWidth - el.offsetWidth - 8)) + 'px';
  el.style.top = top + 'px';
  anchor.setAttribute('aria-expanded', 'true');

  el.addEventListener('keydown', (e) => {
    const k = btns.indexOf(document.activeElement);
    const to = { ArrowDown: k + 1, ArrowUp: k - 1, Home: 0, End: btns.length - 1 }[e.key];
    if (to != null) {
      e.preventDefault();
      btns[(to + btns.length) % btns.length].focus();
    } else if (e.key === 'Escape') {
      e.preventDefault();
      closeMenu(true);
    } else if (e.key === 'Tab') closeMenu(false);
  });
  const ac = new AbortController();
  const away = () => closeMenu(false);
  document.addEventListener('pointerdown', (e) => { if (!el.contains(e.target) && !anchor.contains(e.target)) away(); }, { capture: true, signal: ac.signal });
  // the window losing focus (not an element's blur: capture would see the button's own blur), resizing, scrolling anything
  addEventListener('blur', away, { signal: ac.signal });
  addEventListener('resize', away, { signal: ac.signal });
  addEventListener('scroll', away, { capture: true, signal: ac.signal });
  menu = { el, anchor, ac };
  (btns.find((b) => !b.hasAttribute('aria-disabled')) || btns[0]).focus({ preventScroll: true });
}

// ---- clipboard (127.0.0.1 is a secure context; the textarea path is for a denied permission)
export async function copyText(t) {
  try {
    await navigator.clipboard.writeText(t);
    return true;
  } catch {
    const ta = h('textarea', { class: 'sr-only', readonly: true });
    ta.value = t;
    document.body.append(ta);
    ta.select();
    let ok = false;
    try { ok = document.execCommand('copy'); } catch { /* no clipboard at all */ }
    ta.remove();
    return ok;
  }
}
