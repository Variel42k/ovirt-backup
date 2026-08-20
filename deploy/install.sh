#!/bin/sh
# Установка ovirt-backup. Один скрипт на всё.
#
# Спрашивает, чем запускать, готовит конфигурацию, запускает и печатает пароль
# администратора. Больше делать ничего не нужно.
#
# Работает и из репозитория (deploy/install.sh), и из распакованного комплекта
# .run — в комплекте лежит в корне, compose-файл рядом в compose/.
#
# Использование:
#   ./install.sh                       выбор диалогом
#   ./install.sh --mode docker         docker compose
#   ./install.sh --mode docker-compose docker-compose (старый, через дефис)
#   ./install.sh --mode systemd        бинарь, PostgreSQL и служба systemd
#   ./install.sh --url https://host    внешний адрес (обязателен без диалога)
#   ./install.sh --port 18080          порт наружу, если 8080 занят
#   ./install.sh --database-url-file /root/jhvirt.dsn  внешняя PostgreSQL
#   ./install.sh --no-start            подготовить, но не запускать
#   ./install.sh --migration-export /root/jhvirt-migration.tar.gz
#                                      создать пакет переноса на старом сервере
#   ./install.sh --migration-export /root/rehearsal.tar.gz --keep-source-running
#                                      пакет для репетиции; старый узел продолжит работу
#   ./install.sh --migrate-from /root/jhvirt-migration.tar.gz
#                                      восстановить пакет на новом сервере
#   ./install.sh --tls self-signed      создать и подключить локальный сертификат
#   ./install.sh --tls files --tls-cert-file /root/server.crt \
#                --tls-key-file /root/server.key
#                                      подключить существующую пару сертификат/ключ
#
# Вход в систему (только для docker-вариантов):
#   ./install.sh --oidc none           только по паролю (по умолчанию)
#   ./install.sh --oidc keycloak       поднять Keycloak рядом и настроить
#   ./install.sh --keycloak-port 8081  порт Keycloak наружу
#   ./install.sh --oidc external --oidc-issuer https://kc/realms/infra \
#                --oidc-client-id jhvirt --oidc-client-secret-file /root/kc.secret
#                                      подключить существующий провайдер
#   ./install.sh --uninstall           выбрать, что удалить (с терминалом)
#   ./install.sh --uninstall=systemd   снять только systemd-службу
#   ./install.sh --uninstall=docker    снять только контейнеры
#   ./install.sh --uninstall=all       снять оба варианта, данные оставить
#   ./install.sh --uninstall=all --remove-config  также удалить YAML/env
#   ./install.sh --uninstall=all --purge          удалить всё: базу, ключ, данные

set -eu

# Абсолютный путь до смены каталога: после cd относительный $0 не разрешается.
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
HERE="$(dirname "$SELF")"

PREFIX_EXPLICIT=0; [ "${PREFIX+x}" = x ] && PREFIX_EXPLICIT=1
USER_NAME_EXPLICIT=0; [ "${USER_NAME+x}" = x ] && USER_NAME_EXPLICIT=1
PREFIX="${PREFIX:-/opt/jhvirt}"
USER_NAME="${USER_NAME:-jhvirt}"
UNIT="/etc/systemd/system/jhvirt.service"
SERVER_BINARY="ovirt-backup-server"
LEGACY_SERVER_BINARY="justhpc-virt-server"
COMPOSE_SERVICE="ovirt-backup"
LEGACY_COMPOSE_SERVICE="justhpc-virt-manager"
CONFIG_NAME="ovirt-backup.yaml"
LEGACY_CONFIG_NAME="virt-manager.yaml"

MODE=""; URL=""; DATABASE_URL_FILE=""; UNINSTALL_TARGET=""; START=1; PORT=8080
URL_EXPLICIT=0; PORT_EXPLICIT=0
UNINSTALL_REMOVE_CONFIG=0; UNINSTALL_REMOVE_DATA=0
MIGRATION_ACTION=""; MIGRATION_EXPORT_FILE=""; MIGRATION_IMPORT_FILE=""
MIGRATION_TMP=""; MIGRATION_ACTIVE=0; MIGRATION_SOURCE_MODE=""
MIGRATION_SOURCE_PREFIX=""; MIGRATION_SOURCE_USER=""; MIGRATION_DATABASE_KIND=""
MIGRATION_SOURCE_UID=""; MIGRATION_SOURCE_GID=""; MIGRATION_MARKER=""; MIGRATION_RESUME=0
MIGRATION_TLS_AVAILABLE=0
MIGRATION_KEEP_SOURCE=0; MIGRATION_SOURCE_STOPPED=""; MIGRATION_EXPORT_COMMITTED=0
TLS_MODE=""; TLS_CERT_FILE=""; TLS_KEY_FILE=""
TLS_DAYS=825; TLS_MATERIAL_DIR=""; TLS_RESTART_REQUIRED=0; READY_SCHEME=http
# Внешний вход: none — только пароль, keycloak — поднять рядом, external —
# подключить существующего провайдера.
OIDC_MODE=""; OIDC_ISSUER=""; OIDC_CLIENT_ID=""; OIDC_CLIENT_SECRET_FILE=""
KEYCLOAK_PORT=8081; KEYCLOAK_REALM="jhvirt"; KEYCLOAK_URL=""
KEYCLOAK_ADMIN_USER="admin"; KEYCLOAK_ADMIN_PASSWORD=""; OIDC_CLIENT_SECRET=""
# Имена групп допуска. Пользователь, не попавший ни в одну, в систему не
# допускается: default_role остаётся пустым.
GROUP_ADMIN="virt-admins"; GROUP_OPERATOR="virt-operators"; GROUP_VIEWER="virt-readers"

die() { printf '\nошибка: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --mode) [ $# -ge 2 ] || die "--mode требует значение"; MODE="$2"; shift 2 ;;
        --mode=*) MODE="${1#--mode=}"; shift ;;
        --url) [ $# -ge 2 ] || die "--url требует значение"; URL="$2"; URL_EXPLICIT=1; shift 2 ;;
        --url=*) URL="${1#--url=}"; URL_EXPLICIT=1; shift ;;
        --port) [ $# -ge 2 ] || die "--port требует значение"; PORT="$2"; PORT_EXPLICIT=1; shift 2 ;;
        --port=*) PORT="${1#--port=}"; PORT_EXPLICIT=1; shift ;;
        --database-url-file) [ $# -ge 2 ] || die "--database-url-file требует путь"; DATABASE_URL_FILE="$2"; shift 2 ;;
        --database-url-file=*) DATABASE_URL_FILE="${1#--database-url-file=}"; shift ;;
        --no-start) START=0; shift ;;
        --migration-export) [ $# -ge 2 ] || die "--migration-export требует путь"; MIGRATION_ACTION="export"; MIGRATION_EXPORT_FILE="$2"; shift 2 ;;
        --migration-export=*) MIGRATION_ACTION="export"; MIGRATION_EXPORT_FILE="${1#--migration-export=}"; shift ;;
        --migrate-from) [ $# -ge 2 ] || die "--migrate-from требует путь"; MIGRATION_ACTION="import"; MIGRATION_IMPORT_FILE="$2"; shift 2 ;;
        --migrate-from=*) MIGRATION_ACTION="import"; MIGRATION_IMPORT_FILE="${1#--migrate-from=}"; shift ;;
        --keep-source-running) MIGRATION_KEEP_SOURCE=1; shift ;;
        --tls) [ $# -ge 2 ] || die "--tls требует none, self-signed или files"; TLS_MODE="$2"; shift 2 ;;
        --tls=*) TLS_MODE="${1#--tls=}"; shift ;;
        --self-signed) TLS_MODE=self-signed; shift ;;
        --tls-cert-file) [ $# -ge 2 ] || die "--tls-cert-file требует путь"; TLS_CERT_FILE="$2"; shift 2 ;;
        --tls-cert-file=*) TLS_CERT_FILE="${1#--tls-cert-file=}"; shift ;;
        --tls-key-file) [ $# -ge 2 ] || die "--tls-key-file требует путь"; TLS_KEY_FILE="$2"; shift 2 ;;
        --tls-key-file=*) TLS_KEY_FILE="${1#--tls-key-file=}"; shift ;;
        --tls-days) [ $# -ge 2 ] || die "--tls-days требует число дней"; TLS_DAYS="$2"; shift 2 ;;
        --tls-days=*) TLS_DAYS="${1#--tls-days=}"; shift ;;
        --oidc) [ $# -ge 2 ] || die "--oidc требует значение"; OIDC_MODE="$2"; shift 2 ;;
        --oidc=*) OIDC_MODE="${1#--oidc=}"; shift ;;
        --oidc-issuer) [ $# -ge 2 ] || die "--oidc-issuer требует значение"; OIDC_ISSUER="$2"; shift 2 ;;
        --oidc-issuer=*) OIDC_ISSUER="${1#--oidc-issuer=}"; shift ;;
        --oidc-client-id) [ $# -ge 2 ] || die "--oidc-client-id требует значение"; OIDC_CLIENT_ID="$2"; shift 2 ;;
        --oidc-client-id=*) OIDC_CLIENT_ID="${1#--oidc-client-id=}"; shift ;;
        # Секрет — файлом, а не значением: аргументы командной строки видны в
        # ps любому пользователю машины и оседают в истории оболочки.
        --oidc-client-secret-file) [ $# -ge 2 ] || die "--oidc-client-secret-file требует путь"; OIDC_CLIENT_SECRET_FILE="$2"; shift 2 ;;
        --oidc-client-secret-file=*) OIDC_CLIENT_SECRET_FILE="${1#--oidc-client-secret-file=}"; shift ;;
        --keycloak-port) [ $# -ge 2 ] || die "--keycloak-port требует значение"; KEYCLOAK_PORT="$2"; shift 2 ;;
        --keycloak-port=*) KEYCLOAK_PORT="${1#--keycloak-port=}"; shift ;;
        --keycloak-url) [ $# -ge 2 ] || die "--keycloak-url требует значение"; KEYCLOAK_URL="$2"; shift 2 ;;
        --keycloak-url=*) KEYCLOAK_URL="${1#--keycloak-url=}"; shift ;;
        --uninstall=*) MODE=uninstall; UNINSTALL_TARGET="${1#--uninstall=}"; shift ;;
        --uninstall) MODE=uninstall; shift ;;
        --remove-config) UNINSTALL_REMOVE_CONFIG=1; shift ;;
        # Отдельный ключ, а не значение --uninstall: полное удаление стирает
        # ключ шифрования, и набрать его случайно, промахнувшись по списку
        # вариантов, не выйдет — придётся написать слово целиком.
        --purge) UNINSTALL_REMOVE_CONFIG=1; UNINSTALL_REMOVE_DATA=1; shift ;;
        # Справка — это шапка файла: два описания разъезжаются, одно нет.
        # Границей служит первая строка не-комментарий, а не номер строки:
        # по номерам вывод уже захватывал лишнее при правке шапки.
        -h|--help) awk 'NR>1 && /^#/ {sub(/^# ?/, ""); print; next} NR>1 {exit}' "$SELF"; exit 0 ;;
        *) die "неизвестный ключ: $1 (см. --help)" ;;
    esac
done

# Каталог с compose-файлом: в комплекте это compose/ рядом со скриптом, в
# репозитории — сам каталог deploy/.
if [ -f "$HERE/compose/docker-compose.yml" ]; then
    COMPOSE_DIR="$HERE/compose"
    BUNDLE=1
elif [ -f "$HERE/docker-compose.yml" ]; then
    COMPOSE_DIR="$HERE"
    BUNDLE=0
else
    die "рядом со скриптом нет docker-compose.yml"
fi

# --- Что доступно -----------------------------------------------------------
#
# Проверяется настоящий Docker, а не только имя команды: пакет podman-docker
# ставит совместимый CLI с тем же именем. Этот путь намеренно не поддерживается.
is_real_docker() {
    have docker || return 1
    docker --version 2>&1 | grep -qi podman && return 1
    docker info >/dev/null 2>&1 || return 1
}

has_docker()    { is_real_docker && docker compose version >/dev/null 2>&1; }
has_dockerc()   { is_real_docker && have docker-compose; }
has_systemd()   { have systemctl && [ -d /run/systemd/system ]; }

# Имя проекта: от него зависят имена томов. Берётся из .env, если он есть, —
# иначе то же значение, что скрипт туда пишет.
project_name() {
    PN_ENV="$COMPOSE_DIR/.env"
    if [ -n "${WORK:-}" ] && [ -f "$WORK/.env" ]; then
        PN_ENV="$WORK/.env"
    elif [ -f "$PREFIX/compose/.env" ]; then
        PN_ENV="$PREFIX/compose/.env"
    fi
    if [ -f "$PN_ENV" ]; then
        v="$(grep -m1 '^COMPOSE_PROJECT_NAME=' "$PN_ENV" 2>/dev/null | cut -d= -f2-)"
        [ -n "$v" ] && { printf '%s' "$v"; return; }
    fi
    printf 'ovirt-backup'
}

valid_host_timezone() {
    case "$1" in
        ""|/*|*..*|*[!A-Za-z0-9_+./-]*) return 1 ;;
    esac
    [ "$1" = UTC ] || [ -f "/usr/share/zoneinfo/$1" ]
}

# Debian-like systems expose /etc/timezone, while RHEL-like systems normally
# only symlink /etc/localtime.  timedatectl is the last discovery mechanism:
# it may be unavailable in a minimal installer environment even on systemd.
host_timezone() {
    HTZ_VALUE="${TZ:-}"
    if valid_host_timezone "$HTZ_VALUE"; then printf '%s' "$HTZ_VALUE"; return; fi

    HTZ_VALUE=""
    if [ -r /etc/timezone ]; then IFS= read -r HTZ_VALUE < /etc/timezone || HTZ_VALUE=""; fi
    if valid_host_timezone "$HTZ_VALUE"; then printf '%s' "$HTZ_VALUE"; return; fi

    HTZ_LINK="$(readlink /etc/localtime 2>/dev/null || true)"
    case "$HTZ_LINK" in *zoneinfo/*) HTZ_VALUE="${HTZ_LINK#*zoneinfo/}" ;; esac
    if valid_host_timezone "$HTZ_VALUE"; then printf '%s' "$HTZ_VALUE"; return; fi

    if have timedatectl; then
        HTZ_VALUE="$(timedatectl show -p Timezone --value 2>/dev/null || true)"
        if valid_host_timezone "$HTZ_VALUE"; then printf '%s' "$HTZ_VALUE"; return; fi
    fi
    printf 'UTC'
}

volume_exists() {
    docker volume inspect "$1" >/dev/null 2>&1
}

# Случайная строка в шестнадцатеричном виде: годится и в URL, и в .env, где нет
# кавычек и спецсимволы вышли бы боком.
gen_secret() {
    if have openssl; then
        openssl rand -hex "$1"
    else
        head -c "$1" /dev/urandom | od -An -tx1 | tr -d '[:space:]'
    fi
}

docker_metrics_volume() {
	printf '%s_jhvirt-data' "$(project_name)"
}

ensure_docker_metrics_token() {
	VOL="$(docker_metrics_volume)"
	if ! volume_exists "$VOL"; then
		docker volume create \
			--label "com.docker.compose.project=$(project_name)" \
			--label "com.docker.compose.volume=jhvirt-data" "$VOL" >/dev/null
	fi
	TOKEN="$(gen_secret 32)"
	[ -n "$TOKEN" ] || die "не удалось сгенерировать токен Prometheus"
	printf '%s\n' "$TOKEN" | docker run --rm -i --network none --user root \
		-v "$VOL:/data" docker.io/library/postgres:17-alpine sh -c '
		if [ -s /data/metrics.token ]; then
			cat >/dev/null
		else
			umask 077
			cat > /data/metrics.token
			chown 10001:10001 /data/metrics.token
			chmod 600 /data/metrics.token
		fi
		chown 10001:10001 /data
		chmod 700 /data' || die "не удалось создать token-файл Prometheus в томе $VOL"
}

remove_docker_metrics_token() {
	VOL="$(docker_metrics_volume)"
	volume_exists "$VOL" || return 0
	docker run --rm --network none --user root -v "$VOL:/data" \
		docker.io/library/postgres:17-alpine rm -f /data/metrics.token >/dev/null 2>&1 || {
		say "    предупреждение: не удалось удалить metrics.token из тома $VOL"
		return 1
	}
	return 0
}

# Имена томов, которые заводит compose. Кандидатов несколько: имя проекта
# менялось между версиями, и установка, сделанная прежней, лежит в томах со
# старым префиксом. Удалять или чинить надо именно те, что нашлись, иначе
# «полное удаление» оставило бы данные лежать под чужим именем, а установка
# рядом завела бы пустую базу.
data_volume_candidates() {
    for PREF in "$(project_name)" jhvirt "$COMPOSE_SERVICE" "$LEGACY_COMPOSE_SERVICE"; do
        printf '%s_postgres-data\n%s_jhvirt-data\n' "$PREF" "$PREF"
    done | awk '!seen[$0]++'
}

# reset_db_password задаёт новый пароль пользователю базы прямо в уцелевшем томе.
#
# Пароль PostgreSQL хранит внутри кластера, поэтому «пароль потерян» и «данные
# потеряны» — разные беды: временный контейнер поднимает базу из того же тома и
# меняет пароль через локальный сокет, где действует доверительная
# аутентификация. Сами данные при этом не трогаются.
#
# Так закрывается тупик, в который упиралась установка поверх тома от прошлой
# установки без .env: раньше оставалось либо найти прежний файл, либо стереть
# базу вместе с подключениями, заданиями и историей.
reset_db_password() {
    RDB_VOL="$1"; RDB_PASS="$2"; RDB_USER="${3:-jhvirt}"
    RDB_NAME="jhv-pgreset-$$"

    docker rm -f "$RDB_NAME" >/dev/null 2>&1
    docker run -d --name "$RDB_NAME" --network none \
        -v "$RDB_VOL:/var/lib/postgresql/data" \
        docker.io/library/postgres:17-alpine >/dev/null 2>&1 ||
        { say "    не удалось поднять временный контейнер базы"; return 1; }

    RDB_READY=0; RDB_TRY=0
    while [ "$RDB_TRY" -lt 40 ]; do
        if docker exec "$RDB_NAME" pg_isready -U "$RDB_USER" -q 2>/dev/null; then
            RDB_READY=1; break
        fi
        RDB_TRY=$((RDB_TRY+1)); sleep 2
    done

    RDB_RC=1
    if [ "$RDB_READY" -eq 1 ]; then
        # Пароль передаётся через переменную окружения, а не в тексте команды:
        # в ps его видно всем, кто есть на машине.
        if docker exec -e RDB_PASS="$RDB_PASS" "$RDB_NAME" \
            psql -U "$RDB_USER" -d "$RDB_USER" -q -v ON_ERROR_STOP=1 \
            -c "ALTER USER \"$RDB_USER\" PASSWORD '$(printf '%s' "$RDB_PASS")';" >/dev/null 2>&1; then
            RDB_RC=0
        else
            say "    база поднялась, но сменить пароль не удалось"
        fi
    else
        say "    временная база не поднялась за 80 секунд"
    fi

    docker stop "$RDB_NAME" >/dev/null 2>&1
    docker rm "$RDB_NAME" >/dev/null 2>&1
    return "$RDB_RC"
}

set_plain_env() {
    KEY="$1"; VALUE="$2"; FILE="$3"; JHV_ENV_TMP="${TMPDIR:-/tmp}/jhvirt-env.$$"
	grep -v "^${KEY}=" "$FILE" > "$JHV_ENV_TMP" || true
	printf '%s=%s\n' "$KEY" "$VALUE" >> "$JHV_ENV_TMP"
	chmod 600 "$JHV_ENV_TMP"
	cat "$JHV_ENV_TMP" > "$FILE"
	chmod 600 "$FILE"
	rm -f "$JHV_ENV_TMP"
}

install_bundle_config() {
    SAMPLE="$HERE/config/$CONFIG_NAME"
    DEST="$PREFIX/config/$CONFIG_NAME"
    LEGACY="$PREFIX/config/$LEGACY_CONFIG_NAME"

    [ -f "$SAMPLE" ] || die "в комплекте нет config/$CONFIG_NAME"
    if [ -f "$DEST" ]; then
        cp "$SAMPLE" "$DEST.new"
        say "    конфигурация сохранена; новая версия рядом: $CONFIG_NAME.new"
    elif [ -f "$LEGACY" ]; then
        cp -p "$LEGACY" "$DEST"
        cp "$SAMPLE" "$DEST.new"
        say "    конфигурация перенесена из $LEGACY_CONFIG_NAME в $CONFIG_NAME"
        say "    новый образец рядом: $CONFIG_NAME.new"
    else
        install -m 0640 "$SAMPLE" "$DEST"
    fi
}

# Команда запуска для выбранного способа.
runner() {
    case "$1" in
        docker)         printf 'docker compose' ;;
        docker-compose) printf 'docker-compose' ;;
    esac
}

validate_install_identity() {
    case "$PREFIX" in
        /*) ;;
        *) die "PREFIX должен быть абсолютным путём: PREFIX=/opt/jhvirt" ;;
    esac
    case "$PREFIX" in
        /|/usr|/opt|/srv|/var|/home)
            die "PREFIX не может указывать на системный каталог целиком: $PREFIX" ;;
        */|*/../*|*/..|*/./*|*/.)
            die "PREFIX должен быть нормализованным путём без /./, /../ и завершающего /: $PREFIX" ;;
        *[!A-Za-z0-9_./-]*)
            die "PREFIX содержит неподдерживаемые символы: $PREFIX" ;;
    esac
    if [ -d "$PREFIX" ]; then
        RESOLVED_PREFIX="$(cd "$PREFIX" && pwd -P)"
        case "$RESOLVED_PREFIX" in
            /|/usr|/opt|/srv|/var|/home)
                die "PREFIX разрешается в системный каталог целиком: $RESOLVED_PREFIX" ;;
        esac
    fi
    case "$USER_NAME" in
        ''|-*|*[!A-Za-z0-9_-]*) die "USER_NAME должен начинаться не с '-' и содержать только A-Z, a-z, 0-9, _ и -: $USER_NAME" ;;
        root) die "USER_NAME=root запрещён: служба должна работать без прав root" ;;
    esac
}

