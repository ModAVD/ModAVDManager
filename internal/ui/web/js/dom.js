// DOM helpers and the icon set. Icons: 24px grid, stroke 1.75 (set in CSS), round caps and joins.

const SVGNS = 'http://www.w3.org/2000/svg';

// h('div', {class, text, onclick, ...attrs}, ...children). Never sets style: the page runs under style-src 'self'.
export function h(tag, props, ...kids) {
  const el = document.createElement(tag);
  if (props) {
    for (const k in props) {
      const v = props[k];
      if (v == null || v === false) continue;
      if (k === 'class') el.className = v;
      else if (k === 'text') el.textContent = v;
      else if (k === 'value') el.value = v;
      else if (k === 'checked') el.checked = !!v;
      else if (k[0] === 'o' && k[1] === 'n' && typeof v === 'function') el.addEventListener(k.slice(2), v);
      else el.setAttribute(k, v === true ? '' : v);
    }
  }
  return add(el, kids);
}

export function add(el, kids) {
  for (const c of kids) {
    if (c == null || c === false || c === '' || c === 0) continue;
    if (Array.isArray(c)) add(el, c);
    else el.append(c);
  }
  return el;
}

export const btn = (cls, kids, onclick, more) => h('button', { class: cls, type: 'button', onclick, ...more }, kids);
export const ibtn = (name, lbl, onclick, cls = 'icon-btn sm') =>
  h('button', { class: cls, type: 'button', title: lbl, 'aria-label': lbl, onclick }, icon(name));

// Keyed list: rows is Map(key -> {el}); make(item) builds a row once, upd(row, item) patches it every time.
export function keyed(parent, rows, items, key, make, upd) {
  const seen = new Set();
  let prev = null;
  for (const it of items) {
    const k = key(it);
    let r = rows.get(k);
    if (!r) rows.set(k, (r = make(it)));
    const want = prev ? prev.el.nextSibling : parent.firstChild;
    if (want !== r.el) parent.insertBefore(r.el, want);
    upd(r, it);
    seen.add(k);
    prev = r;
  }
  for (const [k, r] of rows) {
    if (seen.has(k)) continue;
    if (r.gone) r.gone();
    r.el.remove();
    rows.delete(k);
  }
}

// The error block of a dialog: message, then the advice.
export function showError(box, e) {
  box.replaceChildren(icon('bad'), h('div', null, h('strong', { text: e.message }), e.advice && h('div', { class: 'fe-advice', text: e.advice })));
  box.hidden = false;
}

export function setText(el, t) {
  t = t == null ? '' : String(t);
  if (el.textContent !== t) el.textContent = t;
}

export function attr(el, k, v) {
  if (v == null || v === false) { if (el.hasAttribute(k)) el.removeAttribute(k); return; }
  v = String(v); // true -> "true" (aria-* attributes want the word)
  if (el.getAttribute(k) !== v) el.setAttribute(k, v);
}

const dot = (x) => `<circle cx="${x}" cy="12" r="1.5" fill="currentColor" stroke="none"/>`;

