// Formatting for people (zh-CN).

export const KIND = { start: '开机', stop: '关机', install: '安装 APK', create: '新建', components: '补全组件', 'collect-info': '收集问题信息', 'disk-info': '磁盘占用', 'disk-trim': '回收磁盘空间', 'disk-compact': '整理磁盘',
  'playstore-install': '补全 Google Play 商店', 'playstore-uninstall': '移除 Google Play 商店', 'playstore-import': '导入 Google Play 商店的文件' };
export const KIND_ICON = { start: 'power', stop: 'stop', install: 'download', create: 'plus', components: 'download', 'collect-info': 'file', 'disk-info': 'disk', 'disk-trim': 'disk', 'disk-compact': 'disk',
  'playstore-install': 'plus', 'playstore-uninstall': 'x', 'playstore-import': 'download' };
export const STATE = { running: '进行中', done: '完成', failed: '失败', canceled: '已取消' };
export const STATE_TONE = { running: 'warn', done: 'ok', failed: 'err', canceled: 'idle' };

const pad = (n) => String(n).padStart(2, '0');

export function fmtMem(mb) {
  if (!mb) return '—';
  return mb % 1024 ? (mb / 1024).toFixed(1) + ' GB' : mb / 1024 + ' GB';
}

export function label(inst) {
  if (!inst) return '';
  return inst.title && inst.title.trim() ? inst.title : inst.name;
}

// 「明日方舟」, or 「mas_0」 when the instance has no title of its own.
export function quoted(inst, i) {
  return inst ? `「${label(inst)}」` : `「实例 ${i}」`;
}

export function parseTime(s) {
  if (!s) return null;
  const t = Date.parse(s);
  return Number.isNaN(t) ? null : t;
}

export function clock(s) {
  const t = typeof s === 'number' ? s : parseTime(s);
  if (t == null) return '';
  const d = new Date(t);
  const now = new Date();
  const hms = `${pad(d.getHours())}:${pad(d.getMinutes())}:${pad(d.getSeconds())}`;
  if (d.toDateString() === now.toDateString()) return hms;
  return `${pad(d.getMonth() + 1)}-${pad(d.getDate())} ${hms}`;
}

export function dur(sec) {
  sec = Math.max(0, Math.round(sec));
  if (sec < 60) return `${sec} 秒`;
  const m = Math.floor(sec / 60);
  const s = sec % 60;
  if (m < 60) return s ? `${m} 分 ${s} 秒` : `${m} 分钟`;
  return `${Math.floor(m / 60)} 小时 ${m % 60} 分`;
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
  if (r.logical_bytes) parts.push(`逻辑大小 ${fmtBytes(r.logical_bytes)}`);
  if (r.on_disk_bytes != null) parts.push(`宿主占用 ${fmtBytes(r.on_disk_bytes)}`);
  if (r.allocated_bytes != null) parts.push(`已分配 ${fmtBytes(r.allocated_bytes)}`);
  if (r.reclaimable_bytes != null) parts.push(`可回收约 ${fmtBytes(r.reclaimable_bytes)}`);
  let text = parts.join(' · ');
  if (r.running) text += '。开机时算不出已分配和可回收，关机后再看';
  else if (r.reclaimable_bytes != null) text += r.reclaimable_bytes > 0 ? '。用「整理磁盘」可以收回' :'。没有可以回收的';
  return (text ? text + '。' : '') + (r.warnings || []).join('；');
}

export function diskTrimText() {
  return '不等待安卓回收完成；电脑上的磁盘文件要关机后用「整理磁盘」才会变小。';
}

export function diskCompactText(r) {
  if (r.skipped) return `预计可回收空间不足 ${fmtBytes(r.reclaim_threshold_bytes)}，已跳过重写。磁盘文件占用 ${fmtBytes(r.before_on_disk_bytes)}。`;
  const delta = r.on_disk_delta_bytes ?? (r.after_on_disk_bytes - r.before_on_disk_bytes);
  const change = delta > 0 ? `增加 ${fmtBytes(delta)}` : `释放 ${fmtBytes(r.freed_bytes)}`;
  return `磁盘文件从 ${fmtBytes(r.before_on_disk_bytes)} 变为 ${fmtBytes(r.after_on_disk_bytes)}，${change}。`;
}

// The Google Play Store jobs (API.md "Google Play 商店"): PlayStoreResult of an instance's install / uninstall (stopped:
// it was off and is off again; restarted: it was running and booted cold), PlayStoreImportResult of the root's import.
export function playStoreText(kind, r) {
  if (!r) return '';
  if (kind === 'playstore-import') {
    if (r.skipped) return '五个文件本来就齐全，给的文件也核对无误，没有改动。';
    const kept = (r.kept || []).length ? `，原有的 ${r.kept.length} 个留用` : '';
    return `${r.from === 'zip' ? '已从镜像压缩包取出五个文件' : `已导入 ${(r.used || []).length} 个文件${kept}`}，核对无误。`;
  }
  const done = kind === 'playstore-install' ? r.state === 'installed' : r.state === 'absent';
  const how = r.stopped ? '实例照原样关着' : r.restarted ? '实例冷启动过一次' : '';
  if (kind === 'playstore-install') return (done ? '商店已放进实例' : '商店只放进去了一部分，可以再补全一次') + (how ? `，${how}` : '') + '。';
  return (done ? '已移除商店，换回了系统原来的占位应用' : '没有移除干净，可以再移除一次') + (how ? `，${how}` : '') + '。';
}

export function uptime(started) {
  const t = parseTime(started);
  if (t == null) return '';
  const m = Math.floor((Date.now() - t) / 60000);
  if (m < 1) return '刚开机';
  if (m < 60) return `已运行 ${m} 分钟`;
  return `已运行 ${Math.floor(m / 60)} 小时 ${m % 60} 分`;
}

// Progress messages start with "实例 8 "; on a card or job row that is already said.
export function stripInst(msg) {
  return (msg || '').replace(/^实例\s*\d+\s*(的)?/, '');
}

export function pct(p) {
  if (!p || !p.total) return null;
  return Math.max(0, Math.min(100, (p.done / p.total) * 100));
}

const STOP_METHOD = {
  already_offline: '本来就没在运行',
  launching_kill: '模拟器刚拉起、安卓还没开机，已直接关掉（不用保存数据）',
  graceful: '已正常关机',
  sync_kill: '没能正常关机，已先保存数据再关掉',
  console_kill: '没能先保存数据就关掉了',
  force: '已强制关机（先保存了数据）',
};

export function stopMethod(m) {
  if (!m) return '';
  const term = m.endsWith('+terminate');
  const base = term ? m.slice(0, -'+terminate'.length) : m;
  return (STOP_METHOD[base] || `已关机（${base}）`) + (term ? '，最后强行结束了模拟器进程' : '');
}

// 「内存 6 GB · 6 核 · 60 Hz · 盘 64 GB」: what each number is, without a hover
export function cfgText(cfg) {
  cfg = cfg || {};
  return `内存 ${fmtMem(cfg.memory_mb)} · ${cfg.cpu || '—'} 核 · ${cfg.fps || '—'} Hz · 盘 ${cfg.data_partition_gb || '—'} GB`;
}
