// Mock backend for design work: ?mock=normal|empty|notready|busy|many replaces fetch('/api/…') and EventSource with
// an in-memory fake that follows internal/ui/API.md. Extra params: drop=1 (the event stream drops after 1.5 s and
// stays down), offline=1 (it never connects), shotfail=1 (every screenshot answers 500). window.__mockShots counts
// screenshot requests per instance. Never used in production: the server does not serve /dev/.
// Screens are drawn on a canvas: abstract shapes only, no real game content.

const ROOT = 'X:\\ModAVD';
const INDEXES = [0, 1, 2, 3, 4, 6, 7, 8, 9];
const MEM = [3072, 4096, 5120, 6144];
const db = { insts: new Map(), jobs: [], streams: new Set(), seq: 40, check: 'ok', offline: false, drop: false, ready: true };
const now = () => Date.now();
const iso = (t = now()) => {
  const d = new Date(t);
  const off = -d.getTimezoneOffset();
  const p = (n) => String(Math.floor(Math.abs(n))).padStart(2, '0');
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())}T${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}${off >= 0 ? '+' : '-'}${p(off / 60)}:${p(off % 60)}`;
};
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const clone = (o) => JSON.parse(JSON.stringify(o));

// ---- data ----------------------------------------------------------------------------------------------------------

function rootInfo() {
  return {
    root: ROOT, sdk: ROOT + '\\sdk', local_sdk: false, adb_exe: ROOT + '\\sdk\\platform-tools\\adb.exe',
    adb_server_port: 20050, script_adb_server_port: 20049,
    version: db.ready ? '0.35.0' : '', build: db.ready ? 35 : 0,
  };
}

const GPU = 'Google (NVIDIA Corporation), Android Emulator OpenGL ES Translator (NVIDIA GeForce RTX 3060/PCIe/SSE2), OpenGL ES 3.0 (4.5.0 NVIDIA 566.36)';
const SWIFT = 'Google (Google Inc.), Android Emulator OpenGL ES Translator (Google SwiftShader), OpenGL ES 3.0 (SwiftShader 4.1.0.7)';

function mk(i, title, o = {}) {
  const st = o.status || 'offline';
  const console = 20000 + 10 * i;
  const initialized = o.initialized !== false;
  const inst = {
    index: i, name: 'mas_' + i, title: title || 'mas_' + i, status: st,
    running: st !== 'offline', booted: st === 'online', busy: st === 'starting' ? 'starting' : '',
    pid: st !== 'offline' ? 9000 + i * 1371 : 0,
    started_at: st !== 'offline' ? iso(now() - (o.upMin || 30) * 60000) : '',
    serial: 'emulator-' + console, adb_address: '127.0.0.1:' + (console + 1),
    ports: { console, adb: console + 1, grpc: console + 2 },
    screencap: { shm: 'SHM_videmulator' + console, shm_ready: st === 'online' },
    config: { memory_mb: o.mem || 6144, cpu: o.cpu || 6, fps: o.fps || 60, data_partition_gb: o.disk || 64, width: 1280, height: 720, dpi: 240 },
    initialized, launcher: initialized ? 'µLauncher' : '',
    renderer: initialized ? (o.sw ? SWIFT : GPU) : '', software_renderer: !!o.sw,
    path: `${ROOT}\\avd\\mas_${i}.avd`,
  };
  Object.defineProperty(inst, '_', { value: { scene: o.scene ?? i, mode: 'game', win: false, fail: o.fail || null, logs: initialized || st !== 'offline', tap: null }, enumerable: false });
  db.insts.set(i, inst);
  return inst;
}

function oldJob(kind, i, state, agoSec, tookSec, extra = {}) {
  const t0 = now() - agoSec * 1000;
  const j = {
    id: 'j' + ++db.seq, kind, index: i, state, created_at: iso(t0), finished_at: state === 'running' ? '' : iso(t0 + tookSec * 1000),
    progress: null, result: null, error: null, warnings: [], ...extra,
  };
  db.jobs.unshift(j);
  return j;
}

const MEM_FAIL = ['precheck_failed', '电脑可用内存不足：实例内存 6 GB，启动约需 7.5 GB，当前可用 5.1 GB', '请关闭部分程序，或调小实例内存'];

function setup(sc) {
  if (sc === 'empty') return;
  if (sc === 'notready') {
    db.ready = false;
    db.check = 'notready';
    mk(0, '', { initialized: false });
    return;
  }
  if (sc === 'many') {
    db.check = 'warn';
    mk(0, '明日方舟 · 主号', { status: 'online', upMin: 133, scene: 0 });
    mk(1, '明日方舟 · 小号', { status: 'online', upMin: 131, scene: 1, mem: 4096, cpu: 4 });
    mk(2, '星穹铁道', { status: 'online', upMin: 58, scene: 3 });
    mk(3, '重返未来 1999', { status: 'starting', upMin: 0.3, scene: 2 });
    mk(4, '蔚蓝档案', { status: 'online', upMin: 12, scene: 2, fps: 30 });
    mk(6, '碧蓝航线', { status: 'offline', mem: 4096, cpu: 4, fps: 30 });
    mk(7, '日常备用', { status: 'online', upMin: 7, scene: 1, sw: true, mem: 3072, cpu: 2, fps: 30 });
    mk(8, '', { status: 'offline', initialized: false, disk: 32 });
    mk(9, '公主连结', { status: 'offline', mem: 5120, cpu: 4 });
    longStart(3, 9);
    return;
  }
  if (sc === 'busy') {
    mk(0, '明日方舟 · 主号', { status: 'online', upMin: 133, scene: 0 });
    mk(1, '星穹铁道', { status: 'starting', upMin: 1, scene: 3, initialized: false });
    mk(2, '重返未来 1999', { status: 'online', upMin: 61, scene: 2 });
    mk(3, '蔚蓝档案', { status: 'offline', fail: MEM_FAIL });
    mk(4, '日常备用', { status: 'online', upMin: 20, scene: 1 });
    oldJob('stop', 4, 'done', 3600, 9, { result: { index: 4, name: 'mas_4', method: 'graceful', synced: true, seconds: 8.6, warnings: [] } });
    oldJob('start', 4, 'done', 1250, 42, {
      progress: { step: 'done', message: '实例 4 已就绪（共 41.8 秒）', done: 1000, total: 1000 },
      result: { instance: null, launched: true, boot_seconds: 21.4, total_seconds: 41.8, first_boot: false, log_file: '', logcat_file: '', zygote_restarted: false, warnings: ['宿主输入法没有设成默认：settings put 返回 1'] },
      warnings: ['宿主输入法没有设成默认：settings put 返回 1'],
    });
    oldJob('start', 2, 'canceled', 900, 15, { error: { code: 'canceled', message: '已停止等待，模拟器继续开机', advice: '' } });
    oldJob('start', 3, 'failed', 140, 2, { error: { code: MEM_FAIL[0], message: MEM_FAIL[1], advice: MEM_FAIL[2] } });
    const s = longStart(1, 34, true);
    const one = db.insts.get(1);
    s.progress = { step: 'first_boot', message: '实例 1 首次开机初始化', done: 800, total: 1000 };
    Object.assign(one, { status: 'online', booted: true });
    one.screencap.shm_ready = true;
    setTimeout(() => step(s, 'vulkan_fix', '实例 1 客体 Vulkan 驱动补丁', 900), 9000);
    setTimeout(() => {
      Object.assign(one, { busy: '', initialized: true, launcher: 'µLauncher', renderer: GPU });
      finishJob(s, startResult(one, 26.3, 58.1, true));
    }, 15000);
    const st = oldJob('stop', 2, 'running', 3, 0);
    db.insts.get(2).busy = 'stopping';
    setTimeout(() => finishStop(db.insts.get(2), st, false), 9000);
    const ins = oldJob('install', 0, 'running', 4, 0);
    setTimeout(() => finishJob(ins, { index: 0, apk: 'X:\\Temp\\modavd-upload-1.apk', package: 'com.example.helper', output: 'Success' }), 12000);
    for (const [k, msg] of [[s.id, '实例 1 的模拟器已拉起（pid 10371），等安卓开机'], [s.id, '实例 1 等安卓开机，已 15 秒'], [s.id, '实例 1 安卓已开机（26.3 秒）'], [s.id, '实例 1 开机后调优'], [s.id, '实例 1 首次开机初始化']]) log('info', msg, k, 20000);
    log('error', MEM_FAIL[1], db.jobs.find((j) => j.state === 'failed').id, 140000);
    return;
  }
  // normal
  mk(0, '明日方舟 · 主号', { status: 'online', upMin: 133, scene: 0 });
  mk(1, '星穹铁道', { status: 'offline', mem: 6144 });
  mk(8, '重返未来 1999', { status: 'starting', upMin: 0.3, scene: 2 });
  oldJob('stop', 1, 'done', 5400, 7, { result: { index: 1, name: 'mas_1', method: 'graceful', synced: true, seconds: 6.9, warnings: [] } });
  oldJob('start', 0, 'done', 133 * 60, 38, { result: { launched: true, boot_seconds: 19.2, total_seconds: 37.6, first_boot: false, warnings: [], zygote_restarted: false } });
  longStart(8, 18);
}

// A start that is already on its way (boot progress every 2.5 s; finishes after about a minute).
function longStart(i, elapsed, quiet) {
  const inst = db.insts.get(i);
  const j = oldJob('start', i, 'running', elapsed, 0);
  inst.busy = 'starting';
  j.progress = { step: 'boot', message: `实例 ${i} 等安卓开机，已 ${elapsed} 秒`, done: 100 + elapsed * 4, total: 1000 };
  if (quiet) return j;
  let sec = elapsed;
  const t = setInterval(() => {
    if (j.state !== 'running') { clearInterval(t); return; }
    sec += 2.5;
    if (sec < 70) step(j, 'boot', `实例 ${i} 等安卓开机，已 ${Math.round(sec)} 秒`, 100 + Math.round(sec * 6));
    else {
      clearInterval(t);
      inst.status = 'online'; inst.booted = true; inst.screencap.shm_ready = true;
      step(j, 'boot', `实例 ${i} 安卓已开机（${sec.toFixed(1)} 秒）`, 610);
      setTimeout(() => { inst.busy = ''; finishJob(j, startResult(inst, sec, sec + 6, false)); }, 2500);
    }
  }, 2500);
  return j;
}

function pub(inst) { return clone(inst); }
function list() { return { ...rootInfo(), instances: [...db.insts.values()].sort((a, b) => a.index - b.index).map(pub) }; }

// ---- check -----------------------------------------------------------------------------------------------------------

function checkResult() {
  const it = (id, title, ok, blocking, reason, advice = '') => ({ id, title, ok, blocking, reason, advice });
  const items = [
    it('path', '目录路径', true, true, ROOT),
    it('components', '组件', true, true, '平台工具（adb）、系统镜像 Android 14 Google APIs x86_64'),
    it('emulator', '模拟器版本', true, true, '内测包 0.35.0（37.2.10）'),
    it('acceleration', '硬件虚拟化', true, true, 'Windows 虚拟机监控程序平台（WHPX）可用'),
    it('vulkan', '显卡 Vulkan', true, false, '可用显卡：NVIDIA GeForce RTX 3060'),
    it('disk', '磁盘', true, true, '实例所在盘 E:\\ 剩余 412.6 GB'),
    it('memory', '内存', true, true, '实例内存 6 GB，启动约需 7.5 GB，当前可用 18.3 GB'),
  ];
  const comps = [
    { id: 'platform-tools', name: '平台工具（adb）', version: '36.0.0', build: '', installed: true, optional: false, local_sdk: false },
    { id: 'emulator', name: 'Android 模拟器', version: '37.2.10', build: 'mas-35', installed: true, optional: false, local_sdk: false },
    { id: 'system-image', name: '系统镜像 Android 14 Google APIs x86_64', version: '14', build: 'r07', installed: true, optional: false, local_sdk: false },
    { id: 'launcher', name: '轻量桌面 µLauncher', version: '0.6.1', build: '', installed: true, optional: true, local_sdk: false },
    { id: 'ime', name: '宿主输入法', version: '1.2.0', build: '', installed: true, optional: true, local_sdk: false },
  ];
  if (db.check === 'warn') {
    Object.assign(items[4], { ok: false, reason: '没有找到能用 Vulkan 的显卡（只有 Microsoft Basic Render Driver）', advice: '安装或更新显卡驱动（NVIDIA / AMD / Intel 官网驱动自带 Vulkan）。没有可用的显卡时模拟器只能软件渲染，游戏会占满 CPU，性能会很差' });
  }
  if (db.check === 'notready') {
    Object.assign(items[1], { ok: false, reason: '这个目录里缺少：平台工具（adb）、系统镜像 Android 14 Google APIs x86_64', advice: '运行魔改 AVD 根目录里的「补全组件.bat」：它会先显示《Android 软件开发套件许可协议》，输入 y 同意后从官方渠道下载 adb 和系统镜像（约 1.5 GB，中断了再运行会接着下），看到「完成」后再检查一次' });
    Object.assign(items[3], { ok: false, reason: '硬件虚拟化（Windows 虚拟机监控程序平台）不可用，魔改 AVD 无法启动（检查结果：WHPX is not installed）', advice: '先在 BIOS/UEFI 里打开 VT-x/SVM，再在「启用或关闭 Windows 功能」里勾选「Windows 虚拟机监控程序平台」，然后重启' });
    Object.assign(items[4], { ok: null, reason: '查不了 Vulkan：探测程序 8 秒内没有结束' });
    Object.assign(items[2], { reason: '内测包 0.35.0（37.2.10）' });
    comps[0].installed = false; comps[0].version = '';
    comps[2].installed = false; comps[2].version = ''; comps[2].build = '';
    comps[4].installed = false; comps[4].version = '';
  }
  const ready = !items.some((x) => x.blocking && x.ok === false);
  return { ...rootInfo(), ready, items, components: comps };
}

// ---- jobs ------------------------------------------------------------------------------------------------------------

function newJob(kind, i) {
  const j = { id: 'j' + ++db.seq, kind, index: i, state: 'running', created_at: iso(), finished_at: '', progress: null, result: null, error: null, warnings: [] };
  db.jobs.unshift(j);
  emit('job', j);
  return j;
}

function step(j, s, message, done) {
  if (j.state === 'running') {
    j.progress = { step: s, message, done, total: 1000 };
    emit('job', j);
    log('info', message, j.id);
  }
  emit('instances', list());
}

function finishJob(j, result, warnings = []) {
  if (j.state !== 'running') return;
  j.state = 'done';
  j.finished_at = iso();
  j.result = result;
  j.warnings = warnings;
  if (j.kind === 'start' || j.kind === 'restart') j.progress = { step: 'done', message: `实例 ${j.index} 已就绪（共 ${result.total_seconds.toFixed(1)} 秒）`, done: 1000, total: 1000 };
  emit('job', j);
  emit('instances', list());
}

function failJob(j, code, message, advice) {
  if (j.state !== 'running') return;
  j.state = 'failed';
  j.finished_at = iso();
  j.error = { code, message, advice };
  emit('job', j);
  log('error', message, j.id);
  emit('instances', list());
}

function startResult(inst, boot, total, first) {
  return { instance: pub(inst), launched: true, boot_seconds: boot, total_seconds: total, first_boot: first, log_file: `${ROOT}\\logs\\mas_${inst.index}-emulator.log`, logcat_file: `${ROOT}\\logs\\mas_${inst.index}-logcat.txt`, zygote_restarted: first, warnings: [] };
}

function run(j, steps) {
  let k = 0;
  const next = () => {
    if (k >= steps.length) return;
    const [ms, fn] = steps[k++];
    setTimeout(() => { fn(); next(); }, ms);
  };
  next();
}

function startSeq(inst, j, window) {
  const i = inst.index;
  if (!db.ready) {
    setTimeout(() => failJob(j, 'not_ready', '这个目录里缺少：平台工具（adb）、系统镜像 Android 14 Google APIs x86_64', '运行魔改 AVD 根目录里的「补全组件.bat」，看到「完成」后再检查一次'), 600);
    return;
  }
  if (inst._.fail && inst.config.memory_mb > 4096) {
    setTimeout(() => failJob(j, ...inst._.fail), 900);
    return;
  }
  const first = !inst.initialized;
  const boot = 15 + Math.random() * 6;
  const steps = [
    [500, () => {
      Object.assign(inst, { status: 'starting', running: true, busy: 'starting', pid: 10000 + Math.floor(Math.random() * 9000), started_at: iso() });
      inst._.win = !!window;
      inst._.logs = true;
      step(j, 'launch', `实例 ${i} 的模拟器已拉起（pid ${inst.pid}），等安卓开机`, 50);
    }],
    [1100, () => step(j, 'boot', `实例 ${i} 等安卓开机，已 5 秒`, 200)],
    [1100, () => step(j, 'boot', `实例 ${i} 等安卓开机，已 10 秒`, 340)],
    [1100, () => step(j, 'boot', `实例 ${i} 等安卓开机，已 15 秒`, 480)],
    [1000, () => {
      Object.assign(inst, { status: 'online', booted: true });
      inst.screencap.shm_ready = true;
      step(j, 'boot', `实例 ${i} 安卓已开机（${boot.toFixed(1)} 秒）`, 610);
    }],
    [700, () => step(j, 'tune', `实例 ${i} 开机后调优`, 620)],
    [700, () => step(j, 'ime', `实例 ${i} 宿主输入法`, 700)],
  ];
  if (first) {
    steps.push([900, () => step(j, 'first_boot', `实例 ${i} 首次开机初始化`, 800)]);
    steps.push([3200, () => {
      Object.assign(inst, { initialized: true, launcher: 'µLauncher', renderer: GPU });
      step(j, 'vulkan_fix', `实例 ${i} 客体 Vulkan 驱动补丁`, 900);
    }]);
  }
  steps.push([800, () => {
    inst.busy = '';
    finishJob(j, startResult(inst, boot, boot + (first ? 14 : 4.2), first));
  }]);
  run(j, steps);
}

function finishStop(inst, j, force) {
  Object.assign(inst, { status: 'offline', running: false, booted: false, busy: '', pid: 0, started_at: '' });
  inst.screencap.shm_ready = false;
  inst._.win = false;
  finishJob(j, { index: inst.index, name: inst.name, method: force ? 'force' : 'graceful', synced: true, seconds: force ? 1.2 : 6.4, warnings: [] });
}

function stopSeq(inst, j, force) {
  for (const s of db.jobs) {
    if (s.index === inst.index && s.state === 'running' && (s.kind === 'start' || s.kind === 'restart')) {
      s.state = 'canceled';
      s.finished_at = iso();
      s.error = { code: 'canceled', message: '开机被关机打断', advice: '' };
      emit('job', s);
    }
  }
  inst.busy = 'stopping';
  emit('instances', list());
  log('info', force ? `实例 ${inst.index} 强制关机：先保存数据再关掉` : `实例 ${inst.index} 正在关机`, j.id);
  setTimeout(() => finishStop(inst, j, force), force ? 900 : 2600);
}

// ---- logs ------------------------------------------------------------------------------------------------------------

function log(level, msg, job_id, ago = 0) {
  const l = { time: iso(now() - ago), level, msg, job_id: job_id || '' };
  emit('log', l);
  db.logs = db.logs || [];
  db.logs.push(l);
}

function emulatorLog(inst, lines) {
  const n = inst.name;
  const base = [
    `INFO    | Android emulator version 37.2.10.0 (build_id mas-35) (CL:N/A)`,
    `INFO    | Graphics backend: gfxstream`,
    `INFO    | Found systemPath ${ROOT}\\sdk\\system-images\\android-34\\google_apis\\x86_64\\`,
    `INFO    | Increasing RAM size to ${inst.config.memory_mb}MB`,
    `INFO    | Checking system compatibility:`,
    `INFO    |   Checking: hasSufficientDiskSpace`,
    `INFO    |      Ok: Disk space requirements to run avd: \`${n}\` are met`,
    `INFO    |   Checking: hasSufficientHwGpu`,
    `INFO    |      Ok: Hardware GPU requirements to run avd: \`${n}\` are passed`,
    `INFO    |   Checking: hasSufficientSystem`,
    `INFO    |      Ok: System requirements to run avd: \`${n}\` are met`,
    `INFO    | Guest GLES Driver: Auto (ext controls)`,
    `INFO    | WHPX on Windows 10.0.26200 detected.`,
    `INFO    | Window system: ${inst._.win ? 'Qt' : 'headless'}`,
    `INFO    | Storing crashdata in: ${ROOT}\\logs\\crashdata, detection is enabled for process: 18244`,
    `INFO    | Initializing gfxstream backend`,
    `INFO    | GLES: ${inst.renderer || GPU}`,
    `WARNING | unexpected system image feature string, emulator might not function correctly, please try updating the emulator.`,
    `INFO    | Advertising in: ${ROOT}\\avd\\running\\pid_18244.ini`,
    `INFO    | Boot completed in 16243 ms`,
    `INFO    | Increasing screen off timeout, logcat buffer size to 2M.`,
  ];
  const out = [...base];
  let t = 0;
  while (out.length < Math.min(lines, 140)) {
    t += 7;
    out.push(`INFO    | [${String(t).padStart(5, ' ')}s] gfxstream: frame stats  fps=${inst.config.fps - (t % 3)}  gpu=${(4 + (t % 5) * 0.7).toFixed(1)} ms`);
    if (t % 63 === 0) out.push('WARNING | adb: device offline for 312 ms, reconnecting');
  }
  return out.join('\n') + '\n';
}

