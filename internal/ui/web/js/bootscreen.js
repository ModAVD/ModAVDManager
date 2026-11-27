// The boot screen's numbers, the same scale as the overlay on the emulator's window (internal/emu overlay.go): the bar
// is the launch, then Android's boot (only time is known to the page, the overlay sees adb answer), then the setup's own
// percentage (setup_progress of the instance). Pure, so it can be tested without a page.

const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

// bootFraction: how full the bar is, 0 to 1, for an instance (the fields of the instances API) at now (ms).
export function bootFraction(inst, now = Date.now()) {
  if (!inst || !inst.running) return 0.03;
  if (!inst.booted) {
    const secs = inst.started_at_ms ? Math.max(0, (now - inst.started_at_ms) / 1000) : 0;
    return 0.08 + 0.6 * (1 - 1 / (1 + secs / 18));
  }
  return 0.7 + 0.3 * clamp(Number(inst.setup_progress) || 0, 0, 100) / 100;
}

// bootStage: what is being done: launch, android, or the setup's step (setup_step, "boot" when it has not said).
export function bootStage(inst) {
  if (!inst || !inst.running) return 'launch';
  if (!inst.booted) return 'android';
  return inst.setup_step || 'boot';
}
