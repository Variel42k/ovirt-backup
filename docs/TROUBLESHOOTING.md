# Диагностика и исправление ошибок

Документ построен по симптому: сначала короткая проверка, затем причина и
исправление. Команды выполняйте на сервере приложения, если не указано иное.

Пути по умолчанию:

| Установка | Рабочий каталог | Конфигурация |
|---|---|---|
| Docker из репозитория | `<репозиторий>/deploy` | `<репозиторий>/deploy/.env`, `<репозиторий>/config/virt-manager.yaml` |
| Docker из `.run` | `/opt/jhvirt/compose` | `/opt/jhvirt/compose/.env`, `/opt/jhvirt/config/virt-manager.yaml` |
| systemd | `/opt/jhvirt` | `/opt/jhvirt/config/jhvirt.env`, `/opt/jhvirt/config/virt-manager.yaml` |

При другом `PREFIX` замените `/opt/jhvirt` фактическим путём.

## 1. Универсальная последовательность проверки

### Шаг 1. Процесс отвечает?

```bash
curl -i --max-time 5 http://127.0.0.1:8080/healthz
curl -i --max-time 5 http://127.0.0.1:8080/readyz
```

- `/healthz` проверяет, что HTTP-процесс работает;
- `/readyz` дополнительно проверяет БД и возвращает состояние фоновых работ;
- `200` у `/healthz` и ошибка у `/readyz` обычно означают проблему БД или
  миграций;
- отказ соединения у обоих означает, что служба не запущена, слушает другой
  порт или завершилась при старте.

Если задан другой `--port`, используйте его.

### Шаг 2. Служба или контейнер запущены?

Docker:

```bash
docker compose ps
docker compose logs --tail 150 justhpc-virt-manager
docker compose logs --tail 100 postgres
```

Systemd:

```bash
sudo systemctl status jhvirt --no-pager
sudo journalctl -u jhvirt -n 150 --no-pager
sudo systemctl status postgresql --no-pager
```

На RHEL имя службы PostgreSQL может содержать версию. Найти её:

```bash
systemctl list-units --type=service 'postgresql*'
```

### Шаг 3. Проверьте фактические настройки

Docker:

```bash
grep -E '^(JHV_EXTERNAL_URL|JHV_PORT|JHV_BACKUP_DIR|JHV_RESTORE_DIR)=' .env
docker compose config --quiet
```

Systemd:

```bash
sudo grep -E '^(JHV_SERVER_EXTERNAL_URL|JHV_SERVER_PORT|JHV_DATABASE_URL)=' \
  /opt/jhvirt/config/jhvirt.env
sudo systemctl cat jhvirt
sudo systemd-run --quiet --wait --pipe --collect \
  --uid=jhvirt --gid=jhvirt --working-directory=/opt/jhvirt \
  --property=EnvironmentFile=/opt/jhvirt/config/jhvirt.env \
  /opt/jhvirt/bin/justhpc-virt-server \
  -config /opt/jhvirt/config/virt-manager.yaml -check-config
```

Не публикуйте вывод `jhvirt.env` или `.env` целиком: там находится DSN или
пароль PostgreSQL.

### Шаг 4. Отделите API от браузера

```bash
BASE=http://127.0.0.1:8080
read -rsp 'Пароль admin: ' JHV_PASSWORD; echo
curl -sS -D /tmp/jhv.headers -c /tmp/jhv.cookies \
  -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$JHV_PASSWORD\"}"
unset JHV_PASSWORD
curl -i -b /tmp/jhv.cookies "$BASE/api/v1/auth/me"
grep -i '^set-cookie:' /tmp/jhv.headers
rm -f /tmp/jhv.headers /tmp/jhv.cookies
```

Если login и `/auth/me` успешны, backend и БД исправны; ищите проблему в URL,
cookie, прокси или кэше браузера.

## 2. Ошибки установщика

### `нет терминала — укажите способ ключом`

Скрипт запущен через SSH/CI без TTY. В unattended-режиме явно передайте режим
и URL:

