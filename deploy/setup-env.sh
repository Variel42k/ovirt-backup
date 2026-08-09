#!/bin/sh
# Готовит deploy/.env для боевого запуска.
#
# Нужен потому, что просто скопировать .env.example недостаточно: пароль базы
# там пуст, а compose считает пустое значение таким же отсутствующим, как
# незаданное, и отказывается запускаться. Держать в примере готовый пароль
# нельзя — он оказался бы в репозитории и в каждой установке одинаковым.
#
# Пароль генерируется здесь: это внутренний секрет, наружу он не публикуется, и
# человеком был бы придуман хуже. Внешний адрес скрипт спрашивает — из него
# выводится флаг Secure у куки сессии, и молча подставленный localhost означал
# бы куку без Secure в установке за обратным прокси.
#
# Использование:
#   ./setup-env.sh                          спросит внешний адрес
#   ./setup-env.sh https://virt.example.org без вопросов
#   ./setup-env.sh --force …                перезаписать существующий .env

set -eu

cd "$(dirname "$0")"

die() { printf 'ошибка: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

FORCE=0
EXTERNAL=""
while [ $# -gt 0 ]; do
    case "$1" in
        --force) FORCE=1; shift ;;
        -h|--help) sed -n '2,20p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        -*) die "неизвестный ключ: $1" ;;
        *) EXTERNAL="$1"; shift ;;
    esac
done

if [ -f .env ] && [ "$FORCE" -eq 0 ]; then
    die ".env уже существует — в нём пароль работающей базы.
Перезапись сделает данные недоступными: PostgreSQL хранит свой пароль в томе и
новый из файла не примет.

Посмотреть:   cat .env
Перезаписать: ./setup-env.sh --force  (только для чистой установки)"
fi

if [ -z "$EXTERNAL" ]; then
    HOST="$(hostname -f 2>/dev/null || hostname)"
    if [ -t 0 ]; then
        say "Внешний адрес, по которому интерфейс открывают в браузере."
        say "https здесь включает флаг Secure у куки сессии — даже если TLS"
        say "терминирует обратный прокси, а сервис слушает http."
        printf 'Адрес [https://%s:8080]: ' "$HOST"
        read -r EXTERNAL || EXTERNAL=""
    fi
    [ -n "$EXTERNAL" ] || EXTERNAL="https://$HOST:8080"
fi

# Шестнадцатеричный пароль, а не base64: годится и в форме URL, если строку
# подключения потом перепишут руками, — там / и + пришлось бы кодировать.
if command -v openssl >/dev/null 2>&1; then
    PGPASS="$(openssl rand -hex 24)"
else
    PGPASS="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
fi
[ -n "$PGPASS" ] || die "не удалось сгенерировать пароль"

umask 077
{
    printf 'COMPOSE_PROJECT_NAME=justhpc-virt-manager\n'
    printf 'POSTGRES_USER=jhvirt\n'
    printf 'POSTGRES_PASSWORD=%s\n' "$PGPASS"
    printf 'POSTGRES_DB=jhvirt\n'
    printf 'JHV_EXTERNAL_URL=%s\n' "$EXTERNAL"
    printf 'JHV_PORT=8080\n'
    printf 'JHV_ADMIN_PASSWORD=\n'
    printf 'JHV_BACKUP_DIR=./backups\n'
    printf 'JHV_RESTORE_DIR=./restores\n'
    printf 'JHV_LOG_FILE=/app/logs/jhvirt.log\n'
    printf 'TZ=%s\n' "$(cat /etc/timezone 2>/dev/null || echo Europe/Moscow)"
} > .env
umask 022
chmod 600 .env

mkdir -p backups restores

say ""
say "==> .env создан, пароль базы сгенерирован"
say "    внешний адрес: $EXTERNAL"
say ""
say "Запуск:"
say "  docker compose up -d --build      # или podman-compose up -d"
say ""
say "Остальные параметры — в .env.example с пояснениями."