validate_install_identity

ensure_service_user() {
    have getent && have groupadd && have useradd ||
        die "для создания системного пользователя нужны getent, groupadd и useradd"
    ESU_PRESERVE=0; ESU_GROUP_CREATED=0
    if [ "$MIGRATION_ACTIVE" -eq 1 ] && [ "$MIGRATION_SOURCE_MODE" = systemd ] &&
            [ -n "$MIGRATION_SOURCE_UID" ] && [ -n "$MIGRATION_SOURCE_GID" ]; then
        ESU_PRESERVE=1
    fi

    # Unit всегда использует одинаковые User= и Group=. Создаём группу даже
    # для уже существующего локального пользователя, иначе systemd отвергнет
    # корректный UID из-за отсутствующего имени группы.
    if ! getent group "$USER_NAME" >/dev/null 2>&1; then
        if [ "$ESU_PRESERVE" -eq 1 ] && ! getent group "$MIGRATION_SOURCE_GID" >/dev/null 2>&1; then
            groupadd --system --gid "$MIGRATION_SOURCE_GID" "$USER_NAME" ||
                die "не удалось создать группу $USER_NAME с GID $MIGRATION_SOURCE_GID"
        else
            groupadd --system "$USER_NAME" || die "не удалось создать группу $USER_NAME"
        fi
        ESU_GROUP_CREATED=1
    fi

    if ! id "$USER_NAME" >/dev/null 2>&1; then
        if [ "$ESU_PRESERVE" -eq 1 ] && ! getent passwd "$MIGRATION_SOURCE_UID" >/dev/null 2>&1; then
            if ! { useradd --system --uid "$MIGRATION_SOURCE_UID" --gid "$USER_NAME" \
                        --home-dir "$PREFIX" --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null ||
                    useradd --system --uid "$MIGRATION_SOURCE_UID" --gid "$USER_NAME" \
                        --home-dir "$PREFIX" --shell /sbin/nologin "$USER_NAME" 2>/dev/null; }; then
                [ "$ESU_GROUP_CREATED" -eq 0 ] || groupdel "$USER_NAME" >/dev/null 2>&1 || true
                die "не удалось создать пользователя $USER_NAME с UID $MIGRATION_SOURCE_UID"
            fi
        elif ! { useradd --system --gid "$USER_NAME" --home-dir "$PREFIX" \
                    --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null ||
                useradd --system --gid "$USER_NAME" --home-dir "$PREFIX" \
                    --shell /sbin/nologin "$USER_NAME" 2>/dev/null; }; then
            [ "$ESU_GROUP_CREATED" -eq 0 ] || groupdel "$USER_NAME" >/dev/null 2>&1 || true
            die "не удалось создать пользователя $USER_NAME"
        fi
    fi

    if [ "$ESU_PRESERVE" -eq 1 ]; then
        ESU_ACTUAL_UID="$(id -u "$USER_NAME")"
        ESU_ACTUAL_GID="$(getent group "$USER_NAME" | awk -F: '{print $3}')"
        if [ "$ESU_ACTUAL_UID:$ESU_ACTUAL_GID" = "$MIGRATION_SOURCE_UID:$MIGRATION_SOURCE_GID" ]; then
            say "    права службы сохранены: UID:GID $ESU_ACTUAL_UID:$ESU_ACTUAL_GID"
        else
            say "    предупреждение: UID:GID службы изменились: $MIGRATION_SOURCE_UID:$MIGRATION_SOURCE_GID -> $ESU_ACTUAL_UID:$ESU_ACTUAL_GID"
            say "    внешние NFS/локальные каталоги должны разрешать запись новым UID:GID"
        fi
    fi
}

# --- Удаление ---------------------------------------------------------------

docker_bundle_present() {
    [ -f "$PREFIX/compose/.env" ]
}

systemd_install_present() {
    [ -f "$UNIT" ] && return 0
    if have systemctl; then
        systemctl list-unit-files jhvirt.service --no-legend 2>/dev/null |
            grep -q '^jhvirt\.service' && return 0
    fi
    return 1
}

# --- Перенос на другой сервер ----------------------------------------------

migration_cleanup() {
    if [ -n "${MIGRATION_SOURCE_STOPPED:-}" ] && [ "${MIGRATION_EXPORT_COMMITTED:-0}" -eq 0 ]; then
        migration_resume_source || true
    fi
    [ -n "${MIGRATION_TMP:-}" ] && [ -d "$MIGRATION_TMP" ] &&
        rm -rf "$MIGRATION_TMP"
    [ -n "${TLS_MATERIAL_DIR:-}" ] && [ -d "$TLS_MATERIAL_DIR" ] &&
        rm -rf "$TLS_MATERIAL_DIR"
}

migration_quiesce_source() {
    MIGRATION_SOURCE_STOPPED=""
    case "$MODE" in
        docker|docker-compose)
            MQS_WORK="$(migration_docker_work)"; MQS_RUN="$(runner "$MODE")"
            for MQS_SERVICE in "$COMPOSE_SERVICE" keycloak; do
                # shellcheck disable=SC2086
                MQS_ID="$(cd "$MQS_WORK" && $MQS_RUN ps -q "$MQS_SERVICE" 2>/dev/null || true)"
                [ -n "$MQS_ID" ] || continue
                if [ "$(docker inspect -f '{{.State.Running}}' "$MQS_ID" 2>/dev/null || true)" = true ]; then
                    # shellcheck disable=SC2086
                    (cd "$MQS_WORK" && $MQS_RUN stop "$MQS_SERVICE") >/dev/null ||
                        die "не удалось остановить $MQS_SERVICE перед dump"
                    MIGRATION_SOURCE_STOPPED="$MIGRATION_SOURCE_STOPPED $MQS_SERVICE"
                fi
            done
            ;;
        systemd)
            if systemctl is-active --quiet jhvirt 2>/dev/null; then
                systemctl stop jhvirt
                MIGRATION_SOURCE_STOPPED=systemd
            fi
            ;;
    esac
}

migration_resume_source() {
    [ -n "${MIGRATION_SOURCE_STOPPED:-}" ] || return 0
    case "$MODE" in
        docker|docker-compose)
            MRS_WORK="$(migration_docker_work)"; MRS_RUN="$(runner "$MODE")"
            for MRS_SERVICE in $MIGRATION_SOURCE_STOPPED; do
                # shellcheck disable=SC2086
                (cd "$MRS_WORK" && $MRS_RUN start "$MRS_SERVICE") >/dev/null || return 1
            done
            ;;
        systemd) systemctl start jhvirt || return 1 ;;
    esac
    MIGRATION_SOURCE_STOPPED=""
}

migration_ask_nonempty() {
    ANSWER=""
    while [ -z "$ANSWER" ]; do
        printf '%s' "$1"
        read -r ANSWER || ANSWER=""
    done
}

env_file_value() {
    EF_FILE="$1"; EF_KEY="$2"
    [ -f "$EF_FILE" ] || return 0
    EF_VALUE="$(sed -n "s/^${EF_KEY}=//p" "$EF_FILE" | tail -n 1)"
    case "$EF_VALUE" in
        \"*\") EF_VALUE="${EF_VALUE#\"}"; EF_VALUE="${EF_VALUE%\"}" ;;
    esac
    printf '%s' "$EF_VALUE"
}

yaml_server_tls_value() {
    YTV_FILE="$1"; YTV_KEY="$2"
    awk -v wanted="$YTV_KEY" '
        /^server:[[:space:]]*$/ { in_server=1; next }
        in_server && /^[^[:space:]]/ { in_server=0; in_tls=0 }
        in_server && /^  tls:[[:space:]]*$/ { in_tls=1; next }
        in_tls && /^  [^[:space:]]/ { in_tls=0 }
        in_tls && $0 ~ "^    " wanted ":[[:space:]]*" {
            value=$0
            sub("^    " wanted ":[[:space:]]*", "", value)
            gsub(/^\"|\"$/, "", value)
            print value
            exit
        }
    ' "$YTV_FILE"
}

rewrite_prefix_file() {
    RPF_FILE="$1"; RPF_OLD="$2"; RPF_NEW="$3"
    [ -f "$RPF_FILE" ] || return 0
    [ -n "$RPF_OLD" ] && [ "$RPF_OLD" != "$RPF_NEW" ] || return 0
    awk -v old="$RPF_OLD" -v new="$RPF_NEW" '
        {
            line = $0
            while ((pos = index(line, old)) != 0) {
                line = substr(line, 1, pos - 1) new substr(line, pos + length(old))
            }
            print line
        }
    ' "$RPF_FILE" > "$RPF_FILE.tmp.$$"
    cat "$RPF_FILE.tmp.$$" > "$RPF_FILE"
    rm -f "$RPF_FILE.tmp.$$"
}

migration_docker_work() {
    if [ -f "$PREFIX/compose/.env" ] && [ -f "$PREFIX/compose/docker-compose.yml" ]; then
        printf '%s' "$PREFIX/compose"
    elif [ -f "$COMPOSE_DIR/.env" ]; then
        printf '%s' "$COMPOSE_DIR"
    else
        return 1
    fi
}

migration_detect_mode() {
    MD_DOCKER=0; MD_SYSTEMD=0
    migration_docker_work >/dev/null 2>&1 && MD_DOCKER=1
    systemd_install_present && MD_SYSTEMD=1

    case "$MODE" in
        docker|docker-compose)
            [ "$MD_DOCKER" -eq 1 ] || die "контейнерная установка для экспорта не найдена"
            ;;
        systemd)
            [ "$MD_SYSTEMD" -eq 1 ] || die "systemd-установка для экспорта не найдена"
            ;;
        "")
            if [ "$MD_DOCKER" -eq 1 ] && [ "$MD_SYSTEMD" -eq 0 ]; then
                if has_docker; then MODE=docker
                elif has_dockerc; then MODE=docker-compose
                else die "Docker Compose недоступен для экспорта"
                fi
            elif [ "$MD_SYSTEMD" -eq 1 ] && [ "$MD_DOCKER" -eq 0 ]; then
                MODE=systemd
            elif [ "$MD_DOCKER" -eq 1 ] && [ "$MD_SYSTEMD" -eq 1 ] && [ -t 0 ]; then
                say ""
                say "На сервере найдены две установки. Какую переносить?"
                say "  1) Docker Compose"
                say "  2) systemd"
                while :; do
                    printf 'Номер [1]: '
                    read -r MD_CHOICE || MD_CHOICE=""
                    [ -n "$MD_CHOICE" ] || MD_CHOICE=1
                    case "$MD_CHOICE" in
                        1)
                            if has_docker; then MODE=docker
                            elif has_dockerc; then MODE=docker-compose
                            else die "Docker Compose недоступен для экспорта"
                            fi
                            break
                            ;;
                        2) MODE=systemd; break ;;
                        *) say "Нет такого варианта." ;;
                    esac
                done
            elif [ "$MD_DOCKER" -eq 1 ] && [ "$MD_SYSTEMD" -eq 1 ]; then
                die "найдены Docker и systemd; укажите экспортируемую установку через --mode"
            else
                die "установка для переноса не найдена в $PREFIX"
            fi
            ;;
        *) die "--migration-export принимает --mode docker|docker-compose|systemd" ;;
    esac
}

migration_detect_source_identity() {
    case "$MODE" in
        systemd)
            if [ "$PREFIX_EXPLICIT" -eq 0 ] && [ -f "$UNIT" ]; then
                MDSI_PREFIX="$(sed -n 's/^WorkingDirectory=//p' "$UNIT" | head -n 1)"
                [ -z "$MDSI_PREFIX" ] || PREFIX="$MDSI_PREFIX"
            fi
            if [ "$USER_NAME_EXPLICIT" -eq 0 ] && [ -f "$UNIT" ]; then
                MDSI_USER="$(sed -n 's/^User=//p' "$UNIT" | head -n 1)"
                [ -z "$MDSI_USER" ] || USER_NAME="$MDSI_USER"
            fi
            ;;
        docker|docker-compose)
            # Для bundle владельцем каталога является системный пользователь,
            # созданный установщиком. В режиме из репозитория файлы нередко
            # принадлежат root; это не означает, что на новом узле приложение
            # следует запускать от root, поэтому такого владельца игнорируем.
            MDSI_WORK="$(migration_docker_work)"
            if [ "$USER_NAME_EXPLICIT" -eq 0 ]; then
                MDSI_USER="$(stat -c '%U' "$MDSI_WORK/.env" 2>/dev/null || true)"
                if [ -n "$MDSI_USER" ] && [ "$MDSI_USER" != root ] &&
                        [ "$MDSI_USER" != UNKNOWN ] && id "$MDSI_USER" >/dev/null 2>&1; then
                    USER_NAME="$MDSI_USER"
                fi
            fi
            ;;
    esac
    validate_install_identity
}

migration_volume_file() {
    MV_VOLUME="$1"; MV_SOURCE="$2"; MV_DEST="$3"; MV_REQUIRED="$4"
    if docker run --rm --network none --user root -v "$MV_VOLUME:/data:ro" \
            docker.io/library/postgres:17-alpine \
            sh -c "test -f '/data/$MV_SOURCE' && cat '/data/$MV_SOURCE'" \
            > "$MV_DEST.tmp" 2>/dev/null; then
        mv "$MV_DEST.tmp" "$MV_DEST"
        return 0
    fi
    rm -f "$MV_DEST.tmp"
    [ "$MV_REQUIRED" -eq 0 ] && return 0
    die "в томе $MV_VOLUME не найден обязательный файл $MV_SOURCE"
}

migration_write_manifest() {
    MWM_DIR="$1"; MWM_MODE="$2"; MWM_PREFIX="$3"; MWM_DB="$4"; MWM_KEYCLOAK="$5"
    {
        printf 'format=jhvirt-migration-v1\n'
        printf 'source_mode=%s\n' "$MWM_MODE"
        printf 'source_prefix=%s\n' "$MWM_PREFIX"
        printf 'source_user=%s\n' "$USER_NAME"
        printf 'source_uid=%s\n' "$(id -u "$USER_NAME" 2>/dev/null || true)"
        printf 'source_gid=%s\n' "$(id -g "$USER_NAME" 2>/dev/null || true)"
        printf 'database_kind=%s\n' "$MWM_DB"
        printf 'keycloak=%s\n' "$MWM_KEYCLOAK"
        printf 'created_at=%s\n' "$(date -u '+%Y-%m-%dT%H:%M:%SZ')"
    } > "$MWM_DIR/manifest"
}

