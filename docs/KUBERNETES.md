# Развёртывание в Kubernetes

Helm chart находится в `deploy/helm/jhvirt`. Он разворачивает приложение,
Service, опциональные Ingress/PVC и PodDisruptionBudget. PostgreSQL в chart не
входит: для production используйте управляемую БД или PostgreSQL Operator.

## Модель масштабирования

HTTP/API и обработчики очередей могут работать в нескольких pod одновременно.
Расписания, мониторинг и discovery выполняет только ведущий pod: место ведущего
удерживается PostgreSQL advisory lock и автоматически переходит к другому pod
при остановке. Миграции схемы также сериализованы advisory lock, поэтому
несколько pod могут стартовать одновременно после обновления.

Практический старт — `replicaCount: 1`. После проверки сети и хранилищ можно
поставить 2–3 реплики. Все реплики обязаны использовать:

- одну PostgreSQL;
- один и тот же `secret.key`;
- сетевые/object storage targets либо общий RWX-том для `/backups`;
- одинаковый внешний URL и конфигурацию.

Локальный RWO-том нельзя использовать несколькими репликами. Chart не даст
включить локальный backup PVC с несколькими pod, пока оператор явно не укажет
`backupStorage.shared: true`.

## 1. Образ

Соберите и отправьте образ в доступный кластеру registry:

```bash
docker build -t registry.example.org/infra/jhvirt:1.0.0 .
docker push registry.example.org/infra/jhvirt:1.0.0
```

Для закрытого registry создайте pull secret и добавьте его в
`imagePullSecrets` Helm values.

## 2. PostgreSQL и секреты

Создайте namespace и Secret с PostgreSQL DSN. Для внешней БД используйте
`sslmode=require` или `verify-full`:

```bash
kubectl create namespace jhvirt
kubectl -n jhvirt create secret generic jhvirt-database \
  --from-literal=url='postgres://jhvirt:PASSWORD@postgres.example.org:5432/jhvirt?sslmode=require'
```

Ключ приложения должен оставаться неизменным весь срок жизни установки:

```bash
umask 077
openssl rand -base64 32 > secret.key
printf '%s' 'CHANGE-THIS-ADMIN-PASSWORD' > bootstrap-password
kubectl -n jhvirt create secret generic jhvirt-secrets \
  --from-file=secret.key=./secret.key \
  --from-file=bootstrap-password=./bootstrap-password
rm -f secret.key bootstrap-password
```

Содержимое `secret.key` — base64-представление ровно 32 случайных байт. Это не
Kubernetes base64 из YAML, а фактическое содержимое файла, которое читает
приложение. Потеря ключа делает зашифрованные учётные данные и зашифрованные
копии нечитаемыми. Включите Secret в защищённый DR-процесс отдельно от etcd.

Приложение требует права `0600` у файлов с секретами. Secret volume Kubernetes
не обеспечивает нужный режим для non-root UID, поэтому non-root init-контейнер
копирует выбранные ключи в memory `emptyDir` и создаёт их от UID 10001 с правами
`0600`. Основной контейнер также работает без root, с read-only root filesystem
и без Linux capabilities.

## 3. Values и установка

Минимальный `values-production.yaml`:

```yaml
replicaCount: 2

image:
  repository: registry.example.org/infra/jhvirt
  tag: 1.0.0

externalURL: https://jhvirt.example.org
timezone: Asia/Yekaterinburg

database:
  existingSecret: jhvirt-database
  urlKey: url

appSecret:
  existingSecret: jhvirt-secrets
  keyKey: secret.key
  bootstrapPasswordKey: bootstrap-password

ingress:
  enabled: true
  className: nginx
  annotations:
    nginx.ingress.kubernetes.io/proxy-body-size: "0"
    nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
    nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
  hosts:
    - host: jhvirt.example.org
      paths:
        - path: /
          pathType: Prefix
  tls:
    - secretName: jhvirt-tls
      hosts: [jhvirt.example.org]
```

Установка и проверка:

```bash
helm upgrade --install jhvirt deploy/helm/jhvirt \
  --namespace jhvirt \
  --values values-production.yaml

kubectl -n jhvirt rollout status deployment/jhvirt
helm test jhvirt -n jhvirt
kubectl -n jhvirt get pods,service,ingress,pdb
```

После первого входа удалите bootstrap-пароль из Secret и values, затем
перезапустите Deployment. Пользователь уже хранится в PostgreSQL:

```bash
kubectl -n jhvirt create secret generic jhvirt-secrets-next \
  --from-file=secret.key=/secure/path/secret.key
helm upgrade jhvirt deploy/helm/jhvirt -n jhvirt \
  -f values-production.yaml \
  --set appSecret.existingSecret=jhvirt-secrets-next \
  --set appSecret.bootstrapPasswordKey=
```

Не удаляйте прежний Secret, пока новый rollout не стал Ready.

## 4. OIDC и дополнительные настройки

Любую настройку YAML можно передать в `config`. Helm рекурсивно объединяет её
со значениями chart. Пример внешнего OIDC:

```yaml
appSecret:
  existingSecret: jhvirt-secrets
  keyKey: secret.key
  oidcClientSecretKey: oidc-client-secret

config:
  auth:
    oidc:
      enabled: true
      issuer: https://sso.example.org/realms/infra
      client_id: jhvirt
      redirect_url: https://jhvirt.example.org/api/v1/auth/oidc/callback
      groups_claim: groups
      role_mapping:
        virt-admins: admin
        virt-operators: operator
        virt-readers: viewer
      allow_local_login: false
```

Ключ `oidc-client-secret` добавляется в `jhvirt-secrets`. Встроенный Docker
Keycloak и host-helper в Kubernetes не запускаются; используйте внешний IdP
или отдельный chart/operator Keycloak.

## 5. Хранилища и временное место

Для нескольких реплик предпочтительны S3, SFTP, SMB или WebDAV: они
настраиваются в интерфейсе и доступны всем pod без Kubernetes mount. Для
локального/NFS-хранилища включите PVC:

```yaml
backupStorage:
  enabled: true
  shared: true
  storageClass: nfs-rwx
  accessModes: [ReadWriteMany]
  size: 5Ti
```

Том монтируется в `/backups`; этот путь указывается у local storage target.
С существующим PVC задайте `existingClaim`.

`/work` — `emptyDir` для проверки, восстановления образов и совместимой
QCOW2-цепочки oVirt 4.3. Последняя одновременно собирает локальную основу и
текущий raw-образ самого большого диска. Полная цепочка диска может занимать сотни гигабайт, поэтому обеспечьте подходящий ephemeral
storage узла либо подключите отдельный том через модификацию values/chart.
Лимит можно задать через `workVolume.sizeLimit`, а requests/limits для
`ephemeral-storage` — через `resources`.

## 6. Эксплуатационные ограничения

- PostgreSQL должна выдерживать сумму `database.postgres.max_conns` всех pod
  плюс отдельное соединение ведущего и административный запас.
- Ingress должен разрешать длительные скачивания и восстановления; значения
  аннотаций зависят от используемого ingress controller.
- Pod не получает Kubernetes API token и не требует RBAC.
- При смене Secret выполните `kubectl rollout restart deployment/jhvirt`:
  Kubernetes обновляет файл Secret, но процесс читает ключи только при старте.
- Для DR сохраняйте PostgreSQL, `secret.key`, OIDC/metrics secrets и внешние
  backup repositories. Ephemeral `/app/data`, `/tmp` и `/work` в DR не входят.
- Горизонтальное масштабирование увеличивает число API/queue workers, но
  планировщик остаётся один — это защита от двойного запуска заданий.

