-- Возврат к ранее виденному состоянию — это откат, а не изменение.
--
-- Сервис выкатили, Spectra прочитала новую спеку, выкатку откатили. Spectra
-- честно подхватывает старую спеку следующим опросом — данные восстанавливаются
-- сами. Но в ленте это выглядит как «удалены эндпоинты», и уведомление о
-- ломающем изменении сработает без объяснения причины.
--
-- Хэш нормализованной спеки уже хранится, поэтому совпадение с прежней
-- ревизией распознаётся точно.
ALTER TABLE spec_versions ADD COLUMN IF NOT EXISTS rollback_of INT;

COMMENT ON COLUMN spec_versions.rollback_of IS
    'Номер ревизии этого же сервиса, к состоянию которой произошёл возврат';

-- Проставляем задним числом: для каждой ревизии ищем самую раннюю с тем же
-- хэшем, если она не первая.
UPDATE spec_versions v
SET rollback_of = earlier.revision
FROM (
    SELECT service_id, hash, min(revision) AS revision
    FROM spec_versions GROUP BY service_id, hash
) AS earlier
WHERE v.service_id = earlier.service_id
  AND v.hash = earlier.hash
  AND v.revision > earlier.revision
  AND v.rollback_of IS NULL;

CREATE INDEX IF NOT EXISTS idx_versions_hash ON spec_versions (service_id, hash);
