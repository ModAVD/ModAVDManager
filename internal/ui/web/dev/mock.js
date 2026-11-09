// Mock backend for design work: ?mock=normal|empty|notready|busy|many replaces fetch('/api/…') and EventSource with
// an in-memory fake that follows internal/ui/API.md. Extra params: drop=1 (the event stream drops after 1.5 s and
// stays down), offline=1 (it never connects), shotfail=1 (every screenshot answers 500), diag=components-error|components-fail
// (component status request or fetch fails), store=download|import (the root has the Google Play Store's five files,
// fetched or imported), storefail=1 (an instance's 补全 Google Play 商店 fails in the guest and leaves it partial),
// nogsf=1 (复制 GSF ID finds no ID yet). window.__mockShots counts
// screenshot requests per instance. Never used in production: the server does not serve /dev/.
// Screens are drawn on a canvas: abstract shapes only, no real game content.

const ROOT = 'X:\\ModAVD';
const INDEXES = [0, 1, 2, 3, 4, 6, 7, 8, 9];
const MEM = [3072, 4096, 5120, 6144];
const db = { insts: new Map(), jobs: [], streams: new Set(), seq: 40, check: 'ok', offline: false, drop: false, ready: true, diagMode: '', componentsReady: false,
  componentSource: { source: 'auto', mirror: '', mirror_urls: [], configured: false },
  // the root's Google Play Store files (sdk\playstore): source is download | import once they are there
  store: { ready: false, source: '' } };
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
    config: { memory_mb: o.mem || 6144, cpu: o.cpu || 6, fps: o.fps || 60, data_partition_gb: o.disk || 64, resolution: o.resolution || 720, width: o.resolution === 1080 ? 1920 : 1280, height: o.resolution === 1080 ? 1080 : 720, dpi: o.resolution === 1080 ? 280 : 240 },
    initialized, launcher: initialized ? 'µLauncher' : '',
    renderer: initialized ? (o.sw ? SWIFT : GPU) : '', software_renderer: !!o.sw,
    path: `${ROOT}\\avd\\mas_${i}.avd`,
    app_proxy: null,
    // the execution bridge (off | pending | installed | unknown) and the Google Play Store's record
    bridge: o.bridge || (initialized ? 'installed' : 'pending'), play_store: !!o.store, play_store_partial: false, gles32: false,
  };
  Object.defineProperty(inst, '_', { value: { scene: o.scene ?? i, mode: 'game', win: false, fail: o.fail || null, logs: initialized || st !== 'offline', tap: null }, enumerable: false });
  db.insts.set(i, inst);
  if (o.proxy) setProxy(inst, { enabled: true, server: 'socks5://10.0.2.2:7890', apps: o.proxy });
  return inst;
}

// ---- 按应用走代理: the setting per instance, the guest's apps, the cards' marks (the proxy event)
const PKGS = [
  { package: 'com.hypergryph.arknights', name: '明日方舟（官服）', uid: 10123, system: false },
  { package: 'com.shenlan.m.reverse1999', name: '重返未来：1999', uid: 10131, system: false },
  { package: 'com.example.globalgame', name: '', uid: 10140, system: false },
  { package: 'de.jrpie.android.launcher', name: '', uid: 10101, system: false },
  { package: 'com.google.android.gms', name: 'Google Play 服务', uid: 10120, system: true },
];

function setProxy(inst, s) {
  inst._.proxy = s;
  inst.app_proxy = { enabled: s.enabled, server: s.server, apps: s.apps.length };
}

function proxyActive(inst) {
  const s = inst._.proxy;
  return !!s && s.enabled && s.apps.length > 0;
}

function proxyMark(inst) {
  if (!proxyActive(inst) || inst.status !== 'online' || inst.busy) return null;
  const s = inst._.proxy;
  if (db.proxyfail && inst.index === 0) return { index: inst.index, tone: 'err', text: '代理连不上（名单里的应用现在断网）：127.0.0.1:7890 上没有程序在听' };
  const missing = s.apps.filter((p) => !PKGS.some((k) => k.package === p)).length;
  return { index: inst.index, tone: 'ok', text: `已生效：${s.apps.length} 个应用${missing ? `（${missing} 个还没装）→ ` : ' → '}${s.server}` };
}

function proxyGuest(inst) {
  if (!proxyActive(inst) || inst.status !== 'online') return null;
  const s = inst._.proxy;
  return { version: '0.4.0', running: true, pid: 2012, rules: 'ok', rules_error: '', error: '',
    server: { url: s.server, tcp: db.proxyfail && inst.index === 0 ? 'failed' : 'ok', udp: 'ok', last_error: '', last_error_at: '', last_ok_at: iso() },
    apps: s.apps.map((p) => ({ package: p, uid: (PKGS.find((k) => k.package === p) || {}).uid || 0 })),
    counters: { tcp: 42, tcp_failed: 0, udp_sessions: 3, udp_dropped: 0, dns: 17, dns_failed: 0, bytes_up: 81234, bytes_down: 1203450 } };
}

function proxyView(inst, applied = '', notes = []) {
  const s = inst._.proxy || { enabled: false, server: '', apps: [] };
  return { index: inst.index, enabled: s.enabled, server: s.server, apps: [...s.apps], dns: '', applied, notes, guest: proxyGuest(inst), guest_error: '', error: '', mark: proxyMark(inst) };
}

