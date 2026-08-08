# REST API

База: `/api/v1`. Ответы — JSON в UTF-8. Коллекции обёрнуты в `{"items": [...], "total": N}`.

## Аутентификация

Сессионная cookie после `POST /auth/login` либо статический токен из
`auth.api_tokens` в заголовке `Authorization: Bearer <token>`.

```bash
curl -c cookies.txt -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"username":"admin","password":"…"}'

curl -b cookies.txt http://localhost:8080/api/v1/dashboard
```

Роли: `admin` (всё), `operator` (управление ВМ и бэкапами), `viewer` (чтение).

## Ошибки

```json
{ "error": "человекочитаемое сообщение", "code": "not_found" }
```

| Код | HTTP | Смысл |
|---|---|---|
| `unauthorized` | 401 | нет сессии или она истекла |
| `forbidden` | 403 | роли недостаточно |
| `bad_request` | 400 | некорректный запрос |
| `not_found` | 404 | объект не найден |
| `conflict` | 409 | нарушение уникальности |
| `engine_auth` | 502 | движок отверг наши учётные данные |
| `engine_conflict` | 409 | движок отказал: объект в неподходящем состоянии |

---

## Справочники и обзор

| Метод | Путь | Описание |
|---|---|---|
| `GET` | `/meta` | типы бэкапа, режимы проверки, возможности установки |
| `GET` | `/dashboard` | сводка по всем серверам одним запросом |
| `GET` | `/events` | поток server-sent events |
| `GET` | `/audit` | журнал изменений (admin) |

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
  "verify_after": "chain",
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
  "storage_target_id": "…", "quiesce": true, "verify_after": "manifest",
  "retain_days": 30 }

// POST /backups/{id}/verify — quick и chain отвечают сразу, остальные в фоне
{ "mode": "restore" }

// Режим boot поднимает ВМ, поэтому просит хост: подключение типа kvm.
// Пусто — берётся хост, с которого снят бэкап, и это работает только если он
// сам типа kvm; для oVirt-бэкапа запрос отклоняется со списком подходящих.
// disk_id пусто — загрузочный диск; если загрузочного нет и дисков несколько,
// запрос отклоняется, а не угадывает.
{ "mode": "boot", "boot_host_id": "…", "disk_id": "",
  "memory_mib": 2048, "vcpus": 2, "timeout_sec": 300, "keep_on_failure": false }

// POST /backups/{id}/restore
{ "target": "file", "output_dir": "/var/tmp", "output_format": "qcow2" }
{ "target": "new_disk", "target_domain_id": "…", "attach_to_vm_id": "…" }
{ "target": "disk", "target_disk_id": "…", "confirm": true }   // затирает диск
```

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

```jsonc
// POST /remediations — обходит cooldown и лимит попыток
{ "server_id": "…", "scope": "vm", "object_id": "…",
  "action": "vm_start", "reason": "по заявке", "confirm": false }
```

`action`: `vm_start`, `vm_unpause`, `vm_reset`, `host_activate`, `host_fence`,
`engine_reconnect`. Разрушительные (`vm_reset`, `host_fence`) требуют
`confirm: true`.

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
