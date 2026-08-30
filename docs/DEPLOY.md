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
администратора; особенность новой внешней БД описана в разделе 6. Установщик
может включить собственный TLS с новым самоподписанным сертификатом или с
переданной парой PEM. TLS на reverse proxy по-прежнему настраивается отдельно.

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
собирается `.run`: Go 1.27+ и Node.js 20+.

### Сеть

| Откуда → куда | Порт | Назначение |
|---|---:|---|
| браузер → приложение/прокси | 8080 или 443 | веб-интерфейс и API |
| приложение → oVirt/RHV/РЕД Виртуализация | 443 | API движка |
| приложение → гипервизоры oVirt | 54322 | прямой `ovirt-imageio` |
| приложение → прокси движка | 54323 | `ovirt-imageio`, если включён `backup.transfer.prefer_proxy` |
| приложение → KVM/libvirt | 22 | libvirt поверх SSH и передача NBD/образов |
| приложение → внешняя PostgreSQL | 5432 | только при внешней БД |
| приложение → S3/SFTP/SMB/WebDAV/NFS | зависит от хранилища | запись и чтение копий |

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
| `--dr-backup-dir /mnt/dr/ovirt-backup` | каталог ежедневных dump БД и копии `secret.key` |
| `--no-start` | подготовить файлы/БД; первый запуск завершать повторным вызовом установщика без этого ключа, а не прямым `start` |
| `--migration-export /root/jhvirt-migration.tar.gz` | остановить приложение и создать пакет переноса `0600` |
| `--migration-to user@host:/каталог` | отправить готовый пакет на новый сервер сразу после создания |
| `--migrate-from /root/jhvirt-migration.tar.gz` | восстановить пакет на пустом новом сервере |
| `--keep-source-running` | после репетиционного экспорта снова запустить исходное приложение |
| `--tls self-signed` или `--self-signed` | выпустить сертификат с SAN из внешнего URL и включить собственный HTTPS |
| `--tls files` | подключить существующую PEM-пару из `--tls-cert-file` и `--tls-key-file` |
| `--tls none` | не включать либо выключить собственный TLS приложения |
| `--tls-days 825` | срок нового самоподписанного сертификата, `1..3650` дней |
| `--uninstall` | выбрать цель удаления; без терминала снять оба варианта |
| `--uninstall=docker` | снять только Compose-контейнеры и сеть |
| `--uninstall=systemd` | снять только `jhvirt.service` |
| `--uninstall=all` | снять Docker Compose и systemd |
| `--remove-config` | вместе с `--uninstall` удалить YAML/env выбранной установки |
| `PREFIX=/srv/jhvirt` | установить bundle не в `/opt/jhvirt` |
| `--oidc none` | вход только по паролю — умолчание и поведение прежних версий |
| `--oidc keycloak` | поднять Keycloak рядом, завести realm, клиента и группы |
| `--oidc external` | подключить существующего провайдера |
| `--keycloak-port 8081` | порт Keycloak наружу |
| `--keycloak-url https://host:8081` | адрес Keycloak, если он виден не по адресу службы |
| `--oidc-backchannel-url http://idp:8080` | внутренний origin внешнего OIDC-провайдера, если публичный адрес недоступен из контейнера |
| `--oidc-issuer`, `--oidc-client-id` | параметры существующего провайдера |
| `--oidc-client-secret-file /root/kc.secret` | секрет клиента файлом, а не аргументом |
| `--local-login enabled\|disabled` | локальная парольная форма рядом с OIDC; для новой OIDC-установки по умолчанию `disabled` |

### Внешний вход

В диалоговом режиме установщик спрашивает, как входить: только по паролю,
поднять Keycloak рядом или подключить существующего провайдера. Умолчание —
только по паролю, поэтому поведение прежних установок не меняется.

При `--oidc keycloak` установщик поднимает Keycloak тем же compose (профиль
`keycloak`), заводит ему базу в том же кластере PostgreSQL, создаёт realm
`jhvirt` с отображаемым именем `ovirt-backup`, клиента с секретом, три группы
допуска и mapper групп. Случайная bootstrap-запись используется только во
время этой операции. Затем установщик создаёт постоянного администратора
master realm `kc-bootstrap-admin`, проверяет его права, удаляет bootstrap-запись
и печатает пароль постоянного администратора один раз. Это не пользователь
приложения.

