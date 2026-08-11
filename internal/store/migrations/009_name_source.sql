-- Откуда взялось отображаемое имя.
--
-- display_name по умолчанию — имя backend-Service, и по кластеру оно
-- повторяется постоянно: десяток «api», «web», «backend». Настоящее название
-- сервиса лежит в спеке (info.title), но перекрывать им имя, которое человек
-- вписал руками или объявил аннотацией spectra.io/name, нельзя.
--
-- Флаг разделяет эти случаи: явное имя показывается как есть, выведенное из
-- имени Service уступает названию из спеки.
ALTER TABLE services ADD COLUMN IF NOT EXISTS name_is_explicit BOOLEAN NOT NULL DEFAULT FALSE;

-- Существующие записи: имя считается выведенным ровно тогда, когда оно
-- совпадает с именем backend-Service — именно так его и строит дискавери.
-- Всё остальное кто-то назвал сам (аннотация или правка в интерфейсе), и это
-- сохраняется.
UPDATE services
SET name_is_explicit = (display_name IS DISTINCT FROM split_part(COALESCE(backend, ''), '.', 1));
