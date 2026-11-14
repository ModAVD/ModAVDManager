// Run: node --test scripts/test_regional_ui.mjs
import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('../internal/ui/web/js/regional.js', import.meta.url), 'utf8');
const dom = `
const elements = new Map();
function h(tag, props, ...children) {
  const value = { tag, children: children.flat(), hidden: false, disabled: false, checked: false, listeners: {},
    append(...items) { this.children.push(...items); },
    addEventListener(kind, fn) { this.listeners[kind] = fn; }, ...props };
  if (value.id) elements.set(value.id, value);
  return value;
}
export { elements };
`;
const mod = await import(`data:text/javascript;base64,${Buffer.from(source.replace("import { h } from './dom.js';", dom)).toString('base64')}`);

test('opening legacy settings leaves both Android choices unchanged', () => {
  const fields = mod.regionalFields('legacy', { language: 'preserve', time_zone: 'preserve' });
  assert.deepEqual(fields.changes(), {});
  const zone = mod.elements.get('legacy-timezone');
  zone.value = 'fixed';
  zone.listeners.change();
  const input = mod.elements.get('legacy-timezone-value');
  assert.equal(input.hidden, false);
  assert.equal(input.disabled, false);
  input.value = 'Etc/UTC';
  assert.deepEqual(fields.changes(), { time_zone: 'Etc/UTC' });
});

test('new defaults follow host, and a repeated language choice needs an explicit action', () => {
  const created = mod.regionalFields('create', null, true);
  assert.deepEqual(created.values(), { language: 'host', time_zone: 'host' });
  const fields = mod.regionalFields('repeat', { language: 'host', time_zone: 'host', last_language: 'zh-CN' });
  assert.deepEqual(fields.changes(), {});
  mod.elements.get('repeat-language-repeat').checked = true;
  assert.deepEqual(fields.changes(), { language: 'host' });
  mod.elements.get('repeat-language').value = 'preserve';
  assert.deepEqual(fields.changes(), { language: 'preserve' });
});

test('a fixed language stays fixed and does not send an unrelated time-zone edit', () => {
  const fields = mod.regionalFields('fixed', { language: 'en-US', time_zone: 'Asia/Shanghai' });
  assert.deepEqual(fields.changes(), {});
  mod.elements.get('fixed-language-value').value = 'ja-JP';
  assert.deepEqual(fields.changes(), { language: 'ja-JP' });
});

test('the current guest display never uses a configured or last-applied value as a live fact', () => {
  assert.match(mod.guestRegionalText({ config: { language: 'en-US', time_zone: 'Etc/UTC' }, guest: null }), /^未读取/);
  assert.equal(mod.guestRegionalText({ guest: { language: 'ja-JP', time_zone: 'Asia/Tokyo' } }), 'ja-JP · Asia/Tokyo');
  assert.equal(mod.guestRegionalText({ guest: null, guest_error: 'adb timeout' }), '读取失败');
});