function logcat(inst, lines) {
  const tags = [['I', 'ActivityManager', 'Start proc 4321:com.android.launcher3/u0a123 for top-activity'],
    ['D', 'WindowManager', 'Changing focus from null to Window{a1b2 u0 com.android.launcher3}'],
    ['I', 'InputDispatcher', 'Delivering touch to Window{c3d4 u0}: action: 0x0'],
    ['W', 'BroadcastQueue', 'Background execution not allowed: receiving Intent { act=android.intent.action.PACKAGE_CHANGED }'],
    ['I', 'SurfaceFlinger', 'Display 0 HWC layers: 3'],
    ['V', 'gfxstream', 'vkQueueSubmit: 2 command buffers'],
    ['E', 'chromium', '[ERROR:network_change_notifier] NetworkChangeNotifier: no default network'],
    ['I', 'PackageManager', 'Package com.example.helper codePath changed'],
    ['D', 'ConnectivityService', 'NetworkAgentInfo [WIFI () - 100] validation passed']];
  const out = [];
  const t0 = now() - 600000;
  for (let k = 0; k < Math.min(lines, 220); k++) {
    const [lvl, tag, msg] = tags[(k * 7 + inst.index) % tags.length];
    const d = new Date(t0 + k * 2300);
    const p = (n, w = 2) => String(n).padStart(w, '0');
    out.push(`${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}.${p(d.getMilliseconds(), 3)}  ${1200 + (k % 9) * 3}  ${1250 + (k % 13)} ${lvl} ${tag}: ${msg}`);
  }
  return out.join('\n') + '\n';
}

