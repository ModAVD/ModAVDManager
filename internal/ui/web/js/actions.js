// Everything that changes something on the server, with the toasts that go with it.

import { api } from './api.js';
import { S, update, instance, setInstances } from './store.js';
import { toast, toastError, copyText } from './ui.js';
import { quoted, dur, diskInfoText, diskTrimText, diskCompactText, playStoreText } from './format.js';
import { openActivity, runCheck } from './panels.js';

const VERB = { start: '开机', stop: '关机', install: '安装 APK', create: '新建', 'disk-info': '查看磁盘占用', 'disk-trim': '回收磁盘空间', 'disk-compact': '整理磁盘',
  components: '补全组件', 'collect-info': '收集问题信息', 'playstore-install': '补全 Google Play 商店', 'playstore-uninstall': '移除 Google Play 商店',
  'playstore-import': '导入 Google Play 商店的文件' };
// what the store needs once it is in an instance: registering its GSF ID (playstore.js openGSFID, the page is
// playstore.js UNCERTIFIED; not imported from there, as playstore.js imports this module)
const GSF = '登录时提示设备未经 Play 保护机制认证的，在实例的「更多操作」里点「复制 GSF ID」，到 https://www.google.com/android/uncertified/ 登记。';
// an import the server refused (components_rejected, details.reason) when its message has no advice of its own
const IMPORT_ADVICE = {
  unknown_file: '要的是与当前平台匹配的 Google 官方 Google Play 版镜像压缩包，或者从它里面取出的五个文件。',
  mismatch: '这个文件和 Google 发布的那份不一样（可能没下载完整或被改过），重新下载后再试。',
  incomplete: '五个文件要一起选上，或者只选镜像压缩包。',
};

export function importAdvice(e) {
  if (e && !e.advice && e.code === 'components_rejected' && e.details?.reason === 'unknown_file' && S.components?.play_store?.archive) {
    return `要的是 Google 官方的 Google Play 版镜像压缩包 ${S.components.play_store.archive}，或者从它里面取出的五个文件。`;
  }
  return (e && (e.advice || (e.code === 'components_rejected' && IMPORT_ADVICE[e.details?.reason]))) || '';
}

// The root's components changed (a fetch or an import finished): 组件 and the Google Play Store state read again.
function reloadComponents() {
  api.get('/api/components').then((v) => { S.components = v; S.componentsError = null; update(); }).catch(() => {});
}
const isBoot = (j) => j.kind === 'start';
const activity = { label: '查看活动', run: () => openActivity() };
// wraps an API action: errors become a toast with the message and advice
const safe = (fn) => async (...a) => { try { return await fn(...a); } catch (e) { toastError(e); } };

// Job snapshots come from POST responses and from the event stream in either order: an older one never wins.
export function onJob(job) {
  if (!job || !job.id) return;
  const prev = S.jobs.get(job.id);
  if (prev && job.state === 'running' && (prev.state !== 'running' || (prev.progress && job.progress && job.progress.done < prev.progress.done))) return;
  S.jobs.set(job.id, job);
  // components were just installed: 电脑检查 and the banner read the root again (the server dropped its cached check)
  if (job.kind === 'components' && job.state === 'done' && prev && prev.state === 'running') runCheck(true);
  if ((job.kind === 'components' || job.kind === 'playstore-import') && job.state !== 'running' && prev && prev.state === 'running') reloadComponents();
  if (job.state !== 'running' && S.watch.delete(job.id)) notify(job);
  if (S.jobs.size > 80) {
    const old = [...S.jobs.values()].filter((j) => j.state !== 'running').sort((a, b) => Date.parse(a.created_at) - Date.parse(b.created_at));
    for (const j of old.slice(0, S.jobs.size - 80)) S.jobs.delete(j.id);
  }
  update();
}

export function notifyJob(job) { notify(job); }

