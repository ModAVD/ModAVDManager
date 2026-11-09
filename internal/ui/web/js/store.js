// The one state object and a render scheduler: update() batches every change into one frame.

export const S = {
  info: null, // GET /api/info
  list: null, // ListResult; null until the first load
  loadError: null, // ApiError when the first load failed
  jobs: new Map(), // id -> Job
  watch: new Set(), // job ids started from this window (toast when they finish)
  logs: [], // {time, level, msg, job_id}
  check: null, // CheckResult
  checkIndex: undefined, // undefined: first instance; null: new-instance defaults; number: selected instance
  checkTargetKey: '',
  checking: false,
  checkError: null,
  checkedAt: 0,
  update: null, // GET /api/update and the "update" event
  components: null, // GET /api/components
  componentsError: null,
  conn: 'connecting', // connecting | open | lost | auth
  lostAt: 0, // when the event stream went down (0 while it is up)
  pending: {}, // index -> kind while the POST is in flight
  // A settings response can race the instances poll that it triggers. Keep the
  // response's config until a later event contains the same values, so an old
  // poll cannot make a card (or the check target) jump back temporarily.
  pendingInstances: new Map(), // index -> {title, diagnostics, config}
  dismissed: new Set(), // failed job ids the user closed on the card
  proxyMarks: {}, // index -> {index, tone, text}: the 代理 mark of a running instance (the proxy event)
};

const subs = new Set();
let queued = false;

export function update() {
  if (queued) return;
  queued = true;
  requestAnimationFrame(() => {
    queued = false;
    for (const f of [...subs]) {
      try { f(S); } catch (e) { console.error(e); }
    }
  });
}

export function subscribe(f) {
  subs.add(f);
  return () => subs.delete(f);
}

export function instance(i) {
  return (S.list && S.list.instances.find((x) => x.index === i)) || null;
}

function pendingInstance(inst, changed) {
  const pending = {};
  if (changed && Object.prototype.hasOwnProperty.call(changed, 'title')) pending.title = inst.title;
  if (changed && Object.prototype.hasOwnProperty.call(changed, 'diagnostics')) pending.diagnostics = inst.diagnostics;
  const config = {};
  for (const key of ['memory_mb', 'cpu', 'fps', 'resolution']) {
    if (changed && Object.prototype.hasOwnProperty.call(changed, key)) config[key] = (inst.config || {})[key];
  }
  if (Object.keys(config).length) pending.config = config;
  return pending;
}

function pendingMatches(inst, pending) {
  if (pending.title !== undefined && inst.title !== pending.title) return false;
  if (pending.diagnostics !== undefined && inst.diagnostics !== pending.diagnostics) return false;
  for (const [key, value] of Object.entries(pending.config || {})) {
    if ((inst.config || {})[key] !== value) return false;
  }
  return true;
}

// Record the complete instance returned by a successful settings PATCH and
// update the card immediately. The following instances event may still be an
// older poll, so setInstances will merge the saved fields into that event.
export function rememberPatchedInstance(inst, changed) {
  if (!inst || !Number.isInteger(inst.index)) return;
  S.pendingInstances.set(inst.index, pendingInstance(inst, changed));
  if (!S.list) return;
  const k = S.list.instances.findIndex((x) => x.index === inst.index);
  if (k < 0) return;
  S.list.instances[k] = { ...S.list.instances[k], ...inst,
    config: { ...(S.list.instances[k].config || {}), ...(inst.config || {}) } };
}

// Accept a complete list from the initial load or event stream, preserving a
// just-saved instance until the server publishes matching data.
export function setInstances(list) {
  const incoming = list && Array.isArray(list.instances) ? list.instances : [];
  const present = new Set(incoming.map((x) => x.index));
  for (const index of S.pendingInstances.keys()) if (!present.has(index)) S.pendingInstances.delete(index);
  const instances = incoming.map((inst) => {
    const pending = S.pendingInstances.get(inst.index);
    if (!pending) return inst;
    if (pendingMatches(inst, pending)) {
      S.pendingInstances.delete(inst.index);
      return inst;
    }
    return { ...inst,
      title: pending.title,
      diagnostics: pending.diagnostics,
      config: { ...(inst.config || {}), ...(pending.config || {}) } };
  });
  S.list = { ...list, instances };
}

const jobNum = (j) => parseInt(String(j.id).replace(/\D/g, ''), 10) || 0;

export function newestFirst(a, b) {
  return (Date.parse(b.created_at) || 0) - (Date.parse(a.created_at) || 0) || jobNum(b) - jobNum(a);
}

export function jobsSorted() {
  return [...S.jobs.values()].sort(newestFirst);
}

export function lastJob(i, running) {
  let best = null;
  for (const j of S.jobs.values()) {
    if (j.index !== i || (running && j.state !== 'running')) continue;
    if (!best || newestFirst(j, best) < 0) best = j;
  }
  return best;
}

// Memory belongs to one boot's configuration and is checked freshly by Start.
// Only common host/SDK failures may disable every instance's start button.
export function rootBlocked() {
  const c = S.check;
  if (!c) return false;
  if (typeof c.root_ready === 'boolean') return !c.root_ready;
  const items = c.items || [];
  return c.ready === false && (!items.length || items.some((x) => x.id !== 'memory' && x.blocking && x.ok !== true));
}

export function checkTarget() {
  const instances = S.list?.instances || [];
  const selected = S.checkIndex === null ? null : instance(S.checkIndex) || [...instances].sort((a, b) => a.index - b.index)[0];
  return { index: selected?.index ?? null, memoryMB: selected?.config?.memory_mb || 6144, instance: selected };
}

export function runningCount() {
  let n = 0;
  for (const j of S.jobs.values()) if (j.state === 'running') n++;
  return n;
}
