// The page's own text (ModAVD/ModAVD#165). The code is written in Chinese and the Chinese text is the key: t('中文')
// returns the translation from GET /api/locale (the same text when there is none). The back end has already translated
// what it sends (errors, progress, logs, check items), so only the page's own strings go through here.
//
// This module loads the locale before anything else runs (top-level await), so t() can also be used when a module is
// evaluated. It uses plain fetch rather than api.js, which itself needs t() (a cycle).

let view = { language: 'zh-CN', preference: 'auto', system: 'zh-CN', languages: [{ code: 'zh-CN', name: '中文' }, { code: 'en', name: 'English' }], messages: {} }; // i18n: keep
let messages = {};

const adopt = (v) => {
  if (!v || typeof v !== 'object') return;
  view = { ...view, ...v };
  messages = v.messages && typeof v.messages === 'object' ? v.messages : {};
  document.documentElement.lang = lang();
};

// loadLocale reads GET /api/locale; on any failure the page stays in Chinese.
export async function loadLocale() {
  try {
    const res = await fetch('/api/locale', { credentials: 'same-origin', cache: 'no-store' });
    if (res.ok) adopt(await res.json());
  } catch { /* back end not reachable: Chinese */ }
  document.documentElement.lang = lang();
  return view;
}

// setPreference saves the choice ('auto' | 'zh-CN' | 'en') and switches the back end at once; the caller reloads.
export async function setPreference(preference) {
  const res = await fetch('/api/locale', {
    method: 'PUT', credentials: 'same-origin', cache: 'no-store',
    headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ preference }),
  });
  if (!res.ok) throw new Error('HTTP ' + res.status);
  adopt(await res.json());
  return view;
}

// t(key, vars): the translation of a Chinese key, {name} placeholders filled from vars (in the translation and in the
// key itself when there is no translation). The first argument must be a plain string literal (the catalog test reads it).
export function t(key, vars) {
  let s = messages[key];
  if (typeof s !== 'string' || s === '') s = key;
  if (!vars) return s;
  return s.replace(/\{(\w+)\}/g, (m, k) => (Object.prototype.hasOwnProperty.call(vars, k) ? String(vars[k]) : m));
}

// Punctuation that glues translated pieces: the catalog only holds keys with Chinese characters in them, so the glue
// is chosen here, by language, instead of through t().
const en = () => lang() === 'en';
export const comma = () => (en() ? ', ' : '，');
export const semi = () => (en() ? '; ' : '；');
export const stop = () => (en() ? '.' : '。');
export const enumComma = () => (en() ? ', ' : '、');
export const colon = () => (en() ? ': ' : '：');
// cap raises the first letter of a translation written in lower case to be used inside sentences
export const cap = (s) => (en() && s ? s.trim().charAt(0).toUpperCase() + s.trim().slice(1) : s);
export const gap = () => (en() ? ' ' : ''); // between two sentences
export const quote = (s) => (en() ? `“${s}”` : `「${s}」`);
export const paren = (s) => (en() ? ` (${s})` : `（${s}）`);

// tNode(text, nodes): text is an already translated string (t('… {who} …')) whose {name} placeholders are replaced by
// DOM nodes (or strings): the array to pass as an element's children, so a name can be bold inside a sentence.
export function tNode(text, nodes) {
  const out = [];
  let last = 0;
  text.replace(/\{(\w+)\}/g, (m, k, at) => {
    if (!Object.prototype.hasOwnProperty.call(nodes, k)) return m;
    if (at > last) out.push(text.slice(last, at));
    out.push(nodes[k]);
    last = at + m.length;
    return m;
  });
  if (last < text.length) out.push(text.slice(last));
  return out;
}

// lang is the language the page shows: 'zh-CN' or 'en' (also what Intl and toLocale*String take).
export function lang() {
  return view.language === 'en' ? 'en' : 'zh-CN';
}

export function localeView() {
  return view;
}

// translateStatic translates index.html: the text of elements marked data-t, and the title / placeholder / aria-label /
// alt attributes that hold Chinese.
export function translateStatic(root = document) {
  if (lang() === 'zh-CN') return;
  for (const el of root.querySelectorAll('[data-t]')) {
    const key = el.textContent.trim();
    if (key) el.textContent = t(key);
  }
  for (const a of ['title', 'placeholder', 'aria-label', 'alt']) {
    for (const el of root.querySelectorAll(`[${a}]`)) {
      const v = el.getAttribute(a);
      if (/[一-鿿]/.test(v)) el.setAttribute(a, t(v));
    }
  }
}

// ---- dev only: ?mock=… installs the fake back end before the locale is read (app.js used to do it after the imports)
{
  const params = new URLSearchParams(location.search);
  if (params.get('mock')) {
    try {
      (await import('/dev/mock.js')).install(params.get('mock'), params);
    } catch (e) {
      console.error('mock backend unavailable', e);
    }
  }
}
await loadLocale();
