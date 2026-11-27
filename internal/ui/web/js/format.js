import { t, comma, semi, stop, gap, quote, paren } from './i18n.js';

// Formatting for people (text through t(); the Chinese text is the key).

export const KIND = { 'import-mumu': t('从 MuMu 导入游戏'), start: t('开机'), stop: t('关机'), install: t('安装 APK'), create: t('新建'), components: t('补全组件'), 'collect-info': t('收集问题信息'), 'disk-info': t('磁盘占用'), 'disk-trim': t('回收磁盘空间'), 'disk-compact': t('整理磁盘'), clone: t('复制实例'),
  'playstore-install': t('补全 Google Play 商店'), 'playstore-uninstall': t('移除 Google Play 商店'), 'playstore-import': t('导入 Google Play 商店的文件'), 'gles32-on': t('开启报告 GLES 3.2'), 'gles32-off': t('关闭报告 GLES 3.2') };
export const KIND_ICON = { 'import-mumu': 'download', start: 'power', stop: 'stop', install: 'download', create: 'plus', components: 'download', 'collect-info': 'file', 'disk-info': 'disk', 'disk-trim': 'disk', 'disk-compact': 'disk', clone: 'copy',
  'playstore-install': 'plus', 'playstore-uninstall': 'x', 'playstore-import': 'download', 'gles32-on': 'plus', 'gles32-off': 'x' };
export const STATE = { running: t('进行中'), done: t('完成'), failed: t('失败'), canceled: t('已取消') };
export const STATE_TONE = { running: 'warn', done: 'ok', failed: 'err', canceled: 'idle' };

const pad = (n) => String(n).padStart(2, '0');

export function fmtMem(mb) {
  if (!mb) return '—';
  return mb % 1024 ? (mb / 1024).toFixed(1) + ' GB' : mb / 1024 + ' GB';
}

// The instance's own title; '' when it has none (a new instance's avd.ini.displayname is its file name mas_N).
export function ownTitle(inst) {
  const title = inst && inst.title ? inst.title.trim() : '';
  return title && title !== inst.name ? title : '';
}

// 「明日方舟」, or 「emu-0」 when the instance has no title of its own. mas_N is the file name, not shown as a name.
export function defaultName(i) {
  return `emu-${i}`;
}

export function label(inst) {
  if (!inst) return '';
  return ownTitle(inst) || defaultName(inst.index);
}

// 「明日方舟」, or 「emu-0」 when the instance has no title of its own.
export function quoted(inst, i) {
  return quote(inst ? label(inst) : defaultName(i));
}

export function parseTime(s) {
  if (!s) return null;
  const ms = Date.parse(s);
  return Number.isNaN(ms) ? null : ms;
}