// ---- screens ---------------------------------------------------------------------------------------------------------

let canvas = null;

function rr(g, x, y, w, h, r) {
  g.beginPath();
  g.roundRect(x, y, w, h, r);
}

function sceneMeadow(g, t, s) {
  const sky = g.createLinearGradient(0, 0, 0, 470);
  sky.addColorStop(0, '#7FB7E0'); sky.addColorStop(1, '#F6D9B8');
  g.fillStyle = sky; g.fillRect(0, 0, 1280, 720);
  const sun = g.createRadialGradient(980, 170, 10, 980, 170, 120);
  sun.addColorStop(0, 'rgba(255,240,200,1)'); sun.addColorStop(1, 'rgba(255,240,200,0)');
  g.fillStyle = sun; g.fillRect(820, 40, 320, 280);
  g.fillStyle = 'rgba(255,255,255,.85)';
  for (let k = 0; k < 4; k++) {
    const x = ((k * 360 + t * 12) % 1500) - 150;
    const y = 90 + k * 37 % 90;
    g.beginPath(); g.ellipse(x, y, 90, 22, 0, 0, 7); g.ellipse(x + 50, y - 12, 60, 24, 0, 0, 7); g.fill();
  }
  g.fillStyle = '#86A9BC';
  g.beginPath(); g.moveTo(0, 430); g.lineTo(220, 270); g.lineTo(420, 400); g.lineTo(640, 250); g.lineTo(900, 420); g.lineTo(1100, 300); g.lineTo(1280, 410); g.lineTo(1280, 720); g.lineTo(0, 720); g.fill();
  g.fillStyle = '#6E9A86';
  g.beginPath(); g.moveTo(0, 480); g.bezierCurveTo(300, 400, 500, 520, 760, 450); g.bezierCurveTo(980, 400, 1150, 470, 1280, 440); g.lineTo(1280, 720); g.lineTo(0, 720); g.fill();
  const gr = g.createLinearGradient(0, 500, 0, 720);
  gr.addColorStop(0, '#8DBF6E'); gr.addColorStop(1, '#5F9550');
  g.fillStyle = gr;
  g.beginPath(); g.moveTo(0, 540); g.bezierCurveTo(400, 500, 800, 560, 1280, 520); g.lineTo(1280, 720); g.lineTo(0, 720); g.fill();
  g.fillStyle = '#E9D6A6';
  g.beginPath(); g.moveTo(560, 720); g.bezierCurveTo(600, 640, 700, 600, 760, 540); g.lineTo(800, 545); g.bezierCurveTo(780, 610, 760, 660, 760, 720); g.fill();
  for (const [x, y, c] of [[520, 560, '#FDEFDC'], [610, 590, '#F3C9A2'], [455, 600, '#D9E8F5']]) {
    g.fillStyle = 'rgba(40,60,30,.25)'; g.beginPath(); g.ellipse(x, y + 40, 34, 9, 0, 0, 7); g.fill();
    g.fillStyle = c; g.beginPath(); g.arc(x, y, 30, 0, 7); g.fill();
    g.fillStyle = '#4A2C25'; g.beginPath(); g.arc(x - 9, y + 2, 3.5, 0, 7); g.arc(x + 9, y + 2, 3.5, 0, 7); g.fill();
  }
  hud(g, s, '#F07060');
}