Публичный issuer и внутренний адрес различаются намеренно:

```dotenv
JHV_OIDC_ISSUER=https://server.example.org:8081/realms/jhvirt
JHV_OIDC_BACKCHANNEL_URL=http://keycloak:8080
```

Первый адрес получает браузер и он же записывается в `iss` токена. Второй
используется только приложением для discovery, token, JWKS и userinfo внутри
Compose. Установщик не завершает работу, пока `/auth/oidc/start` через само
приложение не вернёт рабочее перенаправление к провайдеру.

Дальше в Keycloak остаётся завести пользователей и включить их в группы.
Не попавший ни в одну не допускается в систему вовсе — это и есть умолчание
`default_role`. Там же, а не в службе, подключаются домены (User Federation)
и включается второй фактор (Required Actions → Configure OTP, FreeOTP и
совместимые).

Локальная запись `local-admin` не является пользователем Keycloak. Её вход
обходит политики и MFA провайдера, поэтому в новой OIDC-установке он выключен
и пароль не показывается. Интерактивный установщик задаёт отдельный вопрос;
для unattended-режима используйте `--local-login`. Прежняя установка без этой
переменной сохраняет включённый вход при обновлении, чтобы не потерять доступ.

Соответствие групп ролям установщик пишет в `ovirt-backup.yaml` рядом с
compose-файлом: переменной окружения словарь не задаётся. Правится оно без
пересборки — `docker compose up -d`.

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
3. создаёт отдельный host-only recovery token: для `.run` это
   `/opt/jhvirt/config/recovery.token` с владельцем `root:root` и правами
   `0600`, для checkout — `deploy/.recovery-token`; в контейнер передаётся
   только SHA-256 токена;
4. генерирует административный пароль PostgreSQL, отдельные пароли ролей
   приложения и Keycloak и хранит их только файлами `0600` в volumes;
5. создаёт каталоги `backups`, `restores` и аварийных dump;
6. собирает закреплённые образы и запускает контейнеры с read-only root,
   урезанными capabilities, лимитами ресурсов и внутренней сетью БД;
7. ждёт `http://127.0.0.1:<порт>/readyz` до трёх минут;
8. запускает проверенный `pg_dump` приложения и Keycloak и копирует
   `secret.key` в каталог DR;
9. удаляет bootstrap-секреты после успешной настройки, принудительно
   пересоздаёт контейнер приложения и ещё раз ждёт `/readyz`, чтобы открытый
   пароль исчез вместе с первым процессом;
10. без OIDC печатает адрес, логин `local-admin` и одноразовый пароль; с OIDC
   печатает данные администратора Keycloak, а локальный вход оставляет
   выключенным, если не задано обратное.

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

Ожидается: приложение и PostgreSQL работают, оба HTTP-запроса успешны;
`dr-backup` остаётся запущенным и ждёт следующего интервала. С профилем
Keycloak дополнительно работают `keycloak` и `keycloak-backup`.

Для установки из `.run` проверьте, что recovery token остаётся только на
хосте и не входит в mounts контейнера:

```bash
sudo stat -c '%U:%G %a %n' /opt/jhvirt/config/recovery.token
cd /opt/jhvirt/compose
docker inspect "$(docker compose ps -q ovirt-backup)" \
  --format '{{range .Mounts}}{{println .Source "->" .Destination}}{{end}}'
```

Ожидается `root:root 600`; в списке mounts нет `recovery.token`. Для checkout
токен находится в `deploy/.recovery-token`, также не монтируется, а его доступ
ограничивается правами `0600`.

### Шаг 4. Проверьте вход

