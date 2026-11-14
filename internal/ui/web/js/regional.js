// Instance language and time zone choices. Configured values never stand in for live Android observations.
import { h } from './dom.js';

export function regionalChanges(initial, language, timeZone, repeatLanguage = false) {
  const changes = {};
  if (repeatLanguage || language !== initial.language) changes.language = language;
  if (timeZone !== initial.time_zone) changes.time_zone = timeZone;
  return changes;
}

export function regionalFields(prefix, config, isNew = false) {
  const initial = config || { language: 'host', time_zone: 'host' };
  const language = choice('language', '语言', initial.language, '按宿主语言设置一次', '保持安卓语言', 'zh-CN',
    ['zh-CN', 'zh-TW', 'en-US', 'en-GB', 'ja-JP', 'ko-KR', 'fr-FR', 'de-DE', 'es-ES', 'pt-BR', 'ru-RU']);
  const zone = choice('timezone', '时区', initial.time_zone, '跟随宿主', '沿用原行为', 'Asia/Shanghai',
    ['Asia/Shanghai', 'Asia/Hong_Kong', 'Asia/Taipei', 'Asia/Tokyo', 'Europe/London', 'America/New_York', 'Etc/UTC']);
  const repeat = h('input', { type: 'checkbox', id: prefix + '-language-repeat' });
  const repeatRow = h('label', { class: 'checkbox' }, repeat, h('span', { text: '下次开机重新设置一次' }));
  repeatRow.hidden = isNew || initial.language === 'preserve';
  language.cell.append(repeatRow, h('p', { class: 'f-hint', text: initial.last_language
    ? `上次设置为 ${initial.last_language}，之后保留安卓里的修改。` : '只设置一次，之后保留安卓里的修改。' }));
  if (initial.pending_language) language.cell.append(h('p', { class: 'f-hint', text: `待应用：${initial.pending_language}` }));
  const warning = h('p', { class: 'f-hint tone-warn', text: '时区变化可能影响游戏每日刷新时间。', hidden: true });
  zone.cell.append(warning);
  zone.cell.addEventListener('change', () => { warning.hidden = isNew || zone.value() === initial.time_zone; });
  language.cell.addEventListener('change', () => { repeatRow.hidden = isNew || language.value() === 'preserve' || language.value() !== initial.language; });
  const values = () => ({ language: language.value(), time_zone: zone.value() });
  return {
    rows: [language.label, language.cell, zone.label, zone.cell],
    values,
    changes: () => regionalChanges(initial, language.value(), zone.value(), repeat.checked && language.value() !== 'preserve'),
  };

  function choice(key, label, value, hostText, preserveText, placeholder, suggestions) {
    const id = prefix + '-' + key;
    const select = h('select', { class: 'select', id },
      h('option', { value: 'preserve', text: preserveText }),
      h('option', { value: 'host', text: hostText }),
      h('option', { value: 'fixed', text: `指定${label}` }));
    select.value = value === 'host' || value === 'preserve' ? value : 'fixed';
    const input = h('input', { class: 'input', id: id + '-value', autocomplete: 'off', placeholder, list: id + '-list',
      value: select.value === 'fixed' ? value : '', 'aria-label': `指定${label}` });
    const list = h('datalist', { id: id + '-list' }, suggestions.map((v) => h('option', { value: v })));
    const sync = () => { input.hidden = input.disabled = select.value !== 'fixed'; };
    select.addEventListener('change', sync);
    sync();
    return { label: h('label', { class: 'f-label', for: id, text: label }),
      cell: h('div', { class: 'f-ctl regional-choice' }, select, input, list),
      value: () => select.value === 'fixed' ? input.value.trim() : select.value };
  }
}

export function guestRegionalText(result) {
  if (result.guest) return `${result.guest.language} · ${result.guest.time_zone}`;
  return result.guest_error ? '读取失败' : '未读取，实例需要开机且空闲';
}
