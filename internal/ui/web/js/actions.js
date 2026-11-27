// Everything that changes something on the server, with the toasts that go with it.

import { t, comma, gap } from './i18n.js';
import { api } from './api.js';
import { S, update, instance } from './store.js';
import { toast, toastError, copyText } from './ui.js';
import { quoted, dur, diskInfoText, diskTrimText, diskCompactText, playStoreText, gles32Text } from './format.js';
import { openActivity, runCheck } from './panels.js';

const VERB = { 'import-mumu': t('导入游戏'), start: t('开机'), stop: t('关机'), install: t('安装 APK'), create: t('新建'), 'disk-info': t('查看磁盘占用'), 'disk-trim': t('回收磁盘空间'), 'disk-compact': t('整理磁盘'), clone: t('复制实例'),
  components: t('补全组件'), 'collect-info': t('收集问题信息'), 'playstore-install': t('补全 Google Play 商店'), 'playstore-uninstall': t('移除 Google Play 商店'),
  'playstore-import': t('导入 Google Play 商店的文件'), 'gles32-on': t('开启报告 GLES 3.2'), 'gles32-off': t('关闭报告 GLES 3.2'),
  update: t('整包更新') };
// what the store needs once it is in an instance: registering its GSF ID (playstore.js openGSFID, the page is
// playstore.js UNCERTIFIED; not imported from there, as playstore.js imports this module)
const GSF = t('登录时提示设备未经 Play 保护机制认证的，在实例的「更多操作」里点「复制 GSF ID」，到 https://www.google.com/android/uncertified/ 登记。');
// an import the server refused (components_rejected, details.reason) when its message has no advice of its own
const IMPORT_ADVICE = {
  unknown_file: t('要的是 Google 官方的 Google Play 版镜像压缩包 x86_64-34_r14.zip，或者从它里面取出的五个文件。'),
  mismatch: t('这个文件和 Google 发布的那份不一样（可能没下载完整或被改过），重新下载后再试。'),
  incomplete: t('五个文件要一起选上，或者只选镜像压缩包。'),
};

export function importAdvice(e) {
  return (e && (e.advice || (e.code === 'components_rejected' && IMPORT_ADVICE[e.details?.reason]))) || '';
}

