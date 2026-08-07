# Отложенное

Не «когда-нибудь потом», а решения, которые уже приняты, но не реализованы.
Здесь лежит то, ради чего не хочется второй раз проводить тот же разбор.

---

## Владельцы сервисов: команда, контакты, исходники

**Статус:** отложено, дизайн выбран.

**Задача.** У сервиса в портале должно быть видно, кто за него отвечает, как
с этими людьми связаться и где лежит код.

### Почему не отдельный реестр в Google Sheets

Рассматривалось и отклонено. Таблица — это ещё одна копия правды, которую надо
поддерживать руками, сматченная по хрупкому ключу. Через месяц она устареет и
будет врать молча. Матчинг по `info.title` ненадёжен вдвойне: заголовок не
уникален и меняется при рефакторинге.

### Что вместо

Три слоя, в порядке приоритета. Совпадение — **по идентификатору сервиса**
(backend `svc.namespace:port`), он стабилен между сканированиями.

**Слой 1. Сама спецификация.** В OpenAPI для этого есть штатное место, и портал
эти спеки уже скачивает — интеграции ноль.

```yaml
info:
  contact:
    name: Команда Профиля
    email: profile-team@company.ru
    url: https://wiki.company.ru/teams/profile
  x-team: profile
  x-repo: https://gitlab.company.ru/backend/profile-service
  x-owners: [ivanov, petrov]
externalDocs:
  url: https://gitlab.company.ru/backend/profile-service
```

Живёт в репозитории сервиса рядом с кодом: обновляется тем же MR, что и код.

**Слой 2. Аннотации на Ingress** — для сервисов, чья спека контактов не несёт.
Механизм уже есть (`spectra.io/name`, `/skip`), добавить ключи:

```yaml
spectra.io/team: profile
spectra.io/repo: https://gitlab.company.ru/backend/profile-service
spectra.io/owners: ivanov,petrov
spectra.io/chat: "#team-profile"
```

**Слой 3. Ручное заполнение в портале** — длинный хвост.

**Если центральный реестр всё-таки понадобится** («покажи все сервисы команды
X»), то файлом в git, который портал тянет по URL, а не через API таблиц.
Google Sheets тут годится источником: «Опубликовать в интернете → CSV» даёт
обычный URL без OAuth и токенов.

### Что делать

1. Портал читает `info.contact`, `externalDocs`, `x-team`/`x-repo`/`x-owners` и
   показывает на странице сервиса. Сейчас из спеки берутся только `title`,
   `version` и пути.
2. Аннотации на Ingress как второй источник.
3. Фильтр «без владельца» в списке адресов — видно, кого догонять, и это само
   создаёт давление на заполнение.
4. Страница-инструкция для команд с готовыми кусками кода. **Это определяет,
   взлетит или нет:** если каждой команде разбираться самой, не заполнит никто.

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
статически. У l5-swagger для этого есть подстановка констант, объявляемых в
`config/l5-swagger.php`:

```php
'constants' => [
    'API_TEAM_NAME'  => env('API_TEAM_NAME',  'Команда Профиля'),
    'API_TEAM_EMAIL' => env('API_TEAM_EMAIL', 'profile-team@company.ru'),
    'API_REPO_URL'   => env('API_REPO_URL',   'https://gitlab.company.ru/...'),
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

Синтаксис `overrides` менялся между мажорными версиями Scribe — свериться с
установленной.

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