const marks = () => ({ marks: [...db.insts.values()].map(proxyMark).filter(Boolean) });

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
    mk(9, '公主连结', { status: 'offline', mem: 5120, cpu: 4, bridge: 'off' });
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
    setTimeout(() => step(s, 'vulkan_fix', '实例 1 客体库补丁（Vulkan 驱动、ARM 转译层）', 900), 9000);
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
  mk(0, '明日方舟 · 主号', { status: 'online', upMin: 133, scene: 0, proxy: ['com.example.globalgame', 'com.google.android.gms'] });
  mk(1, '星穹铁道', { status: 'offline', mem: 6144, proxy: ['com.miHoYo.hkrpg'], store: true });
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
  if (db.check === 'notready' && !db.componentsReady) Object.assign(items[1], { ok: false, reason: '这个目录里缺少：平台工具（adb）、系统镜像 Android 14 Google APIs x86_64', advice: '在管理器里点「补全组件」，或运行 tools\\modavd.exe components fetch；它会先显示《Android 软件开发套件许可协议》，输入 y 同意后从官方渠道下载 adb 和系统镜像（约 1.5 GB，中断了再运行会接着下），完成后再检查一次' });
  if (db.check === 'notready') {
    Object.assign(items[3], { ok: false, reason: '硬件虚拟化（Windows 虚拟机监控程序平台）不可用，ModAVD 无法启动（检查结果：WHPX is not installed）', advice: '先在 BIOS/UEFI 里打开 VT-x/SVM，再在「启用或关闭 Windows 功能」里勾选「Windows 虚拟机监控程序平台」，然后重启' });
    Object.assign(items[4], { ok: null, reason: '查不了 Vulkan：探测程序 8 秒内没有结束' });
    Object.assign(items[2], { reason: '内测包 0.35.0（37.2.10）' });
    // the host IME is optional and stays missing, also after a fetch (it is not a downloaded component)
    comps[4].installed = false; comps[4].version = '';
  }
  if (db.check === 'notready' && !db.componentsReady) {
    comps[0].installed = false; comps[0].version = '';
    comps[2].installed = false; comps[2].version = ''; comps[2].build = '';
  }
  const ready = !items.some((x) => x.blocking && x.ok === false);
  return { ...rootInfo(), ready, items, components: comps };
}

// ---- the Google Play Store: the root's five files (GET /api/components play_store) -------------------------------------

const STORE_FILES = [['Phonesky.apk', '/product/priv-app/Phonesky/Phonesky.apk', 74664153],
  ['PrebuiltGmsCore.apk', '/product/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk', 144060779],
  ['GoogleServicesFramework.apk', '/system_ext/priv-app/GoogleServicesFramework/GoogleServicesFramework.apk', 5744939],
  ['PartnerSetupPrebuilt.apk', '/product/priv-app/PartnerSetupPrebuilt/PartnerSetupPrebuilt.apk', 726118],
  ['play_store_fsi_cert.der', '/product/etc/security/fsverity/play_store_fsi_cert.der', 1422]];
const STORE_ZIP = 1510752654;

function storeStatus() {
  const s = db.store;
  return { dir: ROOT + '\\sdk\\playstore', ready: s.ready, source: s.ready ? s.source : '', version: s.ready ? '14' : '',
    files: STORE_FILES.map(([name, guest, size]) => ({ name, guest, size, present: s.ready })) };
}

// GET /api/components: the components, then the store's summary item
function componentList() {
  return [...checkResult().components,
    { id: 'play-store', name: 'Google Play 商店', version: db.store.ready ? '14' : '', build: '', installed: db.store.ready, optional: true, local_sdk: false }];
}

const sdkJob = () => db.jobs.find((j) => (j.kind === 'components' || j.kind === 'playstore-import') && j.state === 'running');

// 使用我自己的文件: the mock knows a file by its name (the real import by its size, then its hash): *.zip is the Play
// image, the five names are the files, a name with "bad" fails its hash, anything else is not one of them.
function runImport(j, files) {
  const named = files.map((f) => ({ f, zip: /\.zip$/i.test(f.name), slot: STORE_FILES.findIndex(([n]) => n === f.name) }));
  const unknown = named.find((x) => !x.zip && x.slot < 0);
  const zip = named.find((x) => x.zip);
  const steps = [];
  for (const x of named) if (x.zip || x.slot >= 0) steps.push(['verifying', `校验 ${x.f.name}${x.zip ? '（SHA-1）' : `（${STORE_FILES[x.slot][0]}）`}`, x.zip ? STORE_ZIP : STORE_FILES[x.slot][2]]);
  if (zip) steps.push(['extracting', `解压 ${zip.f.name}`, 3101687808]);
  log('info', 'ModAVD：用自己的文件补全 Google Play 商店', j.id);
  let k = 0;
  const timer = setInterval(() => {
    if (j.state !== 'running') { clearInterval(timer); return; }
    if (unknown) {
      clearInterval(timer);
      return failJob(j, 'components_rejected', `${unknown.f.name}（${unknown.f.size} 字节）不是 Google Play 版镜像的压缩包 x86_64-34_r14.zip，也不是商店的五个文件（${STORE_FILES.map(([n]) => n).join('、')}）之一`, '', { reason: 'unknown_file' });
    }
    if (k < steps.length) {
      const [step, name, total] = steps[k++];
      j.progress = { step, name, message: `  ${name}  100.0%  ${(total / 1048576).toFixed(1)} MB / ${(total / 1048576).toFixed(1)} MB`, done: total, total };
      emit('job', j);
      log('info', name + '…', j.id);
      const bad = named.find((x) => /bad/i.test(x.f.name));
      if (bad && name.includes(bad.f.name)) {
        clearInterval(timer);
        failJob(j, 'components_rejected', bad.zip
          ? `${bad.f.name} 的 SHA-1 是 0123…（mock），应为 01d32617fd1937e540faf8731d70cf50e35af854：不是 Google 仓库里那份 Google Play 版镜像（x86_64-34_r14.zip）`
          : `${bad.f.name} 和 Google Play 版镜像 r14 里的 ${STORE_FILES[bad.slot][0]} 大小一样，SHA-256 却对不上（mock）`, '', { reason: 'mismatch' });
      }
      return;
    }
    clearInterval(timer);
    const got = new Set(named.filter((x) => !x.zip).map((x) => x.slot));
    if (!zip && !db.store.ready && got.size < STORE_FILES.length) {
      const missing = STORE_FILES.filter((_, s) => !got.has(s)).map(([n]) => n);
      return failJob(j, 'components_rejected', `还缺 ${missing.join('、')}，什么也没有改`, '把五个文件一起给出，或者给 Google Play 版镜像的压缩包 x86_64-34_r14.zip', { reason: 'incomplete' });
    }
    const skipped = db.store.ready;
    if (!skipped) db.store = { ready: true, source: 'import' };
    finishJob(j, { root: ROOT, from: zip ? 'zip' : 'files', used: files.map((f) => f.name), kept: [], skipped, set_aside: '', status: storeStatus() });
  }, 700);
}