// The root's components changed (a fetch or an import finished): 组件 and the Google Play Store state read again.
function reloadComponents() {
  api.get('/api/components').then((v) => { S.components = v; S.componentsError = null; update(); }).catch(() => {});
}
const isBoot = (j) => j.kind === 'start';
const activity = { label: t('查看活动'), run: () => openActivity() };
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
  if (job.state === 'failed') toast({ tone: 'err', title: who ? t('{who}{what}没有成功', { who, what: VERB[job.kind] || '' }) : t('{what}没有成功', { what: VERB[job.kind] || '' }), text: e.message, advice: job.kind === 'playstore-import' ? importAdvice(e) : e.advice, action: activity });
  else if (job.state === 'canceled') { if (isBoot(job)) toast({ tone: 'info', title: t('已停止等待'), text: t('{who}的模拟器会继续开机。', { who }) }); }
  else if (isBoot(job)) {
    const w = Math.max((r.warnings || []).length, (job.warnings || []).length);
    toast({ tone: w ? 'warn' : 'ok', title: t('{who}已开机', { who }), action: w ? activity : null,
      text: [r.first_boot && t('首次初始化已完成'), r.total_seconds && t('用时 {time}', { time: dur(r.total_seconds) }), w && t('有 {w} 个设置步骤没成功', { w })].filter(Boolean).join(comma()) });
  } else if (job.kind === 'components') toast({ title: t('组件补全完成'), text: t('已重新检查这台电脑。') });
  else if (job.kind === 'collect-info') toast({ title: t('问题信息已收集'), text: r.zip_path ? t('问题包：{zip_path}', { zip_path: r.zip_path }) : '', timeout: 9000 });
  else if (job.kind === 'create') toast({ title: t('{who}新建好了', { who }), text: t('初始化已做完，已关机；第一次开机和平常一样快。') });
  else if (job.kind === 'stop') toast({ title: t('{who}已关机', { who }) });
  else if (job.kind === 'import-mumu') toast({ title: t('游戏已导入到{who}', { who }) });
  else if (job.kind === 'install') toast({ title: t('安装完成'), text: t('{who}装好了 {name}', { who, name: r.package || 'APK' }) });
  // the sizes are the answer, so they stay long enough to read
  else if (job.kind === 'disk-info') toast({ tone: 'info', title: t('{who}的磁盘占用', { who }), text: diskInfoText(r), timeout: 9000 });
  else if (job.kind === 'disk-trim') toast({ title: t('{who}已触发磁盘回收', { who }), text: diskTrimText(), timeout: 6000 });
  else if (job.kind === 'disk-compact') toast({ title: r.skipped ? t('{who}已跳过磁盘重写', { who }) : t('{who}磁盘整理好了', { who }), text: diskCompactText(r), timeout: 6000 });
  else if (job.kind === 'clone' && r.instance) toast({ title: t('{who}已复制成{copy}', { who, copy: quoted(r.instance, r.instance.index) }), text: t('游戏、登录状态和设置都带过去了；两台的设备标识一样，别同时登同一个账号。'), timeout: 9000 });
  else if (job.kind === 'playstore-install') {
    toast({ tone: r.installed ? 'ok' : 'warn', title: r.installed ? t('{who}已补全 Google Play 商店', { who }) : t('{who}的 Google Play 商店没装全', { who }),
      text: r.installed ? playStoreText(job.kind, r) + gap() + GSF : playStoreText(job.kind, r), action: r.installed ? null : activity, timeout: 12000 });
  } else if (job.kind === 'playstore-uninstall') {
    toast({ tone: r.state === 'absent' ? 'ok' : 'warn', title: r.state === 'absent' ? t('{who}已移除 Google Play 商店', { who }) : t('{who}的 Google Play 商店没移除干净', { who }),
      text: playStoreText(job.kind, r), action: r.state === 'absent' ? null : activity, timeout: 6000 });
  } else if (job.kind === 'gles32-on' || job.kind === 'gles32-off') {
    const done = job.kind === 'gles32-on' ? r.state === 'on' : r.state === 'off';
    toast({ tone: done ? 'ok' : 'warn', title: job.kind === 'gles32-on' ? (done ? t('{who}开启报告 GLES 3.2了', { who }) : t('{who}的开启报告 GLES 3.2没有生效', { who }))
      : (done ? t('{who}关闭报告 GLES 3.2了', { who }) : t('{who}的关闭报告 GLES 3.2没有生效', { who })),
      text: gles32Text(job.kind, r), action: done ? null : activity, timeout: (r.notes || []).length ? 14000 : 7000 });
  } else if (job.kind === 'playstore-import') {
    toast({ title: t('Google Play 商店的文件已就绪'), text: t('{text}要用的实例在「更多操作」里补全商店。', { text: playStoreText(job.kind, r) }), timeout: 9000 });
  } else if (job.kind === 'update') {
    if (!r.handoff) toast({ tone: 'info', title: r.message || t('已经是最新版本') });
    else {
      // the manager exits now and tools\modavd.exe starts it again after the swap, in a new window
      toast({ title: t('正在换上新版'), text: t('{message}。这个窗口会自己关掉。', { message: r.message || '' }), timeout: 15000 });
      setTimeout(() => window.close(), 1200);
    }
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

export const importMuMu = (i, source, games) => run(i, 'import-mumu', { source, games });

export async function install(i, file) {
  const fd = new FormData();
  fd.append('apk', file, file.name);
  if (await run(i, 'install', fd)) toast({ tone: 'info', title: t('正在安装'), text: `${file.name} → ${quoted(instance(i), i)}` });
}

// 磁盘占用 reads only and works on or off; 回收磁盘空间 needs the instance booted; 整理磁盘 (copies the whole data disk, minutes) needs it off
export const diskInfo = (i) => run(i, 'disk-info', {});
export const diskTrim = (i) => run(i, 'disk-trim', {});
export async function diskCompact(i) {
  if (await run(i, 'disk-compact', {})) toast({ tone: 'info', title: t('正在整理磁盘'), text: t('{who}正在检查并整理，期间不能开机', { who: quoted(instance(i), i) }), timeout: 5000 });
}

// 复制实例 (clone): copies the shut-down instance whole into a new one (minutes for a large data disk); the new one shows up when done
export async function clone(i) {
  if (await run(i, 'clone', {})) toast({ tone: 'info', title: t('正在复制实例'), text: t('{who}正在复制成一台新实例，期间不能开机', { who: quoted(instance(i), i) }), timeout: 5000 });
}

// 补全 / 移除 Google Play 商店 (action install | uninstall): a job that boots the instance cold once (playstore.js asks first)
export const playStore = (i, action, onErr) => run(i, 'playstore-' + action, {}, onErr);

// 开启 / 关闭报告 GLES 3.2 (action on | off): a job that boots the instance cold once (gles32.js asks first)
export const gles32 = (i, action, onErr) => run(i, 'gles32-' + action, {}, onErr);

export const cancelJob = safe(async (id) => onJob(await api.post(`/api/jobs/${encodeURIComponent(id)}/cancel`)));
export const openFolder = safe((what, index) => api.post('/api/open', index == null ? { what } : { what, index }));
export const input = safe((i, body) => api.post(`/api/instances/${i}/input`, body));

export async function copy(text) {
  if (!text) return;
  if (await copyText(text)) toast({ title: t('已复制'), text });
  else toast({ tone: 'err', title: t('没能复制'), text: t('浏览器不让写剪贴板，请手动选中复制。') });
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
