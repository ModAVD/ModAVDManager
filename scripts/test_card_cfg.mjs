import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';

const source = await readFile(new URL('../internal/ui/web/js/format.js', import.meta.url), 'utf8');
const { cfgText } = await import(`data:text/javascript;base64,${Buffer.from(source).toString('base64')}`);

const cfg = { memory_mb: 4096, cpu: 4, fps: 30, data_disk_mode: 'auto', data_partition_gb: 256 };

test('the card shows what the data disk takes on the computer', () => {
  assert.equal(cfgText(cfg, { host_bytes: 12.34 * 2 ** 30 }), '内存 4 GB · 4 核 · 30 Hz · 占 12.3 GB');
  assert.equal(cfgText({ ...cfg, data_disk_mode: 'manual', data_partition_gb: 64 }, { host_bytes: 512 * 2 ** 20 }),
    '内存 4 GB · 4 核 · 30 Hz · 盘 64 GB · 占 512 MB');
});

test('an unknown size is left out, not shown as a dash', () => {
  assert.equal(cfgText(cfg, { host_bytes: -1 }), '内存 4 GB · 4 核 · 30 Hz');
  assert.equal(cfgText(cfg, null), '内存 4 GB · 4 核 · 30 Hz');
  assert.equal(cfgText(cfg), '内存 4 GB · 4 核 · 30 Hz');
});
