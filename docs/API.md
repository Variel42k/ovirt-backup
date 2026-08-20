# REST API

База: `/api/v1`. Ответы — JSON в UTF-8. Коллекции обёрнуты в `{"items": [...], "total": N}`.

## Аутентификация

Сессионная cookie после `POST /auth/login` либо статический токен из
`auth.api_tokens` в заголовке `Authorization: Bearer <token>`.

Cookie называется `jhvirt_session`, имеет `HttpOnly`, `SameSite=Lax`, путь `/`
и срок из `auth.session_ttl`. Флаг `Secure` включается, если собственный TLS
приложения активен или `server.external_url` начинается с `https://`.
`X-Forwarded-Proto` сам по себе этот выбор не меняет: внешний URL является
явным production-контрактом оператора.

```bash
curl -c cookies.txt -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"…"}'

curl -b cookies.txt http://localhost:8080/api/v1/dashboard
```

Для проверки входа всегда выполняйте второй запрос. Login `200` ещё не
доказывает, что браузер вернёт cookie. Если приложение настроено на HTTPS, но
страница открыта по HTTP, Secure-cookie будет отброшена и `/auth/me` ответит
`401`. Пошаговое исправление:
[TROUBLESHOOTING.md](TROUBLESHOOTING.md#пароль-принят-но-снова-появляется-форма-входа).

Роли: `admin` (всё), `operator` (управление ВМ и бэкапами), `viewer` (чтение).

### Внешний вход (OIDC)

Три точки, доступные без сессии:

| Метод | Путь | Назначение |
|---|---|---|
| `GET` | `/auth/oidc/info` | `{"enabled":bool,"button_label":string,"local_login":bool}` — что показывать на странице входа |
| `GET` | `/auth/oidc/start` | перенаправление к провайдеру; `?redirect=/путь` задаёт, куда вернуть после входа |
| `GET` | `/auth/oidc/callback` | возврат от провайдера; при успехе выдаёт ту же `jhvirt_session` |

Проходить их нужно браузером: это цепочка перенаправлений, и XHR отработает её
молча и без результата. Ошибка возвращает на `/login?oidc_error=<причина>`,
подробности остаются в журнале службы.

`POST /auth/logout` у сессии, заведённой через провайдера, возвращает
`{"status":"ok","logout_url":"…"}` — по этому адресу интерфейс уводит браузер,
чтобы закрылась и сессия провайдера. У входа по паролю поля нет.

`start` и `callback` защищены `state`, `nonce` и PKCE (S256); начатый вход
живёт 10 минут и используется один раз. Подпись токена проверяется по JWKS
провайдера.

Когда `auth.oidc.allow_local_login: false`, `POST /auth/login` отвечает `403`
с кодом `local_login_disabled`. Токены из `auth.api_tokens` при этом
продолжают работать.

Настройка провайдера и правила назначения ролей:
[CONFIGURATION.md](CONFIGURATION.md#внешний-вход-oidc).

## Ошибки

```json
{ "error": "человекочитаемое сообщение", "code": "not_found" }
```

| Код | HTTP | Смысл |
|---|---|---|
| `unauthorized` | 401 | нет сессии или она истекла |
| `too_many_attempts` | 429 | слишком много неудачных входов подряд |
| `forbidden` | 403 | роли недостаточно |
| `local_login_disabled` | 403 | вход по паролю выключен, остаётся внешний провайдер |
| `oidc_disabled` | 404 | внешний вход не настроен |
| `bad_request` | 400 | некорректный запрос |
| `not_found` | 404 | объект не найден |
| `conflict` | 409 | нарушение уникальности |
| `job_busy` | 409 | задание ещё выполняется с прошлого раза |
| `engine_auth` | 502 | движок отверг наши учётные данные |
| `engine_not_found` | 404 | движок не знает такой объект |
| `engine_conflict` | 409 | движок отказал: объект в неподходящем состоянии |
| `internal` | 500 | необработанная ошибка; подробности в журнале службы |

`too_many_attempts` сопровождается заголовком `Retry-After` с числом секунд.
Пауза растёт с каждой неудачей до 15 минут и считается по учётной записи, а не
по адресу: адрес берётся из `X-Forwarded-For`, который подделывается. Успешный
вход обнуляет счётчик.

`job_busy` — не поломка, а состояние: предыдущий запуск задания не закончился.
Интерфейсу стоит показывать его именно так, а не как ошибку.

---

## Справочники и обзор

| Метод | Путь | Описание |
|---|---|---|
| `GET` | `/meta` | типы бэкапа, режимы проверки, возможности установки |
| `GET` | `/dashboard` | сводка по всем серверам одним запросом |
| `GET` | `/events` | поток server-sent events |
| `GET` | `/audit` | журнал изменений (admin) |

## Runtime-настройки

Все методы раздела требуют роль `admin`. Настройки сохраняются в PostgreSQL и
применяются без перезапуска процесса.

| Метод | Путь | Описание |
|---|---|---|
| `GET` | `/settings/runtime` | эффективные значения и их источник |
| `PUT/DELETE` | `/settings/runtime/compression` | задать алгоритм или вернуть конфигурационный |
| `PUT/DELETE` | `/settings/runtime/timezone` | задать системную IANA-зону или вернуть конфигурационную |
| `PUT/DELETE` | `/settings/runtime/log-rotation` | задать политику ротации или вернуть конфигурационную |

```jsonc
// PUT /settings/runtime/compression
{ "compression": "gzip" } // zstd|gzip|s2|none

// PUT /settings/runtime/timezone
{ "timezone": "Asia/Yekaterinburg" }

// PUT /settings/runtime/log-rotation
{ "max_size_mb": 200, "max_backups": 14, "max_age_days": 60 }

// GET и ответы PUT/DELETE
{
  "compression": {
    "value": "gzip", "level": 3, "source": "database",
    "options": [
      { "value": "zstd", "title": "ZSTD", "description": "…" },
      { "value": "gzip", "title": "GZIP", "description": "…" },
      { "value": "s2", "title": "S2", "description": "…" },
      { "value": "none", "title": "NONE", "description": "…" }
    ]
  },
  "timezone": {
    "value": "Asia/Yekaterinburg", "source": "database"
  },
  "log_rotation": {
    "max_size_mb": 200, "max_backups": 14,
    "max_age_days": 60, "source": "database"
  }
}
```

`DELETE` удаляет переопределение и возвращает значения из YAML/окружения.
Сжатие меняется только для новых запусков; активный запуск использует алгоритм,
зафиксированный при старте. `compression_level` через этот API не изменяется.
Смена часового пояса немедленно пересчитывает будущие точки всех cron-заданий,
но не отменяет и не перезапускает уже выполняющиеся бэкапы. Эта же зона используется web,
отчётами, уведомлениями и журналом; открытые web-сессии получают SSE-событие
`settings.changed`. Неизвестная IANA-зона возвращает `400`.

## Подключения

| Метод | Путь | Описание |
|---|---|---|
| `GET` | `/servers` | список |
| `POST` | `/servers` | создать (admin) |
| `GET/PUT/DELETE` | `/servers/{id}` | получить, изменить, удалить |
| `POST` | `/servers/probe` | проверить подключение, ничего не сохраняя |
| `POST` | `/servers/ca-certificate` | скачать CA движка |
| `POST` | `/servers/{id}/refresh` | опросить сейчас |
| `GET` | `/servers/{id}/summary` | сводка по одному серверу |

Пустой `password` при изменении оставляет сохранённый пароль.

## Инвентарь и управление

| Метод | Путь | Описание |
|---|---|---|
| `GET` | `/servers/{id}/clusters` `\|hosts\|vms\|disks\|storage-domains` | кэш инвентаря |
| `GET` | `/servers/{id}/vms/{vmID}` | одна ВМ |
| `GET` | `/servers/{id}/vms/{vmID}/disks` | диски ВМ |
| `GET` | `/servers/{id}/restore-networks` | доступные vNIC profiles oVirt или сети KVM для восстановления |
| `POST` | `/servers/{id}/vms/{vmID}/action` | питание ВМ |
| `PUT` | `/servers/{id}/vms/{vmID}/policy` | требуемое состояние |
| `PUT` | `/servers/{id}/vms/{vmID}/tags` | локальные управляемые теги KVM |
| `POST` | `/servers/{id}/hosts/{hostID}/action` | управление хостом |
| `PUT` | `/servers/{id}/disks/{diskID}/backup-mode` | включить/выключить CBT |
| `GET` | `/servers/{id}/vms/{vmID}/backup-options` | рекомендатель вариантов |

```jsonc
// POST /servers/{id}/vms/{vmID}/action
{ "action": "start" }                      // start|shutdown|stop|suspend|reboot|reset|migrate
{ "action": "stop", "confirm": true }      // stop и reset требуют подтверждения
{ "action": "migrate", "host_id": "…" }    // пустой host_id — выбирает планировщик

// POST /servers/{id}/hosts/{hostID}/action
{ "action": "fence", "fence_type": "restart", "confirm": true }
```

`GET /servers/{id}/vms/{vmID}/backup-options` возвращает оценку ситуации,
варианты с обоснованием и готовые расписания — то, из чего строится экран выбора
стратегии.

## Хранилища

| Метод | Путь | Описание |
|---|---|---|
| `GET/POST` | `/storages` | список, создать (admin) |
| `GET/PUT/DELETE` | `/storages/{id}` | получить, изменить, удалить |
| `POST` | `/storages/{id}/check` | проверка записи и чтения |

Секреты (`secret_key`, `password`, `private_key`) принимаются, но никогда не
возвращаются. Удаление хранилища с живыми бэкапами требует `?force=true`.

Поле `kind` принимает `local`, `s3`, `smb`, `webdav`, `sftp`. Обязательные поля
каждого типа перечислены в разделе «Хранилища копий» документа по конфигурации;
список типов с названиями и описаниями отдаёт `GET /meta`, и интерфейс строит
форму по нему. Для `smb` заполняются `host`, `share`, `username`, `password` и
необязательные `domain`, `port`, `base_path`; для `webdav` — `endpoint`,
`username`, `password` и необязательные `base_path`, `insecure_tls`. Флаг
`insecure_tls` принимается только для `webdav`, `object_lock_enabled` — только
для `s3`.

Поле `rate_limit` задаёт общий предел потоковой записи в байтах в секунду для
всех одновременных операций с экземпляром хранилища; `0` снимает ограничение.
Предел применяется ко всем типам хранилищ. Нативный server-side copy внутри одного
S3 endpoint не передаёт данные через сервис и потому этим пределом не замедляется.

## Задания

| Метод | Путь | Описание |
|---|---|---|
| `GET/POST` | `/jobs` | список, создать |
| `GET/PUT/DELETE` | `/jobs/{id}` | получить, изменить, удалить |
| `POST` | `/jobs/{id}/run` | запустить немедленно |
| `GET` | `/jobs/{id}/preview` | какие ВМ попадают под отбор и почему |

```jsonc
{
  "name": "Ночной инкремент",
  "server_id": "…",
  "vm_ids": [],
  "vm_name_regex": "^(prod|infra)-",
  "cluster_ids": ["…"],
  "tags": ["critical"],             // положительные условия объединяются через OR
  "exclude_vm_ids": ["…"],          // исключения применяются первыми
  "exclude_disk_ids": ["…"],        // пустой selector — все ВМ сервера
  "type": "incremental",
  "full_every": 7,
  "fallback_type": "snapshot",        // если CBT недоступен
  "schedule": "0 1 * * *",
  "storage_target_ids": ["…"],
  "storage_mode": "copy",             // copy|parallel|separate
  "priority": 10,                      // больше — раньше в персистентной очереди
  "concurrency": 2,                    // предел только для этого задания
  "retention": { "keep_last": 3, "keep_daily": 7, "keep_weekly": 4, "keep_monthly": 6 },
  "quiesce": true,
  "export_qcow2": true,
  "verify_after": "boot",
  "verify_options": {
    "boot_host_id": "…",             // включённое подключение типа kvm
    "memory_mib": 0, "vcpus": 0,   // 0 — ресурсы исходной ВМ
    "timeout_sec": 300, "keep_on_failure": false
  },
  "encrypt": false
}
```

Для legacy-клиентов принимается `replication_enabled`: `true` соответствует
`storage_mode: "copy"`. В новых клиентах источником истины служит
`storage_mode`: `copy` снимает одну основную копию и ставит остальные в очередь,
`parallel` пишет доступные цели одновременно и дозаписывает отказавшие,
`separate` независимо читает источник для каждого хранилища.

У задания типа `ova` вместо `storage_target_ids` обязательны `ova_host_id` и
`ova_directory`. OVA остаётся внешним артефактом на гипервизоре, не участвует в
обычной репликации и ретенции. `export_qcow2` относится к управляемым артефактам
обычного бэкапа; до запуска проверяется наличие `qemu-img`.

## Бэкапы

| Метод | Путь | Описание |
|---|---|---|
| `POST` | `/backups` | разовый бэкап |
| `GET` | `/backups` | список; фильтры `server_id`, `vm_id`, `job_id`, `status`, `days`, `limit` |
| `GET` | `/backups/{id}` | запуск вместе с дисками |
| `GET` | `/backups/{id}/chain` | цепочка, от которой зависит эта точка |
| `GET` | `/backups/{id}/copies` | физические копии точки и их состояние |
| `GET` | `/backups/{id}/artifacts` | управляемые производные артефакты, включая QCOW2 |
| `DELETE` | `/backups/{id}` | удалить данные из хранилища |
| `POST` | `/backups/{id}/cancel` | отменить выполняющийся |
| `POST` | `/backups/{id}/verify` | проверить |
| `POST` | `/backups/{id}/restore` | восстановить |
| `POST` | `/backups/{id}/restore-vm/plan` | проверить план восстановления новой ВМ без изменений |
| `POST` | `/backups/{id}/restore-vm` | создать новую ВМ; ответ `202` содержит `restore_id` |
| `GET` | `/verifications` `/verifications/{id}` | история и состояние проверок |
| `GET` | `/restores` `/restores/{id}` | история, фаза, прогресс и результат восстановления |

```jsonc
// POST /backups
{ "server_id": "…", "vm_id": "…", "type": "full",
  "storage_target_id": "…", "quiesce": true, "verify_after": "boot",
  "verify_options": { "boot_host_id": "…", "memory_mib": 0,
    "vcpus": 0, "timeout_sec": 300 },
  "retain_days": 30 }

// POST /backups/{id}/verify — quick и chain отвечают сразу, остальные в фоне
{ "mode": "restore" }

// Режим boot поднимает ВМ, поэтому просит хост: подключение типа kvm.
// Пусто — берётся хост, с которого снят бэкап, и это работает только если он
// сам типа kvm; для oVirt-бэкапа запрос отклоняется со списком подходящих.
// disk_id пусто — восстановить и подключить все диски с сохранёнными шинами и
// порядком загрузки. Конкретный disk_id запускает только один диск и нужен для
// диагностики, а не для обычной приёмочной проверки.
{ "mode": "boot", "boot_host_id": "…", "disk_id": "",
  "memory_mib": 0, "vcpus": 0, "timeout_sec": 300, "keep_on_failure": false }

// POST /backups/{id}/restore
// output_dir должен лежать внутри одного из backup.restore_dirs или внутри
// backup.temp_dir; иначе 400. Пусто — берётся temp_dir.
{ "target": "file", "output_dir": "/srv/restores/vm1", "output_format": "qcow2" }
{ "target": "new_disk", "target_domain_id": "…", "attach_to_vm_id": "…" }
{ "target": "disk", "target_disk_id": "…", "confirm": true }   // затирает диск

// POST /backups/{id}/restore-vm/plan и /restore-vm
// storage_domain_id означает storage domain для oVirt и storage pool для KVM.
// Неуказанные NIC по умолчанию создаются отключёнными, MAC всегда новый.
{
  "server_id": "…", "name": "vm-restored", "cluster_id": "…",
  "storage_domain_id": "…", "start": false,
  "network_mappings": [
    { "nic_id": "nic-1", "target_kind": "vnic_profile",
      "target_id": "…", "connected": false },
    { "nic_id": "nic-2", "exclude": true }
  ]
}
```

Каталог восстановления ограничен списком не из осторожности вообще: путь задаёт
клиент, а результат — образ на десятки гигабайт, то есть без списка любой
оператор мог бы заполнить раздел с базой или положить файл в каталог
конфигурации. Проверка выполняется до ответа, поэтому запрещённый каталог
виден сразу, а не всплывает как «восстановление не выполнено».

Удаление отклоняется, если от точки зависят более поздние инкременты.

Полное восстановление поддерживает oVirt→oVirt и KVM→KVM. Межплатформенная
конвертация не выполняется. Preview возвращает `warnings`, `blockers`, диски,
NIC и оценку места. Для старых точек недостающие части профиля восстанавливаются
по исходному oVirt JSON или libvirt XML с предупреждением. При ошибке служба
откатывает созданные ресурсы; неполная очистка остаётся в поле ошибки запуска.
Конфигурационная точка может создать ВМ без дисков, но автоматический запуск в
этом случае запрещён.

## Снимки конфигурации Engine

| Метод | Путь | Описание |
|---|---|---|
| `GET/POST` | `/engine-config/jobs` | список и создание заданий oVirt Engine |
| `PUT/DELETE` | `/engine-config/jobs/{id}` | изменить или удалить задание |
| `POST` | `/engine-config/jobs/{id}/run` | выполнить задание немедленно |
| `GET/POST` | `/engine-config/runs` | список или разовый снимок без задания |
| `GET` | `/engine-config/runs/{id}` | метаданные, checksum и состояние снимка |
| `GET` | `/engine-config/runs/{id}/download` | скачать JSON снимка |
| `GET` | `/engine-config/compare?left={id}&right={id}` | сравнить секции двух снимков |

```jsonc
// POST /engine-config/jobs
{
  "name": "Engine ежедневно", "enabled": true, "server_id": "…",
  "storage_target_id": "…", "encrypt": true, "schedule": "30 2 * * *",
  "retention": { "keep_last": 3, "keep_daily": 7, "keep_monthly": 6 }
}

// POST /engine-config/runs
{ "server_id": "…", "storage_target_id": "…", "encrypt": true }
```

Снимок является server-level артефактом: в нём нет дисков ВМ. Поддерживаются
только подключения семейства oVirt. API умеет просматривать, сравнивать и
скачивать сохранённые данные, но намеренно не применяет конфигурацию к Engine.

## Нативный файловый бэкап

Функция доступна только при `file_backup.enabled: true`. Клиент выбирает ID
именованного корня из конфигурации и относительные пути; абсолютный исходный
путь через API передать нельзя.

| Метод | Путь | Описание |
|---|---|---|
| `GET` | `/file-backup/roots` | включён ли feature gate и доступные именованные корни |
| `GET/POST` | `/file-backup/jobs` | список и создание файловых заданий |
| `GET/PUT/DELETE` | `/file-backup/jobs/{id}` | получить, изменить или удалить задание |
| `POST` | `/file-backup/jobs/{id}/run` | запустить; ответ `202` содержит запуск |
| `GET` | `/file-backup/runs?job_id={id}&limit=100` | точки файлового бэкапа |
| `GET/DELETE` | `/file-backup/runs/{id}` | получить или удалить точку с данными |
| `GET` | `/file-backup/runs/{id}/tree` | manifest и дерево файлов |
| `POST` | `/file-backup/runs/{id}/restore` | восстановить набор или выбранные пути |

```jsonc
// POST /file-backup/jobs
{
  "name": "Конфигурация приложений", "enabled": true,
  "root_id": "application-data",
  "include_paths": ["config", "certificates/server.pem"],
  "exclude_globs": ["**/*.tmp", "cache/**"],
  "storage_target_ids": ["…"], "storage_mode": "copy",
  "incremental": true, "encrypt": true, "schedule": "0 3 * * *",
  "retention": { "keep_last": 3, "keep_daily": 7 }
}

// POST /file-backup/runs/{id}/restore
{
  "restore_root_index": 0, "destination": "incident-2026-08-21",
  "paths": ["config/app.yaml", "certificates"], "overwrite": false
}
```

`destination` также относительный и обязан оставаться внутри выбранного
`restore_root_index`. По умолчанию существующие файлы не перезаписываются.
Symlink хранится и восстанавливается как ссылка, но при сканировании не
обходится; canonical path не может выйти из allowlist. Изменившийся во время
чтения файл перечитывается один раз, затем точка завершается как `partial` со
списком `unstable_paths`.

## Ретенция

| Метод | Путь | Описание |
|---|---|---|
| `POST` | `/retention/preview` | что политика удалит — без удаления |
| `POST` | `/retention/apply` | применить |

Ответ содержит списки `keep` и `delete` с причиной по каждой копии и
`freed_bytes`.

## Мониторинг

| Метод | Путь | Описание |
|---|---|---|
| `GET` | `/alerts` | оповещения; по умолчанию только активные |
| `POST` | `/alerts/{id}/ack` | принять в работу |
| `GET` | `/remediations` | журнал восстановительных действий |
| `POST` | `/remediations` | выполнить действие вручную |
| `GET` | `/health-samples` | история проб состояния |
| `GET` | `/monitoring/backup-quality` | худшее состояние ВМ по расписаниям и репликам |
| `GET` | `/monitoring/backup-series?period=24h\|7d\|30d\|90d` | успешность, пропуски, скорость и объёмы |
| `GET` | `/monitoring/storage-capacity?period=24h\|7d\|30d\|90d` | история и прогноз хранилищ |
| `GET` | `/job-runs` | общие запуски заданий; фильтры `job_id`, `server_id`, `limit` |

```jsonc
// POST /remediations — обходит cooldown и лимит попыток
{ "server_id": "…", "scope": "vm", "object_id": "…",
  "action": "vm_start", "reason": "по заявке", "confirm": false }
```

`action`: `vm_start`, `vm_unpause`, `vm_reset`, `host_activate`, `host_fence`,
`engine_reconnect`. Разрушительные (`vm_reset`, `host_fence`) требуют
`confirm: true`.

`GET /coverage` сохранён для старых клиентов, но использует прежнюю форму
ответа. Новые клиенты должны читать `/monitoring/backup-quality`: его свежесть
рассчитывается по каждому cron-интервалу и каждой обязательной реплике.

Успешность `backup_job_runs` означает, что все выбранные ВМ и все назначенные
хранилища получили полные дочерние копии. Ответ `POST /jobs/{id}/run`:

```json
{"status":"queued","job_run_id":"…","vms":4,"replicas":8}
```

### Runtime-пороги (admin)

| Метод | Путь | Описание |
|---|---|---|
| `GET` | `/settings/runtime` | эффективные значения и источник `config`/`database` |
| `PUT/DELETE` | `/settings/runtime/timezone` | сохранить системную IANA-зону или сбросить к YAML/окружению |
| `PUT` | `/settings/runtime/backup-quality` | сохранить полный набор порогов в PostgreSQL |
| `DELETE` | `/settings/runtime/backup-quality` | сбросить к YAML/окружению |

```json
{
  "stale_intervals": 2,
  "verify_max_age_days": 7,
  "performance_window_runs": 10,
  "performance_degradation_percent": 50,
  "performance_consecutive_runs": 3,
  "storage_warning_free_percent": 15,
  "storage_critical_free_percent": 5,
  "storage_warning_forecast_days": 30,
  "storage_critical_forecast_days": 7,
  "history_retention_days": 90
}
```

Изменение пишется в аудит и применяется без перезапуска. Запрос всегда передаёт
весь объект: частичное изменение не поддерживается.

### `/metrics`

`GET /metrics` находится вне `/api/v1` и cookie-аутентификации. При
`metrics.enabled: false` возвращается `404`. При включении нужен отдельный
заголовок:

```http
Authorization: Bearer <содержимое metrics.token_file>
```

Отсутствующий или неверный токен даёт `401`. Сравнение выполняется в постоянное
время. Endpoint не принимает `auth.api_tokens` и не выводит URL с учётными
данными, DSN, токены, cookie или ключи. Имена объектов находятся только в
`*_info`-метриках; рабочие ряды маркированы стабильными ID.

## Поток событий

`GET /events` — server-sent events. Типы: `server_state`, `inventory`, `alert`,
`remediation`, `backup_run`, `verify_run`, `restore_run`, `replication`,
`storage_target`, `job`, `settings.changed`.

```javascript
const source = new EventSource('/api/v1/events', { withCredentials: true })
source.addEventListener('alert', (e) => console.log(JSON.parse(e.data)))
```

Доставка намеренно с потерями: подписчик, переставший читать, не должен
останавливать мониторинг. После переподключения интерфейс перечитывает состояние.

## Пользователи

| Метод | Путь | Описание |
|---|---|---|
| `GET/POST` | `/users` | список, создать (admin) |
| `PUT/DELETE` | `/users/{id}` | изменить, удалить (admin) |

Пароль не короче 10 символов. Нельзя удалить себя и последнего администратора.

## Физические копии и репликация

| Метод | Путь | Назначение |
|---|---|---|
| `GET` | `/backups/{id}/copies` | основная копия и реплики точки |
| `POST` | `/backups/{id}/copies` | добавить обязательную реплику |
| `POST` | `/backup-copies/{id}/retry` | немедленно повторить передачу |
| `POST` | `/backup-copies/{id}/cancel` | отменить текущую передачу |
| `GET` | `/replications` | очередь и последние состояния |
| `GET` | `/replications/{id}` | копия и история попыток |
| `POST` | `/jobs/{id}/enable-replication` | перевести legacy-задание и потребовать полную точку |
| `POST` | `/jobs/{id}/change-primary` | назначить первое хранилище и потребовать полную точку |

`POST /backups/{id}/copies` принимает `{"storage_target_id":"..."}`.
Проверка и восстановление принимают необязательный `copy_id`; без него сервер
выбирает успешную основную копию, затем первую успешную реплику, у которой есть
вся родительская цепочка. Ответы бэкапов дополнены `copy_count`,
`healthy_copy_count` и `protection_status`.

## Каталог хранилища

| Метод | Путь | Назначение |
|---|---|---|
| `POST` | `/storages/{id}/catalog-scans` | запустить асинхронный просмотр |
| `GET` | `/storages/{id}/catalog-scans` | последние просмотры хранилища |
| `GET` | `/catalog-scans/{id}` | состояние и найденные записи |
| `POST` | `/catalog-scans/{id}/import` | явно импортировать выбранные записи |

Импорт принимает `{"entry_ids":["..."]}` и транзакционно добавляет выбранную
точку вместе с доступными родителями. Ничего не перезаписывается при конфликте
одинакового `run_id` и различающихся манифестов.

## Аварийная готовность

| Метод | Путь | Назначение |
|---|---|---|
| `GET` | `/disaster-recovery/readiness` | последний результат проверки |
| `POST` | `/disaster-recovery/check` | выполнить проверку сейчас |

Оба маршрута административные. Секреты и содержимое дампа в ответ не входят.
