# AGENTS.md — Spectra

Документ для агентов, работающих с этим репозиторием. По-русски, потому что
по-русски написана вся документация проекта (`README.md`, `deploy/DEPLOY.md`,
`deploy/k8s/README.md`, `docs/BACKLOG.md`), сообщения коммитов, интерфейс и
тексты ошибок для человека.

Пометки у каждой команды и у каждого факта:

- **выполнено** — запущено в этой ревизии (`f44f287`, 2026-09-07) на
  macOS 24.5.0 / go1.26.4 darwin-arm64 / Docker Server 29.4.0, результат ниже;
- **найдено в `файл:строка`** — лежит в репозитории дословно, но не выполнялось;
- всё, что не удалось ни выполнить, ни найти, — в §9 «Не установлено».

Ссылки вида `файл:строка` сверены с содержимым файлов в этой ревизии.

---

## 1. Что это

**Spectra** — портал-агрегатор OpenAPI/Swagger-спецификаций сервисов кластера
Kubernetes. Читает объекты `Ingress` во всех namespace (тот же вызов API, что
`kubectl get ing -A`; интервал по умолчанию — час, `internal/config/config.go:131`),
для каждого найденного сервиса сам ищет спецификацию: сначала 14 HTML-страниц
документации, потом 31 конвенциональный путь — 45 адресов всего
(`internal/discovery/discovery.go:32-98`, порядок перебора — там же, `:255-286`).
Хранит все ревизии спек, считает дату последнего изменения контракта по хэшу
нормализованной спеки и показывает ленту изменений на уровне эндпоинтов с
пометкой ломающих. Аудитория — разработчики, которые ищут чужой API, и DevOps,
который портал разворачивает.

Один Go-модуль `spectra` (`go.mod:1`), одна бинарка (`cmd/spectra`), состояние
целиком в Postgres. Прикладных зависимостей две: `github.com/jackc/pgx/v5` и
`gopkg.in/yaml.v3` (`go.mod:5-8`). Kubernetes — напрямую по HTTP, без client-go.

Размер (выполнено, `git ls-files` + `wc -l`): 66 файлов под git; Go без тестов —
6525 строк, из них 1080 приходится на демо-стенд `demo/`; тесты — 4169 строк,
**126 функций `Test*`, 159 тест-кейсов** с учётом подтестов (ни бенчмарков, ни
фаззинга нет); миграции — 318 строк SQL; UI — 1496 строк (`app.js`, `style.css`,
`index.html`, `viewer.html`) плюс вендоренный вьюер Scalar на 2.7 МБ.

**Чем опасна неаккуратная правка.** Spectra — гость на чужой инфраструктуре: она
сама ходит запросами в чужие сервисы изнутри кластера. Три класса ущерба, каждый
из которых виден не сразу:

1. **Лишний трафик у чужих сервисов.** Ошибка в блокировках коллектора
   (advisory-lock на реплику, мьютекс на свип, backoff) умножает нагрузку на
   каждый опрашиваемый сервис. Заметит это чужая команда, а не мы.
   Опора: `internal/collector/collector.go:1-7`, `internal/store/store.go:34-37`.
2. **SSRF.** Чтение портала открыто без логина, а портал умеет ходить по
   адресам. Ослабление проверок в `internal/discovery/manual.go` или
   `internal/api/proxy.go` превращает портал в способ дотянуться до чего угодно
   внутри кластера. Опора: `manual.go:10-13`, `proxy.go:12-18`.
3. **Утечка топологии.** `/api/services` доступен курлом анонимно; что именно
   вырезается для анонима, решает `internal/api/redact.go:27-42`, а не
   JavaScript (`redact.go:25-26` говорит это прямым текстом).

---

## 2. Карта репозитория

```
cmd/spectra          точка входа; флаг -health-check для HEALTHCHECK в образе
internal/config      конфигурация из переменных окружения (config.go) + TLS (tls.go)
internal/k8s         чтение Ingress, группировка, аннотации spectra.io/*;
                     pods.go — поды по IP для RabbitMQ (право list pods необязательно)
internal/discovery   поиск спеки у сервиса; manual.go — разбор ручного адреса
internal/openapi     разбор Swagger 2.0 / OpenAPI 3.0/3.1, нормализация, sha256,
                     индекс операций, дифф, извлечение владельца
internal/rabbitmq    Management API: снимок топологии, действующие параметры
                     очередей, проблемы, хэш и дифф для истории
internal/collector   опрос по расписанию, запись ревизий, запасной адрес;
                     rabbitmq.go — отдельный цикл снимков брокеров
internal/store       Postgres: миграции (001–012), реестр, ревизии, настройки, локи;
                     mq.go — снимки брокеров, история топологии, заявленные связи
internal/api         HTTP API, метрики, прокси, сессии, редакция, отдача UI
internal/web         go:embed статики + вендоренный вьюер Scalar
demo/                отдельная программа: семь фейковых сервисов (7101–7107)
deploy/              манифесты k8s, DEPLOY.md и deploy/k8s/README.md
docs/BACKLOG.md      отложенное с уже принятыми решениями + «Известные пробелы»
```

