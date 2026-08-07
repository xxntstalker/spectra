# Развёртывание в Kubernetes

## Быстро попробовать (OrbStack, kind, minikube)

```bash
docker build -t api-portal:dev .
docker build -f demo/Dockerfile -t api-portal-demo:dev .
kubectl apply -f deploy/k8s/
kubectl -n api-portal rollout status deploy/api-portal
kubectl -n api-portal port-forward svc/api-portal 8080:80
```

Портал — на <http://localhost:8080>, логин `admin` / `admin`.

В OrbStack сервисы кластера доступны с хоста напрямую, без port-forward:
<http://api-portal.api-portal.svc.cluster.local>

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
| `opted-out` | пропущен (`swagger-portal.io/skip: "true"`) |
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
каждом из них и перечислите их в `PORTAL_K8S_NAMESPACES`.

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
дев-кластера): `PORTAL_K8S_API_SERVER`, `PORTAL_K8S_TOKEN`, `PORTAL_K8S_CA_CERT`.

## Переменные окружения

| Переменная | По умолчанию | |
|---|---|---|
| `PORTAL_DATABASE_URL` | — | **обязательна**, `postgres://…` |
| `PORTAL_ADMIN_USER` / `PORTAL_ADMIN_PASSWORD` | — | без них настройка выключена целиком |
| `PORTAL_LISTEN` | `:8080` | |
| `PORTAL_LOG_LEVEL` | `info` | |
| `PORTAL_K8S_ENABLED` | автоопределение | |
| `PORTAL_K8S_TARGET` | `service` | `service` или `ingress` |
| `PORTAL_K8S_NAMESPACES` | все | список через запятую |
| `PORTAL_K8S_EXCLUDE_NAMESPACES` | `kube-system,kube-public,kube-node-lease,cert-manager` | |
| `PORTAL_K8S_INGRESS_CLASSES` | все | |
| `PORTAL_K8S_SYNC_INTERVAL` | `1h` | |
| `PORTAL_PROXY_ENABLED` | `false` | Try it out через портал |
| `PORTAL_STATIC_SERVICES` | — | адреса вне кластера, через запятую |
| `PORTAL_POLLER_ENABLED` | `true` | выключить, чтобы реплика только отдавала UI |

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
    swagger-portal.io/skip: "true"           # не индексировать
    swagger-portal.io/name: "Orders API"     # человеческое имя
    swagger-portal.io/spec-url: /v3/api-docs # если автопоиск промахивается
```

## Доступ

Чтение — список хостов, спеки, лента изменений — открыто всем, кто дотянулся до
портала. Всё, что меняет состояние (настройка сервиса, ручной адрес спеки,
запуск опроса, страница настроек, прокси), — под логином из
`PORTAL_ADMIN_USER` / `PORTAL_ADMIN_PASSWORD`.

Логин передаётся Basic-авторизацией, поэтому портал должен стоять за TLS.
Если `PORTAL_ADMIN_*` не заданы, администрирование выключено целиком — не
«без проверки», а недоступно.
