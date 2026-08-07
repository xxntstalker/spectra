# Развёртывание Spectra — инструкция для DevOps

Портал читает список Ingress в кластере, ходит по найденным сервисам за их
OpenAPI-спецификациями и показывает всё это одной страницей.

**Что нужно от вас:** Postgres, ServiceAccount с правом читать Ingress, один
Deployment. Всё.

---

## 1. Что портал делает с кластером

| | |
|---|---|
| Читает | объекты `Ingress` во всех namespace (`list`) |
| Не читает | секреты, конфигмапы, поды, ничего больше |
| Пишет в кластер | ничего |
| Ходит по сети | HTTP GET к backend-сервисам из Ingress, раз в час |

Это ровно то же, что делает `kubectl get ing -A` — тот же вызов API-сервера.
Ingress-контроллер не нужен: портал только читает объекты, а запросы шлёт в
`svc.namespace:port` напрямую.

**Нагрузка на ваши сервисы.** Раз в час один GET на сервис (сервисы отвечают
304 на неизменившуюся спеку). При первом знакомстве с незнакомым сервисом — до
45 запросов на перебор адресов, один раз; найденный адрес запоминается. Сервис,
у которого спеки нет, после первой неудачи выпадает из автоматического опроса
совсем.

---

## 2. Что нужно подготовить

### Postgres

Версия 13+. Одна база, один пользователь с правами на неё:

```sql
CREATE USER spectra WITH PASSWORD '...';
CREATE DATABASE spectra OWNER spectra;
```

Схему портал создаст сам при старте (миграции под advisory-lock, одновременный
запуск нескольких реплик безопасен).

Объём: ~1 КБ на сервис плюс размер спеки на каждую её ревизию. Сотня сервисов с
историей в 100 ревизий — единицы гигабайт. Ретенция настраивается в интерфейсе.

### Секрет

```bash
kubectl -n spectra create secret generic spectra-secrets \
  --from-literal=database-url='postgres://spectra:ПАРОЛЬ@postgres.db.svc:5432/spectra?sslmode=require' \
  --from-literal=admin-user='admin' \
  --from-literal=admin-password='ДЛИННЫЙ_ПАРОЛЬ'
```

### Права

```yaml
apiVersion: v1
kind: ServiceAccount
metadata:
  name: spectra
  namespace: spectra
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: spectra-ingress-reader
rules:
  - apiGroups: ["networking.k8s.io"]
    resources: ["ingresses"]
    verbs: ["get", "list"]
---
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: spectra-ingress-reader
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: spectra-ingress-reader
subjects:
  - kind: ServiceAccount
    name: spectra
    namespace: spectra
```

Если ClusterRole не согласуют — сделайте `Role` + `RoleBinding` в нужных
namespace и перечислите их в `SPECTRA_K8S_NAMESPACES`.

**Учётные данные Kubernetes прописывать не нужно.** Токен и CA монтирует kubelet
в `/var/run/secrets/kubernetes.io/serviceaccount`, адрес API-сервера приходит в
`KUBERNETES_SERVICE_HOST`. Достаточно `serviceAccountName` в поде.

---

## 3. Deployment

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: spectra
  namespace: spectra