migration_export_docker() {
    ME_DIR="$1"; ME_WORK="$(migration_docker_work)"
    ME_RUN="$(runner "$MODE")"
    cp "$ME_WORK/.env" "$ME_DIR/environment/docker.env"

    ME_CONFIG_REF="$(env_file_value "$ME_WORK/.env" JHV_CONFIG_FILE)"
    [ -n "$ME_CONFIG_REF" ] || ME_CONFIG_REF="../config/$CONFIG_NAME"
    case "$ME_CONFIG_REF" in
        /*) ME_CONFIG_PATH="$ME_CONFIG_REF" ;;
        *) ME_CONFIG_PATH="$ME_WORK/$ME_CONFIG_REF" ;;
    esac
    [ -f "$ME_CONFIG_PATH" ] || die "не найден активный YAML: $ME_CONFIG_PATH"
    cp "$ME_CONFIG_PATH" "$ME_DIR/config/$CONFIG_NAME"

    ME_PROJECT="$(env_file_value "$ME_WORK/.env" COMPOSE_PROJECT_NAME)"
    [ -n "$ME_PROJECT" ] || ME_PROJECT=ovirt-backup
    ME_DATA_VOLUME="${ME_PROJECT}_jhvirt-data"
    volume_exists "$ME_DATA_VOLUME" || die "не найден том с ключом: $ME_DATA_VOLUME"
    migration_volume_file "$ME_DATA_VOLUME" secret.key "$ME_DIR/data/secret.key" 1
    migration_volume_file "$ME_DATA_VOLUME" metrics.token "$ME_DIR/data/metrics.token" 0
    migration_volume_file "$ME_DATA_VOLUME" tls/server.crt "$ME_DIR/tls/server.crt" 0
    migration_volume_file "$ME_DATA_VOLUME" tls/server.key "$ME_DIR/tls/server.key" 0
    if [ "$(env_file_value "$ME_WORK/.env" JHV_TLS_ENABLED)" = true ]; then
        [ -s "$ME_DIR/tls/server.crt" ] && [ -s "$ME_DIR/tls/server.key" ] ||
            die "TLS включён, но в томе $ME_DATA_VOLUME нет сертификата или ключа"
    fi

    ME_DATABASE_URL="$(env_file_value "$ME_WORK/.env" JHV_DATABASE_URL)"
    ME_KEYCLOAK=0
    case "$(env_file_value "$ME_WORK/.env" COMPOSE_PROFILES)" in
        *keycloak*) ME_KEYCLOAK=1 ;;
    esac
    if [ -n "$ME_DATABASE_URL" ]; then
        ME_DATABASE_KIND=external
        say "    внешняя PostgreSQL останется на месте; её DSN сохранён в пакете"
    else
        ME_DATABASE_KIND=embedded
        ME_PG_USER="$(env_file_value "$ME_WORK/.env" POSTGRES_USER)"; [ -n "$ME_PG_USER" ] || ME_PG_USER=jhvirt
        ME_PG_DB="$(env_file_value "$ME_WORK/.env" POSTGRES_DB)"; [ -n "$ME_PG_DB" ] || ME_PG_DB=jhvirt
        step "согласованный dump PostgreSQL"
        # shellcheck disable=SC2086
        (cd "$ME_WORK" && $ME_RUN exec -T postgres \
            pg_dump -U "$ME_PG_USER" -d "$ME_PG_DB" -Fc --no-owner --no-privileges) \
            > "$ME_DIR/database/jhvirt.dump" ||
            die "не удалось снять dump; контейнер postgres должен быть запущен"
        [ -s "$ME_DIR/database/jhvirt.dump" ] || die "получен пустой dump PostgreSQL"
        if [ "$ME_KEYCLOAK" -eq 1 ]; then
            ME_KC_DB="$(env_file_value "$ME_WORK/.env" KEYCLOAK_DB)"; [ -n "$ME_KC_DB" ] || ME_KC_DB=keycloak
            # shellcheck disable=SC2086
            (cd "$ME_WORK" && $ME_RUN exec -T postgres \
                pg_dump -U "$ME_PG_USER" -d "$ME_KC_DB" -Fc --no-owner --no-privileges) \
                > "$ME_DIR/database/keycloak.dump" ||
                die "не удалось снять dump базы Keycloak"
        fi
    fi

    case "$ME_WORK" in
        "$PREFIX/compose") ME_SOURCE_PREFIX="$PREFIX" ;;
        *) ME_SOURCE_PREFIX="$(cd "$ME_WORK/.." && pwd -P)" ;;
    esac
    migration_write_manifest "$ME_DIR" docker "$ME_SOURCE_PREFIX" "$ME_DATABASE_KIND" "$ME_KEYCLOAK"
}

migration_export_systemd() {
    ME_DIR="$1"
    [ -f "$PREFIX/config/$CONFIG_NAME" ] || die "не найден $PREFIX/config/$CONFIG_NAME"
    [ -f "$PREFIX/config/jhvirt.env" ] || die "не найден $PREFIX/config/jhvirt.env"
    [ -s "$PREFIX/data/secret.key" ] || die "не найден ключ $PREFIX/data/secret.key"
    cp "$PREFIX/config/$CONFIG_NAME" "$ME_DIR/config/$CONFIG_NAME"
    cp "$PREFIX/config/jhvirt.env" "$ME_DIR/environment/systemd.env"
    cp "$PREFIX/data/secret.key" "$ME_DIR/data/secret.key"
    [ ! -f "$PREFIX/config/metrics.token" ] || cp "$PREFIX/config/metrics.token" "$ME_DIR/data/metrics.token"

    ME_TLS_ENABLED="$(env_file_value "$PREFIX/config/jhvirt.env" JHV_SERVER_TLS_ENABLED)"
    [ -n "$ME_TLS_ENABLED" ] || ME_TLS_ENABLED="$(yaml_server_tls_value "$PREFIX/config/$CONFIG_NAME" enabled)"
    if [ "$ME_TLS_ENABLED" = true ]; then
        ME_TLS_CERT="$(env_file_value "$PREFIX/config/jhvirt.env" JHV_SERVER_TLS_CERT_FILE)"
        ME_TLS_KEY="$(env_file_value "$PREFIX/config/jhvirt.env" JHV_SERVER_TLS_KEY_FILE)"
        [ -n "$ME_TLS_CERT" ] || ME_TLS_CERT="$(yaml_server_tls_value "$PREFIX/config/$CONFIG_NAME" cert_file)"
        [ -n "$ME_TLS_KEY" ] || ME_TLS_KEY="$(yaml_server_tls_value "$PREFIX/config/$CONFIG_NAME" key_file)"
        [ -f "$ME_TLS_CERT" ] && cp "$ME_TLS_CERT" "$ME_DIR/tls/server.crt"
        [ -f "$ME_TLS_KEY" ] && cp "$ME_TLS_KEY" "$ME_DIR/tls/server.key"
        [ -s "$ME_DIR/tls/server.crt" ] && [ -s "$ME_DIR/tls/server.key" ] ||
            die "TLS включён, но сертификат или ключ недоступны для экспорта"
    fi

    ME_DATABASE_URL="$(env_file_value "$PREFIX/config/jhvirt.env" JHV_DATABASE_URL)"
    case "$ME_DATABASE_URL" in
        *host=*|*://*)
            ME_DATABASE_KIND=external
            say "    внешняя PostgreSQL останется на месте; её DSN сохранён в пакете"
            ;;
        *)
            ME_DATABASE_KIND=embedded
            have pg_dump || die "для экспорта локальной PostgreSQL нужен pg_dump"
            step "согласованный dump PostgreSQL"
            runuser -u postgres -- pg_dump -d jhvirt -Fc --no-owner --no-privileges \
                > "$ME_DIR/database/jhvirt.dump" || die "не удалось снять dump локальной PostgreSQL"
            [ -s "$ME_DIR/database/jhvirt.dump" ] || die "получен пустой dump PostgreSQL"
            ;;
    esac
    if [ -f "$UNIT" ]; then
        sed -n 's/^ReadWritePaths=//p' "$UNIT" | tr '\n' ' ' |
            sed 's/[[:space:]]*$//' > "$ME_DIR/systemd-write-paths"
    fi
    migration_write_manifest "$ME_DIR" systemd "$PREFIX" "$ME_DATABASE_KIND" 0
}

migration_export() {
    [ -n "$MIGRATION_EXPORT_FILE" ] || die "укажите файл после --migration-export"
    [ ! -e "$MIGRATION_EXPORT_FILE" ] || die "файл уже существует: $MIGRATION_EXPORT_FILE"
    ME_PARENT="$(dirname "$MIGRATION_EXPORT_FILE")"
    [ -d "$ME_PARENT" ] && [ -w "$ME_PARENT" ] || die "каталог назначения недоступен: $ME_PARENT"
    migration_detect_mode
    migration_detect_source_identity
    [ "$MODE" != systemd ] || [ "$(id -u)" -eq 0 ] ||
        die "для экспорта systemd нужны права root: sudo $SELF --migration-export $MIGRATION_EXPORT_FILE"
    if [ -t 0 ] && [ "$MIGRATION_KEEP_SOURCE" -eq 0 ]; then
        say ""
        say "Для согласованного переноса приложение будет остановлено и останется"
        say "остановленным после создания пакета. PostgreSQL продолжит работать."
        printf 'Продолжить? [y/N]: '
        read -r ME_CONFIRM || ME_CONFIRM=""
        case "$ME_CONFIRM" in y|Y|yes|YES|да|Да|ДА) ;; *) die "экспорт отменён" ;; esac
    fi
    MIGRATION_TMP="$(mktemp -d "${TMPDIR:-/tmp}/jhvirt-migration-export.XXXXXX")" ||
        die "не удалось создать временный каталог"
    trap migration_cleanup EXIT INT TERM HUP
    mkdir -p "$MIGRATION_TMP/config" "$MIGRATION_TMP/environment" \
        "$MIGRATION_TMP/database" "$MIGRATION_TMP/data" "$MIGRATION_TMP/tls"
    chmod 700 "$MIGRATION_TMP"

    step "подготовка пакета миграции ($MODE)"
    migration_quiesce_source
    case "$MODE" in
        docker|docker-compose) migration_export_docker "$MIGRATION_TMP" ;;
        systemd) migration_export_systemd "$MIGRATION_TMP" ;;
    esac
    chmod 600 "$MIGRATION_TMP/data/"* "$MIGRATION_TMP/tls/"* \
        "$MIGRATION_TMP/environment/"* "$MIGRATION_TMP/database/"* 2>/dev/null || true
    have sha256sum || die "для контроля целостности пакета нужен sha256sum"
    (cd "$MIGRATION_TMP" && find . -type f ! -name checksums.sha256 -print | LC_ALL=C sort |
        xargs sha256sum) > "$MIGRATION_TMP/checksums.sha256" ||
        die "не удалось вычислить контрольные суммы пакета"
    chmod 600 "$MIGRATION_TMP/checksums.sha256"
    umask 077
    tar czf "$MIGRATION_EXPORT_FILE.tmp.$$" -C "$MIGRATION_TMP" . ||
        die "не удалось упаковать данные миграции"
    mv "$MIGRATION_EXPORT_FILE.tmp.$$" "$MIGRATION_EXPORT_FILE"
    chmod 600 "$MIGRATION_EXPORT_FILE"
    if [ "$MIGRATION_KEEP_SOURCE" -eq 1 ]; then
        migration_resume_source || die "пакет создан, но исходное приложение не удалось запустить снова"
    fi
    MIGRATION_EXPORT_COMMITTED=1
    say ""
    say "Пакет миграции создан: $MIGRATION_EXPORT_FILE"
    say "Права: 0600. Внутри находятся пароль БД, secret.key и настройки."
    say "Передайте его на новый сервер защищённым каналом и выполните:"
    say "  sudo sh ./ovirt-backup-*.run --migrate-from $MIGRATION_EXPORT_FILE"
    say "Локальные каталоги бекапов не копировались; подключите их на новом узле отдельно."
    if [ "$MIGRATION_KEEP_SOURCE" -eq 1 ]; then
        say "Исходное приложение снова запущено: пакет предназначен для репетиции."
    elif [ -n "$MIGRATION_SOURCE_STOPPED" ]; then
        say "Исходное приложение оставлено остановленным, чтобы состояние больше не расходилось."
    fi
}

migration_manifest_value() {
    MMV_KEY="$1"
    sed -n "s/^${MMV_KEY}=//p" "$MIGRATION_TMP/manifest" | tail -n 1
}

# Проверяет SAN/CN переносимого сертификата против нового внешнего адреса.
# В неинтерактивном cutover молча сохранить сертификат от прежнего hostname —
# значит получить формально успешную установку, которую браузеры не откроют.
migration_tls_matches_url() {
    [ -s "$MIGRATION_TMP/tls/server.crt" ] || return 1
    have openssl || return 1
    MTU_HOSTPORT="${URL#*://}"; MTU_HOSTPORT="${MTU_HOSTPORT%%/*}"
    case "$MTU_HOSTPORT" in
        \[*\]*) MTU_HOST="${MTU_HOSTPORT#\[}"; MTU_HOST="${MTU_HOST%%\]*}" ;;
        *) MTU_HOST="${MTU_HOSTPORT%%:*}" ;;
    esac
    [ -n "$MTU_HOST" ] || return 1
    case "$MTU_HOST" in
        *:*)
            openssl x509 -in "$MIGRATION_TMP/tls/server.crt" -noout -checkip "$MTU_HOST" >/dev/null 2>&1
            ;;
        *[!0-9.]*)
            openssl x509 -in "$MIGRATION_TMP/tls/server.crt" -noout -checkhost "$MTU_HOST" >/dev/null 2>&1
            ;;
        *.*)
            openssl x509 -in "$MIGRATION_TMP/tls/server.crt" -noout -checkip "$MTU_HOST" >/dev/null 2>&1
            ;;
        *)
            openssl x509 -in "$MIGRATION_TMP/tls/server.crt" -noout -checkhost "$MTU_HOST" >/dev/null 2>&1
            ;;
    esac
}

migration_validate_archive() {
    [ -f "$MIGRATION_IMPORT_FILE" ] && [ -r "$MIGRATION_IMPORT_FILE" ] ||
        die "пакет миграции недоступен: $MIGRATION_IMPORT_FILE"
    tar tzf "$MIGRATION_IMPORT_FILE" > "$MIGRATION_TMP/archive.list" ||
        die "пакет миграции повреждён или не является tar.gz"
    tar tvzf "$MIGRATION_IMPORT_FILE" > "$MIGRATION_TMP/archive.types" ||
        die "не удалось проверить типы файлов в пакете миграции"
    while IFS= read -r MVA_TYPE_LINE; do
        case "$MVA_TYPE_LINE" in
            -*|d*) ;;
            *) die "пакет миграции содержит недопустимую ссылку или special file" ;;
        esac
    done < "$MIGRATION_TMP/archive.types"
    while IFS= read -r MVA_ENTRY; do
        MVA_ENTRY="${MVA_ENTRY#./}"; MVA_ENTRY="${MVA_ENTRY%/}"
        case "$MVA_ENTRY" in
            ""|manifest|checksums.sha256|systemd-write-paths|config|config/ovirt-backup.yaml|environment|environment/docker.env|environment/systemd.env|database|database/jhvirt.dump|database/keycloak.dump|data|data/secret.key|data/metrics.token|tls|tls/server.crt|tls/server.key) ;;
            *) die "в пакете миграции найден неожиданный путь: $MVA_ENTRY" ;;
        esac
    done < "$MIGRATION_TMP/archive.list"
    tar xzf "$MIGRATION_IMPORT_FILE" -C "$MIGRATION_TMP" || die "не удалось распаковать пакет миграции"
    if find "$MIGRATION_TMP" -type l -print | grep -q .; then
        die "пакет миграции содержит символические ссылки"
    fi
    [ "$(migration_manifest_value format)" = jhvirt-migration-v1 ] ||
        die "неподдерживаемый формат пакета миграции"
    [ -s "$MIGRATION_TMP/data/secret.key" ] || die "в пакете нет secret.key"
    [ -f "$MIGRATION_TMP/manifest" ] && [ ! -L "$MIGRATION_TMP/manifest" ] ||
        die "manifest пакета имеет недопустимый тип"
    if [ -s "$MIGRATION_TMP/checksums.sha256" ]; then
        have sha256sum || die "для проверки целостности пакета нужен sha256sum"
        (cd "$MIGRATION_TMP" && sha256sum -c checksums.sha256 >/dev/null) ||
            die "контрольная сумма пакета миграции не совпала"
    else
        say "    предупреждение: пакет прежнего формата без контрольных сумм"
    fi
    have base64 || die "для проверки secret.key нужна утилита base64"
    MSK_BYTES="$(tr -d '[:space:]' < "$MIGRATION_TMP/data/secret.key" |
        base64 -d 2>/dev/null | wc -c | tr -d ' ')"
    [ "$MSK_BYTES" = 32 ] || die "secret.key в пакете повреждён: ожидается ключ AES-256"
    rm -f "$MIGRATION_TMP/archive.list" "$MIGRATION_TMP/archive.types"
}

migration_prepare_import() {
    [ -n "$MIGRATION_IMPORT_FILE" ] || die "укажите файл после --migrate-from"
    MIGRATION_TMP="$(mktemp -d "${TMPDIR:-/tmp}/jhvirt-migration-import.XXXXXX")" ||
        die "не удалось создать временный каталог"
    trap migration_cleanup EXIT INT TERM HUP
    chmod 700 "$MIGRATION_TMP"
    migration_validate_archive

    MIGRATION_SOURCE_MODE="$(migration_manifest_value source_mode)"
    MIGRATION_SOURCE_PREFIX="$(migration_manifest_value source_prefix)"
    MIGRATION_SOURCE_USER="$(migration_manifest_value source_user)"
    MIGRATION_SOURCE_UID="$(migration_manifest_value source_uid)"
    MIGRATION_SOURCE_GID="$(migration_manifest_value source_gid)"
    MIGRATION_DATABASE_KIND="$(migration_manifest_value database_kind)"
    case "$MIGRATION_SOURCE_MODE" in docker|systemd) ;; *) die "неизвестный source_mode в пакете" ;; esac
    case "$MIGRATION_DATABASE_KIND" in embedded|external) ;; *) die "неизвестный database_kind в пакете" ;; esac
    case "$MIGRATION_SOURCE_UID:$MIGRATION_SOURCE_GID" in
        :|*[!0-9:]*) MIGRATION_SOURCE_UID=""; MIGRATION_SOURCE_GID="" ;;
    esac
    [ -f "$MIGRATION_TMP/config/$CONFIG_NAME" ] || die "в пакете нет конфигурации"
    if [ "$MIGRATION_SOURCE_MODE" = docker ]; then
        [ -f "$MIGRATION_TMP/environment/docker.env" ] || die "в пакете нет Docker env"
    else
        [ -f "$MIGRATION_TMP/environment/systemd.env" ] || die "в пакете нет systemd env"
    fi
    if [ "$MIGRATION_DATABASE_KIND" = embedded ]; then
        [ -s "$MIGRATION_TMP/database/jhvirt.dump" ] || die "в пакете нет dump PostgreSQL"
    fi

    [ "$PREFIX_EXPLICIT" -eq 1 ] || PREFIX="$MIGRATION_SOURCE_PREFIX"
    [ "$USER_NAME_EXPLICIT" -eq 1 ] || USER_NAME="$MIGRATION_SOURCE_USER"
    validate_install_identity

    case "$MIGRATION_SOURCE_MODE:$MODE" in
        docker:"")
            if has_docker; then MODE=docker
            elif has_dockerc; then MODE=docker-compose
            else die "на новом сервере нет Docker Compose"
            fi
            ;;
        docker:docker|docker:docker-compose) ;;
        systemd:"") MODE=systemd ;;
        systemd:systemd) ;;
        *) die "смена способа запуска при миграции не поддерживается: $MIGRATION_SOURCE_MODE -> $MODE" ;;
    esac

    have sha256sum || die "для безопасного возобновления импорта нужен sha256sum"
    MI_ARCHIVE_SHA="$(sha256sum "$MIGRATION_IMPORT_FILE" | awk '{print $1}')"
    MIGRATION_MARKER="$PREFIX/.migration-in-progress"
    if [ -f "$MIGRATION_MARKER" ]; then
        if [ "$(sed -n '1p' "$MIGRATION_MARKER")" = "$MI_ARCHIVE_SHA" ]; then
            MIGRATION_RESUME=1
            say "    найден незавершённый импорт того же пакета; операция будет продолжена идемпотентно"
        else
            die "в $PREFIX остался незавершённый импорт другого пакета; не смешивайте состояния"
        fi
    fi
    if [ -e "$PREFIX/compose/.env" ] || [ -e "$PREFIX/config/jhvirt.env" ] || systemd_install_present; then
        if [ "$MIGRATION_RESUME" -eq 0 ]; then
            die "в $PREFIX уже есть установка; импорт разрешён только в пустую цель"
        fi
    fi
    if [ "$MIGRATION_SOURCE_MODE" = docker ]; then
        MI_ENV="$MIGRATION_TMP/environment/docker.env"
        MI_PROJECT="$(env_file_value "$MI_ENV" COMPOSE_PROJECT_NAME)"; [ -n "$MI_PROJECT" ] || MI_PROJECT=ovirt-backup
        if [ "$MIGRATION_RESUME" -eq 0 ] && have docker && { volume_exists "${MI_PROJECT}_postgres-data" || volume_exists "${MI_PROJECT}_jhvirt-data"; }; then
            die "на новом сервере уже есть тома проекта $MI_PROJECT; выберите пустой PREFIX/проект"
        fi
        [ "$URL_EXPLICIT" -eq 1 ] || URL="$(env_file_value "$MI_ENV" JHV_EXTERNAL_URL)"
        if [ "$PORT_EXPLICIT" -eq 0 ]; then
            MI_PORT="$(env_file_value "$MI_ENV" JHV_PORT)"; [ -z "$MI_PORT" ] || PORT="$MI_PORT"
        fi
    else
        MI_ENV="$MIGRATION_TMP/environment/systemd.env"
        [ "$URL_EXPLICIT" -eq 1 ] || URL="$(env_file_value "$MI_ENV" JHV_SERVER_EXTERNAL_URL)"
        if [ "$PORT_EXPLICIT" -eq 0 ]; then
            MI_PORT="$(env_file_value "$MI_ENV" JHV_SERVER_PORT)"; [ -z "$MI_PORT" ] || PORT="$MI_PORT"
        fi
    fi
    if [ -s "$MIGRATION_TMP/tls/server.crt" ] && [ -s "$MIGRATION_TMP/tls/server.key" ]; then
        MIGRATION_TLS_AVAILABLE=1
        if [ -z "$TLS_MODE" ] && [ -z "$TLS_CERT_FILE" ] && [ -z "$TLS_KEY_FILE" ] && [ ! -t 0 ]; then
            TLS_MODE=preserve
            READY_SCHEME=https
        fi
    fi
    mkdir -p "$PREFIX"
    printf '%s\n' "$MI_ARCHIVE_SHA" > "$MIGRATION_MARKER"
    chmod 600 "$MIGRATION_MARKER"
    MIGRATION_ACTIVE=1
    say ""
    say "Пакет миграции: $MIGRATION_SOURCE_MODE из $MIGRATION_SOURCE_PREFIX"
    say "Новая установка: $MODE в $PREFIX, пользователь $USER_NAME"
    say "Исходная служба должна оставаться остановленной до завершения проверки нового узла."
}

uninstall_containers() {
    UNINSTALL_DOCKER_FOUND=0
    UNINSTALL_LAST_DIR=""
    for dir in "$PREFIX/compose" "$COMPOSE_DIR"; do
        [ "$dir" = "$UNINSTALL_LAST_DIR" ] && continue
        UNINSTALL_LAST_DIR="$dir"
        [ -f "$dir/.env" ] || continue
        UNINSTALL_DOCKER_FOUND=1
        step "остановка контейнеров из $dir"
        if have docker && (cd "$dir" && docker compose down >/dev/null 2>&1); then
            continue
        fi
        if have docker-compose && (cd "$dir" && docker-compose down >/dev/null 2>&1); then
            continue
        fi
        UNINSTALL_ERRORS=1
        say "    предупреждение: compose-стек из $dir не удалось остановить"
    done
    [ "$UNINSTALL_DOCKER_FOUND" -eq 1 ] ||
        say "    контейнерная установка с .env не найдена"
}

uninstall_systemd() {
    if systemd_install_present; then
        step "остановка и удаление jhvirt.service"
        if have systemctl; then
            systemctl disable --now jhvirt >/dev/null 2>&1 || true
        fi
        rm -f "$UNIT"
        if have systemctl; then
            systemctl daemon-reload >/dev/null 2>&1 || true
            systemctl reset-failed jhvirt >/dev/null 2>&1 || true
        fi
    else
        say "    systemd-служба jhvirt.service не найдена"
    fi
}

remove_application_files() {
    rm -rf "${PREFIX:?}/bin" "${PREFIX:?}/web" "${PREFIX:?}/docs"
    rm -f "$PREFIX/VERSION"
}

remove_configuration_files() {
    REMOVE_SHARED_CONFIG=0
    case "$UNINSTALL_TARGET" in
        all) REMOVE_SHARED_CONFIG=1 ;;
        docker)
            systemd_install_present || REMOVE_SHARED_CONFIG=1
            ;;
        systemd)
            docker_bundle_present || REMOVE_SHARED_CONFIG=1
            ;;
    esac

    step "удаление конфигурации выбранной установки"
	case "$UNINSTALL_TARGET" in
		docker|all)
            REMOVE_CONFIG_LAST_DIR=""
            for dir in "$PREFIX/compose" "$COMPOSE_DIR"; do
                [ "$dir" = "$REMOVE_CONFIG_LAST_DIR" ] && continue
                REMOVE_CONFIG_LAST_DIR="$dir"
				if [ -f "$dir/.env" ]; then
					if have docker; then
						COMPOSE_DIR="$dir"
						remove_docker_metrics_token || true
					fi
					rm -f "$dir/.env"
                    say "    удалён $dir/.env"
                fi
            done
            ;;
    esac
    case "$UNINSTALL_TARGET" in
		systemd|all)
            if [ -f "$PREFIX/config/jhvirt.env" ]; then
                rm -f "$PREFIX/config/jhvirt.env"
                say "    удалён $PREFIX/config/jhvirt.env"
			fi
			if [ -f "$PREFIX/config/metrics.token" ]; then
				rm -f "$PREFIX/config/metrics.token"
				say "    удалён $PREFIX/config/metrics.token"
			fi
            ;;
    esac

    if [ "$REMOVE_SHARED_CONFIG" -eq 1 ]; then
        for name in "$CONFIG_NAME" "$CONFIG_NAME.new" \
            "$LEGACY_CONFIG_NAME" "$LEGACY_CONFIG_NAME.new"; do
            if [ -f "$PREFIX/config/$name" ]; then
                rm -f "$PREFIX/config/$name"
                say "    удалён $PREFIX/config/$name"
            fi
        done
    else
        say "    общий YAML сохранён: он нужен оставшемуся способу запуска"
    fi

    if [ "$BUNDLE" -eq 0 ] && { [ "$UNINSTALL_TARGET" = docker ] || [ "$UNINSTALL_TARGET" = all ]; }; then
        say "    config/$CONFIG_NAME в репозитории сохранён как файл исходного кода"
    fi
}

choose_uninstall() {
    say ""
    say "Что удалить?"
    say ""
    say "  1) Docker Compose       — остановить и удалить контейнеры и сеть"
    say "  2) systemd              — остановить и удалить jhvirt.service"
    say "  3) Docker и systemd     — снять оба варианта"
    say "  4) назад"
    say ""
    while :; do
        printf 'Номер [1]: '
        read -r UNINSTALL_CHOICE || UNINSTALL_CHOICE=""
        [ -n "$UNINSTALL_CHOICE" ] || UNINSTALL_CHOICE=1
        case "$UNINSTALL_CHOICE" in
            1) UNINSTALL_TARGET=docker; UNINSTALL_LABEL="Docker Compose" ;;
            2) UNINSTALL_TARGET=systemd; UNINSTALL_LABEL="systemd-службу" ;;
            3) UNINSTALL_TARGET=all; UNINSTALL_LABEL="Docker Compose и systemd-службу" ;;
            4) say "Удаление отменено."; return 1 ;;
            *) say "Нет такого варианта."; continue ;;
        esac

        say ""
        say "Что делать с конфигурацией выбранной установки?"
        say ""
        say "  1) сохранить    — YAML и env останутся (рекомендуется)"
        say "  2) удалить      — YAML/env; ключи, база и данные останутся"
        say "  3) удалить всё  — вместе с базой, ключом шифрования и данными"
        say ""
        while :; do
            printf 'Номер [1]: '
            read -r UNINSTALL_CONFIG_CHOICE || UNINSTALL_CONFIG_CHOICE=""
            [ -n "$UNINSTALL_CONFIG_CHOICE" ] || UNINSTALL_CONFIG_CHOICE=1
            case "$UNINSTALL_CONFIG_CHOICE" in
                1) UNINSTALL_REMOVE_CONFIG=0; UNINSTALL_REMOVE_DATA=0; break ;;
                2) UNINSTALL_REMOVE_CONFIG=1; UNINSTALL_REMOVE_DATA=0; break ;;
                3) UNINSTALL_REMOVE_CONFIG=1; UNINSTALL_REMOVE_DATA=1; break ;;
                *) say "Нет такого варианта." ;;
            esac
        done

        if [ "$UNINSTALL_REMOVE_DATA" -eq 1 ]; then
            # Отдельное подтверждение словом, а не y/N: это единственное
            # действие установщика, которое нельзя отменить ничем. Вместе с
            # ключом шифрования пропадает возможность прочитать уже сделанные
            # копии — они останутся лежать в хранилище нечитаемыми. Нажать «y»
            # не глядя слишком легко, набрать слово — уже осознанное действие.
            say ""
            say "Будут удалены: база со всеми подключениями, заданиями и историей,"
            say "ключ шифрования secret.key и данные приложения."
            say ""
            say "Уже сделанные копии останутся в хранилище, но без secret.key их"
            say "не расшифровать — ни этой установкой, ни любой другой."
            say ""
            printf 'Наберите УДАЛИТЬ, чтобы подтвердить: '
            read -r UNINSTALL_CONFIRM || UNINSTALL_CONFIRM=""
            case "$UNINSTALL_CONFIRM" in
                УДАЛИТЬ|удалить) return 0 ;;
                *) say "Удаление отменено."; return 1 ;;
            esac
        fi

        if [ "$UNINSTALL_REMOVE_CONFIG" -eq 1 ]; then
            printf 'Удалить %s и его конфигурацию? Ключи, база и данные будут сохранены. [y/N]: ' "$UNINSTALL_LABEL"
        else
            printf 'Удалить %s, сохранив конфигурацию, ключи и данные? [y/N]: ' "$UNINSTALL_LABEL"
        fi
        read -r UNINSTALL_CONFIRM || UNINSTALL_CONFIRM=""
        case "$UNINSTALL_CONFIRM" in
            y|Y|yes|YES|да|Да|ДА) return 0 ;;
            *) say "Удаление отменено."; return 1 ;;
        esac
    done
}

# remove_data_stores удаляет то, что все остальные ветки удаления берегут:
# базу, ключ шифрования и данные приложения. Вызывается только после явного
# подтверждения словом.
#
# Тома перебираются по всем известным префиксам имени проекта: установка,
# сделанная прежней версией, лежит в томах со старым именем, и «полное
# удаление», которое их не тронуло, оставило бы после себя и базу с
# подключениями, и ключ — то есть не было бы полным.
remove_data_stores() {
    say ""
    say "==> удаление базы, ключа и данных"

    if [ "$UNINSTALL_TARGET" = docker ] || [ "$UNINSTALL_TARGET" = all ]; then
        if have docker; then
            REMOVED_ANY=0
            for VOL in $(data_volume_candidates); do
                volume_exists "$VOL" || continue
                if docker volume rm "$VOL" >/dev/null 2>&1; then
                    say "    удалён том $VOL"
                    REMOVED_ANY=1
                else
                    say "    предупреждение: не удалось удалить том $VOL"
                    UNINSTALL_ERRORS=$((UNINSTALL_ERRORS+1))
                fi
            done
            [ "$REMOVED_ANY" -eq 1 ] || say "    томов с данными не найдено"
        else
            say "    docker недоступен — тома не удалены"
        fi
    fi

    if [ "$UNINSTALL_TARGET" = systemd ] || [ "$UNINSTALL_TARGET" = all ]; then
        if [ -d "$PREFIX/data" ]; then
            if rm -rf "$PREFIX/data"; then
                say "    удалён $PREFIX/data (ключ шифрования)"
            else
                say "    предупреждение: не удалось удалить $PREFIX/data"
                UNINSTALL_ERRORS=$((UNINSTALL_ERRORS+1))
            fi
        fi
        # Базу локальной PostgreSQL не трогаем сами: кластер может обслуживать и
        # чужие базы, а команда на удаление — короткая и точная, её лучше
        # выполнить осознанно, чем получить в подарок от установщика.
        say "    база локальной PostgreSQL не удалялась; если она больше не нужна:"
        say "      sudo -u postgres dropdb jhvirt && sudo -u postgres dropuser jhvirt"
    fi
}

uninstall() {
    [ "$(id -u)" -eq 0 ] || die "для удаления нужны права root: sudo $SELF --uninstall"
    case "$UNINSTALL_TARGET" in
        docker|systemd|all) ;;
        *) die "неизвестная цель удаления: $UNINSTALL_TARGET (docker, systemd или all)" ;;
    esac

    UNINSTALL_ERRORS=0
    case "$UNINSTALL_TARGET" in
        docker)
            uninstall_containers
            # Бинарники нужны systemd-службе, если оба варианта установлены в
            # одном PREFIX. Контейнерный bundle можно убрать только без неё.
            if docker_bundle_present && ! systemd_install_present; then
                remove_application_files
            fi
            UNINSTALL_SUMMARY="Docker Compose"
            ;;
        systemd)
            uninstall_systemd
            # Docker bundle собирает образ из этих же bin/ и web/.
            docker_bundle_present || remove_application_files
            UNINSTALL_SUMMARY="systemd"
            ;;
        all)
            uninstall_containers
            uninstall_systemd
            remove_application_files
            UNINSTALL_SUMMARY="Docker Compose и systemd"
            ;;
    esac

    if [ "$UNINSTALL_REMOVE_CONFIG" -eq 1 ]; then
        if [ "$UNINSTALL_ERRORS" -eq 0 ]; then
            remove_configuration_files
        else
            say "    конфигурация не удалена: сначала устраните ошибки остановки"
        fi
    fi

    if [ "${UNINSTALL_REMOVE_DATA:-0}" -eq 1 ]; then
        if [ "$UNINSTALL_ERRORS" -eq 0 ]; then
            remove_data_stores
        else
            say "    данные не удалены: сначала устраните ошибки остановки"
        fi
    fi

    say ""
    if [ "$UNINSTALL_REMOVE_CONFIG" -eq 1 ] && [ "$UNINSTALL_ERRORS" -eq 0 ]; then
        say "Снято: $UNINSTALL_SUMMARY. Конфигурация выбранной установки удалена."
        say "Ключи и данные намеренно оставлены:"
    else
        say "Снято: $UNINSTALL_SUMMARY. Конфигурация и данные намеренно оставлены:"
    fi
    if [ "$UNINSTALL_TARGET" = docker ] || [ "$UNINSTALL_TARGET" = all ]; then
        say "  контейнерные тома — ключ, база и данные приложения"
    fi
    if [ "$UNINSTALL_TARGET" = systemd ] || [ "$UNINSTALL_TARGET" = all ]; then
        say "  PostgreSQL и база jhvirt не удалялись"
    fi
    if [ -d "$PREFIX" ] && [ "$UNINSTALL_REMOVE_CONFIG" -eq 0 ]; then
        say "  $PREFIX/data   — ключ шифрования секретов"
        say "  $PREFIX/config — конфигурация"
    elif [ -d "$PREFIX" ]; then
        say "  $PREFIX/data — ключ шифрования секретов"
    fi
    if [ "$UNINSTALL_TARGET" = docker ] || [ "$UNINSTALL_TARGET" = all ]; then
        say ""
        say "Посмотреть тома:  docker volume ls | grep jhvirt"
        say "Без secret.key копии не расшифровать — удаляйте тома только после"
        say "отдельной резервной копии."
    fi
    [ -d "$PREFIX" ] && say "Удалить сохранённые данные вручную: rm -rf $PREFIX && userdel $USER_NAME"
    [ "$UNINSTALL_ERRORS" -eq 0 ] ||
        die "удаление завершено не полностью; исправьте предупреждения выше"
    exit 0
}

# --- Выбор ------------------------------------------------------------------

choose() {
    i=0; a=""; b=""; d=""; m=""; e=""; u=""
    has_docker   && { i=$((i+1)); a=$i; }
    has_dockerc  && { i=$((i+1)); b=$i; }
    has_systemd  && { i=$((i+1)); d=$i; }
    i=$((i+1)); m=$i
    i=$((i+1)); e=$i
    i=$((i+1)); u=$i

    if [ ! -t 0 ]; then
        die "нет терминала — укажите способ ключом:
  ./install.sh --mode docker|docker-compose|systemd --url http://host:8080"
    fi

    say ""
    say "Чем запускать?"
    say ""
    [ -n "$a" ] && say "  $a) docker compose   — сервис и PostgreSQL в контейнерах"
    [ -n "$b" ] && say "  $b) docker-compose   — то же, старой командой через дефис"
    [ -n "$d" ] && say "  $d) systemd          — нативная служба и локальная PostgreSQL"
    say "  $m) перенести сюда    — восстановить пакет со старого сервера"
    say "  $e) подготовить перенос — создать защищённый пакет настроек и БД"
    say "  $u) удалить          — выбрать Docker Compose, systemd или оба варианта"
    say ""
    say "Для установки показаны только доступные на этой машине способы."
    say ""
    while :; do
        printf 'Номер [1]: '
        read -r n || n=""
        [ -n "$n" ] || n=1
        [ -n "$a" ] && [ "$n" = "$a" ] && { MODE=docker; return; }
        [ -n "$b" ] && [ "$n" = "$b" ] && { MODE=docker-compose; return; }
        [ -n "$d" ] && [ "$n" = "$d" ] && { MODE=systemd; return; }
        [ "$n" = "$m" ] && { MIGRATION_ACTION="import"; MODE=migration-import; return; }
        [ "$n" = "$e" ] && { MIGRATION_ACTION="export"; MODE=migration-export; return; }
        [ "$n" = "$u" ] && { MODE=uninstall; return; }
        say "Нет такого варианта."
    done
}

if [ "$UNINSTALL_REMOVE_CONFIG" -eq 1 ] && [ "$MODE" != uninstall ]; then
    die "--remove-config используется только вместе с --uninstall"
fi
[ "$MIGRATION_KEEP_SOURCE" -eq 0 ] || [ "$MIGRATION_ACTION" = export ] ||
    die "--keep-source-running используется только с --migration-export"
[ -n "$MODE" ] || [ -n "$MIGRATION_ACTION" ] || choose

if [ "$MODE" = migration-import ]; then
    MODE=""
    migration_ask_nonempty "Путь к пакету миграции: "
    MIGRATION_IMPORT_FILE="$ANSWER"
elif [ "$MODE" = migration-export ]; then
    MODE=""
    migration_ask_nonempty "Куда записать пакет миграции: "
    MIGRATION_EXPORT_FILE="$ANSWER"
fi

case "$MIGRATION_ACTION" in
    "") ;;
    export)
        [ -z "$MIGRATION_IMPORT_FILE" ] || die "нельзя одновременно экспортировать и импортировать миграцию"
        migration_export
        exit 0
        ;;
    import)
        [ -z "$MIGRATION_EXPORT_FILE" ] || die "нельзя одновременно экспортировать и импортировать миграцию"
        migration_prepare_import
        ;;
    *) die "неизвестное действие миграции" ;;
esac

if [ "$MODE" = uninstall ]; then
    if [ -z "$UNINSTALL_TARGET" ]; then
        if [ -t 0 ]; then
            choose_uninstall || exit 0
        else
            # Сохраняем поведение прежнего unattended --uninstall.
            UNINSTALL_TARGET=all
        fi
    fi
    uninstall
fi

# Права root нужны там, где скрипт трогает систему: раскладывает комплект в
# /opt, заводит пользователя, ставит юнит. Запуск контейнеров из каталога
# репозитория обходится без них, и требовать sudo там значит заставлять
# работать под root без причины.
if [ "$BUNDLE" -eq 1 ] || [ "$MODE" = systemd ]; then
    [ "$(id -u)" -eq 0 ] || die "нужны права root: sudo $SELF"
fi

validate_port() {
    case "$1" in
        ''|*[!0-9]*) die "порт должен быть числом: --port 18080" ;;
    esac
    [ "$1" -ge 1 ] 2>/dev/null && [ "$1" -le 65535 ] 2>/dev/null ||
        die "порт должен быть в диапазоне 1..65535: --port 18080"
}

validate_port "$PORT"

case "$MODE" in
    docker)         has_docker  || die "docker compose недоступен" ;;
    docker-compose) has_dockerc || die "docker-compose не найден" ;;
    podman)         die "Podman больше не поддерживается; используйте Docker Compose или systemd" ;;
    systemd)        has_systemd || die "systemd не найден" ;;
    *) die "неизвестный способ: $MODE" ;;
esac

[ -z "$DATABASE_URL_FILE" ] || [ "$MODE" = systemd ] ||
    die "--database-url-file применим только к --mode systemd"
if [ -n "$DATABASE_URL_FILE" ]; then
    [ -f "$DATABASE_URL_FILE" ] && [ -r "$DATABASE_URL_FILE" ] ||
        die "файл строки подключения недоступен: $DATABASE_URL_FILE"
    DB_FILE_MODE="$(stat -c '%a' "$DATABASE_URL_FILE" 2>/dev/null || true)"
    [ "$DB_FILE_MODE" = 600 ] ||
        die "$DATABASE_URL_FILE должен иметь права 0600 (сейчас ${DB_FILE_MODE:-неизвестно})"
fi

# --- Внешний адрес ----------------------------------------------------------
#
# Из URL выводится флаг Secure у сессионной cookie. Поэтому без терминала URL
# обязателен: предположение https при фактическом HTTP делает вход нерабочим.

validate_url() {
    case "$URL" in
        http://*|https://*) ;;
        *) die "адрес должен начинаться с http:// или https://: $URL" ;;
    esac
    AUTHORITY="${URL#*://}"
    [ -n "$AUTHORITY" ] || die "в адресе не указан хост: $URL"
    case "$AUTHORITY" in
        */*|*\?*|*\#*|*[[:space:]]*)
            die "укажите только схему, хост и порт без пути: $URL" ;;
        :*|*:|*@*)
            die "в адресе неверно указан хост или порт: $URL" ;;
    esac
}