Основа — `README.md:264-276`; расхождения с ним (`config` без TLS, `openapi` без
`owner.go`) уточнены по `git ls-files` (выполнено).

### Чего не читать

**`internal/web/static/vendor/scalar.standalone.js` — 2 705 КБ, не открывайте.**
Это собранный минифицированный вьюер Scalar, скачанный целиком с jsdelivr
(`Makefile:77`). Читать в нём нечего: он не наш, не правится вручную и
обновляется только целиком через `make vendor-viewer`. Один `cat` этого файла
стоит больше токенов, чем весь остальной репозиторий вместе взятый — весь
остальной код и документация занимают около 550 КБ.

Запрета на его чтение нигде нет: `.gitignore` управляет только git, а сам файл
там ещё и force-included строкой `!internal/web/static/vendor/`, потому что без
него не собирается `go:embed`. Так что это соглашение, а не механизм — на него
и рассчитываем.

Всё, что нужно про него знать: он есть, он вшивается в бинарник, он кэшируется
на сутки (`internal/api/api.go`, `staticCache`), и без него сборка падает.

---

## 3. Команды

### 3.1. Проверка изменения — то, что обязан прогнать verifier

| Команда | Пометка | Что делает |
|---|---|---|
| `make test` | **выполнено**; `Makefile:52` | поднимает одноразовый Postgres 17 в контейнере `spectra-test-pg` на порту 55432, гоняет `go test ./...`, гасит контейнер |
| `make test-quick` | **выполнено**; `Makefile:67`, `README.md:258` | `go test ./...` без Postgres — **65 тест-кейсов из 159 при этом молча пропускаются**, см. §8 |
| `make build` | **выполнено**; `Makefile:73` | `go build -o bin/spectra ./cmd/spectra` |
| `gofmt -l .` | **выполнено** | ничего не печатает — форматирование в порядке |
| `go vet ./...` | **выполнено** | молча, ошибок нет. **В Makefile такой цели нет** — это дополнительная проверка, а не договорённость проекта |
| `make fmt` | найдено в `Makefile:70` | `gofmt -w .`. Не выполнялось: переписывает файлы. Эквивалентная проверка — `gofmt -l .` выше |

Вывод `make test` после `go clean -testcache` (**выполнено**):

```
жду Postgres.
?   	spectra/cmd/spectra	[no test files]
?   	spectra/demo	[no test files]
ok  	spectra/internal/api	3.606s
ok  	spectra/internal/collector	12.425s
ok  	spectra/internal/config	1.148s
ok  	spectra/internal/discovery	0.957s
ok  	spectra/internal/k8s	0.740s
ok  	spectra/internal/openapi	1.363s
ok  	spectra/internal/store	3.969s
?   	spectra/internal/web	[no test files]
MAKE_EXIT=0
```

Полный прогон с настоящим Postgres (**выполнено**, отдельный контейнер на порту
55433, `SPECTRA_TEST_DATABASE_URL=… go test ./... -count=1 -v`):
**PASS=159 SKIP=0 FAIL=0**.

`make build` (**выполнено**): `go build -o bin/spectra ./cmd/spectra`, бинарник
19,6 МБ в `bin/` (каталог в `.gitignore:5`).

### 3.2. Запуск локально