spec:
  replicas: 1
  strategy: { type: Recreate }
  selector: { matchLabels: { app: spectra } }
  template:
    metadata: { labels: { app: spectra } }
    spec:
      serviceAccountName: spectra
      containers:
        - name: spectra
          image: РЕГИСТРИ/spectra:ВЕРСИЯ
          env:
            - name: SPECTRA_DATABASE_URL
              valueFrom: { secretKeyRef: { name: spectra-secrets, key: database-url } }
            - name: SPECTRA_ADMIN_USER
              valueFrom: { secretKeyRef: { name: spectra-secrets, key: admin-user } }
            - name: SPECTRA_ADMIN_PASSWORD
              valueFrom: { secretKeyRef: { name: spectra-secrets, key: admin-password } }
          ports: [{ containerPort: 8080 }]
          readinessProbe:
            httpGet: { path: /healthz, port: 8080 }
            initialDelaySeconds: 5
            periodSeconds: 5
          livenessProbe:
            httpGet: { path: /healthz, port: 8080 }
            initialDelaySeconds: 30
            periodSeconds: 20
          resources:
            requests: { cpu: 50m, memory: 96Mi }
            limits: { memory: 512Mi }
          securityContext:
            runAsNonRoot: true
            runAsUser: 65532
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            capabilities: { drop: ["ALL"] }
```

Тома не нужны — всё состояние в Postgres, файловая система только для чтения.

**Реплик может быть больше одной.** Опрашивать всё равно будет одна: её выбирает
advisory-lock в Postgres. Без этого три реплики означали бы тройную нагрузку на
каждый опрашиваемый сервис.

---

## 4. Переменные окружения

| Переменная | По умолчанию | |
|---|---|---|
| `SPECTRA_DATABASE_URL` | — | **обязательна** |
| `SPECTRA_ADMIN_USER` / `SPECTRA_ADMIN_PASSWORD` | — | без них настройка недоступна целиком |
| `SPECTRA_LISTEN` | `:8080` | |
| `SPECTRA_LOG_LEVEL` | `info` | `debug` добавит лог запросов |
| `SPECTRA_K8S_ENABLED` | автоопределение | |
| `SPECTRA_K8S_TARGET` | `service` | `service` или `ingress` |
| `SPECTRA_K8S_NAMESPACES` | все | через запятую |
| `SPECTRA_K8S_EXCLUDE_NAMESPACES` | `kube-system,kube-public,kube-node-lease,cert-manager` | |
| `SPECTRA_K8S_INGRESS_CLASSES` | все | |
| `SPECTRA_K8S_SYNC_INTERVAL` | `1h` | как часто перечитывать Ingress |
| `SPECTRA_PROXY_ENABLED` | `false` | «Try it out» через портал |
| `SPECTRA_STATIC_SERVICES` | — | адреса вне кластера, через запятую |
| `SPECTRA_POLLER_ENABLED` | `true` | `false` — реплика только отдаёт интерфейс |

Интервал опроса, таймауты, ретенция и игнорируемые поля лежат в базе и меняются
на странице настроек — их в окружении нет намеренно.

---

## 5. Доступ и безопасность

**Чтение открыто всем, кто дотянулся до портала.** Это осознанно: искать
документацию по API не должно требовать логина. Внутренние адреса, namespace,
имена backend-сервисов и тексты ошибок неавторизованным не показываются —
вырезаются на сервере.

**Всё, что меняет состояние, — под логином** из `SPECTRA_ADMIN_*`. Туда же
отнесён прокси: читать документацию можно всем, слать запросы в API через портал
— нет.

Логин передаётся Basic-авторизацией. **Ставьте портал за TLS.** Если
`SPECTRA_ADMIN_*` не заданы, администрирование выключено целиком.

Рекомендуется дополнительно закрыть портал вашим SSO (oauth2-proxy,
ingress-auth) — особенно если включаете `SPECTRA_PROXY_ENABLED`.

**NetworkPolicy.** Порталу нужен egress к API-серверу, к Postgres и к
опрашиваемым сервисам. Ingress — только от вашего ingress-контроллера.

---

## 6. Как понять, что всё работает

```bash
kubectl -n spectra logs deploy/spectra | grep -E 'listening|cluster sync'
```

Ожидаемое:

```
msg="kubernetes discovery enabled" apiServer=https://10.96.0.1:443 target=service
msg="spectra listening" addr=:8080
msg="cluster sync" found=42 new=42 gone=0
```

| Симптом | Причина |
|---|---|
| `HTTP 403` при списке Ingress | нет ClusterRole — текст ошибки назовёт нужное право |
| `waiting for postgres` в цикле | недоступна база; портал ретраит 30 раз по 2 секунды |
| `found=0` | все namespace отфильтрованы или Ingress нет |
| `kubernetes discovery is off` | нет токена или `KUBERNETES_SERVICE_HOST`; проверьте `serviceAccountName` |

Страница настроек показывает журнал сканирований: когда, сколько нашлось, какие
сервисы появились и какие исчезли. Это отличает «новых нет» от «сканирование
падает со вторника».

---

## 7. Обновление и откат

Обычный `kubectl set image` / `rollout undo`. Миграции только добавляют — откат
на предыдущую версию образа безопасен, схема остаётся совместимой.

Бэкап — обычный бэкап вашей Postgres. Отдельного состояния у портала нет.

---

## 8. Как командам управлять своим сервисом в портале

Аннотации на своём Ingress, без обращения к вам:

```yaml
metadata:
  annotations:
    spectra.io/skip: "true"           # не индексировать этот сервис
    spectra.io/name: "Orders API"     # человеческое имя вместо имени Service
    spectra.io/spec-url: /v3/api-docs # если автопоиск промахивается
```