ask_url() {
    if [ -z "$URL" ]; then
        [ -t 0 ] || die "без диалога внешний адрес обязателен: --url http://host:$PORT"
        HOST="$(hostname -I 2>/dev/null | awk '{print $1}')"
        [ -n "$HOST" ] || HOST="$(hostname -f 2>/dev/null || hostname)"
        GUESS="http://$HOST:$PORT"
        say ""
        say "Адрес, по которому интерфейс открывают в браузере."
        say "Указывайте https только если TLS уже настроен здесь или на прокси:"
        say "от схемы зависит флаг Secure у сессионной cookie."
        printf 'Адрес [%s]: ' "$GUESS"
        read -r URL || URL=""
        [ -n "$URL" ] || URL="$GUESS"
    fi
    validate_url
}

# --- TLS приложения --------------------------------------------------------

installed_tls_enabled() {
    IT_ENV=""
    case "$MODE" in
        docker|docker-compose)
            IT_WORK="$(migration_docker_work 2>/dev/null || true)"
            [ -z "$IT_WORK" ] || IT_ENV="$IT_WORK/.env"
            [ "$(env_file_value "$IT_ENV" JHV_TLS_ENABLED)" = true ]
            ;;
        systemd)
            IT_ENV="$PREFIX/config/jhvirt.env"
            [ "$(env_file_value "$IT_ENV" JHV_SERVER_TLS_ENABLED)" = true ] ||
                [ "$(yaml_server_tls_value "$PREFIX/config/$CONFIG_NAME" enabled 2>/dev/null)" = true ]
            ;;
        *) return 1 ;;
    esac
}