| Команда | Пометка | Результат |
|---|---|---|
| `make demo-up` | **выполнено**; `Makefile:23`, `README.md:259` | поднялись postgres, demo, spectra; `GET /healthz` → `200`; `spectra_up 1`, `spectra_services_total 8`, `spectra_services{state="ok"} 7`, `spectra_services{state="unreachable"} 1`, `spectra_poller_active 1`. Порт 8080, логин `admin`/`admin` |
| `make demo-down` | **выполнено**; `Makefile:27` | `docker compose -f demo/docker-compose.yml down -v` — контейнеры, сеть и данные удалены |
| `make demo-logs` | найдено в `Makefile:30` | — |
| `make up` | **выполнено**; `Makefile:35` | поднялись postgres и spectra; `GET /healthz` → `200`; `GET /api/services` → `{"services":[]}` (реестр пуст: без Kubernetes и без `SPECTRA_STATIC_SERVICES` наполнять его нечем, `docker-compose.yml:40`) |
| `make down` | **выполнено**; `Makefile:39` | остановлено; том `swaggeragregate_pgdata` остаётся (`docker-compose.yml:47-48`) |
| `make logs` | найдено в `Makefile:42` | — |
| `make image` | **выполнено**; `Makefile:45`, `README.md:219` | `docker build -t spectra:latest .` → образ **16 МБ**, distroless, nonroot (`Dockerfile:21-31`) |
| `make help` | **выполнено**; `Makefile:4` | цель по умолчанию, печатает список целей |
| `make vendor-viewer` | найдено в `Makefile:77` | тянет Scalar 1.25.130 с jsdelivr. Не выполнялось: меняет вендоренный файл на 2.7 МБ |

Восьмой демо-адрес (`http://demo:7199`) мёртв намеренно — так выглядит
недоступный хост (`demo/docker-compose.yml:45-48`). Отсюда
`state="unreachable" 1` выше.

### 3.3. Kubernetes

Найдено в `deploy/k8s/README.md:5-11` (**не выполнялось**, см. §9):

```bash
docker build -t spectra:dev .
docker build -f demo/Dockerfile -t spectra-demo:dev .
kubectl apply -f deploy/k8s/
kubectl -n spectra rollout status deploy/spectra
kubectl -n spectra port-forward svc/spectra 8080:80
```

Проверка результата — `deploy/k8s/README.md:43-46`:

```bash
kubectl -n spectra port-forward svc/spectra 8080:80
curl -s localhost:8080/metrics | grep spectra_services
```

Ожидаемое: из 15 объектов Ingress в реестр попадают 10; таблица «что с каждым» —
`deploy/k8s/README.md:27-40`. Бэкап и восстановление — `deploy/DEPLOY.md:312-327`.

### 3.4. CI и линтеры

**CI нет.** Проверено (выполнено, `ls -a` + `find` + `git ls-files`): нет
`.github/`, `.gitlab-ci.yml`, `Jenkinsfile`, `.circleci` — ничего. Это заявлено
и в `README.md:245` («CI не настроен»), и в `docs/BACKLOG.md:93`.

**Конфигурации линтера нет:** ни `.golangci.yml`, ни `.pre-commit-config.yaml`,
ни `justfile`/`Taskfile` (проверено `find`, выполнено). Линтер проекта —
`gofmt` через `make fmt`, и больше ничего.

Единственная честная запись о том, что должно проходить, — `Makefile` и
`README.md:255-260`.

---

## 4. Что нужно поставить и чего нет в свежем worktree

- **`AGENTS.md` не под git.** `git status --short` (выполнено) показывает
  `?? AGENTS.md`; в `.gitignore` и `.git/info/exclude` его нет. **В свежем
  worktree этого файла не будет.** Роль, которой он нужен, должна получить его
  копию явно — или файл надо закоммитить (это решение человека, не онбордера).
- **Go 1.25+.** `go.mod:3` объявляет `go 1.25.0`; образ собирается на
  `golang:1.25-alpine` (`Dockerfile:3`). Локально go1.26.4 — собирается и
  тестируется без замечаний (выполнено).
- **Docker.** Нужен для `make test` (одноразовый Postgres), `make demo-up`,
  `make up`, `make image`. Проверено на Docker Server 29.4.0.
- **Свободные порты:** 55432 (`make test`, `Makefile:56`), 8080 (`make up` /
  `make demo-up`), 7101–7107 (демо-сервисы, `demo/docker-compose.yml:27`).
- **`.env` не существует и для тестов не нужен.** Он в `.gitignore:120`;
  образец — `.env.example` (исключён из игнора, `.gitignore:122`).
  `docker-compose.yml:31-44` задаёт все значения через `${VAR:-…}`, поэтому
  `make up` работает и без `.env` (выполнено).
- **Генерировать ничего не нужно.** Вьюер Scalar
  (`internal/web/static/vendor/scalar.standalone.js`, 2.7 МБ) лежит в git
  намеренно — в `.gitignore:29-30` под него сделано исключение
  `!internal/web/static/vendor/`. `go:embed static` (`internal/web/web.go:14`)
  собирается в свежем worktree.
