-- Ревизии нумеруются внутри сервиса, а не сквозным счётчиком.
--
-- Раньше в интерфейсе показывался первичный ключ, общий для всех сервисов:
-- у одного «Ревизия #13», у соседнего «#8», хотя у обоих их по три. Номер
-- ничего не сообщал и только сбивал с толку.
--
-- Первичный ключ остаётся прежним: на него ссылается таблица changes.
-- Добавляется отдельный человеческий номер.
ALTER TABLE spec_versions ADD COLUMN IF NOT EXISTS revision INT;

UPDATE spec_versions v
SET revision = numbered.rn
FROM (
    SELECT id, row_number() OVER (PARTITION BY service_id ORDER BY id) AS rn
    FROM spec_versions
) AS numbered
WHERE v.id = numbered.id AND v.revision IS NULL;

ALTER TABLE spec_versions ALTER COLUMN revision SET NOT NULL;

-- Гонка между плановым обходом и ручным опросом одного сервиса маловероятна,
-- но при ней два номера совпали бы молча. Пусть лучше вставка упадёт.
CREATE UNIQUE INDEX IF NOT EXISTS idx_versions_revision
    ON spec_versions (service_id, revision);
