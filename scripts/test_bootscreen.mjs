import assert from 'node:assert/strict';
import test from 'node:test';
import { bootFraction, bootStage } from '../internal/ui/web/js/bootscreen.js';

const at = 1_000_000;

test('the bar follows the launch, the time Android takes, then the setup percentage', () => {
  assert.equal(bootFraction(null), 0.03);
  assert.equal(bootFraction({ running: false }), 0.03);
  const booting = { running: true, booted: false, started_at_ms: at };
  const early = bootFraction(booting, at + 2000);
  const later = bootFraction(booting, at + 30000);
  assert.ok(early > 0.08 && later > early && later < 0.7, `${early} ${later}`);
  assert.equal(bootFraction({ ...booting, started_at_ms: 0 }, at), 0.08);
  assert.equal(bootFraction({ running: true, booted: true, setup_progress: 0 }), 0.7);
  assert.ok(Math.abs(bootFraction({ running: true, booted: true, setup_progress: 50 }) - 0.85) < 1e-9);
  assert.equal(bootFraction({ running: true, booted: true, setup_progress: 100 }), 1);
  assert.equal(bootFraction({ running: true, booted: true, setup_progress: 500 }), 1);
  assert.equal(bootFraction({ running: true, booted: true, setup_progress: -5 }), 0.7);
});

test('the bar never goes back across the stages', () => {
  const steps = [
    bootFraction({ running: true, booted: false, started_at_ms: at }, at + 120000),
    bootFraction({ running: true, booted: true, setup_progress: 0 }),
    bootFraction({ running: true, booted: true, setup_progress: 60 }),
  ];
  assert.deepEqual([...steps].sort((a, b) => a - b), steps);
});

test('the stage is what the overlay says', () => {
  assert.equal(bootStage({ running: false }), 'launch');
  assert.equal(bootStage({ running: true, booted: false }), 'android');
  assert.equal(bootStage({ running: true, booted: true, setup_step: 'ime' }), 'ime');
  assert.equal(bootStage({ running: true, booted: true }), 'boot');
});
