-- Сервис, у которого спека не нашлась, перестаёт опрашиваться автоматически.
--
-- Хост без API в кластере — обычное дело, и раз за разом перебирать по нему
-- полсотни путей бессмысленно: результат не изменится сам по себе. Он вернётся
-- в автоматический опрос, когда человек укажет адрес спеки и опрос вручную
-- завершится успехом.
ALTER TABLE services ADD COLUMN IF NOT EXISTS auto_probe BOOLEAN NOT NULL DEFAULT TRUE;

-- Отметка «новый» убрана: было непонятно, когда она снимается, а списки и так
-- показывают всё, что нашлось. Состояние остаётся двузначным: обычный сервис
-- или спрятанный.
UPDATE services SET review_state = 'reviewed' WHERE review_state = 'new';

-- Подробности сканирования: какие именно сервисы появились и какие исчезли.
-- Числа «найдено 9, новых 0» не отвечают на вопрос, что произошло.
ALTER TABLE discovery_runs ADD COLUMN IF NOT EXISTS added_names TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE discovery_runs ADD COLUMN IF NOT EXISTS gone_names TEXT[] NOT NULL DEFAULT '{}';
ALTER TABLE discovery_runs ADD COLUMN IF NOT EXISTS duration_ms INT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_services_auto_probe ON services (auto_probe);
