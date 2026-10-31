// Everything that changes something on the server, with the toasts that go with it.

import { api } from './api.js';
import { S, update, instance } from './store.js';
import { toast, toastError, copyText } from './ui.js';
import { quoted, dur } from './format.js';
import { openActivity } from './panels.js';

const VERB = { start: '开机', stop: '关机', restart: '重启', install: '安装 APK', create: '新建' };
const isBoot = (j) => j.kind === 'start' || j.kind === 'restart';
const activity = { label: '查看活动', run: () => openActivity() };
// wraps an API action: errors become a toast with the message and advice
const safe = (fn) => async (...a) => { try { return await fn(...a); } catch (e) { toastError(e); } };

// Job snapshots come from POST responses and from the event stream in either order: an older one never wins.
export function onJob(job) {
  if (!job || !job.id) return;
  const prev = S.jobs.get(job.id);
  if (prev && job.state === 'running' && (prev.state !== 'running' || (prev.progress && job.progress && job.progress.done < prev.progress.done))) return;
  S.jobs.set(job.id, job);
  if (job.state !== 'running' && S.watch.delete(job.id)) notify(job);
  if (S.jobs.size > 80) {
    const old = [...S.jobs.values()].filter((j) => j.state !== 'running').sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at));
    for (const j of old.slice(0, S.jobs.size - 80)) S.jobs.delete(j.id);
  }
  update();
}

function notify(job) {
  const who = quoted(instance(job.index), job.index);
  const r = job.result || {};
  const e = job.error || {};
  if (job.state === 'failed') toast({ tone: 'err', title: `${who}${VERB[job.kind] || ''}没有成功`, text: e.message, advice: e.advice, action: activity });
  else if (job.state === 'canceled') { if (isBoot(job)) toast({ tone: 'info', title: '已停止等待', text: `${who}的模拟器会继续开机。` }); }
  else if (isBoot(job)) {
    const w = Math.max((r.warnings || []).length, (job.warnings || []).length);
    toast({ tone: w ? 'warn' : 'ok', title: `${who}已开机`, action: w ? activity : null,
      text: [r.first_boot && '首次初始化已完成', r.total_seconds && `用时 ${dur(r.total_seconds)}`, w && `有 ${w} 个设置步骤没成功`].filter(Boolean).join('，') });
  } else if (job.kind === 'create') toast({ title: `${who}新建好了`, text: '初始化已做完，已关机；第一次开机和平常一样快。' });
  else if (job.kind === 'stop') toast({ title: `${who}已关机` });
  else if (job.kind === 'install') toast({ title: '安装完成', text: `${who}装好了 ${r.package || 'APK'}` });
}

async function run(i, kind, body) {
  S.pending[i] = kind;
  update();
  try {
    const job = await api.post(`/api/instances/${i}/${kind}`, body);
    const seen = S.jobs.get(job.id);
    if (seen && seen.state !== 'running') notify(seen); // the stream was faster than the response
    else {
      S.watch.add(job.id);
      onJob(job);
    }
    return job;
  } catch (e) {
    toastError(e);
  } finally {
    delete S.pending[i];
    update();
  }
}

export async function start(i, window = false) {
  if (await run(i, 'start', { window })) S.win[i] = window;
}
export const stop = (i, force = false) => run(i, 'stop', { force });
export const restart = (i) => run(i, 'restart', { window: !!S.win[i] });
// 重试初始化: the setup of a new instance that failed (its job is a create job)
export const resumeCreate = (i) => run(i, 'resume', {});

export async function install(i, file) {
  const fd = new FormData();
  fd.append('apk', file, file.name);
  if (await run(i, 'install', fd)) toast({ tone: 'info', title: '正在安装', text: `${file.name} → ${quoted(instance(i), i)}` });
}

export const cancelJob = safe(async (id) => onJob(await api.post(`/api/jobs/${encodeURIComponent(id)}/cancel`)));
export const openFolder = safe((what, index) => api.post('/api/open', index == null ? { what } : { what, index }));
export const input = safe((i, body) => api.post(`/api/instances/${i}/input`, body));

export const setWindow = safe(async (i, visible) => {
  const r = await api.post(`/api/instances/${i}/window`, { visible });
  S.win[i] = !!r.windows && r.visible;
  if (r.windows) toast({ title: r.visible ? '窗口已显示' : '窗口已隐藏' });
  else toast({ tone: 'info', title: '这台实例没有窗口', text: '它是无窗口开机的。看画面请用「画面」；要窗口，关机后选「带窗口开机」。' });
  update();
});

export async function copy(text) {
  if (!text) return;
  if (await copyText(text)) toast({ title: '已复制', text });
  else toast({ tone: 'err', title: '没能复制', text: '浏览器不让写剪贴板，请手动选中复制。' });
}

export async function loadInstances() {
  try {
    S.list = await api.get('/api/instances');
    S.loadError = null;
  } catch (e) {
    if (!S.list) S.loadError = e;
  }
  update();
}
