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
