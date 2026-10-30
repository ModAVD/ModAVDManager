// The one state object and a render scheduler: update() batches every change into one frame.

export const S = {
  info: null, // GET /api/info
  list: null, // ListResult; null until the first load
  loadError: null, // ApiError when the first load failed
  jobs: new Map(), // id -> Job
  watch: new Set(), // job ids started from this window (toast when they finish)
  logs: [], // {time, level, msg, job_id}
  check: null, // CheckResult
  checking: false,
  checkError: null,
  checkedAt: 0,
  conn: 'connecting', // connecting | open | lost | auth
  lostAt: 0, // when the event stream went down (0 while it is up)
  pending: {}, // index -> kind while the POST is in flight
  win: {}, // index -> window visible, as last told by the server
  dismissed: new Set(), // failed job ids the user closed on the card
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

// The root cannot boot right now (the banner says why); 开机 / 重启 would only fail.
export const rootBlocked = () => !!S.check && S.check.ready === false;

export function runningCount() {
  let n = 0;
  for (const j of S.jobs.values()) if (j.state === 'running') n++;
  return n;
}