function notify(job) {
  // components / collect-info belong to the root (index -1), not to an instance
  const who = job.index < 0 ? '' : quoted(instance(job.index), job.index);
  const r = job.result || {};
  const e = job.error || {};
  if (job.state === 'failed') toast({ tone: 'err', title: `${who}${VERB[job.kind] || ''}没有成功`, text: e.message, advice: job.kind === 'playstore-import' ? importAdvice(e) : e.advice, action: activity });
  else if (job.state === 'canceled') { if (isBoot(job)) toast({ tone: 'info', title: '已停止等待', text: `${who}的模拟器会继续开机。` }); }
  else if (isBoot(job)) {
    const w = Math.max((r.warnings || []).length, (job.warnings || []).length);
    toast({ tone: w ? 'warn' : 'ok', title: `${who}已开机`, action: w ? activity : null,
      text: [r.first_boot && '首次初始化已完成', r.total_seconds && `用时 ${dur(r.total_seconds)}`, w && `有 ${w} 个设置步骤没成功`].filter(Boolean).join('，') });
  } else if (job.kind === 'components') toast({ title: '组件补全完成', text: '已重新检查这台电脑。' });
  else if (job.kind === 'collect-info') toast({ title: '问题信息已收集', text: r.zip_path ? `问题包：${r.zip_path}` : '', timeout: 9000 });
  else if (job.kind === 'create') toast({ title: `${who}新建好了`, text: '初始化已做完，已关机；第一次开机和平常一样快。' });
  else if (job.kind === 'stop') toast({ title: `${who}已关机` });
  else if (job.kind === 'install') toast({ title: '安装完成', text: `${who}装好了 ${r.package || 'APK'}` });
  // the sizes are the answer, so they stay long enough to read
  else if (job.kind === 'disk-info') toast({ tone: 'info', title: `${who}的磁盘占用`, text: diskInfoText(r), timeout: 9000 });
  else if (job.kind === 'disk-trim') toast({ title: `${who}已触发磁盘回收`, text: diskTrimText(), timeout: 6000 });
  else if (job.kind === 'disk-compact') toast({ title: r.skipped ? `${who}已跳过磁盘重写` : `${who}磁盘整理好了`, text: diskCompactText(r), timeout: 6000 });
  else if (job.kind === 'playstore-install') {
    toast({ tone: r.installed ? 'ok' : 'warn', title: r.installed ? `${who}已补全 Google Play 商店` : `${who}的 Google Play 商店没装全`,
      text: playStoreText(job.kind, r) + (r.installed ? GSF : ''), action: r.installed ? null : activity, timeout: 12000 });
  } else if (job.kind === 'playstore-uninstall') {
    toast({ tone: r.state === 'absent' ? 'ok' : 'warn', title: r.state === 'absent' ? `${who}已移除 Google Play 商店` : `${who}的 Google Play 商店没移除干净`,
      text: playStoreText(job.kind, r), action: r.state === 'absent' ? null : activity, timeout: 6000 });
  } else if (job.kind === 'playstore-import') {
    toast({ title: 'Google Play 商店的文件已就绪', text: `${playStoreText(job.kind, r)}要用的实例在「更多操作」里补全商店。`, timeout: 9000 });
  }
}

// onErr: what to do when the server refuses at once (default: a toast with its message and advice)
async function run(i, kind, body, onErr = toastError) {
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
    onErr(e);
  } finally {
    delete S.pending[i];
    update();
  }
}

// the manager boots with the window only (the server makes it so); there is no restart and no headless boot here
export const start = (i) => run(i, 'start', {});
export const stop = (i, force = false) => run(i, 'stop', { force });
export const cancelCreate = (i) => run(i, 'stop', { cancel_create: true });
// 重试初始化: the setup of a new instance that failed (its job is a create job)
export const resumeCreate = (i) => run(i, 'resume', {});

export async function install(i, file) {
  const fd = new FormData();
  fd.append('apk', file, file.name);
  if (await run(i, 'install', fd)) toast({ tone: 'info', title: '正在安装', text: `${file.name} → ${quoted(instance(i), i)}` });
}

// 磁盘占用 reads only and works on or off; 回收磁盘空间 needs the instance booted; 整理磁盘 (copies the whole data disk, minutes) needs it off
export const diskInfo = (i) => run(i, 'disk-info', {});
export const diskTrim = (i) => run(i, 'disk-trim', {});
export async function diskCompact(i) {
  if (await run(i, 'disk-compact', {})) toast({ tone: 'info', title: '正在整理磁盘', text: `${quoted(instance(i), i)}正在检查并整理，期间不能开机`, timeout: 5000 });
}

// 补全 / 移除 Google Play 商店 (action install | uninstall): a job that boots the instance cold once (playstore.js asks first)
export const playStore = (i, action, onErr) => run(i, 'playstore-' + action, {}, onErr);

export const cancelJob = safe(async (id) => onJob(await api.post(`/api/jobs/${encodeURIComponent(id)}/cancel`)));
export const openFolder = safe((what, index) => api.post('/api/open', index == null ? { what } : { what, index }));
export const input = safe((i, body) => api.post(`/api/instances/${i}/input`, body));

export async function copy(text) {
  if (!text) return;
  if (await copyText(text)) toast({ title: '已复制', text });
  else toast({ tone: 'err', title: '没能复制', text: '浏览器不让写剪贴板，请手动选中复制。' });
}

export async function loadInstances() {
  try {
    setInstances(await api.get('/api/instances'));
    S.loadError = null;
  } catch (e) {
    if (!S.list) S.loadError = e;
  }
  update();
}