```bash
sudo sh jhvirt-*.run --mode systemd \
  --url http://10.20.30.40:8080 --port 8080
```

### `без диалога внешний адрес обязателен`

Добавьте `--url`. Не подставляйте случайный адрес: его схема управляет флагом
`Secure` у cookie.

### `адрес должен начинаться с http:// или https://`

Передавайте только схему, хост и необязательный порт, без пути:

```text
http://10.20.30.40:8080
https://virt.example.org
```

Значения вида `virt.example.org`, `https://host/path` и URL с логином не
принимаются.

### `порт должен быть в диапазоне 1..65535`

`--port` принимает только целое число от 1 до 65535. Это порт публикации
Docker и `JHV_SERVER_PORT` для systemd.

### `docker compose недоступен`

Проверьте:

```bash
docker --version
docker info
docker compose version
```

Частые причины: Docker daemon не запущен, пользователь не имеет доступа к
socket, установлен только CLI или команда `docker` является эмуляцией Podman.
Podman проектом не поддерживается. Используйте настоящий Docker или systemd.

### `systemd не найден`

Systemd должен быть запущен как init-система, а не только установлен как
пакет. В обычном непривилегированном контейнере режим systemd не работает.

### `неподдерживаемая платформа`

Автоматическая systemd-установка поддерживает Ubuntu/Debian с `apt` и
RHEL/AlmaLinux/Rocky Linux с `dnf`. Для другой системы распакуйте `.run` через
`--extract` и подготовьте БД/unit своим средством управления конфигурацией.

### `файл строки подключения ... должен иметь права 0600`

```bash
sudo chown root:root /root/jhvirt.dsn
sudo chmod 600 /root/jhvirt.dsn
stat -c '%a %U:%G' /root/jhvirt.dsn
```

Файл должен содержать ровно одну непустую строку.

### `контрольная сумма не сошлась`

`.run` был обрезан или изменён при переносе. Скопируйте его заново в бинарном
виде, предпочтительно `scp`, и выполните:

```bash
sh jhvirt-*.run --check
```

Не открывайте `.run` редактором: после shell-заголовка находится двоичный
архив.

### `комплект собран под ..., а система ...`

Сверьте:

```bash
uname -m
sh jhvirt-*.run --version
```

`x86_64` соответствует `linux/amd64`, `aarch64` — `linux/arm64`.

### `за 3 минуты сервис не стал готов`

Установщик уже выводит последние строки журнала. Дополнительно проверьте
`/healthz`, `/readyz`, БД, занятость порта и полные журналы по алгоритму из
раздела 1. Не запускайте установщик многократно, пока не понятна первая ошибка.

## 3. Docker Compose

### `required variable ... is missing a value`

`.env` отсутствует или обязательное значение пусто. `.env.example` не является
готовой production-конфигурацией: пароль БД там пуст намеренно.

Исправление:

```bash
cd deploy
./install.sh --mode docker --url http://10.20.30.40:8080 --port 8080
```

Установщик создаст `.env` с правами `0600` и случайными паролями.

### Том PostgreSQL остался, а `.env` потерян

Симптомы:

- установщик сообщает, что том существует без `.env`;
- приложение циклически перезапускается;
- PostgreSQL пишет `password authentication failed`.

Пароль задаётся при первом создании тома. Новый `.env` не меняет пароль внутри
существующей БД.

Без потери данных исправление одно: восстановить прежний `.env` из резервной
копии. Удаление тома создаст пустую БД и потеряет подключения, задания и
историю. Копии ВМ во внешнем хранилище останутся, но их придётся повторно
обнаружить и привязать.

### Порт уже занят

```bash
sudo ss -ltnp | grep ':8080 '
```

Освободите порт или выберите другой:

```bash
./install.sh --mode docker \
  --url http://10.20.30.40:18080 --port 18080
```

За обратным прокси внешний URL может остаться без внутреннего порта:
`--url https://virt.example.org --port 18080`.

### Контейнер приложения `unhealthy`

```bash
docker compose ps
docker inspect --format '{{json .State.Health}}' \
  "$(docker compose ps -q justhpc-virt-manager)"
docker compose logs --tail 150 justhpc-virt-manager
```

