import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('../internal/ui/web/js/format.js', import.meta.url), 'utf8');
const { diskCompactText } = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`);

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
