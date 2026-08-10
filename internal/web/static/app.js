'use strict';

// Portal UI. Plain DOM and fetch, no build step — the whole thing ships inside
// the Go binary.
//
// Two audiences share these pages. Reading is open to anyone who can reach the
// portal; editing needs the admin login. Every control that writes is hidden
// until logged in, but that is a courtesy — the server checks for itself.

const state = {
  overview: null,
  services: [],
  groups: [],
  filter: '',
  stateFilter: '',
  route: { view: 'urls' },
  auth: sessionStorage.getItem('spectra-auth') || '',
};

const PROBE_LABEL = {
  ok: 'спека собрана',
  no_spec: 'спека не найдена',
  unreachable: 'недоступен',
  invalid_spec: 'спека не разбирается',
  unknown: 'ещё не опрашивался',
};

const SOURCE_LABEL = {
  probe: 'найдена перебором путей',
  'swagger-html': 'найдена на странице документации',
  manual: 'задана вручную',
  annotation: 'указана в аннотации Ingress',
};

const DETAIL_LABEL = {
  'op.removed': () => 'эндпоинт удалён',
  'param.added': s => `добавлен параметр ${s}`,
  'param.added.required': s => `добавлен обязательный параметр ${s}`,
  'param.removed': s => `убран параметр ${s}`,
  'param.removed.required': s => `убран обязательный параметр ${s}`,
  'param.required.on': s => `параметр ${s} стал обязательным`,
  'param.required.off': s => `параметр ${s} стал необязательным`,
  'response.added': s => `добавлен ответ ${s}`,
  'response.removed': s => `убран ответ ${s}`,
  'response.removed.success': s => `убран успешный ответ ${s}`,
  'body.required.on': () => 'тело запроса стало обязательным',
  'body.required.off': () => 'тело запроса стало необязательным',
  'body.field.added.required': s => `в теле запроса появилось обязательное поле ${s}`,
  'body.field.removed.required': s => `поле ${s} больше не обязательно в теле запроса`,
  'op.deprecated.on': () => 'помечен deprecated',
  'op.deprecated.off': () => 'снята пометка deprecated',
  'op.summary.changed': () => 'изменено описание',
  'op.schema.changed': () => 'изменилась схема данных или описание',
  'spec.initial': () => 'первичный импорт спецификации',
  'spec.version.changed': s => `версия API: ${s}`,
  'spec.openapi.changed': s => `версия OpenAPI: ${s}`,
  'spec.title.changed': s => `название: ${s}`,
  'spec.other.changed': () => 'изменения вне эндпоинтов (security, tags, описание спеки)',
  'spec.rollback': s => `откат: контракт вернулся к состоянию ревизии ${s}`,
};

const KIND_LABEL = { added: 'добавлен', removed: 'удалён', modified: 'изменён' };

/* ---------- helpers ---------- */

const esc = s => String(s ?? '').replace(/[&<>"']/g, c =>
  ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]));

const el = id => document.getElementById(id);

async function api(path, options = {}) {
  const headers = Object.assign({}, options.headers);
  if (state.auth) headers['Authorization'] = 'Basic ' + state.auth;
  if (options.body) headers['Content-Type'] = 'application/json';

  const res = await fetch(path, Object.assign({}, options, { headers }));
  if (res.status === 401) {
    // Stale or missing credentials: drop them so the next attempt asks again.
    state.auth = '';
    sessionStorage.removeItem('spectra-auth');
    renderAuthBox();
    throw new Error('Нужен вход администратора');
  }
  if (!res.ok) {
    let message = `${path}: HTTP ${res.status}`;
    try {
      const body = await res.json();
      if (body.error) message = body.error;
    } catch { /* keep the status line */ }
    throw new Error(message);
  }
  return res.status === 204 ? null : res.json();
}

const rtf = new Intl.RelativeTimeFormat('ru', { numeric: 'auto' });
const UNITS = [['year', 31536000], ['month', 2592000], ['day', 86400],
               ['hour', 3600], ['minute', 60], ['second', 1]];

function ago(iso) {
  if (!iso) return '—';
  const delta = (new Date(iso).getTime() - Date.now()) / 1000;
  for (const [unit, seconds] of UNITS) {
    if (Math.abs(delta) >= seconds || unit === 'second') {
      return rtf.format(Math.round(delta / seconds), unit);
    }
  }
}

