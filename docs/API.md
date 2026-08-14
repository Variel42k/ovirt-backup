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

## Ошибки

```json
{ "error": "человекочитаемое сообщение", "code": "not_found" }
```

| Код | HTTP | Смысл |
|---|---|---|
| `unauthorized` | 401 | нет сессии или она истекла |
| `too_many_attempts` | 429 | слишком много неудачных входов подряд |
| `forbidden` | 403 | роли недостаточно |
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
| `PUT/DELETE` | `/settings/runtime/timezone` | задать IANA-зону расписаний или вернуть конфигурационную |
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
но не отменяет и не перезапускает уже выполняющиеся бэкапы. Неизвестная IANA-зона
возвращает `400`.

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
| `POST` | `/servers/{id}/vms/{vmID}/action` | питание ВМ |
| `PUT` | `/servers/{id}/vms/{vmID}/policy` | требуемое состояние |
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
  "vm_ids": [],                       // пусто — все ВМ сервера
  "type": "incremental",
  "full_every": 7,
  "fallback_type": "snapshot",        // если CBT недоступен
  "schedule": "0 1 * * *",
  "storage_target_ids": ["…"],
  "retention": { "keep_last": 3, "keep_daily": 7, "keep_weekly": 4, "keep_monthly": 6 },
  "quiesce": true,
  "verify_after": "boot",
  "verify_options": {
    "boot_host_id": "…",             // включённое подключение типа kvm
    "memory_mib": 0, "vcpus": 0,   // 0 — ресурсы исходной ВМ
    "timeout_sec": 300, "keep_on_failure": false
  },
  "encrypt": false
}
```

## Бэкапы

| Метод | Путь | Описание |
|---|---|---|
| `POST` | `/backups` | разовый бэкап |
| `GET` | `/backups` | список; фильтры `server_id`, `vm_id`, `job_id`, `status`, `days`, `limit` |
| `GET` | `/backups/{id}` | запуск вместе с дисками |
| `GET` | `/backups/{id}/chain` | цепочка, от которой зависит эта точка |
| `DELETE` | `/backups/{id}` | удалить данные из хранилища |
| `POST` | `/backups/{id}/cancel` | отменить выполняющийся |
| `POST` | `/backups/{id}/verify` | проверить |
| `POST` | `/backups/{id}/restore` | восстановить |

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
```

Каталог восстановления ограничен списком не из осторожности вообще: путь задаёт
клиент, а результат — образ на десятки гигабайт, то есть без списка любой
оператор мог бы заполнить раздел с базой или положить файл в каталог
конфигурации. Проверка выполняется до ответа, поэтому запрещённый каталог
виден сразу, а не всплывает как «восстановление не выполнено».

Удаление отклоняется, если от точки зависят более поздние инкременты.

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
| `PUT/DELETE` | `/settings/runtime/timezone` | сохранить IANA-зону расписаний или сбросить к YAML/окружению |
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
`remediation`, `backup_run`, `verify_run`, `restore_run`, `storage_target`, `job`.

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