Откройте ровно тот адрес, который передали через `--url`. Без OIDC войдите как
`local-admin`. С OIDC сначала войдите в консоль Keycloak как
`kc-bootstrap-admin`, создайте пользователя и заполните его email, имя и
фамилию. Включите пользователя в `virt-admins`, затем используйте кнопку
Keycloak в приложении. Если профиль неполный, Keycloak сначала откроет
`Update Account Information`; это обязательное действие провайдера, а не ошибка
callback приложения. Выполните независимую проверку сессии из раздела 7.

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
11. ждёт `/readyz` и печатает пароль `local-admin`;
12. создаёт host-only `/opt/jhvirt/config/recovery.token` (`root:root 0600`),
    передавая службе только его SHA-256;
13. после успешного старта удаляет файл bootstrap-пароля и перезапускает
    службу, чтобы пароль исчез вместе с первым процессом;
14. устанавливает `jhvirt-dr-backup.timer`, создаёт и проверяет первый dump
    локальной PostgreSQL и копию `secret.key`.

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

Проверьте, что аварийный токен не читается сервисным пользователем:

```bash
sudo stat -c '%U:%G %a %n' /opt/jhvirt/config/recovery.token
sudo -u jhvirt test ! -r /opt/jhvirt/config/recovery.token
```

Ожидается `root:root 600`. Для сброса доступа используется только host-side
команда `sudo /opt/jhvirt/bin/ovirt-backup-recover-admin`; прямой
`-reset-password` без токена отклоняется.

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
read -rsp 'Пароль local-admin: ' JHV_PASSWORD; echo
curl -sS -D /tmp/jhvirt-login.headers -c /tmp/jhvirt.cookies \
  -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"local-admin\",\"password\":\"$JHV_PASSWORD\"}"
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

## 8. TLS приложения и TLS через nginx

### Самоподписанный сертификат через установщик

Для новой установки:

```bash
sudo sh /tmp/ovirt-backup-*.run \
  --mode systemd \
  --url https://virt.example.org:8080 \
  --port 8080 \
  --tls self-signed
```

Та же команда работает с `--mode docker`. Имя или IP из `--url` попадает в
Subject Alternative Name; сертификат и RSA-ключ создаются через OpenSSL. Для
Docker закрытый ключ хранится в постоянном `jhvirt-data` с владельцем
контейнерного UID `10001` и правами `0600`. Для systemd файлы находятся в
`$PREFIX/config/tls`, принадлежат системному пользователю приложения, ключ
имеет права `0600`.

Существующую PEM-пару подключают так:

```bash
sudo sh /tmp/ovirt-backup-*.run \
  --mode systemd \
  --url https://virt.example.org:8080 \
  --tls files \
  --tls-cert-file /root/server.crt \
  --tls-key-file /root/server.key
```

Установщик проверяет срок сертификата, чтение закрытого ключа и совпадение их
публичных ключей. Команду можно выполнить и поверх текущей инсталляции: YAML,
env, база, пользователи и ключ шифрования бекапов сохраняются, меняется только
TLS и внешний URL. В интерактивном режиме те же варианты появляются после
ввода внешнего адреса. Для отключения собственного TLS без удаления файлов:

```bash
sudo sh /tmp/ovirt-backup-*.run \
  --mode systemd --url https://virt.example.org --tls none
```

Внешний URL может остаться `https://`, если TLS завершает reverse proxy.
Самоподписанный сертификат необходимо установить в доверенные на каждом
рабочем месте; иначе шифрование работает, но браузер продолжит предупреждать о
неизвестном центре сертификации. Получить публичную часть из Docker:

```bash
cd /opt/jhvirt/compose
docker compose cp ovirt-backup:/app/data/tls/server.crt ./jhvirt-server.crt
```

Проверка до добавления сертификата в доверенные:

```bash
curl -kfsS https://virt.example.org:8080/readyz
```

### TLS через nginx

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

Для systemd TLS можно по-прежнему включить вручную в
`config/ovirt-backup.yaml`, но установочный режим выше дополнительно проверяет
пару, выставляет права и сохраняет её при миграции. Внешний URL в обоих случаях
должен начинаться с `https://`:

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
5. Откройте **Покрытие бэкапами** и устраните ВМ без заданий или без включённых дисков.
6. Создайте первое задание с `verify_after: chain`.
7. Оставьте авто-восстановление инфраструктуры в `dry_run: true` минимум на
   период наблюдения. Это не относится к восстановлению образов из копий.