- **`.idea/` (`.gitignore:80`) и `.claude/` (`.gitignore:116`) не под git** —
  их отсутствие в worktree нормально.
- **Кластер Kubernetes для тестов не нужен:** `internal/k8s` тестируется против
  поддельного HTTP-сервера (`internal/k8s/ingress_test.go`), 26 тестов проходят
  без Docker и без кластера (выполнено).

---

## 5. Что нельзя ломать

Каждый пункт — с опорой на код, тест, комментарий или документ. Пунктов без
опоры здесь нет.

### 5.1. Публичная поверхность

**HTTP-маршруты** (`internal/api/api.go:41-73`). Их знает встроенный UI
(`internal/web/static/app.js`) и, по `README.md:195`, посторонние скрипты с
курлом. Разделение на открытые и закрытые — часть контракта:

| Открыто без логина | Только под логином (`requireAdmin`) |
|---|---|
| `GET /healthz`, `GET /metrics` | `POST /api/login`, `POST /api/logout` |
| `GET /api/overview` | `PATCH /api/services/{id}` |
| `GET /api/services`, `/{id}`, `/{id}/spec` | `POST /api/services/{id}/probe` |
| `GET /api/changes`, `/api/groups`, `/api/tags` | `POST /api/groups`, `DELETE /api/groups/{id}` |
| `GET /` (UI и статика) | `GET/PUT /api/settings`, `POST /api/sync` |
| `GET /api/mq`, `GET /api/mq/changes` | `/proxy/{id}/` |
| | `POST /api/mq/refresh` — шлёт запросы брокеру |
| | `PUT /api/mq/declared`, `DELETE /api/mq/declared/{id}` — только `source=manual` |

`/api/mq` анониму не отдаёт учётку соединения и текст ошибки брокера
(`internal/api/mq.go`, `redactSnapshot`); тест — `internal/api/mq_test.go`.

**Метрики Prometheus** — имена и метки (`internal/api/metrics.go:35-119`):
`spectra_up`, `spectra_services{state=ok|no_spec|unreachable|hidden|gone}`,
`spectra_services_total`, `spectra_services_manual_only`,
`spectra_services_without_owner`, `spectra_discovery_last_success_timestamp_seconds`,
`spectra_discovery_last_failure_timestamp_seconds`,
`spectra_oldest_successful_check_timestamp_seconds`, `spectra_spec_revisions_total`,
`spectra_changes_total`, `spectra_breaking_changes_total`, `spectra_poller_active`;
при заданных брокерах — `spectra_mq_last_success_timestamp_seconds{cluster}` и
`spectra_mq_problems{cluster,kind}` (каждый вид печатается, нули тоже).
На них написаны алерты в `deploy/DEPLOY.md:289-295` и `README.md:235`; тест —
`internal/api/api_test.go:691 TestMetricsExposeWhatYouWouldAlertOn`.

**Переменные окружения `SPECTRA_*`** — таблица в `deploy/k8s/README.md:82-96`,
описание каждой в `.env.example`. Их читают ConfigMap и Secret в
`deploy/k8s/01-spectra.yaml`. Тест умолчаний — `internal/config/config_test.go:36
TestDefaults`.

**Аннотации на чужих Ingress**: `spectra.io/skip`, `/spec-url`, `/name`,
`/group`, `/team`, `/contact`, `/repo`, `/owners` — определены в
`internal/k8s/ingress.go:89-96`, описаны в `README.md:323-333` и
`deploy/k8s/README.md:113-120`. Их проставляют чужие команды в своих манифестах —
переименование ломает то, что нам не принадлежит.

### 5.2. Инварианты поведения

- **Опрашивает ровно одна реплика.** Advisory-lock `store.LockPoller`
  (`internal/store/store.go:34-37`), берётся в
  `internal/collector/collector.go:106`; смысл — в комментарии к пакету,
  `collector.go:1-7`. Тест: `internal/store/store_test.go:376
  TestPollerLockIsExclusive`.
- **Свипы не накладываются.** Мьютекс `sweeping`, `collector.go:47`, с
  комментарием «letting two overlap would double the requests» (`:44-46`). Тест:
  `internal/collector/collector_test.go:676 TestSweepsDoNotOverlap`.
- **Ревизия пишется только при смене хэша нормализованной спеки**, а не по факту
  опроса (`README.md:142-145`). Тесты: `collector_test.go:160
  TestRevisionOnlyWhenTheContractMoves`, `internal/openapi/diff_test.go:46
  TestHashIgnoresServerAddress`, `diff_test.go:72 TestRegeneratedSpecIsNotAChange`.
