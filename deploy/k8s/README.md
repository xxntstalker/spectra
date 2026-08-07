# Развёртывание в Kubernetes

## Быстро попробовать (OrbStack, kind, minikube)

```bash
docker build -t spectra:dev .
docker build -f demo/Dockerfile -t spectra-demo:dev .
kubectl apply -f deploy/k8s/
kubectl -n spectra rollout status deploy/spectra
kubectl -n spectra port-forward svc/spectra 8080:80
```

Портал — на <http://localhost:8080>, логин `admin` / `admin`.

В OrbStack сервисы кластера доступны с хоста напрямую, без port-forward:
<http://spectra.spectra.svc.cluster.local>

Развернётся: Postgres, портал с правами на чтение Ingress, семь фейковых
сервисов в namespace `demo-apis` и шесть граничных случаев.

**Ingress-контроллер не нужен.** Портал только *читает* объекты Ingress, а
запросы шлёт в backend-сервисы (`svc.namespace:port`). Демо работает в кластере
вообще без контроллера — это и проверяется.

## Что должно получиться

Из 14 объектов Ingress в реестр попадают 9:

| Ingress | Итог |
|---|---|
| 7 демо-сервисов | спека собрана |
| `plain-website` (nginx) | «спека не найдена» — нормальное состояние |
| `dead-backend` | «недоступен» — отличается от «спеки нет» |
| `cm-acme-http-solver-*` | пропущен (челлендж cert-manager) |
| `opted-out` | пропущен (`spectra.io/skip: "true"`) |
| `wildcard` (`*.demo.local`) | пропущен (по нему нельзя сделать запрос) |
| `system-dashboard` в `kube-system` | пропущен (исключённый namespace) |
| `orders-annotated` | схлопнут с `orders`: тот же backend = один API |

## Права

Единственное, что нужно порталу, — прочитать список Ingress. Это ровно то же,
что делает `kubectl get ing -A`: один и тот же вызов API.

```yaml
kind: ClusterRole
rules:
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses"]
    verbs: ["get", "list"]
```

Если доступ только к своим namespace — используйте `Role` + `RoleBinding` в
каждом из них и перечислите их в `SPECTRA_K8S_NAMESPACES`.

## Учётные данные Kubernetes

**Их не нужно задавать.** Kubernetes сам кладёт в под всё необходимое:

| Что | Откуда | Кто создаёт |
|---|---|---|
| Токен | `/var/run/secrets/kubernetes.io/serviceaccount/token` | kubelet |
| CA | `/var/run/secrets/kubernetes.io/serviceaccount/ca.crt` | kubelet |
| Адрес API | `KUBERNETES_SERVICE_HOST` / `KUBERNETES_SERVICE_PORT` | kubelet |

В манифесте достаточно `serviceAccountName`. Дискавери включается само, когда
портал видит, что работает внутри кластера.

Переопределения нужны только для запуска снаружи (например, с ноутбука против
дев-кластера): `SPECTRA_K8S_API_SERVER`, `SPECTRA_K8S_TOKEN`, `SPECTRA_K8S_CA_CERT`.

## Переменные окружения

| Переменная | По умолчанию | |
|---|---|---|
| `SPECTRA_DATABASE_URL` | — | **обязательна**, `postgres://…` |
| `SPECTRA_ADMIN_USER` / `SPECTRA_ADMIN_PASSWORD` | — | без них настройка выключена целиком |
| `SPECTRA_LISTEN` | `:8080` | |
| `SPECTRA_LOG_LEVEL` | `info` | |
| `SPECTRA_K8S_ENABLED` | автоопределение | |
| `SPECTRA_K8S_TARGET` | `service` | `service` или `ingress` |
| `SPECTRA_K8S_NAMESPACES` | все | список через запятую |
| `SPECTRA_K8S_EXCLUDE_NAMESPACES` | `kube-system,kube-public,kube-node-lease,cert-manager` | |
| `SPECTRA_K8S_INGRESS_CLASSES` | все | |
| `SPECTRA_K8S_SYNC_INTERVAL` | `1h` | |
| `SPECTRA_PROXY_ENABLED` | `false` | Try it out через портал |
| `SPECTRA_STATIC_SERVICES` | — | адреса вне кластера, через запятую |
| `SPECTRA_POLLER_ENABLED` | `true` | выключить, чтобы реплика только отдавала UI |

Всё остальное — интервал опроса, таймауты, ретенция, `ignoreFields` — лежит в
базе и меняется на странице настроек, потому что страница настроек и
неизменяемое окружение не могут владеть одним и тем же значением.

## Масштабирование

Реплик может быть сколько угодно: опрашивает всегда одна, её выбирает
advisory-lock в Postgres. Без этого три реплики означали бы тройную нагрузку на
каждый опрашиваемый сервис.

## Аннотации на Ingress

Владелец сервиса управляет тем, как его сервис выглядит в портале, не трогая
конфигурацию портала:

```yaml
metadata:
  annotations:
    spectra.io/skip: "true"           # не индексировать
    spectra.io/name: "Orders API"     # человеческое имя
    spectra.io/spec-url: /v3/api-docs # если автопоиск промахивается
    spectra.io/group: "Профиль"       # группа в списке
```

Аннотация только заполняет пустоту: если группу или адрес спеки уже задали
руками в интерфейсе, аннотация их не перезапишет.

## Доступ

Чтение — список хостов, спеки, лента изменений — открыто всем, кто дотянулся до
портала. Всё, что меняет состояние (настройка сервиса, ручной адрес спеки,
запуск опроса, страница настроек, прокси), — под логином из
`SPECTRA_ADMIN_USER` / `SPECTRA_ADMIN_PASSWORD`.

Логин передаётся Basic-авторизацией, поэтому портал должен стоять за TLS.
Если `SPECTRA_ADMIN_*` не заданы, администрирование выключено целиком — не
«без проверки», а недоступно.