Если PostgreSQL ещё запускается, дождитесь её `healthy`. Если приложение
пишет об ошибке подключения, сверяйте пароль только с восстановленным `.env`,
не меняйте его наугад.

### Compose v2 предупреждает, что `version` устарел

Это предупреждение, не ошибка. Поле `version: "2.4"` сохранено ради
поддерживаемого `docker-compose` v1 и игнорируется Compose v2.

## 4. Systemd и PostgreSQL

### `jhvirt.service` сразу завершилась

```bash
sudo systemctl status jhvirt --no-pager
sudo journalctl -u jhvirt -b -n 200 --no-pager
sudo systemctl cat jhvirt
```

Проверьте, что пути в `WorkingDirectory`, `ExecStart` и `EnvironmentFile`
соответствуют фактическому `PREFIX`. Повторная установка из текущего `.run`
перерендерит unit без удаления данных.

### `/healthz` работает, `/readyz` не работает

Процесс запущен, но не готов. Чаще всего недоступна PostgreSQL или не прошли
миграции:

```bash
sudo journalctl -u jhvirt -n 200 --no-pager
sudo -u postgres psql -d jhvirt -c 'select 1'
sudo -u jhvirt psql -d jhvirt -c 'select 1'
```

Вторая команда проверяет сервер БД, третья — тот же peer-путь, которым
пользуется локальная systemd-установка.

### Внешняя PostgreSQL недоступна

Проверьте DNS, маршрут, порт, `pg_hba.conf`, TLS и DSN:

```bash
getent hosts db.example.org
nc -vz db.example.org 5432
```

Не выводите DSN в общий журнал. Для `sslmode=require` сервер должен реально
поддерживать TLS. Для проверки сертификата используйте `verify-full` и
настройки PostgreSQL-клиента вашей организации.

### `permission denied` при записи копии или восстановлении

Проверьте все уровни:

1. каталог существует и доступен пользователю `jhvirt`;
2. каталог восстановления входит в `backup.restore_dirs`;
3. путь добавлен в `ReadWritePaths` unit;
4. для NFS разрешена запись с фактическим UID/GID;
5. SELinux/AppArmor не запрещает доступ.

```bash
sudo -u jhvirt test -w /srv/restores && echo writable
sudo systemctl cat jhvirt | grep ReadWritePaths
```

После правки unit:

```bash
sudo systemctl daemon-reload
sudo systemctl restart jhvirt
```

### Служба была остановлена до обновления и не запустилась

Это штатно: обновление перезапускает только ранее активную службу.

```bash
sudo systemctl enable --now jhvirt
```

## 5. Авторизация

### Неверный логин или пароль (`401 unauthorized`)

Проверьте отсутствие пробелов и переносов при копировании. Bootstrap-пароль
печатается отдельной строкой и после первого старта не хранится в открытом
виде.

Сброс для systemd:

```bash
sudo systemd-run --quiet --wait --pipe --collect \
  --uid=jhvirt --gid=jhvirt --working-directory=/opt/jhvirt \
  --property=EnvironmentFile=/opt/jhvirt/config/jhvirt.env \
  /opt/jhvirt/bin/justhpc-virt-server -reset-password admin
```

Сброс для Docker из рабочего Compose-каталога:

```bash
docker compose run --rm justhpc-virt-manager -reset-password admin
```

Команда генерирует новый пароль, включает отключённую учётную запись и удаляет
все её действующие сессии. Свой пароль длиной не менее 10 символов можно задать
одноразовой переменной `JHV_NEW_PASSWORD`; учитывайте риск попадания значения в
историю оболочки и окружение процесса.

### Слишком много попыток (`429 too_many_attempts`)

Ответ содержит `Retry-After`. Пауза начинается после серии неудачных попыток и
растёт до 15 минут. Она считается по имени пользователя, а не по IP.

Предпочтительно дождаться указанного времени и проверить аудит. Для аварийного
снятия паузы перезапустите процесс, потому что счётчик хранится в памяти:

```bash
sudo systemctl restart jhvirt
# или
docker compose restart justhpc-virt-manager
```

Сброс пароля отдельной командой сам по себе счётчик работающего процесса не
обнуляет.

### Пароль принят, но снова появляется форма входа

Это исторически встречавшаяся ошибка конфигурации. Типичная последовательность:

1. `POST /api/v1/auth/login` возвращает `200`;
2. ответ содержит `Set-Cookie: jhvirt_session=...; Secure`;
3. интерфейс фактически открыт по `http://`;
4. браузер не отправляет Secure-cookie по HTTP;
5. `GET /api/v1/auth/me` возвращает `401`, SPA снова показывает login.

Диагностика:

```bash
BASE=http://10.20.30.40:8080
read -rsp 'Пароль admin: ' JHV_PASSWORD; echo
curl -i -c /tmp/jhv.cookies \
  -X POST "$BASE/api/v1/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"admin\",\"password\":\"$JHV_PASSWORD\"}"
unset JHV_PASSWORD
curl -i -b /tmp/jhv.cookies "$BASE/api/v1/auth/me"
rm -f /tmp/jhv.cookies
```

Если первый ответ `200`, содержит `Secure`, а второй `401`, схема
`external_url` не совпадает с адресом браузера.

Проверить значение:

```bash
# systemd
sudo grep '^JHV_SERVER_EXTERNAL_URL=' /opt/jhvirt/config/jhvirt.env

# Docker
grep '^JHV_EXTERNAL_URL=' .env
```

Исправить безопаснее повторным запуском установщика, чтобы одновременно
обновить окружение и перезапустить нужный процесс:

```bash
# прямой HTTP, systemd
sudo sh /tmp/jhvirt-*.run --mode systemd \
  --url http://10.20.30.40:8080 --port 8080

# прямой HTTP, Docker
./install.sh --mode docker \
  --url http://10.20.30.40:8080 --port 8080
```

Для реального HTTPS, включая TLS на обратном прокси, URL должен начинаться с
`https://`. После исправления очистите cookie сайта или откройте новую
приватную вкладку и войдите снова.

Обратное несоответствие тоже неправильно: если браузер работает по HTTPS, а
`external_url` задан как HTTP, вход будет работать, но cookie уйдёт без флага
`Secure` и сможет передаваться по открытому каналу.

### Вход работает через curl, но не через браузер

Проверьте в DevTools браузера:

- запрос login и его `Set-Cookie`;
- наличие `jhvirt_session` в Storage/Application → Cookies;
- запрос `/api/v1/auth/me` и переданный заголовок `Cookie`;
- ошибки CORS и mixed content;
- совпадение схемы и хоста страницы с production URL.

SPA и API должны раздаваться с одного origin. `server.cors_origins` оставляйте
пустым, если интерфейс обслуживает само приложение. Не добавляйте `*` для
запросов с cookie.

### После смены пароля старые вкладки получили `401`

Это ожидаемо: сброс пароля закрывает все сессии пользователя. Войдите заново.

## 6. Обратный прокси и TLS

### За nginx нет живых обновлений или долгие операции обрываются

Нужны:

```nginx
proxy_buffering off;
proxy_read_timeout 24h;
client_max_body_size 0;
```

Проверьте также передачу `Host`, `X-Forwarded-For` и `X-Forwarded-Proto`.

### Браузер сообщает mixed content

Страница открыта по HTTPS, но один из URL ссылается на HTTP. Задайте
`external_url=https://...`, не публикуйте отдельный HTTP API для SPA и
проверьте конфигурацию прокси.

### Циклический redirect

TLS завершается на прокси, но прокси и приложение по-разному определяют схему.
Не перенаправляйте внутренний `proxy_pass http://127.0.0.1:8080` на HTTPS.
Внешний URL при этом должен оставаться `https://...`.

## 7. Подключение к oVirt и KVM

### oVirt: `engine_auth`

Движок отверг учётные данные. Проверьте имя в полном формате, например
`admin@internal`, пароль, срок действия и права роли. Адрес задавайте без
`/ovirt-engine/api`: клиент добавляет API-путь сам.

