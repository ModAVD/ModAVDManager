import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const source = await readFile(new URL('../internal/ui/web/js/format.js', import.meta.url), 'utf8');
const fmt = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`);
const { diskCompactText } = fmt;
const read = (name) => readFile(new URL(`../internal/ui/web/js/${name}`, import.meta.url), 'utf8');
const moduleBody = (s) => s.replace(/^import .*;\r?\n/gm, '').replace(/^export /gm, '');

test('compact text reports small growth without claiming a saving', () => {
  const text = diskCompactText({ before_on_disk_bytes: 2 ** 30, after_on_disk_bytes: 2 ** 30 + 1, freed_bytes: 0 });
  assert.match(text, /增加 1 B/);
  assert.doesNotMatch(text, /缩到|释放|省下/);
});
test('compact text reports shrinkage and unchanged allocation', () => {
  assert.match(diskCompactText({ before_on_disk_bytes: 2 ** 30, after_on_disk_bytes: 0, freed_bytes: 2 ** 30 }), /释放 1.0 GB/);
  assert.match(diskCompactText({ before_on_disk_bytes: 4096, after_on_disk_bytes: 4096, freed_bytes: 0 }), /释放 0 B/);
});
test('compact text uses signed delta when host sizes exceed exact JavaScript integers', () => {
  const text = diskCompactText({ before_on_disk_bytes: 2 ** 54, after_on_disk_bytes: 2 ** 54, freed_bytes: 0, on_disk_delta_bytes: 1 });
  assert.match(text, /增加 1 B/);
});
test('compact text distinguishes skip from rewrite', () => {
  const text = diskCompactText({ before_on_disk_bytes: 4096, after_on_disk_bytes: 4096, freed_bytes: 0, skipped: true, reclaim_threshold_bytes: 16 * 2 ** 20 });
  assert.match(text, /不足 16 MB，已跳过重写/);
  assert.match(text, /占用 4 KB/);
});

test('trim, fallback info, growth and skipped notifications coexist', async () => {
  const notices = [];
  const S = { jobs: new Map(), watch: new Set(), logs: [] };
  const scope = { ...fmt, S, instance: () => null, update() {}, toast: (r) => notices.push(r) };
  vm.runInNewContext(moduleBody(await read('actions.js')) + '\nglobalThis.accept = onJob;', scope);
  const send = (id, kind, result) => {
    S.watch.add(id);
    scope.accept({ id, index: 2, kind, state: 'done', result });
    return notices.at(-1);
  };
  const trim = send('trim', 'disk-trim', { discard_max_bytes: 2 ** 30 });
  assert.match(trim.title, /已触发/);
  assert.doesNotMatch(trim.title, /已回收|已完成/);
  assert.match(trim.text, /不等待安卓回收完成/);
  const info = send('info', 'disk-info', { logical_bytes: 64 * 2 ** 20, warnings: ['正在整理磁盘'] });
  assert.match(info.text, /正在整理磁盘/);
  assert.doesNotMatch(info.text, /宿主占用|开机时/);
  const growth = send('growth', 'disk-compact', { before_on_disk_bytes: 32 * 2 ** 20, after_on_disk_bytes: 33 * 2 ** 20, on_disk_delta_bytes: 2 ** 20, freed_bytes: 0 });
  assert.match(growth.text, /增加 1 MB/);
  assert.doesNotMatch(growth.text, /省下|释放/);
  const skip = send('skip', 'disk-compact', { before_on_disk_bytes: 4096, skipped: true, reclaim_threshold_bytes: 16 * 2 ** 20 });
  assert.match(skip.title, /已跳过磁盘重写/);
  assert.match(skip.text, /已跳过重写/);
  assert.equal(notices.length, 4);
  // An old progress snapshot must not replace a completed trim request.
  scope.accept({ id: 'trim', kind: 'disk-trim', state: 'running' });
  assert.equal(S.jobs.get('trim').state, 'done');
  assert.equal(notices.length, 4);
});

test('activity keeps trim request timing and compact cancellation distinct', async () => {
  const element = () => ({ hidden: false, style: {}, classList: { toggle() {} }, replaceChildren() {} });
  const row = () => Object.fromEntries(['title', 'meta', 'pill', 'prog', 'acts', 'cancel', 'cancelHint', 'why', 'err', 'warn', 'res', 'det', 'sum', 'msg', 'pctEl', 'bar', 'fill'].map((k) => [k, element()]));
  const scope = { ...fmt, S: { logs: [] }, instance: () => null, setText: (el, text) => { el.text = text; }, attr() {}, h: element, icon: element };
  vm.runInNewContext(moduleBody(await read('panels.js')) + '\nglobalThis.render = updateJob;', scope);
  const r = row();
  const common = { id: 'one', index: 2, created_at: '2026-11-05T00:00:00Z', finished_at: '2026-11-05T00:00:02Z' };
  scope.render(r, { ...common, kind: 'disk-trim', state: 'running', finished_at: undefined });
  assert.equal(r.pill.text, '进行中');
  assert.match(r.meta.text, /已进行/);
  assert.doesNotMatch(r.meta.text, /触发请求用时/);
  scope.render(r, { ...common, kind: 'disk-trim', state: 'done', result: {} });
  assert.equal(r.pill.text, '已触发');
  assert.match(r.meta.text, /触发请求用时/);
  assert.match(r.res.text, /不等待安卓回收完成/);
  scope.render(r, { ...common, kind: 'disk-compact', state: 'running', progress: { done: 1, total: 10 } });
  assert.equal(r.acts.hidden, false);
  assert.equal(r.cancel.text, '取消整理');
  assert.doesNotMatch(r.meta.text, /触发请求/);
  scope.render(r, { ...common, kind: 'disk-compact', state: 'done', result: { before_on_disk_bytes: 2 ** 54, after_on_disk_bytes: 2 ** 54, on_disk_delta_bytes: 1, freed_bytes: 0 } });
  assert.match(r.res.text, /增加 1 B/);
  assert.equal(r.acts.hidden, true);
  assert.equal(r.pill.text, '完成');
});
