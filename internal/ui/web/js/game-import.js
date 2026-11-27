// Copyright 2026 jinghero. SPDX-License-Identifier: AGPL-3.0-or-later
import { t } from './i18n.js';
import { api, request } from './api.js';
import { h, showError } from './dom.js';
import { openDialog, toastError } from './ui.js';
import { instance } from './store.js';
import { quoted } from './format.js';
import { importGames } from './actions.js';

// Load one level at a time. Expanding a folder never selects it for import.
function fileTree(source, pkg, rel, selected, signal) {
  const summary = h('summary', { text: rel ? rel.split('/').pop() : t('选择 Android/data 资源…') });
  const tree = h('details', { class: 'game-resource-tree' }, summary);
  let loaded = false;
  tree.ontoggle = async () => {
    if (!tree.open || loaded) return;
    loaded = true;
    try {
      const rows = await request('POST', '/api/game-sources/browse', { ...source, package: pkg, path: rel }, { signal });
      if (signal.aborted) return;
      for (const item of rows) {
        const path = rel ? rel + '/' + item.name : item.name;
        const input = h('input', { type: 'checkbox', 'aria-label': path });
        const branch = item.directory ? fileTree(source, pkg, path, selected, signal) : null;
        input.onchange = () => {
          if (input.checked) selected.add(path); else selected.delete(path);
          if (branch && input.checked) branch.open = false;
        };
        tree.append(branch ? h('div', { class: 'game-resource-folder' }, input, branch)
          : h('div', null, h('label', null, input, ' ', item.name)));
      }
      if (!rows.length) tree.append(t('目录为空'));
    } catch (e) {
      if (!signal.aborted) toastError(e);
      tree.open = false; loaded = false;
    }
  };
  return tree;
}

export async function openGameImport(i) {
  let players;
  try { players = await api.get('/api/game-sources'); } catch (e) { toastError(e); return; }
  const source = h('select', { 'aria-label': t('源模拟器实例') },
    h('option', { value: '', text: t('选择源模拟器实例') }),
    players.map((p, i) => h('option', { value: String(i), disabled: !!p.error_code, text: p.index < 0 ? (p.kind === 'mumu' ? 'MuMu' : t('雷电')) + ' · ' + p.error : (p.kind === 'mumu' ? 'MuMu' : t('雷电')) + ' ' + p.index + ' · ' + p.name })));
  const list = h('div');
  const error = h('div', { class: 'form-error', hidden: true });
  const ok = h('button', { class: 'btn btn-primary', type: 'submit', disabled: true, text: t('导入') });
  let controller, games = [];
  const ctl = openDialog({
    title: t('导入游戏到{who}', { who: quoted(instance(i), i) }), form: true,
    body: h('div', { class: 'dlg-body' }, source, h('p', { text: t('选择源实例会按需启动它。勾选游戏会复制 APK 和 OBB。Android/data 中只勾资源，跳过账号、存档和设置。导入期间请退出两边的游戏。') }), list, error),
    actions: { ok },
    onSubmit() {
      const selected = games.filter(g => g.input.checked).map(g => {
        const paths = [...g.paths].sort();
        return { package: g.pkg, data: paths.filter(p => !paths.some(a => p.startsWith(a + '/'))) };
      });
      if (!selected.length) return;
      const { kind, index } = players[Number(source.value)];
      const origin = { player: kind, source: index };
      ctl.close(); importGames(i, origin, selected);
    },
  });
  ctl.onClose.push(() => controller?.abort());
  if (!players.length) list.textContent = t('没有找到 MuMu 或雷电实例');
  source.onchange = async () => {
    controller?.abort();
    controller = new AbortController();
    const signal = controller.signal;
    games = []; ok.disabled = true; error.hidden = true; list.replaceChildren();
    if (!source.value) return;
    list.textContent = t('正在读取源实例的应用…');
    const { kind, index } = players[Number(source.value)];
    const origin = { player: kind, source: index };
    try {
      const packages = await request('POST', '/api/game-sources/browse', origin, { signal });
      if (signal.aborted) return;
      list.replaceChildren();
      if (!packages.length) list.textContent = t('没有第三方应用');
      for (const pkg of packages) {
        const input = h('input', { type: 'checkbox' }), paths = new Set();
        const tree = fileTree(origin, pkg, '', paths, signal);
        tree.hidden = true;
        input.onchange = () => { tree.hidden = !input.checked; ok.disabled = !games.some(g => g.input.checked); };
        games.push({ pkg, input, paths });
        list.append(h('div', null, h('label', null, input, ' ', pkg), tree));
      }
    } catch (e) { if (!signal.aborted) { list.replaceChildren(); showError(error, e); } }
  };
}