function sceneBase(g, t, s) {
  const bg = g.createLinearGradient(0, 0, 1280, 720);
  bg.addColorStop(0, '#16303A'); bg.addColorStop(1, '#22485A');
  g.fillStyle = bg; g.fillRect(0, 0, 1280, 720);
  const cx = 560; const cy = 250; const w = 64; const hh = 32;
  for (let a = 0; a < 9; a++) for (let b = 0; b < 9; b++) {
    const x = cx + (a - b) * w / 2 * 1.6;
    const y = cy + (a + b) * hh / 2 * 1.6;
    g.fillStyle = (a + b) % 2 ? '#2D5E6E' : '#2A5766';
    g.beginPath(); g.moveTo(x, y - hh * .8); g.lineTo(x + w * .8, y); g.lineTo(x, y + hh * .8); g.lineTo(x - w * .8, y); g.closePath(); g.fill();
    const k = (a * 7 + b * 3 + s) % 11;
    if (k < 3) {
      const ht = 26 + k * 18;
      const col = ['#F07060', '#E8B04B', '#7CC6B5'][k];
      g.fillStyle = col; g.beginPath(); g.moveTo(x - w * .5, y); g.lineTo(x, y + hh * .5); g.lineTo(x, y + hh * .5 - ht); g.lineTo(x - w * .5, y - ht); g.fill();
      g.fillStyle = shade(col, -30); g.beginPath(); g.moveTo(x + w * .5, y); g.lineTo(x, y + hh * .5); g.lineTo(x, y + hh * .5 - ht); g.lineTo(x + w * .5, y - ht); g.fill();
      g.fillStyle = shade(col, 25); g.beginPath(); g.moveTo(x, y - hh * .5 - ht); g.lineTo(x + w * .5, y - ht); g.lineTo(x, y + hh * .5 - ht); g.lineTo(x - w * .5, y - ht); g.fill();
    }
  }
  const pulse = (Math.sin(t * 2) + 1) / 2;
  g.strokeStyle = `rgba(253,239,220,${0.4 + pulse * 0.4})`; g.lineWidth = 3;
  g.beginPath(); g.ellipse(cx + 40, cy + 230, 40, 20, 0, 0, 7); g.stroke();
  g.fillStyle = 'rgba(10,22,28,.72)'; rr(g, 960, 110, 280, 500, 18); g.fill();
  for (let k = 0; k < 6; k++) {
    g.fillStyle = k === 1 ? 'rgba(240,112,96,.35)' : 'rgba(255,255,255,.06)';
    rr(g, 976, 130 + k * 76, 248, 62, 12); g.fill();
    g.fillStyle = ['#F07060', '#E8B04B', '#7CC6B5', '#9DB4FF', '#F3C9A2', '#C9A7EB'][k];
    g.beginPath(); g.arc(1008, 161 + k * 76, 16, 0, 7); g.fill();
    g.fillStyle = 'rgba(253,239,220,.75)'; rr(g, 1036, 150 + k * 76, 120 - (k * 17) % 50, 9, 4); g.fill();
    g.fillStyle = 'rgba(253,239,220,.35)'; rr(g, 1036, 167 + k * 76, 160, 7, 3); g.fill();
  }
  hud(g, s, '#7CC6B5');
}

