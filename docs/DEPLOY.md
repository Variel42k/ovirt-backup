# Развёртывание

Поддерживаются два production-варианта:

1. Docker Compose v2 или `docker-compose` v1: приложение и PostgreSQL работают
   в контейнерах.
2. Нативная служба systemd: приложение работает как `jhvirt.service`,
   PostgreSQL может быть локальной или внешней.

Podman не поддерживается. Установщик отличает настоящий Docker от
`podman-docker` и для `--mode podman` выводит явную ошибку.

Установщик создаёт конфигурацию, проверяет её, запускает приложение и ждёт
`/readyz`. Для Docker и новой локальной БД он печатает пароль первого
администратора; особенность новой внешней БД описана в разделе 6. TLS и
обратный прокси установщик не настраивает.

При ошибке переходите к [TROUBLESHOOTING.md](TROUBLESHOOTING.md). Там команды
разделены для Docker и systemd.

Все пути YAML, `.env`, `jhvirt.env`, unit и ключа для каждого режима собраны
в отдельном справочнике [CONFIGURATION.md](CONFIGURATION.md).

## 1. Требования

### Сервер приложения

| Компонент | Требование |
|---|---|
| ОС для systemd | Ubuntu/Debian с `apt` или RHEL/AlmaLinux/Rocky Linux с `dnf`; systemd должен быть PID 1 |
| Контейнерный вариант | Docker Engine и Docker Compose v2 либо `docker-compose` 1.29.2 |
| Архитектура | `linux/amd64` или `linux/arm64`; архитектура `.run` должна совпадать с сервером |
| СУБД | PostgreSQL 13+; Compose использует PostgreSQL 17 |
| Ресурсы | жёсткого минимума нет; для приложения и небольшой базы разумная начальная конфигурация — 2 vCPU и 2 ГиБ RAM |
| Диск | место для БД, журналов и временных файлов плюс отдельное хранилище копий; объём копий рассчитывается по защищаемым ВМ и ретенции |
| Права | root для установки `.run` и systemd; для запуска Compose из репозитория достаточно доступа к Docker daemon |

Go и Node на production-сервере не нужны. Они требуются только там, где
собирается `.run`: Go 1.26+ и Node.js 20+.

### Сеть

| Откуда → куда | Порт | Назначение |
|---|---:|---|
| браузер → приложение/прокси | 8080 или 443 | веб-интерфейс и API |
| приложение → oVirt/RHV/РЕД Виртуализация | 443 | API движка |
| приложение → гипервизоры oVirt | 54322 | прямой `ovirt-imageio` |
| приложение → прокси движка | 54323 | `ovirt-imageio`, если включён `backup.transfer.prefer_proxy` |
| приложение → KVM/libvirt | 22 | libvirt поверх SSH и передача NBD/образов |
| приложение → внешняя PostgreSQL | 5432 | только при внешней БД |
| приложение → S3/SFTP/NFS | зависит от хранилища | запись и чтение копий |

Для проверки `boot` KVM-хост должен иметь QEMU, libvirt, `/dev/kvm`, прошивку
BIOS/UEFI нужного типа и достаточно места в scratch. SSH-пользователь должен
иметь доступ к libvirt и право записи в scratch. Внутри гостя нужен
`qemu-guest-agent`, иначе сервис увидит запуск ВМ, но не сможет подтвердить
загрузку ОС.

DNS-имена движка и гипервизоров должны разрешаться с host и, для Docker,
внутри контейнера. Корпоративные зоны `.local` требуют отдельной настройки;
см. [DNS.md](DNS.md).

## 2. Сначала выберите внешний URL

`--url` — это адрес, который вводит пользователь в браузере. Это не
обязательно адрес, на котором процесс слушает внутри сервера.

Прямой доступ по HTTP:

```text
--url http://10.20.30.40:8080 --port 8080
```

HTTPS через nginx, который передаёт запросы на локальный порт 8080:

```text
--url https://virt.example.org --port 8080
```

Схема принципиальна для авторизации:

- `http://` создаёт сессионную cookie без флага `Secure`;
- `https://` создаёт cookie с флагом `Secure`;
- если указать `https://`, а открыть интерфейс по HTTP, пароль будет принят,
  но браузер не вернёт cookie и снова покажет форму входа.

