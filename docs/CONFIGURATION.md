# Конфигурация и данные

Этот документ отвечает на три практических вопроса:

1. где находится конфигурация для каждого способа установки;
2. какой файл нужно менять;
3. что сохраняется при обновлении и удалении.

Системная настройка разрешения имён движков и гипервизоров вынесена в
[DNS.md](DNS.md).

## 1. Краткая таблица путей

При стандартном `PREFIX=/opt/jhvirt`:

Название продукта и новые артефакты — `ovirt-backup`. Идентификаторы
`/opt/jhvirt`, `jhvirt.service`, `jhvirt.env`, `JHV_*`, база `jhvirt` и cookie
`jhvirt_session` намеренно сохранены: их переименование разорвало бы обычное
обновление, существующие сессии и привязку к данным.

| Режим | Основной YAML | Переменные окружения | Запуск |
|---|---|---|---|
| Docker из репозитория | `<репозиторий>/config/ovirt-backup.yaml` | `<репозиторий>/deploy/.env` | `<репозиторий>/deploy/docker-compose.yml` |
| Docker из `.run` | `/opt/jhvirt/config/ovirt-backup.yaml` | `/opt/jhvirt/compose/.env` | `/opt/jhvirt/compose/docker-compose.yml` |
| systemd из `.run` | `/opt/jhvirt/config/ovirt-backup.yaml` | `/opt/jhvirt/config/jhvirt.env` | `/etc/systemd/system/jhvirt.service` |

При установке с другим `PREFIX` замените `/opt/jhvirt` указанным каталогом:

```bash
sudo PREFIX=/srv/jhvirt sh ./ovirt-backup-*.run
```

Unit всегда устанавливается в `/etc/systemd/system/jhvirt.service`, но пути
внутри него рендерятся с фактическим `PREFIX`.

## 2. Что считается конфигурацией

Настройки собираются в следующем порядке, каждый следующий уровень
перекрывает предыдущий:

1. встроенные значения по умолчанию;
2. `ovirt-backup.yaml`;
3. переменные окружения `JHV_*`;
4. сохранённые в PostgreSQL runtime-переопределения для поддерживаемых
   параметров.

Имя переменной строится из YAML-пути: точки заменяются подчёркиваниями, имя
переводится в верхний регистр и добавляется префикс `JHV_`:

```text
server.port                     -> JHV_SERVER_PORT
server.external_url             -> JHV_SERVER_EXTERNAL_URL
database.url                    -> JHV_DATABASE_URL
monitor.remediation.dry_run     -> JHV_MONITOR_REMEDIATION_DRY_RUN
backup.transfer.prefer_proxy    -> JHV_BACKUP_TRANSFER_PREFER_PROXY
```

`JHV_DATABASE_URL` имеет приоритет над отдельными полями
`database.postgres.*`.

Runtime-переопределения доступны администратору в интерфейсе:

| Экран | Параметры | Применение |
|---|---|---|
| «Настройки → Система» | `backup.compression` | только к новым запускам бэкапа |
| «Настройки → Журнал» | `logging.max_size_mb`, `logging.max_backups`, `logging.max_age_days` | сразу, без перезапуска |

Интерфейс показывает источник эффективного значения: `config` означает YAML
или окружение, `database` — переопределение в PostgreSQL. Кнопка сброса удаляет
только запись из БД и сразу возвращает значение конфигурации. Уровень сжатия
`backup.compression_level` остаётся параметром YAML/окружения и в интерфейсе
доступен только для чтения. Уже запущенный бэкап сохраняет алгоритм, выбранный
при его старте; цепочка может состоять из звеньев с разными алгоритмами.

Подключения к oVirt/KVM, пользователи, задания, история, runtime-переопределения
и состояние авто-восстановления, изменённое через интерфейс, находятся в
PostgreSQL. Их нет в YAML или `.env`.

## 3. Docker из репозитория

Рабочий каталог:

```bash
cd <репозиторий>/deploy
```

Файлы:

```text
../config/ovirt-backup.yaml   полная конфигурация приложения
.env                          параметры Compose и секреты PostgreSQL
docker-compose.yml            описание контейнеров и привязка переменных
```

`.env` создаёт `install.sh`. В нём находятся, в частности:

| Переменная | Назначение |
|---|---|
| `JHV_EXTERNAL_URL` | внешний адрес браузера и схема сессионной cookie |
| `JHV_PORT` | опубликованный host-порт; внутри контейнера сервер слушает 8080 |
| `POSTGRES_USER`, `POSTGRES_PASSWORD`, `POSTGRES_DB` | локальная контейнерная PostgreSQL |
| `JHV_BACKUP_DIR` | host-каталог, смонтированный как `/backups` |
| `JHV_RESTORE_DIR` | host-каталог, смонтированный как `/restores` |
| `TZ` | часовой пояс контейнера и планировщика |

YAML копируется в образ как `/app/config/ovirt-backup.yaml`. Не редактируйте
его внутри контейнера: изменение исчезнет при пересоздании. После изменения
host-файла пересоберите образ:

```bash
cd <репозиторий>/deploy
docker compose build ovirt-backup
docker compose run --rm --no-deps ovirt-backup -check-config
docker compose up -d
PORT="$(sed -n 's/^JHV_PORT=//p' .env)"
curl -fsS "http://127.0.0.1:${PORT:-8080}/readyz"
```

После изменения только `.env` достаточно пересоздать контейнер:

```bash
docker compose up -d
```

Для Compose v1 замените `docker compose` на `docker-compose`.

## 4. Docker из `.run`

Стандартные пути:

```text
/opt/jhvirt/config/ovirt-backup.yaml
/opt/jhvirt/compose/.env
/opt/jhvirt/compose/docker-compose.yml
```

Редактирование и применение:

```bash
sudoedit /opt/jhvirt/config/ovirt-backup.yaml
sudoedit /opt/jhvirt/compose/.env

cd /opt/jhvirt/compose
sudo docker compose build ovirt-backup
sudo docker compose run --rm --no-deps ovirt-backup -check-config
sudo docker compose up -d
```

При повторном запуске `.run` существующий YAML сохраняется, а конфигурация из
новой версии записывается как
`/opt/jhvirt/config/ovirt-backup.yaml.new`. Сравните файлы вручную и перенесите
нужные новые параметры. `.env` также сохраняется; установщик изменяет в нём
только внешний URL и host-порт согласно переданным `--url` и `--port`.

## 5. Systemd

Стандартные пути:

```text
/opt/jhvirt/config/ovirt-backup.yaml   основные настройки
/opt/jhvirt/config/jhvirt.env          секреты и переопределения JHV_*
/etc/systemd/system/jhvirt.service     unit
/opt/jhvirt/data/secret.key            ключ шифрования
/opt/jhvirt/logs/                      файлы журналов, если включены
```

Посмотреть фактические пути, включая нестандартный `PREFIX`:

```bash
sudo systemctl cat jhvirt
sudo systemctl show jhvirt -p FragmentPath -p ExecStart -p EnvironmentFiles
```

Изменение YAML:

```bash
sudoedit /opt/jhvirt/config/ovirt-backup.yaml
```

Изменение переменных или DSN:

```bash
sudoedit /opt/jhvirt/config/jhvirt.env
sudo chmod 600 /opt/jhvirt/config/jhvirt.env
sudo chown jhvirt:jhvirt /opt/jhvirt/config/jhvirt.env
```

Не передавайте содержимое `jhvirt.env` в диагностических сообщениях: там
может находиться пароль PostgreSQL.

Перед перезапуском проверьте итоговую конфигурацию от имени службы:

```bash
sudo systemd-run --quiet --wait --pipe --collect \
  --unit="jhvirt-config-check-$$" \
  --uid=jhvirt --gid=jhvirt \
  --working-directory=/opt/jhvirt \
  --property=EnvironmentFile=/opt/jhvirt/config/jhvirt.env \
  /opt/jhvirt/bin/ovirt-backup-server \
  -config /opt/jhvirt/config/ovirt-backup.yaml -check-config

sudo systemctl restart jhvirt
curl -fsS http://127.0.0.1:8080/readyz
```

При обновлении существующий `ovirt-backup.yaml` не перезаписывается. Новый
образец появляется рядом как `ovirt-backup.yaml.new`; `jhvirt.env` также
сохраняется. При первом обновлении с прежнего имени файла установщик копирует
`virt-manager.yaml` в `ovirt-backup.yaml`, оставляя исходный файл на месте.

## 6. Ключ, база и каталоги данных

Не путайте конфигурацию с данными приложения:

| Объект | Docker | systemd |
|---|---|---|
| Ключ шифрования | volume `jhvirt-data`, файл `/app/data/secret.key` | `<PREFIX>/data/secret.key` |
| PostgreSQL | именованный volume `postgres-data` | локальная или внешняя PostgreSQL |
| Подключения, задания, пользователи | PostgreSQL | PostgreSQL |
| Бэкапы ВМ | host-путь из `JHV_BACKUP_DIR` или внешнее хранилище | путь/хранилище из YAML |
| Восстановленные образы | host-путь из `JHV_RESTORE_DIR` | разрешённые каталоги из YAML и unit |

Надёжно получить Docker-ключ, не завися от внутреннего пути Docker volume:

```bash
cd <каталог-compose>
docker compose cp ovirt-backup:/app/data/secret.key \
  ./secret.key.backup
chmod 600 ./secret.key.backup
```

`secret.key` не является заменяемой настройкой. Потеря этого файла делает
сохранённые пароли подключений и зашифрованные копии нерасшифровываемыми.

## 7. Что сохраняется

| Операция | YAML | `.env`/`jhvirt.env` | `secret.key` | PostgreSQL и тома |
|---|---|---|---|---|
| Обновление Docker `.run` | сохраняется, новый образец `.new` | сохраняется; URL и порт обновляются | сохраняется | сохраняются |
| Обновление systemd | сохраняется, новый образец `.new` | сохраняется | сохраняется | сохраняются |
| `--uninstall=docker` | сохраняется | сохраняется | volume сохраняется | volumes сохраняются |
| `--uninstall=systemd` | сохраняется | сохраняется | сохраняется | PostgreSQL сохраняется |
| `--uninstall=all` | сохраняется | сохраняется | сохраняется | PostgreSQL и volumes сохраняются |

Полное удаление `/opt/jhvirt`, Docker volumes или базы выполняется только
вручную. Перед этим сохраните `secret.key`, конфигурацию, env-файл и дамп
PostgreSQL.

## 8. Быстрая диагностика

Docker из репозитория:

```bash
cd <репозиторий>/deploy
pwd
ls -la .env docker-compose.yml ../config/ovirt-backup.yaml
grep -E '^(JHV_EXTERNAL_URL|JHV_PORT|JHV_BACKUP_DIR|JHV_RESTORE_DIR)=' .env
```

Docker из `.run`:

```bash
sudo ls -la /opt/jhvirt/compose/.env \
  /opt/jhvirt/compose/docker-compose.yml \
  /opt/jhvirt/config/ovirt-backup.yaml
```

Systemd:

```bash
sudo ls -la /opt/jhvirt/config/ovirt-backup.yaml \
  /opt/jhvirt/config/jhvirt.env \
  /etc/systemd/system/jhvirt.service
sudo grep -E '^(JHV_SERVER_EXTERNAL_URL|JHV_SERVER_PORT)=' \
  /opt/jhvirt/config/jhvirt.env
```

Не выводите `.env` или `jhvirt.env` целиком: они содержат секреты.
