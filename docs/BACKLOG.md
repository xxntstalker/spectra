# Отложенное

Не «когда-нибудь потом», а решения, которые уже приняты, но не реализованы.
Здесь лежит то, ради чего не хочется второй раз проводить тот же разбор.

---

## Владельцы сервисов — сделано

Реализовано: три источника (спека, аннотации Ingress, ручной ввод) с
разрешением по каждому полю, фильтр «без владельца», счётчик. См. README.

Осталось из первоначального плана: **страница-инструкция для команд** с
готовыми кусками кода под l5-swagger, Scribe, springdoc. Это определяет,
дойдёт ли дело до автоматических источников вообще: если каждой команде
разбираться самой, не заполнит никто. Заготовки — ниже.

### Заготовки для инструкции

**Laravel, l5-swagger / swagger-php.** Блок `info` объявляется один раз, обычно
в `app/Http/Controllers/Controller.php`:

```php
/**
 * @OA\Info(
 *     title="Profile API",
 *     version="1.0.0",
 *     x={"team": "profile", "repo": "https://gitlab.company.ru/backend/profile"},
 *     @OA\Contact(name=API_TEAM_NAME, email=API_TEAM_EMAIL)
 * )
 * @OA\ExternalDocumentation(url="https://gitlab.company.ru/backend/profile")
 */
```

Внутри аннотаций **нельзя вызывать `config()` или `env()`** — они вычисляются
статически. У l5-swagger для этого есть подстановка констант из
`config/l5-swagger.php`:

```php
'constants' => [
    'API_TEAM_NAME'  => env('API_TEAM_NAME',  'Команда Профиля'),
    'API_TEAM_EMAIL' => env('API_TEAM_EMAIL', 'profile-team@company.ru'),
],
```

Тогда значения приезжают из `.env`, а в CI можно подставить `CI_PROJECT_URL` —
адрес репозитория проставится сам.

**Laravel, Scribe** — без аннотаций, в `config/scribe.php`:

```php
'openapi' => ['enabled' => true, 'overrides' => [
    'info.contact.name'  => 'Команда Профиля',
    'info.contact.email' => 'profile-team@company.ru',
    'externalDocs.url'   => 'https://gitlab.company.ru/backend/profile',
]],
```

Синтаксис `overrides` менялся между мажорными версиями Scribe.

**Аннотации на Ingress** — если PHP-код править не хотят:

```yaml
spectra.io/team: "Команда Профиля"
spectra.io/contact: profile-team@company.ru
spectra.io/repo: https://gitlab.company.ru/backend/profile
spectra.io/owners: ivanov,petrov
```

---

## Известные пробелы

**Вернувшийся сервис в журнале сканирований.** Сервис, пропавший из кластера и
появившийся снова, показывается как «изменений нет»: он не новый (запись уже
есть), а признак `present` меняется молча. Нужен третий список — «вернулись».
Правится в `SyncDiscovered`: до upsert собрать ключи с `present = false`, после
— пересечь с найденными.

**Тесты на `store`, `collector`, `api`.** Были при SQLite, удалены при переезде
на Postgres — тестировали старый API. Покрыты `openapi`, `discovery` и `k8s`.
Для остальных нужен Postgres в тестах: контейнер или `SPECTRA_TEST_DATABASE_URL`
со скипом, если базы нет.

**Уведомления о ломающих изменениях** (Slack, почта). Данные готовы: признак
`breaking` лежит в таблице `changes`.

**Метрики `/metrics`.** Сейчас только `/healthz` и логи.

**Поиск по эндпоинтам через все сервисы.** Требует индекса операций в базе —
сейчас он строится в памяти при сравнении.

**Аутентификация сложнее Basic.** Предполагается SSO впереди портала.
