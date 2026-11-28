// The boot screen's numbers: the same rules as the overlay on the emulator's window (internal/emu overlay.go). The bar
// is the setup's own percentage (setup_progress of the instance) while the setup says what it is doing; before that
// nothing reliable is known (launch, Android booting) and the bar is indeterminate. Pure, so it can be tested without
// a page.

const clamp = (v, lo, hi) => Math.min(hi, Math.max(lo, v));

// bootStage: what is being done: launch, android, or the setup's step (setup_step, "boot" when it has not said).
export function bootStage(inst) {
  if (!inst || !inst.running) return 'launch';
  if (!inst.booted) return 'android';
  return inst.setup_step || 'boot';
}

// bootFraction: how full the bar is, 0 to 1, or null while there is no real percentage (the bar is indeterminate).
export function bootFraction(inst) {
  const stage = bootStage(inst);
  if (stage === 'launch' || stage === 'android' || stage === 'boot') return null;
  return clamp(Number(inst.setup_progress) || 0, 0, 100) / 100;
}