### Ошибка сертификата oVirt

Не отключайте проверку TLS как постоянное решение. Загрузите CA через форму,
сверьте отпечаток независимым каналом и сохраните подключение с проверенным
сертификатом. Проверьте имя хоста: сертификат должен соответствовать адресу.

### `ovirt-imageio` недоступен

Проверьте порт 54322 каждого гипервизора с сервера приложения. Если сеть
хранения изолирована, включите:

```yaml
backup:
  transfer:
    prefer_proxy: true
```

Тогда данные пойдут через прокси движка на 54323, обычно медленнее.

### KVM: TCP 22 открыт, но SSH зависает на banner

TCP connect не доказывает работоспособность SSH. Проверьте:

```bash
ssh -vvv -o ConnectTimeout=15 user@kvm-host true
```

Если таймаут происходит до строки с версией SSH-сервера, приложение также не
подключится. Проверяйте `sshd`, firewall/LB, fail2ban, лимиты `MaxStartups` и
маршрут. Пароль и ключ ещё не участвуют в обмене, пока banner не получен.

### KVM: доступ запрещён к libvirt socket

На гипервизоре под тем же SSH-пользователем:

```bash
virsh -c qemu:///system list --all
```

Настройте членство в группе libvirt или polkit согласно политике системы. Не
выдавайте полный root только для обхода одной ошибки без оценки риска.

## 8. Бэкапы и инкременты

### `диски заблокированы` после неуспешного бэкапа

Проверьте открытые операции Backup API и завершите зависшую операцию средствами
движка. Не запускайте новый бэкап поверх незакрытого: он может использовать
неправильный checkpoint.

### Бэкап стал полным вместо инкрементального

Это может быть защитным fallback, а не ошибка. Проверьте причину в карточке
запуска:

- отсутствует родительская точка;
- checkpoint удалён или не совпадает;
- диск raw и не поддерживает persistent dirty bitmap;
- изменился состав дисков;
- политика задания разрешает полный fallback.

Не переключайте fallback на `fail`, пока не устранена причина: это превращает
полную, но более дорогую копию в отсутствие копии.

### Инкременты недоступны

Для KVM нужны qcow2, libvirt с Backup API и рабочие persistent dirty bitmap.
Для oVirt нужны поддерживаемая версия движка и режим incremental у дисков.
Raw-диски копируются полностью.

### Временный snapshot не удалился

Найдите snapshot с описанием `jhvirt backup <run-id>` и удалите его через
движок после проверки, что операция завершена. Оставленный snapshot растит
цепочку образов и расходует место storage domain.

### Задание отвечает `409 job_busy`

Предыдущий запуск того же задания ещё выполняется. Второй экземпляр намеренно
не стартует. Проверьте скорость хранилища, превратился ли инкремент в полный,
расписание и `max_duration`. При необходимости отмените текущий запуск.

### Хранилище недоступно

В интерфейсе нажмите **Проверить**: операция делает запись, чтение, сверку и
удаление пробного объекта. Затем проверяйте DNS, сеть, учётные данные, права,
свободное место и квоту целевого S3/SFTP/NFS.

## 9. Проверка `boot` на KVM

### `KVM-хост не поддерживает профиль`

Целевой хост не подтвердил сочетание архитектуры и machine type через
`ConnectGetDomainCapabilities`. Проверьте:

- архитектура хоста совпадает с гостем;
- установлен QEMU нужной архитектуры;
- для UEFI установлен OVMF/AAVMF;
- для Secure Boot доступна прошивка с enrolled keys;
- machine family `pc`, `q35` или `virt` доступна на хосте.

### `/dev/kvm` отсутствует

Проверка требует аппаратной виртуализации. Убедитесь, что она включена в
BIOS/UEFI хоста и модуль KVM загружен:

```bash
test -c /dev/kvm && echo ok
lsmod | grep '^kvm'
```

Docker Desktop без проброса `/dev/kvm` не является реальным KVM-стендом.

### Не хватает места в scratch

