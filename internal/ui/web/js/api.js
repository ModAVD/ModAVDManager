// The HTTP contract (internal/ui/API.md): JSON in and out, errors as {error:{code,message,advice}}.

export class ApiError extends Error {
  constructor(status, code, message, advice) {
    super(message);
    this.status = status;
    this.code = code;
    this.advice = advice || '';
  }
}

const RELOGIN = '关掉这个窗口，再双击 ModAVD 重新打开';

export async function request(method, path, body, opt = {}) {
  const init = { method, credentials: 'same-origin', cache: 'no-store', headers: {} };
  if (body instanceof FormData) init.body = body;
  else if (body !== undefined) {
    init.headers['Content-Type'] = 'application/json';
    init.body = JSON.stringify(body);
  }
  if (opt.signal) init.signal = opt.signal;
  if (opt.headers) Object.assign(init.headers, opt.headers);
  let res;
  try {
    res = await fetch(path, init);
  } catch (e) {
    // a caller's own abort or AbortSignal.timeout(): not "the server is gone"
    if (e && (e.name === 'AbortError' || e.name === 'TimeoutError')) throw e;
    throw new ApiError(0, 'network', '连不上 ModAVD 的后台服务', '后台可能已经退出。' + RELOGIN);
  }
  if (!res.ok) throw await toError(res);
  if (opt.raw) return res;
  const ct = res.headers.get('Content-Type') || '';
  if (ct.includes('json')) return res.json();
  return res.text();
}

async function toError(res) {
  if (res.status === 401) return new ApiError(401, 'unauthorized', '和后台的连接已失效', RELOGIN);
  let e = null;
  try { e = (await res.json()).error; } catch { /* not JSON */ }
  return new ApiError(res.status, (e && e.code) || 'http_' + res.status,
    (e && e.message) || `请求没有成功（HTTP ${res.status}）`, e && e.advice);
}

export const api = {
  get: (p, o) => request('GET', p, undefined, o),
  post: (p, b = {}, o) => request('POST', p, b, o),
  patch: (p, b) => request('PATCH', p, b),
  del: (p) => request('DELETE', p, {}), // non-GET bodies are JSON (API.md), even an empty one
};

// /api/events with reconnects. EventSource retries by itself after a dropped stream; when it gives up (CLOSED, e.g.
// after a 401) we open a new one with backoff and let the caller find out why. reconnect() starts over right away.
export function connectEvents(on) {
  let retry = 0;
  let es = null;
  let timer = 0;
  const open = () => {
    clearTimeout(timer);
    const me = (es = new window.EventSource('/api/events'));
    me.onopen = () => { retry = 0; on.open(); };
    me.onerror = () => {
      if (me !== es) return;
      if (me.readyState === 2) {
        me.close();
        timer = setTimeout(open, Math.min(10000, 1000 * 2 ** retry++));
        on.lost(true);
      } else on.lost(false);
    };
    for (const type of ['instances', 'job', 'log']) {
      me.addEventListener(type, (ev) => {
        if (me !== es) return;
        let d;
        try { d = JSON.parse(ev.data); } catch { return; }
        on[type](d);
      });
    }
  };
  open();
  return {
    reconnect() {
      if (es) es.close();
      retry = 0;
      open();
    },
  };
}