- **«Сервис ответил, но спеки нет» выключает автоопрос совсем**, а «недоступен» —
  нет (таблица `README.md:123-134`). Тесты: `collector_test.go:217
  TestNoSpecLeavesTheSchedule`, `collector_test.go:248
  TestUnreachableBacksOffButStays`, `collector_test.go:359
  TestNoSpecDoesNotProbeTheSecondAddress`.
- **Ручной адрес спеки ограничен двумя адресами самого сервиса.** Функция
  `ResolveManualSpecURL` — `internal/discovery/manual.go:42-86`, цена ослабления
  названа в её doc-комментарии `manual.go:38-41`. Плюс запрет литеральных
  адресов из зарезервированных диапазонов, включая метаданные облака, —
  `manual.go:10-20`. Тесты: `internal/discovery/manual_test.go:48
  TestForeignHostsAreStillRefused`, `internal/api/api_test.go:454
  TestManualSpecURLIsConfinedToTheService`.
- **Прокси ходит только по адресу из записи реестра, никогда из запроса.**
  `internal/api/proxy.go:12-18` (обоснование), `proxy.go:39` (реализация). Тесты:
  `api_test.go:578 TestProxyOnlyReachesRegisteredServices`, `api_test.go:614
  TestProxyOffByConfiguration`.
- **Прокси вырезает `Authorization` портала и `Cookie`**, но пропускает чужой
  токен пользователя. `proxy.go:50-59`, `internal/api/auth.go:66-79`. Тест:
  `api_test.go:664 TestProxyForwardsTheUsersOwnToken`.
- **Нет учётных данных — администрирование недоступно, а не «без проверки».**
  `auth.go:38-46` («Failing open here would hand write access to everyone on the
  network»), продублировано в `deploy/k8s/README.md:132-134`.
- **Пароль сравнивается за постоянное время** — `auth.go:81-85`
  (`subtle.ConstantTimeCompare` на строке 84).
- **Ключ подписи сессии выводится из логина и пароля**, поэтому смена пароля
  инвалидирует все сессии. Заявлено в `internal/api/session.go:26-29`,
  реализовано в `sessionKey()` — `session.go:37-43`. Тест: `api_test.go:299
  TestForgedAndExpiredSessionsAreRejected`.
- **Аноним не видит ровно пять полей:** `IngressName`, `SpecURL`,
  `SpecURLSource`, `ManualSpecURL`, `LastError` — их и только их вырезает
  `redactService` (`internal/api/redact.go:27-42`). Тесты: `api_test.go:154
  TestAnonymousSeesNoClusterInternals`, `api_test.go:622
  TestOverviewHidesOperationalDataFromAnonymous`.
- **Внутренние адреса и namespace аноним видит — это осознанно**
  (`redact.go:11-30`, `README.md:179-184`). Тест `api_test.go:180-191` прямо
  проверяет, что `backend`, `namespace`, `baseUrl` и `key` **не пустые** для
  анонима. Смотри также `api_test.go:205 TestOwnerIsPublic`, `api_test.go:738`
  / `:769` (`TestPublicAddressIsTheOneBuiltOnTheHost` /
  `TestPublicAddressNeverReturnsTheInClusterOne`). **Не «чините» это как
  утечку** — и не верьте абзацу `README.md:192-195`, он устарел, см. §8.
- **Миграции не удаляют данные.** `README.md:305-306` формулирует это как
  «только добавляющие». Точнее (проверено `grep`, выполнено): ни `DROP TABLE`,
  ни `DROP COLUMN` нет ни в одной из десяти миграций. `DROP INDEX IF EXISTS` и
  `UPDATE` встречаются — `internal/store/migrations/010_undo_merge.sql:19-26`.
  Прямое основание инварианта — комментарий там же, `010_undo_merge.sql:28-30`:
  колонка `superseded_by` оставлена неиспользованной намеренно, «её удаление
  сделало бы откат невозможным». Тесты: `store_test.go:10
  TestMigrationsRunTwiceWithoutHarm`, `store_test.go:668
  TestMigrationRestoresWhatTheMergeHid`.
- **Имя файла миграции обязано начинаться с числа** — иначе старт падает,
  `internal/store/store.go:133`. Миграции применяются под advisory-lock
  `lockMigrations` (`store.go:33`, взятие — `store.go:107`), нумерация
  непрерывная 001–010.