Сервис заранее сравнивает свободное место с суммой занятых чанков дисков.
Освободите место или измените scratch-каталог в подключении KVM. Не размещайте
scratch на системном разделе без запаса.

### ВМ запустилась, но guest agent не ответил

Проверочная ВМ создаётся без сети. Ответ должен прийти по virtio channel.
Проверьте в исходном госте:

```bash
systemctl status qemu-guest-agent
```

Агент должен быть установлен, включён и ожидать канал
`org.qemu.guest_agent.0`. Без агента результат не доказывает, что ОС не
загрузилась; он означает, что автоматического подтверждения нет.

### Проверочная ВМ выключилась или аварийно завершилась

Смотрите консоль и журнал libvirt/QEMU на гипервизоре. Частые причины:
несовместимая прошивка, отсутствующий загрузчик, неправильный boot order,
драйвер дисковой шины или повреждённая цепочка. Временно включайте
`keep_on_failure` только для диагностики и затем вручную удаляйте transient-ВМ
и все `jhv-verify-*` образы.

### Почему у проверочной ВМ нет сети

Это обязательная защита, а не ошибка. Восстановленная production-ВМ может
занять существующий IP, зарегистрироваться в каталогах и начать писать во
внешние системы. Успех подтверждается guest agent, сеть для этого не нужна.

## 10. Восстановление

### `каталог не разрешён`

Добавьте путь в `backup.restore_dirs`. Для systemd также добавьте его в
`ReadWritePaths`; для Docker смонтируйте каталог в контейнер и перечислите в
`JHV_RESTORE_DIRS`. Перезапустите приложение.

### `permission denied` при записи файла

Проверьте права пользователя/контейнера, NFS root squash, SELinux/AppArmor,
mount в Docker и systemd sandbox. Не запускайте приложение от root как
постоянное исправление.

### Цепочка повреждена или отсутствует родитель

Нельзя восстановить инкремент без всех родителей до полной точки. Проверьте
архив независимо от БД:

```bash
jvbackup list -repo /srv/backups
jvbackup verify -repo /srv/backups -run <run-id>
```

Восстанавливайте другую целую точку. Не редактируйте `run.json` и манифесты
вручную: это скроет симптом, но не вернёт отсутствующие данные.

### `qemu-img` не найден

Он нужен только для экспорта qcow2 и проверки `qemu`. На Debian/Ubuntu
установите `qemu-utils`. На RHEL-подобной системе сначала найдите пакет для
конкретной версии дистрибутива:

```bash
dnf provides '*/qemu-img'
```

Затем установите найденный пакет или укажите `backup.qemu_img_path`.

## 11. Ключ шифрования и потеря сервиса

### `secret.key` потерян

Восстановить его математически невозможно. Без исходного ключа нельзя
расшифровать сохранённые пароли подключений и зашифрованные копии. Не создавайте
новый ключ поверх старой установки как «исправление»: он не подходит к уже
зашифрованным данным.

Восстановите точную резервную копию `secret.key` с правами `0600` и владельцем
службы, затем перезапустите приложение.

### База потеряна, но копии остались

Формат хранилища самодостаточен. Используйте `jvbackup list`, `verify` и
`restore`. Для шифрованных копий передайте сохранённый ключ. После
восстановления сервиса заново создайте подключения и задания.

## 12. Что приложить к обращению

Передавайте только необходимое и удаляйте секреты:

- версия: `justhpc-virt-server -version` или `sh jhvirt-*.run --version`;
- ОС и архитектура: `cat /etc/os-release`, `uname -m`;
- способ установки и фактический внешний URL без паролей;
- ответы `/healthz` и `/readyz`;
- `systemctl status`/`docker compose ps`;
- релевантный фрагмент журнала вокруг ошибки;
- код и текст HTTP-ошибки;
- для KVM: версия libvirt/QEMU, архитектура гостя, BIOS/UEFI и machine type.

Не прикладывайте `.env`, `jhvirt.env`, DSN, приватные SSH-ключи,
`secret.key`, cookie `jhvirt_session` и полные URL S3 с учётными данными.
