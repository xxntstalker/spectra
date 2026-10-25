-- RabbitMQ: последний снимок топологии каждого брокера и история её изменений.
--
-- Снимок хранится целиком, текстом, как спека: внутрь него не запрашиваем,
-- он только кладётся и отдаётся странице. На реальном объёме — десятки vhost,
-- сотни очередей — это дешевле, чем раскладывать каждый объект в строку.
--
-- Отдельно от снимка лежат definitions — та часть, по которой ведётся история
-- (что существует и кто подключён, без счётчиков сообщений). Запись в
-- mq_changes появляется только при смене их хэша, а не при каждом опросе:
-- тот же принцип, что у ревизий спек.
CREATE TABLE IF NOT EXISTS mq_state (
    cluster         TEXT PRIMARY KEY,
    snapshot        TEXT NOT NULL DEFAULT '',
    definitions     TEXT NOT NULL DEFAULT '',
    hash            TEXT NOT NULL DEFAULT '',
    captured_at     TIMESTAMPTZ,
    last_checked_at TIMESTAMPTZ,
    last_success_at TIMESTAMPTZ,
    last_error      TEXT NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS mq_changes (
    id          BIGSERIAL PRIMARY KEY,
    cluster     TEXT        NOT NULL,
    vhost       TEXT        NOT NULL DEFAULT '',
    object_kind TEXT        NOT NULL,
    object_name TEXT        NOT NULL DEFAULT '',
    kind        TEXT        NOT NULL,
    severity    TEXT        NOT NULL DEFAULT 'info',
    lines       TEXT[]      NOT NULL DEFAULT '{}',
    detected_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_mq_changes_detected ON mq_changes (detected_at DESC);
CREATE INDEX IF NOT EXISTS idx_mq_changes_object
    ON mq_changes (cluster, vhost, object_name, id DESC);
