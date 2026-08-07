-- Реестр больше не файл в git: сервисы приходят из Kubernetes и настраиваются
-- в интерфейсе, поэтому весь реестр живёт здесь.

CREATE TABLE IF NOT EXISTS groups (
    id         BIGSERIAL PRIMARY KEY,
    name       TEXT        NOT NULL UNIQUE,
    position   INT         NOT NULL DEFAULT 0,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS services (
    id  BIGSERIAL PRIMARY KEY,
    -- Стабильный идентификатор между синхронизациями: по нему сервис из
    -- кластера узнаётся повторно и не заводится заново.
    key TEXT NOT NULL UNIQUE,

    source       TEXT NOT NULL DEFAULT 'kubernetes',  -- kubernetes | manual
    display_name TEXT NOT NULL,
    base_url     TEXT NOT NULL,

    -- Три независимых измерения. Складывать их в одно поле нельзя:
    -- «человек ещё не смотрел» и «спека не найдена» — разные вещи, и хост
    -- без API в принципе не должен вечно висеть как проблема.
    review_state TEXT NOT NULL DEFAULT 'new',      -- new | reviewed | ignored
    probe_status TEXT NOT NULL DEFAULT 'unknown',  -- unknown | ok | no_spec | unreachable | invalid_spec
    present      BOOLEAN NOT NULL DEFAULT TRUE,    -- ещё существует в кластере

    -- Откуда сервис взялся в кластере.
    host         TEXT,
    namespace    TEXT,
    ingress_name TEXT,
    backend      TEXT,

    group_id BIGINT REFERENCES groups (id) ON DELETE SET NULL,
    tags     TEXT[] NOT NULL DEFAULT '{}',
    notes    TEXT   NOT NULL DEFAULT '',

    spec_url        TEXT,
    spec_url_source TEXT,  -- probe | swagger-html | manual
    -- Адрес, введённый руками. Хранится отдельно от найденного, чтобы
    -- переоткрытие не затирало решение человека.
    manual_spec_url TEXT,
    etag            TEXT,
    last_modified   TEXT,

    last_error      TEXT NOT NULL DEFAULT '',
    last_checked_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    fail_count      INT NOT NULL DEFAULT 0,
    -- Разрежение повторных попыток: пока не наступит, полный перебор путей
    -- не запускается.
    next_probe_at   TIMESTAMPTZ,

    first_seen_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Проставляется, когда человек посмотрел на сервис: снимает отметку «новый».
    acknowledged_at TIMESTAMPTZ,
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_services_review ON services (review_state);
CREATE INDEX IF NOT EXISTS idx_services_group ON services (group_id);
CREATE INDEX IF NOT EXISTS idx_services_present ON services (present);

CREATE TABLE IF NOT EXISTS spec_versions (
    id             BIGSERIAL PRIMARY KEY,
    service_id     BIGINT      NOT NULL REFERENCES services (id) ON DELETE CASCADE,
    hash           TEXT        NOT NULL,
    captured_at    TIMESTAMPTZ NOT NULL,
    spec           JSONB       NOT NULL,
    title          TEXT        NOT NULL DEFAULT '',
    api_version    TEXT        NOT NULL DEFAULT '',
    spec_version   TEXT        NOT NULL DEFAULT '',
    endpoint_count INT         NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_versions_service ON spec_versions (service_id, id DESC);

CREATE TABLE IF NOT EXISTS changes (
    id              BIGSERIAL PRIMARY KEY,
    service_id      BIGINT      NOT NULL REFERENCES services (id) ON DELETE CASCADE,
    from_version_id BIGINT,
    to_version_id   BIGINT      NOT NULL,
    detected_at     TIMESTAMPTZ NOT NULL,
    breaking        BOOLEAN     NOT NULL DEFAULT FALSE,
    added           INT         NOT NULL DEFAULT 0,
    removed         INT         NOT NULL DEFAULT 0,
    modified        INT         NOT NULL DEFAULT 0,
    diff            JSONB       NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_changes_detected ON changes (detected_at DESC);

-- Эксплуатационные настройки, которые крутят в интерфейсе. Всё, что нужно
-- до подключения к базе, остаётся в переменных окружения.
CREATE TABLE IF NOT EXISTS settings (
    key        TEXT PRIMARY KEY,
    value      TEXT        NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Журнал синхронизаций с кластером: сколько хостов пришло, сколько новых.
CREATE TABLE IF NOT EXISTS discovery_runs (
    id         BIGSERIAL PRIMARY KEY,
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    source     TEXT        NOT NULL,
    found      INT         NOT NULL DEFAULT 0,
    added      INT         NOT NULL DEFAULT 0,
    gone       INT         NOT NULL DEFAULT 0,
    error      TEXT        NOT NULL DEFAULT ''
);