function sceneCards(g, t, s) {
  const bg = g.createRadialGradient(640, 300, 50, 640, 360, 800);
  bg.addColorStop(0, '#5B3A6E'); bg.addColorStop(1, '#1E1530');
  g.fillStyle = bg; g.fillRect(0, 0, 1280, 720);
  g.fillStyle = 'rgba(255,255,255,.06)'; g.beginPath(); g.ellipse(640, 420, 520, 120, 0, 0, 7); g.fill();
  for (let k = 0; k < 3; k++) {
    const x = 400 + k * 240; const y = 260 + Math.sin(t * 1.5 + k) * 8;
    const orb = g.createRadialGradient(x - 20, y - 20, 5, x, y, 70);
    orb.addColorStop(0, ['#FFB3A6', '#B8E0FF', '#FFE3A1'][k]); orb.addColorStop(1, ['#C24A3A', '#3A6EA8', '#B7832A'][k]);
    g.fillStyle = orb; g.beginPath(); g.arc(x, y, 62, 0, 7); g.fill();
    g.fillStyle = 'rgba(0,0,0,.45)'; rr(g, x - 60, y - 100, 120, 10, 5); g.fill();
    g.fillStyle = '#F07060'; rr(g, x - 60, y - 100, 120 * (0.35 + k * 0.25), 10, 5); g.fill();
  }
  for (let k = 0; k < 5; k++) {
    g.save();
    g.translate(640 + (k - 2) * 120, 700);
    g.rotate((k - 2) * 0.12);
    const cg = g.createLinearGradient(0, -260, 0, -60);
    cg.addColorStop(0, ['#FDEFDC', '#F3C9A2', '#D9E8F5', '#E5D4F5', '#FFE3A1'][k]); cg.addColorStop(1, ['#E4C9A8', '#D99A6C', '#9DB4D8', '#B49AD8', '#E0B45A'][k]);
    g.fillStyle = 'rgba(0,0,0,.3)'; rr(g, -66, -262, 136, 196, 14); g.fill();
    g.fillStyle = cg; rr(g, -70, -270, 140, 200, 14); g.fill();
    g.strokeStyle = 'rgba(74,44,37,.35)'; g.lineWidth = 3; g.stroke();
    g.fillStyle = '#4A2C25'; g.beginPath(); g.arc(-44, -244, 16, 0, 7); g.fill();
    g.fillStyle = 'rgba(74,44,37,.25)'; rr(g, -50, -200, 100, 70, 10); g.fill();
    g.restore();
  }
  g.fillStyle = '#7CC6B5'; g.beginPath(); g.arc(120, 620, 54, 0, 7); g.fill();
  g.fillStyle = 'rgba(255,255,255,.25)'; g.beginPath(); g.arc(120, 620, 40, 0, 7); g.fill();
  hud(g, s, '#C9A7EB');
}

