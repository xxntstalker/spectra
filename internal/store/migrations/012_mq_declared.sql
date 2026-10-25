-- Кто читает очередь или публикует в exchange — заявлено, а не увидено.
--
-- Снимок брокера видит только тех, кто подписан в момент снимка. Задача,
-- которая забирает очередь раз в сутки, и чтение через basic.get в снимок не
-- попадают никогда, поэтому «в брокере не видно» не значит «никто не читает».
-- Это знание приходит от людей и из кода, и живёт здесь, рядом со снимком,
-- а не в нём: снимок перезаписывается каждый час.
--
-- source различает, кто заявил: manual — вписал человек на странице очереди,
-- code — сканер репозиториев в CI (появится позже и будет писать сюда же).
-- Одна и та же связь может быть заявлена обоими источниками сразу.
CREATE TABLE IF NOT EXISTS mq_declared (
    id          BIGSERIAL PRIMARY KEY,
    cluster     TEXT        NOT NULL,
    vhost       TEXT        NOT NULL,
    object_kind TEXT        NOT NULL,              -- queue | exchange
    object_name TEXT        NOT NULL,
    role        TEXT        NOT NULL,              -- consume | publish
    service     TEXT        NOT NULL,
    mode        TEXT        NOT NULL DEFAULT '',   -- subscribe | schedule | get | manual
    note        TEXT        NOT NULL DEFAULT '',
    source      TEXT        NOT NULL DEFAULT 'manual',
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (cluster, vhost, object_kind, object_name, role, service, source)
);