- **Автоматика (аннотации, спека) только заполняет пустое и никогда не
  перезаписывает вписанное человеком.** `README.md:347-348`,
  `deploy/k8s/README.md:122-123`. Тесты: `store_test.go:25 TestSyncKeepsHumanEdits`,
  `store_test.go:108 TestAnnotationsFillGapsButNeverOverride`,
  `store_test.go:450 TestOwnerManualWinsPerField`, `store_test.go:584
  TestManualSpecURLSurvivesASweepOnASharedAddress`.
- **Ключ группировки Ingress — хост И префикс пути**, а дубли на одном хосте с
  одним путём **помечаются, а не схлопываются** (`README.md:89-100`). Схлопывание
  уже пробовали и откатили миграцией 010 — возвращать его нельзя без явного
  решения. Тесты: `internal/k8s/ingress_test.go:144
  TestSameHostDifferentPathsStayApart`, `ingress_test.go:491
  TestSamePrefixOnOneHostIsStillFlagged`, `store_test.go:527
  TestEntriesSharingAPublicAddressAreKept`.
- **Адрес в отдаваемой спеке всегда переписывается**: на прокси, если он включён,
  иначе на публичный адрес сервиса; пустым `servers` оставлять нельзя — вьюер
  подставит адрес самого портала. `internal/api/api.go:282-300`. Тесты:
  `api_test.go:379 TestSpecServersArePointedAtTheProxy`, `api_test.go:411
  TestProxyOffLeavesNoAddressInTheSpecification`.
- **`/vendor/` кэшируется на сутки, остальная статика — `no-cache`.**
  `internal/api/api.go:542-551`. Тест: `api_test.go:810
  TestStaticIsRevalidatedButVendorIsNot`.
- **`spectra_up == 0` обнуляет вывод остальных метрик, а не печатает нули.**
  `internal/api/metrics.go:42-47` («zeroes would look like real measurements»).

### 5.3. Похоже на баг, но сделано нарочно

- Прокси выключен по умолчанию (`SPECTRA_PROXY_ENABLED=false`,
  `.env.example:88-91`, `docker-compose.yml:36-38`), и кнопка отправки запроса не
  показывается — `README.md:53-55`.
- «Спека не найдена» — нормальное конечное состояние хоста без API, а не ошибка
  (`deploy/k8s/README.md:32`).
- Колонка `superseded_by` в БД не используется — см. миграцию 010 выше.
- `WWW-Authenticate` не отдаётся намеренно, чтобы браузер не показывал свой
  диалог поверх страницы — `internal/api/auth.go:53-56`.
- Признак «ломающее» не снимается с отката выкатки — `README.md:161-164`.
- Дифф ломающих изменений неполон намеренно и **не годится как гейт CI** —
  `README.md:172-175`.
- `internal/api/proxy.go:71` — `var _ = strings.TrimPrefix`, заглушка ради
  неиспользуемого импорта. Убирать — отдельная правка, не по пути.

### 5.4. Язык

Проект двуязычен, и это последовательно:

| Что | Язык | Пример |
|---|---|---|
| README, DEPLOY, BACKLOG, комментарии в Makefile/.env.example/SQL | русский | `README.md` целиком |
| Doc-комментарии к пакетам и функциям | английский | `redact.go:11-12`, `collector.go:1-7` |
| Комментарии, объясняющие продуктовое решение | часто русский | `redact.go:14-23`, `api.go:282-292` |
| Имена тестов | английский | `TestSyncKeepsHumanEdits` |
| Сообщения о провале теста (`t.Fatalf`/`t.Errorf`) | русский | `"аноним видит %s = %q"` (`api_test.go:173`) |
| Тексты ошибок для человека в UI | русский | `"нужен вход администратора"` (`auth.go:57`) |
| Технические ошибки про переменные окружения | английский | `"administration is disabled: set SPECTRA_ADMIN_USER…"` (`auth.go:43`) |
| Логи | преимущественно английский | `"poller election"` (`collector.go:109`) |

Правило: **держитесь языка соседних строк, не «исправляйте» существующий.**

---

## 6. Где живут архитектурные решения

**Отдельного каталога ADR нет.** В `docs/` лежит только `BACKLOG.md`; файлов
вида `docs/adr/NNN-*.md` не существует (проверено `find . -ipath '*adr*'` и
`git ls-files`, выполнено). Не заводите его, не спросив.

Решения записаны в четырёх местах:

1. **`README.md:353-382`, раздел «Почему так сделано»** — восемь решений формата
   «жирный тезис — почему — чем платим». Это ближайшее к ADR, что в проекте
   есть. Пример формы (`README.md:377-378`): «**Свой дифф вместо oasdiff.**
   Внешний бинарник в рантайме ради «привлечь внимание» — лишняя зависимость.
   Платим неполнотой покрытия.»
2. **`docs/BACKLOG.md`** — решения уже принятые, но не реализованные, плюс раздел
   «Известные пробелы» (`BACKLOG.md:72`) с указанием, где именно правится каждый.
3. **Сообщения коммитов** — здесь их больше всего. Устойчиво по всем 38 коммитам:
   - заголовок по-русски, **результат утверждением, а не «добавить X»**:
     «Namespace третьей строкой под внутренним адресом, а не колонкой», «Дубли
     помечаются, а не схлопываются». Длина не ограничена 72 символами — самый
     длинный заголовок 152 символа (выполнено, `git log --format=%s`);
   - тело содержательно: что было не так, почему выбрано именно это, что
     измерено, чем платим. 427 непустых строк тел на 38 коммитов (выполнено).
     Исключения: два коммита с пустым телом, `1c45e00` и `6383a33`, оба чисто
     документационные.
   - **Трейлер `Co-Authored-By` — не договорённость проекта.** Он есть ровно в
     19 старых коммитах и отсутствует во всех 18 последних, начиная с `acbdbb7`
     (выполнено, посчитано по `git log`). Не добавляйте его по инерции.
4. **Комментарии в коде и в SQL-миграциях.**
   `internal/store/migrations/010_undo_merge.sql` — это полноценный ADR об отказе
   от схлопывания дублей (34 строки, из них 21 — обоснование). То же в шапках
   пакетов `collector` (`collector.go:1-7`), `web` (`web.go:1-6`) и в
   `redact.go:11-26`, `session.go:13-29`, `proxy.go:12-18`.

Ищите обоснование в этом порядке: комментарий рядом с кодом → `git log -S` /
`git log --follow` по файлу → README «Почему так сделано» → BACKLOG.

---

## 7. Правила изменений

- **Продуктовый код только по заданию.** Замеченные попутно недочёты (см. §5.3 и
  §8) — в handoff, не в диван.
- **Тест на исправленный дефект — практика проекта:** `README.md:299` — «Почти
  каждый из этих тестов написан под конкретный найденный дефект».
- **Тесты `store`, `collector`, `api` идут против настоящего Postgres и настоящих
  HTTP-серверов** (`README.md:294-297`, `internal/store/testsupport_test.go:13-15`
  — «A fake would test the fake»). Не подменяйте это моками ради скорости:
  половина логики `store` живёт в SQL, а половина поведения коллектора — это то,
  чего он *не* делает.
- **Правки интерфейса нужны в двух местах.** `internal/web/static/app.js` рисует
  одно и то же дважды: список адресов — `urlRow()` (`:347`) и `renderURLs()`
  (`:382`); страницу сервиса — `serviceHeader()` (`:509`) и `renderService()`
  (`:613`). Коммиты `8c62cd1`, `c727ea0`, `f44f287` — каждый чинил забытое второе
  место; тело `8c62cd1` разбирает это прямым текстом.

---

## 8. Что удивило за первые десять минут

**1. `make test` возвращает 0, даже если тесты упали.** В `Makefile:62` рецепт
начинается с `-@`, и дефис велит make игнорировать провал строки. Проверено на
изолированном Makefile вне репозитория, запущенном из корня (выполнено):

```
$ make -f /tmp/…/Mk_probe probe    # рецепт: -@go test ./internal/nosuchpkg/...
FAIL	./internal/nosuchpkg/... [setup failed]
FAIL
make: [probe] Error 1 (ignored)
хвост рецепта выполнился
MAKE_EXIT=0
```

Сделано это ради следующей строки (`Makefile:64`) — она гасит контейнер Postgres
и должна отработать при любом исходе. Последствие: **код возврата `make test` —
не вердикт. Читайте вывод.** Признак провала — строка `FAIL` в выводе `go test`.

**2. `make test-quick` печатает `ok` по всем пакетам и при этом молча пропускает
65 тест-кейсов из 159.** Механизм — `internal/store/testsupport_test.go:22-29`:
без `SPECTRA_TEST_DATABASE_URL` тесты вызывают `t.Skip`, а не падают. Замерено
(выполнено, `go test ./... -v`):

