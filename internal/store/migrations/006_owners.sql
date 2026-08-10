-- Владелец сервиса: команда, контакт, исходники, люди.
--
-- Два набора колонок, а не один. Автоматика (спека и аннотации на Ingress)
-- пишет в spec_*, человек — в manual_*, действующим считается manual, если он
-- заполнен. Так синк из кластера никогда не затирает то, что вписали руками,
-- и наоборот — если команда однажды добавит контакты в спеку, они подтянутся
-- в пустые поля сами.
--
-- Разрешение идёт по каждому полю отдельно: репозиторий вполне может приехать
-- из externalDocs, пока название команды вписано вручную.
ALTER TABLE services ADD COLUMN IF NOT EXISTS manual_team    TEXT;
ALTER TABLE services ADD COLUMN IF NOT EXISTS manual_contact TEXT;
ALTER TABLE services ADD COLUMN IF NOT EXISTS manual_repo    TEXT;
ALTER TABLE services ADD COLUMN IF NOT EXISTS manual_owners  TEXT[];

ALTER TABLE services ADD COLUMN IF NOT EXISTS spec_team    TEXT;
ALTER TABLE services ADD COLUMN IF NOT EXISTS spec_contact TEXT;
ALTER TABLE services ADD COLUMN IF NOT EXISTS spec_repo    TEXT;
ALTER TABLE services ADD COLUMN IF NOT EXISTS spec_owners  TEXT[];

-- Фильтр «без владельца» — то, ради чего это всё и заводится в первой
-- итерации: данных не будет нигде, и надо видеть, кого догонять.
CREATE INDEX IF NOT EXISTS idx_services_no_owner ON services
    ((COALESCE(NULLIF(manual_team, ''), NULLIF(spec_team, '')) IS NULL));