const absolute = iso => iso ? new Date(iso).toLocaleString('ru-RU',
  { dateStyle: 'medium', timeStyle: 'short' }) : '';

function detailText(d) {
  const fn = DETAIL_LABEL[d.code];
  return fn ? fn(d.subject || '') : `${d.code} ${d.subject || ''}`.trim();
}

const isAdmin = () => Boolean(state.auth);
const findService = id => state.services.find(s => String(s.id) === String(id));

function toast(message, kind = 'error') {
  const box = document.createElement('div');
  box.className = `toast ${kind}`;
  box.textContent = message;
  document.body.appendChild(box);
  setTimeout(() => box.remove(), 5000);
}

/* ---------- theme ---------- */

// Three states rather than two: "как в системе" is a real preference, not the
// absence of one, and a portal people keep open all day should follow the OS
// when they switch it at dusk.
const THEMES = [
  { value: 'auto', icon: '◐', label: 'Тема: как в системе' },
  { value: 'light', icon: '☀', label: 'Тема: светлая' },
  { value: 'dark', icon: '☾', label: 'Тема: тёмная' },
];

function currentTheme() {
  const stored = localStorage.getItem('spectra-theme');
  return THEMES.some(t => t.value === stored) ? stored : 'auto';
}

function applyTheme(value) {
  if (value === 'auto') {
    delete document.documentElement.dataset.theme;
    localStorage.removeItem('spectra-theme');
  } else {
    document.documentElement.dataset.theme = value;
    localStorage.setItem('spectra-theme', value);
  }
  const theme = THEMES.find(t => t.value === value);
  const button = el('theme');
  if (button) {
    button.textContent = theme.icon;
    button.title = theme.label + ' — нажмите, чтобы переключить';
  }
  // The specification viewer lives in its own document and cannot see the
  // attribute, so it is told through the URL.
  const frame = document.querySelector('iframe.viewer');
  if (frame) {
    const url = new URL(frame.src, location.href);
    url.searchParams.set('theme', value);
    if (frame.src !== url.toString()) frame.src = url.toString();
  }
}

function initTheme() {
  applyTheme(currentTheme());
  el('theme').addEventListener('click', () => {
    const next = THEMES[(THEMES.findIndex(t => t.value === currentTheme()) + 1) % THEMES.length];
    applyTheme(next.value);
  });
}

/* ---------- auth ---------- */

function renderAuthBox() {
  const box = el('auth-box');
  if (!state.overview?.adminEnabled) {
    box.innerHTML = '<span class="muted" title="SPECTRA_ADMIN_USER / SPECTRA_ADMIN_PASSWORD не заданы">настройка выключена</span>';
    return;
  }
  box.innerHTML = isAdmin()
    ? '<button class="btn small" id="logout">Выйти</button>'
    : '<button class="btn small" id="login">Войти</button>';

  const login = el('login');
  if (login) {
    login.addEventListener('click', async () => {
      const user = prompt('Логин администратора');
      if (user === null) return;
      const password = prompt('Пароль');
      if (password === null) return;
      state.auth = btoa(`${user}:${password}`);
      try {
        await api('/api/settings'); // cheapest endpoint that requires the login
        sessionStorage.setItem('spectra-auth', state.auth);
        await reload();
      } catch (err) {
        toast(err.message);
      }
    });
  }
  const logout = el('logout');
  if (logout) {
    logout.addEventListener('click', () => {
      state.auth = '';
      sessionStorage.removeItem('spectra-auth');
      reload();
    });
  }
}

/* ---------- routing ---------- */