choose_tls() {
    if [ -z "$TLS_MODE" ] && { [ -n "$TLS_CERT_FILE" ] || [ -n "$TLS_KEY_FILE" ]; }; then
        TLS_MODE=files
    fi
    if [ -n "$TLS_MODE" ]; then
        return 0
    fi
    if [ "$MIGRATION_ACTIVE" -eq 1 ] && [ "$MIGRATION_TLS_AVAILABLE" -eq 1 ]; then
        if [ ! -t 0 ]; then
            TLS_MODE=preserve
            READY_SCHEME=https
            return 0
        fi
        say ""
        say "В пакете есть TLS-сертификат старого сервера. Что использовать?"
        say "  1) сохранить его (по умолчанию)"
        say "  2) выпустить новый самоподписанный сертификат"
        say "  3) взять сертификат и ключ из файлов"
        say "  4) выключить собственный TLS"
        while :; do
            printf 'Номер [1]: '
            read -r TLS_CHOICE || TLS_CHOICE=""
            [ -n "$TLS_CHOICE" ] || TLS_CHOICE=1
            case "$TLS_CHOICE" in
                1) TLS_MODE=preserve; READY_SCHEME=https; return 0 ;;
                2) TLS_MODE=self-signed; return 0 ;;
                3) TLS_MODE=files; return 0 ;;
                4) TLS_MODE=none; return 0 ;;
                *) say "Нет такого варианта." ;;
            esac
        done
    fi
    if installed_tls_enabled; then
        if [ ! -t 0 ]; then
            TLS_MODE=preserve
            READY_SCHEME=https
            return 0
        fi
        say ""
        say "В установке уже включён собственный TLS."
        say "  1) сохранить текущий сертификат (по умолчанию)"
        say "  2) выпустить новый самоподписанный сертификат"
        say "  3) заменить сертификатом и ключом из файлов"
        say "  4) выключить собственный TLS (например, для reverse proxy)"
        while :; do
            printf 'Номер [1]: '
            read -r TLS_CHOICE || TLS_CHOICE=""
            [ -n "$TLS_CHOICE" ] || TLS_CHOICE=1
            case "$TLS_CHOICE" in
                1) TLS_MODE=preserve; READY_SCHEME=https; return 0 ;;
                2) TLS_MODE=self-signed; return 0 ;;
                3) TLS_MODE=files; return 0 ;;
                4) TLS_MODE=none; return 0 ;;
                *) say "Нет такого варианта." ;;
            esac
        done
    fi
    if [ ! -t 0 ]; then
        TLS_MODE=none
        return 0
    fi

    say ""
    say "Как обслуживать HTTPS?"
    say "  1) без собственного TLS — HTTP или TLS на reverse proxy (по умолчанию)"
    say "  2) создать самоподписанный сертификат и включить HTTPS приложения"
    say "  3) подключить существующие сертификат и закрытый ключ"
    while :; do
        printf 'Номер [1]: '
        read -r TLS_CHOICE || TLS_CHOICE=""
        [ -n "$TLS_CHOICE" ] || TLS_CHOICE=1
        case "$TLS_CHOICE" in
            1) TLS_MODE=none; return 0 ;;
            2) TLS_MODE=self-signed; return 0 ;;
            3) TLS_MODE=files; return 0 ;;
            *) say "Нет такого варианта." ;;
        esac
    done
}

tls_host_from_url() {
    TLS_AUTHORITY="${URL#*://}"
    case "$TLS_AUTHORITY" in
        \[*\]*) TLS_HOST="${TLS_AUTHORITY#\[}"; TLS_HOST="${TLS_HOST%%\]*}" ;;
        *) TLS_HOST="${TLS_AUTHORITY%%:*}" ;;
    esac
    case "$TLS_HOST" in
        ""|*[!A-Za-z0-9_.:-]*) die "не удалось получить безопасное имя сертификата из URL: $URL" ;;
    esac
    printf '%s' "$TLS_HOST"
}

tls_validate_pair() {
    TV_CERT="$1"; TV_KEY="$2"
    have openssl || die "для проверки или выпуска TLS-сертификата нужен openssl"
    openssl x509 -in "$TV_CERT" -noout -checkend 1 >/dev/null 2>&1 ||
        die "сертификат не читается или уже истёк: $TV_CERT"
    openssl pkey -in "$TV_KEY" -passin pass: -noout >/dev/null 2>&1 ||
        die "закрытый ключ не читается или защищён паролем: $TV_KEY"
    openssl x509 -in "$TV_CERT" -pubkey -noout > "$TLS_MATERIAL_DIR/cert.pub"
    openssl pkey -in "$TV_KEY" -passin pass: -pubout > "$TLS_MATERIAL_DIR/key.pub"
    cmp -s "$TLS_MATERIAL_DIR/cert.pub" "$TLS_MATERIAL_DIR/key.pub" ||
        die "сертификат и закрытый ключ не образуют пару"
    rm -f "$TLS_MATERIAL_DIR/cert.pub" "$TLS_MATERIAL_DIR/key.pub"
}

prepare_tls() {
    case "$TLS_MODE" in
        none) return 0 ;;
        preserve)
            if [ "$MIGRATION_ACTIVE" -eq 0 ]; then
                READY_SCHEME=https
                return 0
            fi
            [ -s "$MIGRATION_TMP/tls/server.crt" ] && [ -s "$MIGRATION_TMP/tls/server.key" ] ||
                die "в пакете включён TLS, но нет сертификата или ключа"
            migration_tls_matches_url || die "сертификат из пакета не подходит к адресу $URL.
Выберите новый самоподписанный сертификат, укажите PEM-пару либо выключите собственный TLS."
            ;;
        self-signed|files) ;;
        *) die "--tls принимает none, self-signed или files" ;;
    esac

    case "$TLS_DAYS" in ''|*[!0-9]*) die "--tls-days должен быть целым числом" ;; esac
    [ "$TLS_DAYS" -ge 1 ] && [ "$TLS_DAYS" -le 3650 ] ||
        die "--tls-days должен быть в диапазоне 1..3650"
    case "$URL" in
        https://*) ;;
        http://*)
            if [ -t 0 ]; then
                URL="https://${URL#http://}"
                say "    внешний URL изменён на $URL, потому что включён TLS"
            else
                die "собственный TLS требует --url https://host[:port]"
            fi
            ;;
    esac

    TLS_MATERIAL_DIR="$(mktemp -d "${TMPDIR:-/tmp}/jhvirt-tls.XXXXXX")" ||
        die "не удалось создать временный каталог TLS"
    chmod 700 "$TLS_MATERIAL_DIR"
    trap migration_cleanup EXIT INT TERM HUP
    if [ "$TLS_MODE" = preserve ]; then
        cp "$MIGRATION_TMP/tls/server.crt" "$TLS_MATERIAL_DIR/server.crt"
        cp "$MIGRATION_TMP/tls/server.key" "$TLS_MATERIAL_DIR/server.key"
    elif [ "$TLS_MODE" = files ]; then
        if [ -t 0 ]; then
            [ -n "$TLS_CERT_FILE" ] || { migration_ask_nonempty "Путь к сертификату PEM: "; TLS_CERT_FILE="$ANSWER"; }
            [ -n "$TLS_KEY_FILE" ] || { migration_ask_nonempty "Путь к закрытому ключу PEM: "; TLS_KEY_FILE="$ANSWER"; }
        fi
        [ -f "$TLS_CERT_FILE" ] && [ -r "$TLS_CERT_FILE" ] || die "сертификат недоступен: $TLS_CERT_FILE"
        [ -f "$TLS_KEY_FILE" ] && [ -r "$TLS_KEY_FILE" ] || die "закрытый ключ недоступен: $TLS_KEY_FILE"
        cp "$TLS_CERT_FILE" "$TLS_MATERIAL_DIR/server.crt"
        cp "$TLS_KEY_FILE" "$TLS_MATERIAL_DIR/server.key"
    else
        TLS_HOST="$(tls_host_from_url)"
        case "$TLS_HOST" in *:*) TLS_SAN="IP:$TLS_HOST" ;; *[!0-9.]* ) TLS_SAN="DNS:$TLS_HOST" ;; *) TLS_SAN="IP:$TLS_HOST" ;; esac
        have openssl || die "для выпуска самоподписанного сертификата нужен openssl"
        step "самоподписанный TLS-сертификат для $TLS_HOST"
        openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days "$TLS_DAYS" \
            -subj "/CN=$TLS_HOST" \
            -addext "subjectAltName=$TLS_SAN" \
            -addext "basicConstraints=critical,CA:FALSE" \
            -addext "keyUsage=critical,digitalSignature,keyEncipherment" \
            -addext "extendedKeyUsage=serverAuth" \
            -keyout "$TLS_MATERIAL_DIR/server.key" \
            -out "$TLS_MATERIAL_DIR/server.crt" >/dev/null 2>&1 ||
            die "openssl не смог создать сертификат"
    fi
    chmod 644 "$TLS_MATERIAL_DIR/server.crt"
    chmod 600 "$TLS_MATERIAL_DIR/server.key"
    tls_validate_pair "$TLS_MATERIAL_DIR/server.crt" "$TLS_MATERIAL_DIR/server.key"
    READY_SCHEME=https
}

install_tls_docker() {
    ITD_ENV="$1"; ITD_VOL="$(docker_metrics_volume)"
    case "$TLS_MODE" in
        preserve)
            [ -n "$TLS_MATERIAL_DIR" ] || return 0
            ;;
        none)
            set_plain_env JHV_TLS_ENABLED false "$ITD_ENV"
            return 0
            ;;
    esac
    [ -n "$TLS_MATERIAL_DIR" ] || die "TLS-материалы не подготовлены"
    docker run --rm -i --network none --user root -v "$ITD_VOL:/data" \
        docker.io/library/postgres:17-alpine sh -c '
            mkdir -p /data/tls
            umask 077
            cat > /data/tls/server.key
            chown 10001:10001 /data/tls/server.key
            chmod 600 /data/tls/server.key' < "$TLS_MATERIAL_DIR/server.key" ||
        die "не удалось записать TLS-ключ в том $ITD_VOL"
    docker run --rm -i --network none --user root -v "$ITD_VOL:/data" \
        docker.io/library/postgres:17-alpine sh -c '
            cat > /data/tls/server.crt
            chown 10001:10001 /data/tls/server.crt /data/tls
            chmod 700 /data/tls
            chmod 644 /data/tls/server.crt' < "$TLS_MATERIAL_DIR/server.crt" ||
        die "не удалось записать TLS-сертификат в том $ITD_VOL"
    set_plain_env JHV_TLS_ENABLED true "$ITD_ENV"
    set_plain_env JHV_TLS_CERT_FILE /app/data/tls/server.crt "$ITD_ENV"
    set_plain_env JHV_TLS_KEY_FILE /app/data/tls/server.key "$ITD_ENV"
    # Go загружает пару сертификат/ключ при старте HTTP-сервера. При явной
    # замене файлов Compose может не пересоздать контейнер, поскольку пути и
    # переменные окружения не изменились. Перезапуск ниже гарантирует, что
    # текущая инсталляция сразу начнёт отдавать новый сертификат.
    case "$TLS_MODE" in self-signed|files) TLS_RESTART_REQUIRED=1 ;; esac
}

install_tls_systemd() {
    ITS_ENV="$1"
    case "$TLS_MODE" in
        preserve)
            [ -n "$TLS_MATERIAL_DIR" ] || return 0
            ;;
        none)
            set_env JHV_SERVER_TLS_ENABLED false "$ITS_ENV"
            return 0
            ;;
    esac
    [ -n "$TLS_MATERIAL_DIR" ] || die "TLS-материалы не подготовлены"
    install -d -o "$USER_NAME" -g "$USER_NAME" -m 0750 "$PREFIX/config/tls"
    install -o "$USER_NAME" -g "$USER_NAME" -m 0644 \
        "$TLS_MATERIAL_DIR/server.crt" "$PREFIX/config/tls/server.crt"
    install -o "$USER_NAME" -g "$USER_NAME" -m 0600 \
        "$TLS_MATERIAL_DIR/server.key" "$PREFIX/config/tls/server.key"
    set_env JHV_SERVER_TLS_ENABLED true "$ITS_ENV"
    set_env JHV_SERVER_TLS_CERT_FILE "$PREFIX/config/tls/server.crt" "$ITS_ENV"
    set_env JHV_SERVER_TLS_KEY_FILE "$PREFIX/config/tls/server.key" "$ITS_ENV"
}

# Docker сообщает о занятом host-порте только после сборки образа. Проверяем
# раньше. /proc ловит обычные процессы, docker ps — публикации через iptables,
# которые не всегда видны как слушающий socket.
host_port_listening() {
    CHECK_PORT_HEX="$(printf '%04X' "$1")"
    CHECK_PROC_FILES=""
    [ -r /proc/net/tcp ] && CHECK_PROC_FILES="/proc/net/tcp"
    [ -r /proc/net/tcp6 ] && CHECK_PROC_FILES="$CHECK_PROC_FILES /proc/net/tcp6"
    if [ -n "$CHECK_PROC_FILES" ]; then
        # shellcheck disable=SC2086
        awk -v suffix=":$CHECK_PORT_HEX" '
            FNR > 1 && substr($2, length($2) - 4) == suffix && $4 == "0A" {
                found = 1
            }
            END { exit(found ? 0 : 1) }
        ' $CHECK_PROC_FILES
        return
    fi
    if have ss; then
        ss -H -ltn 2>/dev/null |
            awk -v suffix=":$1" 'substr($4, length($4) - length(suffix) + 1) == suffix { found = 1 } END { exit(found ? 0 : 1) }'
        return
    fi
    if have netstat; then
        netstat -ltn 2>/dev/null |
            awk -v suffix=":$1" 'substr($4, length($4) - length(suffix) + 1) == suffix { found = 1 } END { exit(found ? 0 : 1) }'
        return
    fi
    return 1
}

compose_check_dir() {
    if [ "$BUNDLE" -eq 1 ] && [ -f "$PREFIX/compose/docker-compose.yml" ]; then
        printf '%s' "$PREFIX/compose"
    else
        printf '%s' "$COMPOSE_DIR"
    fi
}

compose_container_ids() {
    CHECK_COMPOSE_DIR="$(compose_check_dir)"
    [ -f "$CHECK_COMPOSE_DIR/docker-compose.yml" ] || return 0
    CHECK_RUN="$(runner "$MODE")"
    # shellcheck disable=SC2086
    (
        cd "$CHECK_COMPOSE_DIR"
        $CHECK_RUN ps -q "$COMPOSE_SERVICE" 2>/dev/null || true
        # Старое имя нужно только для обновления уже развёрнутого Compose.
        $CHECK_RUN ps -q "$LEGACY_COMPOSE_SERVICE" 2>/dev/null || true
    )
}

container_port_in_use() {
    CHECK_PUBLISHED="$(docker ps --no-trunc --filter "publish=$1" --format '{{.ID}}' 2>/dev/null || true)"
    if [ -n "$CHECK_PUBLISHED" ]; then
        CHECK_OWN=" $(compose_container_ids | tr '\n' ' ')"
        for CHECK_ID in $CHECK_PUBLISHED; do
            case "$CHECK_OWN" in
                *" $CHECK_ID "*) ;;
                *) return 0 ;;
            esac
        done
        # Порт опубликован только текущим compose-проектом. Во время обновления
        # compose сам остановит старый контейнер перед запуском нового.
        return 1
    fi
    host_port_listening "$1"
}

suggest_container_port() {
    CHECK_CANDIDATE=18080
    while [ "$CHECK_CANDIDATE" -le 18179 ]; do
        if ! container_port_in_use "$CHECK_CANDIDATE"; then
            printf '%s' "$CHECK_CANDIDATE"
            return 0
        fi
        CHECK_CANDIDATE=$((CHECK_CANDIDATE+1))
    done
    return 1
}

ensure_container_port() {
    while container_port_in_use "$PORT"; do
        CHECK_SUGGESTED="$(suggest_container_port || true)"
        [ -n "$CHECK_SUGGESTED" ] || CHECK_SUGGESTED=18080
        if [ -n "$URL" ] || [ ! -t 0 ]; then
            die "порт $PORT уже занят другим процессом или контейнером.

Посмотрите владельца:
  docker ps --filter publish=$PORT
  sudo ss -ltnp 'sport = :$PORT'

Повторите установку с другим портом:
  $SELF --mode $MODE --url http://host:$CHECK_SUGGESTED --port $CHECK_SUGGESTED"
        fi
        say ""
        say "Порт $PORT уже занят другим процессом или контейнером."
        printf 'Другой порт [%s]: ' "$CHECK_SUGGESTED"
        read -r CHECK_SELECTED || CHECK_SELECTED=""
        [ -n "$CHECK_SELECTED" ] || CHECK_SELECTED="$CHECK_SUGGESTED"
        validate_port "$CHECK_SELECTED"
        PORT="$CHECK_SELECTED"
    done
}

case "$MODE" in
    docker|docker-compose)
        [ "$START" -eq 0 ] || ensure_container_port
        ;;
esac

ask_url
choose_tls
prepare_tls