function sceneCity(g, t, s) {
  const bg = g.createLinearGradient(0, 0, 0, 720);
  bg.addColorStop(0, '#0F1A2E'); bg.addColorStop(1, '#3B2B4C');
  g.fillStyle = bg; g.fillRect(0, 0, 1280, 720);
  g.fillStyle = '#FDEFDC'; g.beginPath(); g.arc(1050, 140, 46, 0, 7); g.fill();
  g.fillStyle = '#0F1A2E'; g.beginPath(); g.arc(1068, 128, 42, 0, 7); g.fill();
  for (let k = 0; k < 16; k++) {
    const w = 60 + (k * 37) % 70; const x = k * 82 - 20; const hgt = 180 + (k * 53) % 260;
    g.fillStyle = k % 2 ? '#1B2440' : '#222C4D'; g.fillRect(x, 560 - hgt, w, hgt);
    for (let a = 0; a < hgt / 26 - 1; a++) for (let b = 0; b < w / 18 - 1; b++) {
      if ((a * 5 + b * 3 + k + Math.floor(t / 2)) % 7 < 2) { g.fillStyle = 'rgba(255,214,140,.8)'; g.fillRect(x + 8 + b * 16, 560 - hgt + 12 + a * 24, 7, 10); }
    }
  }
  g.fillStyle = '#121828'; g.fillRect(0, 560, 1280, 160);
  g.fillStyle = 'rgba(20,14,30,.82)'; rr(g, 140, 500, 1000, 170, 18); g.fill();
  g.strokeStyle = 'rgba(253,239,220,.25)'; g.lineWidth = 2; g.stroke();
  g.fillStyle = '#F07060'; rr(g, 170, 478, 170, 40, 10); g.fill();
  g.fillStyle = 'rgba(253,239,220,.9)'; rr(g, 196, 492, 110, 12, 6); g.fill();
  for (let k = 0; k < 3; k++) { g.fillStyle = 'rgba(253,239,220,.7)'; rr(g, 180, 548 + k * 34, 820 - k * 180, 12, 6); g.fill(); }
  hud(g, s, '#E8B04B');
}

function hud(g, s, accent) {
  g.fillStyle = 'rgba(20,12,10,.45)'; rr(g, 24, 22, 300, 64, 32); g.fill();
  g.fillStyle = '#FDEFDC'; g.beginPath(); g.arc(56, 54, 24, 0, 7); g.fill();
  g.fillStyle = '#4A2C25'; g.beginPath(); g.arc(56, 54, 18, 0, 7); g.fill();
  g.fillStyle = 'rgba(253,239,220,.9)'; rr(g, 92, 40, 120, 11, 6); g.fill();
  g.fillStyle = 'rgba(253,239,220,.25)'; rr(g, 92, 60, 200, 9, 5); g.fill();
  g.fillStyle = accent; rr(g, 92, 60, 200 * (0.3 + (s % 5) * 0.12), 9, 5); g.fill();
  for (let k = 0; k < 3; k++) {
    const x = 860 + k * 135;
    g.fillStyle = 'rgba(20,12,10,.45)'; rr(g, x, 28, 120, 40, 20); g.fill();
    g.fillStyle = ['#E8B04B', '#7CC6B5', '#F07060'][k]; g.beginPath(); g.arc(x + 20, 48, 12, 0, 7); g.fill();
    g.fillStyle = 'rgba(253,239,220,.85)'; rr(g, x + 42, 43, 60 - k * 8, 10, 5); g.fill();
  }
  for (let k = 0; k < 3; k++) {
    g.fillStyle = 'rgba(253,239,220,.88)'; g.beginPath(); g.arc(1210, 180 + k * 92, 34, 0, 7); g.fill();
    g.fillStyle = [accent, '#4A2C25', '#7CA7C8'][k]; rr(g, 1194, 164 + k * 92, 32, 32, 9); g.fill();
  }
  const btn = g.createLinearGradient(0, 600, 0, 680);
  btn.addColorStop(0, '#F58A7B'); btn.addColorStop(1, '#E0604F');
  g.fillStyle = btn; rr(g, 1010, 610, 230, 74, 18); g.fill();
  g.fillStyle = 'rgba(255,255,255,.9)'; rr(g, 1070, 640, 110, 14, 7); g.fill();
}

function sceneHome(g) {
  const bg = g.createLinearGradient(0, 0, 1280, 720);
  bg.addColorStop(0, '#4A2C25'); bg.addColorStop(1, '#C8705E');
  g.fillStyle = bg; g.fillRect(0, 0, 1280, 720);
  g.fillStyle = 'rgba(253,239,220,.85)'; rr(g, 40, 18, 70, 12, 6); g.fill();
  rr(g, 1160, 18, 80, 12, 6); g.fill();
  const cols = ['#F07060', '#7CC6B5', '#E8B04B', '#9DB4FF', '#FDEFDC', '#C9A7EB', '#86C38B', '#F3C9A2'];
  for (let k = 0; k < 8; k++) {
    const x = 250 + (k % 4) * 210; const y = 170 + Math.floor(k / 4) * 190;
    g.fillStyle = 'rgba(0,0,0,.18)'; rr(g, x + 4, y + 6, 104, 104, 26); g.fill();
    g.fillStyle = cols[k]; rr(g, x, y, 104, 104, 26); g.fill();
    g.fillStyle = 'rgba(253,239,220,.8)'; rr(g, x + 16, y + 122, 72, 10, 5); g.fill();
  }
  g.fillStyle = 'rgba(253,239,220,.18)'; rr(g, 340, 600, 600, 90, 28); g.fill();
}

const SCENES = [sceneMeadow, sceneBase, sceneCards, sceneCity];

function shade(hex, amt) {
  const n = parseInt(hex.slice(1), 16);
  const c = (v) => Math.max(0, Math.min(255, v + amt));
  return `rgb(${c(n >> 16)},${c((n >> 8) & 255)},${c(n & 255)})`;
}

async function screenshot(inst) {
  if (!canvas) { canvas = document.createElement('canvas'); canvas.width = 1280; canvas.height = 720; }
  const g = canvas.getContext('2d');
  const t = now() / 1000;
  const mode = inst._.mode;
  if (mode === 'home') sceneHome(g);
  else SCENES[inst._.scene % SCENES.length](g, t, inst.index);
  if (mode === 'recents') {
    g.fillStyle = 'rgba(14,10,9,.72)'; g.fillRect(0, 0, 1280, 720);
    for (let k = 0; k < 2; k++) {
      g.fillStyle = 'rgba(253,239,220,.12)'; rr(g, 200 + k * 470, 150, 400, 400, 28); g.fill();
      g.fillStyle = 'rgba(253,239,220,.7)'; rr(g, 230 + k * 470, 580, 160, 14, 7); g.fill();
    }
  }
  const tap = inst._.tap;
  if (tap && now() - tap.t < 1200) {
    g.strokeStyle = 'rgba(253,239,220,.9)'; g.lineWidth = 4;
    g.beginPath(); g.arc(tap.x, tap.y, 26 + (now() - tap.t) / 30, 0, 7); g.stroke();
  }
  // toDataURL encodes on this thread: headless --virtual-time-budget does not wait for toBlob's encoder thread.
  const b64 = atob(canvas.toDataURL('image/png').split(',')[1]);
  const bytes = new Uint8Array(b64.length);
  for (let k = 0; k < b64.length; k++) bytes[k] = b64.charCodeAt(k);
  return new Blob([bytes], { type: 'image/png' });
}

