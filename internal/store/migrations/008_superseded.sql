-- Слияние дублей одного API.
--
-- Реестр опознаёт сервис по backend-Service: так одна запись переживает
-- переезд публичного хоста. Но бывает наоборот — несколько Ingress в разных
-- namespace объявляют один и тот же хост и путь, каждый на свой backend
-- (переезд между namespace, забытая копия, blue/green). Снаружи это один
-- адрес и один API, а в реестре получалось несколько записей: одинаковые
-- имена, разные namespace, один хост.
--
-- Проигравшие не удаляются: у них своя история ревизий, а решение о слиянии
-- принято по текущему состоянию кластера и может смениться. Они помечаются
-- ссылкой на победителя, выпадают из списков и из опроса — но остаются на
-- случай разбирательства и возвращаются сами, если хост снова разъедется.
ALTER TABLE services ADD COLUMN IF NOT EXISTS superseded_by BIGINT;

-- Публичный адрес — хост и префикс пути, без backend за ними. По нему и
-- группируются записи, которые снаружи неразличимы.
ALTER TABLE services ADD COLUMN IF NOT EXISTS public_key TEXT;

CREATE INDEX IF NOT EXISTS idx_services_public_key ON services (public_key)
    WHERE public_key IS NOT NULL AND present;

CREATE INDEX IF NOT EXISTS idx_services_superseded ON services (superseded_by)
    WHERE superseded_by IS NOT NULL;

-- Существующие дубли схлопнет ближайшее сканирование кластера: кто с кем
-- совпадает, видно из Ingress, а не из того, что уже лежит в базе.