http_ok() {
    if have curl; then
        case "$1" in
            https://*) curl -kfsS --max-time 5 "$1" >/dev/null 2>&1 ;;
            *) curl -fsS --max-time 5 "$1" >/dev/null 2>&1 ;;
        esac
    elif have wget; then
        case "$1" in
            https://*) wget --no-check-certificate -qO- -T 5 "$1" >/dev/null 2>&1 ;;
            *) wget -qO- -T 5 "$1" >/dev/null 2>&1 ;;
        esac
    else
        return 1
    fi
}

wait_ready() {
    READY_URL="$1"
    i=0
    while [ "$i" -lt 90 ]; do
        http_ok "$READY_URL" && return 0
        i=$((i+1))
        sleep 2
    done
    return 1
}

# --- Внешний вход -----------------------------------------------------------

# Спрашивается только для docker-вариантов: Keycloak поднимается тем же
# compose. Для systemd остаётся вход по паролю — существующего провайдера там
# подключают правкой конфигурации, отдельного вопроса это не стоит.
choose_oidc() {
    [ -z "$OIDC_MODE" ] || return 0
    if [ "$MODE" = systemd ] || [ ! -t 0 ]; then
        OIDC_MODE=none
        return 0
    fi

    say ""
    say "Как входить в систему?"
    say ""
    say "  1) только по паролю — учётные записи ведутся здесь (по умолчанию)"
    say "  2) поднять Keycloak рядом — он же настраивается на домены и"
    say "     второй фактор (FreeOTP и совместимые)"
    say "  3) подключить существующий Keycloak или другой OIDC-провайдер"
    say ""
    say "Вход по паролю остаётся в любом случае: это путь внутрь, когда"
    say "провайдер недоступен, а недоступен он бывает как раз в аварии."
    say ""
    while :; do
        printf 'Номер [1]: '
        read -r OIDC_CHOICE || OIDC_CHOICE=""
        [ -n "$OIDC_CHOICE" ] || OIDC_CHOICE=1
        case "$OIDC_CHOICE" in
            1) OIDC_MODE=none; return 0 ;;
            2) OIDC_MODE=keycloak; return 0 ;;
            3) OIDC_MODE=external; return 0 ;;
            *) say "Нет такого варианта." ;;
        esac
    done
}

ask_nonempty() {
    ANSWER=""
    while [ -z "$ANSWER" ]; do
        printf '%s' "$1"
        read -r ANSWER || ANSWER=""
    done
}

# Проверяет, что для выбранного способа хватает данных, и добирает недостающее
# вопросами. Без терминала недостающее — это ошибка, а не повод продолжить с
# наполовину настроенным входом.
prepare_oidc() {
    case "$OIDC_MODE" in
        ""|none) OIDC_MODE=none; return 0 ;;
        keycloak|external) ;;
        *) die "--oidc принимает none, keycloak или external" ;;
    esac

    [ "$MODE" = systemd ] && die "--oidc поддерживается только для docker-вариантов"
    have curl || die "для настройки внешнего входа нужен curl"

    if [ "$OIDC_MODE" = external ]; then
        if [ -t 0 ]; then
            [ -n "$OIDC_ISSUER" ] || { ask_nonempty "Адрес realm (issuer), например https://keycloak.example.org/realms/infra: "; OIDC_ISSUER="$ANSWER"; }
            [ -n "$OIDC_CLIENT_ID" ] || { ask_nonempty "Идентификатор клиента [jhvirt]: "; OIDC_CLIENT_ID="$ANSWER"; }
            if [ -z "$OIDC_CLIENT_SECRET_FILE" ] && [ -z "${OIDC_CLIENT_SECRET:-}" ]; then
                ask_nonempty "Секрет клиента: "; OIDC_CLIENT_SECRET="$ANSWER"
            fi
        fi
        [ -n "$OIDC_ISSUER" ] || die "--oidc external требует --oidc-issuer"
        [ -n "$OIDC_CLIENT_ID" ] || OIDC_CLIENT_ID="jhvirt"
        if [ -n "$OIDC_CLIENT_SECRET_FILE" ]; then
            [ -f "$OIDC_CLIENT_SECRET_FILE" ] || die "нет файла с секретом: $OIDC_CLIENT_SECRET_FILE"
            OIDC_CLIENT_SECRET="$(tr -d '\r\n' < "$OIDC_CLIENT_SECRET_FILE")"
        fi
        [ -n "${OIDC_CLIENT_SECRET:-}" ] || die "--oidc external требует --oidc-client-secret-file"
    else
        # Свой Keycloak: адрес складывается из внешнего адреса службы и порта
        # Keycloak. Именно он попадёт в issuer, поэтому браузер и служба будут
        # звать провайдера одинаково.
        if [ -z "$KEYCLOAK_URL" ]; then
            KC_SCHEME="${URL%%://*}"
            KC_HOSTPORT="${URL#*://}"; KC_HOSTPORT="${KC_HOSTPORT%%/*}"
            KEYCLOAK_URL="$KC_SCHEME://${KC_HOSTPORT%%:*}:$KEYCLOAK_PORT"
        fi
        validate_port "$KEYCLOAK_PORT" || die "--keycloak-port: нужен номер от 1 до 65535"
        OIDC_ISSUER="$KEYCLOAK_URL/realms/$KEYCLOAK_REALM"
        OIDC_CLIENT_ID="jhvirt"
        OIDC_CLIENT_SECRET="$(gen_secret 24)"
        KEYCLOAK_ADMIN_PASSWORD="$(gen_secret 18)"
        [ -n "$OIDC_CLIENT_SECRET" ] && [ -n "$KEYCLOAK_ADMIN_PASSWORD" ] ||
            die "не удалось сгенерировать секреты для Keycloak"
    fi

    if [ -t 0 ] && [ "$OIDC_MODE" != none ]; then
        say ""
        say "Группы допуска. Кто не попал ни в одну — в систему не допускается."
        printf 'Группа администраторов [%s]: ' "$GROUP_ADMIN"
        read -r ANSWER || ANSWER=""; [ -n "$ANSWER" ] && GROUP_ADMIN="$ANSWER"
        printf 'Группа операторов [%s]: ' "$GROUP_OPERATOR"
        read -r ANSWER || ANSWER=""; [ -n "$ANSWER" ] && GROUP_OPERATOR="$ANSWER"
        printf 'Группа наблюдателей [%s]: ' "$GROUP_VIEWER"
        read -r ANSWER || ANSWER=""; [ -n "$ANSWER" ] && GROUP_VIEWER="$ANSWER"
    fi
}

# Соответствие групп ролям — словарь, а viper словари из переменных окружения
# не собирает. Поэтому оно пишется в файл настроек, который compose монтирует
# в контейнер.
#
# Файл делается из штатного образца заменой единственной строки role_mapping,
# а не дописыванием секции в конец: второй ключ auth: в том же документе — это
# дубликат, и разбор YAML откажет целиком.
write_oidc_config() {
    OIDC_CONFIG="$1"
    OIDC_SAMPLE="$COMPOSE_DIR/../config/$CONFIG_NAME"
    [ -f "$OIDC_SAMPLE" ] || OIDC_SAMPLE="$HERE/config/$CONFIG_NAME"
    [ -f "$OIDC_SAMPLE" ] || die "не найден образец конфигурации $CONFIG_NAME"

    awk -v a="$GROUP_ADMIN" -v o="$GROUP_OPERATOR" -v v="$GROUP_VIEWER" '
        /^[[:space:]]*role_mapping: \{\}[[:space:]]*$/ {
            print "    role_mapping:"
            printf "      \"%s\": \"admin\"\n", a
            printf "      \"%s\": \"operator\"\n", o
            printf "      \"%s\": \"viewer\"\n", v
            found = 1
            next
        }
        { print }
        END { if (!found) exit 3 }
    ' "$OIDC_SAMPLE" > "$OIDC_CONFIG" ||
        die "в образце $CONFIG_NAME не нашлась строка role_mapping: {} — впишите соответствие групп вручную"
    chmod 644 "$OIDC_CONFIG"
}

# Настройки внешнего входа в .env. Секрет клиента живёт только здесь: файл
# конфигурации лежит в репозитории установки, и секретам там не место.
write_oidc_env() {
    OIDC_ENV_FILE="$1"
    [ "$OIDC_MODE" = none ] && return 0

    write_oidc_config "$WORK/$CONFIG_NAME"

    set_plain_env JHV_CONFIG_FILE "./$CONFIG_NAME" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_ENABLED true "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_ISSUER "$OIDC_ISSUER" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_CLIENT_ID "$OIDC_CLIENT_ID" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_CLIENT_SECRET "$OIDC_CLIENT_SECRET" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_REDIRECT_URL "$URL/api/v1/auth/oidc/callback" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_POST_LOGOUT_URL "$URL/login" "$OIDC_ENV_FILE"

    if [ "$OIDC_MODE" = keycloak ]; then
        set_plain_env COMPOSE_PROFILES keycloak "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_PORT "$KEYCLOAK_PORT" "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_DB keycloak "$OIDC_ENV_FILE"
        set_plain_env JHV_KEYCLOAK_URL "$KEYCLOAK_URL" "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_ADMIN_USER "$KEYCLOAK_ADMIN_USER" "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_ADMIN_PASSWORD "$KEYCLOAK_ADMIN_PASSWORD" "$OIDC_ENV_FILE"
        set_plain_env JHV_OIDC_BUTTON_LABEL "Войти через Keycloak" "$OIDC_ENV_FILE"
    fi
}

# --- Keycloak ---------------------------------------------------------------

keycloak_token() {
    curl -sS -m 20 --fail \
        -d "grant_type=password" -d "client_id=admin-cli" \
        -d "username=$KEYCLOAK_ADMIN_USER" --data-urlencode "password=$KEYCLOAK_ADMIN_PASSWORD" \
        "$KEYCLOAK_URL/realms/master/protocol/openid-connect/token" 2>/dev/null |
        sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'
}

keycloak_post() {
    curl -sS -m 30 -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $KC_TOKEN" -H 'Content-Type: application/json' \
        -X POST -d "$2" "$KEYCLOAK_URL/admin/realms$1" 2>/dev/null
}

# Первый запуск Keycloak с пустой базой — это миграция схемы, и три минуты там
# не редкость.
keycloak_wait() {
    KC_TRY=0
    while [ "$KC_TRY" -lt 90 ]; do
        if curl -sS -m 5 -o /dev/null "$KEYCLOAK_URL/realms/master" 2>/dev/null; then
            return 0
        fi
        KC_TRY=$((KC_TRY+1))
        sleep 2
    done
    return 1
}

# Заводит realm, группы и клиента. Повторный запуск не ломается: уже
# существующее Keycloak отвергает кодом 409, и это не ошибка установки.
keycloak_bootstrap() {
    KC_TOKEN="$(keycloak_token)"
    [ -n "$KC_TOKEN" ] || die "Keycloak не принял пароль администратора — проверьте $KEYCLOAK_URL"

    KC_CODE="$(keycloak_post "" "{\"realm\":\"$KEYCLOAK_REALM\",\"enabled\":true}")"
    case "$KC_CODE" in
        201|409) ;;
        *) die "не удалось создать realm $KEYCLOAK_REALM (код $KC_CODE)" ;;
    esac

    for KC_GROUP in "$GROUP_ADMIN" "$GROUP_OPERATOR" "$GROUP_VIEWER"; do
        KC_CODE="$(keycloak_post "/$KEYCLOAK_REALM/groups" "{\"name\":\"$KC_GROUP\"}")"
        case "$KC_CODE" in
            201|409) ;;
            *) die "не удалось создать группу $KC_GROUP (код $KC_CODE)" ;;
        esac
    done

    # Mapper обязателен: без него утверждения groups в токене нет вовсе, и
    # войти не сможет никто. full.path=false — иначе группы приедут как
    # /virt-admins и не совпадут с настройкой.
    KC_CLIENT="{
        \"clientId\":\"$OIDC_CLIENT_ID\",
        \"enabled\":true,
        \"publicClient\":false,
        \"secret\":\"$OIDC_CLIENT_SECRET\",
        \"standardFlowEnabled\":true,
        \"redirectUris\":[\"$URL/api/v1/auth/oidc/callback\"],
        \"webOrigins\":[\"+\"],
        \"attributes\":{\"post.logout.redirect.uris\":\"$URL/login\"},
        \"protocolMappers\":[{
            \"name\":\"groups\",
            \"protocol\":\"openid-connect\",
            \"protocolMapper\":\"oidc-group-membership-mapper\",
            \"config\":{
                \"claim.name\":\"groups\",
                \"full.path\":\"false\",
                \"id.token.claim\":\"true\",
                \"access.token.claim\":\"true\",
                \"userinfo.token.claim\":\"true\"
            }
        }]
    }"
    KC_CODE="$(keycloak_post "/$KEYCLOAK_REALM/clients" "$KC_CLIENT")"
    case "$KC_CODE" in
        201) ;;
        409) say "    клиент $OIDC_CLIENT_ID уже был заведён; секрет из .env должен совпадать с его" ;;
        *) die "не удалось создать клиента $OIDC_CLIENT_ID (код $KC_CODE)" ;;
    esac
}

# --- Контейнеры -------------------------------------------------------------

docker_volume_put() {
    DVP_VOLUME="$1"; DVP_SOURCE="$2"; DVP_TARGET="$3"; DVP_MODE="$4"
    docker run --rm -i --network none --user root -v "$DVP_VOLUME:/data" \
        -e DVP_TARGET="$DVP_TARGET" -e DVP_MODE="$DVP_MODE" \
        docker.io/library/postgres:17-alpine sh -c '
            target="/data/$DVP_TARGET"
            mkdir -p "$(dirname "$target")"
            umask 077
            cat > "$target"
            chown 10001:10001 "$target" "$(dirname "$target")"
            chmod "$DVP_MODE" "$target"' < "$DVP_SOURCE" ||
        die "не удалось восстановить $DVP_TARGET в том $DVP_VOLUME"
}

migration_apply_docker_files() {
    MAD_WORK="$1"
    [ "$MIGRATION_ACTIVE" -eq 1 ] || return 0
    [ -f "$MIGRATION_TMP/environment/docker.env" ] || die "в пакете нет Docker env"
    [ -f "$MIGRATION_TMP/config/$CONFIG_NAME" ] || die "в пакете нет YAML"

    cp "$MIGRATION_TMP/environment/docker.env" "$MAD_WORK/.env"
    chmod 600 "$MAD_WORK/.env"
    if [ "$BUNDLE" -eq 1 ]; then
        MAD_CONFIG="$PREFIX/config/$CONFIG_NAME"
        cp "$MIGRATION_TMP/config/$CONFIG_NAME" "$MAD_CONFIG"
        set_plain_env JHV_CONFIG_FILE "../config/$CONFIG_NAME" "$MAD_WORK/.env"
    else
        MAD_CONFIG="$MAD_WORK/ovirt-backup.migrated.yaml"
        cp "$MIGRATION_TMP/config/$CONFIG_NAME" "$MAD_CONFIG"
        set_plain_env JHV_CONFIG_FILE ./ovirt-backup.migrated.yaml "$MAD_WORK/.env"
    fi
    rewrite_prefix_file "$MAD_CONFIG" "$MIGRATION_SOURCE_PREFIX" "$PREFIX"
    chmod 644 "$MAD_CONFIG"
    rewrite_prefix_file "$MAD_WORK/.env" "$MIGRATION_SOURCE_PREFIX" "$PREFIX"
    set_plain_env JHV_EXTERNAL_URL "$URL" "$MAD_WORK/.env"
    set_plain_env JHV_PORT "$PORT" "$MAD_WORK/.env"
    set_plain_env JHV_ADMIN_PASSWORD "" "$MAD_WORK/.env"
    if [ "$(env_file_value "$MAD_WORK/.env" JHV_OIDC_ENABLED)" = true ]; then
        set_plain_env JHV_OIDC_REDIRECT_URL "$URL/api/v1/auth/oidc/callback" "$MAD_WORK/.env"
        set_plain_env JHV_OIDC_POST_LOGOUT_URL "$URL/login" "$MAD_WORK/.env"
    fi
    say "    YAML, env и настройки входа восстановлены из пакета"
}

migration_restore_docker_data() {
    [ "$MIGRATION_ACTIVE" -eq 1 ] || return 0
    MRD_VOL="$(docker_metrics_volume)"
    docker_volume_put "$MRD_VOL" "$MIGRATION_TMP/data/secret.key" secret.key 600
    if [ -s "$MIGRATION_TMP/data/metrics.token" ]; then
        docker_volume_put "$MRD_VOL" "$MIGRATION_TMP/data/metrics.token" metrics.token 600
    fi
    say "    secret.key и служебные токены восстановлены с владельцем UID 10001"
}

docker_wait_postgres() {
    DWP_WORK="$1"; DWP_RUN="$2"; DWP_USER="$3"
    DWP_TRY=0
    while [ "$DWP_TRY" -lt 40 ]; do
        # shellcheck disable=SC2086
        (cd "$DWP_WORK" && $DWP_RUN exec -T postgres pg_isready -U "$DWP_USER" -q) >/dev/null 2>&1 && return 0
        DWP_TRY=$((DWP_TRY+1)); sleep 2
    done
    return 1
}

migration_restore_docker_database() {
    MRDB_WORK="$1"; MRDB_RUN="$2"
    [ "$MIGRATION_ACTIVE" -eq 1 ] || return 0
    [ "$MIGRATION_DATABASE_KIND" = embedded ] || return 0
    MRDB_USER="$(env_file_value "$MRDB_WORK/.env" POSTGRES_USER)"; [ -n "$MRDB_USER" ] || MRDB_USER=jhvirt
    MRDB_DB="$(env_file_value "$MRDB_WORK/.env" POSTGRES_DB)"; [ -n "$MRDB_DB" ] || MRDB_DB=jhvirt
    step "восстановление PostgreSQL из пакета"
    # shellcheck disable=SC2086
    (cd "$MRDB_WORK" && $MRDB_RUN up -d postgres) >/dev/null || die "не удалось запустить PostgreSQL"
    docker_wait_postgres "$MRDB_WORK" "$MRDB_RUN" "$MRDB_USER" || die "PostgreSQL не стала готова"
    # shellcheck disable=SC2086
    (cd "$MRDB_WORK" && $MRDB_RUN exec -T postgres \
        pg_restore -U "$MRDB_USER" -d "$MRDB_DB" --clean --if-exists --no-owner --no-privileges) \
        < "$MIGRATION_TMP/database/jhvirt.dump" || die "не удалось восстановить базу jhvirt"

    if [ -s "$MIGRATION_TMP/database/keycloak.dump" ]; then
        MRDB_KC="$(env_file_value "$MRDB_WORK/.env" KEYCLOAK_DB)"; [ -n "$MRDB_KC" ] || MRDB_KC=keycloak
        # shellcheck disable=SC2086
        if ! (cd "$MRDB_WORK" && $MRDB_RUN exec -T postgres psql -U "$MRDB_USER" -d postgres -tAc \
                "SELECT 1 FROM pg_database WHERE datname='$MRDB_KC'") | grep -q 1; then
            # shellcheck disable=SC2086
            (cd "$MRDB_WORK" && $MRDB_RUN exec -T postgres createdb -U "$MRDB_USER" "$MRDB_KC") ||
                die "не удалось создать базу Keycloak"
        fi
        # shellcheck disable=SC2086
        (cd "$MRDB_WORK" && $MRDB_RUN exec -T postgres \
            pg_restore -U "$MRDB_USER" -d "$MRDB_KC" --clean --if-exists --no-owner --no-privileges) \
            < "$MIGRATION_TMP/database/keycloak.dump" || die "не удалось восстановить базу Keycloak"
    fi
    say "    база и все runtime-настройки восстановлены"
}