// ---- HTTP ------------------------------------------------------------------------------------------------------------

const json = (body, status = 200) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json' } });
const fail = (status, code, message, advice = '') => json({ error: { code, message, advice } }, status);
const STATUS_OF = { usage: 400, invalid_argument: 400, not_found: 404, busy: 409, already_exists: 409, not_ready: 422, precheck_failed: 422, canceled: 499, launch_failed: 502, timeout: 504 };
const errResp = (code, message, advice) => fail(STATUS_OF[code] || 500, code, message, advice);

function validate(b, create) {
  if (b.memory_mb != null && !MEM.includes(b.memory_mb)) return ['invalid_argument', `内存只能选 3、4、5、6 GB（收到 ${b.memory_mb} MB）`, ''];
  if (b.cpu != null && ![2, 4, 6].includes(b.cpu)) return ['invalid_argument', `CPU 核数只能是 2、4 或 6（收到 ${b.cpu}）`, ''];
  if (b.fps != null && ![30, 60].includes(b.fps)) return ['invalid_argument', `帧率只能是 30 或 60（收到 ${b.fps}）`, ''];
  if (create && b.data_partition_gb != null && (!Number.isInteger(b.data_partition_gb) || b.data_partition_gb < 16 || b.data_partition_gb > 512)) {
    return ['invalid_argument', `数据盘上限只能是 16–512 GB 的整数（收到 ${b.data_partition_gb}）`, ''];
  }
  if (b.title != null && b.title.length > 40) return ['invalid_argument', '名称太长了（最多 40 个字）', ''];
  return null;
}

const runningJobOf = (i) => db.jobs.find((j) => j.index === i && j.state === 'running');

async function handle(url, init) {
  const method = (init.method || 'GET').toUpperCase();
  const p = url.pathname;
  let body = {};
  if (typeof init.body === 'string') { try { body = JSON.parse(init.body); } catch { return errResp('usage', '请求体不是 JSON'); } }
  await sleep(p.endsWith('/screenshot') ? 40 : 80 + Math.random() * 120);

  if (p === '/api/info') return json({ version: '0.1.0', protocol: 1, root: rootInfo() });
  if (p === '/api/instances' && method === 'GET') return json(list());
  if (p === '/api/instances' && method === 'POST') {
    const bad = validate(body, true);
    if (bad) return errResp(...bad);
    let i = body.index;
    if (i == null) i = INDEXES.find((k) => !db.insts.has(k));
    if (i == null) return errResp('invalid_argument', '没有空闲的实例编号了（最多 9 台）');
    if (!INDEXES.includes(i)) return errResp('invalid_argument', `实例编号 ${i} 不可用（只能是 0–9，5 除外）`);
    if (db.insts.has(i)) return errResp('already_exists', `实例 mas_${i} 已存在`);
    await sleep(500);
    const inst = mk(i, body.title, { mem: body.memory_mb, cpu: body.cpu, fps: body.fps, disk: body.data_partition_gb, initialized: false, scene: i });
    log('info', `已新建魔改 AVD 实例 ${i}（内存 ${inst.config.memory_mb} MB / ${inst.config.cpu} 核 / 数据盘 ${inst.config.data_partition_gb} GB / 帧率 ${inst.config.fps}）`);
    emit('instances', list());
    return json(pub(inst), 201);
  }
  if (p === '/api/open') return json({});
  if (p === '/api/check') {
    await sleep(url.searchParams.get('refresh') ? 2400 : 1300);
    return json(checkResult());
  }
  if (p === '/api/jobs') return json({ jobs: clone(db.jobs.slice(0, 50)) });
  let m = /^\/api\/jobs\/([^/]+)(\/cancel)?$/.exec(p);
  if (m) {
    const j = db.jobs.find((x) => x.id === decodeURIComponent(m[1]));
    if (!j) return errResp('not_found', `没有任务 ${m[1]}`);
    if (m[2] && method === 'POST') {
      if (j.state === 'running' && (j.kind === 'start' || j.kind === 'restart')) {
        j.state = 'canceled';
        j.finished_at = iso();
        j.error = { code: 'canceled', message: '已停止等待，模拟器继续开机', advice: '' };
        emit('job', j);
        log('warn', `实例 ${j.index} 已停止等待开机，模拟器继续开机`, j.id);
      }
    }
    return json(clone(j));
  }
  m = /^\/api\/instances\/(\d+)(?:\/([a-z]+))?$/.exec(p);
  if (!m) return errResp('not_found', '没有这个接口：' + p);
  const i = Number(m[1]);
  const inst = db.insts.get(i);
  if (!inst) return errResp('not_found', `魔改 AVD 实例 ${i} 不存在`, '先新建这个实例');
  const act = m[2];
  const busyJob = runningJobOf(i);

  if (!act) {
    if (method === 'GET') return json(pub(inst));
    if (method === 'PATCH') {
      const bad = validate(body, false);
      if (bad) return errResp(...bad);
      const applied = {};
      if (body.title != null) { inst.title = body.title || inst.name; applied.title = body.title; }
      for (const k of ['memory_mb', 'cpu', 'fps']) if (body[k] != null) { inst.config[k] = body[k]; applied[k] = body[k]; }
      emit('instances', list());
      return json({ instance: pub(inst), applied, applies_next_boot: inst.running });
    }
    if (method === 'DELETE') {
      if (url.searchParams.get('confirm') !== '1') return errResp('usage', '删除要带 confirm=1');
      if (inst.running || busyJob) return errResp('busy', `魔改 AVD 实例 ${i} 未关闭，无法删除`, '先关机再删除');
      await sleep(700);
      db.insts.delete(i);
      emit('instances', list());
      return json({ index: i, name: inst.name, path: inst.path });
    }
  }
  if (act === 'start' || act === 'restart') {
    if (busyJob) return errResp('busy', `魔改 AVD 实例 ${i} ${busyJob.kind === 'stop' ? '正在关机' : '正在开机或做开机后设置'}`, '等它做完再试');
    if (act === 'start' && inst.status === 'online') {
      const j = newJob('start', i);
      run(j, [[400, () => step(j, 'launch', `实例 ${i} 已在运行（pid ${inst.pid}），不重新开机`, 50)], [600, () => step(j, 'boot', `实例 ${i} 安卓已开机`, 610)],
        [600, () => finishJob(j, { ...startResult(inst, 0, 1.6, false), launched: false })]]);
      return json(j, 202);
    }
    const j = newJob(act, i);
    if (act === 'restart' && inst.running) {
      inst.busy = 'stopping';
      emit('instances', list());
      setTimeout(() => {
        Object.assign(inst, { status: 'offline', running: false, booted: false, busy: '', pid: 0 });
        step(j, 'launch', `实例 ${i} 已关机，重新开机`, 30);
        startSeq(inst, j, body.window);
      }, 2200);
    } else startSeq(inst, j, body.window);
    return json(j, 202);
  }
  if (act === 'stop') {
    if (busyJob && busyJob.kind !== 'start' && busyJob.kind !== 'restart') return errResp('busy', `魔改 AVD 实例 ${i} 正在${busyJob.kind === 'stop' ? '关机' : '安装 APK'}`, '等它做完再试');
    const j = newJob('stop', i);
    if (!inst.running) {
      setTimeout(() => finishJob(j, { index: i, name: inst.name, method: 'already_offline', synced: false, seconds: 0.1, warnings: [] }), 300);
    } else stopSeq(inst, j, !!body.force);
    return json(j, 202);
  }
  if (act === 'install') {
    if (inst.status !== 'online') return errResp('busy', `魔改 AVD 实例 ${i} 没有开机`, '开机后再安装');
    if (busyJob) return errResp('busy', `魔改 AVD 实例 ${i} 正在做别的事`, '等它做完再试');
    const f = init.body instanceof FormData ? init.body.get('apk') : null;
    if (!f) return errResp('usage', '没有收到 APK 文件');
    const j = newJob('install', i);
    log('info', `实例 ${i} 开始安装 ${f.name}（${(f.size / 1048576).toFixed(1)} MB）`, j.id);
    setTimeout(() => finishJob(j, { index: i, apk: f.name, package: 'com.example.' + f.name.replace(/\.apk$/i, '').replace(/\W+/g, '').toLowerCase(), output: 'Success' }), 3000);
    return json(j, 202);
  }
  if (act === 'window') {
    if (!inst.running) return errResp('busy', `魔改 AVD 实例 ${i} 没有在运行`);
    return json({ index: i, visible: inst._.win ? !!body.visible : false, windows: inst._.win ? 1 : 0 });
  }
  if (act === 'input') {
    if (inst.status !== 'online') return errResp('busy', `魔改 AVD 实例 ${i} 没有开机`);
    if (body.type === 'tap') {
      inst._.tap = { x: body.x, y: body.y, t: now() };
      if (inst._.mode !== 'game') inst._.mode = 'game';
    } else if (body.type === 'key') {
      inst._.mode = body.key === 'home' ? 'home' : body.key === 'recents' ? 'recents' : 'game';
    } else return errResp('usage', 'type 只能是 tap 或 key');
    return json({});
  }
  if (act === 'screenshot') {
    window.__mockShots[i] = (window.__mockShots[i] || 0) + 1;
    if (inst.status !== 'online') return errResp('busy', `魔改 AVD 实例 ${i} 没有开机`);
    if (db.shotfail) return errResp('internal', '截图失败：adb exec-out screencap 退出码 1', '');
    const blob = await screenshot(inst);
    return new Response(blob, { status: 200, headers: { 'Content-Type': 'image/png', 'Cache-Control': 'no-store' } });
  }
  if (act === 'log') {
    const kind = url.searchParams.get('kind') || 'emulator';
    const lines = Number(url.searchParams.get('lines') || 300);
    if (!inst._.logs) return errResp('not_found', `实例 ${i} 还没有${kind === 'logcat' ? ' logcat ' : '模拟器'}日志`, '开过一次机才会有日志');
    const text = kind === 'logcat' ? logcat(inst, lines) : emulatorLog(inst, lines);
    return new Response(text, { status: 200, headers: { 'Content-Type': 'text/plain; charset=utf-8' } });
  }
  return errResp('not_found', '没有这个接口：' + p);
}

