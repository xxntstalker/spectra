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
  // RabbitMQ: последний снимок каждого брокера и выбор на странице топологии.
  mq: null,
  mqCluster: '',
  mqVhost: '',
  mqProblem: '',
  changesFilter: 'all',
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

// Как называть сервис на экране. Имя по умолчанию — имя backend-Service, и оно
// повторяется по кластеру десятками: «api», «web», «backend». Настоящее
// название берётся из спеки, но уступает тому, что кто-то вписал сам.
// Правило одно и то же на сервере (поле label) и здесь — на случай ответа,
// собранного старой версией портала.
const label = svc =>
  svc.label || (svc.nameIsExplicit ? svc.displayName : (svc.title || svc.displayName));

// Вход держится на сессионной куке, которую браузер ставит сам и делит между
// вкладками. Страница пароль не хранит — раньше он лежал в sessionStorage,
// из-за чего каждая новая вкладка начинала неавторизованной.
async function api(path, options = {}) {
  const headers = Object.assign({}, options.headers);
  if (options.body) headers['Content-Type'] = 'application/json';

  const res = await fetch(path, Object.assign({}, options, {
    headers, credentials: 'same-origin',
  }));
  if (res.status === 401) {
    // Сессия истекла или её не было: обновляем шапку, чтобы появилась кнопка входа.
    if (state.overview) state.overview.admin = false;
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

const isAdmin = () => Boolean(state.overview?.admin);

function duplicateCount() {
  return state.services.filter(s => duplicatesOf(s).length).length;
}

// Записи с одинаковым хостом И одинаковым префиксом пути — то есть те, между
// которыми ingress-контроллеру пришлось бы выбирать. Разные префиксы на одном
// хосте (/orders и /billing) — не дубли: они маршрутизируются по пути.
//
// Совпадение — повод присмотреться, а не приговор. Какая из записей рабочая,
// порталу неоткуда знать: он показывает подозрение, решает человек.
function duplicatesOf(svc) {
  if (!svc?.publicKey) return [];
  return state.services.filter(o => o.publicKey === svc.publicKey && o.id !== svc.id);
}
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
      try {
        // Пароль уходит один раз, в обмен на куку; в странице он не остаётся.
        await api('/api/login', {
          method: 'POST',
          headers: { Authorization: 'Basic ' + btoa(`${user}:${password}`) },
        });
        await reload();
      } catch (err) {
        toast(err.message);
      }
    });
  }
  const logout = el('logout');
  if (logout) {
    logout.addEventListener('click', async () => {
      try {
        await api('/api/logout', { method: 'POST' });
      } catch { /* кука всё равно перевыставляется просроченной */ }
      await reload();
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
  if (parts[0] === 'mq') {
    // Имя vhost по умолчанию — «/», поэтому каждая часть адреса закодирована
    // целиком, а не склеена через слэши.
    if (parts[1] === 'q' && parts[4]) {
      return {
        view: 'mq-queue',
        cluster: decodeURIComponent(parts[2]),
        vhost: decodeURIComponent(parts[3]),
        name: decodeURIComponent(parts[4]),
      };
    }
    return { view: 'mq' };
  }
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
  // Брокер — дополнение к каталогу, а не его условие: если его снимок не
  // отдался, остальной портал всё равно должен открыться.
  state.mq = overview.rabbitmq ? await api('/api/mq').catch(() => null) : null;
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
    state.overview?.rabbitmq
      ? link('#/mq', state.route.view === 'mq-queue' ? 'mq-queue' : 'mq', 'Сообщения · RabbitMQ', mqSeriousCount() || '')
      : '',
    isAdmin() ? link('#/settings', 'settings', 'Настройки', '') : '',
  ];

  for (const [name, list] of groups) {
    parts.push(`<div class="group-title">${esc(name)}</div>`);
    for (const svc of list) {
      const active = state.route.view === 'service' && String(state.route.id) === String(svc.id);
      // Имя сервиса берётся из имени backend-Service, и по кластеру оно
      // повторяется постоянно: десяток «api», «web», «backend». Одного имени
      // мало, чтобы отличить строки друг от друга, поэтому под ним идёт хост —
      // он и есть то, чем сервисы различаются.
      parts.push(`
        <a class="svc ${active ? 'active' : ''}" href="#/s/${svc.id}"
           title="${esc(label(svc))}${svc.host ? ' — ' + esc(svc.host) : ''}">
          <span class="svc-line">
            <span class="dot ${esc(svc.probeStatus)}" title="${esc(PROBE_LABEL[svc.probeStatus] || '')}"></span>
            <span class="svc-name">${esc(label(svc))}</span>
            ${duplicatesOf(svc).length ? '<span class="dup-dot" title="Возможный дубль">≡</span>' : ''}
            <span class="svc-count">${svc.probeStatus === 'ok' ? svc.endpointCount : ''}</span>
          </span>
          <span class="svc-host">${esc(svc.host || svc.baseUrl || '')}</span>
        </a>`);
    }
  }
  if (!visible.length) parts.push('<div class="empty small">Ничего не найдено</div>');

  el('sidebar').innerHTML = parts.join('');
}

/* ---------- services list ---------- */

function matchesFilter(svc) {
  if (state.stateFilter === 'noOwner' && (svc.owner?.team || svc.owner?.contact)) return false;
  if (state.stateFilter === 'manualOnly' && (svc.autoProbe !== false)) return false;
  if (state.stateFilter === 'withSpec' && svc.probeStatus !== 'ok') return false;
  if (state.stateFilter === 'noSpec' && svc.probeStatus === 'ok') return false;
  if (state.stateFilter === 'duplicates' && !duplicatesOf(svc).length) return false;
  if (state.stateFilter === 'ignored' && svc.reviewState !== 'ignored') return false;
  if (state.stateFilter !== 'ignored' && svc.reviewState === 'ignored') return false;

  const needle = state.filter.trim().toLowerCase();
  if (!needle) return true;
  return [label(svc), svc.displayName, svc.host, svc.namespace, svc.baseUrl, svc.title, svc.groupName,
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
        <a href="#/s/${svc.id}">${esc(label(svc))}</a>
        ${badges}
        ${duplicatesOf(svc).length
          ? '<span class="badge dup" title="Тот же публичный адрес, что и у другой записи">возможный дубль</span>'
          : ''}
      </td>
      <td class="addr">
        <span class="addr-public" title="Публичный адрес — по нему ходят потребители API">${
          address ? esc(address) : '<span class="muted">—</span>'}</span>
        ${svc.backend
          ? `<span class="addr-internal" title="Адрес внутри кластера — по нему ходит Spectra">${esc(svc.backend)}</span>`
          : ''}
        ${svc.namespace
          ? `<span class="addr-ns" title="Namespace в кластере">ns: ${esc(svc.namespace)}</span>`
          : ''}
      </td>
      <td class="owner-cell">${svc.owner?.team
        ? esc(svc.owner.team)
        : '<span class="muted">—</span>'}</td>
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
        ${chip('noOwner', 'без владельца', counts.noOwner)}
        ${duplicateCount() ? chip('duplicates', 'возможные дубли', duplicateCount()) : ''}
        ${isAdmin() ? chip('manualOnly', 'только вручную', counts.manualOnly) : ''}
        ${isAdmin() ? chip('ignored', 'спрятанные', counts.hidden) : ''}
        ${isAdmin() ? '<button class="btn small" id="sync-now" style="margin-left:auto">Пересканировать кластер</button>' : ''}
      </div>

      ${visible.length ? [...groups.entries()].map(([name, list]) => `
        <h2 class="group">${esc(name)} <span class="muted">${list.length}</span></h2>
        <div class="table-scroll">
          <table class="urls">
            <thead><tr><th></th><th>Сервис</th><th>Адрес</th><th>Команда</th><th>Эндп.</th><th></th></tr></thead>
            <tbody>${list.map(urlRow).join('')}</tbody>
          </table>
        </div>`).join('') : '<div class="empty">Ничего не найдено</div>'}
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
  const withMQ = Boolean(state.overview?.rabbitmq);
  let items;
  try {
    const [data, mq] = await Promise.all([
      api('/api/changes?limit=60'),
      withMQ ? api('/api/mq/changes?limit=60') : Promise.resolve({ changes: [] }),
    ]);
    // Одна лента, а не две вкладки: контракт API и топология брокера ломают
    // интеграции одинаково, и важно, что из них случилось раньше.
    items = [
      ...(data.changes || []).map(c => ({ at: c.detectedAt, source: 'api', loud: c.breaking, html: changeCard(c, true) })),
      ...(mq.changes || []).map(c => ({ at: c.detectedAt, source: 'mq', loud: c.severity !== 'info', html: mqChangeCard(c) })),
    ].sort((a, b) => new Date(b.at) - new Date(a.at));
  } catch (err) {
    el('main').innerHTML = `<div class="page"><div class="error-box">${esc(err.message)}</div></div>`;
    return;
  }

  const draw = () => {
    const f = state.changesFilter;
    const shown = items.filter(i => f === 'all' || (f === 'loud' ? i.loud : i.source === f));
    const chips = withMQ ? `
      <div class="chips">
        ${[['all', 'Все'], ['api', 'API'], ['mq', 'RabbitMQ'], ['loud', 'Ломающие и требующие внимания']].map(([key, text]) =>
          `<button class="chip ${f === key ? 'active' : ''}" data-changes="${key}">${text}</button>`).join('')}
      </div>` : '';
    el('main').innerHTML = `
      <div class="page">
        <h1>Последние изменения</h1>
        <p class="muted">Портал сравнивает спецификации${withMQ ? ' и топологию брокеров' : ''} между опросами
        и показывает только реальные изменения.</p>
        ${chips}
        ${shown.map(i => i.html).join('') || '<div class="empty">Изменений пока нет.</div>'}
      </div>`;
    for (const button of document.querySelectorAll('[data-changes]')) {
      button.addEventListener('click', () => {
        state.changesFilter = button.dataset.changes;
        draw();
      });
    }
  };
  draw();
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
        ${esc(label(svc))} ${badges.join(' ')}
        ${isAdmin() ? '<button class="btn small" id="probe-now" style="margin-left:auto">Опросить сейчас</button>' : ''}
      </h1>
      <p class="svc-sub">
        ${esc(svc.title || '')}
        ${svc.host ? `${svc.title ? ' · ' : ''}<code class="addr-public"
          title="Публичный адрес">${esc(svc.host)}</code>` : ''}
        ${(svc.baseUrl || svc.backend) ? ` · <code class="addr-internal"
          title="Адрес внутри кластера — по нему идёт опрос">${esc(svc.baseUrl || svc.backend)}</code>` : ''}
        ${svc.namespace ? ` · <code class="addr-ns" title="Namespace в кластере">ns: ${esc(svc.namespace)}</code>` : ''}
        ${svc.specUrl ? ` · спека: <code>${esc(svc.specUrl)}</code> · ${esc(SOURCE_LABEL[svc.specUrlSource] || '')}` : ''}
      </p>
      ${svc.lastError ? `<div class="error-box">${esc(svc.lastError)}</div>` : ''}
      ${duplicateNotice(svc)}
      ${svc.usingFallback ? `<div class="notice-box">Основной адрес не отвечал — опрос идёт по запасному:
        <code>${esc(svc.fallbackUrl || '')}</code>.
        Ближайшее сканирование кластера снова попробует основной.</div>` : ''}
      ${ownerBlock(svc)}
      <ul class="facts">
        <li>статус: <b>${esc(PROBE_LABEL[svc.probeStatus] || svc.probeStatus)}</b></li>
        <li>обновлено: <b title="${esc(absolute(svc.updatedAt))}">${esc(ago(svc.updatedAt))}</b></li>
        <li>эндпоинтов: <b>${svc.endpointCount || 0}</b></li>
        <li>ревизий: <b>${svc.versionCount || 0}</b></li>
        <li>проверено: <b title="${esc(absolute(svc.lastCheckedAt))}">${esc(ago(svc.lastCheckedAt))}</b></li>
      </ul>
      <div class="tabs">
        <a class="tab ${tab === 'spec' ? 'active' : ''}" href="${base}">Спецификация</a>
        <a class="tab ${tab === 'history' ? 'active' : ''}" href="${base}/history">История</a>
        ${tab === 'mq' || mqForService(svc).any
          ? `<a class="tab ${tab === 'mq' ? 'active' : ''}" href="${base}/mq">Сообщения</a>` : ''}
        ${isAdmin() ? `<a class="tab ${tab === 'config' ? 'active' : ''}" href="${base}/config">Настройка</a>` : ''}
      </div>
    </div>`;
}

// Подозрение на дубль показывается обеим записям сразу, с основанием и со
// ссылками друг на друга. Портал ничего не выбирает и ничего не прячет: он
// один раз попробовал решать сам, ошибся, и записи пропадали из списка.
function duplicateNotice(svc) {
  const others = duplicatesOf(svc);
  if (!others.length) return '';
  const links = others.map(o =>
    `<a href="#/s/${o.id}">${esc(label(o))}</a>${o.namespace ? ` <span class="muted">(${esc(o.namespace)})</span>` : ''}`
  ).join(', ');
  return `<div class="notice-box">
    Возможный дубль: тот же публичный адрес <code>${esc(svc.publicKey)}</code>, что и у ${links}.
    Ingress-контроллер направляет один адрес ровно в один backend, поэтому рабочей
    здесь, скорее всего, является одна запись из ${others.length + 1}, а остальные
    остались от переезда. Какая именно — портал не решает: посмотрите на историю
    и на дату проверки, а лишние спрячьте на вкладке «Настройка».
  </div>`;
}

// The owner block is shown even when empty: an explicit "владелец не указан"
// with a link to fill it in is what turns a blank field into something anyone
// actually does. A hidden field nobody sees stays empty forever.
function ownerBlock(svc) {
  const o = svc.owner || {};
  if (!o.team && !o.contact && !o.repo && !(o.owners || []).length) {
    return `<p class="owner empty">Владелец не указан${
      isAdmin() ? ` · <a href="#/s/${svc.id}/config">указать</a>` : ''}</p>`;
  }
  const parts = [];
  if (o.team) parts.push(`<b>${esc(o.team)}</b>`);
  if ((o.owners || []).length) parts.push(esc(o.owners.join(', ')));
  if (o.contact) {
    parts.push(o.contact.includes('@')
      ? `<a href="mailto:${esc(o.contact)}">${esc(o.contact)}</a>`
      : esc(o.contact));
  }
  if (o.repo) parts.push(`<a href="${esc(o.repo)}" target="_blank" rel="noopener">исходный код</a>`);
  const from = o.source === 'spec' ? ' <span class="muted">— из спецификации</span>' : '';
  return `<p class="owner">${parts.join(' · ')}${from}</p>`;
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
    // Страница строилась только из общего списка, и запись, которой там нет,
    // упиралась в «сервис не найден» — хотя сама запись существует. Спрашиваем
    // сервер напрямую: ссылка на сервис должна работать всегда.
    el('main').innerHTML = '<div class="page"><div class="empty">Загружаю…</div></div>';
    api(`/api/services/${encodeURIComponent(state.route.id)}`)
      .then(body => {
        const one = body.service || body;
        if (!one || !one.id) throw new Error('нет данных');
        state.services = state.services.concat([one]);
        renderService();
      })
      .catch(() => {
        el('main').innerHTML = '<div class="page"><div class="empty">Сервис не найден.</div></div>';
      });
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
  if (tab === 'mq') {
    el('main').innerHTML = serviceHeader(svc, tab) + `<div class="page">${serviceMQ(svc)}</div>`;
    bindProbe(svc);
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
      <label>Название
        <input name="displayName" value="${esc(svc.nameIsExplicit ? svc.displayName : '')}"
               placeholder="${esc(label(svc))}">
        <span class="muted">${svc.nameIsExplicit
          ? 'Вписано вручную. Очистите поле, чтобы вернуться к названию из спецификации.'
          : 'Пусто — берётся из спецификации, а если её нет, из имени backend-Service.'}</span>
      </label>
      <label>Группа
        <select name="groupId">${groupOptions}</select>
      </label>
      <label>Теги <span class="muted">через запятую</span>
        <input name="tags" value="${esc((svc.tags || []).join(', '))}">
      </label>
      <label>Адрес спецификации вручную
        <input name="manualSpecUrl" placeholder="/v3/api-docs" value="${esc(svc.manualSpecUrl || '')}">
        <span class="muted">Путь (<code>/v3/api-docs</code>) — тогда опрос пойдёт по
        адресу, которым портал уже пользуется${svc.baseUrl ? ` (<code>${esc(svc.baseUrl)}</code>)` : ''}.
        Либо полный адрес — он сохраняется целиком, вместе с хостом. Это нужно, когда
        документация отдаётся только через ingress и на backend-порту её нет:
        укажите <code>https://${esc(svc.host || 'публичный-хост')}/…</code>, и опрашиваться
        будет именно он. Допустимы только эти два хоста сервиса — портал ходит изнутри
        кластера, и произвольный адрес стал бы способом дотянуться до чего угодно.
        ${!svc.autoProbe ? '<br><b>Сейчас автоопрос выключен:</b> сохраните адрес и нажмите «Опросить сейчас». Если спека найдётся, опрос снова пойдёт по расписанию.' : ''}</span>
      </label>
      <h3 class="form-section">Владелец</h3>
      <p class="muted">Кого спросить про этот API и где лежит код. Если команда
      однажды пропишет контакты в самой спецификации (<code>info.contact</code>,
      <code>externalDocs</code>) или в аннотациях Ingress, они подтянутся сюда
      сами — но заполненное здесь всегда важнее.</p>
      <label>Команда<input name="ownerTeam" value="${esc(svc.owner?.manual?.team || '')}"
        placeholder="${esc(svc.owner?.source === 'spec' && svc.owner?.team ? svc.owner.team + ' (из спецификации)' : 'Профиль и авторизация')}"></label>
      <label>Контакт <span class="muted">почта или чат</span>
        <input name="ownerContact" value="${esc(svc.owner?.manual?.contact || '')}"
          placeholder="${esc(svc.owner?.source === 'spec' && svc.owner?.contact ? svc.owner.contact + ' (из спецификации)' : 'team@company.ru или #team-profile')}"></label>
      <label>Исходный код<input name="ownerRepo" value="${esc(svc.owner?.manual?.repo || '')}"
        placeholder="${esc(svc.owner?.source === 'spec' && svc.owner?.repo ? svc.owner.repo + ' (из спецификации)' : 'https://gitlab.company.ru/...')}"></label>
      <label>Люди <span class="muted">через запятую</span>
        <input name="ownerOwners" value="${esc((svc.owner?.manual?.owners || []).join(', '))}"></label>

      <h3 class="form-section">Прочее</h3>
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
      // Пустое поле — не «оставить как было», а «убрать своё название»:
      // сервер вернёт запись к имени backend-Service, и снова начнёт
      // действовать заголовок из спецификации.
      displayName: form.get('displayName').trim(),
      notes: form.get('notes'),
      tags: form.get('tags').split(',').map(t => t.trim()).filter(Boolean),
      manualSpecUrl: form.get('manualSpecUrl').trim(),
      owner: {
        team: form.get('ownerTeam'),
        contact: form.get('ownerContact'),
        repo: form.get('ownerRepo'),
        owners: form.get('ownerOwners').split(',').map(o => o.trim()).filter(Boolean),
      },
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
        ${state.overview?.rabbitmq
          ? `<label>Интервал снимка RabbitMQ<input name="mqPollInterval" value="${esc(settings.mqPollInterval)}">
              <span class="muted">Например 1h. Топология меняется на деплое, как и контракты;
              следить за брокером поминутно — задача мониторинга, а не каталога.</span></label>`
          : `<input type="hidden" name="mqPollInterval" value="${esc(settings.mqPollInterval)}">`}
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
          mqPollInterval: form.get('mqPollInterval'),
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

/* ---------- RabbitMQ ---------- */

const MQ_PROBLEM_LABEL = {
  nodlq: 'без DLQ',
  unbound: 'exchange без привязок',
  anonymous: 'соединения без имени',
  noaccess: 'vhost без доступа',
  shovel: 'shovel и federation',
};

// В счётчике на боковой колонке — только то, что видно наверняка и из-за чего
// сообщения теряются. «Без DLQ» и «без имени» — гигиена: они видны на
// странице, но цифра в навигации, которая никогда не гаснет, перестаёт
// замечаться.
const MQ_SERIOUS = new Set(['unbound', 'noaccess', 'shovel']);

// Чего нет в снимке, того Spectra не видела — и не более того: задача,
// которая забирает очередь раз в сутки, или чтение через basic.get в снимок
// не попадают никогда.
const MQ_NOT_SEEN = '<span class="muted" title="В момент снимка подписчиков не было. Чтение по расписанию и через basic.get брокер не показывает.">не видно</span>';
const MQ_QUIET = new Set(['nodlq', 'anonymous']);

const MQ_SOURCE_LABEL = { argument: 'аргумент очереди', policy: 'политика', 'operator-policy': 'operator policy' };
const MQ_SEVERITY = { breaking: ['breaking', 'ломающее'], attention: ['attention', 'внимание'] };
const MQ_OBJECT_LABEL = {
  cluster: 'кластер', vhost: 'vhost', exchange: 'exchange', queue: 'очередь',
  binding: 'привязка', policy: 'политика', shovel: 'shovel',
};

const mqBrokers = () => state.mq?.brokers || [];

/* Заявленные связи: кто читает очередь или публикует в exchange — со слов
   человека (manual) или по коду (code, позже из CI). Показываются рядом с
   увиденным в брокере и отличаются видом: пунктирная рамка. */

const MQ_MODE_LABEL = {
  subscribe: 'постоянная подписка', schedule: 'по расписанию',
  get: 'через basic.get', manual: 'вручную, по запросу',
};
const MQ_DECLARED_SOURCE = { manual: 'вручную', code: 'по коду' };

const mqDeclared = () => state.mq?.declared || [];
const declaredFor = (cluster, vhost, kind, name, role) => mqDeclared().filter(d =>
  d.cluster === cluster && d.vhost === vhost && d.objectKind === kind && d.object === name && d.role === role);

// Заявленный сервис — это текст, который вписал человек; ссылкой он становится,
// если совпал с сервисом из реестра по тем же именам, что и connection_name.
function serviceByName(name) {
  const n = String(name || '').trim().toLowerCase();
  return state.services.find(s => [s.label, s.displayName, (s.backend || '').split('.')[0]]
    .some(x => String(x || '').toLowerCase() === n));
}

function declaredChips(list) {
  return list.map(d => {
    const svc = serviceByName(d.service);
    const text = `${esc(d.service)}${d.mode ? ` · ${esc(MQ_MODE_LABEL[d.mode] || d.mode)}` : ''}`;
    const title = `${MQ_DECLARED_SOURCE[d.source] || d.source}${d.note ? `: ${d.note}` : ''}`;
    return svc
      ? `<a class="party declared" href="#/s/${svc.id}/mq" title="${esc(title)}">${text}</a>`
      : `<span class="party declared" title="${esc(title)}">${text}</span>`;
  }).join(' ');
}

// Кто публикует в exchange: увиденное (если брокер это сообщает) плюс заявленное.
function publishersCell(cluster, vhost, x, known) {
  const declared = declaredFor(cluster, vhost, 'exchange', x.name, 'publish');
  if (!known && !declared.length) return '';
  const seen = known && x.publishers?.length ? partyChips(x.publishers) : '';
  return `<span class="mq-pubs"><span class="muted">публикуют:</span>
    ${[seen, declaredChips(declared)].filter(Boolean).join(' ') || MQ_NOT_SEEN}</span>`;
}

// Кто читает очередь: увиденное в брокере плюс заявленное. Если нет ни того,
// ни другого — «не видно», а не «никто».
function readersCell(cluster, vhost, q) {
  const seen = q.consumedBy || [];
  const declared = declaredFor(cluster, vhost, 'queue', q.name, 'consume');
  if (!seen.length && !declared.length) {
    return q.deadLetterQueue ? '<span class="muted" title="DLQ разбирают вручную или по запросу">—</span>' : MQ_NOT_SEEN;
  }
  return [seen.length ? partyChips(seen) : '', declaredChips(declared)].filter(Boolean).join(' ');
}

function mqSeriousCount() {
  let n = 0;
  for (const b of mqBrokers()) {
    for (const p of b.snapshot?.problems || []) if (MQ_SERIOUS.has(p.kind)) n++;
  }
  return n;
}

const mqQueueHref = (cluster, vhost, name) =>
  `#/mq/q/${encodeURIComponent(cluster)}/${encodeURIComponent(vhost)}/${encodeURIComponent(name)}`;

// Те же правила, что на сервере (rabbitmq.Queue.Temporary, Exchange.BuiltIn):
// временные очереди живут одно соединение, встроенные exchange есть в любом vhost.
const isTemporary = q => q.exclusive || q.autoDelete || q.name.startsWith('amq.gen-');
const isBuiltIn = x => x.name === '' || x.name.startsWith('amq.');
const dlxParam = q => (q.effective || []).find(p => p.key === 'dead-letter-exchange');

// Сервис у очереди или exchange. Как он опознан, видно сразу: по имени
// соединения — обычный, по IP пода — догадка, никак — «не определён».
function partyChips(parties, empty) {
  if (!parties?.length) return empty ?? MQ_NOT_SEEN;
  return parties.map(p => {
    const text = `${esc(p.service)}${p.count > 1 ? ` ×${p.count}` : ''}`;
    const title = p.via === 'name' ? 'по connection_name'
      : p.via === 'ip' ? 'по IP пода: соединение не назвалось'
        : 'соединение не назвалось, и под с таким адресом не найден';
    const cls = p.via === 'name' ? 'party' : p.via === 'ip' ? 'party guess' : 'party none';
    return p.serviceId
      ? `<a class="${cls}" href="#/s/${p.serviceId}/mq" title="${esc(title)}">${text}</a>`
      : `<span class="${cls}" title="${esc(title)}">${text}</span>`;
  }).join(' ');
}

function problemsIndex(snap) {
  const map = new Map();
  for (const p of snap.problems || []) {
    const key = `${p.vhost}\u0000${p.objectKind}\u0000${p.object}`;
    if (!map.has(key)) map.set(key, []);
    map.get(key).push(p);
  }
  return (vhost, kind, name) => map.get(`${vhost}\u0000${kind}\u0000${name}`) || [];
}

const problemBadges = list => list.map(p =>
  `<span class="badge ${MQ_QUIET.has(p.kind) ? 'warn' : 'breaking'}" title="${esc(p.detail)}">${
    esc(MQ_PROBLEM_LABEL[p.kind] || p.kind)}</span>`).join(' ');

function mqQueueCells(cluster, vhost, q, problemsOf) {
  const dlx = dlxParam(q);
  return `
    <td><a class="mono" href="${mqQueueHref(cluster, vhost.name, q.name)}">${esc(q.name)}</a>
      ${q.deadLetterQueue ? '<span class="badge">DLQ</span>' : ''}
      ${problemBadges(problemsOf(vhost.name, 'queue', q.name))}</td>
    <td class="mono muted">${esc(q.type)}</td>
    <td>${readersCell(cluster, vhost.name, q)}</td>
    <td class="mono muted">${dlx ? esc(dlx.value === '' ? '(по умолчанию)' : String(dlx.value))
      : (q.deadLetterQueue ? '—' : '<span class="warn-text">нет</span>')}</td>`;
}

function mqChangeCard(c) {
  const [cls, text] = MQ_SEVERITY[c.severity] || [];
  const href = (c.objectKind === 'queue' || c.objectKind === 'binding') && c.object
    ? mqQueueHref(c.cluster, c.vhost, c.object) : '#/mq';
  return `
    <article class="card">
      <div class="card-head">
        <span class="badge mq">RabbitMQ</span>
        <span class="muted">${esc(MQ_OBJECT_LABEL[c.objectKind] || c.objectKind)}</span>
        <a class="title mono" href="${href}">${esc(c.object || c.cluster)}</a>
        <span class="muted">${esc(c.cluster)}${c.vhost ? ` · vhost ${esc(c.vhost)}` : ''}</span>
        ${cls ? `<span class="badge ${cls}">${text}</span>` : ''}
        <span class="when" title="${esc(absolute(c.detectedAt))}">${esc(ago(c.detectedAt))}</span>
      </div>
      <ul class="notes">${(c.lines || []).map(line => `<li>${esc(line)}</li>`).join('')}</ul>
    </article>`;
}

function renderMQ() {
  if (!state.overview?.rabbitmq) {
    el('main').innerHTML = `
      <div class="page"><h1>RabbitMQ</h1>
        <div class="empty">Брокеры не подключены: задайте SPECTRA_RABBITMQ_BROKERS.</div></div>`;
    return;
  }
  const brokers = mqBrokers();
  if (!brokers.length) {
    el('main').innerHTML = '<div class="page"><h1>RabbitMQ</h1><div class="error-box">Снимок брокеров не загрузился.</div></div>';
    return;
  }
  const broker = brokers.find(b => b.cluster === state.mqCluster) || brokers[0];
  state.mqCluster = broker.cluster;
  const snap = broker.snapshot;

  const clusterChips = brokers.length > 1 ? `
    <div class="chips">${brokers.map(b => `
      <button class="chip ${b.cluster === broker.cluster ? 'active' : ''}" data-mq-cluster="${esc(b.cluster)}">
        ${esc(b.cluster)}${b.failing ? ' · нет связи' : ''}</button>`).join('')}
    </div>` : '';
  const refresh = isAdmin() ? '<button class="btn small" id="mq-refresh">Снять снимок сейчас</button>' : '';
  const failure = broker.failing
    ? `<div class="error-box">Последний опрос брокера не удался${broker.lastError ? `: ${esc(broker.lastError)}` : ''}.${
        snap ? ` Показан снимок от ${esc(absolute(snap.capturedAt))}.` : ''}</div>` : '';

  if (!snap) {
    el('main').innerHTML = `
      <div class="page"><h1>RabbitMQ</h1>${clusterChips}${failure}
        ${broker.failing ? '' : '<div class="empty">Снимка ещё нет: брокер опрашивается по расписанию.</div>'}
        ${refresh}</div>`;
    bindMQ();
    return;
  }

  const problemsOf = problemsIndex(snap);
  const vhosts = snap.vhosts || [];
  const vf = vhosts.some(v => v.name === state.mqVhost) ? state.mqVhost : '';
  const scoped = (snap.problems || []).filter(p => !vf || p.vhost === vf);
  const byKind = {};
  for (const p of scoped) byKind[p.kind] = (byKind[p.kind] || 0) + 1;
  const pf = byKind[state.mqProblem] ? state.mqProblem : '';
  const hits = new Set(scoped.filter(p => p.kind === pf).map(p => `${p.vhost}\u0000${p.objectKind}\u0000${p.object}`));
  const flagged = (vhost, kind, name) => hits.has(`${vhost}\u0000${kind}\u0000${name}`);

  const totals = { exchanges: 0, queues: 0 };
  for (const v of vhosts) {
    totals.exchanges += v.exchanges.filter(x => !isBuiltIn(x)).length;
    totals.queues += v.queues.filter(q => !isTemporary(q)).length;
  }

  const sections = vhosts.filter(v => !vf || v.name === vf)
    .map(v => mqVhostSection(broker.cluster, v, snap, problemsOf, pf, flagged)).join('');

  const anonymous = (snap.connections || []).filter(c => !c.providedName && (!vf || c.vhost === vf));
  const connections = anonymous.length && (!pf || pf === 'anonymous') ? `
    <section class="card mq-exchange">
      <div class="card-head"><span class="title">Соединения без connection_name · ${anonymous.length}</span>
        <span class="muted">по стандарту connection_name — имя сервиса; без него сервис угадывается по IP пода</span></div>
      <div class="table-scroll"><table class="mq">
        <thead><tr><th>адрес</th><th>vhost</th><th>сервис</th><th>клиент</th>${isAdmin() ? '<th>пользователь</th>' : ''}<th>соединение в брокере</th></tr></thead>
        <tbody>${anonymous.map(c => `
          <tr><td class="mono">${esc(c.peerHost)}</td><td class="mono muted">${esc(c.vhost)}</td>
            <td>${c.service
              ? partyChips([{ service: c.service, serviceId: c.serviceId, via: c.via, count: 1 }])
              : '<span class="party none">не определён</span>'}</td>
            <td class="muted">${esc(c.product || '')}</td>
            ${isAdmin() ? `<td class="mono muted">${esc(c.user || '')}</td>` : ''}
            <td class="mono muted">${esc(c.name)}</td></tr>`).join('')}</tbody>
      </table></div>
    </section>` : '';

  el('main').innerHTML = `
    <div class="page">
      <h1>RabbitMQ</h1>
      <p class="muted">Кластер <code>${esc(broker.cluster)}</code>${snap.clusterName ? ` (${esc(snap.clusterName)})` : ''}${
        snap.version ? ` · RabbitMQ ${esc(snap.version)}` : ''} · снимок
        <span title="${esc(absolute(snap.capturedAt))}">${esc(ago(snap.capturedAt))}</span>.
        Это каталог: что есть в брокере и кто с чем связан. Сообщения и их
        количество Spectra не читает — это задача мониторинга.</p>
      ${failure}
      ${snap.publishersKnown ? '' : `<div class="notice-box">Брокер не сообщает, кто публикует в exchange: это видно
        только при <code>management.rates_mode = detailed</code>. Потребители видны всегда.</div>`}
      ${clusterChips}
      <ul class="facts">
        <li>vhost: <b>${vhosts.length}</b></li>
        <li>exchange: <b>${totals.exchanges}</b></li>
        <li>очередей: <b>${totals.queues}</b></li>
        <li>соединений: <b>${(snap.connections || []).length}</b></li>
        <li>проблем: <b>${(snap.problems || []).length}</b></li>
        <li>${refresh}</li>
      </ul>
      ${vhosts.length > 1 ? `<div class="chips">
        <button class="chip ${vf ? '' : 'active'}" data-mq-vhost="">Все vhost</button>
        ${vhosts.map(v => {
          const n = (snap.problems || []).filter(p => p.vhost === v.name && MQ_SERIOUS.has(p.kind)).length;
          return `<button class="chip ${vf === v.name ? 'active' : ''}" data-mq-vhost="${esc(v.name)}">${esc(v.name)}${
            v.accessible ? '' : ' · нет доступа'}${n ? ` <b>${n}</b>` : ''}</button>`;
        }).join('')}
      </div>` : ''}
      ${scoped.length ? `<div class="chips">
        <button class="chip ${pf ? '' : 'active'}" data-mq-problem="">Всё</button>
        ${Object.keys(MQ_PROBLEM_LABEL).filter(k => byKind[k]).map(k =>
          `<button class="chip ${pf === k ? 'active' : ''}" data-mq-problem="${k}">${MQ_PROBLEM_LABEL[k]} <b>${byKind[k]}</b></button>`).join('')}
      </div>` : ''}
      ${sections}
      ${connections}
      ${!sections && !connections ? '<div class="empty">Ничего не найдено.</div>' : ''}
      <h2>История топологии</h2>
      <div id="mq-history"><div class="empty small">Загрузка…</div></div>
    </div>`;
  bindMQ();

  const params = new URLSearchParams({ cluster: broker.cluster, limit: '30' });
  if (vf) params.set('vhost', vf);
  api(`/api/mq/changes?${params}`)
    .then(data => {
      const box = el('mq-history');
      if (box) box.innerHTML = (data.changes || []).map(mqChangeCard).join('') || '<div class="empty small">Изменений пока нет.</div>';
    })
    .catch(err => {
      const box = el('mq-history');
      if (box) box.innerHTML = `<div class="error-box">${esc(err.message)}</div>`;
    });
}

function mqVhostSection(cluster, v, snap, problemsOf, pf, flagged) {
  if (pf === 'anonymous') return '';
  const title = extra => `<h2 class="group">vhost <code>${esc(v.name)}</code>${extra ? ` <span class="muted">· ${extra}</span>` : ''}</h2>`;
  if (!v.accessible) {
    if (pf && pf !== 'noaccess') return '';
    return title('') + `<div class="notice-box">Нет прав: пользователю портала нужно разрешение в этом vhost —
      без него его топология не видна. Права выдаются на каждый vhost отдельно.</div>`;
  }
  if (pf === 'noaccess') return '';

  const queues = new Map(v.queues.map(q => [q.name, q]));
  const bound = new Set();
  const bySource = new Map();
  for (const b of v.bindings) {
    if (!bySource.has(b.source)) bySource.set(b.source, []);
    bySource.get(b.source).push(b);
    if (b.destinationType === 'queue') bound.add(b.destination);
  }
  const shownBinding = b => !(b.destinationType === 'queue' && queues.has(b.destination) && isTemporary(queues.get(b.destination)));

  const exchanges = v.exchanges.filter(x => !isBuiltIn(x)).map(x => {
    let rows = (bySource.get(x.name) || []).filter(shownBinding);
    if (pf) {
      const own = flagged(v.name, 'exchange', x.name);
      if (!own) rows = rows.filter(b => b.destinationType === 'queue' && flagged(v.name, 'queue', b.destination));
      if (!own && !rows.length) return '';
    }
    const body = rows.length ? `
      <div class="table-scroll"><table class="mq">
        <thead><tr><th>routing key</th><th>→ очередь</th><th>тип</th><th>потребители</th><th>DLX</th></tr></thead>
        <tbody>${rows.map(b => {
          const q = queues.get(b.destination);
          if (b.destinationType !== 'queue' || !q) {
            return `<tr><td class="mono muted">${esc(b.routingKey || '—')}</td>
              <td colspan="4">→ exchange <span class="mono">${esc(b.destination)}</span></td></tr>`;
          }
          return `<tr><td class="mono muted">${esc(b.routingKey || '—')}</td>${mqQueueCells(cluster, v, q, problemsOf)}</tr>`;
        }).join('')}</tbody>
      </table></div>` : `<div class="empty small">${x.deadLetter
        ? 'Это DLX, но к нему не привязана ни одна очередь: недоставленные сообщения теряются.'
        : 'Привязок нет: опубликованное сюда сообщение никуда не попадёт.'}</div>`;
    return `
      <section class="card mq-exchange">
        <div class="card-head">
          <span class="mq-kind">exchange</span>
          <span class="title mono">${esc(x.name)}</span>
          <span class="badge">${esc(x.type)}</span>
          ${x.deadLetter ? '<span class="badge">DLX</span>' : ''}
          ${problemBadges(problemsOf(v.name, 'exchange', x.name))}
          ${publishersCell(cluster, v.name, x, snap.publishersKnown)}
        </div>
        ${body}
      </section>`;
  }).join('');

  let direct = v.queues.filter(q => !isTemporary(q) && !bound.has(q.name));
  if (pf) direct = direct.filter(q => flagged(v.name, 'queue', q.name));
  const directCard = direct.length ? `
    <section class="card mq-exchange">
      <div class="card-head"><span class="title">Очереди без привязок к exchange</span>
        <span class="muted">в них публикуют напрямую, через exchange по умолчанию</span></div>
      <div class="table-scroll"><table class="mq">
        <thead><tr><th>очередь</th><th>тип</th><th>потребители</th><th>DLX</th></tr></thead>
        <tbody>${direct.map(q => `<tr>${mqQueueCells(cluster, v, q, problemsOf)}</tr>`).join('')}</tbody>
      </table></div>
    </section>` : '';

  const policies = !pf && v.policies.length ? `
    <section class="card mq-exchange">
      <div class="card-head"><span class="title">Политики</span>
        <span class="muted">задают DLX, TTL и лимиты сразу многим очередям</span></div>
      <div class="table-scroll"><table class="mq">
        <thead><tr><th>имя</th><th>шаблон</th><th>к чему</th><th>что задаёт</th><th class="num">очередей</th></tr></thead>
        <tbody>${v.policies.map(p => `
          <tr><td class="mono">${esc(p.name)}${p.operator ? ' <span class="badge">operator</span>' : ''}</td>
            <td class="mono muted">${esc(p.pattern)}</td>
            <td class="muted">${esc(p.applyTo || 'all')}</td>
            <td class="mono muted">${esc(Object.entries(p.definition || {}).map(([k, val]) =>
              `${k}: ${typeof val === 'object' ? JSON.stringify(val) : val}`).join(', '))}</td>
            <td class="num">${p.queues}</td></tr>`).join('')}</tbody>
      </table></div>
    </section>` : '';

  const links = [
    ...(v.shovels || []).map(s => ({ name: s.name, what: `shovel · ${s.type}`, route: `${s.source} → ${s.destination}`, state: s.state, kind: 'shovel' })),
    ...(v.federation || []).map(l => ({ name: l.upstream, what: `federation · ${l.type}`, route: `${l.upstream} → ${l.object}`, state: l.status, kind: 'federation' })),
  ].filter(l => !pf || flagged(v.name, l.kind, l.name));
  const linksCard = links.length ? `
    <section class="card mq-exchange">
      <div class="card-head"><span class="title">Связи с другими vhost и кластерами</span>
        <span class="muted">сообщения ходят между vhost только так</span></div>
      <div class="table-scroll"><table class="mq">
        <thead><tr><th>имя</th><th>что</th><th>маршрут</th><th>состояние</th></tr></thead>
        <tbody>${links.map(l => `
          <tr><td class="mono">${esc(l.name)}</td><td class="muted">${esc(l.what)}</td>
            <td class="mono muted">${esc(l.route)}</td>
            <td>${l.state === 'running' ? '<span class="badge">работает</span>'
              : `<span class="badge breaking">${esc(l.state)}</span>`}</td></tr>`).join('')}</tbody>
      </table></div>
    </section>` : '';

  const content = exchanges + directCard + policies + linksCard;
  if (!content) return '';
  const temporary = v.queues.filter(isTemporary).length;
  return title(`exchange ${v.exchanges.filter(x => !isBuiltIn(x)).length} · очередей ${v.queues.length - temporary}${
    temporary ? ` · временных ${temporary} (скрыты)` : ''}`) + content;
}

function bindMQ() {
  for (const button of document.querySelectorAll('[data-mq-cluster]')) {
    button.addEventListener('click', () => {
      state.mqCluster = button.dataset.mqCluster;
      state.mqVhost = '';
      state.mqProblem = '';
      renderMQ();
    });
  }
  for (const button of document.querySelectorAll('[data-mq-vhost]')) {
    button.addEventListener('click', () => { state.mqVhost = button.dataset.mqVhost; renderMQ(); });
  }
  for (const button of document.querySelectorAll('[data-mq-problem]')) {
    button.addEventListener('click', () => { state.mqProblem = button.dataset.mqProblem; renderMQ(); });
  }
  el('mq-refresh')?.addEventListener('click', async event => {
    event.target.disabled = true;
    try {
      await api('/api/mq/refresh', { method: 'POST' });
      toast('Снимок запрошен', 'ok');
      setTimeout(reload, 3000);
    } catch (err) {
      toast(err.message);
      event.target.disabled = false;
    }
  });
}

async function renderMQQueue() {
  const { cluster, vhost, name } = state.route;
  const broker = mqBrokers().find(b => b.cluster === cluster);
  const snap = broker?.snapshot;
  const v = snap?.vhosts?.find(x => x.name === vhost);
  const q = v?.queues?.find(x => x.name === name);

  let body;
  if (!q) {
    body = `<h1><span class="mono">${esc(name)}</span></h1>
      <div class="notice-box">Очереди нет в последнем снимке — возможно, её удалили. История ниже сохранилась.</div>
      ${mqReadersSection(cluster, vhost, name, null)}`;
  } else {
    const problems = problemsIndex(snap)(vhost, 'queue', name);
    const exchanges = new Map(v.exchanges.map(x => [x.name, x]));
    const incoming = v.bindings.filter(b => b.destinationType === 'queue' && b.destination === name);
    const dlx = dlxParam(q);
    let dlqs = [];
    if (dlx && dlx.value === '') {
      // DLX по умолчанию — это exchange "", он кладёт сообщение прямо в
      // очередь с именем из dead-letter-routing-key.
      const key = (q.effective || []).find(p => p.key === 'dead-letter-routing-key');
      if (key) dlqs = [String(key.value)];
    } else if (dlx) {
      dlqs = v.bindings.filter(b => b.source === String(dlx.value) && b.destinationType === 'queue').map(b => b.destination);
    }

    body = `
      <h1><span class="mono">${esc(q.name)}</span>
        <span class="badge">${esc(q.type)}</span>
        ${q.durable ? '<span class="badge">durable</span>' : ''}
        ${q.deadLetterQueue ? '<span class="badge">DLQ</span>' : ''}
        ${problemBadges(problems)}</h1>
      <ul class="facts">
        <li>подписчиков в момент снимка: <b>${q.consumers}</b></li>
        <li>снимок: <b title="${esc(absolute(snap.capturedAt))}">${esc(ago(snap.capturedAt))}</b></li>
        ${q.policy ? `<li>политика: <b>${esc(q.policy)}</b></li>` : ''}
        ${q.operatorPolicy ? `<li>operator policy: <b>${esc(q.operatorPolicy)}</b></li>` : ''}
      </ul>
      ${problems.map(p => `<div class="${MQ_QUIET.has(p.kind) ? 'notice-box' : 'error-box'}">${esc(p.detail)}</div>`).join('')}

      <h2>Откуда приходят сообщения</h2>
      ${incoming.length ? `<div class="table-scroll"><table class="mq">
        <thead><tr><th>exchange</th><th>routing key</th>${snap.publishersKnown ? '<th>публикуют</th>' : ''}</tr></thead>
        <tbody>${incoming.map(b => `
          <tr><td class="mono">${esc(b.source)}</td><td class="mono muted">${esc(b.routingKey || '—')}</td>
            ${snap.publishersKnown ? `<td>${partyChips(exchanges.get(b.source)?.publishers)}</td>` : ''}</tr>`).join('')}</tbody>
      </table></div>` : '<div class="empty small">Привязок нет: в очередь публикуют напрямую, через exchange по умолчанию.</div>'}

      ${mqReadersSection(cluster, vhost, name, q)}

      <h2>Куда уходят недоставленные</h2>
      ${!dlx ? '<div class="empty small">dead-letter-exchange не задан ни аргументом, ни политикой.</div>' : `
        <p>через ${dlx.value === '' ? 'exchange по умолчанию' : `exchange <code>${esc(String(dlx.value))}</code>`} →
        ${dlqs.length ? dlqs.map(n => `<a class="mono" href="${mqQueueHref(cluster, vhost, n)}">${esc(n)}</a>`).join(', ')
          : '<span class="err">ни в одну очередь — сообщения теряются</span>'}</p>`}

      <h2>Действующие параметры</h2>
      ${(q.effective || []).length ? `<div class="table-scroll"><table class="mq">
        <thead><tr><th>параметр</th><th>значение</th><th>откуда</th></tr></thead>
        <tbody>${q.effective.map(p => `
          <tr><td class="mono">${esc(p.key)}</td>
            <td class="mono">${esc(typeof p.value === 'object' ? JSON.stringify(p.value) : String(p.value))}</td>
            <td class="muted">${esc(MQ_SOURCE_LABEL[p.source] || p.source)}${p.sourceName ? ` <code>${esc(p.sourceName)}</code>` : ''}</td></tr>`).join('')}</tbody>
      </table></div>` : '<div class="empty small">Ни аргументов, ни политик: очередь работает с настройками брокера по умолчанию.</div>'}`;
  }

  el('main').innerHTML = `
    <div class="page">
      <p class="muted"><a href="#/mq">RabbitMQ</a> / ${esc(cluster)} / vhost <code>${esc(vhost)}</code> / очередь</p>
      ${body}
      <h2>История</h2>
      <div id="mq-history"><div class="empty small">Загрузка…</div></div>
    </div>`;
  bindDeclared(cluster, vhost, name);

  try {
    const params = new URLSearchParams({ cluster, vhost, object: name, kinds: 'queue,binding', limit: '50' });
    const data = await api(`/api/mq/changes?${params}`);
    const box = el('mq-history');
    if (box) box.innerHTML = (data.changes || []).map(mqChangeCard).join('') || '<div class="empty small">Изменений не было.</div>';
  } catch (err) {
    const box = el('mq-history');
    if (box) box.innerHTML = `<div class="error-box">${esc(err.message)}</div>`;
  }
}

// «Кто читает» на странице очереди: что видно в брокере, что заявлено и —
// администратору — форма, чтобы заявить. Это и есть способ сказать порталу
// про задачу, которая забирает очередь раз в сутки: брокер её не покажет.
function mqReadersSection(cluster, vhost, name, q) {
  const declared = declaredFor(cluster, vhost, 'queue', name, 'consume');
  const options = state.services.map(s => `<option value="${esc(label(s))}"></option>`).join('');
  return `
    <h2>Кто читает</h2>
    <p><span class="muted">видно в брокере:</span> ${q ? partyChips(q.consumedBy) : MQ_NOT_SEEN}</p>
    ${declared.length ? `<div class="table-scroll"><table class="mq">
      <thead><tr><th>сервис</th><th>как читает</th><th>комментарий</th><th>источник</th>${isAdmin() ? '<th></th>' : ''}</tr></thead>
      <tbody>${declared.map(d => `
        <tr><td>${declaredChips([{ ...d, mode: '' }])}</td>
          <td class="muted">${esc(MQ_MODE_LABEL[d.mode] || '—')}</td>
          <td class="muted">${esc(d.note || '')}</td>
          <td><span class="badge">${esc(MQ_DECLARED_SOURCE[d.source] || d.source)}</span></td>
          ${isAdmin() ? `<td>${d.source === 'manual'
            ? `<button class="btn small" data-undeclare="${d.id}">Убрать</button>` : ''}</td>` : ''}</tr>`).join('')}</tbody>
    </table></div>` : `<p class="muted">Заявленных читателей нет. Если очередь читают по расписанию
      или через basic.get, брокер этого не покажет — такую связь можно указать вручную.</p>`}
    ${isAdmin() ? `
      <form id="declare-form" class="form inline" style="flex-wrap: wrap">
        <input name="service" list="declare-services" placeholder="Сервис, например documents-archiver" required>
        <datalist id="declare-services">${options}</datalist>
        <select name="mode">
          ${Object.entries(MQ_MODE_LABEL).map(([k, text]) => `<option value="${k}" ${k === 'schedule' ? 'selected' : ''}>${text}</option>`).join('')}
        </select>
        <input name="note" placeholder="Комментарий: когда и зачем, например «каждую ночь в 03:00»">
        <button class="btn" type="submit">Указать читателя</button>
      </form>` : ''}`;
}

function bindDeclared(cluster, vhost, name) {
  el('declare-form')?.addEventListener('submit', async event => {
    event.preventDefault();
    const form = new FormData(event.target);
    try {
      await api('/api/mq/declared', {
        method: 'PUT',
        body: JSON.stringify({
          cluster, vhost, objectKind: 'queue', object: name, role: 'consume',
          service: form.get('service'), mode: form.get('mode'), note: form.get('note'),
        }),
      });
      await reload();
    } catch (err) {
      toast(err.message);
    }
  });
  for (const button of document.querySelectorAll('[data-undeclare]')) {
    button.addEventListener('click', async () => {
      try {
        await api(`/api/mq/declared/${button.dataset.undeclare}`, { method: 'DELETE' });
        await reload();
      } catch (err) {
        toast(err.message);
      }
    });
  }
}

// Где сервис участвует в обмене сообщениями. Сопоставление — по id в реестре:
// его проставляет сервер, когда connection_name или под совпали с сервисом.
function mqForService(svc) {
  const out = { consumes: [], publishes: [], connections: [], publishersKnown: true, any: false };
  for (const b of mqBrokers()) {
    const snap = b.snapshot;
    if (!snap) continue;
    if (!snap.publishersKnown) out.publishersKnown = false;
    for (const v of snap.vhosts || []) {
      for (const q of v.queues || []) {
        if ((q.consumedBy || []).some(p => p.serviceId === svc.id)) out.consumes.push({ cluster: b.cluster, v, q, snap });
      }
      for (const x of v.exchanges || []) {
        if ((x.publishers || []).some(p => p.serviceId === svc.id)) out.publishes.push({ cluster: b.cluster, v, x, snap });
      }
    }
    for (const c of snap.connections || []) {
      if (c.serviceId === svc.id) out.connections.push({ cluster: b.cluster, c });
    }
  }
  out.declared = mqDeclared().filter(d => serviceByName(d.service)?.id === svc.id);
  out.any = Boolean(out.consumes.length || out.publishes.length || out.connections.length || out.declared.length);
  return out;
}

function serviceMQ(svc) {
  const m = mqForService(svc);
  if (!m.any) {
    return `<div class="empty">В RabbitMQ сервис не видно: ни среди подписчиков, ни среди соединений,
      и связей для него не заявлено. Если он читает очередь по расписанию, это можно указать на странице очереди.</div>`;
  }
  const where = (cluster, v) => `${mqBrokers().length > 1 ? `${esc(cluster)} · ` : ''}<code>${esc(v.name)}</code>`;

  const declaredLinks = m.declared.length ? `
    <h2>Заявлено</h2>
    <p class="muted">Связи, которых в брокере может быть не видно: чтение по расписанию, через basic.get.</p>
    <div class="table-scroll"><table class="mq">
      <thead><tr><th>где</th><th>что</th><th>как</th><th>комментарий</th><th>источник</th></tr></thead>
      <tbody>${m.declared.map(d => `
        <tr><td>${where(d.cluster, { name: d.vhost })}</td>
          <td>${d.role === 'consume'
            ? `читает <a class="mono" href="${mqQueueHref(d.cluster, d.vhost, d.object)}">${esc(d.object)}</a>`
            : `публикует в <span class="mono">${esc(d.object)}</span>`}</td>
          <td class="muted">${esc(MQ_MODE_LABEL[d.mode] || '—')}</td>
          <td class="muted">${esc(d.note || '')}</td>
          <td><span class="badge">${esc(MQ_DECLARED_SOURCE[d.source] || d.source)}</span></td></tr>`).join('')}</tbody>
    </table></div>` : '';

  // Кто стоит до и после сервиса через брокер: публикаторы exchange, из которых
  // приходит в его очереди, и потребители очередей, куда уходит то, что он публикует.
  const upstream = new Map();
  const downstream = new Map();
  for (const { v, q } of m.consumes) {
    for (const b of v.bindings.filter(b => b.destinationType === 'queue' && b.destination === q.name)) {
      for (const p of v.exchanges.find(x => x.name === b.source)?.publishers || []) {
        if (p.serviceId !== svc.id) upstream.set(p.service, p);
      }
    }
  }
  for (const { v, x } of m.publishes) {
    for (const b of v.bindings.filter(b => b.source === x.name && b.destinationType === 'queue')) {
      for (const p of v.queues.find(q => q.name === b.destination)?.consumedBy || []) {
        if (p.serviceId !== svc.id) downstream.set(p.service, p);
      }
    }
  }
  const neighbours = (title, map, note) => `
    <section class="card mq-exchange">
      <div class="card-head"><span class="title">${title}</span></div>
      <div class="empty small">${map.size ? partyChips([...map.values()].map(p => ({ ...p, count: 1 }))) : note}</div>
    </section>`;

  const consumes = m.consumes.length ? `
    <h2>Потребляет</h2>
    <div class="table-scroll"><table class="mq">
      <thead><tr><th>где</th><th>очередь</th><th>откуда приходит</th><th class="num">потребителей</th></tr></thead>
      <tbody>${m.consumes.map(({ cluster, v, q, snap }) => `
        <tr><td>${where(cluster, v)}</td>
          <td><a class="mono" href="${mqQueueHref(cluster, v.name, q.name)}">${esc(q.name)}</a>
            ${problemBadges(problemsIndex(snap)(v.name, 'queue', q.name))}</td>
          <td class="mono muted">${esc(v.bindings.filter(b => b.destinationType === 'queue' && b.destination === q.name)
            .map(b => `${b.source}${b.routingKey ? ` [${b.routingKey}]` : ''}`).join(', ') || 'напрямую')}</td>
          <td class="num">${(q.consumedBy.find(p => p.serviceId === svc.id) || {}).count || 0}</td></tr>`).join('')}</tbody>
    </table></div>` : '';

  const publishes = m.publishes.length ? `
    <h2>Публикует</h2>
    <div class="table-scroll"><table class="mq">
      <thead><tr><th>где</th><th>exchange</th><th>куда попадает</th></tr></thead>
      <tbody>${m.publishes.map(({ cluster, v, x }) => `
        <tr><td>${where(cluster, v)}</td><td class="mono">${esc(x.name)} <span class="badge">${esc(x.type)}</span></td>
          <td>${v.bindings.filter(b => b.source === x.name).map(b =>
            `<span class="mono muted">${b.routingKey ? `[${esc(b.routingKey)}] ` : ''}</span>${b.destinationType === 'queue'
              ? `<a class="mono" href="${mqQueueHref(cluster, v.name, b.destination)}">${esc(b.destination)}</a>`
              : `exchange <span class="mono">${esc(b.destination)}</span>`}`).join('<br>') || '<span class="err">никуда: привязок нет</span>'}</td></tr>`).join('')}</tbody>
    </table></div>` : (m.publishersKnown ? '' : `<div class="notice-box">Что публикует сервис, брокер не сообщает:
      нужен <code>management.rates_mode = detailed</code>.</div>`);

  const connections = m.connections.length ? `
    <h2>Соединения с брокером</h2>
    <div class="table-scroll"><table class="mq">
      <thead><tr><th>где</th><th>адрес</th><th>connection_name</th><th>как опознан</th></tr></thead>
      <tbody>${m.connections.map(({ cluster, c }) => `
        <tr><td>${mqBrokers().length > 1 ? `${esc(cluster)} · ` : ''}<code>${esc(c.vhost)}</code></td>
          <td class="mono">${esc(c.peerHost)}</td>
          <td class="mono">${c.providedName ? esc(c.providedName) : '<span class="warn-text">не задано</span>'}</td>
          <td class="muted">${c.via === 'name' ? 'по имени' : 'по IP пода'}</td></tr>`).join('')}</tbody>
    </table></div>` : '';

  return `
    <div class="mq-neighbours">
      ${neighbours('Получает сообщения от', upstream, m.publishersKnown ? 'в брокере не видно' : 'брокер не сообщает, кто публикует')}
      ${neighbours('От него зависят через брокер', downstream, 'в брокере не видно')}
    </div>
    ${consumes}${publishes}${declaredLinks}${connections}`;
}

/* ---------- boot ---------- */

function render() {
  renderSidebar();
  switch (state.route.view) {
    case 'service': return renderService();
    case 'changes': return renderChanges();
    case 'settings': return renderSettings();
    case 'mq': return renderMQ();
    case 'mq-queue': return renderMQQueue();
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