// 开启 / 关闭报告 GLES 3.2 on an instance: boots it cold once (an instance that was off is booted for it and stopped again).
function runGLES32(inst, j, action) {
  const i = inst.index;
  const was = inst.running;
  const set = (o) => { Object.assign(inst, o); emit('instances', list()); };
  const off = () => { set({ status: 'offline', running: false, booted: false, busy: '', pid: 0, started_at: '' }); inst.screencap.shm_ready = false; };
  const boot = () => set({ status: 'starting', running: true, busy: 'starting', pid: 10000 + Math.floor(Math.random() * 9000), started_at: iso() });
  const up = () => { set({ status: 'online', booted: true, busy: '' }); inst.screencap.shm_ready = true; };
  const doing = action === 'on' ? '正在开启报告 GLES 3.2（会冷启动一次）' : '正在关闭报告 GLES 3.2（会冷启动一次）';
  j.progress = { step: j.kind, message: `实例 ${i} ${doing}`, done: 0, total: 1000 };
  emit('job', j);
  log('info', `实例 ${i} ${doing}`, j.id);
  const steps = [];
  if (!was) steps.push([600, boot], [2500, up]);
  steps.push([1200, () => log('info', `实例 ${i} ${action === 'on' ? '写入' : '去掉'} /vendor/build.prop 里的 ro.opengles.version`, j.id)],
    [800, () => set({ busy: 'stopping' })], [1600, off], [700, boot], [2600, up]);
  if (!was) steps.push([800, () => set({ busy: 'stopping' })], [1400, off]);
  steps.push([600, () => {
    inst.gles32 = action === 'on';
    finishJob(j, { index: i, action, state: action, enabled: action === 'on', prop: action === 'on' ? '196610' : '196609', restarted: true, stopped: !was,
      notes: inst.play_store ? ['Play 商店会记着旧的设备信息；要它按新版本重新判断兼容，到 设置 → 应用 → Google Play 商店 → 存储，清除数据（账号不会退出）'] : [], warnings: [] });
  }]);
  run(j, steps);
}

// 补全 / 移除 Google Play 商店 on an instance: boots it cold once (an instance that was off is booted for it and stopped
// again); the card follows the instance through the stop and the boot.
function runPlayStore(inst, j, action) {
  const i = inst.index;
  const was = inst.running;
  const set = (o) => { Object.assign(inst, o); emit('instances', list()); };
  const stopIt = () => { set({ busy: 'stopping' }); };
  const off = () => { set({ status: 'offline', running: false, booted: false, busy: '', pid: 0, started_at: '' }); inst.screencap.shm_ready = false; };
  const boot = () => set({ status: 'starting', running: true, busy: 'starting', pid: 10000 + Math.floor(Math.random() * 9000), started_at: iso() });
  const up = () => { set({ status: 'online', booted: true, busy: '' }); inst.screencap.shm_ready = true; };
  const doing = action === 'install' ? '正在补全 Google Play 商店（会冷启动一次）' : '正在移除 Google Play 商店（会冷启动一次）';
  j.progress = { step: j.kind, message: `实例 ${i} ${doing}`, done: 0, total: 1000 };
  emit('job', j);
  log('info', `实例 ${i} ${doing}`, j.id);
  const steps = [];
  if (!was) steps.push([600, boot], [2500, up]);
  steps.push([1500, () => log('info', action === 'install' ? `实例 ${i} 推送商店的五个文件，放进系统覆盖层` : `实例 ${i} 卸掉 Google Play 装的更新，删掉商店的文件`, j.id)]);
  if (action === 'install' && db.storefail) {
    // the undo could not take everything back: recorded partial (移除 is offered too); an instance that was off goes off again
    const failNow = () => {
      inst.play_store_partial = true;
      failJob(j, 'launch_failed', `实例 ${i} 没装好 Google Play 商店（partial）：install failed: cannot copy /product/priv-app/PrebuiltGmsCore/PrebuiltGmsCore.apk（mock）`,
        `放进去的文件没能全部撤回：运行 modavd playstore ${i} uninstall 移除（管理器里是「移除 Google Play 商店」），或者解决问题后再补全一次`);
    };
    if (!was) steps.push([800, stopIt], [1400, off]);
    steps.push([800, failNow]);
    run(j, steps);
    return;
  }
  steps.push([800, stopIt], [1600, off], [700, boot], [2600, up]);
  if (!was) steps.push([800, stopIt], [1400, off]);
  steps.push([600, () => {
    inst.play_store = action === 'install';
    inst.play_store_partial = false;
    const state = action === 'install' ? 'installed' : 'absent';
    finishJob(j, { index: i, action, state, installed: state === 'installed', files: STORE_FILES.map(([, g]) => `${state === 'installed' ? 'play' : 'absent'} ${g}`),
      restarted: true, stopped: !was, warnings: [] });
  }]);
  run(j, steps);
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
  if (j.kind === 'start') j.progress = { step: 'done', message: `实例 ${j.index} 已就绪（共 ${result.total_seconds.toFixed(1)} 秒）`, done: 1000, total: 1000 };
  emit('job', j);
  emit('instances', list());
}