export function clock(s) {
  const ms = typeof s === 'number' ? s : parseTime(s);
  if (ms == null) return '';
  const d = new Date(ms);
  const now = new Date();
  const hms = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  if (d.toDateString() === now.toDateString()) return hms;
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${hms}`;
}

export function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return t('{sec} 秒', { sec });
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  if (m < 60) return s ? t('{m} 分 {s} 秒', { m, s }) : t('{m} 分钟', { m });
  return t('{h} 小时 {m} 分', { h: Math.floor(m / 60), m: m % 60 });
}

export function fmtBytes(n) {
  if (n == null || !(n >= 0)) return '—';
  if (n >= 2 ** 30) return (n / 2 ** 30).toFixed(1) + ' GB';
  if (n >= 2 ** 20) return Math.round(n / 2 ** 20) + ' MB';
  if (n >= 2 ** 10) return Math.round(n / 2 ** 10) + ' KB';
  return n + ' B';
}

// 磁盘占用 (DiskInfoResult). A running instance has the disk open: allocated and reclaimable are not known then.
export function diskInfoText(r) {
  const parts = [];
  if (r.logical_bytes) parts.push(t('逻辑大小 {size}', { size: fmtBytes(r.logical_bytes) }));
  if (r.on_disk_bytes != null) parts.push(t('宿主占用 {size}', { size: fmtBytes(r.on_disk_bytes) }));
  if (r.allocated_bytes != null) parts.push(t('已分配 {size}', { size: fmtBytes(r.allocated_bytes) }));
  if (r.reclaimable_bytes != null) parts.push(t('可回收约 {size}', { size: fmtBytes(r.reclaimable_bytes) }));
  let text = parts.join(' · ');
  if (r.running) text += t('。开机时算不出已分配和可回收，关机后再看');
  else if (r.reclaimable_bytes != null) text += r.reclaimable_bytes > 0 ? t('。用「整理磁盘」可以收回') : t('。没有可以回收的');
  const warnings = (r.warnings || []).join(semi());
  return (text ? text + stop() : '') + (text && warnings ? gap() : '') + warnings;
}

export function diskTrimText() {
  return t('不等待安卓回收完成；电脑上的磁盘文件要关机后用「整理磁盘」才会变小。');
}

export function diskCompactText(r) {
  if (r.skipped) return t('预计可回收空间不足 {threshold}，已跳过重写。磁盘文件占用 {size}。', { threshold: fmtBytes(r.reclaim_threshold_bytes), size: fmtBytes(r.before_on_disk_bytes) });
  const delta = r.on_disk_delta_bytes ?? (r.after_on_disk_bytes - r.before_on_disk_bytes);
  const size = fmtBytes(delta > 0 ? delta : r.freed_bytes);
  const before = fmtBytes(r.before_on_disk_bytes);
  const after = fmtBytes(r.after_on_disk_bytes);
  return delta > 0 ? t('磁盘文件从 {before} 变为 {after}，增加 {size}。', { before, after, size })
    : t('磁盘文件从 {before} 变为 {after}，释放 {size}。', { before, after, size });
}

// The Google Play Store jobs (API.md "Google Play 商店"): PlayStoreResult of an instance's install / uninstall (stopped:
// it was off and is off again; restarted: it was running and booted cold), PlayStoreImportResult of the root's import.
export function playStoreText(kind, r) {
  if (!r) return '';
  if (kind === 'playstore-import') {
    if (r.skipped) return t('五个文件本来就齐全，给的文件也核对无误，没有改动。');
    if (r.from === 'zip') return t('已从镜像压缩包取出五个文件，核对无误。');
    const n = (r.used || []).length;
    const kept = (r.kept || []).length;
    return kept ? t('已导入 {n} 个文件，原有的 {kept} 个留用，核对无误。', { n, kept }) : t('已导入 {n} 个文件，核对无误。', { n });
  }
  const done = kind === 'playstore-install' ? r.state === 'installed' : r.state === 'absent';
  const how = r.stopped ? t('实例照原样关着') : r.restarted ? t('实例冷启动过一次') : '';
  const base = kind === 'playstore-install' ? (done ? t('商店已放进实例') : t('商店只放进去了一部分，可以再补全一次'))
    : (done ? t('已移除商店，换回了系统原来的占位应用') : t('没有移除干净，可以再移除一次'));
  return how ? base + comma() + how + stop() : base + stop();
}

// The GLES 3.2 jobs (API.md "报告 GLES 3.2"): GLES32Result of an instance's on / off (stopped: it was off and is off
// again; restarted: it was running and booted cold; prop: what the guest reports now).
export function gles32Text(kind, r) {
  if (!r) return '';
  const on = kind === 'gles32-on';
  const done = on ? r.state === 'on' : r.state === 'off';
  const how = r.stopped ? t('实例照原样关着') : r.restarted ? t('实例冷启动过一次') : '';
  const base = on ? (done ? t('实例现在报 GLES 3.2') : t('还没有生效，可以再开一次')) : (done ? t('实例改回报 GLES 3.1') : t('还没有改回来，可以再关一次'));
  const head = r.prop ? base + paren('ro.opengles.version=' + r.prop) : base;
  const text = how ? head + comma() + how + stop() : head + stop();
  return text + (done && (r.notes || []).length ? gap() + r.notes.join(gap()) : '');
}

export function uptime(started) {
  const ms = parseTime(started);
  if (ms == null) return '';
  const m = Math.floor((Date.now() - ms) / 60000);
  if (m < 1) return t('刚开机');
  if (m < 60) return t('已运行 {m} 分钟', { m });
  return t('已运行 {h} 小时 {m} 分', { h: Math.floor(m / 60), m: m % 60 });
}

// Progress messages start with "实例 8 " (in English "Instance 8 "; the back end translates them); on a card or job
// row that is already said, so the prefix goes. The first letter is raised again when the rest was a sentence.
export function stripInst(msg) {
  const s = (msg || '').replace(/^(?:实例|Instance)\s*\d+\s*(?:的|'s|[:：])?\s*/i, '');
  return s !== (msg || '') && /^[a-z]/.test(s) ? s[0].toUpperCase() + s.slice(1) : s;
}

export function pct(p) {
  if (!p || !p.total) return null;
  return Math.max(0, Math.min(100, (p.done / p.total) * 100));
}

const STOP_METHOD = {
  already_offline: t('本来就没在运行'),
  launching_kill: t('模拟器刚拉起、安卓还没开机，已直接关掉（不用保存数据）'),
  graceful: t('已正常关机'),
  sync_kill: t('没能正常关机，已先保存数据再关掉'),
  console_kill: t('没能先保存数据就关掉了'),
  force: t('已强制关机（先保存了数据）'),
};

export function stopMethod(m) {
  if (!m) return '';
  const term = m.endsWith('+terminate');
  const base = term ? m.slice(0, -'+terminate'.length) : m;
  const what = STOP_METHOD[base] || t('已关机（{base}）', { base });
  return term ? t('{what}，最后强行结束了模拟器进程', { what }) : what;
}

// 「内存 6 GB · 6 核 · 60 Hz · 盘 64 GB · 占 12.3 GB」: what each number is, without a hover. The disk size only for a
// set size (自动扩盘 is the default, nothing to tell); 占 is what the data disk takes on the computer now
// (space.host_bytes, only what was written), left out when unknown.
export function cfgText(cfg, space) {
  cfg = cfg || {};
  const disk = cfg.data_disk_mode === 'manual' ? t(' · 盘 {gb} GB', { gb: cfg.data_partition_gb || '—' }) : '';
  const used = space && space.host_bytes >= 0 ? t(' · 占 {size}', { size: fmtBytes(space.host_bytes) }) : '';
  return t('内存 {mem} · {cpu} 核 · {fps} Hz', { mem: fmtMem(cfg.memory_mb), cpu: cfg.cpu || '—', fps: cfg.fps || '—' }) + disk + used;
}