docker_host_path() {
    DHP_WORK="$1"; DHP_VALUE="$2"
    case "$DHP_VALUE" in
        /*) printf '%s' "$DHP_VALUE" ;;
        *) printf '%s/%s' "$DHP_WORK" "${DHP_VALUE#./}" ;;
    esac
}

prepare_docker_data_paths() {
    PDP_WORK="$1"
    for PDP_KEY in JHV_BACKUP_DIR JHV_RESTORE_DIR; do
        PDP_VALUE="$(env_file_value "$PDP_WORK/.env" "$PDP_KEY")"
        [ -n "$PDP_VALUE" ] || continue
        PDP_PATH="$(docker_host_path "$PDP_WORK" "$PDP_VALUE")"
        if [ "$MIGRATION_ACTIVE" -eq 1 ] && [ ! -d "$PDP_PATH" ]; then
            case "$PDP_PATH" in
                "$PREFIX"/*|"$PDP_WORK"/*) ;;
                *) die "внешний каталог из $PDP_KEY не найден: $PDP_PATH
Сначала подключите прежнее хранилище или создайте каталог, затем повторите импорт." ;;
            esac
        fi
        mkdir -p "$PDP_PATH" || die "не удалось создать каталог $PDP_PATH"
        # Меняется только сам корень, не содержимое подключённого хранилища.
        if ! docker run --rm --network none --user 10001:10001 -v "$PDP_PATH:/target" \
                docker.io/library/postgres:17-alpine test -w /target >/dev/null 2>&1; then
            # Для пустого локального каталога установщик может исправить сам
            # корень. Содержимое и ACL подключённого хранилища не меняются.
            docker run --rm --network none --user root -v "$PDP_PATH:/target" \
                docker.io/library/postgres:17-alpine \
                sh -c 'chown 10001:10001 /target && chmod u+rwx /target' >/dev/null 2>&1 || true
            docker run --rm --network none --user 10001:10001 -v "$PDP_PATH:/target" \
                docker.io/library/postgres:17-alpine test -w /target >/dev/null 2>&1 ||
                die "контейнерный UID 10001 не получил право записи в $PDP_PATH"
        fi
    done
}

install_containers() {
    RUN="$(runner "$MODE")"

    if [ "$START" -eq 1 ] && ! have curl && ! have wget; then
        die "для проверки готовности нужен curl или wget"
    fi

    # Спрашивается после адреса службы: из него складывается и адрес Keycloak,
    # и адрес возврата, который провайдер сверяет побуквенно.
    if [ "$MIGRATION_ACTIVE" -eq 0 ]; then
        choose_oidc
        prepare_oidc
    else
        OIDC_MODE=none
    fi

    if [ "$BUNDLE" -eq 1 ]; then
        step "раскладка в $PREFIX"
        ensure_service_user
        mkdir -p "$PREFIX/compose" "$PREFIX/config" "$PREFIX/data" "$PREFIX/logs" \
                 "$PREFIX/docs" "$PREFIX/backups" "$PREFIX/restores"
        rm -rf "${PREFIX:?}/bin" "${PREFIX:?}/web"
        # Образ собирается из bin/ и web/dist рядом с Dockerfile, поэтому весь
        # комплект копируется целиком.
        cp -r "$HERE/bin" "$HERE/web" "$PREFIX/"
        cp "$HERE/Dockerfile" "$PREFIX/Dockerfile"
        install_bundle_config
        cp "$HERE/compose/docker-compose.yml" "$HERE/compose/.env.example" "$PREFIX/compose/"
        chmod 644 "$PREFIX/Dockerfile" "$PREFIX/compose/docker-compose.yml" \
            "$PREFIX/compose/.env.example"
        [ -d "$HERE/docs" ] && cp -r "$HERE/docs/." "$PREFIX/docs/"
        [ -f "$HERE/VERSION" ] && cp "$HERE/VERSION" "$PREFIX/"
        WORK="$PREFIX/compose"
        BACKUPS="$PREFIX/backups"; RESTORES="$PREFIX/restores"
    else
        WORK="$COMPOSE_DIR"
        mkdir -p "$WORK/backups" "$WORK/restores"
        BACKUPS="./backups"; RESTORES="./restores"
    fi

    migration_apply_docker_files "$WORK"

	if [ -f "$WORK/.env" ]; then
        say "    $WORK/.env уже есть; пароль базы и пользовательские настройки сохранены"
        set_plain_env JHV_EXTERNAL_URL "$URL" "$WORK/.env"
		set_plain_env JHV_PORT "$PORT" "$WORK/.env"
		set_plain_env JHV_METRICS_ENABLED true "$WORK/.env"
        [ "$MIGRATION_ACTIVE" -eq 1 ] || write_oidc_env "$WORK/.env"
    else
        # Пароль базы задаётся один раз — при создании тома. Если том с прошлой
        # установки уцелел, а .env исчез, сгенерированный пароль базе не
        # подойдёт: она примет только тот, с которым была создана. Служба тогда
        # уходит в цикл перезапуска с «password authentication failed», и связь
        # с пропавшим .env совсем не очевидна.
        VOL=""
        for CANDIDATE in \
                "$(project_name)_postgres-data" \
                "jhvirt_postgres-data" \
                "${LEGACY_COMPOSE_SERVICE}_postgres-data"; do
            if volume_exists "$CANDIDATE"; then
                VOL="$CANDIDATE"
                break
            fi
        done
        RESET_DB=0
        if [ -n "$VOL" ]; then
            if [ -t 0 ]; then
                say ""
                say "Том базы $VOL остался с прошлой установки, а $WORK/.env — нет."
                say "Пароль базы был только в нём, и новый база не примет: он задан"
                say "внутри тома при создании кластера."
                say ""
                say "  1) задать базе новый пароль — подключения, задания и история"
                say "     сохраняются (рекомендуется)"
                say "  2) отменить установку — например, чтобы поискать прежний .env"
                say ""
                while :; do
                    printf 'Номер [1]: '
                    read -r VOL_CHOICE || VOL_CHOICE=""
                    [ -n "$VOL_CHOICE" ] || VOL_CHOICE=1
                    case "$VOL_CHOICE" in
                        1) RESET_DB=1; break ;;
                        2) die "установка отменена; прежний .env — единственное место,
где хранился пароль базы. Полностью убрать старую установку вместе с
данными: $SELF --uninstall, вариант «удалить всё»." ;;
                        *) say "Нет такого варианта." ;;
                    esac
                done
            else
                die "том базы $VOL остался с прошлой установки, а $WORK/.env — нет.

PostgreSQL хранит пароль внутри тома и новый не примет: служба будет
перезапускаться с «password authentication failed».

В диалоговом режиме установщик предлагает задать базе новый пароль, сохранив
данные. Без диалога: верните прежний .env либо снимите установку вместе с
данными — $SELF --uninstall, вариант «удалить всё»."
            fi
        fi

        # Пароль базы генерируется: внутренний секрет, человеком был бы придуман
        # хуже. Шестнадцатеричный — годится и в форме URL, где / и + пришлось бы
        # кодировать.
        PGPASS="$(gen_secret 24)"
        [ -n "$PGPASS" ] || die "не удалось сгенерировать пароль базы"

        # Имя проекта берётся у найденного тома, а не пишется постоянной строкой:
        # оно менялось между версиями, и compose с новым именем завёл бы пустые
        # тома рядом со старыми. База выглядела бы чистой, хотя данные целы и
        # лежат под прежним префиксом — а новый пароль достался бы не тому тому.
        PROJECT="ovirt-backup"
        [ -n "$VOL" ] && PROJECT="${VOL%_postgres-data}"

        if [ "$RESET_DB" -eq 1 ]; then
            say "==> смена пароля базы в томе $VOL"
            reset_db_password "$VOL" "$PGPASS" ||
                die "не удалось сменить пароль базы в томе $VOL.
Данные не тронуты. Верните прежний .env либо снимите установку вместе с
данными: $SELF --uninstall, вариант «удалить всё»."
            say "    пароль базы изменён, данные сохранены"
            # Учётные записи уже есть в этой базе, и первый администратор
            # заново не создаётся: сгенерировать и напечатать пароль значило бы
            # выдать за рабочий тот, которым войти нельзя.
            ADMPASS=""
        fi

        # Пароль администратора задаём сами, а не вылавливаем потом из журнала:
        # формат вывода у docker compose и docker-compose разный, поэтому пароль
        # задаётся до старта, а не извлекается из журнала.
        #
        # Из .env он стирается сразу после запуска: учётная запись уже создана,
        # и держать пароль в файле дольше незачем.
        if [ "$RESET_DB" -eq 0 ]; then
            ADMPASS="$(gen_secret 18)"
            [ -n "$ADMPASS" ] || die "не удалось сгенерировать пароль администратора"
        fi

        umask 077
        {
            printf 'COMPOSE_PROJECT_NAME=%s\n' "$PROJECT"
            printf 'POSTGRES_USER=jhvirt\n'
            printf 'POSTGRES_PASSWORD=%s\n' "$PGPASS"
            printf 'POSTGRES_DB=jhvirt\n'
            printf 'JHV_EXTERNAL_URL=%s\n' "$URL"
            printf 'JHV_PORT=%s\n' "$PORT"
            printf 'JHV_ADMIN_PASSWORD=%s\n' "$ADMPASS"
            printf 'JHV_BACKUP_DIR=%s\n' "$BACKUPS"
            printf 'JHV_RESTORE_DIR=%s\n' "$RESTORES"
            # Внутри тома с данными, а не в /app/logs: тот каталог образ создаёт
            # в своём слое, и при пересоздании контейнера журнал пропадает — как
            # раз тогда, когда по нему разбираются, что было до обновления.
            printf 'JHV_LOG_FILE=/app/data/logs/jhvirt.log\n'
			printf 'JHV_METRICS_ENABLED=true\n'
            printf 'TZ=%s\n' "$(host_timezone)"
        } > "$WORK/.env"
        write_oidc_env "$WORK/.env"
        umask 022
        chmod 600 "$WORK/.env"
        say "    создан $WORK/.env, пароль базы сгенерирован"
	fi

	if [ "$BUNDLE" -eq 1 ]; then
		chown -R "$USER_NAME:$USER_NAME" "$PREFIX"
		chmod 700 "$PREFIX/data"
		# YAML не содержит паролей, а контейнер читает bind mount под UID
		# 10001, который не обязан совпадать с системным пользователем хоста.
		chmod 644 "$PREFIX/config/$CONFIG_NAME"
	fi

	step "token-файл Prometheus"
	ensure_docker_metrics_token
	migration_restore_docker_data
	install_tls_docker "$WORK/.env"
	prepare_docker_data_paths "$WORK"
	migration_restore_docker_database "$WORK" "$RUN"

    if [ "$START" -eq 0 ]; then
        say ""
        say "Подготовлено. Запуск:"
        say "  cd $WORK && $RUN up -d --build"
        if [ "$MIGRATION_ACTIVE" -eq 1 ] && [ "$MIGRATION_DATABASE_KIND" = embedded ]; then
            # shellcheck disable=SC2086
            (cd "$WORK" && $RUN stop postgres) >/dev/null 2>&1 || true
        fi
        return
    fi

    if [ "$OIDC_MODE" = keycloak ]; then
        step "база для Keycloak"
        # Заводится до его старта: без своей базы Keycloak не поднимется, а
        # docker-entrypoint-initdb.d отрабатывает только на пустом томе — том
        # же здесь чаще всего уже есть с прошлой установки.
        # shellcheck disable=SC2086
        (cd "$WORK" && $RUN up -d postgres) >/dev/null 2>&1 ||
            die "не удалось запустить PostgreSQL"
        KC_DB_TRY=0
        while [ "$KC_DB_TRY" -lt 40 ]; do
            # shellcheck disable=SC2086
            (cd "$WORK" && $RUN exec -T postgres pg_isready -U jhvirt -q) >/dev/null 2>&1 && break
            KC_DB_TRY=$((KC_DB_TRY+1)); sleep 2
        done
        # shellcheck disable=SC2086
        if (cd "$WORK" && $RUN exec -T postgres psql -U jhvirt -d postgres -tAc \
                "SELECT 1 FROM pg_database WHERE datname='keycloak'") 2>/dev/null | grep -q 1; then
            say "    база keycloak уже есть"
        else
            # shellcheck disable=SC2086
            (cd "$WORK" && $RUN exec -T postgres createdb -U jhvirt keycloak) >/dev/null 2>&1 ||
                die "не удалось создать базу keycloak"
            say "    база keycloak создана"
        fi
    fi

    step "сборка образа и запуск (в первый раз это несколько минут)"
    # shellcheck disable=SC2086
    (cd "$WORK" && $RUN up -d --build --remove-orphans) || die "запуск не удался; смотрите вывод выше"
    if [ "$TLS_RESTART_REQUIRED" -eq 1 ]; then
        # shellcheck disable=SC2086
        (cd "$WORK" && $RUN restart "$COMPOSE_SERVICE") ||
            die "не удалось перезапустить приложение после замены TLS"
    fi

    step "жду готовности"
    if ! wait_ready "$READY_SCHEME://127.0.0.1:$PORT/readyz"; then
        say ""
        # shellcheck disable=SC2086
        (cd "$WORK" && $RUN logs --tail 30 "$COMPOSE_SERVICE" 2>/dev/null) || true
        die "за 3 минуты сервис не стал готов — последние строки журнала выше"
    fi

    if [ "$OIDC_MODE" = keycloak ]; then
        step "настройка Keycloak"
        keycloak_wait ||
            die "Keycloak не ответил за три минуты по адресу $KEYCLOAK_URL.
Журнал: cd $WORK && $RUN logs keycloak"
        keycloak_bootstrap
        say "    realm $KEYCLOAK_REALM, клиент $OIDC_CLIENT_ID и группы созданы"
        # Пароль администратора Keycloak из файла стираем — как и пароль
        # администратора самой службы: учётная запись заведена, дальше он в
        # файле лишний.
        set_plain_env KEYCLOAK_ADMIN_PASSWORD "" "$WORK/.env"
    fi

    # Пароль администратора: если .env создавали мы, он известен точно. Если
    # .env был раньше — учётная запись уже существует, и показывать нечего.
    if [ -n "${ADMPASS:-}" ]; then
        # Стираем из файла: служба учётную запись создала, дальше он там лишний.
        sed -i 's|^JHV_ADMIN_PASSWORD=.*|JHV_ADMIN_PASSWORD=|' "$WORK/.env" 2>/dev/null || true
    fi

    say ""
    say "════════════════════════════════════════════════════════════"
    say "  ГОТОВО"
    say ""
    say "  интерфейс:     $URL"
    if [ -n "${ADMPASS:-}" ]; then
        say "  пользователь:  admin"
        say "  пароль:        $ADMPASS"
        say ""
        say "  Запишите пароль — больше он нигде не хранится."
    else
        say "  учётная запись уже была создана прежде; пароль не менялся."
        say "  Забыли — задайте новый:"
        say "    cd $WORK && $RUN run --rm $COMPOSE_SERVICE -reset-password admin"
    fi
    if [ "$OIDC_MODE" = keycloak ]; then
        say ""
        say "  Keycloak:      $KEYCLOAK_URL"
        say "  администратор: $KEYCLOAK_ADMIN_USER"
        say "  пароль:        $KEYCLOAK_ADMIN_PASSWORD"
        say ""
        say "  Запишите и его — из файла он тоже стёрт."
    elif [ "$OIDC_MODE" = external ]; then
        say ""
        say "  внешний вход:  $OIDC_ISSUER"
    fi
    say "════════════════════════════════════════════════════════════"
    say ""
    if [ "$OIDC_MODE" != none ]; then
        say "Внешний вход настроен, но пускать пока некого:"
        say "  • заведите пользователей и включите их в группы допуска —"
        say "    $GROUP_ADMIN, $GROUP_OPERATOR, $GROUP_VIEWER;"
        say "  • кто не попал ни в одну, в систему не допускается: так и задумано;"
        if [ "$OIDC_MODE" = keycloak ]; then
            say "  • домены подключаются в Keycloak: realm $KEYCLOAK_REALM → User Federation;"
            say "  • второй фактор — там же: Authentication → Required Actions → Configure OTP"
            say "    (FreeOTP и совместимые);"
        fi
        say "  • соответствие групп ролям правится в $WORK/$CONFIG_NAME без пересборки:"
        say "      cd $WORK && $RUN up -d"
        say "  • вход по паролю остался: он не проходит через провайдера, и второй"
        say "    фактор на него не распространяется — держите такие записи аварийными."
        say ""
    fi
    say "Дальше:"
    if [ "$READY_SCHEME" = https ]; then
        say "  • Собственный TLS включён. Добавьте сертификат в доверенные на рабочих местах."
        say "    Сертификат можно заменить повторным запуском установщика с --tls."
    else
        say "  • Собственный TLS не настроен — при необходимости поставьте reverse proxy перед портом $PORT."
    fi
    if [ "$MIGRATION_ACTIVE" -eq 1 ]; then
        say "  • Подключите на новом сервере прежние backup/restore mount points и проверьте их запись."
        if [ "$(env_file_value "$WORK/.env" JHV_OIDC_ENABLED)" = true ]; then
            say "  • В OIDC-провайдере проверьте redirect URI: $URL/api/v1/auth/oidc/callback"
        fi
    fi
    say "  • Скопируйте ключ шифрования отдельно от базы и не туда, где копии:"
    say "      cd $WORK && $RUN cp $COMPOSE_SERVICE:/app/data/secret.key ./secret.key.backup"
    say "  • Чек-лист перед боем: docs/DEPLOY.md"
    say "  • Забыли пароль: cd $WORK && $RUN run --rm $COMPOSE_SERVICE -reset-password admin"
}

# --- Служба systemd ---------------------------------------------------------

env_value() {
    # EnvironmentFile понимает двойные кавычки. Экранируем только то, что
    # имеет внутри них специальный смысл; значение передаётся аргументом, а не
    # исполняется оболочкой.
    printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g'
}

set_env() {
    KEY="$1"; VALUE="$2"; FILE="$3"; TMP="$FILE.tmp.$$"
    ENCODED="$(env_value "$VALUE")"
    if [ -f "$FILE" ] &&
            [ "$(grep -c "^${KEY}=" "$FILE" || true)" -eq 1 ] &&
            grep -Fqx "${KEY}=\"${ENCODED}\"" "$FILE"; then
        chown "$USER_NAME:$USER_NAME" "$FILE"
        chmod 600 "$FILE"
        return
    fi
    if [ -f "$FILE" ]; then
        grep -v "^${KEY}=" "$FILE" > "$TMP" || true
    else
        : > "$TMP"
    fi
    printf '%s="%s"\n' "$KEY" "$ENCODED" >> "$TMP"
    install -o "$USER_NAME" -g "$USER_NAME" -m 0600 "$TMP" "$FILE"
    rm -f "$TMP"
}

install_postgres_packages() {
    if have apt-get; then
        step "установка PostgreSQL (apt)"
        DEBIAN_FRONTEND=noninteractive apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y postgresql curl
        PG_FAMILY=debian
    elif have dnf; then
        step "установка PostgreSQL (dnf)"
        dnf install -y postgresql-server
        PG_FAMILY=rhel
    else
        die "поддерживаются Ubuntu/Debian (apt) и RHEL/Alma/Rocky (dnf)"
    fi
}

detect_postgres_family() {
    [ -r /etc/os-release ] || die "не найден /etc/os-release для проверки платформы"
    # shellcheck disable=SC1091
    . /etc/os-release
    OS_FAMILY=" ${ID:-} ${ID_LIKE:-} "
    case "$OS_FAMILY" in
        *" ubuntu "*|*" debian "*)
            have apt-get || die "для Ubuntu/Debian нужен пакетный менеджер apt"
            PG_FAMILY=debian
            ;;
        *" rhel "*|*" rocky "*|*" almalinux "*|*" centos "*)
            have dnf || die "для RHEL/Alma/Rocky нужен пакетный менеджер dnf"
            PG_FAMILY=rhel
            ;;
        *)
            die "неподдерживаемая платформа ${PRETTY_NAME:-${ID:-неизвестная}}; нужны Ubuntu/Debian или RHEL/Alma/Rocky"
            ;;
    esac
}

ensure_http_client() {
    if have curl || have wget; then
        return 0
    fi
    detect_postgres_family
    step "установка curl для проверки готовности"
    if [ "$PG_FAMILY" = debian ]; then
        DEBIAN_FRONTEND=noninteractive apt-get update
        DEBIAN_FRONTEND=noninteractive apt-get install -y curl
    else
        dnf install -y curl
    fi
}

prepare_local_postgres() {
    detect_postgres_family
    if ! have psql || ! id postgres >/dev/null 2>&1; then
        install_postgres_packages
    elif ! have curl && ! have wget; then
        if [ "$PG_FAMILY" = debian ]; then
            DEBIAN_FRONTEND=noninteractive apt-get install -y curl
        else
            dnf install -y curl
        fi
    fi

    have runuser || die "не найдена команда runuser (пакет util-linux)"

    if [ "$PG_FAMILY" = rhel ] && [ ! -s /var/lib/pgsql/data/PG_VERSION ]; then
        have postgresql-setup || die "не найдена postgresql-setup после установки PostgreSQL"
        step "инициализация PostgreSQL"
        postgresql-setup --initdb
    fi

    step "запуск PostgreSQL"
    systemctl enable --now postgresql

    i=0
    while [ "$i" -lt 60 ]; do
        if runuser -u postgres -- psql -d postgres -Atc 'select 1' >/dev/null 2>&1; then
            break
        fi
        i=$((i+1)); sleep 1
    done
    [ "$i" -lt 60 ] || die "PostgreSQL не стала готова за минуту"

    if ! runuser -u postgres -- psql -d postgres -Atc \
            "select 1 from pg_roles where rolname='$USER_NAME'" | grep -q 1; then
        runuser -u postgres -- createuser "$USER_NAME"
    fi
    runuser -u postgres -- psql -d postgres -v ON_ERROR_STOP=1 \
        -c "alter role \"$USER_NAME\" login" >/dev/null

    if ! runuser -u postgres -- psql -d postgres -Atc \
            "select 1 from pg_database where datname='jhvirt'" | grep -q 1; then
        runuser -u postgres -- createdb -O "$USER_NAME" jhvirt
    fi
    runuser -u postgres -- psql -d postgres -v ON_ERROR_STOP=1 \
        -c "alter database jhvirt owner to \"$USER_NAME\"" >/dev/null

    # Проверяем именно тот путь, которым пойдёт служба: системный пользователь
    # jhvirt через Unix socket и peer-аутентификацию, без пароля в файле.
    runuser -u "$USER_NAME" -- psql -d jhvirt -Atc 'select 1' >/dev/null ||
        die "локальная база не принимает пользователя $USER_NAME через Unix socket"

    DATABASE_URL="user=$USER_NAME dbname=jhvirt sslmode=disable"
}

read_external_database_url() {
    LINES="$(awk 'END {print NR}' "$DATABASE_URL_FILE")"
    [ "$LINES" -eq 1 ] || die "$DATABASE_URL_FILE должен содержать ровно одну строку DSN"
    DATABASE_URL="$(sed -n '1p' "$DATABASE_URL_FILE")"
    [ -n "$DATABASE_URL" ] || die "строка подключения в $DATABASE_URL_FILE пуста"
}

local_database_needs_admin() {
    TABLE="$(runuser -u postgres -- psql -d jhvirt -Atc \
        "select to_regclass('public.users') is not null" 2>/dev/null || true)"
    if [ "$TABLE" != t ]; then
        return 0
    fi
    USERS="$(runuser -u postgres -- psql -d jhvirt -Atc \
        'select count(*) from users' 2>/dev/null || printf '1')"
    [ "$USERS" = 0 ]
}

check_installed_config() {
    CHECK_UNIT="jhvirt-config-check-$$"
    systemd-run --quiet --wait --pipe --collect \
        --unit="$CHECK_UNIT" --uid="$USER_NAME" --gid="$USER_NAME" \
        --working-directory="$PREFIX" \
        --property="EnvironmentFile=$PREFIX/config/jhvirt.env" \
        "$PREFIX/bin/$SERVER_BINARY" \
        -config "$PREFIX/config/$CONFIG_NAME" -check-config
}

migration_apply_systemd_files() {
    [ "$MIGRATION_ACTIVE" -eq 1 ] || return 0
    [ -f "$MIGRATION_TMP/environment/systemd.env" ] || die "в пакете нет systemd env"
    [ -f "$MIGRATION_TMP/config/$CONFIG_NAME" ] || die "в пакете нет YAML"
    cp "$MIGRATION_TMP/config/$CONFIG_NAME" "$PREFIX/config/$CONFIG_NAME"
    cp "$MIGRATION_TMP/environment/systemd.env" "$PREFIX/config/jhvirt.env"
    cp "$MIGRATION_TMP/data/secret.key" "$PREFIX/data/secret.key"
    [ ! -s "$MIGRATION_TMP/data/metrics.token" ] ||
        cp "$MIGRATION_TMP/data/metrics.token" "$PREFIX/config/metrics.token"
    rewrite_prefix_file "$PREFIX/config/$CONFIG_NAME" "$MIGRATION_SOURCE_PREFIX" "$PREFIX"
    rewrite_prefix_file "$PREFIX/config/jhvirt.env" "$MIGRATION_SOURCE_PREFIX" "$PREFIX"
    set_env JHV_SERVER_EXTERNAL_URL "$URL" "$PREFIX/config/jhvirt.env"
    set_env JHV_SERVER_PORT "$PORT" "$PREFIX/config/jhvirt.env"
    if [ "$(env_file_value "$PREFIX/config/jhvirt.env" JHV_AUTH_OIDC_ENABLED)" = true ]; then
        set_env JHV_AUTH_OIDC_REDIRECT_URL "$URL/api/v1/auth/oidc/callback" "$PREFIX/config/jhvirt.env"
        set_env JHV_AUTH_OIDC_POST_LOGOUT_REDIRECT_URL "$URL/login" "$PREFIX/config/jhvirt.env"
    fi
    set_env JHV_AUTH_BOOTSTRAP_PASSWORD "" "$PREFIX/config/jhvirt.env"
    set_env JHV_METRICS_TOKEN_FILE "$PREFIX/config/metrics.token" "$PREFIX/config/jhvirt.env"
    say "    YAML, env, secret.key и runtime-настройки восстановлены из пакета"
}

migration_restore_systemd_database() {
    [ "$MIGRATION_ACTIVE" -eq 1 ] || return 0
    [ "$MIGRATION_DATABASE_KIND" = embedded ] || return 0
    [ -s "$MIGRATION_TMP/database/jhvirt.dump" ] || die "в пакете нет dump PostgreSQL"
    step "восстановление PostgreSQL из пакета"
    runuser -u postgres -- pg_restore -d jhvirt --clean --if-exists \
        --no-owner --no-privileges --role="$USER_NAME" \
        "$MIGRATION_TMP/database/jhvirt.dump" || die "не удалось восстановить базу jhvirt"
    say "    база, пользователи, задания и runtime-настройки восстановлены"
}

systemd_write_paths() {
    SWP_VALUE="$PREFIX/data $PREFIX/logs"
    if [ "$MIGRATION_ACTIVE" -eq 1 ] && [ -s "$MIGRATION_TMP/systemd-write-paths" ]; then
        SWP_IMPORTED="$(sed -n '1p' "$MIGRATION_TMP/systemd-write-paths")"
        SWP_TEMP="$MIGRATION_TMP/systemd-write-paths.rewritten"
        printf '%s\n' "$SWP_IMPORTED" > "$SWP_TEMP"
        rewrite_prefix_file "$SWP_TEMP" "$MIGRATION_SOURCE_PREFIX" "$PREFIX"
        SWP_VALUE="$(sed -n '1p' "$SWP_TEMP")"
    elif [ -f "$UNIT" ]; then
        SWP_EXISTING="$(sed -n 's/^ReadWritePaths=//p' "$UNIT" | head -n 1)"
        [ -z "$SWP_EXISTING" ] || SWP_VALUE="$SWP_EXISTING"
    fi
    for SWP_PATH in $SWP_VALUE; do
        case "$SWP_PATH" in
            /*) ;;
            *) die "ReadWritePaths содержит не абсолютный путь: $SWP_PATH" ;;
        esac
        case "$SWP_PATH" in
            *[!A-Za-z0-9_./-]*) die "ReadWritePaths содержит неподдерживаемый путь: $SWP_PATH" ;;
        esac
    done
    printf '%s' "$SWP_VALUE"
}

prepare_systemd_write_paths() {
    for PSWP_PATH in $1; do
        if [ ! -d "$PSWP_PATH" ]; then
            case "$PSWP_PATH" in
                "$PREFIX"/*)
                    install -d -o "$USER_NAME" -g "$USER_NAME" -m 0750 "$PSWP_PATH"
                    ;;
                *)
                    die "внешний путь из ReadWritePaths не найден: $PSWP_PATH
Сначала подключите прежнее хранилище или создайте каталог на новом сервере,
назначьте владельца $USER_NAME:$USER_NAME и повторите импорт."
                    ;;
            esac
        fi
        runuser -u "$USER_NAME" -- test -w "$PSWP_PATH" ||
            die "пользователь $USER_NAME не может писать в $PSWP_PATH"
    done
}

install_systemd() {
    [ "$BUNDLE" -eq 1 ] || die "установка службой возможна только из комплекта .run
Из репозитория соберите его: ./run build --target linux/amd64"

    [ -f "$HERE/bin/$SERVER_BINARY" ] || die "в комплекте нет bin/$SERVER_BINARY"
    [ -f "$HERE/web/dist/index.html" ] || die "в комплекте нет web/dist/index.html"
    [ -f "$HERE/config/$CONFIG_NAME" ] || die "в комплекте нет конфигурации"
    [ -f "$HERE/systemd/jhvirt.service" ] || die "в комплекте нет unit systemd"
    have systemd-run || die "не найдена команда systemd-run"

    detect_postgres_family
    [ "$START" -eq 0 ] || ensure_http_client

    UPGRADE=0
    # Бинарь мог остаться от прерванной первой установки; наличие unit
    # подтверждает, что это обновление завершённой установки.
    INSTALLED_BINARY=""
    if [ -x "$PREFIX/bin/$SERVER_BINARY" ]; then
        INSTALLED_BINARY="$PREFIX/bin/$SERVER_BINARY"
    elif [ -x "$PREFIX/bin/$LEGACY_SERVER_BINARY" ]; then
        INSTALLED_BINARY="$PREFIX/bin/$LEGACY_SERVER_BINARY"
    fi
    [ -n "$INSTALLED_BINARY" ] && [ -f "$UNIT" ] && UPGRADE=1

    if [ "$UPGRADE" -eq 1 ]; then
        # -version печатает «<имя бинаря> 1.0.0»; нужна только версия.
        OLD="$("$INSTALLED_BINARY" -version 2>/dev/null | awk '{print $NF}' || true)"
        NEW="$("$HERE/bin/$SERVER_BINARY" -version 2>/dev/null | awk '{print $NF}' || true)"
        step "обновление: ${OLD:-?} -> ${NEW:-?}"
    else
        step "установка службой systemd в $PREFIX"
    fi

    ensure_service_user
    mkdir -p "$PREFIX/bin" "$PREFIX/web" "$PREFIX/config" "$PREFIX/data" "$PREFIX/logs" "$PREFIX/docs"

    WAS_ACTIVE=0
    if [ "$UPGRADE" -eq 1 ] && systemctl is-active --quiet jhvirt 2>/dev/null; then
        say "    останавливаю службу на время замены"
        systemctl stop jhvirt
        WAS_ACTIVE=1
    fi

    install -m 0755 "$HERE/bin/$SERVER_BINARY" "$PREFIX/bin/"
    [ -f "$HERE/bin/jvbackup" ] && install -m 0755 "$HERE/bin/jvbackup" "$PREFIX/bin/"
    rm -rf "$PREFIX/web/dist"
    cp -r "$HERE/web/dist" "$PREFIX/web/dist"
    [ -d "$HERE/docs" ] && cp -r "$HERE/docs/." "$PREFIX/docs/"
    [ -f "$HERE/VERSION" ] && cp "$HERE/VERSION" "$PREFIX/"

    # Конфигурацию не трогаем: в ней уже могут быть правки оператора.
    install_bundle_config
	migration_apply_systemd_files

    chown -R "$USER_NAME:$USER_NAME" "$PREFIX"
    chmod 700 "$PREFIX/data"
    chmod 750 "$PREFIX/logs"
	chmod 750 "$PREFIX/config"
	chmod 640 "$PREFIX/config/$CONFIG_NAME"
	chmod 600 "$PREFIX/config/jhvirt.env" 2>/dev/null || true
	chmod 600 "$PREFIX/data/secret.key" 2>/dev/null || true

    ENV_FILE="$PREFIX/config/jhvirt.env"
    ENV_EXISTED=0
    [ -f "$ENV_FILE" ] && ENV_EXISTED=1

    DATABASE_URL=""
    if [ -n "$DATABASE_URL_FILE" ]; then
        step "подключение внешней PostgreSQL"
        read_external_database_url
        set_env JHV_DATABASE_URL "$DATABASE_URL" "$ENV_FILE"
    elif [ "$MIGRATION_ACTIVE" -eq 1 ] && [ "$MIGRATION_DATABASE_KIND" = embedded ]; then
        if [ "$MIGRATION_RESUME" -eq 0 ] && have runuser && id postgres >/dev/null 2>&1 &&
                runuser -u postgres -- psql -d jhvirt -Atc \
                    "select to_regclass('public.users') is not null" 2>/dev/null | grep -q t; then
            die "локальная база jhvirt на новом сервере уже содержит данные; импорт остановлен"
        fi
        prepare_local_postgres
        set_env JHV_DATABASE_URL "$DATABASE_URL" "$ENV_FILE"
    elif [ "$ENV_EXISTED" -eq 0 ]; then
        prepare_local_postgres
        set_env JHV_DATABASE_URL "$DATABASE_URL" "$ENV_FILE"
    else
        say "    подключение к базе сохранено из $ENV_FILE"
    fi

	set_env JHV_SERVER_EXTERNAL_URL "$URL" "$ENV_FILE"
	set_env JHV_SERVER_PORT "$PORT" "$ENV_FILE"
	METRICS_TOKEN_FILE="$PREFIX/config/metrics.token"
	if [ ! -s "$METRICS_TOKEN_FILE" ]; then
		METRICS_TOKEN="$(gen_secret 32)"
		[ -n "$METRICS_TOKEN" ] || die "не удалось сгенерировать токен Prometheus"
		umask 077
		printf '%s\n' "$METRICS_TOKEN" > "$METRICS_TOKEN_FILE"
		umask 022
	fi
	chown "$USER_NAME:$USER_NAME" "$METRICS_TOKEN_FILE"
	chmod 600 "$METRICS_TOKEN_FILE"
	set_env JHV_METRICS_ENABLED true "$ENV_FILE"
	set_env JHV_METRICS_TOKEN_FILE "$METRICS_TOKEN_FILE" "$ENV_FILE"
	install_tls_systemd "$ENV_FILE"
	migration_restore_systemd_database

    ADMPASS=""
    if [ "$START" -eq 1 ] && [ "$ENV_EXISTED" -eq 0 ] &&
            [ -z "$DATABASE_URL_FILE" ] && local_database_needs_admin; then
        ADMPASS="$(gen_secret 18)"
        [ -n "$ADMPASS" ] || die "не удалось сгенерировать пароль администратора"
        set_env JHV_AUTH_BOOTSTRAP_PASSWORD "$ADMPASS" "$ENV_FILE"
    elif [ "$ENV_EXISTED" -eq 0 ]; then
        set_env JHV_AUTH_BOOTSTRAP_PASSWORD "" "$ENV_FILE"
    fi

    SYSTEMD_WRITE_PATHS="$(systemd_write_paths)"
	prepare_systemd_write_paths "$SYSTEMD_WRITE_PATHS"
    sed -e "s|@PREFIX@|$PREFIX|g" -e "s|@USER_NAME@|$USER_NAME|g" \
        -e "s|@READ_WRITE_PATHS@|$SYSTEMD_WRITE_PATHS|g" \
        "$HERE/systemd/jhvirt.service" > "$UNIT.tmp"
    install -m 0644 "$UNIT.tmp" "$UNIT"
    rm -f "$UNIT.tmp"
    systemctl daemon-reload

    step "проверка конфигурации"
    check_installed_config || die "установленная конфигурация не прошла проверку"
    rm -f "$PREFIX/bin/$LEGACY_SERVER_BINARY"

    SHOULD_START=0
    if [ "$START" -eq 1 ]; then
        if [ "$UPGRADE" -eq 0 ] || [ "$WAS_ACTIVE" -eq 1 ]; then
            SHOULD_START=1
        fi
    fi

    if [ "$SHOULD_START" -eq 1 ]; then
        step "запуск службы"
        if [ "$UPGRADE" -eq 0 ]; then
            systemctl enable --now jhvirt
        else
            systemctl start jhvirt
        fi
        step "жду готовности"
        if ! wait_ready "$READY_SCHEME://127.0.0.1:$PORT/readyz"; then
            journalctl -u jhvirt -n 40 --no-pager || true
            die "за 3 минуты служба не стала готова — последние строки журнала выше"
        fi
        if [ -n "$ADMPASS" ]; then
            set_env JHV_AUTH_BOOTSTRAP_PASSWORD "" "$ENV_FILE"
        fi
    fi

    say ""
    say "════════════════════════════════════════════════════════════"
    if [ "$SHOULD_START" -eq 1 ]; then
        say "  ГОТОВО"
    else
        say "  УСТАНОВЛЕНО, НО НЕ ЗАПУЩЕНО"
    fi
    say ""
    say "  каталог:        $PREFIX"
    say "  интерфейс:      $URL"
    if [ -n "$ADMPASS" ]; then
        say "  пользователь:   admin"
        say "  пароль:         $ADMPASS"
        say ""
        say "  Запишите пароль — после первого успешного старта он удаляется из env."
    else
        say "  учётные записи сохранены; пароль администратора не менялся."
    fi
    say "════════════════════════════════════════════════════════════"
    say ""
    if [ "$SHOULD_START" -eq 0 ]; then
        if [ "$UPGRADE" -eq 1 ]; then
            say "Служба до обновления была остановлена и осталась остановленной."
        else
            say "Запуск: systemctl enable --now jhvirt"
        fi
    fi
    say "Журнал: journalctl -u jhvirt -f"
    say "Ключ шифрования: $PREFIX/data/secret.key — сохраните его отдельно от базы."
    if [ "$READY_SCHEME" = https ]; then
        say "TLS: $PREFIX/config/tls/server.crt — добавьте сертификат в доверенные на рабочих местах."
    fi
    if [ "$MIGRATION_ACTIVE" -eq 1 ]; then
        say "Проверьте существование и владельцев внешних путей из ReadWritePaths:"
        say "  $SYSTEMD_WRITE_PATHS"
    fi
}

case "$MODE" in
    docker|docker-compose) install_containers ;;
    systemd)               install_systemd ;;
esac

if [ "$MIGRATION_ACTIVE" -eq 1 ] && [ -n "$MIGRATION_MARKER" ]; then
    rm -f "$MIGRATION_MARKER"
fi