8. Создайте именные записи с ролями `operator` и `viewer`; не используйте одну
   общую административную запись для повседневной работы.

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
репозитория на машине с Go 1.27+:

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

## Перенос приложения на другой сервер

Перенос поддерживается между одинаковыми способами запуска: Docker→Docker или
systemd→systemd. Установщик переносит:

- YAML и защищённый env-файл со всеми системными настройками;
- PostgreSQL dump с пользователями, подключениями, заданиями, историей и
  runtime-переопределениями;
- `secret.key`, без которого нельзя прочитать сохранённые пароли и
  зашифрованные копии;
- metrics token;
- управляемый установщиком TLS-сертификат и закрытый ключ;
- базу Keycloak, если использовался Compose profile `keycloak`;
- исходные `PREFIX`, имя системного пользователя и `ReadWritePaths` systemd.

В пакет также входит файл SHA-256 для контроля повреждения при передаче.
Перед импортом установщик проверяет все entries архива, запрещает symlink и
special files, сверяет контрольные суммы и валидирует, что `secret.key`
действительно содержит 32-байтовый AES-ключ.

SHA-256 внутри того же архива подтверждает целостность при копировании, но не
подлинность источника. Пакет не зашифрован и не подписан: он содержит действующие
секреты. Храните его как парольный vault, передавайте по защищённому каналу и
проверяйте происхождение самого `.run` отдельно.

Сами каталоги с бекапами и восстановленными образами не помещаются в архив:
они могут занимать терабайты. В пакет попадают их настройки. NFS/SMB/local
mount points следует подключить на новом сервере до импорта.

### 1. Создайте пакет на старом сервере

```bash
sudo sh /tmp/ovirt-backup-*.run \
  --migration-export /root/jhvirt-migration.tar.gz
```

Если на узле одновременно есть Docker и systemd, добавьте `--mode`. При обычном
экспорте приложение останавливается перед dump и остаётся остановленным. Это
защищает от расхождения базы после снятия копии. PostgreSQL остаётся запущенной.
При ошибке экспорта установщик пытается вернуть службу в исходное состояние.

Если Docker bundle установлен не в стандартный каталог, укажите его исходный
путь и при экспорте: `sudo PREFIX=/srv/jhvirt sh ... --migration-export ...`.
Для systemd установщик читает фактические `WorkingDirectory` и `User` из unit;
для Docker также сохраняется владелец установленного bundle. Эти значения
можно явно переопределить через `PREFIX` и `USER_NAME`.

Для репетиции без переключения:

```bash
sudo sh /tmp/ovirt-backup-*.run \
  --migration-export /root/jhvirt-rehearsal.tar.gz \
  --keep-source-running
```

В этом случае служба останавливается только на время согласованного dump, затем
запускается снова. Такой пакет нельзя считать окончательной точкой переключения:
сразу после его создания старая база снова начинает изменяться.

Архив получает права `0600`. Внутри находятся пароль БД, DSN, ключ шифрования
и закрытый TLS-ключ — передавайте файл через SCP/SFTP либо другой защищённый
канал и удалите после приёмки.

#### Отправка сразу из установщика

Ключ `--migration-to user@host:/каталог` передаёт готовый пакет на новый сервер,
не заставляя нести его руками:

```bash
sudo sh /tmp/ovirt-backup-*.run \
  --migration-export /root/jhvirt-migration.tar.gz \
  --migration-to user@10.0.0.5:/home/user
```

В диалоговом режиме установщик спрашивает то же самое после вопроса о пути к
пакету; пустой ответ оставляет пакет на месте.

Ручная передача спотыкается предсказуемо: пакет лежит с правами `0600` под
`root`, обычный `scp` его не прочитает, а `scp` из-под `sudo` идёт уже от `root`
— с его ключами и его `known_hosts`, которых на свежем сервере обычно нет.

Отправка обставлена двумя условиями, и оба намеренные:

- **Ключ хоста должен быть известен заранее.** Автоматическое принятие нового
  ключа здесь означало бы отправку `secret.key` тому, кто окажется по этому
  адресу. Установщик отказывается и печатает команды `ssh-keyscan`, чтобы
  отпечаток сверил человек — один раз.
- **После передачи сверяются контрольные суммы.** Обрезанный пакет провалил бы
  импорт позже и куда непонятнее: уже на другом сервере, на разборе архива.

Локальная копия пакета остаётся: неудачная передача не должна означать потерю
единственной копии состояния. Уничтожьте её после успешного импорта.

### 2. Подготовьте новый сервер

Установите Docker Compose либо подготовьте поддерживаемую systemd-систему того
же типа. Не создавайте новую инсталляцию заранее: импорт намеренно отклоняется,
если в целевом `PREFIX`, unit или именованных Docker volumes уже есть данные.

Подключите внешние файловые системы по прежним путям. Для systemd каждый
внешний каталог из сохранённого `ReadWritePaths` должен существовать и быть
доступен на запись будущему пользователю службы; установщик проверяет это до
старта. UID/GID systemd-пользователя сохраняются в manifest: если номера на
новом узле свободны, пользователь создаётся с прежними значениями. При
конфликте используется локальный UID и установщик явно предупреждает — ACL/NFS
экспорт тогда нужно поправить вручную. Для Docker проверяется запись от UID `10001`, которым работает
контейнер, и исправляется владелец только самого корня каталога, без
рекурсивного изменения содержимого. Отсутствующий абсолютный внешний каталог
при импорте не создаётся молча: сначала подключите прежнее хранилище или
создайте целевой каталог осознанно.

### 3. Импортируйте пакет

```bash
scp /root/jhvirt-migration.tar.gz new-server:/root/
ssh new-server
sudo sh /tmp/ovirt-backup-<новая-версия>.run \
  --migrate-from /root/jhvirt-migration.tar.gz
```

Если `PREFIX` и `USER_NAME` не заданы, берутся значения старого узла. При
переносе в другой каталог или под другим системным пользователем задайте их
явно:

```bash
sudo PREFIX=/srv/jhvirt USER_NAME=jhvirt-backup \
  sh /tmp/ovirt-backup-*.run \
  --migrate-from /root/jhvirt-migration.tar.gz \
  --url https://new-virt.example.org:8080
```

Известные абсолютные пути внутри прежнего `PREFIX` переписываются на новый.
Внешние пути не переписываются: это точки хранения, которые нужно подключить
осознанно. Новый `--url` обновляет адрес приложения и OIDC callback в env; тот
же callback необходимо разрешить в самом внешнем OIDC-провайдере.

TLS-пара из пакета сохраняется по умолчанию. Если имя сервера меняется, сразу
выпустите новый сертификат для нового URL:

```bash
sudo PREFIX=/srv/jhvirt USER_NAME=jhvirt-backup \
  sh /tmp/ovirt-backup-*.run \
  --migrate-from /root/jhvirt-migration.tar.gz \
  --url https://new-virt.example.org:8080 \
  --tls self-signed
```

Вместо этого можно передать новую PEM-пару через `--tls files` или отключить
собственный TLS через `--tls none`. В интерактивном импорте установщик предлагает
сохранить сертификат из пакета, выпустить новый, выбрать файлы или отключить TLS.
Код приложения берётся из запускаемого `.run`; архив переносит состояние и
настройки, а не старый бинарник.

При сохранении сертификата установщик проверяет его SAN для итогового внешнего
адреса — и с флагами, и в интерактивном режиме. Если hostname/IP не подходит,
импорт прекращается с требованием выбрать новый self-signed сертификат,
указать PEM-пару либо выключить собственный TLS; «успешная» установка с
заведомо ошибочным сертификатом не создаётся.

Для внешней PostgreSQL dump не создаётся: пакет сохраняет DSN, а новый сервер
подключается к той же базе. Старый экземпляр всё равно должен оставаться
остановленным, иначе два scheduler-а начнут выполнять одни задания. Перед
cutover сделайте snapshot/`pg_dump` средствами самой внешней БД: миграционный
архив не является её резервной копией и не откатывает изменения схемы.