function failJob(j, code, message, advice, details) {
  if (j.state !== 'running') return;
  j.state = 'failed';
  j.finished_at = iso();
  j.error = { code, message, advice, ...(details ? { details } : {}) };
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

// [stage, name, done MB, total MB, speed MB/s]: a download's message is the real backend's line (internal/components
// progress.go), name and sizes included; the collect steps only have a message
const mb = (x) => (x >= 1024 ? `${(x / 1024).toFixed(2)} GB` : `${x.toFixed(1)} MB`);
function diagProgress(j, stages, done) {
  if (j.state !== 'running') return;
  const [step, name, d, total, speed] = stages[Math.min(done, stages.length - 1)];
  const pct = total ? ((d * 100) / total).toFixed(1).padStart(5) : '';
  const message = step === 'downloading' ? `  ${name}  ${pct}%  ${mb(d)} / ${mb(total)}  ${mb(speed)}/s  剩余 00:${String(Math.round((total - d) / speed)).padStart(2, '0')}`
    : step === 'extracting' ? `  解压 ${name}  ${pct}%  ${mb(d)} / ${mb(total)}` : name;
  j.progress = { step, name, message, done: d * 1048576, total: total * 1048576, speed: speed * 1048576 };
  emit('job', j); emit('instances', list());
}

// the license each source serves (a custom one says its address), with a stand-in for its SHA-256
function mockLicense(source, urls) {
  const names = { auto: 'Google 中国下载（googledownloads.cn）', googledownloads_cn: 'Google 中国下载（googledownloads.cn）', tencent: '腾讯云镜像（mirrors.cloud.tencent.com）', google: 'Google 官方（dl.google.com）' };
  let from = names[source];
  if (source === 'custom') {
    if (!urls.length) return { error: ['usage', '自定义组件下载源至少要有一个地址'] };
    if (urls.some((u) => !/^https?:\/\/[^/?#\s@]+/i.test(u) || /[?#\s]/.test(u))) return { error: ['invalid_argument', `自定义下载地址要写 http:// 或 https:// 开头、带主机名的目录地址，收到「${urls[0]}」`] };
    if (urls.every((u) => /unreachable/.test(u))) return { error: ['components_unavailable', '所有镜像都取不到许可协议（mock）', '检查地址，或换一个下载源'] };
    from = `自定义镜像（${urls[0].replace(/\/?$/, '/')}）`;
  }
  if (!from) return { error: ['invalid_argument', `组件下载源只能是 auto、googledownloads_cn、tencent、google 或 custom，收到「${source}」`] };
  const license = `Terms and Conditions (mock, served by ${from})\n\nThis is the Android Software Development Kit License Agreement as ${from} serves it.\n\n1. Introduction\n1.1 The Android Software Development Kit (referred to in the License Agreement as the "SDK") …`;
  let x = 0x811c9dc5;
  for (const ch of license) x = Math.imul(x ^ ch.codePointAt(0), 16777619) >>> 0;
  return { license, license_source: from, license_sha256: x.toString(16).padStart(8, '0').repeat(8) };
}

function runDiag(j, kind = j.kind, store = false) {
  const stages = kind === 'components'
    ? [['downloading', '平台工具（adb）', 2.7, 8.0, 1.1], ['downloading', '平台工具（adb）', 6.4, 8.0, 1.3], ['extracting', '平台工具（adb）', 8.0, 8.0, 0],
      ['downloading', '系统镜像（Android 14 Google APIs x86_64）', 310, 1480, 24], ['downloading', '系统镜像（Android 14 Google APIs x86_64）', 960, 1480, 26],
      ['extracting', '系统镜像（Android 14 Google APIs x86_64）', 1480, 1480, 0]]
    : [['collect', '正在收集目录与版本', 1, 12, 0.4], ['collect', '正在收集日志', 4, 12, 0.8], ['collect', '正在收集现场状态', 8, 12, 1.1], ['zip', '正在压缩问题包', 11, 12, 1.4], ['done', '问题包已生成', 12, 12, 0]];
  // play_store: Google's Play image too, of which only the five files stay
  if (store) stages.push(['downloading', 'Google Play 商店（取自 Android 14 Google Play x86_64 镜像 r14）', 420, 1441, 25], ['downloading', 'Google Play 商店（取自 Android 14 Google Play x86_64 镜像 r14）', 1180, 1441, 27],
    ['extracting', 'Google Play 商店（取自 Android 14 Google Play x86_64 镜像 r14）', 2958, 2958, 0]);
  if (kind === 'components') ['ModAVD：补全 Google 组件（adb、Android 14 系统镜像）', '已在管理器里同意许可协议。'].forEach((m) => log('info', m, j.id));
  let n = 0;
  const timer = setInterval(() => {
    if (j.state !== 'running') { clearInterval(timer); return; }
    diagProgress(j, stages, n);
    n++;
    // components-fail: the first fetch of the page fails half way (so 重试 can be tried), the next ones work
    if (kind === 'components' && db.diagMode === 'components-fail' && !db.diagFailed && n === 3) {
      db.diagFailed = true;
      clearInterval(timer); failJob(j, 'components_unavailable', '所有镜像都没下成：系统镜像（Android 14 Google APIs x86_64）', '检查网络或换一个下载源后重试；已经下好的部分会接着下'); return;
    }
    if (n >= stages.length) {
      clearInterval(timer);
      if (kind === 'components') {
        db.componentsReady = true;
        if (store) db.store = { ready: true, source: 'download' };
        finishJob(j, { root: ROOT, license: 'Android SDK License', license_source: 'Google 中国下载（mock）', components: componentList(), downloads: [] });
      } else finishJob(j, { zip_path: `${ROOT}\\logs\\ModAVD-问题信息-mock.zip`, zip_size: '1.2 MB', problems: [], mas_found: true, entries: ['summary.txt'],
        privacy_note: '隐私提示（mock）：问题包只保存在本机，不会自动上传。\r\n- 可以公开：summary.txt 和日志。\r\n- 不要公开：crash 文件夹。' });
    }
  }, 500);
}

function startSeq(inst, j, window) {
  const i = inst.index;
  if (!db.ready) {
    setTimeout(() => failJob(j, 'not_ready', '这个目录里缺少：平台工具（adb）、系统镜像 Android 14 Google APIs x86_64', '在管理器里点「补全组件」，或运行 tools\\modavd.exe components fetch；完成后再检查一次'), 600);
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
      step(j, 'vulkan_fix', `实例 ${i} 客体库补丁（Vulkan 驱动、ARM 转译层）`, 900);
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
    if (s.index === inst.index && s.state === 'running' && s.kind === 'start') {
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
const fail = (status, code, message, advice = '', details) => json({ error: { code, message, advice, ...(details ? { details } : {}) } }, status);
const STATUS_OF = { usage: 400, invalid_argument: 400, not_found: 404, busy: 409, already_exists: 409, not_ready: 422, precheck_failed: 422, unsupported: 422, canceled: 499, launch_failed: 502, proxy_unavailable: 502, timeout: 504 };
const PKG = /^[A-Za-z][A-Za-z0-9_]*(\.[A-Za-z][A-Za-z0-9_]*)+$/;

// what speaks at a typed proxy address: 7890 / 7897 a mixed port, 7891 an HTTP proxy, anything else nothing
function probeAt(server) {
  const m = /^(?:(socks5h?|http):\/\/)?(localhost|\d+\.\d+\.\d+\.\d+):(\d+)$/i.exec((server || '').trim());
  if (!m) return ['invalid_argument', `代理地址要写成 socks5://IP:端口 或 http://IP:端口，收到「${server}」`];
  const host = /^(localhost|127\.)/i.test(m[2]) ? '10.0.2.2' : m[2];
  const port = Number(m[3]);
  const kind = port === 7891 ? 'http' : [7890, 7897].includes(port) ? 'socks5' : null;
  if (!kind) return ['proxy_unavailable', `127.0.0.1:${port} 上没有程序在听（连接被拒绝）`, '代理软件开着时再试'];
  const notes = [];
  if (host !== m[2]) notes.push(`实例里连宿主要用 10.0.2.2，代理地址记成 ${kind}://${host}:${port}`);
  if (m[1] && m[1].toLowerCase().replace('socks5h', 'socks5') !== kind) notes.push(`写的是 ${m[1]}://，这个端口说的是 ${kind === 'socks5' ? 'SOCKS5' : 'HTTP 代理'}，改成了 ${kind}://${host}:${port}`);
  return { server: `${kind}://${host}:${port}`, host_addr: `127.0.0.1:${port}`, kind, udp: kind === 'socks5' ? 'ok' : 'unsupported', notes };
}
const errResp = (code, message, advice) => fail(STATUS_OF[code] || 500, code, message, advice);

function validate(b, create) {
  if (b.memory_mb != null && !MEM.includes(b.memory_mb)) return ['invalid_argument', `内存只能选 3、4、5、6 GB（收到 ${b.memory_mb} MB）`, ''];
  if (b.cpu != null && ![2, 4, 6].includes(b.cpu)) return ['invalid_argument', `CPU 核数只能是 2、4 或 6（收到 ${b.cpu}）`, ''];
  if (b.resolution != null && ![1080, 720].includes(b.resolution)) return ['invalid_argument', '分辨率只能是 1080 或 720', ''];
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
    if ('play_store' in body) return errResp('usage', '新建不带 Google Play 商店（没有 play_store 字段）', '建好后在实例的「更多操作」里补全');
    const bad = validate(body, true);
    if (bad) return errResp(...bad);
    let i = body.index;
    if (i == null) i = INDEXES.find((k) => !db.insts.has(k));
    if (i == null) return errResp('invalid_argument', '没有空闲的实例编号了（最多 9 台）');
    if (!INDEXES.includes(i)) return errResp('invalid_argument', `实例编号 ${i} 不可用（只能是 0–9，5 除外）`);
    if (db.insts.has(i)) return errResp('already_exists', `实例 mas_${i} 已存在`);
    await sleep(500);
    const inst = mk(i, body.title, { mem: body.memory_mb, cpu: body.cpu, fps: body.fps, resolution: body.resolution || 1080, disk: body.data_partition_gb, initialized: false, scene: i });
    log('info', `已新建 ModAVD 实例 ${i}（内存 ${inst.config.memory_mb} MB / ${inst.config.cpu} 核 / 数据盘 ${inst.config.data_partition_gb} GB / 帧率 ${inst.config.fps}）`);
    emit('instances', list());
    return json(pub(inst), 201);
  }
  if (p === '/api/open') {
    if (body.what === 'collect-info') {
      const cj = db.jobs.find((j) => j.id === body.job);
      return cj && cj.kind === 'collect-info' && cj.state === 'done' ? json({}) : errResp('not_found', `任务 ${body.job} 不是做完了的收集问题信息`);
    }
    if (!['root', 'logs', 'instance'].includes(body.what)) return errResp('invalid_argument', `what 只能是 root、logs、instance 或 collect-info（给的是 "${body.what}"）`);
    return json({});
  }
  if (p === '/api/components' && method === 'GET') {
    if (db.diagMode === 'components-error') return errResp('internal', '组件状态暂不可用', '稍后重试');
    return json({ root: ROOT, components: componentList(), play_store: storeStatus() });
  }
  if (p === '/api/components/import-playstore' && method === 'POST') {
    if (!(init.body instanceof FormData)) return errResp('usage', '导入要用 multipart/form-data 上传，文件字段名 file（每个文件一个）');
    const running = sdkJob();
    if (running) return errResp('busy', `任务 ${running.kind === 'components' ? '补全组件' : '导入 Google Play 商店的文件'} 正在运行（任务 ${running.id}），等它做完再试`);
    const files = init.body.getAll('file');
    if (!files.length) return errResp('usage', '上传里没有文件字段 file');
    if (files.length > 8) return errResp('invalid_argument', '一次最多导入 8 个文件');
    const empty = files.find((f) => !f.size);
    if (empty) return errResp('invalid_argument', `上传的 ${empty.name} 是空文件`);
    await sleep(Math.min(3000, files.reduce((n, f) => n + f.size, 0) / 1e6)); // the upload itself
    const j = newJob('playstore-import', -1);
    runImport(j, files);
    return json(j, 202);
  }
  if (p === '/api/components/license' && method === 'GET') {
    await sleep(400);
    const lic = mockLicense(url.searchParams.get('source') || db.componentSource.source, url.searchParams.getAll('mirror_urls'));
    return lic.error ? errResp(...lic.error) : json(lic);
  }
  if (p === '/api/components/source' && method === 'GET') return json({ ...db.componentSource, mirror_urls: [...db.componentSource.mirror_urls] });
  if (p === '/api/components/source' && method === 'PUT') {
    const source = String(body.source || '').toLowerCase();
    const urls = Array.isArray(body.mirror_urls) ? body.mirror_urls.map((x) => String(x).trim()).filter(Boolean) : [];
    if (!['auto', 'googledownloads_cn', 'tencent', 'google', 'custom'].includes(source)) return errResp('invalid_argument', '组件下载源不认识');
    if (source === 'custom' && !urls.length) return errResp('usage', '自定义组件下载源至少需要一个地址');
    if (source === 'custom' && urls.some((x) => !/^https?:\/\//i.test(x))) return errResp('invalid_argument', '自定义组件下载源必须是 HTTP(S) 地址');
    const normalized = urls.map((x) => x.endsWith('/') ? x : x + '/');
    db.componentSource = { source, mirror: source === 'custom' ? '' : source === 'auto' ? '' : source, mirror_urls: normalized, configured: true };
    return json({ ...db.componentSource, mirror_urls: [...normalized] });
  }
  if (p === '/api/components/fetch' && method === 'POST') {
    if (body.accept_license !== true) return errResp('usage', '补全组件必须明确同意许可协议');
    const lic = mockLicense(body.source || db.componentSource.source, body.mirror_urls || db.componentSource.mirror_urls);
    if (lic.error || body.license_sha256 !== lic.license_sha256) return errResp('usage', '补全组件要带上看过的许可协议的 license_sha256');
    if (sdkJob()) return errResp('busy', sdkJob().kind === 'components' ? '组件补全正在进行' : '正在导入 Google Play 商店的文件，等它做完再试');
    const j = newJob('components', -1); runDiag(j, 'components', body.play_store === true && !db.store.ready); return json(j, 202);
  }
  if (p === '/api/diag/collect' && method === 'POST') {
    if (db.jobs.some((j) => j.kind === 'collect-info' && j.state === 'running')) return errResp('busy', '收集问题信息正在进行');
    const j = newJob('collect-info', -1); runDiag(j); return json(j, 202);
  }
  if (p === '/api/diag/report-config' && method === 'GET') return json({ text: `ModAVD mock 配置信息\nroot=${ROOT}` });
  if (p === '/api/check') {
    await sleep(url.searchParams.get('refresh') ? 2400 : 1300);
    return json(checkResult());
  }
  if (p === '/api/jobs') return json({ jobs: clone(db.jobs.slice(0, 50)) });
  if (p === '/api/proxy/suggest') {
    await sleep(200);
    return json({ server: 'socks5://10.0.2.2:7890', notes: ['代理地址取自Windows 代理设置：127.0.0.1:7890', '实例里连宿主要用 10.0.2.2，代理地址记成 socks5://10.0.2.2:7890'] });
  }
  if (p === '/api/proxy/probe' && method === 'POST') {
    await sleep(600);
    const r = probeAt(body.server);
    return Array.isArray(r) ? errResp(...r) : json(r);
  }
  let m = /^\/api\/jobs\/([^/]+)(\/cancel)?$/.exec(p);
  if (m) {
    const j = db.jobs.find((x) => x.id === decodeURIComponent(m[1]));
    if (!j) return errResp('not_found', `没有任务 ${m[1]}`);
    if (m[2] && method === 'POST') {
      if (j.state === 'running' && (j.kind === 'start' || j.kind === 'components' || j.kind === 'collect-info' || j.kind === 'playstore-import')) {
        j.state = 'canceled';
        j.finished_at = iso();
        j.error = { code: 'canceled', message: j.kind === 'start' ? '已停止等待，模拟器继续开机' : '已取消后台任务', advice: '' };
        emit('job', j);
        log('warn', `实例 ${j.index} 已停止等待开机，模拟器继续开机`, j.id);
      }
    }
    return json(clone(j));
  }
  m = /^\/api\/instances\/(\d+)(?:\/([a-z-]+))?$/.exec(p);
  if (!m) return errResp('not_found', '没有这个接口：' + p);
  const i = Number(m[1]);
  const inst = db.insts.get(i);
  if (!inst) return errResp('not_found', `ModAVD 实例 ${i} 不存在`, '先新建这个实例');
  const act = m[2];
  const busyJob = runningJobOf(i);

  if (!act) {
    if (method === 'GET') return json(pub(inst));
    if (method === 'PATCH') {
      const bad = validate(body, false);
      if (bad) return errResp(...bad);
      const applied = {};
      if (body.title != null) { inst.title = body.title || inst.name; applied.title = body.title; }
      if (body.resolution != null) {
        const portrait = inst.config.height > inst.config.width;
        const w = body.resolution === 1080 ? 1920 : 1280, h = body.resolution;
        Object.assign(inst.config, { resolution: body.resolution, width: portrait ? h : w, height: portrait ? w : h, dpi: body.resolution === 1080 ? 280 : 240 });
        applied.resolution = body.resolution;
      }
      for (const k of ['memory_mb', 'cpu', 'fps']) if (body[k] != null) { inst.config[k] = body[k]; applied[k] = body[k]; }
      emit('instances', list());
      return json({ instance: pub(inst), applied, applies_next_boot: inst.running });
    }
    if (method === 'DELETE') {
      if (url.searchParams.get('confirm') !== '1') return errResp('usage', '删除要带 confirm=1');
      if (inst.running || busyJob) return errResp('busy', `ModAVD 实例 ${i} 未关闭，无法删除`, '先关机再删除');
      await sleep(700);
      db.insts.delete(i);
      emit('instances', list());
      return json({ index: i, name: inst.name, path: inst.path });
    }
  }
  if (act === 'start') {
    if (busyJob) return errResp('busy', `ModAVD 实例 ${i} ${busyJob.kind === 'stop' ? '正在关机' : '正在开机或做开机后设置'}`, '等它做完再试');
    // the manager boots with the window only; one running headless is not adopted (no switch while it runs)
    if (inst.running && !inst._.win) return errResp('busy', `ModAVD 实例 ${i} 正以无头方式运行，运行中不能换成带窗口`, `先关机再换：modavd stop ${i}，再 modavd start ${i} --window`);
    if (inst.status === 'online') {
      const j = newJob('start', i);
      run(j, [[400, () => step(j, 'launch', `实例 ${i} 已在运行（pid ${inst.pid}），不重新开机`, 50)], [600, () => step(j, 'boot', `实例 ${i} 安卓已开机`, 610)],
        [600, () => finishJob(j, { ...startResult(inst, 0, 1.6, false), launched: false })]]);
      return json(j, 202);
    }
    const j = newJob(act, i);
    startSeq(inst, j, true);
    return json(j, 202);
  }
  if (act === 'stop') {
    if (busyJob && busyJob.kind !== 'start') return errResp('busy', `ModAVD 实例 ${i} 正在${busyJob.kind === 'stop' ? '关机' : '安装 APK'}`, '等它做完再试');
    const j = newJob('stop', i);
    if (!inst.running) {
      setTimeout(() => finishJob(j, { index: i, name: inst.name, method: 'already_offline', synced: false, seconds: 0.1, warnings: [] }), 300);
    } else stopSeq(inst, j, !!body.force);
    return json(j, 202);
  }
  if (act === 'install') {
    if (inst.status !== 'online') return errResp('busy', `ModAVD 实例 ${i} 没有开机`, '开机后再安装');
    if (busyJob) return errResp('busy', `ModAVD 实例 ${i} 正在做别的事`, '等它做完再试');
    const f = init.body instanceof FormData ? init.body.get('apk') : null;
    if (!f) return errResp('usage', '没有收到 APK 文件');
    const j = newJob('install', i);
    log('info', `实例 ${i} 开始安装 ${f.name}（${(f.size / 1048576).toFixed(1)} MB）`, j.id);
    setTimeout(() => finishJob(j, { index: i, apk: f.name, package: 'com.example.' + f.name.replace(/\.apk$/i, '').replace(/\W+/g, '').toLowerCase(), output: 'Success' }), 3000);
    return json(j, 202);
  }
  if (act === 'playstore-install' || act === 'playstore-uninstall') {
    const install = act === 'playstore-install';
    const reason = (code, message, advice, r) => fail(STATUS_OF[code], code, message, advice, { reason: r });
    if (busyJob || inst.status === 'creating') return errResp('busy', `ModAVD 实例 ${i} 正在做别的事`, '等它做完再试');
    if (sdkJob()) return errResp('busy', `任务 ${sdkJob().kind === 'components' ? '补全组件' : '导入 Google Play 商店的文件'} 正在运行（任务 ${sdkJob().id}），等它做完再试`);
    if (install && !db.store.ready) {
      return reason('not_ready', `这个根目录还没有 Google Play 商店的五个文件，或者文件不全、已损坏（${ROOT}\\sdk\\playstore）`,
        '先在管理器的「补全组件」里勾选「补全 Google Play 商店服务」（或者用自己的文件导入）', 'playstore_files_missing');
    }
    if (install && inst.bridge === 'off') {
      return reason('not_ready', `实例 ${i} 没有 arm64 执行桥，不按可写系统镜像开机，放不进 Google Play 商店`, `先运行 modavd bridge ${i} install 装上执行桥，再补全商店`, 'no_bridge');
    }
    const j = newJob(act, i);
    runPlayStore(inst, j, install ? 'install' : 'uninstall');
    return json(j, 202);
  }
  if (act === 'gles32-on' || act === 'gles32-off') {
    const on = act === 'gles32-on';
    if (busyJob || inst.status === 'creating') return errResp('busy', `ModAVD 实例 ${i} 正在做别的事`, '等它做完再试');
    if (inst.bridge === 'off') {
      return fail(STATUS_OF.not_ready, 'not_ready', `实例 ${i} 没有 arm64 执行桥，不按可写系统镜像开机，改不了它报的 GLES 版本`, `先运行 modavd bridge ${i} install 装上执行桥，再开这个开关`, { reason: 'gles32_no_bridge' });
    }
    const j = newJob(act, i);
    runGLES32(inst, j, on ? 'on' : 'off');
    return json(j, 202);
  }
  if (act === 'playstore-gsfid') {
    if (inst.status !== 'online') return errResp('busy', `实例 ${i} 没有开机`, '开机后才能取 GSF ID');
    if (busyJob) return errResp('busy', `ModAVD 实例 ${i} 正在做别的事`, '等它做完再试');
    if (db.nogsf) {
      return fail(STATUS_OF.not_ready, 'not_ready', `实例 ${i} 还没有 GSF ID`, 'Google 服务框架联网登记后才有：开着网等一两分钟，再试一次', { reason: 'no_gsf_id' });
    }
    const id = 3881793548573364239n + BigInt(i);
    return json({ index: i, decimal: id.toString(), hex: id.toString(16) });
  }
  if (act === 'disk-info' || act === 'disk-trim' || act === 'disk-compact') {
    if (busyJob) return errResp('busy', `实例 ${i} 正在${busyJob.kind === 'stop' ? '关机' : '做别的事'}（任务 ${busyJob.id}），等它做完再试`);
    const GB = 2 ** 30;
    const d = (inst._.disk ||= { logical: 64 * GB, onDisk: 12 * GB, alloc: 9.6 * GB });
    const j = newJob(act, i);
    // like the real job: emu decides by the instance state, so a trim of a stopped instance (or a compact of a running one) fails in the job
    if (act === 'disk-info') {
      step(j, 'disk_info', `实例 ${i} 正在查看磁盘占用`, 0);
      setTimeout(() => {
        const running = inst.running;
        finishJob(j, { index: i, overlay: `${inst.path}\\userdata-qemu.img.qcow2`, running, needs_shutdown: running, logical_bytes: d.logical, on_disk_bytes: d.onDisk, cluster_bytes: 65536,
          ...(running ? {} : { allocated_bytes: Math.round(d.alloc), reclaimable_bytes: Math.max(0, Math.round(d.onDisk - d.alloc)) }) });
      }, 500);
    } else if (act === 'disk-trim') {
      step(j, 'disk_trim', `实例 ${i} 正在触发磁盘回收`, 0);
      setTimeout(() => {
        if (inst.status !== 'online') return failJob(j, 'unsupported', `ModAVD 实例 ${i} 没有在运行，请先开机再回收磁盘空间`, '');
        d.alloc = Math.max(3 * GB, d.alloc - 1.5 * GB);
        finishJob(j, { index: i, discard_max_bytes: 2 * GB });
      }, 1800);
    } else {
      step(j, 'disk_compact', `实例 ${i} 正在整理磁盘（重写数据盘文件，要几分钟）`, 0);
      setTimeout(() => {
        if (inst.running) return failJob(j, 'busy', `ModAVD 实例 ${i} 未关闭，无法整理磁盘`, `先 modavd stop ${i}，关机后再整理`);
        const before = d.onDisk;
        d.onDisk = Math.min(before, d.alloc + 0.1 * GB);
        finishJob(j, { index: i, overlay: `${inst.path}\\userdata-qemu.img.qcow2`, before_on_disk_bytes: Math.round(before), after_on_disk_bytes: Math.round(d.onDisk), freed_bytes: Math.round(before - d.onDisk) });
      }, 5000);
    }
    return json(j, 202);
  }
  if (act === 'input') {
    if (inst.status !== 'online') return errResp('busy', `ModAVD 实例 ${i} 没有开机`);
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
    if (inst.status !== 'online') return errResp('busy', `ModAVD 实例 ${i} 没有开机`);
    if (db.shotfail) return errResp('internal', '截图失败：adb exec-out screencap 退出码 1', '');
    const blob = await screenshot(inst);
    return new Response(blob, { status: 200, headers: { 'Content-Type': 'image/png', 'Cache-Control': 'no-store' } });
  }
  if (act === 'proxy' && method === 'GET') return json(proxyView(inst));
  if (act === 'proxy' && method === 'PUT') {
    if (body.enabled == null && body.server == null && body.apps == null) return errResp('usage', '没有要改的设置', '至少给出 enabled、server、apps 中的一项');
    const s = { ...(inst._.proxy || { enabled: false, server: '', apps: [] }) };
    const notes = [];
    if (body.enabled != null) s.enabled = body.enabled;
    if (body.server != null || (s.enabled && !s.server)) {
      if (body.server == null || body.server === 'auto') {
        s.server = 'socks5://10.0.2.2:7890';
        notes.push('代理地址取自Windows 代理设置：127.0.0.1:7890');
      } else {
        await sleep(400);
        const r = probeAt(body.server);
        if (Array.isArray(r)) return errResp(...r);
        s.server = r.server;
        notes.push(...r.notes);
      }
    }
    if (body.apps != null) {
      const bad = body.apps.find((x) => !PKG.test(x));
      if (bad) return errResp('invalid_argument', `不是安卓包名：「${bad}」`);
      if (body.apps.length > 64) return errResp('invalid_argument', '名单里最多 64 个应用');
      s.apps = [...new Set(body.apps)];
    }
    await sleep(inst.status === 'online' ? 900 : 150);
    setProxy(inst, s);
    emit('instances', list());
    emit('proxy', marks());
    const applied = inst.status === 'online' ? (proxyActive(inst) ? 'now' : 'off') : inst.running ? 'booting' : proxyActive(inst) ? 'next_boot' : 'off';
    log('info', `已写入实例 ${i} 的按应用走代理：${s.enabled ? '开' : '关'}，${s.server || '没有地址'}，${s.apps.length} 个应用`);
    return json(proxyView(inst, applied, notes));
  }
  if (act === 'packages') {
    if (inst.status !== 'online') return errResp('busy', `实例 ${i} 没有开机`, '开机后才能列出装了哪些应用；没开机时可以手填包名');
    await sleep(500);
    return json({ packages: clone(PKGS) });
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
    this._send('proxy', JSON.stringify(marks()));
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
  db.proxyfail = params.get('proxyfail') === '1';
  db.diagMode = params.get('diag') || (scenario === 'components-fail' ? 'components-fail' : scenario === 'diag-fail' ? 'diag-fail' : '');
  if (['download', 'import'].includes(params.get('store'))) db.store = { ready: true, source: params.get('store') };
  db.storefail = params.get('storefail') === '1';
  db.nogsf = params.get('nogsf') === '1';
  window.__mockShots = {};
  setup(scenario);
  const real = window.fetch.bind(window);
  window.fetch = (input, init = {}) => {
    const url = new URL(typeof input === 'string' ? input : input.url, location.href);
    if (!url.pathname.startsWith('/api/')) return real(input, init);
    return handle(url, init);
  };
  window.EventSource = MockEventSource;
  setInterval(() => { emit('instances', list()); emit('proxy', marks()); }, 3000);
  setInterval(() => emit('ping', {}), 15000);
  console.info(`[mock] scenario "${scenario}" with ${db.insts.size} instances`);
}
