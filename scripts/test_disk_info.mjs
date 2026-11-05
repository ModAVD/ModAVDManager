import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('../internal/ui/web/js/format.js', import.meta.url), 'utf8');
const { diskInfoText } = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`);

test('busy info shows readable values and the actual reason', () => {
  const text = diskInfoText({ logical_bytes: 8 * 2 ** 30, warnings: ['正在整理磁盘'], needs_shutdown: true });
  assert.match(text, /逻辑大小 8.0 GB/);
  assert.match(text, /正在整理磁盘/);
  assert.doesNotMatch(text, /开机时|宿主占用/);
});

test('running info and damaged-image warnings remain visible', () => {
  assert.match(diskInfoText({ running: true, on_disk_bytes: 0 }), /宿主占用 0 B.*关机后再看/);
  assert.match(diskInfoText({ warnings: ['不会修复镜像'] }), /不会修复镜像/);
});