const I = {
  power: '<path d="M12 3.75v7.5"/><path d="M7.1 6.6a7.25 7.25 0 1 0 9.8 0"/>',
  plus: '<path d="M12 5.5v13M5.5 12h13"/>',
  more: dot(5.5) + dot(12) + dot(18.5),
  copy: '<rect x="8.75" y="8.75" width="11" height="11" rx="2.5"/><path d="M15.25 8.75V7A2.25 2.25 0 0 0 13 4.75H7A2.25 2.25 0 0 0 4.75 7v6A2.25 2.25 0 0 0 7 15.25h1.75"/>',
  x: '<path d="M6.75 6.75l10.5 10.5M17.25 6.75L6.75 17.25"/>',
  folder: '<path d="M3.75 7.5A2.25 2.25 0 0 1 6 5.25h3.1c.6 0 1.17.27 1.55.73l1.1 1.27H18a2.25 2.25 0 0 1 2.25 2.25v7.25A2.25 2.25 0 0 1 18 19H6a2.25 2.25 0 0 1-2.25-2.25z"/>',
  screen: '<rect x="3.25" y="4.75" width="17.5" height="11.5" rx="2.25"/><path d="M9 19.5h6M12 16.25v3.25"/>',
  shield: '<path d="M12 3.5l6.75 2.6v5.15c0 4.2-2.8 7.55-6.75 9.25-3.95-1.7-6.75-5.05-6.75-9.25V6.1z"/><path d="M9.2 12.1l1.9 1.9 3.8-3.9"/>',
  pulse: '<path d="M3.5 12h3.6l2.4-5.5 5 11 2.4-5.5h3.6"/>',
  refresh: '<path d="M19.25 12a7.25 7.25 0 1 1-2.12-5.13"/><path d="M19.25 4.75v3.5h-3.5"/>',
  window: '<rect x="3.75" y="4.75" width="16.5" height="14.5" rx="2.25"/><path d="M3.75 9h16.5"/>',
  eye: '<path d="M2.75 12S6.15 5.75 12 5.75 21.25 12 21.25 12 17.85 18.25 12 18.25 2.75 12 2.75 12z"/><circle cx="12" cy="12" r="2.75"/>',
  eyeOff: '<path d="M9.9 6a9.6 9.6 0 0 1 2.1-.25c5.85 0 9.25 6.25 9.25 6.25a15.6 15.6 0 0 1-2.35 3.05M6.55 7.1C4.1 8.85 2.75 12 2.75 12S6.15 18.25 12 18.25a8.6 8.6 0 0 0 4.2-1.1"/><path d="M4.25 4.25l15.5 15.5"/><path d="M10.05 10.1a2.75 2.75 0 0 0 3.85 3.85"/>',
  download: '<path d="M12 4.25v10.5"/><path d="M7.75 10.75L12 15l4.25-4.25"/><path d="M5 19.25h14"/>',
  file: '<path d="M6.75 3.75h6.5l4 4v10.5a2 2 0 0 1-2 2h-8.5a2 2 0 0 1-2-2V5.75a2 2 0 0 1 2-2z"/><path d="M13 3.75V8h4.25M8.75 12.25h6.5M8.75 15.75h4.5"/>',
  sliders: '<path d="M4.5 7.5h8M17.5 7.5h2M4.5 16.5h2M11.5 16.5h8"/><circle cx="15" cy="7.5" r="2.25"/><circle cx="9" cy="16.5" r="2.25"/>',
  trash: '<path d="M4.75 7h14.5M10 11v5.5M14 11v5.5"/><path d="M6.5 7l.75 11a2 2 0 0 0 2 1.75h5.5a2 2 0 0 0 2-1.75l.75-11"/><path d="M9.25 7V5.75a1.5 1.5 0 0 1 1.5-1.5h2.5a1.5 1.5 0 0 1 1.5 1.5V7"/>',
  bolt: '<path d="M13.25 3.5L5.75 13.25h5.5l-.5 7.25 7.5-9.75h-5.5z"/>',
  back: '<path d="M16.75 5.5v13L6.25 12z"/>',
  home: '<circle cx="12" cy="12" r="6.25"/>',
  recents: '<rect x="6" y="6" width="12" height="12" rx="2"/>',
  alert: '<path d="M10.3 4.9a2 2 0 0 1 3.4 0l7 12.1a2 2 0 0 1-1.7 3H5a2 2 0 0 1-1.7-3z"/><path d="M12 9.75v4M12 16.75h.01"/>',
  info: '<circle cx="12" cy="12" r="8.25"/><path d="M12 11v5M12 7.9h.01"/>',
  ok: '<circle cx="12" cy="12" r="8.25"/><path d="M8.6 12.3l2.3 2.3 4.5-4.7"/>',
  bad: '<circle cx="12" cy="12" r="8.25"/><path d="M9.3 9.3l5.4 5.4M14.7 9.3l-5.4 5.4"/>',
  unknown: '<circle cx="12" cy="12" r="8.25"/><path d="M9.75 9.6a2.3 2.3 0 0 1 4.45.8c0 1.55-2.2 1.9-2.2 3.3M12 16.6h.01"/>',
  chevron: '<path d="M8.5 10l3.5 3.5 3.5-3.5"/>',
  sun: '<circle cx="12" cy="12" r="3.75"/><path d="M12 3v1.75M12 19.25V21M5.64 5.64l1.24 1.24M17.12 17.12l1.24 1.24M3 12h1.75M19.25 12H21M5.64 18.36l1.24-1.24M17.12 6.88l1.24-1.24"/>',
  moon: '<path d="M19.5 14.6A7.75 7.75 0 0 1 9.4 4.5a7.75 7.75 0 1 0 10.1 10.1z"/>',
  auto: '<circle cx="12" cy="12" r="8.25"/><path d="M12 3.75a8.25 8.25 0 0 1 0 16.5z" fill="currentColor" stroke="none"/>',
  bulb: '<path d="M9.5 17.5h5M10.25 20.25h3.5"/><path d="M12 3.75a5.75 5.75 0 0 0-3.4 10.4c.55.4.9 1.05.9 1.75v.1h5v-.1c0-.7.35-1.35.9-1.75A5.75 5.75 0 0 0 12 3.75z"/>',
  stop: '<rect x="6.5" y="6.5" width="11" height="11" rx="2.5"/>',
  cards: '<rect x="4.25" y="4.75" width="6.5" height="6.25" rx="1.75"/><rect x="13.25" y="4.75" width="6.5" height="6.25" rx="1.75"/><rect x="4.25" y="13" width="6.5" height="6.25" rx="1.75"/><rect x="13.25" y="13" width="6.5" height="6.25" rx="1.75"/>',
  rows: '<path d="M9.75 6.75h9.5M9.75 12h9.5M9.75 17.25h9.5"/><circle cx="5.5" cy="6.75" r="1.25" fill="currentColor" stroke="none"/><circle cx="5.5" cy="12" r="1.25" fill="currentColor" stroke="none"/><circle cx="5.5" cy="17.25" r="1.25" fill="currentColor" stroke="none"/>',
};

export function icon(name, cls) {
  const s = document.createElementNS(SVGNS, 'svg');
  s.setAttribute('viewBox', '0 0 24 24');
  s.setAttribute('class', cls ? 'ic ' + cls : 'ic');
  s.setAttribute('aria-hidden', 'true');
  s.setAttribute('focusable', 'false');
  s.innerHTML = I[name] || '';
  return s;
}

// Replace every <span data-icon="name"> in root with the icon.
export function hydrateIcons(root) {
  for (const el of root.querySelectorAll('[data-icon]')) el.replaceWith(icon(el.dataset.icon));
}
