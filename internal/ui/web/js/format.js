// Formatting for people (zh-CN).

export const KIND = { start: '开机', stop: '关机', restart: '重启', install: '安装 APK' };
export const KIND_ICON = { start: 'power', stop: 'stop', restart: 'refresh', install: 'download' };
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