| Пакет | без Postgres | с Postgres |
|---|---|---|
| `internal/store` | PASS 0, SKIP 22 | PASS 22 |
| `internal/collector` | PASS 2, SKIP 22 | PASS 24 |
| `internal/api` | PASS 7, SKIP 21 | PASS 28 |
| `config`, `discovery`, `k8s`, `openapi` | PASS 85 | PASS 85 |
| **итого** | **PASS 94, SKIP 65** | **PASS 159, SKIP 0** |

Пропускается весь `store`, почти весь `collector` и три четверти `api` — то есть
ровно тесты доступа, редакции, прокси и сессий. `make test-quick` годится как
быстрая петля, **но не как гейт**.

**3. `README.md:192-195` противоречит и коду, и самому README.** Абзац
утверждает, что анониму «не показываются и не отдаются в API: внутренние адреса,
backend-сервисы, namespace…». Это неправда с коммита `8c62cd1` («Внутренний
адрес открыт везде»): `redactService` (`redact.go:27-42`) их не трогает, а тест
`api_test.go:184-191` требует, чтобы они были непустыми. Соседний абзац того же
README (`:179-184`) описывает актуальное поведение. **Авторитет — код и тест**,
не этот абзац.

**4. `make demo-up` поверх уже существующего стенда сохраняет старую базу.** У
postgres в `demo/docker-compose.yml` нет секции `volumes` (строки 9-19), поэтому
используется анонимный том. Проверено (выполнено): после
`docker compose … up -d --force-recreate` поле `firstSeenAt` осталось прежним —
том перенесён в новый контейнер. После `docker compose … down` + `make demo-up`
база чистая: том осиротел, создался новый. Чистый старт гарантирует
`make demo-down` (он с `-v`, `Makefile:28`) — он же единственный, кто убирает том
за собой.

**5. Два комментария в репозитории устарели и уводят в сторону.** Продуктовый код
онбордер не трогает, поэтому оба оставлены как есть — кандидаты на отдельную
правку:
- `.gitignore:32` — «Ваш реестр сервисов обычно коммитят (см. **ADR №4** в
  README): он живёт в git». Реестр давно переехал в Postgres
  (`README.md:355-358`), файла `services.yaml` в проекте нет, пронумерованных
  ADR в README тоже нет. **Не ищите «ADR №4» — его не существует.**
- `Dockerfile:14-15` — «CGO не нужен: SQLite здесь чистый Go
  (modernc.org/sqlite)». SQLite в проекте нет: `go.mod` знает только `pgx` и
  `yaml.v3`, состояние в Postgres (`Dockerfile:25-26` двумя строками ниже это и
  говорит).

---

## 9. Не установлено

Честные дыры. Не выдумывайте здесь ничего — проверьте сами и допишите.

- **Kubernetes-путь не проверен.** `kubectl` установлен, контекст `orbstack`, но
  кластер не поднят: `kubectl cluster-info` → `dial tcp 127.0.0.1:26443:
  connect: connection refused` (выполнено). Поэтому `kubectl apply -f
  deploy/k8s/`, «10 записей из 15 Ingress», права ClusterRole, поведение
  запасного адреса, бэкап и восстановление (`deploy/DEPLOY.md:312-327`) —
  переписаны из документации и **не выполнялись**. `DEPLOY.md:329-331`
  утверждает, что процедура восстановления проверена на живом кластере; это
  чужое утверждение, не проверенное здесь.
- **Проценты покрытия из `README.md:284-292`** (config 93 %, openapi 80 %,
  k8s 76 %, api 74 %, store 74 %, discovery 58 %, collector 57 %) не перемерялись.
  Цели `go test -cover` в Makefile нет.
- **`make fmt` и `make vendor-viewer` не выполнялись.** Первое переписывает файлы
  (эквивалентная проверка `gofmt -l .` пуста), второе тянет 2.7 МБ с CDN и меняет
  вендоренный файл.
- **Поведение `make test` при занятом порте 55432 не проверено.** Известно из
  `Makefile:53`: перед стартом делается `docker rm -f spectra-test-pg` — чужой
  контейнер с этим именем будет удалён без предупреждения.
- **Нагрузочные характеристики.** `README.md:394` сам признаёт: гонялось на 11
  сервисах, не на 200.
- **Владелец проекта и внешнее ревью.** В репозитории один автор, `CODEOWNERS` и
  `CONTRIBUTING` отсутствуют (проверено `find`, выполнено).
- **Не проверялось, что делает портал под нагрузкой на сам `/proxy/`** и
  насколько корректен дифф на спеках с внешними `$ref` — тестов на это в
  репозитории не нашлось.
