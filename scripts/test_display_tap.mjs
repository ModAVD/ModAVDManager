import assert from 'node:assert/strict';
import test from 'node:test';
import { displayTap } from '../internal/ui/web/js/display.js';

const frame = { left: 10, top: 20, width: 320, height: 180 };

test('both presets use the current screenshot size', () => {
  assert.deepEqual(displayTap(frame, 1280, 720, 170, 110), { type: 'tap', x: 640, y: 360 });
  assert.deepEqual(displayTap(frame, 1920, 1080, 170, 110), { type: 'tap', x: 960, y: 540 });
  assert.deepEqual(displayTap(frame, 1920, 1080, 330, 200), { type: 'tap', x: 1919, y: 1079 });
});

test('portrait taps exclude the empty side bands', () => {
  assert.equal(displayTap(frame, 1080, 1920, 20, 110), null);
  assert.equal(displayTap(frame, 720, 1280, 320, 110), null);
  assert.deepEqual(displayTap(frame, 1080, 1920, 170, 110), { type: 'tap', x: 540, y: 960 });
  assert.deepEqual(displayTap(frame, 720, 1280, 170, 110), { type: 'tap', x: 360, y: 640 });
  assert.deepEqual(displayTap(frame, 1080, 1920, 119.375, 20), { type: 'tap', x: 0, y: 0 });
});

test('a tall frame excludes empty top and bottom bands', () => {
  const tall = { left: 0, top: 0, width: 180, height: 320 };
  assert.equal(displayTap(tall, 1920, 1080, 90, 10), null);
  assert.deepEqual(displayTap(tall, 1920, 1080, 90, 160), { type: 'tap', x: 960, y: 540 });
});

test('unloaded images and zero-size frames cannot send a tap', () => {
  assert.equal(displayTap(frame, 0, 0, 170, 110), null);
  assert.equal(displayTap({ ...frame, width: 0 }, 1280, 720, 170, 110), null);
  assert.equal(displayTap(frame, 1280, 720, 9, 110), null);
});