Если импорт оборвался после создания файлов или Docker volumes, повторите
точно ту же команду с тем же архивом. В `PREFIX` остаётся marker с SHA-256
пакета; установщик распознаёт его и идемпотентно продолжает `pg_restore` и
раскладку. Другой архив поверх незавершённого состояния отклоняется. Marker
удаляется только после успешного завершения установщика.

Все варианты доступны и без флагов: запустите `.run`, выберите
**«подготовить перенос»** на старом узле и **«перенести сюда»** на новом.

### 4. Приёмка и завершение

1. Проверьте `/readyz`, вход существующей учётной записью, список подключений,
   заданий и историю.
2. Выполните проверку записи каждого хранилища и дешёвую проверку цепочки.
3. Проверьте владельцев `secret.key`, TLS-ключа, env и внешних каталогов.
4. Только после приёмки удалите миграционный архив.
5. Старую установку не запускайте. Снимите её установщиком без `--purge`, пока
   новый узел не прошёл полный эксплуатационный цикл.

Тонкости, которые установщик намеренно не автоматизирует:

- переключение Docker↔systemd — сначала переносится тем же способом запуска;
- терабайтные локальные репозитории, file-backup roots и restore-каталоги не
  копируются, а заранее монтируются по ожидаемым путям;
- DNS, firewall/NAT, доверие к self-signed сертификату и callback у внешнего OIDC
  провайдера меняются во внешних системах;
- при общей внешней PostgreSQL нельзя одновременно запускать старый и новый
  экземпляры, даже если leader election ранее был выключен.

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

Штатная установка уже создаёт ежедневный аварийный комплект. Он не заменяет
внешнюю политику резервирования: путь по умолчанию находится на том же сервере,
поэтому переживёт удаление container volume, но не потерю диска или узла.
Указывайте `--dr-backup-dir` на отдельный mount либо забирайте каталог с другого
сервера по pull-схеме.

### Docker

- `<JHV_DR_BACKUP_DIR>/app/postgres/postgres-*.dump` — проверенный custom-format
  dump БД приложения;
- `<JHV_DR_BACKUP_DIR>/app/secret.key` — побайтовая копия ключа;
- с Keycloak: `<JHV_DR_BACKUP_DIR>/keycloak/postgres/postgres-*.dump`;
- рабочий `.env` с правами `0600`;
- изменённый `config/ovirt-backup.yaml`;
- внешние каталоги/NFS/S3 с копиями ВМ.

Создание dump выполняют `dr-backup` и `keycloak-backup`: финальный файл
появляется только после успешного `pg_dump` и `pg_restore --list`, имеет права
`0600`, хранение по умолчанию семь дней. Проверьте:

```bash
docker compose logs --tail 50 dr-backup keycloak-backup
find "$(sed -n 's/^JHV_DR_BACKUP_DIR=//p' .env)" -maxdepth 4 -type f -ls
```

Получить ключ из контейнера:

```bash
docker compose cp ovirt-backup:/app/data/secret.key \
  /root/jhvirt-secret.key.backup
chmod 600 /root/jhvirt-secret.key.backup
```

### Systemd

- `/var/backups/ovirt-backup/postgres/postgres-*.dump` и
  `/var/backups/ovirt-backup/secret.key` либо путь из `--dr-backup-dir`;
- `/opt/jhvirt/config/ovirt-backup.yaml`;
- `/opt/jhvirt/config/jhvirt.env`;
- `/opt/jhvirt/config/metrics.token`;
- внешние хранилища копий.

Проверка timer и последнего запуска:

```bash
sudo systemctl status jhvirt-dr-backup.timer --no-pager
sudo journalctl -u jhvirt-dr-backup.service -n 50 --no-pager
sudo -u jhvirt pg_restore --list \
  "$(find /var/backups/ovirt-backup/postgres -type f -name 'postgres-*.dump' | sort | tail -1)" >/dev/null
```

Для внешней PostgreSQL timer не включается: используйте резервирование СУБД,
PITR/WAL-архив и регулярную проверку восстановления. Храните dump и ключ
отдельно от основной БД и отдельно от копий ВМ.

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