Не указывайте HTTPS заранее. Сначала должен существовать реальный TLS-вход
через прокси или собственный TLS приложения. Разбор этой ошибки приведён в
[разделе об авторизации](TROUBLESHOOTING.md#пароль-принят-но-снова-появляется-форма-входа).

В режиме без диалога `--url` обязателен. В интерактивном режиме установщик
предложит `http://<первый-адрес-сервера>:<порт>`.

### Параметры установщика

| Параметр | Назначение |
|---|---|
| `--mode docker` | Docker Compose v2 |
| `--mode docker-compose` | старый `docker-compose` v1 |
| `--mode systemd` | нативная служба systemd |
| `--url http[s]://host[:port]` | обязательный внешний URL в unattended-режиме |
| `--port 8080` | опубликованный Docker-порт и `JHV_SERVER_PORT` для systemd |
| `--database-url-file /root/jhvirt.dsn` | внешняя PostgreSQL только для systemd; файл `0600` |
| `--no-start` | подготовить файлы/БД, но не запускать приложение |
| `--uninstall` | выбрать цель удаления; без терминала снять оба варианта |
| `--uninstall=docker` | снять только Compose-контейнеры и сеть |
| `--uninstall=systemd` | снять только `jhvirt.service` |
| `--uninstall=all` | снять Docker Compose и systemd |
| `--remove-config` | вместе с `--uninstall` удалить YAML/env выбранной установки |
| `PREFIX=/srv/jhvirt` | установить bundle не в `/opt/jhvirt` |

`--mode podman` не является допустимым режимом и завершается подсказкой выбрать
Docker Compose или systemd.

## 3. Получите установочный комплект

### Вариант A: собрать `.run`

На машине сборки из корня репозитория:

```bash
./run build --target linux/amd64
# либо
./run build --target linux/arm64
```

Получится `dist/ovirt-backup-<версия>-linux-<архитектура>.run`. Проверка до переноса:

```bash
sh dist/ovirt-backup-*.run --check
sh dist/ovirt-backup-*.run --version
```

Передайте один файл на сервер:

```bash
scp dist/ovirt-backup-*.run user@virt-server:/tmp/
```

`.run` содержит оба бинаря, собранную SPA, конфигурацию, Compose-файл,
systemd unit и документацию. Подробнее о сборке: [BUILD.md](BUILD.md).

### Вариант B: Docker из репозитория

Для Docker можно устанавливать прямо из checkout:

```bash
sudo mkdir -p /opt/jhvirt
sudo chown "$USER" /opt/jhvirt
git clone <URL-репозитория> /opt/jhvirt/src
cd /opt/jhvirt/src/deploy
```

В этом варианте Go и Node на хосте тоже не нужны: их используют стадии сборки
Docker-образа.

## 4. Установка Docker Compose

### Шаг 1. Проверьте Docker

```bash
docker info
docker compose version
```

Для Compose v1 вторая команда:

```bash
docker-compose version
```

Если `docker --version` упоминает Podman, выберите systemd-вариант или
установите настоящий Docker.

### Шаг 2. Запустите установщик

Из каталога `deploy` репозитория:

```bash
./install.sh --mode docker \
  --url http://10.20.30.40:8080 \
  --port 8080
```

Для Compose v1:

```bash
./install.sh --mode docker-compose \
  --url http://10.20.30.40:8080 \
  --port 8080
```

Из `.run` команда такая же, но нужны root-права, потому что комплект
раскладывается в `/opt/jhvirt`:

```bash
sudo sh /tmp/ovirt-backup-*.run --mode docker \
  --url http://10.20.30.40:8080 \
  --port 8080
```

Без `--mode` откроется интерактивное меню. Оно показывает только доступные
способы и всегда содержит пункт удаления с отдельным подтверждением.
Перед сборкой образа установщик проверяет host-порт. Если порт занят, в
интерактивном режиме он предложит свободный; для unattended-запуска укажите
одинаковый новый порт в `--port` и в HTTP URL. За HTTPS-прокси внешний URL
может не содержать внутренний порт.

Установщик:

1. проверяет, что выбранный host-порт свободен;
2. создаёт `.env` с правами `0600`;
3. генерирует пароли PostgreSQL и первого администратора;
4. создаёт каталоги `backups` и `restores`;
5. собирает образ и запускает приложение с PostgreSQL;
6. ждёт `http://127.0.0.1:<порт>/readyz` до трёх минут;
7. удаляет bootstrap-пароль администратора из `.env`;
8. печатает адрес, логин `admin` и одноразово показанный пароль.

Запишите пароль сразу. При повторной установке существующий `.env`, пароль
БД, тома, ключ и учётные записи сохраняются.

### Шаг 3. Проверьте контейнеры

Из рабочего Compose-каталога:

```bash
docker compose ps
docker compose logs --tail 100 ovirt-backup
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
```

Для `.run` рабочий каталог по умолчанию — `/opt/jhvirt/compose`. Для установки
из репозитория — каталог `deploy`.

Ожидается: оба контейнера `healthy`, оба HTTP-запроса успешны.

### Шаг 4. Проверьте вход

Откройте ровно тот адрес, который передали через `--url`, и войдите как
`admin`. Затем выполните независимую проверку сессии из раздела 7.

## 5. Установка systemd с локальной PostgreSQL

Этот путь работает только из `.run` и требует root.

### Шаг 1. Запустите установщик

```bash
sudo sh /tmp/ovirt-backup-*-linux-amd64.run \
  --mode systemd \
  --url http://10.20.30.40:8080 \
  --port 8080
```

Для arm64 используйте соответствующий комплект. Другой префикс задаётся до
`sh`:

```bash
sudo PREFIX=/srv/jhvirt sh /tmp/ovirt-backup-*.run \
  --mode systemd --url http://10.20.30.40:8080 --port 8080
```

Установщик:

1. проверяет ОС, архитектуру, порт, URL и содержимое комплекта;
2. через `apt` или `dnf` устанавливает PostgreSQL и HTTP-клиент, если нужно;
3. на RHEL-подобных системах инициализирует кластер PostgreSQL;
4. запускает PostgreSQL;
5. идемпотентно создаёт локальные роль и базу `jhvirt`;
6. создаёт системного пользователя `jhvirt`;
7. устанавливает файлы в `/opt/jhvirt` или заданный `PREFIX`;
8. создаёт `/opt/jhvirt/config/jhvirt.env` с правами `0600`;
9. формирует `/etc/systemd/system/jhvirt.service` с фактическими путями;
10. выполняет `-check-config`, `daemon-reload` и `enable --now`;
11. ждёт `/readyz` и печатает пароль администратора;
12. после успешного старта удаляет bootstrap-пароль из env.

Локальное подключение к PostgreSQL использует Unix socket и peer-аутентификацию,
поэтому пароль БД в env отсутствует.

### Шаг 2. Проверьте службу

```bash
sudo systemctl status jhvirt --no-pager
sudo journalctl -u jhvirt -n 100 --no-pager
curl -fsS http://127.0.0.1:8080/healthz
curl -fsS http://127.0.0.1:8080/readyz
sudo systemctl is-enabled jhvirt
```

Ожидается: `active (running)`, `enabled`, оба HTTP-запроса успешны.

### Шаг 3. Проверьте сохранение после перезапуска

```bash
sudo systemctl restart jhvirt
curl -fsS http://127.0.0.1:8080/readyz
```

Перед production-приёмкой также перезагрузите сервер и повторите проверку.

## 6. Systemd с внешней PostgreSQL

Предварительно создайте роль и базу. Роль должна владеть базой и иметь право
создавать/изменять объекты схемы, потому что миграции выполняются при старте.

Создайте DSN-файл, не передавая пароль в истории команд:

```bash
sudo install -m 0600 /dev/null /root/jhvirt.dsn
sudo sh -c 'printf "%s\n" \
  "host=db.example.org port=5432 user=jhvirt password=SECRET dbname=jhvirt sslmode=require" \
  > /root/jhvirt.dsn'
```

Установите приложение:

```bash
sudo sh /tmp/ovirt-backup-*.run \
  --mode systemd \
  --url https://virt.example.org \
  --port 8080 \
  --database-url-file /root/jhvirt.dsn
```

Файл обязан быть обычным читаемым файлом с правами ровно `0600` и содержать
одну непустую DSN-строку. При этом локальная PostgreSQL не устанавливается.
После установки DSN переносится в защищённый `jhvirt.env`; исходный файл можно
убрать в выбранное вами хранилище секретов.

Если внешняя база новая и таблица пользователей пуста, пароль первого
администратора напечатает сама служба отдельным блоком в journal:

```bash
sudo journalctl -u jhvirt -n 100 --no-pager
```

Для существующей базы учётные записи не изменяются. Потерянный пароль
восстанавливается консольной командой из
[TROUBLESHOOTING.md](TROUBLESHOOTING.md#неверный-логин-или-пароль-401-unauthorized).

## 7. Проверка авторизации

Проверяйте не только ответ login, но и последующий запрос с той же cookie.
Именно это обнаруживает неправильный флаг `Secure`.

```bash
BASE=http://10.20.30.40:8080
read -rsp 'Пароль admin: ' JHV_PASSWORD; echo
curl -sS -D /tmp/jhvirt-login.headers -c /tmp/jhvirt.cookies \
  -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$JHV_PASSWORD\"}"
unset JHV_PASSWORD
curl -sS -b /tmp/jhvirt.cookies "$BASE/api/v1/auth/me"
grep -i '^set-cookie:' /tmp/jhvirt-login.headers
rm -f /tmp/jhvirt-login.headers /tmp/jhvirt.cookies
```

Результат:

- login возвращает пользователя и срок сессии;
- `/auth/me` возвращает того же пользователя, а не `401`;
- при `BASE=http://...` cookie `jhvirt_session` не имеет `Secure`;
- при `BASE=https://...` cookie имеет `Secure`.

После этого проверьте вход через браузерную форму и обновление страницы. При
возврате на форму входа смотрите
[диагностику авторизации](TROUBLESHOOTING.md#5-авторизация).

## 8. TLS через nginx

Рекомендуемый вариант: приложение слушает HTTP на 8080, nginx завершает TLS.

```nginx
server {
    listen 443 ssl;
    server_name virt.example.org;

    ssl_certificate     /etc/ssl/certs/virt.crt;
    ssl_certificate_key /etc/ssl/private/virt.key;

    location / {
        proxy_pass http://127.0.0.1:8080;
        proxy_set_header Host              $host;
        proxy_set_header X-Real-IP         $remote_addr;
        proxy_set_header X-Forwarded-For   $proxy_add_x_forwarded_for;
        proxy_set_header X-Forwarded-Proto $scheme;
        proxy_buffering off;
        proxy_read_timeout 24h;
        client_max_body_size 0;
    }
}
```

После появления рабочего HTTPS обновите внешний URL повторным запуском того
же установщика:

```bash
# Docker
./install.sh --mode docker --url https://virt.example.org --port 8080

# systemd
sudo sh /tmp/ovirt-backup-*.run --mode systemd \
  --url https://virt.example.org --port 8080
```

Для systemd можно включить TLS непосредственно в
`config/ovirt-backup.yaml`, но внешний URL всё равно должен начинаться с
`https://`:

```yaml
server:
  tls:
    enabled: true
    cert_file: "/opt/jhvirt/config/tls/server.crt"
    key_file: "/opt/jhvirt/config/tls/server.key"
```

## 9. Первая настройка в интерфейсе

1. В разделе **Серверы** добавьте oVirt или KVM/libvirt подключение.
2. Нажмите **Проверить подключение** до сохранения.
3. Для oVirt при загрузке CA сверьте отпечаток сертификата независимым путём.
4. В разделе **Хранилища** добавьте целевое хранилище и выполните проверку
   записи, чтения и удаления тестового объекта.
5. Откройте **Защита** и устраните ВМ без заданий или без включённых дисков.
6. Создайте первое задание с `verify_after: chain`.
7. Оставьте авто-восстановление инфраструктуры в `dry_run: true` минимум на
   период наблюдения. Это не относится к восстановлению образов из копий.
8. Создайте отдельных пользователей с ролями `operator` и `viewer`; не
   используйте общий `admin` для повседневной работы.

## 10. Каталоги восстановления для systemd

Каталог должен быть разрешён одновременно в двух местах.

В `/opt/jhvirt/config/ovirt-backup.yaml`:

```yaml
backup:
  restore_dirs: ["/srv/restores"]
```

В `/etc/systemd/system/jhvirt.service`:

```ini
ReadWritePaths=/opt/jhvirt/data /opt/jhvirt/logs /srv/restores
```

Затем:

```bash
sudo install -d -o jhvirt -g jhvirt -m 0750 /srv/restores
sudo systemctl daemon-reload
sudo systemctl restart jhvirt
```

`restore_dirs` ограничивает запрос оператора, а `ReadWritePaths` ограничивает
процесс средствами systemd. Одного списка недостаточно.

В Docker каждый дополнительный каталог также должен быть смонтирован в
контейнер и перечислен в `JHV_RESTORE_DIRS`.

## 11. Production-чек-лист

- [ ] `/healthz` и `/readyz` отвечают после перезапуска и перезагрузки.
- [ ] Вход через форму и `/auth/me` работают по фактическому URL.
- [ ] Для HTTPS cookie имеет `Secure`; для временного HTTP не имеет.
- [ ] `data/secret.key` скопирован отдельно от БД и хранилища ВМ.
- [ ] `/metrics` отвечает только с отдельным Bearer-токеном.
- [ ] Снята резервная копия PostgreSQL.
- [ ] Хранилище прошло встроенную проверку записи/чтения/удаления.
- [ ] Выполнен полный и инкрементальный бэкап выбрасываемой ВМ.
- [ ] Выполнены проверки `restore` и `boot`.
- [ ] Восстановленный образ реально загружен.
- [ ] Для systemd проверены `ReadWritePaths` и разрешённые каталоги.
- [ ] Настроены журналирование, ретенция и оповещения.
- [ ] Автоматические действия сначала наблюдались в `dry_run`.

Пока реальный цикл `backup → verify → restore → boot` не прошёл на вашем
гипервизоре, систему нельзя считать единственной подтверждённой копией данных.

### Реальный KVM acceptance-тест

Для низкоуровневой проверки transient-domain нужен выбрасываемый загрузочный
qcow2 с включённым `qemu-guest-agent`. Команда запускается из корня исходного
репозитория на машине с Go 1.26+:

```bash
JHV_TEST_KVM_HOST=kvm.example.org \
JHV_TEST_KVM_USER=jhvirt-test \
JHV_TEST_KVM_KEY_FILE=/root/.ssh/jhvirt-test \
JHV_TEST_KVM_IMAGE=/var/lib/libvirt/images/jhvirt-fixture.qcow2 \
go test ./internal/kvm -run '^TestRealKVMGuestAgentBoot$' -v
```

Тест клонирует fixture в scratch, добавляет второй временный диск, запускает
ВМ без сети, ждёт guest agent и удаляет transient-domain с копиями. Исходный
образ не подключается напрямую и не изменяется. Для UEFI задайте
`JHV_TEST_KVM_FIRMWARE=efi`, для Secure Boot дополнительно
`JHV_TEST_KVM_SECURE_BOOT=true`. Полный список переменных находится в
`internal/kvm/testboot_integration_test.go`.

## 12. Обновление

### Перед обновлением

1. Сохраните PostgreSQL.
2. Сохраните `secret.key`.
3. Зафиксируйте текущую версию и состояние службы.

Docker:

```bash
cd /opt/jhvirt/src/deploy
docker compose exec -T postgres pg_dump -U jhvirt jhvirt \
  > /srv/backups/jhvirt-db-$(date +%F-%H%M).sql
git pull
./install.sh --mode docker --url https://virt.example.org --port 8080
```

Systemd:

```bash
sudo -u postgres pg_dump jhvirt \
  | sudo tee /srv/backups/jhvirt-db-$(date +%F-%H%M).sql >/dev/null
sudo sh /tmp/ovirt-backup-<новая-версия>.run \
  --mode systemd --url https://virt.example.org --port 8080
```

При обновлении сохраняются env, текущая конфигурация, ключ, база и данные.
Новый образец конфигурации кладётся рядом как `ovirt-backup.yaml.new`.
Служба перезапускается только если до обновления была активна.

Установщик распознаёт бинарник и Compose-сервис предыдущего выпуска. При
обновлении конфигурация переносится в `ovirt-backup.yaml`, новый контейнер
получает имя сервиса `ovirt-backup`, а прежний контейнер удаляется как orphan.
Значения существующего `.env`, включая `COMPOSE_PROJECT_NAME`, сохраняются,
поэтому именованные тома и база остаются прежними.

После обновления повторите `/readyz`, вход, `/auth/me` и одну дешёвую проверку
цепочки.

## 13. Удаление

Интерактивно запустите установщик без аргументов и выберите **удалить**. Затем
выберите Docker Compose, systemd или оба варианта. Следующий выбор определяет,
сохранить или удалить YAML/env-файлы. Безопасное значение по умолчанию —
сохранить. Перед действием будет отдельное подтверждение с итоговым составом
удаления.

Для автоматизации:

```bash
sudo sh /tmp/ovirt-backup-*.run --uninstall=docker
sudo sh /tmp/ovirt-backup-*.run --uninstall=systemd
sudo sh /tmp/ovirt-backup-*.run --uninstall=all
sudo sh /tmp/ovirt-backup-*.run --uninstall=all --remove-config
```

Из репозитория:

```bash
cd deploy
sudo ./install.sh --uninstall=docker
sudo ./install.sh --uninstall=systemd
sudo ./install.sh --uninstall=all
sudo ./install.sh --uninstall=all --remove-config
```

`docker` останавливает и удаляет только Compose-контейнеры и сеть, `systemd`
останавливает службу и удаляет unit, `all` выполняет оба действия. Если оба
варианта используют один `PREFIX`, общие бинарники не удаляются при снятии
только одного варианта.

Без `--remove-config` во всех режимах намеренно сохраняются:

- PostgreSQL и её данные;
- `config/ovirt-backup.yaml` и `jhvirt.env`;
- `data/secret.key`;
- контейнерные тома;
- хранилище резервных копий и восстановленные образы.

Полное удаление этих данных выполняйте только после отдельной резервной копии.
Потеря `secret.key` делает сохранённые пароли и зашифрованные копии
нерасшифровываемыми.

`--remove-config` удаляет только конфигурацию выбранной установки:

- Docker: Compose `.env`;
- systemd: `jhvirt.env`;
- при снятии последнего способа запуска или `--uninstall=all`: также YAML и
  прежние `.new`/legacy-варианты из `$PREFIX/config`.

Если Docker и systemd используют общий `$PREFIX`, YAML сохраняется при снятии
только одного варианта, поскольку он нужен оставшемуся. При запуске установщика
из Git-репозитория versioned-файл `config/ovirt-backup.yaml` не удаляется как
часть исходного кода. PostgreSQL, `secret.key`, volumes, бэкапы и восстановленные
образы `--remove-config` не затрагивает. Отдельный `metrics.token` считается
частью конфигурации и при этом ключе удаляется, хотя сам Docker volume остаётся.

## 14. Что резервировать в самом сервисе

### Docker

- том `jhvirt-data` с `secret.key` и `metrics.token`;
- том `postgres-data` или логический `pg_dump`;
- рабочий `.env` с правами `0600`;
- изменённый `config/ovirt-backup.yaml`;
- внешние каталоги/NFS/S3 с копиями ВМ.

Получить ключ из контейнера:

```bash
docker compose cp ovirt-backup:/app/data/secret.key \
  /root/jhvirt-secret.key.backup
chmod 600 /root/jhvirt-secret.key.backup
```

### Systemd

- `/opt/jhvirt/data/secret.key`;
- `/opt/jhvirt/config/ovirt-backup.yaml`;
- `/opt/jhvirt/config/jhvirt.env`;
- `/opt/jhvirt/config/metrics.token`;
- логический дамп PostgreSQL;
- внешние хранилища копий.

Храните ключ отдельно от базы и отдельно от копий ВМ.

## 15. Prometheus после установки

Установщик включает endpoint и создаёт отдельный token-файл с правами `0600`.
Он сохраняется при обновлении и обычном удалении приложения; удаляется только
при явном `--remove-config`.

Systemd:

```bash
sudo stat -c '%U:%G %a %n' /opt/jhvirt/config/metrics.token
sudo -u jhvirt test -r /opt/jhvirt/config/metrics.token
curl -fsS -H "Authorization: Bearer $(sudo cat /opt/jhvirt/config/metrics.token)" \
  http://127.0.0.1:8080/metrics | head
```

Docker Compose:

```bash
cd /opt/jhvirt/compose   # либо каталог deploy репозитория
docker compose exec -T ovirt-backup sh -c \
  'test "$(stat -c %a /app/data/metrics.token)" = 600'
TOKEN="$(docker compose exec -T ovirt-backup cat /app/data/metrics.token | tr -d '\r\n')"
curl -fsS -H "Authorization: Bearer $TOKEN" http://127.0.0.1:8080/metrics | head
unset TOKEN
```

Для удалённого Prometheus используйте TLS reverse proxy, VPN или отдельную
управляющую сеть. Пример `prometheus.yml`, названия метрик и запросы Grafana
приведены в [OPERATIONS.md](OPERATIONS.md).

## 16. Аварийное восстановление без сервиса

Формат копий самодостаточен. После установки бинаря `jvbackup`:

```bash
jvbackup list -repo /srv/backups
jvbackup verify -repo /srv/backups -run <run-id>
jvbackup restore -repo /srv/backups -run <run-id> -out /mnt/restore
```

Для зашифрованных копий требуется исходный `secret.key`. Формат подробно
описан в [BACKUP-FORMAT.md](BACKUP-FORMAT.md), эксплуатация — в
[OPERATIONS.md](OPERATIONS.md).
