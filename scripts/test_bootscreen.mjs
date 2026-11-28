import assert from 'node:assert/strict';
import test from 'node:test';
import { bootFraction, bootStage } from '../internal/ui/web/js/bootscreen.js';

test('the bar is the setup percentage; before the setup says anything it is indeterminate (null)', () => {
  assert.equal(bootFraction(null), null);
  assert.equal(bootFraction({ running: false }), null);
  assert.equal(bootFraction({ running: true, booted: false }), null);
  assert.equal(bootFraction({ running: true, booted: true }), null);
  assert.equal(bootFraction({ running: true, booted: true, setup_step: 'boot', setup_progress: 0 }), null);
  assert.equal(bootFraction({ running: true, booted: true, setup_step: 'vulkan_fix', setup_progress: 0 }), 0);
  assert.ok(Math.abs(bootFraction({ running: true, booted: true, setup_step: 'tune', setup_progress: 40 }) - 0.4) < 1e-9);
  assert.equal(bootFraction({ running: true, booted: true, setup_step: 'sdcard', setup_progress: 500 }), 1);
  assert.equal(bootFraction({ running: true, booted: true, setup_step: 'ime', setup_progress: -5 }), 0);
});

test('the stage is what the overlay says', () => {
  assert.equal(bootStage({ running: false }), 'launch');
  assert.equal(bootStage({ running: true, booted: false }), 'android');
  assert.equal(bootStage({ running: true, booted: true, setup_step: 'ime' }), 'ime');
  assert.equal(bootStage({ running: true, booted: true }), 'boot');
});