// ---- events ----------------------------------------------------------------------------------------------------------

function emit(type, data) {
  const s = JSON.stringify(data);
  for (const es of db.streams) es._send(type, s);
}

class MockEventSource {
  static CONNECTING = 0;
  static OPEN = 1;
  static CLOSED = 2;
  constructor(url) {
    this.url = url;
    this.readyState = 0;
    this.onopen = null;
    this.onerror = null;
    this.onmessage = null;
    this.l = {};
    setTimeout(() => this.connect(), 60);
  }
  connect() {
    if (this.readyState === 2) return;
    if (db.offline) {
      this.readyState = 0;
      this.fire('error', new Event('error'));
      this.retry = setTimeout(() => this.connect(), 5000);
      return;
    }
    this.readyState = 1;
    db.streams.add(this);
    this.fire('open', new Event('open'));
    this._send('instances', JSON.stringify(list()));
    if (!db.replayed) {
      // the scenario's history, so the activity panel has lines to show (the real server only streams new ones)
      db.replayed = true;
      for (const l of db.logs || []) this._send('log', JSON.stringify(l));
    }
    if (db.drop) {
      db.drop = false;
      setTimeout(() => {
        db.offline = true;
        db.streams.delete(this);
        this.readyState = 0;
        this.fire('error', new Event('error'));
        this.retry = setTimeout(() => this.connect(), 5000);
      }, 1500);
    }
  }
  _send(type, data) { this.fire(type, new MessageEvent(type, { data })); }
  fire(type, ev) {
    const h = this['on' + type];
    if (h) h.call(this, ev);
    for (const f of this.l[type] || []) f.call(this, ev);
  }
  addEventListener(t, f) { (this.l[t] ||= []).push(f); }
  removeEventListener(t, f) { this.l[t] = (this.l[t] || []).filter((x) => x !== f); }
  close() { this.readyState = 2; db.streams.delete(this); clearTimeout(this.retry); }
}

export function install(scenario, params) {
  db.offline = params.get('offline') === '1';
  db.drop = params.get('drop') === '1';
  db.shotfail = params.get('shotfail') === '1';
  window.__mockShots = {};
  setup(scenario);
  const real = window.fetch.bind(window);
  window.fetch = (input, init = {}) => {
    const url = new URL(typeof input === 'string' ? input : input.url, location.href);
    if (!url.pathname.startsWith('/api/')) return real(input, init);
    return handle(url, init);
  };
  window.EventSource = MockEventSource;
  setInterval(() => emit('instances', list()), 3000);
  setInterval(() => emit('ping', {}), 15000);
  console.info(`[mock] scenario "${scenario}" with ${db.insts.size} instances`);
}