function parseRoute() {
  const parts = location.hash.replace(/^#\/?/, '').split('/').filter(Boolean);
  if (parts[0] === 's' && parts[1]) {
    return { view: 'service', id: decodeURIComponent(parts[1]), tab: parts[2] || 'spec' };
  }
  if (parts[0] === 'changes') return { view: 'changes' };
  if (parts[0] === 'settings') return { view: 'settings' };
  return { view: 'urls' };
}

window.addEventListener('hashchange', () => {
  state.route = parseRoute();
  render();
});

/* ---------- data ---------- */

async function reload() {
  const [overview, services, groups] = await Promise.all([
    api('/api/overview'), api('/api/services'), api('/api/groups'),
  ]);
  state.overview = overview;
  state.services = services.services || [];
  state.groups = groups.groups || [];
  renderAuthBox();
  render();
}

function renderSidebar() {
  const counts = state.overview?.counts || {};
  const visible = state.services.filter(matchesFilter);

  const groups = new Map();
  for (const svc of visible) {
    const key = svc.groupName || 'Без группы';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(svc);
  }

  const link = (href, view, label, badge) => `
    <a class="side-link ${state.route.view === view ? 'active' : ''}" href="${href}">
      ${label}${badge ? `<span class="pill">${badge}</span>` : ''}
    </a>`;

  const parts = [
    // The badge sits where the new services can be dealt with: the address
    // list has the "новые" filter. It is an operator's concern, so a visitor
    // never sees it.
    link('#/', 'urls', 'Все адреса', ''),
    link('#/changes', 'changes', 'Изменения', ''),
    isAdmin() ? link('#/settings', 'settings', 'Настройки', '') : '',
  ];

  for (const [name, list] of groups) {
    parts.push(`<div class="group-title">${esc(name)}</div>`);
    for (const svc of list) {
      const active = state.route.view === 'service' && String(state.route.id) === String(svc.id);
      parts.push(`
        <a class="svc ${active ? 'active' : ''}" href="#/s/${svc.id}">
          <span class="svc-line">
            <span class="dot ${esc(svc.probeStatus)}" title="${esc(PROBE_LABEL[svc.probeStatus] || '')}"></span>
            <span class="svc-name">${esc(svc.displayName)}</span>
          </span>
          <span class="svc-meta">${svc.probeStatus === 'ok'
            ? `${svc.endpointCount} эндп`
            : esc(PROBE_LABEL[svc.probeStatus] || '')}</span>
        </a>`);
    }
  }
  if (!visible.length) parts.push('<div class="empty small">Ничего не найдено</div>');

  el('sidebar').innerHTML = parts.join('');
}

/* ---------- services list ---------- */

function matchesFilter(svc) {
  if (state.stateFilter === 'manualOnly' && (svc.autoProbe !== false)) return false;
  if (state.stateFilter === 'withSpec' && svc.probeStatus !== 'ok') return false;
  if (state.stateFilter === 'noSpec' && svc.probeStatus === 'ok') return false;
  if (state.stateFilter === 'ignored' && svc.reviewState !== 'ignored') return false;
  if (state.stateFilter !== 'ignored' && svc.reviewState === 'ignored') return false;

  const needle = state.filter.trim().toLowerCase();
  if (!needle) return true;
  return [svc.displayName, svc.host, svc.namespace, svc.baseUrl, svc.title, svc.groupName,
          ...(svc.tags || [])]
    .filter(Boolean).some(v => v.toLowerCase().includes(needle));
}

// The plain directory: every address the portal knows about, one per line.
// This is the page the portal exists for — someone who needs an API looks it up
// here — so it stays public and deliberately unadorned. Cluster internals are
// stripped server-side for anyone not logged in; what shows for a visitor is
// the public hostname and whether documentation is available.
function urlRow(svc) {
  const badges = (svc.tags || []).map(t => `<span class="badge">${esc(t)}</span>`).join(' ');
  const address = svc.host || svc.baseUrl || '';
  const spec = svc.probeStatus === 'ok'
    ? `<a href="#/s/${svc.id}">открыть спецификацию</a>`
    : `<span class="muted">${esc(PROBE_LABEL[svc.probeStatus] || svc.probeStatus)}</span>`;

  return `
    <tr>
      <td><span class="dot ${esc(svc.probeStatus)}" title="${esc(PROBE_LABEL[svc.probeStatus] || '')}"></span></td>
      <td>
        <a href="#/s/${svc.id}">${esc(svc.displayName)}</a>
        ${badges}
      </td>
      <td class="addr">${address ? esc(address) : '<span class="muted">—</span>'}</td>
      <td class="num">${svc.probeStatus === 'ok' ? svc.endpointCount : ''}</td>
      <td>${spec}</td>
    </tr>`;
}

function renderURLs() {
  const counts = state.overview?.counts || {};
  const visible = state.services.filter(matchesFilter);

  const chip = (key, label, n) => `
    <button class="chip ${state.stateFilter === key ? 'active' : ''}" data-state="${key}">
      ${label}${n !== undefined ? ` <b>${n}</b>` : ''}
    </button>`;

  const groups = new Map();
  for (const svc of visible) {
    const key = svc.groupName || 'Без группы';
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(svc);
  }

  el('main').innerHTML = `
    <div class="page">
      <h1>Все адреса</h1>
      <p class="muted">
        ${isAdmin()
          ? (state.overview?.kubernetes
              ? 'Список приходит из Ingress кластера. Хост без спеки — нормальное состояние; спрятать его можно на вкладке «Настройка» у сервиса.'
              : 'Дискавери из Kubernetes выключено; список берётся из SPECTRA_STATIC_SERVICES.')
          : 'Спецификации собираются автоматически. Часть сервисов их не публикует — это нормально.'}
      </p>

      <div class="chips">
        ${chip('', 'все', (counts.total || 0) - (counts.hidden || 0))}
        ${chip('withSpec', 'со спецификацией', counts.withSpec)}
        ${chip('noSpec', 'без спецификации', (counts.noSpec || 0) + (counts.unreachable || 0))}
        ${isAdmin() ? chip('manualOnly', 'только вручную', counts.manualOnly) : ''}
        ${isAdmin() ? chip('ignored', 'спрятанные', counts.hidden) : ''}
        ${isAdmin() ? '<button class="btn small" id="sync-now" style="margin-left:auto">Пересканировать кластер</button>' : ''}
      </div>

      ${visible.length ? [...groups.entries()].map(([name, list]) => `
        <h2 class="group">${esc(name)} <span class="muted">${list.length}</span></h2>
        <table class="urls">
          <thead><tr><th></th><th>Сервис</th><th>Адрес</th><th>Эндп.</th><th></th></tr></thead>
          <tbody>${list.map(urlRow).join('')}</tbody>
        </table>`).join('') : '<div class="empty">Ничего не найдено</div>'}
    </div>`;

  for (const button of document.querySelectorAll('[data-state]')) {
    button.addEventListener('click', () => {
      state.stateFilter = button.dataset.state;
      renderURLs();
    });
  }
  el('sync-now')?.addEventListener('click', async event => {
    event.target.disabled = true;
    try {
      await api('/api/sync', { method: 'POST' });
      toast('Сканирование запущено', 'ok');
      setTimeout(reload, 3000);
    } catch (err) {
      toast(err.message);
    } finally {
      setTimeout(() => { event.target.disabled = false; }, 3000);
    }
  });
}

/* ---------- change feed ---------- */

function changeCard(change, showService) {
  const diff = change.diff || {};
  const counts = [];
  if (change.added) counts.push(`<span class="added">+${change.added}</span>`);
  if (change.removed) counts.push(`<span class="removed">−${change.removed}</span>`);
  if (change.modified) counts.push(`<span>~${change.modified}</span>`);

  return `
    <article class="card">
      <div class="card-head">
        ${showService
          ? `<a class="title" href="#/s/${change.serviceId}/history">${esc(change.serviceName)}</a>`
          : `<span class="title">Ревизия ${change.toRevision}${
              change.fromRevision ? ` <span class="muted">(была ${change.fromRevision})</span>` : ''
            }</span>`}
        ${(change.diff?.notes || []).some(n => n.code === 'spec.rollback')
          ? '<span class="badge rollback">откат</span>' : ''}
        ${change.breaking ? '<span class="badge breaking">ломающее</span>' : ''}
        <span class="counts">${counts.join('')}</span>
        <span class="when" title="${esc(absolute(change.detectedAt))}">${esc(ago(change.detectedAt))}</span>
      </div>
      ${(diff.notes || []).length ? `<ul class="notes">${diff.notes.map(n =>
        `<li>${esc(detailText(n))}</li>`).join('')}</ul>` : ''}
      ${(diff.changes || []).length ? `<ul class="changes">${diff.changes.map(item => `
        <li>
          <div class="change-line">
            <span class="method ${esc(item.method)}">${esc(item.method)}</span>
            <span class="change-path">${esc(item.path)}</span>
            <span class="change-kind">${esc(KIND_LABEL[item.kind] || item.kind)}</span>
            ${item.breaking ? '<span class="badge breaking">ломающее</span>' : ''}
          </div>
          ${(item.details || []).filter(d => d.code !== 'op.removed').length
            ? `<ul class="details">${item.details.filter(d => d.code !== 'op.removed').map(d =>
                `<li class="${d.breaking ? 'breaking' : ''}">${esc(detailText(d))}</li>`).join('')}</ul>`
            : ''}
        </li>`).join('')}</ul>` : ''}
    </article>`;
}

async function renderChanges() {
  el('main').innerHTML = '<div class="page"><h1>Последние изменения</h1><div class="empty">Загрузка…</div></div>';
  try {
    const data = await api('/api/changes?limit=60');
    const cards = (data.changes || []).map(c => changeCard(c, true)).join('');
    el('main').innerHTML = `
      <div class="page">
        <h1>Последние изменения</h1>
        <p class="muted">Портал сравнивает спецификации между опросами и показывает только реальные изменения контракта.</p>
        ${cards || '<div class="empty">Изменений пока нет.</div>'}
      </div>`;
  } catch (err) {
    el('main').innerHTML = `<div class="page"><div class="error-box">${esc(err.message)}</div></div>`;
  }
}

/* ---------- service detail ---------- */

function serviceHeader(svc, tab) {
  const base = `#/s/${svc.id}`;
  const badges = [];
  // Review state and cluster presence are the operator's workflow.
  if (isAdmin() && svc.reviewState === 'ignored') badges.push('<span class="badge">скрыт</span>');
  if (isAdmin() && !svc.present) badges.push('<span class="badge gone">исчез из кластера</span>');
  if (svc.apiVersion) badges.push(`<span class="badge ver">v${esc(svc.apiVersion)}</span>`);
  if (svc.specVersion) badges.push(`<span class="badge">OpenAPI ${esc(svc.specVersion)}</span>`);
  for (const tag of svc.tags || []) badges.push(`<span class="badge">${esc(tag)}</span>`);

  return `
    <div class="svc-header">
      <h1>
        <span class="dot ${esc(svc.probeStatus)}"></span>
        ${esc(svc.displayName)} ${badges.join(' ')}
        ${isAdmin() ? '<button class="btn small" id="probe-now" style="margin-left:auto">Опросить сейчас</button>' : ''}
      </h1>
      <p class="svc-sub">
        ${esc(svc.title || '')}
        ${svc.host ? `${svc.title ? ' · ' : ''}<code>${esc(svc.host)}</code>` : ''}
        ${svc.baseUrl ? ` · <code>${esc(svc.baseUrl)}</code>` : ''}
        ${svc.specUrl ? ` · спека: <code>${esc(svc.specUrl)}</code> · ${esc(SOURCE_LABEL[svc.specUrlSource] || '')}` : ''}
      </p>
      ${svc.lastError ? `<div class="error-box">${esc(svc.lastError)}</div>` : ''}
      <ul class="facts">
        <li>статус: <b>${esc(PROBE_LABEL[svc.probeStatus] || svc.probeStatus)}</b></li>
        <li>обновлено: <b title="${esc(absolute(svc.updatedAt))}">${esc(ago(svc.updatedAt))}</b></li>
        <li>эндпоинтов: <b>${svc.endpointCount || 0}</b></li>
        <li>ревизий: <b>${svc.versionCount || 0}</b></li>
        <li>проверено: <b title="${esc(absolute(svc.lastCheckedAt))}">${esc(ago(svc.lastCheckedAt))}</b></li>
        ${svc.namespace ? `<li>namespace: <b>${esc(svc.namespace)}</b></li>` : ''}
        ${svc.backend ? `<li>backend: <b>${esc(svc.backend)}</b></li>` : ''}
      </ul>
      <div class="tabs">
        <a class="tab ${tab === 'spec' ? 'active' : ''}" href="${base}">Спецификация</a>
        <a class="tab ${tab === 'history' ? 'active' : ''}" href="${base}/history">История</a>
        ${isAdmin() ? `<a class="tab ${tab === 'config' ? 'active' : ''}" href="${base}/config">Настройка</a>` : ''}
      </div>
    </div>`;
}

function bindProbe(svc) {
  el('probe-now')?.addEventListener('click', async event => {
    event.target.disabled = true;
    event.target.textContent = 'Опрашиваем…';
    try {
      await api(`/api/services/${svc.id}/probe`, { method: 'POST' });
      setTimeout(reload, 2500);
    } catch (err) {
      toast(err.message);
    } finally {
      setTimeout(() => {
        if (el('probe-now')) { el('probe-now').disabled = false; el('probe-now').textContent = 'Опросить сейчас'; }
      }, 2500);
    }
  });
}

function renderService() {
  const svc = findService(state.route.id);
  if (!svc) {
    el('main').innerHTML = '<div class="page"><div class="empty">Сервис не найден.</div></div>';
    return;
  }
  // A bookmark or a link shared by an admin should not dead-end on a tab that
  // is not there for this reader — show the documentation instead.
  const tab = (state.route.tab === 'config' && !isAdmin()) ? 'spec' : (state.route.tab || 'spec');

  if (tab === 'config') {
    el('main').innerHTML = serviceHeader(svc, tab) + `<div class="page" id="config"></div>`;
    bindProbe(svc);
    renderConfig(svc);
    return;
  }
  if (tab === 'history') {
    el('main').innerHTML = serviceHeader(svc, tab) + '<div class="page" id="history"><div class="empty">Загрузка…</div></div>';
    bindProbe(svc);
    renderHistory(svc);
    return;
  }
  if (!svc.versionId) {
    el('main').innerHTML = serviceHeader(svc, tab) +
      `<div class="page"><div class="empty">Спецификация не собрана.${
        isAdmin() ? ' Укажите адрес вручную на вкладке «Настройка».' : ''}</div></div>`;
    bindProbe(svc);
    return;
  }
  el('main').innerHTML = serviceHeader(svc, tab) +
    `<iframe class="viewer" title="Спецификация"
             src="/viewer.html?service=${svc.id}&theme=${encodeURIComponent(currentTheme())}"></iframe>`;
  bindProbe(svc);
}

function renderConfig(svc) {
  if (!isAdmin()) {
    el('config').innerHTML = '<div class="empty">Настройка доступна после входа администратора.</div>';
    return;
  }
  const groupOptions = ['<option value="">— без группы —</option>']
    .concat(state.groups.map(g =>
      `<option value="${g.id}" ${svc.groupId === g.id ? 'selected' : ''}>${esc(g.name)}</option>`))
    .join('');

  el('config').innerHTML = `
    <h2>Настройка сервиса</h2>
    <form id="svc-form" class="form">
      <label>Название<input name="displayName" value="${esc(svc.displayName)}"></label>
      <label>Группа
        <select name="groupId">${groupOptions}</select>
      </label>
      <label>Теги <span class="muted">через запятую</span>
        <input name="tags" value="${esc((svc.tags || []).join(', '))}">
      </label>
      <label>Адрес спецификации вручную
        <input name="manualSpecUrl" placeholder="/v3/api-docs" value="${esc(svc.manualSpecUrl || '')}">
        <span class="muted">Путь (<code>/v3/api-docs</code>) или полный адрес на
        <code>${esc(svc.host || svc.baseUrl)}</code> — том самом, который показан выше.
        Из полного адреса берётся только путь: опрашивать портал будет по своему
        внутреннему адресу. Чужой хост запрещён — портал ходит изнутри кластера,
        и такой адрес стал бы способом дотянуться до чего угодно.
        ${!svc.autoProbe ? '<br><b>Сейчас автоопрос выключен:</b> сохраните адрес и нажмите «Опросить сейчас». Если спека найдётся, опрос снова пойдёт по расписанию.' : ''}</span>
      </label>
      <label>Заметка<textarea name="notes" rows="2">${esc(svc.notes || '')}</textarea></label>
      <label class="check">
        <input type="checkbox" name="ignored" ${svc.reviewState === 'ignored' ? 'checked' : ''}>
        Скрыть: у этого хоста нет API. Портал перестанет его опрашивать.
      </label>
      <div class="form-actions">
        <button class="btn primary" type="submit">Сохранить</button>
      </div>
    </form>`;

  el('svc-form').addEventListener('submit', async event => {
    event.preventDefault();
    const form = new FormData(event.target);
    const body = {
      displayName: form.get('displayName').trim() || svc.displayName,
      notes: form.get('notes'),
      tags: form.get('tags').split(',').map(t => t.trim()).filter(Boolean),
      manualSpecUrl: form.get('manualSpecUrl').trim(),
      reviewState: form.get('ignored') ? 'ignored' : 'reviewed',
    };
    const groupId = form.get('groupId');
    if (groupId) body.groupId = Number(groupId); else body.clearGroup = true;

    try {
      await api(`/api/services/${svc.id}`, { method: 'PATCH', body: JSON.stringify(body) });
      toast('Сохранено', 'ok');
      await reload();
    } catch (err) {
      toast(err.message);
    }
  });
}

async function renderHistory(svc) {
  try {
    const data = await api(`/api/services/${svc.id}`);
    const changes = (data.changes || []).map(c => changeCard(c, false)).join('');
    const versions = (data.versions || []).map(v => `
      <tr>
        <td>${v.revision}</td>
        <td title="${esc(absolute(v.capturedAt))}">${esc(ago(v.capturedAt))}</td>
        <td>${esc(v.apiVersion || '—')}</td>
        <td>${v.endpointCount}</td>
        <td class="hash">${esc(v.hash.slice(0, 12))}${
          v.rollbackOf ? `<br><span class="badge rollback">= ревизия ${v.rollbackOf}</span>` : ''}</td>
        <td><a href="/viewer.html?service=${svc.id}&version=${v.revision}" target="_blank" rel="noopener">открыть</a></td>
      </tr>`).join('');

    el('history').innerHTML = `
      <h2>Изменения</h2>
      ${changes || '<div class="empty">Изменений пока не зафиксировано.</div>'}
      <h2>Ревизии</h2>
      <p class="muted">Ревизия пишется только при смене хэша нормализованной спеки — опрос сам по себе её не создаёт.</p>
      <table class="versions">
        <thead><tr><th>№</th><th>Зафиксирована</th><th>Версия API</th><th>Эндпоинтов</th><th>Хэш</th><th></th></tr></thead>
        <tbody>${versions || '<tr><td colspan="6" class="muted">нет данных</td></tr>'}</tbody>
      </table>`;
  } catch (err) {
    el('history').innerHTML = `<div class="error-box">${esc(err.message)}</div>`;
  }
}

/* ---------- settings ---------- */

async function renderSettings() {
  if (!isAdmin()) {
    el('main').innerHTML = `
      <div class="page">
        <h1>Настройки</h1>
        <div class="empty">Нужен вход администратора.${
          state.overview?.adminEnabled ? '' :
          ' Учётные данные не заданы: SPECTRA_ADMIN_USER и SPECTRA_ADMIN_PASSWORD.'}</div>
      </div>`;
    return;
  }

  el('main').innerHTML = '<div class="page"><h1>Настройки</h1><div class="empty">Загрузка…</div></div>';
  let settings;
  try {
    settings = await api('/api/settings');
  } catch (err) {
    el('main').innerHTML = `<div class="page"><div class="error-box">${esc(err.message)}</div></div>`;
    return;
  }

  const names = (list, prefix, cls) => (list || []).length
    ? `<div class="run-names ${cls}">${prefix} ${list.map(esc).join(', ')}</div>` : '';

  const runs = (state.overview?.discoveryRuns || []).map(r => `
    <tr>
      <td title="${esc(absolute(r.startedAt))}">${esc(ago(r.startedAt))}</td>
      <td>${esc(r.source)}</td>
      <td>
        ${r.error
          ? `<span class="err">ошибка</span><div class="run-names">${esc(r.error)}</div>`
          : `найдено ${r.found}${r.durationMs ? ` за ${r.durationMs} мс` : ''}
             ${names(r.addedNames, 'появились:', 'added')}
             ${names(r.goneNames, 'исчезли:', 'gone')}
             ${!r.addedNames?.length && !r.goneNames?.length ? '<div class="run-names">изменений нет</div>' : ''}`}
      </td>
    </tr>`).join('');

  el('main').innerHTML = `
    <div class="page">
      <h1>Настройки</h1>
      <p class="muted">Эти значения лежат в базе и действуют на все реплики.
      Адрес базы, доступ к Kubernetes и учётные данные администратора задаются
      переменными окружения — их здесь нет намеренно.</p>

      <form id="settings-form" class="form grid">
        <label>Интервал опроса<input name="pollInterval" value="${esc(settings.pollInterval)}">
          <span class="muted">Например 1h. Спека меняется на деплое, а не поминутно.</span></label>
        <label>Таймаут запроса<input name="requestTimeout" value="${esc(settings.requestTimeout)}"></label>
        <label>Бюджет на опрос сервиса<input name="probeBudget" value="${esc(settings.probeBudget)}">
          <span class="muted">Автопоиск делает до 45 запросов; должен быть заметно больше таймаута.</span></label>
        <label>Пауза между попытками<input name="probeDelay" value="${esc(settings.probeDelay)}">
          <span class="muted">Разносит запросы к одному сервису во времени. Без неё автопоиск
          шлёт до 45 запросов за секунду — для WAF это выглядит как сканирование.</span></label>
        <label>Потолок паузы<input name="maxBackoff" value="${esc(settings.maxBackoff)}">
          <span class="muted">Пауза удваивается от интервала после каждой неудачи.</span></label>
        <label>Параллелизм<input name="concurrency" type="number" min="1" value="${settings.concurrency}"></label>
        <label>Хранить ревизий<input name="retainVersions" type="number" min="1" value="${settings.retainVersions}"></label>
        <label class="check wide">
          <input type="checkbox" name="ignoreServers" ${settings.ignoreServers ? 'checked' : ''}>
          Игнорировать адреса (servers/host/basePath) при сравнении — переезд сервиса не считается сменой контракта
        </label>
        <label class="wide">Игнорировать поля <span class="muted">через запятую</span>
          <input name="ignoreFields" value="${esc((settings.ignoreFields || []).join(', '))}"
                 placeholder="info.version, info.description">
          <span class="muted">Нужно, если генератор пишет в спеку номер сборки: без этого каждый билд даёт «новую ревизию».</span>
        </label>
        <div class="form-actions wide"><button class="btn primary" type="submit">Сохранить</button></div>
      </form>

      <h2>Группы</h2>
      <div class="rows">
        ${state.groups.map(g => `
          <div class="row static">
            <span class="row-main"><span class="row-title">${esc(g.name)}</span>
            <span class="row-sub">${g.count} сервисов</span></span>
            <button class="btn small" data-del-group="${g.id}">Удалить</button>
          </div>`).join('') || '<div class="empty">Групп пока нет.</div>'}
      </div>
      <form id="group-form" class="form inline">
        <input name="name" placeholder="Название группы, например «Профиль и авторизация»">
        <button class="btn" type="submit">Добавить</button>
      </form>

      <h2>Сканирования кластера</h2>
      <table class="versions">
        <thead><tr><th>Когда</th><th>Источник</th><th>Результат</th></tr></thead>
        <tbody>${runs || '<tr><td colspan="3" class="muted">пока не было</td></tr>'}</tbody>
      </table>
    </div>`;

  el('settings-form').addEventListener('submit', async event => {
    event.preventDefault();
    const form = new FormData(event.target);
    try {
      await api('/api/settings', {
        method: 'PUT',
        body: JSON.stringify({
          pollInterval: form.get('pollInterval'),
          requestTimeout: form.get('requestTimeout'),
          probeBudget: form.get('probeBudget'),
          probeDelay: form.get('probeDelay'),
          maxBackoff: form.get('maxBackoff'),
          concurrency: Number(form.get('concurrency')),
          retainVersions: Number(form.get('retainVersions')),
          ignoreServers: Boolean(form.get('ignoreServers')),
          ignoreFields: form.get('ignoreFields').split(',').map(f => f.trim()).filter(Boolean),
        }),
      });
      toast('Сохранено', 'ok');
    } catch (err) {
      toast(err.message);
    }
  });

  el('group-form').addEventListener('submit', async event => {
    event.preventDefault();
    const name = new FormData(event.target).get('name').trim();
    if (!name) return;
    try {
      await api('/api/groups', { method: 'POST', body: JSON.stringify({ name }) });
      await reload();
    } catch (err) {
      toast(err.message);
    }
  });

  for (const button of document.querySelectorAll('[data-del-group]')) {
    button.addEventListener('click', async () => {
      try {
        await api(`/api/groups/${button.dataset.delGroup}`, { method: 'DELETE' });
        await reload();
      } catch (err) {
        toast(err.message);
      }
    });
  }
}

/* ---------- boot ---------- */

function render() {
  renderSidebar();
  switch (state.route.view) {
    case 'service': return renderService();
    case 'changes': return renderChanges();
    case 'settings': return renderSettings();
    default: return renderURLs();
  }
}

async function boot() {
  initTheme();

  el('filter').addEventListener('input', event => {
    state.filter = event.target.value;
    render();
  });

  // Refreshing on a timer redraws the page under the reader and throws them
  // back to the top mid-scroll. The data is minutes-fresh at best anyway, so
  // it is refreshed on demand instead.
  el('reload').addEventListener('click', async event => {
    event.target.disabled = true;
    try {
      await reload();
    } catch (err) {
      toast(err.message);
    } finally {
      event.target.disabled = false;
    }
  });

  state.route = parseRoute();
  try {
    await reload();
  } catch (err) {
    el('main').innerHTML = `<div class="page"><div class="error-box">${esc(err.message)}</div></div>`;
  }
}

boot();
