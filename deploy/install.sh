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
#   ./install.sh --migration-export /root/jhvirt.tar.gz \
#                --migration-to user@10.0.0.5:/home/user
#                                      создать пакет и сразу отправить его
#   ./install.sh --backup-dir /srv/backups --restore-dir /srv/restores
#                                      хранилище копий и каталог восстановления
#                                      лежат по другим путям, чем на прежнем узле
#   ./install.sh --dr-backup-dir /mnt/dr/ovirt-backup
#                                      ежедневные dump БД и копия secret.key
#   ./install.sh --tls self-signed      создать и подключить локальный сертификат
#   ./install.sh --tls files --tls-cert-file /root/server.crt \
#                --tls-key-file /root/server.key
#                                      подключить существующую пару сертификат/ключ
#
# Вход в систему (встроенный Keycloak — Docker; внешний OIDC — также systemd):
#   ./install.sh --oidc none           только по паролю (по умолчанию)
#   ./install.sh --oidc keycloak       поднять Keycloak рядом и настроить
#   ./install.sh --keycloak-port 8081  порт Keycloak наружу
#   ./install.sh --keycloak-app-admin-user backup-admin
#                                      первый администратор приложения в realm jhvirt
#   ./install.sh --keycloak-ad --keycloak-ad-domain example.org \
#                --keycloak-ad-controller dc01.example.org \
#                --keycloak-ad-bind-user svc-keycloak@example.org \
#                --keycloak-ad-bind-password-file /root/ad-bind.password \
#                --keycloak-ad-ca-file /root/ad-ca-chain.pem
#                                      простое подключение AD по DNS-домену
#   ./install.sh --keycloak-ad --keycloak-ad-url ldaps://dc.example.org:636 \
#                --keycloak-ad-users-dn 'OU=Users,DC=example,DC=org' \
#                --keycloak-ad-groups-dn 'OU=Groups,DC=example,DC=org' \
#                --keycloak-ad-bind-dn 'CN=svc-keycloak,OU=Service Accounts,DC=example,DC=org' \
#                --keycloak-ad-bind-password-file /root/ad-bind.password \
#                --keycloak-ad-ca-file /root/ad-ca-chain.pem
#                                      расширенное подключение с отдельными DN
#   ./install.sh --keycloak-ad-provider corp-ad
#                                      имя provider; нужно то же имя при обновлении
#   ./install.sh --keycloak-ad-group-mode read-only
#                                      AD управляет членством без записи из Keycloak
#   ./install.sh --oidc external --oidc-issuer https://kc/realms/infra \
#                --oidc-client-id jhvirt --oidc-client-secret-file /root/kc.secret
#                                      подключить существующий провайдер
#   ./install.sh --oidc-backchannel-url http://keycloak:8080
#                                      внутренний origin провайдера
#   ./install.sh --local-login disabled локальный парольный вход при OIDC
#                                      (enabled оставляет аварийный доступ)
#   sudo /opt/jhvirt/bin/ovirt-backup-recover-admin
#                                      сбросить local-admin с доверенного хоста
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
DR_UNIT="/etc/systemd/system/jhvirt-dr-backup.service"
DR_TIMER="/etc/systemd/system/jhvirt-dr-backup.timer"
KEYCLOAK_HELPER_UNIT="/etc/systemd/system/jhvirt-keycloak-helper.service"
KEYCLOAK_HELPER_SOCKET="/etc/systemd/system/jhvirt-keycloak-helper.socket"
SERVER_BINARY="ovirt-backup-server"
COMPOSE_SERVICE="ovirt-backup"
CONFIG_NAME="ovirt-backup.yaml"
LEGACY_CONFIG_NAME="virt-manager.yaml"
# Один digest используется и Compose, и служебными одноразовыми контейнерами.
# Так установка не получает другой образ только потому, что тег обновился
# между двумя командами.
POSTGRES_HELPER_IMAGE="docker.io/library/postgres:17-alpine@sha256:18cfe3ef5e6815560c98237d6216d1e5119702fb0f3894c8785dd58b8bbe5d73"
APP_DB_USER="ovirt_backup_app"
KEYCLOAK_DB_USER="keycloak_app"
LOCAL_ADMIN_USER="local-admin"

MODE=""; URL=""; DATABASE_URL_FILE=""; UNINSTALL_TARGET=""; START=1; PORT=8080
URL_EXPLICIT=0; PORT_EXPLICIT=0
UNINSTALL_REMOVE_CONFIG=0; UNINSTALL_REMOVE_DATA=0
MIGRATION_ACTION=""; MIGRATION_EXPORT_FILE=""; MIGRATION_IMPORT_FILE=""
# Пусто — брать пути из .env: при обычной установке они складываются рядом, при
# переносе приезжают из пакета. Заданы — заменить, о чём установщик скажет вслух.
BACKUP_DIR_OVERRIDE=""; RESTORE_DIR_OVERRIDE=""
DR_BACKUP_DIR_OVERRIDE=""
# Куда отправить пакет переноса: user@сервер:/каталог. Пусто — оставить рядом.
MIGRATION_TO=""
MIGRATION_TMP=""; MIGRATION_ACTIVE=0; MIGRATION_SOURCE_MODE=""
MIGRATION_SOURCE_PREFIX=""; MIGRATION_SOURCE_USER=""; MIGRATION_DATABASE_KIND=""
MIGRATION_SOURCE_UID=""; MIGRATION_SOURCE_GID=""; MIGRATION_MARKER=""; MIGRATION_RESUME=0
MIGRATION_TLS_AVAILABLE=0
MIGRATION_KEEP_SOURCE=0; MIGRATION_SOURCE_STOPPED=""; MIGRATION_EXPORT_COMMITTED=0
TLS_MODE=""; TLS_CERT_FILE=""; TLS_KEY_FILE=""
TLS_DAYS=825; TLS_MATERIAL_DIR=""; TLS_RESTART_REQUIRED=0; READY_SCHEME=http
SETUP_TMP=""; SETUP_BINARY=""; OIDC_CONFIG_SOURCE=""; OIDC_GROUPS_EXISTING=0
ALLOW_HTTP=0; BIND_ADDRESS=0.0.0.0
# Внешний вход: none — только пароль, keycloak — поднять рядом, external —
# подключить существующего провайдера.
OIDC_MODE=""; OIDC_EXISTING=0; OIDC_ISSUER=""; OIDC_BACKCHANNEL_URL=""; OIDC_CLIENT_ID=""; OIDC_CLIENT_SECRET_FILE=""
OIDC_ALLOW_LOCAL_LOGIN=""
KEYCLOAK_PORT=8081; KEYCLOAK_PORT_EXPLICIT=0; KEYCLOAK_REALM="jhvirt"; KEYCLOAK_URL=""; KEYCLOAK_API_URL=""
KEYCLOAK_ADMIN_USER="kc-bootstrap-admin"; KEYCLOAK_ADMIN_PASSWORD=""; OIDC_CLIENT_SECRET=""
KEYCLOAK_BOOTSTRAP_USER=""; KEYCLOAK_ACTIVE_ADMIN_USER=""; KEYCLOAK_ACTIVE_ADMIN_PASSWORD=""
KEYCLOAK_RECOVERY_PASSWORD=""
KEYCLOAK_APP_ADMIN_USER="backup-admin"; KEYCLOAK_APP_ADMIN_USER_EXPLICIT=0
KEYCLOAK_APP_ADMIN_PASSWORD=""; KEYCLOAK_APP_ADMIN_CREATED=0
KEYCLOAK_DIRECT_TLS=0; KEYCLOAK_BIND_ADDRESS=127.0.0.1; KEYCLOAK_CONTAINER_PORT=8080
# Параметры AD применяются только при явном --keycloak-ad либо в ответ на
# интерактивный вопрос. Bind-пароль копируется в file vault Keycloak и никогда
# не попадает в .env, аргументы процессов или БД Keycloak.
KEYCLOAK_AD_REQUESTED=0; KEYCLOAK_AD_PROVIDER="active-directory"; KEYCLOAK_AD_PROVIDER_EXPLICIT=0
KEYCLOAK_AD_DOMAIN=""; KEYCLOAK_AD_CONTROLLER=""; KEYCLOAK_AD_URL=""
KEYCLOAK_AD_USERS_DN=""; KEYCLOAK_AD_GROUPS_DN=""; KEYCLOAK_AD_BIND_DN=""
KEYCLOAK_AD_BIND_PASSWORD_FILE=""; KEYCLOAK_AD_BIND_PASSWORD=""; KEYCLOAK_AD_PASSWORD_WAS_INTERACTIVE=0
KEYCLOAK_AD_CA_FILE=""; KEYCLOAK_AD_SIMPLE=0
KEYCLOAK_AD_GROUP_MODE="read-only"; KEYCLOAK_AD_CA_TARGET=""; KEYCLOAK_AD_VAULT_TARGET=""
# Имена групп допуска. Пользователь, не попавший ни в одну, в систему не
# допускается: default_role остаётся пустым.
GROUP_ADMIN="virt-admins"; GROUP_OPERATOR="virt-operators"; GROUP_VIEWER="virt-readers"

die() { printf '\nошибка: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --allow-http) ALLOW_HTTP=1; shift ;;
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
        # Куда отправить готовый пакет: пользователь, сервер и каталог одной
        # строкой, как у scp. Пакет остаётся и здесь — неудачная передача не
        # должна означать потерю единственной копии состояния.
        --migration-to) [ $# -ge 2 ] || die "--migration-to требует user@сервер:/каталог"; MIGRATION_TO="$2"; shift 2 ;;
        --migration-to=*) MIGRATION_TO="${1#--migration-to=}"; shift ;;
        # Пути к данным на этом узле. Нужны прежде всего при переносе: диск на
        # новом сервере называется иначе, монтируется в другое место или вовсе
        # заменён сетевым хранилищем. Путь внутри контейнера при этом не
        # меняется, поэтому записанные в базе хранилища остаются рабочими.
        --backup-dir) [ $# -ge 2 ] || die "--backup-dir требует путь"; BACKUP_DIR_OVERRIDE="$2"; shift 2 ;;
        --backup-dir=*) BACKUP_DIR_OVERRIDE="${1#--backup-dir=}"; shift ;;
        --restore-dir) [ $# -ge 2 ] || die "--restore-dir требует путь"; RESTORE_DIR_OVERRIDE="$2"; shift 2 ;;
        --restore-dir=*) RESTORE_DIR_OVERRIDE="${1#--restore-dir=}"; shift ;;
        --dr-backup-dir) [ $# -ge 2 ] || die "--dr-backup-dir требует путь"; DR_BACKUP_DIR_OVERRIDE="$2"; shift 2 ;;
        --dr-backup-dir=*) DR_BACKUP_DIR_OVERRIDE="${1#--dr-backup-dir=}"; shift ;;
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
        --oidc-backchannel-url) [ $# -ge 2 ] || die "--oidc-backchannel-url требует значение"; OIDC_BACKCHANNEL_URL="$2"; shift 2 ;;
        --oidc-backchannel-url=*) OIDC_BACKCHANNEL_URL="${1#--oidc-backchannel-url=}"; shift ;;
        --oidc-client-id) [ $# -ge 2 ] || die "--oidc-client-id требует значение"; OIDC_CLIENT_ID="$2"; shift 2 ;;
        --oidc-client-id=*) OIDC_CLIENT_ID="${1#--oidc-client-id=}"; shift ;;
        # Секрет — файлом, а не значением: аргументы командной строки видны в
        # ps любому пользователю машины и оседают в истории оболочки.
        --oidc-client-secret-file) [ $# -ge 2 ] || die "--oidc-client-secret-file требует путь"; OIDC_CLIENT_SECRET_FILE="$2"; shift 2 ;;
        --oidc-client-secret-file=*) OIDC_CLIENT_SECRET_FILE="${1#--oidc-client-secret-file=}"; shift ;;
        --local-login) [ $# -ge 2 ] || die "--local-login требует enabled или disabled"; OIDC_ALLOW_LOCAL_LOGIN="$2"; shift 2 ;;
        --local-login=*) OIDC_ALLOW_LOCAL_LOGIN="${1#--local-login=}"; shift ;;
        --keycloak-port) [ $# -ge 2 ] || die "--keycloak-port требует значение"; KEYCLOAK_PORT="$2"; KEYCLOAK_PORT_EXPLICIT=1; shift 2 ;;
        --keycloak-port=*) KEYCLOAK_PORT="${1#--keycloak-port=}"; KEYCLOAK_PORT_EXPLICIT=1; shift ;;
        --keycloak-url) [ $# -ge 2 ] || die "--keycloak-url требует значение"; KEYCLOAK_URL="$2"; shift 2 ;;
        --keycloak-url=*) KEYCLOAK_URL="${1#--keycloak-url=}"; shift ;;
        --keycloak-app-admin-user) [ $# -ge 2 ] || die "--keycloak-app-admin-user требует имя или none"; KEYCLOAK_APP_ADMIN_USER="$2"; KEYCLOAK_APP_ADMIN_USER_EXPLICIT=1; shift 2 ;;
        --keycloak-app-admin-user=*) KEYCLOAK_APP_ADMIN_USER="${1#--keycloak-app-admin-user=}"; KEYCLOAK_APP_ADMIN_USER_EXPLICIT=1; shift ;;
        --keycloak-ad) KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-provider) [ $# -ge 2 ] || die "--keycloak-ad-provider требует имя"; KEYCLOAK_AD_PROVIDER="$2"; KEYCLOAK_AD_PROVIDER_EXPLICIT=1; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-provider=*) KEYCLOAK_AD_PROVIDER="${1#--keycloak-ad-provider=}"; KEYCLOAK_AD_PROVIDER_EXPLICIT=1; KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-domain) [ $# -ge 2 ] || die "--keycloak-ad-domain требует DNS-домен"; KEYCLOAK_AD_DOMAIN="$2"; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-domain=*) KEYCLOAK_AD_DOMAIN="${1#--keycloak-ad-domain=}"; KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-controller) [ $# -ge 2 ] || die "--keycloak-ad-controller требует DNS-имя DC"; KEYCLOAK_AD_CONTROLLER="$2"; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-controller=*) KEYCLOAK_AD_CONTROLLER="${1#--keycloak-ad-controller=}"; KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-url) [ $# -ge 2 ] || die "--keycloak-ad-url требует ldaps:// URL"; KEYCLOAK_AD_URL="$2"; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-url=*) KEYCLOAK_AD_URL="${1#--keycloak-ad-url=}"; KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-users-dn) [ $# -ge 2 ] || die "--keycloak-ad-users-dn требует DN"; KEYCLOAK_AD_USERS_DN="$2"; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-users-dn=*) KEYCLOAK_AD_USERS_DN="${1#--keycloak-ad-users-dn=}"; KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-groups-dn) [ $# -ge 2 ] || die "--keycloak-ad-groups-dn требует DN"; KEYCLOAK_AD_GROUPS_DN="$2"; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-groups-dn=*) KEYCLOAK_AD_GROUPS_DN="${1#--keycloak-ad-groups-dn=}"; KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-bind-dn) [ $# -ge 2 ] || die "--keycloak-ad-bind-dn требует DN"; KEYCLOAK_AD_BIND_DN="$2"; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-bind-dn=*) KEYCLOAK_AD_BIND_DN="${1#--keycloak-ad-bind-dn=}"; KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-bind-user) [ $# -ge 2 ] || die "--keycloak-ad-bind-user требует UPN или DN"; KEYCLOAK_AD_BIND_DN="$2"; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-bind-user=*) KEYCLOAK_AD_BIND_DN="${1#--keycloak-ad-bind-user=}"; KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-bind-password-file) [ $# -ge 2 ] || die "--keycloak-ad-bind-password-file требует путь"; KEYCLOAK_AD_BIND_PASSWORD_FILE="$2"; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-bind-password-file=*) KEYCLOAK_AD_BIND_PASSWORD_FILE="${1#--keycloak-ad-bind-password-file=}"; KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-ca-file) [ $# -ge 2 ] || die "--keycloak-ad-ca-file требует путь к PEM bundle"; KEYCLOAK_AD_CA_FILE="$2"; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-ca-file=*) KEYCLOAK_AD_CA_FILE="${1#--keycloak-ad-ca-file=}"; KEYCLOAK_AD_REQUESTED=1; shift ;;
        --keycloak-ad-group-mode) [ $# -ge 2 ] || die "--keycloak-ad-group-mode требует ldap-only или read-only"; KEYCLOAK_AD_GROUP_MODE="$2"; KEYCLOAK_AD_REQUESTED=1; shift 2 ;;
        --keycloak-ad-group-mode=*) KEYCLOAK_AD_GROUP_MODE="${1#--keycloak-ad-group-mode=}"; KEYCLOAK_AD_REQUESTED=1; shift ;;
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

# docker_unavailable_reason объясняет, почему вариант с Docker недоступен.
#
# Три причины выглядят одинаково для проверки и совершенно по-разному для того,
# кто это читает: программы нет, программа есть но демон не запущен, вместо
# docker стоит podman. Общее «нет Docker Compose» отправляет искать пакет,
# который на самом деле установлен, — а всё лечится запуском службы.
docker_unavailable_reason() {
    if ! have docker; then
        printf 'docker не установлен'
        return
    fi
    if docker --version 2>&1 | grep -qi podman; then
        printf 'вместо docker установлен podman: этот способ рассчитан на docker'
        return
    fi
    if ! docker info >/dev/null 2>&1; then
        if have systemctl && [ "$(systemctl is-active docker 2>/dev/null)" != active ]; then
            printf 'служба docker не запущена — sudo systemctl enable --now docker'
        else
            printf 'демон docker не отвечает: проверьте systemctl status docker и права на /var/run/docker.sock'
        fi
        return
    fi
    if ! docker compose version >/dev/null 2>&1 && ! have docker-compose; then
        printf 'нет docker compose: установите плагин docker-compose-plugin'
        return
    fi
    printf 'docker доступен'
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

ensure_labeled_volume() {
    ELV_NAME="$1"; ELV_SUFFIX="$2"
    volume_exists "$ELV_NAME" && return 0
    docker volume create \
        --label "com.docker.compose.project=$(project_name)" \
        --label "com.docker.compose.volume=$ELV_SUFFIX" "$ELV_NAME" >/dev/null
}

docker_volume_value() {
    DVV_VOLUME="$1"; DVV_PATH="$2"
    volume_exists "$DVV_VOLUME" || return 0
    docker run --rm --network none --user root -v "$DVV_VOLUME:/data:ro" \
        "$POSTGRES_HELPER_IMAGE" sh -c 'test -s "/data/$1" && cat "/data/$1"' sh "$DVV_PATH" 2>/dev/null || true
}

docker_volume_write() {
    DVW_VOLUME="$1"; DVW_PATH="$2"; DVW_OWNER="$3"; DVW_MODE="$4"
    ensure_labeled_volume "$DVW_VOLUME" "${DVW_VOLUME#$(project_name)_}"
    docker run --rm -i --network none --user root -v "$DVW_VOLUME:/data" \
        -e DVW_PATH="$DVW_PATH" -e DVW_OWNER="$DVW_OWNER" -e DVW_MODE="$DVW_MODE" \
        "$POSTGRES_HELPER_IMAGE" sh -c '
            target="/data/$DVW_PATH"
            mkdir -p "$(dirname "$target")"
            umask 077
            cat > "$target"
            chown "$DVW_OWNER" "$target"
            chmod "$DVW_MODE" "$target"' || die "не удалось записать $DVW_PATH в том $DVW_VOLUME"
}

docker_volume_remove() {
    DVR_VOLUME="$1"; DVR_PATH="$2"
    volume_exists "$DVR_VOLUME" || return 0
    docker run --rm --network none --user root -v "$DVR_VOLUME:/data" \
        "$POSTGRES_HELPER_IMAGE" rm -f "/data/$DVR_PATH" >/dev/null 2>&1 || true
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

sha256_value() {
    if have sha256sum; then
        sha256sum | awk '{print $1}'
    elif have shasum; then
        shasum -a 256 | awk '{print $1}'
    elif have openssl; then
        openssl dgst -sha256 -r | awk '{print $1}'
    else
        die "для подготовки recovery-токена нужен sha256sum, shasum или openssl"
    fi
}

# Сам токен остаётся на хосте и не монтируется в штатную службу. В её
# конфигурацию попадает только verifier, поэтому shell внутри контейнера не
# позволяет воспользоваться штатным CLI восстановления.
ensure_recovery_token() {
    ERT_FILE="$1"
    mkdir -p "$(dirname "$ERT_FILE")"
    if [ ! -s "$ERT_FILE" ]; then
        ERT_TOKEN="$(gen_secret 32)"
        [ -n "$ERT_TOKEN" ] || die "не удалось сгенерировать recovery-токен"
        umask 077
        printf '%s\n' "$ERT_TOKEN" > "$ERT_FILE"
        umask 022
        ERT_TOKEN=""
    fi
    ERT_VALUE="$(cat "$ERT_FILE")"
    [ -n "$ERT_VALUE" ] || die "recovery-токен $ERT_FILE пуст"
    case "$ERT_VALUE" in
        *[!0-9a-fA-F]*) die "recovery-токен $ERT_FILE должен быть шестнадцатеричной строкой" ;;
    esac
	[ "${#ERT_VALUE}" -ge 64 ] || die "recovery-токен $ERT_FILE слишком короткий"
    chmod 600 "$ERT_FILE"
    if [ "$(id -u)" -eq 0 ]; then
        chown root:root "$ERT_FILE"
    fi
    RECOVERY_TOKEN_HASH="$(printf '%s' "$ERT_VALUE" | sha256_value)"
    ERT_VALUE=""
    [ "${#RECOVERY_TOKEN_HASH}" -eq 64 ] || die "не удалось вычислить SHA-256 recovery-токена"
}

docker_metrics_volume() {
	printf '%s_jhvirt-data' "$(project_name)"
}

postgres_secrets_volume() {
    printf '%s_postgres-secrets' "$(project_name)"
}

keycloak_data_volume() {
    printf '%s_keycloak-data' "$(project_name)"
}

# Переносит runtime-секреты из .env в тома, доступные только нужному
# контейнеру. Функция идемпотентна и одновременно обновляет старые установки,
# где приложение и PostgreSQL пользовались одним паролем из environment.
prepare_docker_runtime_secrets() {
    PDRS_WORK="$1"
    PDRS_APP_VOL="$(docker_metrics_volume)"
    PDRS_PG_VOL="$(postgres_secrets_volume)"
    ensure_labeled_volume "$PDRS_APP_VOL" jhvirt-data
    ensure_labeled_volume "$PDRS_PG_VOL" postgres-secrets

    PGPASS="$(env_file_value "$PDRS_WORK/.env" POSTGRES_PASSWORD)"
    [ -n "$PGPASS" ] || PGPASS="$(docker_volume_value "$PDRS_PG_VOL" admin-password)"
    [ -n "$PGPASS" ] || PGPASS="$(gen_secret 24)"
    [ -n "$PGPASS" ] || die "не удалось подготовить административный пароль PostgreSQL"
    printf '%s\n' "$PGPASS" | docker_volume_write "$PDRS_PG_VOL" admin-password 0:0 0400

    APP_DB_PASSWORD="$(docker_volume_value "$PDRS_APP_VOL" database.password)"
    [ -n "$APP_DB_PASSWORD" ] || APP_DB_PASSWORD="$(gen_secret 24)"
    [ -n "$APP_DB_PASSWORD" ] || die "не удалось подготовить пароль роли приложения"
    printf '%s\n' "$APP_DB_PASSWORD" | docker_volume_write "$PDRS_APP_VOL" database.password 10001:10001 0600

    PDRS_DATABASE_URL="$(env_file_value "$PDRS_WORK/.env" JHV_DATABASE_URL)"
    [ -n "$PDRS_DATABASE_URL" ] || PDRS_DATABASE_URL="$(docker_volume_value "$PDRS_APP_VOL" database.url)"
    if [ -n "$DATABASE_URL_FILE" ]; then
        read_external_database_url
        PDRS_DATABASE_URL="$DATABASE_URL"
    fi
    if [ -n "$PDRS_DATABASE_URL" ]; then
        printf '%s\n' "$PDRS_DATABASE_URL" | docker_volume_write "$PDRS_APP_VOL" database.url 10001:10001 0600
        set_plain_env JHV_DATABASE_URL_FILE /app/data/database.url "$PDRS_WORK/.env"
    else
        docker_volume_remove "$PDRS_APP_VOL" database.url
        set_plain_env JHV_DATABASE_URL_FILE "" "$PDRS_WORK/.env"
    fi

    if [ -n "${ADMPASS:-}" ]; then
        printf '%s\n' "$ADMPASS" | docker_volume_write "$PDRS_APP_VOL" bootstrap-admin.password 10001:10001 0600
        set_plain_env JHV_ADMIN_PASSWORD_FILE /app/data/bootstrap-admin.password "$PDRS_WORK/.env"
    else
        set_plain_env JHV_ADMIN_PASSWORD_FILE "" "$PDRS_WORK/.env"
    fi

    PDRS_OIDC_SECRET="$(docker_volume_value "$PDRS_APP_VOL" oidc-client.secret)"
    [ -n "$PDRS_OIDC_SECRET" ] || PDRS_OIDC_SECRET="$(env_file_value "$PDRS_WORK/.env" JHV_OIDC_CLIENT_SECRET)"
    [ -n "$PDRS_OIDC_SECRET" ] || PDRS_OIDC_SECRET="${OIDC_CLIENT_SECRET:-}"
    if [ -n "$PDRS_OIDC_SECRET" ]; then
        OIDC_CLIENT_SECRET="$PDRS_OIDC_SECRET"
        printf '%s\n' "$OIDC_CLIENT_SECRET" | docker_volume_write "$PDRS_APP_VOL" oidc-client.secret 10001:10001 0600
        set_plain_env JHV_OIDC_CLIENT_SECRET_FILE /app/data/oidc-client.secret "$PDRS_WORK/.env"
    else
        set_plain_env JHV_OIDC_CLIENT_SECRET_FILE "" "$PDRS_WORK/.env"
    fi

    # Старые имена оставляются пустыми ради понятного обновления .env, но
    # секретов в выводе docker inspect после пересоздания уже нет.
    set_plain_env POSTGRES_PASSWORD "" "$PDRS_WORK/.env"
    set_plain_env POSTGRES_APP_USER "$APP_DB_USER" "$PDRS_WORK/.env"
    set_plain_env JHV_DATABASE_URL "" "$PDRS_WORK/.env"
    set_plain_env JHV_ADMIN_PASSWORD "" "$PDRS_WORK/.env"
    set_plain_env JHV_OIDC_CLIENT_SECRET "" "$PDRS_WORK/.env"
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
		-v "$VOL:/data" "$POSTGRES_HELPER_IMAGE" sh -c '
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
		"$POSTGRES_HELPER_IMAGE" rm -f /data/metrics.token >/dev/null 2>&1 || {
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
    for PREF in "$(project_name)" jhvirt "$COMPOSE_SERVICE"; do
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
        "$POSTGRES_HELPER_IMAGE" >/dev/null 2>&1 ||
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
    KEY="$1"; VALUE="$2"; FILE="$3"
    JHV_ENV_DIR="${FILE%/*}"
    [ "$JHV_ENV_DIR" != "$FILE" ] || JHV_ENV_DIR=.
    JHV_ENV_BASE="${FILE##*/}"
    JHV_ENV_TMP="$(mktemp "$JHV_ENV_DIR/.${JHV_ENV_BASE}.XXXXXX")" ||
        die "не удалось создать временный env рядом с $FILE"
    chmod 600 "$JHV_ENV_TMP"
    if [ -f "$FILE" ]; then
        JHV_ENV_GREP_STATUS=0
        grep -v "^${KEY}=" "$FILE" > "$JHV_ENV_TMP" || JHV_ENV_GREP_STATUS=$?
        [ "$JHV_ENV_GREP_STATUS" -le 1 ] || {
            rm -f "$JHV_ENV_TMP"
            die "не удалось прочитать $FILE"
        }
    fi
    printf '%s=%s\n' "$KEY" "$VALUE" >> "$JHV_ENV_TMP" || {
        rm -f "$JHV_ENV_TMP"
        die "не удалось обновить $FILE"
    }
    chmod 600 "$JHV_ENV_TMP"
    mv -f "$JHV_ENV_TMP" "$FILE" || {
        rm -f "$JHV_ENV_TMP"
        die "не удалось опубликовать $FILE"
    }
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
    [ -z "${SETUP_TMP:-}" ] || rm -rf "$SETUP_TMP"
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
            "$POSTGRES_HELPER_IMAGE" \
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
    migration_volume_file "$ME_DATA_VOLUME" database.url "$ME_DIR/data/database.url" 0
    migration_volume_file "$ME_DATA_VOLUME" oidc-client.secret "$ME_DIR/data/oidc-client.secret" 0
    migration_volume_file "$ME_DATA_VOLUME" bootstrap-admin.password "$ME_DIR/data/bootstrap-admin.password" 0
    migration_volume_file "$ME_DATA_VOLUME" tls/server.crt "$ME_DIR/tls/server.crt" 0
    migration_volume_file "$ME_DATA_VOLUME" tls/server.key "$ME_DIR/tls/server.key" 0
    if [ -s "$PREFIX/keycloak-helper/keycloak.json" ] && [ ! -L "$PREFIX/keycloak-helper/keycloak.json" ]; then
        cp "$PREFIX/keycloak-helper/keycloak.json" "$ME_DIR/data/keycloak-helper.json"
        chmod 600 "$ME_DIR/data/keycloak-helper.json"
    fi

    # Федерация AD хранит bind-пароль вне БД Keycloak. Без vault-файла и CA
    # восстановленная база выглядит исправной, но доменный вход не работает.
    ME_KC_REALM="$(env_file_value "$ME_WORK/.env" KEYCLOAK_REALM)"
    [ -n "$ME_KC_REALM" ] || ME_KC_REALM="$KEYCLOAK_REALM"
    ME_KC_VAULT_VALUE="$(env_file_value "$ME_WORK/.env" JHV_KEYCLOAK_VAULT_DIR)"
    if [ -n "$ME_KC_VAULT_VALUE" ]; then
        ME_KC_VAULT_PATH="$(docker_host_path "$ME_WORK" "$ME_KC_VAULT_VALUE")"
        if [ -s "$ME_KC_VAULT_PATH/${ME_KC_REALM}_ad-bind" ]; then
            cp "$ME_KC_VAULT_PATH/${ME_KC_REALM}_ad-bind" "$ME_DIR/data/keycloak-ad-bind"
        fi
    fi
    ME_KC_TRUST_VALUE="$(env_file_value "$ME_WORK/.env" JHV_KEYCLOAK_TRUSTSTORE_DIR)"
    if [ -n "$ME_KC_TRUST_VALUE" ]; then
        ME_KC_TRUST_PATH="$(docker_host_path "$ME_WORK" "$ME_KC_TRUST_VALUE")"
        mkdir -p "$ME_DIR/truststores"
        for ME_KC_CA in "$ME_KC_TRUST_PATH"/*; do
            [ -f "$ME_KC_CA" ] && [ ! -L "$ME_KC_CA" ] || continue
            cp "$ME_KC_CA" "$ME_DIR/truststores/$(basename "$ME_KC_CA")"
        done
    fi
    if [ "$(env_file_value "$ME_WORK/.env" JHV_TLS_ENABLED)" = true ]; then
        [ -s "$ME_DIR/tls/server.crt" ] && [ -s "$ME_DIR/tls/server.key" ] ||
            die "TLS включён, но в томе $ME_DATA_VOLUME нет сертификата или ключа"
    fi

    ME_DATABASE_URL="$(env_file_value "$ME_WORK/.env" JHV_DATABASE_URL)"
    [ -n "$ME_DATABASE_URL" ] || ME_DATABASE_URL="$(sed -n '1p' "$ME_DIR/data/database.url" 2>/dev/null || true)"
    if [ -n "$ME_DATABASE_URL" ]; then
        printf '%s\n' "$ME_DATABASE_URL" > "$ME_DIR/data/database.url"
        set_plain_env JHV_DATABASE_URL "" "$ME_DIR/environment/docker.env"
        set_plain_env JHV_DATABASE_URL_FILE /app/data/database.url "$ME_DIR/environment/docker.env"
    fi
    if [ -s "$ME_DIR/data/oidc-client.secret" ]; then
        set_plain_env JHV_OIDC_CLIENT_SECRET "" "$ME_DIR/environment/docker.env"
        set_plain_env JHV_OIDC_CLIENT_SECRET_FILE /app/data/oidc-client.secret "$ME_DIR/environment/docker.env"
    fi
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
    ME_DATABASE_URL_FILE="$(env_file_value "$PREFIX/config/jhvirt.env" JHV_DATABASE_URL_FILE)"
    if [ -n "$ME_DATABASE_URL_FILE" ]; then
        [ -s "$ME_DATABASE_URL_FILE" ] || die "не найден файл DSN внешней PostgreSQL: $ME_DATABASE_URL_FILE"
        cp "$ME_DATABASE_URL_FILE" "$ME_DIR/data/database.url"
        set_env JHV_DATABASE_URL "" "$ME_DIR/environment/systemd.env"
        set_env JHV_DATABASE_URL_FILE "$PREFIX/config/database.url" "$ME_DIR/environment/systemd.env"
    fi
    ME_OIDC_SECRET_FILE="$(env_file_value "$PREFIX/config/jhvirt.env" JHV_AUTH_OIDC_CLIENT_SECRET_FILE)"
    if [ -n "$ME_OIDC_SECRET_FILE" ]; then
        [ -s "$ME_OIDC_SECRET_FILE" ] || die "не найден файл OIDC client secret: $ME_OIDC_SECRET_FILE"
        cp "$ME_OIDC_SECRET_FILE" "$ME_DIR/data/oidc-client.secret"
        set_env JHV_AUTH_OIDC_CLIENT_SECRET "" "$ME_DIR/environment/systemd.env"
        set_env JHV_AUTH_OIDC_CLIENT_SECRET_FILE "$PREFIX/config/oidc-client.secret" "$ME_DIR/environment/systemd.env"
    fi
    ME_BOOTSTRAP_FILE="$(env_file_value "$PREFIX/config/jhvirt.env" JHV_AUTH_BOOTSTRAP_PASSWORD_FILE)"
    [ -z "$ME_BOOTSTRAP_FILE" ] || [ ! -s "$ME_BOOTSTRAP_FILE" ] ||
        cp "$ME_BOOTSTRAP_FILE" "$ME_DIR/data/bootstrap-admin.password"

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
    [ -n "$ME_DATABASE_URL" ] || ME_DATABASE_URL="$(sed -n '1p' "$ME_DIR/data/database.url" 2>/dev/null || true)"
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
        "$MIGRATION_TMP/database" "$MIGRATION_TMP/data" "$MIGRATION_TMP/tls" \
        "$MIGRATION_TMP/truststores"
    chmod 700 "$MIGRATION_TMP"

    step "подготовка пакета миграции ($MODE)"
    migration_quiesce_source
    case "$MODE" in
        docker|docker-compose) migration_export_docker "$MIGRATION_TMP" ;;
        systemd) migration_export_systemd "$MIGRATION_TMP" ;;
    esac
    chmod 600 "$MIGRATION_TMP/data/"* "$MIGRATION_TMP/tls/"* \
        "$MIGRATION_TMP/truststores/"* \
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

    if [ -n "$MIGRATION_TO" ]; then
        migration_send_package
        return
    fi

    say "Передайте его на новый сервер защищённым каналом и выполните:"
    say "  sudo sh ./ovirt-backup-*.run --migrate-from $MIGRATION_EXPORT_FILE"
    say ""
    say "Либо сразу отправьте его отсюда:"
    say "  $SELF --migration-export $MIGRATION_EXPORT_FILE --migration-to user@сервер:/каталог"
    say "Локальные каталоги бэкапов не копировались; подключите их на новом узле отдельно."
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
            ""|manifest|checksums.sha256|systemd-write-paths|config|config/ovirt-backup.yaml|environment|environment/docker.env|environment/systemd.env|database|database/jhvirt.dump|database/keycloak.dump|data|data/secret.key|data/metrics.token|data/database.url|data/oidc-client.secret|data/bootstrap-admin.password|data/keycloak-ad-bind|data/keycloak-helper.json|tls|tls/server.crt|tls/server.key|truststores) ;;
            truststores/*)
                MVA_TRUST_NAME="${MVA_ENTRY#truststores/}"
                case "$MVA_TRUST_NAME" in ""|*/*|.|..) die "недопустимое имя truststore в пакете: $MVA_ENTRY" ;; esac
                ;;
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

# migration_send_package передаёт пакет на новый сервер.
#
# Раньше это делал оператор руками, и на ровном месте спотыкался: пакет лежит с
# правами 0600 под root, обычный scp его не прочитает, а scp из-под sudo идёт
# уже от root — с его ключами и его known_hosts, которых обычно нет.
#
# Внутри пакета secret.key и пароль базы. Поэтому отправка обставлена двумя
# условиями: ключ хоста должен быть известен заранее, и после передачи
# сверяются контрольные суммы.
migration_send_package() {
    MSP_DEST="$MIGRATION_TO"
    case "$MSP_DEST" in
        *:*) ;;
        *) die "формат назначения: user@сервер:/каталог (получено: $MSP_DEST)" ;;
    esac
    MSP_LOGIN="${MSP_DEST%%:*}"
    MSP_DIR="${MSP_DEST#*:}"
    MSP_HOST="${MSP_LOGIN#*@}"
    [ -n "$MSP_DIR" ] || die "не указан каталог назначения: $MSP_DEST"
    [ -n "$MSP_HOST" ] || die "не указан сервер назначения: $MSP_DEST"
    have scp || die "для отправки пакета нужен scp"

    # Ключ хоста проверяется заранее и намеренно не принимается автоматически.
    # Отправить secret.key на сервер, подлинность которого не подтверждена, —
    # это отдать ключ от всех копий тому, кто окажется на том адресе. Сверять
    # отпечаток должен человек, один раз.
    if ! ssh-keygen -F "$MSP_HOST" >/dev/null 2>&1; then
        die "ключ хоста $MSP_HOST не известен этому пользователю ($(id -un)).
В пакете лежит secret.key — отправлять его на непроверенный сервер нельзя.

Сверьте отпечаток и запомните ключ:
  ssh-keyscan -t ed25519 $MSP_HOST | ssh-keygen -lf -
  ssh-keyscan -t ed25519 $MSP_HOST >> ~/.ssh/known_hosts

Отпечаток должен совпасть с тем, что показывает сам сервер:
  ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub"
    fi

    MSP_NAME="$(basename "$MIGRATION_EXPORT_FILE")"
    MSP_REMOTE="${MSP_DIR%/}/$MSP_NAME"

    say ""
    say "==> отправка пакета на $MSP_HOST"
    say "    $MIGRATION_EXPORT_FILE -> $MSP_LOGIN:$MSP_REMOTE"
    # -p сохраняет режим 0600: пакет не должен доехать читаемым для всех.
    scp -p "$MIGRATION_EXPORT_FILE" "$MSP_LOGIN:$MSP_REMOTE" ||
        die "не удалось передать пакет. Он остался здесь: $MIGRATION_EXPORT_FILE"

    # Сверка после передачи: обрезанный пакет провалит импорт позже и куда
    # непонятнее — уже на разборе архива, на другом сервере.
    if have sha256sum; then
        MSP_LOCAL_SUM="$(sha256sum "$MIGRATION_EXPORT_FILE" | awk '{print $1}')"
        MSP_REMOTE_SUM="$(ssh "$MSP_LOGIN" "sha256sum '$MSP_REMOTE' 2>/dev/null | awk '{print \$1}'" 2>/dev/null || true)"
        if [ -z "$MSP_REMOTE_SUM" ]; then
            say "    предупреждение: не удалось сверить контрольную сумму на той стороне"
        elif [ "$MSP_LOCAL_SUM" != "$MSP_REMOTE_SUM" ]; then
            die "пакет доехал повреждённым: суммы не совпали.
Здесь:  $MSP_LOCAL_SUM
Там:    $MSP_REMOTE_SUM
Повторите отправку; локальный пакет цел."
        else
            say "    контрольная сумма совпала"
        fi
    fi

    say ""
    say "Пакет на месте. На новом сервере выполните:"
    say "  sudo bash deploy/install.sh --migrate-from $MSP_REMOTE \\"
    say "    --url https://<адрес нового сервера>:8080 \\"
    say "    --backup-dir /путь/к/копиям --restore-dir /путь/к/восстановлению"
    say ""
    say "Локальные каталоги бэкапов не копировались; подключите их на новом узле отдельно."
    say "После успешного импорта уничтожьте пакет с обеих машин: он содержит secret.key."
    say "  shred -u $MIGRATION_EXPORT_FILE"
    if [ "$MIGRATION_KEEP_SOURCE" -eq 1 ]; then
        say "Исходное приложение снова запущено: пакет предназначен для репетиции."
    elif [ -n "$MIGRATION_SOURCE_STOPPED" ]; then
        say "Исходное приложение оставлено остановленным, чтобы состояние больше не расходилось."
    fi
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
            else die "пакет создан Docker-установкой, а здесь запустить её нечем: $(docker_unavailable_reason).
Способ запуска при переносе сменить нельзя — сначала подготовьте Docker."
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
    if have systemctl; then
        systemctl disable --now jhvirt-keycloak-helper.socket >/dev/null 2>&1 || true
        systemctl stop jhvirt-keycloak-helper.service >/dev/null 2>&1 || true
    fi
    rm -f "$KEYCLOAK_HELPER_UNIT" "$KEYCLOAK_HELPER_SOCKET"
    if have systemctl; then
        systemctl daemon-reload >/dev/null 2>&1 || true
        systemctl reset-failed jhvirt-keycloak-helper.service >/dev/null 2>&1 || true
    fi
}

uninstall_systemd() {
    if systemd_install_present; then
        step "остановка и удаление jhvirt.service"
        if have systemctl; then
            systemctl disable --now jhvirt >/dev/null 2>&1 || true
            systemctl disable --now jhvirt-dr-backup.timer >/dev/null 2>&1 || true
            systemctl stop jhvirt-dr-backup.service >/dev/null 2>&1 || true
        fi
        rm -f "$UNIT" "$DR_UNIT" "$DR_TIMER"
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
				rm -f "$dir/.recovery-token"
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
		rm -f "$PREFIX/config/recovery.token"
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
  ./install.sh --mode docker|docker-compose|systemd --url https://host:8080"
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
    # Скрытый вариант без объяснения выглядит как отсутствие возможности.
    # Чаще всего Docker установлен, а служба просто не запущена — и это видно
    # только тому, кто догадается проверить сам.
    if ! has_docker && ! has_dockerc; then
        DOCKER_WHY="$(docker_unavailable_reason)"
        [ "$DOCKER_WHY" = "docker не установлен" ] ||
            say "Вариант с Docker скрыт: $DOCKER_WHY"
    fi
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
    # Отправку предлагаем сразу: нести пакет руками — это scp из-под sudo,
    # чужие ключи и права 0600, на чём спотыкаются в первый же раз.
    if [ -z "$MIGRATION_TO" ]; then
        say ""
        say "Отправить пакет на новый сервер сразу после создания?"
        say "Укажите user@сервер:/каталог либо оставьте пустым — тогда пакет"
        say "останется здесь и его нужно будет перенести самостоятельно."
        printf 'Куда отправить: '
        read -r MIGRATION_TO || MIGRATION_TO=""
    fi
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
        [ -t 0 ] || die "без диалога внешний адрес обязателен: --url https://host:$PORT"
        HOST="$(hostname -I 2>/dev/null | awk '{print $1}')"
        [ -n "$HOST" ] || HOST="$(hostname -f 2>/dev/null || hostname)"
        GUESS="https://$HOST:$PORT"
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
        case "$URL" in https://*) TLS_MODE=self-signed ;; *) TLS_MODE=none ;; esac
        return 0
    fi

    say ""
    say "Как обслуживать HTTPS?"
    say "  1) создать самоподписанный сертификат (по умолчанию; CA можно заменить позже)"
    say "  2) TLS на reverse proxy — приложение слушает только 127.0.0.1"
    say "  3) подключить существующие сертификат и закрытый ключ"
    while :; do
        printf 'Номер [1]: '
        read -r TLS_CHOICE || TLS_CHOICE=""
        [ -n "$TLS_CHOICE" ] || TLS_CHOICE=1
        case "$TLS_CHOICE" in
            1) TLS_MODE=self-signed; return 0 ;;
            2) TLS_MODE=none; return 0 ;;
            3) TLS_MODE=files; return 0 ;;
            *) say "Нет такого варианта." ;;
        esac
    done
}

url_host() {
    URL_HOST_AUTHORITY="${1#*://}"
    URL_HOST_AUTHORITY="${URL_HOST_AUTHORITY%%/*}"
    case "$URL_HOST_AUTHORITY" in
        \[*\]*) URL_HOST_VALUE="${URL_HOST_AUTHORITY#\[}"; URL_HOST_VALUE="${URL_HOST_VALUE%%\]*}" ;;
        *) URL_HOST_VALUE="${URL_HOST_AUTHORITY%%:*}" ;;
    esac
    printf '%s' "$URL_HOST_VALUE"
}

tls_host_from_url() {
    TLS_HOST="$(url_host "$URL")"
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
    TV_CERT_PUBLIC="$(openssl x509 -in "$TV_CERT" -pubkey -noout)" ||
        die "не удалось прочитать публичный ключ сертификата: $TV_CERT"
    TV_KEY_PUBLIC="$(openssl pkey -in "$TV_KEY" -passin pass: -pubout)" ||
        die "не удалось получить публичный ключ из закрытого ключа: $TV_KEY"
    [ "$TV_CERT_PUBLIC" = "$TV_KEY_PUBLIC" ] ||
        die "сертификат и закрытый ключ не образуют пару"
}

prepare_tls() {
    case "$TLS_MODE" in
        none)
            case "$URL" in
                https://*) BIND_ADDRESS=127.0.0.1 ;;
                http://*)
                    PLAIN_HOST="$(url_host "$URL")"
                    case "$PLAIN_HOST" in localhost|127.*|::1) BIND_ADDRESS=127.0.0.1 ;;
                        *) [ "$ALLOW_HTTP" -eq 1 ] || die "сетевой HTTP запрещён по умолчанию. Используйте HTTPS; для изолированного тестового стенда явно задайте --allow-http --tls none." ;;
                    esac
                    ;;
            esac
            return 0 ;;
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
        # Git Bash на Windows иначе превращает /CN=... в C:/Program Files/Git/CN=...
        # как будто это Unix-путь. Исключение касается только subject; пути к
        # ключу и сертификату MSYS продолжает переводить для Windows openssl.
        MSYS2_ARG_CONV_EXCL="/CN=" openssl req -x509 -newkey rsa:3072 -sha256 -nodes -days "$TLS_DAYS" \
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
        "$POSTGRES_HELPER_IMAGE" sh -c '
            mkdir -p /data/tls
            umask 077
            cat > /data/tls/server.key
            chown 10001:10001 /data/tls/server.key
            chmod 600 /data/tls/server.key' < "$TLS_MATERIAL_DIR/server.key" ||
        die "не удалось записать TLS-ключ в том $ITD_VOL"
    docker run --rm -i --network none --user root -v "$ITD_VOL:/data" \
        "$POSTGRES_HELPER_IMAGE" sh -c '
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
        $CHECK_RUN ps -q 2>/dev/null || true
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
  $SELF --mode $MODE --url https://host:$CHECK_SUGGESTED --port $CHECK_SUGGESTED"
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

# Внешний OIDC доступен в обоих режимах; встроенный Keycloak требует Compose.
choose_oidc() {
    [ -z "$OIDC_MODE" ] || return 0
    if [ ! -t 0 ]; then
        OIDC_MODE=none
        return 0
    fi

    say ""
    say "Как входить в систему?"
    say ""
    say "  1) только по паролю — учётные записи ведутся здесь (по умолчанию)"
    if [ "$MODE" != systemd ]; then
        say "  2) поднять Keycloak рядом — AD и обязательный второй фактор"
    fi
    say "  3) подключить существующий Keycloak или другой OIDC-провайдер"
    say ""
    say "При OIDC локальный парольный вход настраивается отдельно."
    say ""
    while :; do
        printf 'Номер [1]: '
        read -r OIDC_CHOICE || OIDC_CHOICE=""
        [ -n "$OIDC_CHOICE" ] || OIDC_CHOICE=1
        case "$OIDC_CHOICE" in
            1) OIDC_MODE=none; return 0 ;;
            2) if [ "$MODE" != systemd ]; then OIDC_MODE=keycloak; return 0; fi ;;
            3) OIDC_MODE=external; return 0 ;;
            *) say "Нет такого варианта." ;;
        esac
    done
}

# Обновление сохраняет уже выбранный способ входа. Без этого unattended-запуск
# считал OIDC выключенным и не добавлял новые обязательные параметры в старый
# .env, хотя профиль Keycloak продолжал запускаться самим Compose.
load_existing_oidc() {
    [ -f "$WORK/.env" ] || return 0
    [ "$(env_file_value "$WORK/.env" JHV_OIDC_ENABLED)" = true ] || return 0

    case "$(env_file_value "$WORK/.env" COMPOSE_PROFILES)" in
        *keycloak*) EXISTING_OIDC_MODE=keycloak ;;
        *) EXISTING_OIDC_MODE=external ;;
    esac
    # Явный повтор того же режима остаётся обновлением, а не новой настройкой.
    # Переход на другой режим, напротив, должен пройти его обычную валидацию.
    if [ -n "$OIDC_MODE" ] && [ "$OIDC_MODE" != "$EXISTING_OIDC_MODE" ]; then
        return 0
    fi
    OIDC_EXISTING=1
    [ -n "$OIDC_MODE" ] || OIDC_MODE="$EXISTING_OIDC_MODE"

    OIDC_STORED="$(env_file_value "$WORK/.env" JHV_OIDC_ISSUER)"
    [ -n "$OIDC_ISSUER" ] || OIDC_ISSUER="$OIDC_STORED"
    OIDC_STORED="$(env_file_value "$WORK/.env" JHV_OIDC_BACKCHANNEL_URL)"
    [ -n "$OIDC_BACKCHANNEL_URL" ] || OIDC_BACKCHANNEL_URL="$OIDC_STORED"
    OIDC_STORED="$(env_file_value "$WORK/.env" JHV_OIDC_CLIENT_ID)"
    [ -n "$OIDC_CLIENT_ID" ] || OIDC_CLIENT_ID="$OIDC_STORED"
    OIDC_CLIENT_SECRET="$(env_file_value "$WORK/.env" JHV_OIDC_CLIENT_SECRET)"
    [ -n "$OIDC_CLIENT_SECRET" ] || OIDC_CLIENT_SECRET="$(docker_volume_value "$(docker_metrics_volume)" oidc-client.secret)"
    if [ -z "$OIDC_ALLOW_LOCAL_LOGIN" ]; then
        OIDC_ALLOW_LOCAL_LOGIN="$(env_file_value "$WORK/.env" JHV_OIDC_ALLOW_LOCAL_LOGIN)"
        # До появления явного выбора установщик всегда оставлял локальный
        # вход включённым. Обновление такой установки не должно запереть её.
        [ -n "$OIDC_ALLOW_LOCAL_LOGIN" ] || OIDC_ALLOW_LOCAL_LOGIN=true
    fi
    case "$EXISTING_OIDC_MODE" in
        keycloak)
            OIDC_STORED="$(env_file_value "$WORK/.env" KEYCLOAK_REALM)"
            [ -z "$OIDC_STORED" ] || KEYCLOAK_REALM="$OIDC_STORED"
            OIDC_STORED="$(env_file_value "$WORK/.env" JHV_KEYCLOAK_URL)"
            [ -n "$KEYCLOAK_URL" ] || KEYCLOAK_URL="$OIDC_STORED"
            KEYCLOAK_ADMIN_USER="$(env_file_value "$WORK/.env" KEYCLOAK_ADMIN_USER)"
            [ -n "$KEYCLOAK_ADMIN_USER" ] || KEYCLOAK_ADMIN_USER=kc-bootstrap-admin
            if [ "$KEYCLOAK_APP_ADMIN_USER_EXPLICIT" -eq 0 ]; then
                OIDC_STORED="$(env_file_value "$WORK/.env" JHV_KEYCLOAK_APP_ADMIN_USER)"
                [ -z "$OIDC_STORED" ] || KEYCLOAK_APP_ADMIN_USER="$OIDC_STORED"
            fi
            OIDC_STORED="$(env_file_value "$WORK/.env" KEYCLOAK_PORT)"
            if [ "$KEYCLOAK_PORT_EXPLICIT" -eq 0 ] && [ -n "$OIDC_STORED" ]; then
                KEYCLOAK_PORT="$OIDC_STORED"
            fi
            [ -n "$KEYCLOAK_PORT" ] || KEYCLOAK_PORT=8081
            KEYCLOAK_DIRECT_TLS="$(env_file_value "$WORK/.env" KEYCLOAK_DIRECT_TLS)"
            [ "$KEYCLOAK_DIRECT_TLS" = 1 ] || KEYCLOAK_DIRECT_TLS=0
            KEYCLOAK_BIND_ADDRESS="$(env_file_value "$WORK/.env" KEYCLOAK_BIND_ADDRESS)"
            [ -n "$KEYCLOAK_BIND_ADDRESS" ] || KEYCLOAK_BIND_ADDRESS=127.0.0.1
            KEYCLOAK_CONTAINER_PORT="$(env_file_value "$WORK/.env" KEYCLOAK_CONTAINER_PORT)"
            [ -n "$KEYCLOAK_CONTAINER_PORT" ] || KEYCLOAK_CONTAINER_PORT=8080
            if [ "$KEYCLOAK_AD_PROVIDER_EXPLICIT" -eq 0 ]; then
                OIDC_STORED="$(env_file_value "$WORK/.env" JHV_KEYCLOAK_AD_PROVIDER)"
                [ -z "$OIDC_STORED" ] || KEYCLOAK_AD_PROVIDER="$OIDC_STORED"
            fi
            OIDC_STORED="$(env_file_value "$WORK/.env" JHV_KEYCLOAK_AD_DOMAIN)"
            [ -n "$KEYCLOAK_AD_DOMAIN" ] || KEYCLOAK_AD_DOMAIN="$OIDC_STORED"
            OIDC_STORED="$(env_file_value "$WORK/.env" JHV_KEYCLOAK_AD_CONTROLLER)"
            [ -n "$KEYCLOAK_AD_CONTROLLER" ] || KEYCLOAK_AD_CONTROLLER="$OIDC_STORED"
            OIDC_STORED="$(env_file_value "$WORK/.env" JHV_KEYCLOAK_AD_URL)"
            [ -n "$KEYCLOAK_AD_URL" ] || KEYCLOAK_AD_URL="$OIDC_STORED"
            OIDC_STORED="$(env_file_value "$WORK/.env" JHV_KEYCLOAK_AD_USERS_DN)"
            [ -n "$KEYCLOAK_AD_USERS_DN" ] || KEYCLOAK_AD_USERS_DN="$OIDC_STORED"
            OIDC_STORED="$(env_file_value "$WORK/.env" JHV_KEYCLOAK_AD_GROUPS_DN)"
            [ -n "$KEYCLOAK_AD_GROUPS_DN" ] || KEYCLOAK_AD_GROUPS_DN="$OIDC_STORED"
            OIDC_STORED="$(env_file_value "$WORK/.env" JHV_KEYCLOAK_AD_BIND_DN)"
            [ -n "$KEYCLOAK_AD_BIND_DN" ] || KEYCLOAK_AD_BIND_DN="$OIDC_STORED"
            say "    сохраняется вход через встроенный Keycloak"
            ;;
        external)
            say "    сохраняется вход через внешний OIDC-провайдер"
            ;;
    esac
}

ask_nonempty() {
    ANSWER=""
    while [ -z "$ANSWER" ]; do
        printf '%s' "$1"
        read -r ANSWER || ANSWER=""
    done
}

validate_ad_domain() {
    printf '%s\n' "$1" | awk -F. '
        NF < 2 { exit 1 }
        {
            for (i = 1; i <= NF; i++) {
                if (length($i) < 1 || length($i) > 63 ||
                        $i !~ /^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$/) exit 1
            }
        }
    '
}

ad_domain_to_base_dn() {
    printf '%s\n' "$1" | awk -F. '{
        for (i = 1; i <= NF; i++) {
            if (i > 1) printf ","
            printf "DC=%s", $i
        }
    }'
}

discover_ad_controller() {
    DAC_QUERY="_ldap._tcp.dc._msdcs.$1"
    DAC_RESULT=""
    if have dig; then
        DAC_RESULT="$(dig +short SRV "$DAC_QUERY" 2>/dev/null |
            awk 'NF >= 4 { print $4; exit }')"
    elif have host; then
        DAC_RESULT="$(host -t SRV "$DAC_QUERY" 2>/dev/null |
            awk '/SRV record/ { print $NF; exit }')"
    elif have nslookup; then
        DAC_RESULT="$(nslookup -type=SRV "$DAC_QUERY" 2>/dev/null |
            awk '/service =|svr hostname =/ { print $NF; exit }')"
    fi
    printf '%s' "${DAC_RESULT%.}"
}

read_hidden_value() (
    printf '%s' "$1" >&2
    trap 'stty echo 2>/dev/null || true; printf "\n" >&2; exit 130' HUP INT TERM
    stty -echo || exit 1
    IFS= read -r RHV_VALUE
    RHV_STATUS=$?
    stty echo || true
    printf '\n' >&2
    [ "$RHV_STATUS" -eq 0 ] || exit "$RHV_STATUS"
    printf '%s' "$RHV_VALUE"
)

prepare_keycloak_ad_simple() {
    KEYCLOAK_AD_SIMPLE=1
    if [ -z "$KEYCLOAK_AD_DOMAIN" ] && [ -t 0 ]; then
        ask_nonempty "DNS-домен Active Directory, например example.org: "
        KEYCLOAK_AD_DOMAIN="$ANSWER"
    fi
    KEYCLOAK_AD_DOMAIN="$(printf '%s' "${KEYCLOAK_AD_DOMAIN%.}" | tr '[:upper:]' '[:lower:]')"
    [ -n "$KEYCLOAK_AD_DOMAIN" ] ||
        die "для простой настройки укажите --keycloak-ad-domain"
    while ! validate_ad_domain "$KEYCLOAK_AD_DOMAIN"; do
        [ -t 0 ] || die "некорректный DNS-домен Active Directory"
        say "Некорректное имя DNS-домена; повторите ввод."
        ask_nonempty "DNS-домен: "; KEYCLOAK_AD_DOMAIN="$ANSWER"
    done

    KC_AD_BASE_DN="$(ad_domain_to_base_dn "$KEYCLOAK_AD_DOMAIN")"
    [ -n "$KEYCLOAK_AD_USERS_DN" ] || KEYCLOAK_AD_USERS_DN="$KC_AD_BASE_DN"
    [ -n "$KEYCLOAK_AD_GROUPS_DN" ] || KEYCLOAK_AD_GROUPS_DN="$KC_AD_BASE_DN"

    if [ -z "$KEYCLOAK_AD_URL" ]; then
        if [ -z "$KEYCLOAK_AD_CONTROLLER" ]; then
            KEYCLOAK_AD_CONTROLLER="$(discover_ad_controller "$KEYCLOAK_AD_DOMAIN")"
            if [ -t 0 ]; then
                if [ -n "$KEYCLOAK_AD_CONTROLLER" ]; then
                    printf 'Контроллер домена [%s]: ' "$KEYCLOAK_AD_CONTROLLER"
                    read -r ANSWER || ANSWER=""
                    [ -z "$ANSWER" ] || KEYCLOAK_AD_CONTROLLER="$ANSWER"
                else
                    ask_nonempty "DNS-имя контроллера домена: "
                    KEYCLOAK_AD_CONTROLLER="$ANSWER"
                fi
            fi
        fi
        [ -n "$KEYCLOAK_AD_CONTROLLER" ] || die "контроллер домена не найден через DNS SRV
Укажите --keycloak-ad-controller с полным DNS-именем DC."
        KEYCLOAK_AD_CONTROLLER="${KEYCLOAK_AD_CONTROLLER%.}"
        case "$KEYCLOAK_AD_CONTROLLER" in
            *://*|*/*|*[!A-Za-z0-9._-]*) die "некорректное DNS-имя контроллера: $KEYCLOAK_AD_CONTROLLER" ;;
        esac
        case "$KEYCLOAK_AD_CONTROLLER" in
            *.*) ;;
            *) KEYCLOAK_AD_CONTROLLER="$KEYCLOAK_AD_CONTROLLER.$KEYCLOAK_AD_DOMAIN" ;;
        esac
        KEYCLOAK_AD_URL="ldaps://$KEYCLOAK_AD_CONTROLLER:636"
    fi

    if [ -z "$KEYCLOAK_AD_BIND_DN" ] && [ -t 0 ]; then
        ask_nonempty "Bind-пользователь только для чтения (UPN или DN): "
        KEYCLOAK_AD_BIND_DN="$ANSWER"
    fi
    case "$KEYCLOAK_AD_BIND_DN" in
        ""|*@*|*=*) ;;
        *) KEYCLOAK_AD_BIND_DN="$KEYCLOAK_AD_BIND_DN@$KEYCLOAK_AD_DOMAIN" ;;
    esac

    if [ -z "$KEYCLOAK_AD_BIND_PASSWORD_FILE" ] &&
            [ -z "$KEYCLOAK_AD_BIND_PASSWORD" ] && [ -t 0 ]; then
        while :; do
        KEYCLOAK_AD_BIND_PASSWORD="$(read_hidden_value 'Пароль bind-пользователя: ')" ||
            die "не удалось безопасно прочитать bind-пароль"
        KC_AD_PASSWORD_CONFIRM="$(read_hidden_value 'Повторите пароль: ')" ||
            die "не удалось безопасно прочитать подтверждение bind-пароля"
        if [ -n "$KEYCLOAK_AD_BIND_PASSWORD" ] && [ "$KEYCLOAK_AD_BIND_PASSWORD" = "$KC_AD_PASSWORD_CONFIRM" ]; then break; fi
        say "Пароли пусты или не совпадают; повторите ввод."
        done
        KC_AD_PASSWORD_CONFIRM=""
        KEYCLOAK_AD_PASSWORD_WAS_INTERACTIVE=1
    fi

    if [ -z "$KEYCLOAK_AD_CA_FILE" ] && [ -t 0 ]; then
        if [ "$OIDC_EXISTING" -eq 1 ]; then
            printf 'PEM-файл корпоративного CA [использовать уже установленный]: '
            read -r KEYCLOAK_AD_CA_FILE || KEYCLOAK_AD_CA_FILE=""
        else
            ask_nonempty "PEM-файл корневого/промежуточного CA: "
            KEYCLOAK_AD_CA_FILE="$ANSWER"
        fi
    fi

    say "    домен AD: $KEYCLOAK_AD_DOMAIN"
    say "    контроллер: $KEYCLOAK_AD_URL"
    say "    поиск пользователей и групп: $KC_AD_BASE_DN (Subtree)"
}

keycloak_use_direct_tls() {
    KEYCLOAK_DIRECT_TLS=1
    KEYCLOAK_BIND_ADDRESS=0.0.0.0
    KEYCLOAK_CONTAINER_PORT=8443
    KEYCLOAK_API_URL="https://127.0.0.1:$KEYCLOAK_PORT"
}

prepare_keycloak_ad() {
    [ "$OIDC_MODE" = keycloak ] || {
        [ "$KEYCLOAK_AD_REQUESTED" -eq 0 ] || die "--keycloak-ad работает только вместе с --oidc keycloak"
        return 0
    }

    if [ -t 0 ] && [ "$KEYCLOAK_AD_REQUESTED" -eq 0 ]; then
        say ""
        say "Active Directory можно подключить сейчас или позднее повторным запуском .run."
        say "Достаточно DNS-домена, контроллера, bind-пользователя и корпоративного CA."
        say "Пароль вводится скрыто; разрешён только LDAPS."
        printf 'Настроить Active Directory сейчас? [y/N]: '
        read -r ANSWER || ANSWER=""
        case "$ANSWER" in
            y|Y|yes|YES|д|Д|да|ДА) KEYCLOAK_AD_REQUESTED=1 ;;
        esac
    fi
    [ "$KEYCLOAK_AD_REQUESTED" -eq 1 ] || return 0

    if [ -n "$KEYCLOAK_AD_DOMAIN" ] || [ -n "$KEYCLOAK_AD_CONTROLLER" ]; then
        KEYCLOAK_AD_SIMPLE=1
    elif [ -t 0 ] && [ -z "$KEYCLOAK_AD_URL" ] &&
            [ -z "$KEYCLOAK_AD_USERS_DN" ] && [ -z "$KEYCLOAK_AD_GROUPS_DN" ]; then
        say ""
        printf 'Настройка AD: 1 — по DNS-домену, 2 — расширенная [1]: '
        read -r ANSWER || ANSWER=""
        case "$ANSWER" in
            2) KEYCLOAK_AD_SIMPLE=0 ;;
            *) KEYCLOAK_AD_SIMPLE=1 ;;
        esac
    fi

    if [ "$KEYCLOAK_AD_SIMPLE" -eq 1 ]; then
        prepare_keycloak_ad_simple
    elif [ -t 0 ]; then
        [ -n "$KEYCLOAK_AD_URL" ] || {
            ask_nonempty "LDAPS URL контроллера, например ldaps://dc01.example.org:636: "
            KEYCLOAK_AD_URL="$ANSWER"
        }
        [ -n "$KEYCLOAK_AD_USERS_DN" ] || {
            ask_nonempty "Users DN, например OU=Users,DC=example,DC=org: "
            KEYCLOAK_AD_USERS_DN="$ANSWER"
        }
        [ -n "$KEYCLOAK_AD_GROUPS_DN" ] || {
            ask_nonempty "Groups DN с группами допуска: "
            KEYCLOAK_AD_GROUPS_DN="$ANSWER"
        }
        [ -n "$KEYCLOAK_AD_BIND_DN" ] || {
            ask_nonempty "Bind DN read-only service account: "
            KEYCLOAK_AD_BIND_DN="$ANSWER"
        }
        [ -n "$KEYCLOAK_AD_BIND_PASSWORD_FILE" ] || {
            ask_nonempty "Файл 0600 с bind-паролем: "
            KEYCLOAK_AD_BIND_PASSWORD_FILE="$ANSWER"
        }
        if [ -z "$KEYCLOAK_AD_CA_FILE" ]; then
            printf 'PEM bundle корпоративного CA [использовать уже установленный]: '
            read -r KEYCLOAK_AD_CA_FILE || KEYCLOAK_AD_CA_FILE=""
        fi
    fi

    case "$KEYCLOAK_AD_PROVIDER" in
        ""|*[!A-Za-z0-9._-]*) die "--keycloak-ad-provider: допустимы A-Z, a-z, 0-9, точка, _ и -" ;;
    esac
    [ -n "$KEYCLOAK_AD_URL" ] || die "--keycloak-ad требует --keycloak-ad-url"
    for KC_AD_ONE_URL in $KEYCLOAK_AD_URL; do
        case "$KC_AD_ONE_URL" in
            ldaps://*:*) ;;
            *) die "--keycloak-ad-url принимает только ldaps:// URL с портом; незашифрованный LDAP запрещён" ;;
        esac
    done
    [ -n "$KEYCLOAK_AD_USERS_DN" ] || die "--keycloak-ad требует --keycloak-ad-users-dn"
    [ -n "$KEYCLOAK_AD_GROUPS_DN" ] || die "--keycloak-ad требует --keycloak-ad-groups-dn"
    [ -n "$KEYCLOAK_AD_BIND_DN" ] || die "--keycloak-ad требует --keycloak-ad-bind-dn"
    if [ -n "$KEYCLOAK_AD_BIND_PASSWORD_FILE" ]; then
        [ -f "$KEYCLOAK_AD_BIND_PASSWORD_FILE" ] || die "нет файла с bind-паролем: $KEYCLOAK_AD_BIND_PASSWORD_FILE"
        KC_AD_SECRET_MODE="$(stat -c '%a' "$KEYCLOAK_AD_BIND_PASSWORD_FILE" 2>/dev/null || true)"
        [ "$KC_AD_SECRET_MODE" = 600 ] || die "$KEYCLOAK_AD_BIND_PASSWORD_FILE должен иметь права 0600 (сейчас ${KC_AD_SECRET_MODE:-неизвестно})"
        [ "$(wc -l < "$KEYCLOAK_AD_BIND_PASSWORD_FILE" | tr -d '[:space:]')" -le 1 ] ||
            die "bind-пароль должен занимать одну строку"
        [ -n "$(tr -d '\r\n' < "$KEYCLOAK_AD_BIND_PASSWORD_FILE")" ] ||
            die "файл с bind-паролем пуст"
    else
        [ -n "$KEYCLOAK_AD_BIND_PASSWORD" ] ||
            die "в unattended-режиме укажите --keycloak-ad-bind-password-file"
    fi

    case "$KEYCLOAK_AD_GROUP_MODE" in
        read-only) KEYCLOAK_AD_GROUP_MODE_API=READ_ONLY ;;
        ldap-only) KEYCLOAK_AD_GROUP_MODE_API=LDAP_ONLY ;;
        *) die "--keycloak-ad-group-mode принимает ldap-only или read-only" ;;
    esac

    if [ -n "$KEYCLOAK_AD_CA_FILE" ]; then
        [ -f "$KEYCLOAK_AD_CA_FILE" ] || die "нет PEM bundle CA: $KEYCLOAK_AD_CA_FILE"
        grep -q -- '-----BEGIN CERTIFICATE-----' "$KEYCLOAK_AD_CA_FILE" ||
            die "$KEYCLOAK_AD_CA_FILE не содержит PEM-сертификат"
        if grep -Eq -- '-----BEGIN ([A-Z ]*)PRIVATE KEY-----' "$KEYCLOAK_AD_CA_FILE"; then
            die "$KEYCLOAK_AD_CA_FILE содержит закрытый ключ; truststore должен содержать только сертификаты CA"
        fi
    elif [ "$OIDC_EXISTING" -eq 0 ]; then
        die "новая настройка LDAPS требует --keycloak-ad-ca-file с цепочкой корпоративного CA"
    fi
    # Host-side preflight precedes changes to the LDAP provider. The Keycloak
    # test below separately exercises container DNS, its truststore and bind.
    if [ -n "$KEYCLOAK_AD_CA_FILE" ]; then
        while ! setup_tool tls-check "$KEYCLOAK_AD_URL" "$KEYCLOAK_AD_CA_FILE"; do
            [ -t 0 ] || die "предварительная проверка LDAPS не пройдена"
            say "Исправьте DNS/доступ к DC или укажите правильную цепочку CA."
            ask_nonempty "PEM-файл CA для повторной проверки: "; KEYCLOAK_AD_CA_FILE="$ANSWER"
        done
    fi
    setup_tool ldap-filter "$GROUP_ADMIN" "$GROUP_OPERATOR" "$GROUP_VIEWER" >/dev/null || die "неверные имена групп"
}

# Проверяет, что для выбранного способа хватает данных, и добирает недостающее
# вопросами. Без терминала недостающее — это ошибка, а не повод продолжить с
# наполовину настроенным входом.
prepare_oidc() {
    case "$OIDC_MODE" in
        ""|none)
            OIDC_MODE=none
            [ "$KEYCLOAK_AD_REQUESTED" -eq 0 ] ||
                die "--keycloak-ad работает только вместе с --oidc keycloak"
            case "$OIDC_ALLOW_LOCAL_LOGIN" in
                ""|enabled|true) OIDC_ALLOW_LOCAL_LOGIN=true ;;
                disabled|false) die "нельзя отключить единственный способ входа при --oidc none" ;;
                *) die "--local-login принимает enabled или disabled" ;;
            esac
            return 0
            ;;
        keycloak|external) ;;
        *) die "--oidc принимает none, keycloak или external" ;;
    esac

    if [ "$MODE" = systemd ] && [ "$OIDC_MODE" = keycloak ]; then
        die "для systemd используйте --oidc external; встроенный Keycloak устанавливается в Docker"
    fi
    have curl || die "для настройки внешнего входа нужен curl"
    case "$URL" in https://*) ;; *) die "OIDC требует HTTPS-адрес приложения" ;; esac

    if [ "$OIDC_MODE" = external ]; then
        if [ -t 0 ]; then
            [ -n "$OIDC_ISSUER" ] || { ask_nonempty "Адрес realm (issuer), например https://keycloak.example.org/realms/infra: "; OIDC_ISSUER="$ANSWER"; }
            [ -n "$OIDC_CLIENT_ID" ] || { ask_nonempty "Идентификатор клиента [jhvirt]: "; OIDC_CLIENT_ID="$ANSWER"; }
            if [ -z "$OIDC_CLIENT_SECRET_FILE" ] && [ -z "${OIDC_CLIENT_SECRET:-}" ]; then
                OIDC_CLIENT_SECRET="$(read_hidden_value 'Секрет клиента: ')" || die "не удалось прочитать секрет"
            fi
        fi
        [ -n "$OIDC_ISSUER" ] || die "--oidc external требует --oidc-issuer"
        case "$OIDC_ISSUER" in https://*) ;; *) die "внешний OIDC issuer должен использовать HTTPS" ;; esac
        # Чужой провайдер стоит не здесь: петлевой адрес к нему не ведёт.
        KEYCLOAK_API_URL="$KEYCLOAK_URL"
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
            KC_PUBLIC_HOST="$(url_host "$URL")"
            case "$KC_PUBLIC_HOST" in *:*) KC_PUBLIC_AUTHORITY="[$KC_PUBLIC_HOST]" ;; *) KC_PUBLIC_AUTHORITY="$KC_PUBLIC_HOST" ;; esac
            case "$URL" in
                https://*)
                    case "$TLS_MODE" in
                        self-signed|files|preserve)
                            keycloak_use_direct_tls
                            KEYCLOAK_URL="https://$KC_PUBLIC_AUTHORITY:$KEYCLOAK_PORT"
                            ;;
                        *)
                            die "для встроенного Keycloak за HTTPS reverse proxy укажите его публичный адрес:
  --keycloak-url https://keycloak.example.org
Прокси должен направлять этот адрес на 127.0.0.1:$KEYCLOAK_PORT. Либо включите
собственный TLS: --tls self-signed"
                            ;;
                    esac
                    ;;
                *)
                    case "$KC_PUBLIC_HOST" in
                        localhost|127.*|::1) ;;
                        *) die "встроенный Keycloak нельзя публиковать по HTTP: пароль входа пойдёт по сети открытым текстом.
Используйте --tls self-signed, сертификат из файлов или HTTPS reverse proxy." ;;
                    esac
                    KEYCLOAK_BIND_ADDRESS=127.0.0.1
                    KEYCLOAK_URL="http://$KC_PUBLIC_AUTHORITY:$KEYCLOAK_PORT"
                    KEYCLOAK_API_URL="http://127.0.0.1:$KEYCLOAK_PORT"
                    ;;
            esac
        elif [ "$OIDC_EXISTING" -eq 1 ] && [ "$KEYCLOAK_DIRECT_TLS" -eq 0 ] &&
                [ "$TLS_MODE" != none ] &&
                [ "${KEYCLOAK_URL#http://}" != "$KEYCLOAK_URL" ]; then
            # Безопасно обновляем прежнюю встроенную HTTP-установку: тот же
            # сертификат уже выбран для приложения и подходит тому же хосту.
            KC_PUBLIC_HOST="$(url_host "$URL")"
            case "$KC_PUBLIC_HOST" in *:*) KC_PUBLIC_AUTHORITY="[$KC_PUBLIC_HOST]" ;; *) KC_PUBLIC_AUTHORITY="$KC_PUBLIC_HOST" ;; esac
            keycloak_use_direct_tls
            KEYCLOAK_URL="https://$KC_PUBLIC_AUTHORITY:$KEYCLOAK_PORT"
            say "    встроенный Keycloak переводится с HTTP на HTTPS"
        elif [ "$OIDC_EXISTING" -eq 1 ] && [ "$KEYCLOAK_DIRECT_TLS" -eq 1 ]; then
            keycloak_use_direct_tls
        else
            case "$KEYCLOAK_URL" in
                https://*)
                    # Тот же host и собственный сертификат означают прямой TLS
                    # Keycloak. Другой host обслуживает внешний reverse proxy,
                    # которому оставляем только loopback HTTP upstream.
                    KC_PUBLIC_HOST="$(url_host "$URL")"
                    KC_EXPLICIT_HOST="$(url_host "$KEYCLOAK_URL")"
                    case "$TLS_MODE:$KC_PUBLIC_HOST:$KC_EXPLICIT_HOST" in
                        self-signed:*:*|files:*:*|preserve:*:*)
                            if [ "$KC_PUBLIC_HOST" = "$KC_EXPLICIT_HOST" ]; then
                                keycloak_use_direct_tls
                            else
                                KEYCLOAK_BIND_ADDRESS=127.0.0.1
                                KEYCLOAK_CONTAINER_PORT=8080
                                KEYCLOAK_API_URL="http://127.0.0.1:$KEYCLOAK_PORT"
                            fi
                            ;;
                        *)
                            KEYCLOAK_BIND_ADDRESS=127.0.0.1
                            KEYCLOAK_CONTAINER_PORT=8080
                            KEYCLOAK_API_URL="http://127.0.0.1:$KEYCLOAK_PORT"
                            ;;
                    esac
                    ;;
                http://localhost*|http://127.*|http://\[::1\]*)
                    KEYCLOAK_BIND_ADDRESS=127.0.0.1
                    KEYCLOAK_API_URL="http://127.0.0.1:$KEYCLOAK_PORT"
                    ;;
                http://*)
                    die "публичный --keycloak-url должен использовать HTTPS"
                    ;;
                *) die "--keycloak-url должен начинаться с http:// или https://" ;;
            esac
        fi
        validate_port "$KEYCLOAK_PORT" || die "--keycloak-port: нужен номер от 1 до 65535"
        # Сам установщик ходит к Keycloak на петлевой адрес, а не на внешний.
        #
        # Контейнер поднимается здесь же, и обращение с этой машины на её
        # собственный внешний IP идёт через цепочку INPUT firewall. Порт службы
        # обычно открыт, а порт Keycloak — нет, и настройка упиралась в
        # «Keycloak не ответил за три минуты» при живом контейнере. Готовность
        # самой службы проверяется по 127.0.0.1 ровно по этой причине.
        #
        # Внешний адрес остаётся тем, чем и был: issuer токенов и KC_HOSTNAME.
        [ -n "$KEYCLOAK_API_URL" ] || KEYCLOAK_API_URL="http://127.0.0.1:$KEYCLOAK_PORT"
        OIDC_BACKCHANNEL_URL="http://keycloak:8080"
        OIDC_ISSUER="$KEYCLOAK_URL/realms/$KEYCLOAK_REALM"
        OIDC_CLIENT_ID="jhvirt"
        OIDC_CLIENT_SECRET="$(gen_secret 24)"
        KEYCLOAK_APP_ADMIN_PASSWORD="$(gen_secret 18)"
        if [ "$OIDC_EXISTING" -eq 0 ]; then
            KEYCLOAK_ADMIN_PASSWORD="$(gen_secret 18)"
            KEYCLOAK_BOOTSTRAP_USER="kc-install-bootstrap-$(gen_secret 6)"
            KEYCLOAK_ACTIVE_ADMIN_USER="$KEYCLOAK_BOOTSTRAP_USER"
            KEYCLOAK_ACTIVE_ADMIN_PASSWORD="$KEYCLOAK_ADMIN_PASSWORD"
        else
            KEYCLOAK_RECOVERY_PASSWORD="$(gen_secret 18)"
            KEYCLOAK_ADMIN_PASSWORD=""
            KEYCLOAK_ACTIVE_ADMIN_USER="$KEYCLOAK_ADMIN_USER"
            KEYCLOAK_ACTIVE_ADMIN_PASSWORD=""
        fi
        [ -n "$OIDC_CLIENT_SECRET" ] && [ -n "$KEYCLOAK_APP_ADMIN_PASSWORD" ] &&
            { [ -n "$KEYCLOAK_ADMIN_PASSWORD" ] || [ -n "$KEYCLOAK_RECOVERY_PASSWORD" ]; } ||
            die "не удалось сгенерировать секреты для Keycloak"
        if [ "$OIDC_EXISTING" -eq 0 ] && [ -z "$KEYCLOAK_BOOTSTRAP_USER" ]; then
            die "не удалось сгенерировать имя bootstrap-администратора Keycloak"
        fi
    fi

    if [ -t 0 ] && [ "$OIDC_MODE" != none ]; then
        say ""
        say "Группы допуска. Кто не попал ни в одну — в систему не допускается."
        # if, а не «[ -n ... ] && VAR=...». Разница не косметическая: при
        # пустом ответе такая строка возвращает 1, и если она стоит последней в
        # функции, её статус становится статусом функции. Со `set -e` в начале
        # скрипта это молчаливый выход посреди установки — ровно на самом
        # частом ответе, когда все три умолчания принимают Enter'ом.
        if [ "$OIDC_GROUPS_EXISTING" -eq 0 ]; then
        printf 'Группа администраторов [%s]: ' "$GROUP_ADMIN"
        read -r ANSWER || ANSWER=""
        if [ -n "$ANSWER" ]; then GROUP_ADMIN="$ANSWER"; fi
        printf 'Группа операторов [%s]: ' "$GROUP_OPERATOR"
        read -r ANSWER || ANSWER=""
        if [ -n "$ANSWER" ]; then GROUP_OPERATOR="$ANSWER"; fi
        printf 'Группа наблюдателей [%s]: ' "$GROUP_VIEWER"
        read -r ANSWER || ANSWER=""
        if [ -n "$ANSWER" ]; then GROUP_VIEWER="$ANSWER"; fi
        else
            say "Существующее role_mapping сохраняется; группы изменяются в YAML."
        fi

        if [ "$OIDC_MODE" = keycloak ] && [ "$OIDC_EXISTING" -eq 0 ] &&
                [ "$KEYCLOAK_APP_ADMIN_USER_EXPLICIT" -eq 0 ]; then
            say ""
            say "Эта запись входит в oVirt Backup через realm $KEYCLOAK_REALM."
            say "Она отличается от администратора консоли Keycloak."
            printf 'Первый администратор приложения [%s; none — не создавать]: ' "$KEYCLOAK_APP_ADMIN_USER"
            read -r ANSWER || ANSWER=""
            if [ -n "$ANSWER" ]; then KEYCLOAK_APP_ADMIN_USER="$ANSWER"; fi
        fi

        if [ -z "$OIDC_ALLOW_LOCAL_LOGIN" ]; then
            say ""
            say "Локальный local-admin обходит Keycloak, включая его MFA и блокировки."
            say "Оставлять его следует только как документированный аварийный доступ."
            printf 'Разрешить локальный вход по паролю? [y/N]: '
            read -r ANSWER || ANSWER=""
            case "$ANSWER" in
                y|Y|yes|YES|д|Д|да|ДА) OIDC_ALLOW_LOCAL_LOGIN=true ;;
                *) OIDC_ALLOW_LOCAL_LOGIN=false ;;
            esac
        fi
    fi

    [ -n "$OIDC_ALLOW_LOCAL_LOGIN" ] || OIDC_ALLOW_LOCAL_LOGIN=false
    case "$OIDC_ALLOW_LOCAL_LOGIN" in
        enabled|true) OIDC_ALLOW_LOCAL_LOGIN=true ;;
        disabled|false) OIDC_ALLOW_LOCAL_LOGIN=false ;;
        *) die "--local-login принимает enabled или disabled" ;;
    esac
    if [ "$OIDC_MODE" = keycloak ]; then
        case "$KEYCLOAK_APP_ADMIN_USER" in
            none) ;;
            ""|*[!A-Za-z0-9._@-]*)
                die "--keycloak-app-admin-user: допустимы A-Z, a-z, 0-9, точка, _, @ и -; либо none"
                ;;
        esac
    fi
    # В unattended-режиме интерактивная ветка выше не выполняется.
    prepare_keycloak_ad
}

load_systemd_oidc() {
    LSO_ENV="$PREFIX/config/jhvirt.env"
    LSO_ENABLED="$(env_file_value "$LSO_ENV" JHV_AUTH_OIDC_ENABLED)"
    if [ -z "$LSO_ENABLED" ] && [ -n "$OIDC_CONFIG_SOURCE" ]; then
        LSO_ENABLED="$(setup_tool config-get auth oidc enabled < "$OIDC_CONFIG_SOURCE")"
    fi
    [ "$LSO_ENABLED" = true ] || return 0
    [ -z "$OIDC_MODE" ] || [ "$OIDC_MODE" = external ] || return 0
    OIDC_MODE=external; OIDC_EXISTING=1
    for LSO_KEY in issuer client_id backchannel_url allow_local_login client_secret_file; do
        LSO_ENV_KEY="JHV_AUTH_OIDC_$(printf '%s' "$LSO_KEY" | tr '[:lower:]' '[:upper:]')"
        LSO_VALUE="$(env_file_value "$LSO_ENV" "$LSO_ENV_KEY")"
        if [ -z "$LSO_VALUE" ] && [ -n "$OIDC_CONFIG_SOURCE" ]; then
            LSO_VALUE="$(setup_tool config-get auth oidc "$LSO_KEY" < "$OIDC_CONFIG_SOURCE")"
        fi
        case "$LSO_KEY" in
            issuer) [ -n "$OIDC_ISSUER" ] || OIDC_ISSUER="$LSO_VALUE" ;;
            client_id) [ -n "$OIDC_CLIENT_ID" ] || OIDC_CLIENT_ID="$LSO_VALUE" ;;
            backchannel_url) [ -n "$OIDC_BACKCHANNEL_URL" ] || OIDC_BACKCHANNEL_URL="$LSO_VALUE" ;;
            allow_local_login) [ -n "$OIDC_ALLOW_LOCAL_LOGIN" ] || OIDC_ALLOW_LOCAL_LOGIN="$LSO_VALUE" ;;
            client_secret_file) [ -n "$OIDC_CLIENT_SECRET_FILE" ] || OIDC_CLIENT_SECRET_FILE="$LSO_VALUE" ;;
        esac
    done
    if [ -z "$OIDC_CLIENT_SECRET_FILE" ]; then
        OIDC_CLIENT_SECRET="$(env_file_value "$LSO_ENV" JHV_AUTH_OIDC_CLIENT_SECRET)"
        if [ -z "$OIDC_CLIENT_SECRET" ] && [ -n "$OIDC_CONFIG_SOURCE" ]; then
            OIDC_CLIENT_SECRET="$(setup_tool config-get auth oidc client_secret < "$OIDC_CONFIG_SOURCE")"
        fi
    fi
}

write_systemd_oidc() {
    [ "$OIDC_MODE" != none ] || { set_env JHV_AUTH_OIDC_ENABLED false "$ENV_FILE"; return 0; }
    write_oidc_config "$PREFIX/config/$CONFIG_NAME"
    # Existing inline secrets are kept until explicitly migrated; do not create
    # two simultaneous secret sources in a manually maintained configuration.
    if [ "$OIDC_EXISTING" -eq 0 ] || [ -n "$OIDC_CLIENT_SECRET_FILE" ]; then
        WSO_SECRET="$PREFIX/config/oidc-client.secret"
        WSO_TMP="$(mktemp "$WSO_SECRET.XXXXXX")"
        printf '%s' "$OIDC_CLIENT_SECRET" > "$WSO_TMP"
        install -o "$USER_NAME" -g "$USER_NAME" -m 0600 "$WSO_TMP" "$WSO_SECRET"
        rm -f "$WSO_TMP"
        set_env JHV_AUTH_OIDC_CLIENT_SECRET_FILE "$WSO_SECRET" "$ENV_FILE"
        set_env JHV_AUTH_OIDC_CLIENT_SECRET "" "$ENV_FILE"
    fi
    set_env JHV_AUTH_OIDC_ENABLED true "$ENV_FILE"
    set_env JHV_AUTH_OIDC_ISSUER "$OIDC_ISSUER" "$ENV_FILE"
    set_env JHV_AUTH_OIDC_CLIENT_ID "$OIDC_CLIENT_ID" "$ENV_FILE"
    set_env JHV_AUTH_OIDC_BACKCHANNEL_URL "$OIDC_BACKCHANNEL_URL" "$ENV_FILE"
    set_env JHV_AUTH_OIDC_REDIRECT_URL "$URL/api/v1/auth/oidc/callback" "$ENV_FILE"
    set_env JHV_AUTH_OIDC_POST_LOGOUT_REDIRECT_URL "$URL/login" "$ENV_FILE"
    set_env JHV_AUTH_OIDC_ALLOW_LOCAL_LOGIN "$OIDC_ALLOW_LOCAL_LOGIN" "$ENV_FILE"
    chown "$USER_NAME:$USER_NAME" "$OIDC_CONFIG"
    chmod 640 "$OIDC_CONFIG"
}

# Соответствие групп ролям — словарь, а viper словари из переменных окружения
# не собирает. Поэтому оно пишется в файл настроек, который compose монтирует
# в контейнер.
#
# Файл делается из штатного образца заменой единственной строки role_mapping,
# а не дописыванием секции в конец: второй ключ auth: в том же документе — это
# дубликат, и разбор YAML откажет целиком.
prepare_setup_tool() {
    [ -z "$SETUP_BINARY" ] || return 0
    if [ "$BUNDLE" -eq 1 ]; then
        SETUP_BINARY="$HERE/bin/$SERVER_BINARY"
        return 0
    fi
    SETUP_TMP="$(mktemp -d "${TMPDIR:-/tmp}/jhvirt-setup.XXXXXX")"
    chmod 700 "$SETUP_TMP"
    trap migration_cleanup EXIT INT TERM HUP
    SETUP_BINARY="$SETUP_TMP/setup"
    if have go; then
        (cd "$HERE/.." && go build -mod=readonly -o "$SETUP_BINARY" ./cmd/jhvirt-setup) || die "сборка setup helper не удалась"
    else
        docker run --rm -v "$HERE/..:/src:ro" -v "$SETUP_TMP:/out" -w /src \
            docker.io/library/golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc \
            go build -mod=readonly -o /out/setup ./cmd/jhvirt-setup || die "сборка setup helper не удалась"
    fi
}

setup_tool() {
    "$SETUP_BINARY" -setup "$@"
}

select_oidc_config() {
    prepare_setup_tool
    if [ "$MODE" = systemd ]; then
        [ ! -f "$PREFIX/config/$CONFIG_NAME" ] || OIDC_CONFIG_SOURCE="$PREFIX/config/$CONFIG_NAME"
    elif [ -f "$WORK/.env" ]; then
        SOC_PATH="$(env_file_value "$WORK/.env" JHV_CONFIG_FILE)"
        if [ -n "$SOC_PATH" ]; then
            OIDC_CONFIG_SOURCE="$(docker_host_path "$WORK" "$SOC_PATH")"
            [ -f "$OIDC_CONFIG_SOURCE" ] || die "сохранённая конфигурация не найдена: $OIDC_CONFIG_SOURCE"
        fi
    fi
    [ -n "$OIDC_CONFIG_SOURCE" ] || return 0
    SOC_GROUPS="$(setup_tool config-groups < "$OIDC_CONFIG_SOURCE")" || die "не удалось прочитать role_mapping"
    SOC_ADMIN="$(printf '%s' "$SOC_GROUPS" | setup_tool json-get admin)"
    SOC_OPERATOR="$(printf '%s' "$SOC_GROUPS" | setup_tool json-get operator)"
    SOC_VIEWER="$(printf '%s' "$SOC_GROUPS" | setup_tool json-get viewer)"
    if [ "$SOC_GROUPS" != '{}' ]; then
        OIDC_GROUPS_EXISTING=1
        [ -z "$SOC_ADMIN" ] || GROUP_ADMIN="$SOC_ADMIN"
        [ -z "$SOC_OPERATOR" ] || GROUP_OPERATOR="$SOC_OPERATOR"
        [ -z "$SOC_VIEWER" ] || GROUP_VIEWER="$SOC_VIEWER"
    fi
}

write_oidc_config() {
    OIDC_CONFIG="$1"
    [ -z "$OIDC_CONFIG_SOURCE" ] || OIDC_CONFIG="$OIDC_CONFIG_SOURCE"
    OIDC_SAMPLE="$COMPOSE_DIR/../config/$CONFIG_NAME"
    [ -f "$OIDC_SAMPLE" ] || OIDC_SAMPLE="$HERE/config/$CONFIG_NAME"
    [ ! -f "$OIDC_CONFIG" ] || OIDC_SAMPLE="$OIDC_CONFIG"
    [ -f "$OIDC_SAMPLE" ] || die "не найдена конфигурация $CONFIG_NAME"
    OIDC_CONFIG_TMP="$(mktemp "$OIDC_CONFIG.tmp.XXXXXX")"
    chmod 600 "$OIDC_CONFIG_TMP"
    if ! setup_tool config-init-roles "$GROUP_ADMIN" "$GROUP_OPERATOR" "$GROUP_VIEWER" \
            < "$OIDC_SAMPLE" > "$OIDC_CONFIG_TMP"; then
        rm -f "$OIDC_CONFIG_TMP"
        die "конфигурация не изменена: проверьте YAML и role_mapping"
    fi
    if [ -f "$OIDC_CONFIG" ] && cmp -s "$OIDC_CONFIG" "$OIDC_CONFIG_TMP"; then
        rm -f "$OIDC_CONFIG_TMP"
        return 0
    fi
    if [ -f "$OIDC_CONFIG" ]; then
        OIDC_CONFIG_BACKUP="$(mktemp "$OIDC_CONFIG.before-auth.XXXXXX")"
        cat "$OIDC_CONFIG" > "$OIDC_CONFIG_BACKUP"
        chmod 600 "$OIDC_CONFIG_BACKUP"
        chmod --reference="$OIDC_CONFIG" "$OIDC_CONFIG_TMP"
        chown --reference="$OIDC_CONFIG" "$OIDC_CONFIG_TMP"
        say "    прежняя конфигурация: $OIDC_CONFIG_BACKUP"
    else
        chmod 644 "$OIDC_CONFIG_TMP"
    fi
    mv -f "$OIDC_CONFIG_TMP" "$OIDC_CONFIG"
}

# Настройки внешнего входа в .env. Секрет клиента живёт только здесь: файл
# конфигурации лежит в репозитории установки, и секретам там не место.
write_oidc_env() {
    OIDC_ENV_FILE="$1"
    [ "$OIDC_MODE" = none ] && return 0

    # Уже выданные секреты переиспользуются, а не выпускаются заново.
    #
    # Keycloak заводит учётную запись администратора один раз — при первом
    # старте с пустой базой, — и клиента с секретом тоже один раз. Свежий
    # пароль в .env их не меняет: повторный запуск установщика упирался в
    # «Keycloak не принял пароль администратора», а если бы прошёл, то развёл
    # бы секрет клиента в .env и в самом Keycloak, и вход перестал бы работать
    # молча.
    OIDC_PREV="$(docker_volume_value "$(docker_metrics_volume)" oidc-client.secret)"
    [ -n "$OIDC_PREV" ] || OIDC_PREV="$(env_file_value "$OIDC_ENV_FILE" JHV_OIDC_CLIENT_SECRET)"
    if [ -n "$OIDC_PREV" ]; then OIDC_CLIENT_SECRET="$OIDC_PREV"; fi
    OIDC_PREV="$(env_file_value "$OIDC_ENV_FILE" KEYCLOAK_ADMIN_PASSWORD)"
    if [ -n "$OIDC_PREV" ]; then
        KEYCLOAK_ADMIN_PASSWORD="$OIDC_PREV"
        KEYCLOAK_RECOVERY_PASSWORD="$OIDC_PREV"
        KEYCLOAK_ACTIVE_ADMIN_PASSWORD="$OIDC_PREV"
    fi

    write_oidc_config "$WORK/$CONFIG_NAME"

    set_plain_env JHV_CONFIG_FILE "$OIDC_CONFIG" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_ENABLED true "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_ISSUER "$OIDC_ISSUER" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_BACKCHANNEL_URL "$OIDC_BACKCHANNEL_URL" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_CLIENT_ID "$OIDC_CLIENT_ID" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_CLIENT_SECRET "" "$OIDC_ENV_FILE"
    if [ -n "$OIDC_CLIENT_SECRET" ]; then
        set_plain_env JHV_OIDC_CLIENT_SECRET_FILE /app/data/oidc-client.secret "$OIDC_ENV_FILE"
    fi
    set_plain_env JHV_OIDC_REDIRECT_URL "$URL/api/v1/auth/oidc/callback" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_POST_LOGOUT_URL "$URL/login" "$OIDC_ENV_FILE"
    set_plain_env JHV_OIDC_ALLOW_LOCAL_LOGIN "$OIDC_ALLOW_LOCAL_LOGIN" "$OIDC_ENV_FILE"

    if [ "$OIDC_MODE" = keycloak ]; then
        set_plain_env COMPOSE_PROFILES keycloak "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_PORT "$KEYCLOAK_PORT" "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_BIND_ADDRESS "$KEYCLOAK_BIND_ADDRESS" "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_CONTAINER_PORT "$KEYCLOAK_CONTAINER_PORT" "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_DIRECT_TLS "$KEYCLOAK_DIRECT_TLS" "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_DB keycloak "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_DB_USER "$KEYCLOAK_DB_USER" "$OIDC_ENV_FILE"
        set_plain_env JHV_KEYCLOAK_URL "$KEYCLOAK_URL" "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_ADMIN_USER "$KEYCLOAK_ADMIN_USER" "$OIDC_ENV_FILE"
        set_plain_env JHV_KEYCLOAK_APP_ADMIN_USER "$KEYCLOAK_APP_ADMIN_USER" "$OIDC_ENV_FILE"
        set_plain_env KEYCLOAK_ADMIN_PASSWORD "" "$OIDC_ENV_FILE"
        set_plain_env JHV_OIDC_BUTTON_LABEL "Войти через Keycloak" "$OIDC_ENV_FILE"
    fi
}

# --- Keycloak ---------------------------------------------------------------

keycloak_token() {
    KC_TOKEN_USER="${KEYCLOAK_ACTIVE_ADMIN_USER:-$KEYCLOAK_ADMIN_USER}"
    printf 'grant_type=password&client_id=admin-cli&username=%s&password=%s' \
        "$KC_TOKEN_USER" "${KEYCLOAK_ACTIVE_ADMIN_PASSWORD:-$KEYCLOAK_ADMIN_PASSWORD}" |
    curl -sS -k -m 20 --fail \
        -H 'Content-Type: application/x-www-form-urlencoded' --data-binary @- \
        "$KEYCLOAK_API_URL/realms/master/protocol/openid-connect/token" 2>/dev/null |
        sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'
}

keycloak_post() {
    printf '%s' "$2" |
    curl -sS -k -m 30 -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $KC_TOKEN" -H 'Content-Type: application/json' \
        -X POST --data-binary @- "$KEYCLOAK_API_URL/admin/realms$1" 2>/dev/null
}

keycloak_put() {
    printf '%s' "$2" |
    curl -sS -k -m 30 -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $KC_TOKEN" -H 'Content-Type: application/json' \
        -X PUT --data-binary @- "$KEYCLOAK_API_URL/admin/realms$1" 2>/dev/null
}

# Все значения LDAP приходят от оператора. JSON собирается без jq, потому что
# минимальная production-система не обязана его иметь; кавычки и обратные слэши
# при этом всё равно должны быть экранированы.
json_quote() {
    JQ_VALUE="$(printf '%s' "$1" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g')"
    printf '"%s"' "$JQ_VALUE"
}

keycloak_component_id() {
    KCI_NAME="$1"; KCI_PARENT="$2"; KCI_TYPE="$3"
    KCI_JSON="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" --get \
        --data-urlencode "name=$KCI_NAME" \
        --data-urlencode "parent=$KCI_PARENT" \
        --data-urlencode "type=$KCI_TYPE" \
        "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/components" 2>/dev/null)" || return 1
    KCI_IDS="$(printf '%s' "$KCI_JSON" | sed 's/},{/}\
{/g' | sed -n \
        's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
    KCI_COUNT="$(printf '%s\n' "$KCI_IDS" | awk 'NF { n++ } END { print n + 0 }')"
    if [ "$KCI_COUNT" -gt 1 ]; then
        printf 'ошибка: в realm %s найдено несколько компонентов %s с именем %s: %s\n' \
            "$KEYCLOAK_REALM" "$KCI_TYPE" "$KCI_NAME" \
            "$(printf '%s' "$KCI_IDS" | tr '\n' ' ')" >&2
        printf 'Отключите дубликат в Admin Console после проверки привязанных пользователей; установщик не выберет его случайно.\n' >&2
        return 1
    fi
    printf '%s' "$KCI_IDS"
}

keycloak_ldap_provider_inventory() {
    KLPI_PARENT="$1"
    KLPI_JSON="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" --get \
        --data-urlencode "parent=$KLPI_PARENT" \
        --data-urlencode 'type=org.keycloak.storage.UserStorageProvider' \
        "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/components" 2>/dev/null)" || return 1
    printf '%s' "$KLPI_JSON" | tr '\n' ' ' | sed 's/},{/}\
{/g' |
        while IFS= read -r KLPI_ITEM; do
            printf '%s' "$KLPI_ITEM" |
                grep -Eq '"providerId"[[:space:]]*:[[:space:]]*"ldap"' || continue
            KLPI_ID="$(printf '%s' "$KLPI_ITEM" | sed -n \
                's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
            KLPI_NAME="$(printf '%s' "$KLPI_ITEM" | sed -n \
                's/.*"name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
            [ -n "$KLPI_ID" ] || continue
            printf '%s=%s\n' "${KLPI_NAME:-без-имени}" "$KLPI_ID"
        done
}

keycloak_sync_users() {
    KSU_PROVIDER_ID="$1"
    curl -sS -k -m 300 --fail \
        -H "Authorization: Bearer $KC_TOKEN" -X POST \
        "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/user-storage/$KSU_PROVIDER_ID/sync?action=triggerFullSync" \
        2>/dev/null
}

keycloak_sync_mapper() {
    KSM_PROVIDER_ID="$1"; KSM_MAPPER_ID="$2"
    curl -sS -k -m 300 --fail \
        -H "Authorization: Bearer $KC_TOKEN" -X POST \
        "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/user-storage/$KSM_PROVIDER_ID/mappers/$KSM_MAPPER_ID/sync?direction=fedToKeycloak" \
        2>/dev/null
}

# Первый запуск Keycloak с пустой базой — это миграция схемы, и три минуты там
# не редкость.
keycloak_wait() {
    KC_TRY=0
    while [ "$KC_TRY" -lt 90 ]; do
        # -k нужен, когда --keycloak-url указывает на прокси с
        # самоподписанным сертификатом: без него ожидание не кончится ничем.
        if curl -sS -k --fail -m 5 -o /dev/null "$KEYCLOAK_API_URL/realms/master" 2>/dev/null; then
            return 0
        fi
        KC_TRY=$((KC_TRY+1))
        sleep 2
    done
    return 1
}

# keycloak_realm_exists — публичная проверка, настроен ли realm.
#
# Не требует ни токена, ни прав: адрес /realms/<имя> отвечает 200 всякому, у
# кого realm существует.
keycloak_realm_exists() {
    KC_REALM_URL="$KEYCLOAK_API_URL/realms/$KEYCLOAK_REALM"
    KC_REALM_CODE="$(curl -sS -k -m 5 -o /dev/null -w '%{http_code}' "$KC_REALM_URL" 2>/dev/null)"
    [ "$KC_REALM_CODE" = 200 ]
}

# Обновление прежней установки не может полагаться на bootstrap-пароль:
# Keycloak применяет его только к пустой БД. Если постоянный администратор уже
# существует, создаём штатной offline-командой одноразового администратора,
# используем его для настройки и обязательно удаляем через Admin API.
keycloak_start_recovery_admin() {
    KC_RECOVERY_ORIGINAL_USER="$KEYCLOAK_ACTIVE_ADMIN_USER"
    KC_RECOVERY_ADMIN_USER="kc-install-recovery-$(gen_secret 6)"
    [ -n "$KC_RECOVERY_ADMIN_USER" ] || return 1

    say "    прежний bootstrap-пароль недоступен; создаётся временный администратор"
    # shellcheck disable=SC2086
    (cd "$WORK" && $RUN stop keycloak) >/dev/null 2>&1 || return 1
    # Значение пароля передаётся через окружение процесса, а не аргументом:
    # аргументы доступны другим пользователям хоста через список процессов.
    # shellcheck disable=SC2086
    if ! (cd "$WORK" && KC_RECOVERY_PASSWORD="$KEYCLOAK_RECOVERY_PASSWORD" \
            $RUN run --rm --no-deps -e KC_RECOVERY_PASSWORD keycloak \
            --config-file=/opt/keycloak/data/ovirt-backup/keycloak.conf \
            bootstrap-admin service --optimized --client-id "$KC_RECOVERY_ADMIN_USER" \
            --client-secret:env=KC_RECOVERY_PASSWORD --no-prompt) >/dev/null; then
        # shellcheck disable=SC2086
        (cd "$WORK" && $RUN up -d keycloak) >/dev/null 2>&1 || true
        return 1
    fi
    KC_RECOVERY_ACTIVE=1
    KEYCLOAK_ACTIVE_ADMIN_USER="$KC_RECOVERY_ADMIN_USER"
    KEYCLOAK_ACTIVE_ADMIN_PASSWORD="$KEYCLOAK_RECOVERY_PASSWORD"
    # В существующей БД bootstrap-поле не используется штатным запуском. После
    # offline-команды пароль больше не должен оставаться в volume.
    clear_keycloak_bootstrap_secret

    # shellcheck disable=SC2086
    (cd "$WORK" && $RUN up -d keycloak) >/dev/null 2>&1 || return 1
    keycloak_wait || return 1
    KC_TOKEN="$(printf 'grant_type=client_credentials&client_id=%s&client_secret=%s' \
        "$KC_RECOVERY_ADMIN_USER" "$KEYCLOAK_RECOVERY_PASSWORD" | \
        curl -sS -k --fail -m 30 -H 'Content-Type: application/x-www-form-urlencoded' --data-binary @- \
        "$KEYCLOAK_API_URL/realms/master/protocol/openid-connect/token" | setup_tool json-get access_token)"
    [ -n "$KC_TOKEN" ]
}

keycloak_remove_recovery_admin() {
    [ "${KC_RECOVERY_ACTIVE:-0}" -eq 1 ] || return 0
    KC_RECOVERY_USERS="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" \
        --get --data-urlencode "clientId=$KC_RECOVERY_ADMIN_USER" \
        "$KEYCLOAK_API_URL/admin/realms/master/clients" 2>/dev/null)" || return 1
    KC_RECOVERY_USER_ID="$(printf '%s' "$KC_RECOVERY_USERS" | setup_tool json-find clientId "$KC_RECOVERY_ADMIN_USER" | setup_tool json-get id)"
    [ -n "$KC_RECOVERY_USER_ID" ] || return 1
    KC_RECOVERY_DELETE_CODE="$(curl -sS -k -m 30 -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $KC_TOKEN" -X DELETE \
        "$KEYCLOAK_API_URL/admin/realms/master/clients/$KC_RECOVERY_USER_ID" 2>/dev/null)"
    [ "$KC_RECOVERY_DELETE_CODE" = 204 ] || return 1

    KC_RECOVERY_ACTIVE=0
    KEYCLOAK_ACTIVE_ADMIN_USER="$KC_RECOVERY_ORIGINAL_USER"
    KEYCLOAK_ACTIVE_ADMIN_PASSWORD=""
    KEYCLOAK_ADMIN_PASSWORD=""
    KEYCLOAK_RECOVERY_PASSWORD=""
    say "    временный администратор Keycloak удалён"
}

# Убирает recovery-записи, которые могла оставить аварийно прерванная прежняя
# установка. Активную запись текущего запуска удаляет обычный cleanup после
# настройки realm, чтобы токен оставался действительным до конца операции.
keycloak_remove_stale_recovery_admins() {
    KC_STALE_CLIENTS="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" --get \
        --data-urlencode 'clientId=kc-install-recovery-' \
        --data-urlencode 'search=true' --data-urlencode 'max=1000' \
        "$KEYCLOAK_API_URL/admin/realms/master/clients" 2>/dev/null)" || return 1

    printf '%s' "$KC_STALE_CLIENTS" | setup_tool json-prefix clientId kc-install-recovery- |
        while IFS=' ' read -r KC_STALE_ID KC_STALE_NAME; do
            [ "$KC_STALE_NAME" = "${KC_RECOVERY_ADMIN_USER:-}" ] && continue
            [ -n "$KC_STALE_ID" ] || return 1
            KC_STALE_CODE="$(curl -sS -k -m 30 -o /dev/null -w '%{http_code}' \
                -H "Authorization: Bearer $KC_TOKEN" -X DELETE \
                "$KEYCLOAK_API_URL/admin/realms/master/clients/$KC_STALE_ID" 2>/dev/null)"
            [ "$KC_STALE_CODE" = 204 ] || return 1
            say "    удалён оставшийся временный service account $KC_STALE_NAME"
        done
}

# Keycloak bootstrap-admin is deliberately temporary. A fresh installation
# replaces it with an ordinary master-realm administrator before the password
# is removed from keycloak.conf.
keycloak_create_permanent_admin() {
    [ -n "$KEYCLOAK_BOOTSTRAP_USER" ] || return 0
    [ "$KEYCLOAK_BOOTSTRAP_USER" != "$KEYCLOAK_ADMIN_USER" ] || return 0

    KC_TOKEN="$(keycloak_token)"
    [ -n "$KC_TOKEN" ] || return 1

    KC_ADMIN_BODY="{\"username\":\"$KEYCLOAK_ADMIN_USER\",\"enabled\":true,\"email\":\"kc-admin@ovirt-backup.local\",\"emailVerified\":true,\"firstName\":\"Keycloak\",\"lastName\":\"Administrator\"}"
    KC_CODE="$(keycloak_post "/master/users" "$KC_ADMIN_BODY")"
    [ "$KC_CODE" = 201 ] || return 1

    KC_ADMIN_USERS="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" --get \
        --data-urlencode "username=$KEYCLOAK_ADMIN_USER" --data-urlencode 'exact=true' \
        "$KEYCLOAK_API_URL/admin/realms/master/users" 2>/dev/null)" || return 1
    KC_PERMANENT_ADMIN_ID="$(printf '%s' "$KC_ADMIN_USERS" | sed -n \
        's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
    [ -n "$KC_PERMANENT_ADMIN_ID" ] || return 1

    KC_CODE="$(printf '%s' \
        "{\"type\":\"password\",\"value\":\"$KEYCLOAK_ADMIN_PASSWORD\",\"temporary\":false}" |
        curl -sS -k -m 30 -o /dev/null -w '%{http_code}' \
            -H "Authorization: Bearer $KC_TOKEN" -H 'Content-Type: application/json' \
            -X PUT --data-binary @- \
            "$KEYCLOAK_API_URL/admin/realms/master/users/$KC_PERMANENT_ADMIN_ID/reset-password" 2>/dev/null)"
    [ "$KC_CODE" = 204 ] || return 1

    KC_ADMIN_ROLE="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" \
        "$KEYCLOAK_API_URL/admin/realms/master/roles/admin" 2>/dev/null)" || return 1
    [ -n "$KC_ADMIN_ROLE" ] || return 1
    KC_CODE="$(curl -sS -k -m 30 -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $KC_TOKEN" -H 'Content-Type: application/json' \
        -X POST -d "[$KC_ADMIN_ROLE]" \
        "$KEYCLOAK_API_URL/admin/realms/master/users/$KC_PERMANENT_ADMIN_ID/role-mappings/realm" 2>/dev/null)"
    [ "$KC_CODE" = 204 ] || return 1

    KEYCLOAK_ACTIVE_ADMIN_USER="$KEYCLOAK_ADMIN_USER"
    KC_PERMANENT_TOKEN="$(keycloak_token)"
    [ -n "$KC_PERMANENT_TOKEN" ] || return 1
    KC_CODE="$(curl -sS -k -m 30 -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $KC_PERMANENT_TOKEN" \
        "$KEYCLOAK_API_URL/admin/realms/master/users" 2>/dev/null)"
    [ "$KC_CODE" = 200 ] || return 1

    KC_BOOTSTRAP_USERS="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_PERMANENT_TOKEN" --get \
        --data-urlencode "username=$KEYCLOAK_BOOTSTRAP_USER" --data-urlencode 'exact=true' \
        "$KEYCLOAK_API_URL/admin/realms/master/users" 2>/dev/null)" || return 1
    KC_BOOTSTRAP_ID="$(printf '%s' "$KC_BOOTSTRAP_USERS" | sed -n \
        's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
    [ -n "$KC_BOOTSTRAP_ID" ] || return 1
    KC_CODE="$(curl -sS -k -m 30 -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $KC_PERMANENT_TOKEN" -X DELETE \
        "$KEYCLOAK_API_URL/admin/realms/master/users/$KC_BOOTSTRAP_ID" 2>/dev/null)"
    [ "$KC_CODE" = 204 ] || return 1

    KC_TOKEN="$KC_PERMANENT_TOKEN"
    KEYCLOAK_BOOTSTRAP_USER=""
    KEYCLOAK_ACTIVE_ADMIN_USER="$KEYCLOAK_ADMIN_USER"
    KEYCLOAK_ACTIVE_ADMIN_PASSWORD="$KEYCLOAK_ADMIN_PASSWORD"
    say "    постоянный администратор Keycloak создан, bootstrap-запись удалена"
}

# Создаёт первую учётную запись именно в прикладном realm. Администратор
# master realm не виден форме входа jhvirt, поэтому без отдельного пользователя
# формально исправный OIDC после чистой установки не даёт войти в приложение.
# На обновлении существующая запись и её пароль не меняются.
keycloak_create_app_admin() {
    KEYCLOAK_APP_ADMIN_CREATED=0
    if [ "$KEYCLOAK_APP_ADMIN_USER" = none ]; then
        KEYCLOAK_APP_ADMIN_PASSWORD=""
        say "    первый пользователь realm $KEYCLOAK_REALM не создаётся"
        return 0
    fi

    KC_APP_USERS="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" --get \
        --data-urlencode "username=$KEYCLOAK_APP_ADMIN_USER" --data-urlencode 'exact=true' \
        "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/users" 2>/dev/null)" || return 1
    KC_APP_USER_ID="$(printf '%s' "$KC_APP_USERS" | sed -n \
        's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
    if [ -n "$KC_APP_USER_ID" ]; then
        KEYCLOAK_APP_ADMIN_PASSWORD=""
        say "    пользователь $KEYCLOAK_APP_ADMIN_USER в realm $KEYCLOAK_REALM сохранён; пароль не менялся"
        return 0
    fi

    KC_APP_USER_BODY="{\"username\":\"$KEYCLOAK_APP_ADMIN_USER\",\"enabled\":true,\"email\":\"initial-admin@ovirt-backup.local\",\"emailVerified\":true,\"firstName\":\"Backup\",\"lastName\":\"Administrator\"}"
    KC_CODE="$(keycloak_post "/$KEYCLOAK_REALM/users" "$KC_APP_USER_BODY")"
    [ "$KC_CODE" = 201 ] || return 1

    KC_APP_USERS="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" --get \
        --data-urlencode "username=$KEYCLOAK_APP_ADMIN_USER" --data-urlencode 'exact=true' \
        "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/users" 2>/dev/null)" || return 1
    KC_APP_USER_ID="$(printf '%s' "$KC_APP_USERS" | sed -n \
        's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
    [ -n "$KC_APP_USER_ID" ] || return 1

    KC_CODE="$(printf '%s' \
        "{\"type\":\"password\",\"value\":\"$KEYCLOAK_APP_ADMIN_PASSWORD\",\"temporary\":false}" |
        curl -sS -k -m 30 -o /dev/null -w '%{http_code}' \
            -H "Authorization: Bearer $KC_TOKEN" -H 'Content-Type: application/json' \
            -X PUT --data-binary @- \
            "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/users/$KC_APP_USER_ID/reset-password" 2>/dev/null)"
    [ "$KC_CODE" = 204 ] || return 1

    KC_APP_GROUPS="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" --get \
        --data-urlencode "search=$GROUP_ADMIN" --data-urlencode 'exact=true' \
        "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/groups" 2>/dev/null)" || return 1
    KC_APP_GROUP_ID="$(printf '%s' "$KC_APP_GROUPS" | sed -n \
        's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
    [ -n "$KC_APP_GROUP_ID" ] || return 1
    KC_CODE="$(curl -sS -k -m 30 -o /dev/null -w '%{http_code}' \
        -H "Authorization: Bearer $KC_TOKEN" -X PUT \
        "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/users/$KC_APP_USER_ID/groups/$KC_APP_GROUP_ID" 2>/dev/null)"
    [ "$KC_CODE" = 204 ] || return 1

    KC_APP_MEMBERSHIP="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" \
        "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/users/$KC_APP_USER_ID/groups" 2>/dev/null)" || return 1
    printf '%s' "$KC_APP_MEMBERSHIP" | tr -d '[:space:]' |
        grep -Fq "\"id\":\"$KC_APP_GROUP_ID\"" || return 1

    KEYCLOAK_APP_ADMIN_CREATED=1
    say "    пользователь $KEYCLOAK_APP_ADMIN_USER создан в realm $KEYCLOAK_REALM и включён в $GROUP_ADMIN"
}

keycloak_bootstrap_die() {
    KBD_MESSAGE="$1"
    if [ "${KC_RECOVERY_ACTIVE:-0}" -eq 1 ] && ! keycloak_remove_recovery_admin; then
        die "$KBD_MESSAGE

Дополнительно не удалось удалить временного администратора
$KC_RECOVERY_ADMIN_USER. Немедленно удалите его в master realm Keycloak."
    fi
    die "$KBD_MESSAGE"
}

# Проверяет OIDC тем же путём, которым пойдёт браузер. Успешный /start обязан
# вернуть абсолютный адрес провайдера; перенаправление обратно на /login с
# oidc_error означает, что discovery из контейнера приложения не работает.
oidc_app_check() {
    OAC_URL="$READY_SCHEME://127.0.0.1:$PORT/api/v1/auth/oidc/start"
    OAC_HEADERS="$(curl -k -sS -m 35 -D - -o /dev/null "$OAC_URL" 2>/dev/null)" || return 1
    OAC_LOCATION="$(printf '%s\n' "$OAC_HEADERS" | tr -d '\r' | awk '
        tolower(substr($0, 1, 9)) == "location:" {
            sub(/^[^:]*:[[:space:]]*/, "")
            print
            exit
        }')"
    case "$OAC_LOCATION" in
        http://*|https://*)
            # Discovery alone is insufficient for the bundled Keycloak. An
            # unknown scope is rejected by the authorization endpoint, so the
            # first redirect looks healthy while the browser immediately
            # returns to /login?oidc_error=... . External providers may expose
            # their public issuer only to client DNS, therefore this extra
            # host-side probe is intentionally limited to the bundled mode.
            if [ "$OIDC_MODE" = keycloak ]; then
                # Публичный адрес обязан попасть в redirect браузеру, но сам
                # сервер может не ходить на собственный внешний IP из-за
                # hairpin NAT или firewall. Проверяем тот же путь и query через
                # loopback API, которым установщик уже дождался Keycloak.
                OAC_PROVIDER_AUTH_PATH="${OAC_LOCATION#*://}"
                OAC_PROVIDER_PATH="/${OAC_PROVIDER_AUTH_PATH#*/}"
                OAC_PROVIDER_CHECK_URL="${KEYCLOAK_API_URL%/}$OAC_PROVIDER_PATH"
                OAC_PROVIDER_HEADERS="$(curl -k -sS -m 35 -D - -o /dev/null \
                    "$OAC_PROVIDER_CHECK_URL" 2>/dev/null)" || return 1
                OAC_PROVIDER_LOCATION="$(printf '%s\n' "$OAC_PROVIDER_HEADERS" | tr -d '\r' | awk '
                    tolower(substr($0, 1, 9)) == "location:" {
                        sub(/^[^:]*:[[:space:]]*/, "")
                        print
                        exit
                    }')"
                case "$OAC_PROVIDER_LOCATION" in
                    *error=*)
                        say "    Keycloak отклонил OIDC-запрос: $OAC_PROVIDER_LOCATION"
                        return 1
                        ;;
                esac
            fi
            return 0
            ;;
    esac
    [ -z "$OAC_LOCATION" ] || say "    приложение вернуло: $OAC_LOCATION"
    return 1
}

keycloak_configure_ad() {
    [ "$KEYCLOAK_AD_REQUESTED" -eq 1 ] || return 0
    [ -r "$KEYCLOAK_AD_VAULT_TARGET" ] || {
        say "    bind-секрет не найден в защищённом Keycloak vault"
        return 1
    }
    KC_AD_TEST_PASSWORD="$(cat "$KEYCLOAK_AD_VAULT_TARGET")"
    for KC_AD_ACTION in testConnection testAuthentication; do
        KC_AD_TEST="{\"action\":\"$KC_AD_ACTION\",\"connectionUrl\":$(json_quote "$KEYCLOAK_AD_URL"),\"bindDn\":$(json_quote "$KEYCLOAK_AD_BIND_DN"),\"bindCredential\":$(json_quote "$KC_AD_TEST_PASSWORD"),\"useTruststoreSpi\":\"always\",\"connectionTimeout\":\"10000\",\"startTls\":\"false\"}"
        KC_AD_TEST_CODE="$(keycloak_post "/$KEYCLOAK_REALM/testLDAPConnection" "$KC_AD_TEST")"
        [ "$KC_AD_TEST_CODE" = 204 ] || {
            say "    AD: $KC_AD_ACTION не пройден из Keycloak (HTTP $KC_AD_TEST_CODE). Проверьте DNS контейнера, CA и bind-учётную запись."
            return 1
        }
    done
    KC_AD_TEST_PASSWORD=""; KC_AD_TEST=""

    KC_AD_REALM_JSON="$(curl -sS -k -m 30 --fail \
        -H "Authorization: Bearer $KC_TOKEN" \
        "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM" 2>/dev/null)" || return 1
    KC_AD_REALM_ID="$(printf '%s' "$KC_AD_REALM_JSON" | sed -n \
        's/^[[:space:]]*{"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
    [ -n "$KC_AD_REALM_ID" ] || return 1

    KC_AD_PROVIDER_ID="$(keycloak_component_id "$KEYCLOAK_AD_PROVIDER" "$KC_AD_REALM_ID" \
        'org.keycloak.storage.UserStorageProvider')" || return 1
    if [ -z "$KC_AD_PROVIDER_ID" ]; then
        KC_AD_EXISTING_PROVIDERS="$(keycloak_ldap_provider_inventory "$KC_AD_REALM_ID")" || return 1
        KC_AD_EXISTING_COUNT="$(printf '%s\n' "$KC_AD_EXISTING_PROVIDERS" |
            awk 'NF { n++ } END { print n + 0 }')"
        if [ "$KC_AD_EXISTING_COUNT" -eq 1 ] && [ "$KEYCLOAK_AD_PROVIDER_EXPLICIT" -eq 0 ]; then
            KEYCLOAK_AD_PROVIDER="${KC_AD_EXISTING_PROVIDERS%%=*}"
            KC_AD_PROVIDER_ID="${KC_AD_EXISTING_PROVIDERS#*=}"
            say "    выбран существующий LDAP provider $KEYCLOAK_AD_PROVIDER"
        elif [ "$KC_AD_EXISTING_COUNT" -gt 0 ]; then
            printf 'ошибка: в realm %s уже есть LDAP provider:\n%s\n' \
                "$KEYCLOAK_REALM" "$KC_AD_EXISTING_PROVIDERS" >&2
            printf 'Укажите его точное имя через --keycloak-ad-provider. Если providers несколько, сначала отключите и разберите дубликаты в Admin Console. Новый provider автоматически не создаётся.\n' >&2
            return 1
        fi
    fi

    KC_AD_Q_NAME="$(json_quote "$KEYCLOAK_AD_PROVIDER")"
    KC_AD_Q_URL="$(json_quote "$KEYCLOAK_AD_URL")"
    KC_AD_Q_USERS_DN="$(json_quote "$KEYCLOAK_AD_USERS_DN")"
    KC_AD_Q_BIND_DN="$(json_quote "$KEYCLOAK_AD_BIND_DN")"
    # Default file-vault resolver ищет <realm>_<key>. В БД хранится только эта
    # ссылка; фактический bind-пароль находится в read-only bind mount.
    KC_AD_Q_BIND_PASSWORD="$(json_quote '${vault.ad-bind}')"
    KC_AD_PROVIDER_BODY="{
        \"name\":$KC_AD_Q_NAME,
        \"providerId\":\"ldap\",
        \"providerType\":\"org.keycloak.storage.UserStorageProvider\",
        \"parentId\":\"$KC_AD_REALM_ID\",
        \"config\":{
            \"enabled\":[\"true\"],
            \"priority\":[\"0\"],
            \"vendor\":[\"ad\"],
            \"connectionUrl\":[$KC_AD_Q_URL],
            \"usersDn\":[$KC_AD_Q_USERS_DN],
            \"bindDn\":[$KC_AD_Q_BIND_DN],
            \"bindCredential\":[$KC_AD_Q_BIND_PASSWORD],
            \"authType\":[\"simple\"],
            \"editMode\":[\"READ_ONLY\"],
            \"importEnabled\":[\"true\"],
            \"syncRegistrations\":[\"false\"],
            \"usernameLDAPAttribute\":[\"sAMAccountName\"],
            \"rdnLDAPAttribute\":[\"cn\"],
            \"uuidLDAPAttribute\":[\"objectGUID\"],
            \"userObjectClasses\":[\"person, organizationalPerson, user\"],
            \"searchScope\":[\"2\"],
            \"useTruststoreSpi\":[\"always\"],
            \"startTls\":[\"false\"],
            \"connectionPooling\":[\"true\"],
            \"pagination\":[\"true\"],
            \"batchSizeForSync\":[\"1000\"],
            \"changedSyncPeriod\":[\"900\"],
            \"fullSyncPeriod\":[\"86400\"],
            \"allowKerberosAuthentication\":[\"false\"],
            \"useKerberosForPasswordAuthentication\":[\"false\"],
            \"trustEmail\":[\"false\"],
            \"validatePasswordPolicy\":[\"false\"],
            \"removeInvalidUsersEnabled\":[\"true\"]
        }
    }"

    if [ -n "$KC_AD_PROVIDER_ID" ]; then
        KC_AD_PROVIDER_BODY="$(printf '%s' "$KC_AD_PROVIDER_BODY" | sed \
            "1s/{/{\"id\":\"$KC_AD_PROVIDER_ID\",/")"
        KC_CODE="$(keycloak_put "/$KEYCLOAK_REALM/components/$KC_AD_PROVIDER_ID" "$KC_AD_PROVIDER_BODY")"
        [ "$KC_CODE" = 204 ] || return 1
        say "    LDAP provider $KEYCLOAK_AD_PROVIDER обновлён"
    else
        KC_CODE="$(keycloak_post "/$KEYCLOAK_REALM/components" "$KC_AD_PROVIDER_BODY")"
        [ "$KC_CODE" = 201 ] || return 1
        KC_AD_PROVIDER_ID="$(keycloak_component_id "$KEYCLOAK_AD_PROVIDER" "$KC_AD_REALM_ID" \
            'org.keycloak.storage.UserStorageProvider')" || return 1
        [ -n "$KC_AD_PROVIDER_ID" ] || return 1
        say "    LDAP provider $KEYCLOAK_AD_PROVIDER создан"
    fi

    # При обновлении старого provider автоматически созданный username mapper
    # мог остаться на cn, даже если основной параметр уже изменён.
    KC_AD_USERNAME_ID="$(keycloak_component_id username "$KC_AD_PROVIDER_ID" \
        'org.keycloak.storage.ldap.mappers.LDAPStorageMapper')" || return 1
    KC_AD_USERNAME_BODY="{
        \"name\":\"username\",
        \"providerId\":\"user-attribute-ldap-mapper\",
        \"providerType\":\"org.keycloak.storage.ldap.mappers.LDAPStorageMapper\",
        \"parentId\":\"$KC_AD_PROVIDER_ID\",
        \"config\":{
            \"ldap.attribute\":[\"sAMAccountName\"],
            \"user.model.attribute\":[\"username\"],
            \"read.only\":[\"true\"],
            \"always.read.value.from.ldap\":[\"false\"],
            \"is.mandatory.in.ldap\":[\"true\"]
        }
    }"
    if [ -n "$KC_AD_USERNAME_ID" ]; then
        KC_AD_USERNAME_BODY="$(printf '%s' "$KC_AD_USERNAME_BODY" | sed \
            "1s/{/{\"id\":\"$KC_AD_USERNAME_ID\",/")"
        KC_CODE="$(keycloak_put "/$KEYCLOAK_REALM/components/$KC_AD_USERNAME_ID" "$KC_AD_USERNAME_BODY")"
        [ "$KC_CODE" = 204 ] || return 1
    else
        KC_CODE="$(keycloak_post "/$KEYCLOAK_REALM/components" "$KC_AD_USERNAME_BODY")"
        [ "$KC_CODE" = 201 ] || return 1
    fi

    KC_AD_Q_GROUPS_DN="$(json_quote "$KEYCLOAK_AD_GROUPS_DN")"
    KC_AD_GROUP_FILTER="$(setup_tool ldap-filter "$GROUP_ADMIN" "$GROUP_OPERATOR" "$GROUP_VIEWER")" || return 1
    KC_AD_Q_GROUP_FILTER="$(json_quote "$KC_AD_GROUP_FILTER")"
    KC_AD_GROUP_MAPPER_NAME="ovirt-backup-groups"
    KC_AD_GROUP_MAPPER_ID="$(keycloak_component_id "$KC_AD_GROUP_MAPPER_NAME" \
        "$KC_AD_PROVIDER_ID" 'org.keycloak.storage.ldap.mappers.LDAPStorageMapper')" || return 1
    KC_AD_GROUP_BODY="{
        \"name\":\"$KC_AD_GROUP_MAPPER_NAME\",
        \"providerId\":\"group-ldap-mapper\",
        \"providerType\":\"org.keycloak.storage.ldap.mappers.LDAPStorageMapper\",
        \"parentId\":\"$KC_AD_PROVIDER_ID\",
        \"config\":{
            \"groups.dn\":[$KC_AD_Q_GROUPS_DN],
            \"group.name.ldap.attribute\":[\"cn\"],
            \"group.object.classes\":[\"group\"],
            \"preserve.group.inheritance\":[\"false\"],
            \"ignore.missing.groups\":[\"true\"],
            \"membership.ldap.attribute\":[\"member\"],
            \"membership.attribute.type\":[\"DN\"],
            \"membership.user.ldap.attribute\":[\"sAMAccountName\"],
            \"user.roles.retrieve.strategy\":[\"GET_GROUPS_FROM_USER_MEMBEROF_ATTRIBUTE\"],
            \"memberof.ldap.attribute\":[\"memberOf\"],
            \"groups.ldap.filter\":[$KC_AD_Q_GROUP_FILTER],
            \"groups.path\":[\"/\"],
            \"mode\":[\"$KEYCLOAK_AD_GROUP_MODE_API\"],
            \"mapped.group.attributes\":[\"\"],
            \"drop.non.existing.groups.during.sync\":[\"false\"]
        }
    }"
    if [ -n "$KC_AD_GROUP_MAPPER_ID" ]; then
        KC_AD_GROUP_BODY="$(printf '%s' "$KC_AD_GROUP_BODY" | sed \
            "1s/{/{\"id\":\"$KC_AD_GROUP_MAPPER_ID\",/")"
        KC_CODE="$(keycloak_put "/$KEYCLOAK_REALM/components/$KC_AD_GROUP_MAPPER_ID" "$KC_AD_GROUP_BODY")"
        [ "$KC_CODE" = 204 ] || return 1
    else
        KC_CODE="$(keycloak_post "/$KEYCLOAK_REALM/components" "$KC_AD_GROUP_BODY")"
        [ "$KC_CODE" = 201 ] || return 1
        KC_AD_GROUP_MAPPER_ID="$(keycloak_component_id "$KC_AD_GROUP_MAPPER_NAME" \
            "$KC_AD_PROVIDER_ID" 'org.keycloak.storage.ldap.mappers.LDAPStorageMapper')" || return 1
        [ -n "$KC_AD_GROUP_MAPPER_ID" ] || return 1
    fi

    KC_AD_USER_SYNC="$(keycloak_sync_users "$KC_AD_PROVIDER_ID")" || return 1
    printf '%s' "$KC_AD_USER_SYNC" | grep -Eq '"failed"[[:space:]]*:[[:space:]]*0' || return 1
    KC_AD_USER_STATUS="$(printf '%s' "$KC_AD_USER_SYNC" | sed -n \
        's/.*"status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
    [ -n "$KC_AD_USER_STATUS" ] || KC_AD_USER_STATUS="синхронизация завершена"
    say "    пользователи AD: $KC_AD_USER_STATUS"

    KC_AD_GROUP_SYNC="$(keycloak_sync_mapper "$KC_AD_PROVIDER_ID" "$KC_AD_GROUP_MAPPER_ID")" || return 1
    printf '%s' "$KC_AD_GROUP_SYNC" | grep -Eq '"failed"[[:space:]]*:[[:space:]]*0' || return 1
    KC_AD_GROUP_ADDED="$(printf '%s' "$KC_AD_GROUP_SYNC" | sed -n \
        's/.*"added"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
    KC_AD_GROUP_UPDATED="$(printf '%s' "$KC_AD_GROUP_SYNC" | sed -n \
        's/.*"updated"[[:space:]]*:[[:space:]]*\([0-9][0-9]*\).*/\1/p')"
    [ -n "$KC_AD_GROUP_ADDED" ] || KC_AD_GROUP_ADDED=0
    [ -n "$KC_AD_GROUP_UPDATED" ] || KC_AD_GROUP_UPDATED=0
    KC_AD_GROUP_TOTAL=$((KC_AD_GROUP_ADDED + KC_AD_GROUP_UPDATED))
    [ "$KC_AD_GROUP_TOTAL" -ge 3 ] || {
        say "    AD не вернула все группы: $GROUP_ADMIN, $GROUP_OPERATOR, $GROUP_VIEWER"
        return 1
    }
    for KC_AD_EXPECTED_GROUP in "$GROUP_ADMIN" "$GROUP_OPERATOR" "$GROUP_VIEWER"; do
        KC_AD_GROUPS="$(curl -sS -k --fail -m 30 -H "Authorization: Bearer $KC_TOKEN" --get \
            --data-urlencode "search=$KC_AD_EXPECTED_GROUP" --data-urlencode 'exact=true' \
            "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/groups")" || return 1
        printf '%s' "$KC_AD_GROUPS" | setup_tool json-groups "$KC_AD_EXPECTED_GROUP" || return 1
    done
    KC_AD_GROUP_STATUS="$(printf '%s' "$KC_AD_GROUP_SYNC" | sed -n \
        's/.*"status"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p')"
    [ -n "$KC_AD_GROUP_STATUS" ] || KC_AD_GROUP_STATUS="синхронизация завершена"
    say "    группы AD: $KC_AD_GROUP_STATUS"

    KC_AD_Q_BIND_PASSWORD=""
    KC_AD_PROVIDER_BODY=""
    KC_AD_USER_SYNC=""
    if [ "$KEYCLOAK_AD_SIMPLE" -eq 1 ]; then
        set_plain_env JHV_KEYCLOAK_AD_PROVIDER "$KEYCLOAK_AD_PROVIDER" "$WORK/.env"
        set_plain_env JHV_KEYCLOAK_AD_DOMAIN "$KEYCLOAK_AD_DOMAIN" "$WORK/.env"
        set_plain_env JHV_KEYCLOAK_AD_CONTROLLER "$KEYCLOAK_AD_CONTROLLER" "$WORK/.env"
        set_plain_env JHV_KEYCLOAK_AD_URL "$KEYCLOAK_AD_URL" "$WORK/.env"
        set_plain_env JHV_KEYCLOAK_AD_USERS_DN "$KEYCLOAK_AD_USERS_DN" "$WORK/.env"
        set_plain_env JHV_KEYCLOAK_AD_GROUPS_DN "$KEYCLOAK_AD_GROUPS_DN" "$WORK/.env"
        case "$KEYCLOAK_AD_BIND_DN" in
            *[!A-Za-z0-9._@-]*) set_plain_env JHV_KEYCLOAK_AD_BIND_DN "" "$WORK/.env" ;;
            *) set_plain_env JHV_KEYCLOAK_AD_BIND_DN "$KEYCLOAK_AD_BIND_DN" "$WORK/.env" ;;
        esac
    fi
    say "    доменная авторизация настроена; источник членства: $KEYCLOAK_AD_GROUP_MODE_API"
}

# Заводит realm, группы, клиента и первую прикладную учётную запись. Повторный
# запуск не ломается: существующие объекты сохраняются, а отсутствующий первый
# пользователь добавляется через одноразовый recovery-admin.
keycloak_bootstrap() {
    KC_RECOVERY_ACTIVE=0
    if [ "$OIDC_EXISTING" -eq 1 ] && keycloak_realm_exists; then
        say "    realm $KEYCLOAK_REALM уже настроен; проверяются обязательные объекты и первый пользователь"
    fi

    KC_TOKEN="$(keycloak_token)"
    if [ -z "$KC_TOKEN" ]; then
        keycloak_start_recovery_admin || keycloak_bootstrap_die \
            "не удалось создать временного администратора Keycloak"
    fi
    keycloak_remove_stale_recovery_admins || keycloak_bootstrap_die \
        "не удалось удалить временного администратора от прерванной установки"

    KC_CODE="$(keycloak_post "" "{\"realm\":\"$KEYCLOAK_REALM\",\"enabled\":true,\"displayName\":\"oVirt Backup\",\"displayNameHtml\":\"oVirt Backup\",\"internationalizationEnabled\":true,\"defaultLocale\":\"ru\",\"supportedLocales\":[\"ru\",\"en\"]}")"
    case "$KC_CODE" in
        201|409) ;;
        *) keycloak_bootstrap_die "не удалось создать realm $KEYCLOAK_REALM (код $KC_CODE)" ;;
    esac

    for KC_GROUP in "$GROUP_ADMIN" "$GROUP_OPERATOR" "$GROUP_VIEWER"; do
        KC_CODE="$(keycloak_post "/$KEYCLOAK_REALM/groups" "{\"name\":$(json_quote "$KC_GROUP")}")"
        case "$KC_CODE" in
            201|409) ;;
            *) keycloak_bootstrap_die "не удалось создать группу $KC_GROUP (код $KC_CODE)" ;;
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
    KC_CLIENT="$(printf '%s' "$KC_CLIENT" | setup_tool json-client "$URL")" || keycloak_bootstrap_die "не удалось подготовить OIDC client"
    KC_CODE="$(keycloak_post "/$KEYCLOAK_REALM/clients" "$KC_CLIENT")"
    case "$KC_CODE" in
        201) ;;
        409)
            KC_CLIENT_LIST="$(curl -sS -k --fail -m 30 -H "Authorization: Bearer $KC_TOKEN" --get \
                --data-urlencode "clientId=$OIDC_CLIENT_ID" "$KEYCLOAK_API_URL/admin/realms/$KEYCLOAK_REALM/clients")" || keycloak_bootstrap_die "не удалось прочитать OIDC client"
            KC_EXISTING_CLIENT="$(printf '%s' "$KC_CLIENT_LIST" | setup_tool json-find clientId "$OIDC_CLIENT_ID")" || keycloak_bootstrap_die "OIDC client неоднозначен"
            KC_CLIENT_UUID="$(printf '%s' "$KC_EXISTING_CLIENT" | setup_tool json-get id)"
            [ -n "$KC_CLIENT_UUID" ] || keycloak_bootstrap_die "OIDC client не найден"
            KC_UPDATED_CLIENT="$(printf '%s' "$KC_EXISTING_CLIENT" | setup_tool json-client "$URL")" || keycloak_bootstrap_die "не удалось обновить OIDC client"
            [ "$(keycloak_put "/$KEYCLOAK_REALM/clients/$KC_CLIENT_UUID" "$KC_UPDATED_CLIENT")" = 204 ] || keycloak_bootstrap_die "не удалось сохранить redirect URI и mapper клиента"
            say "    адрес возврата, PKCE и groups mapper клиента сверены"
            ;;
        *) keycloak_bootstrap_die "не удалось создать клиента $OIDC_CLIENT_ID (код $KC_CODE)" ;;
    esac

    keycloak_create_permanent_admin || keycloak_bootstrap_die \
        "не удалось заменить временного bootstrap-admin постоянным администратором Keycloak"

    keycloak_create_app_admin || keycloak_bootstrap_die \
        "не удалось создать первого администратора приложения в realm $KEYCLOAK_REALM"

    keycloak_configure_ad || keycloak_bootstrap_die \
        "не удалось настроить Active Directory.
Проверьте LDAPS, цепочку CA, Users DN, Groups DN и наличие групп
$GROUP_ADMIN, $GROUP_OPERATOR, $GROUP_VIEWER в Active Directory."

    keycloak_secure_realm "$KEYCLOAK_REALM" || keycloak_bootstrap_die "не удалось включить MFA прикладного realm"
    keycloak_secure_realm master || keycloak_bootstrap_die "не удалось защитить master realm"
    KC_ADMIN_CLI="$(curl -sS -k --fail -m 30 -H "Authorization: Bearer $KC_TOKEN" --get \
        --data-urlencode 'clientId=admin-cli' "$KEYCLOAK_API_URL/admin/realms/master/clients" | \
        setup_tool json-find clientId admin-cli | setup_tool json-get id)"
    [ -n "$KC_ADMIN_CLI" ] || keycloak_bootstrap_die "не найден admin-cli"
    [ "$(keycloak_put "/master/clients/$KC_ADMIN_CLI" '{"directAccessGrantsEnabled":false}')" = 204 ] || keycloak_bootstrap_die "не удалось закрыть парольный grant admin-cli"

    keycloak_remove_recovery_admin || die "не удалось удалить временного администратора
$KC_RECOVERY_ADMIN_USER из master realm Keycloak"
}

keycloak_secure_realm() {
    KSR_REALM="$1"
    KSR_FLOW=jhvirt-browser-mfa-v1
    KSR_PATH="/$KSR_REALM/authentication/flows/$KSR_FLOW"
    KSR_CODE="$(keycloak_post "/$KSR_REALM/authentication/flows" \
        "{\"alias\":\"$KSR_FLOW\",\"description\":\"Password and mandatory OTP\",\"providerId\":\"basic-flow\",\"topLevel\":true,\"builtIn\":false}")"
    case "$KSR_CODE" in 201|409) ;; *) return 1 ;; esac
    for KSR_PROVIDER in auth-username-password-form auth-otp-form; do
        KSR_EXECUTIONS="$(curl -sS -k --fail -m 30 -H "Authorization: Bearer $KC_TOKEN" \
            "$KEYCLOAK_API_URL/admin/realms$KSR_PATH/executions")" || return 1
        KSR_EXECUTION="$(printf '%s' "$KSR_EXECUTIONS" | setup_tool json-find providerId "$KSR_PROVIDER")" || return 1
        if [ -z "$KSR_EXECUTION" ]; then
            KSR_CODE="$(keycloak_post "$KSR_PATH/executions/execution" "{\"provider\":\"$KSR_PROVIDER\"}")"
            [ "$KSR_CODE" = 201 ] || return 1
            KSR_EXECUTIONS="$(curl -sS -k --fail -m 30 -H "Authorization: Bearer $KC_TOKEN" \
                "$KEYCLOAK_API_URL/admin/realms$KSR_PATH/executions")" || return 1
            KSR_EXECUTION="$(printf '%s' "$KSR_EXECUTIONS" | setup_tool json-find providerId "$KSR_PROVIDER")" || return 1
        fi
        KSR_ID="$(printf '%s' "$KSR_EXECUTION" | setup_tool json-get id)"
        [ -n "$KSR_ID" ] || return 1
        [ "$(keycloak_put "$KSR_PATH/executions" "{\"id\":\"$KSR_ID\",\"requirement\":\"REQUIRED\"}")" = 204 ] || return 1
    done
    [ "$(keycloak_put "/$KSR_REALM" "{\"browserFlow\":\"$KSR_FLOW\",\"bruteForceProtected\":true,\"permanentLockout\":false,\"failureFactor\":5,\"waitIncrementSeconds\":60,\"maxFailureWaitSeconds\":900,\"maxDeltaTimeSeconds\":1800}")" = 204 ] || return 1
    say "    $KSR_REALM: обязательный OTP и ограничение перебора включены"
}

# --- Контейнеры -------------------------------------------------------------

docker_volume_put() {
    DVP_VOLUME="$1"; DVP_SOURCE="$2"; DVP_TARGET="$3"; DVP_MODE="$4"
    docker run --rm -i --network none --user root -v "$DVP_VOLUME:/data" \
        -e DVP_TARGET="$DVP_TARGET" -e DVP_MODE="$DVP_MODE" \
        "$POSTGRES_HELPER_IMAGE" sh -c '
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
        if [ -s "$MIGRATION_TMP/data/keycloak-helper.json" ]; then
            install -d -m 0700 -o root -g root "$PREFIX/keycloak-helper"
            install -m 0600 -o root -g root "$MIGRATION_TMP/data/keycloak-helper.json" \
                "$PREFIX/keycloak-helper/keycloak.json"
        fi
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
    set_plain_env JHV_BIND_ADDRESS "$BIND_ADDRESS" "$MAD_WORK/.env"
    set_plain_env JHV_ADMIN_PASSWORD "" "$MAD_WORK/.env"
    if [ "$(env_file_value "$MAD_WORK/.env" JHV_OIDC_ENABLED)" = true ]; then
        set_plain_env JHV_OIDC_REDIRECT_URL "$URL/api/v1/auth/oidc/callback" "$MAD_WORK/.env"
        set_plain_env JHV_OIDC_POST_LOGOUT_URL "$URL/login" "$MAD_WORK/.env"
        case "$(env_file_value "$MAD_WORK/.env" COMPOSE_PROFILES)" in
            *keycloak*) set_plain_env JHV_OIDC_BACKCHANNEL_URL "http://keycloak:8080" "$MAD_WORK/.env" ;;
        esac
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
    if [ -s "$MIGRATION_TMP/data/database.url" ]; then
        docker_volume_put "$MRD_VOL" "$MIGRATION_TMP/data/database.url" database.url 600
    fi
    if [ -s "$MIGRATION_TMP/data/oidc-client.secret" ]; then
        docker_volume_put "$MRD_VOL" "$MIGRATION_TMP/data/oidc-client.secret" oidc-client.secret 600
    fi
    if [ -s "$MIGRATION_TMP/data/bootstrap-admin.password" ]; then
        docker_volume_put "$MRD_VOL" "$MIGRATION_TMP/data/bootstrap-admin.password" bootstrap-admin.password 600
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

# apply_data_dir_overrides подставляет пути к данным, заданные ключами.
#
# Нужно прежде всего при переносе: пакет несёт пути прежнего сервера, а на новом
# диск монтируется в другое место или заменён сетевым хранилищем. Меняется
# только сторона хоста — внутри контейнера каталоги по-прежнему /backups и
# /restores, поэтому хранилища, записанные в базе, остаются рабочими и трогать
# их не нужно.
apply_data_dir_overrides() {
    ADO_WORK="$1"
    for ADO_PAIR in "JHV_BACKUP_DIR:$BACKUP_DIR_OVERRIDE" "JHV_RESTORE_DIR:$RESTORE_DIR_OVERRIDE"; do
        ADO_KEY="${ADO_PAIR%%:*}"; ADO_VALUE="${ADO_PAIR#*:}"
        [ -n "$ADO_VALUE" ] || continue
        ADO_OLD="$(env_file_value "$ADO_WORK/.env" "$ADO_KEY")"
        set_plain_env "$ADO_KEY" "$ADO_VALUE" "$ADO_WORK/.env"
        if [ -n "$ADO_OLD" ] && [ "$ADO_OLD" != "$ADO_VALUE" ]; then
            say "    $ADO_KEY: $ADO_OLD -> $ADO_VALUE"
        else
            say "    $ADO_KEY: $ADO_VALUE"
        fi
    done
}

# ask_data_dir спрашивает путь, когда каталог из пакета на этом узле не найден.
#
# Отказ здесь был бы формально правильным, но заставлял бы оператора городить
# симлинк или монтировать в чужое место только ради совпадения строки. Спросить
# дешевле и честнее: перенос на сервер с другой разметкой — обычное дело, а не
# исключение.
ask_data_dir() {
    ADD_KEY="$1"; ADD_MISSING="$2"; ADD_ANSWER=""
    say ""
    say "Каталог из пакета не найден на этом сервере:"
    say "  $ADD_KEY = $ADD_MISSING"
    say ""
    say "Так бывает, когда диск с копиями монтируется здесь в другое место."
    say "Укажите путь на этом сервере либо оставьте пустым, чтобы прервать импорт"
    say "и подключить прежнее хранилище."
    say ""
    printf 'Путь: '
    read -r ADD_ANSWER || ADD_ANSWER=""
    printf '%s' "$ADD_ANSWER"
}

prepare_docker_data_paths() {
    PDP_WORK="$1"
    for PDP_KEY in JHV_BACKUP_DIR JHV_RESTORE_DIR JHV_FILE_RESTORE_DIR; do
        PDP_VALUE="$(env_file_value "$PDP_WORK/.env" "$PDP_KEY")"
        [ -n "$PDP_VALUE" ] || continue
        PDP_PATH="$(docker_host_path "$PDP_WORK" "$PDP_VALUE")"
        if [ "$MIGRATION_ACTIVE" -eq 1 ] && [ ! -d "$PDP_PATH" ]; then
            case "$PDP_PATH" in
                "$PREFIX"/*|"$PDP_WORK"/*) ;;
                *)
                    # В диалоге предлагаем указать другой путь: перенос на
                    # сервер с иной разметкой — обычное дело. Без терминала
                    # спрашивать некого, поэтому там прежний отказ.
                    PDP_NEW=""
                    [ -t 0 ] && PDP_NEW="$(ask_data_dir "$PDP_KEY" "$PDP_PATH")"
                    [ -n "$PDP_NEW" ] || die "внешний каталог из $PDP_KEY не найден: $PDP_PATH
Подключите прежнее хранилище, создайте каталог или укажите другой путь:
  $SELF --migrate-from ... --backup-dir /путь --restore-dir /путь"
                    set_plain_env "$PDP_KEY" "$PDP_NEW" "$PDP_WORK/.env"
                    PDP_VALUE="$PDP_NEW"
                    PDP_PATH="$(docker_host_path "$PDP_WORK" "$PDP_NEW")"
                    say "    $PDP_KEY: $PDP_NEW"
                    ;;
            esac
        fi
        mkdir -p "$PDP_PATH" || die "не удалось создать каталог $PDP_PATH"
        # Меняется только сам корень, не содержимое подключённого хранилища.
        if ! docker run --rm --network none --user 10001:10001 -v "$PDP_PATH:/target" \
                "$POSTGRES_HELPER_IMAGE" test -w /target >/dev/null 2>&1; then
            # Для пустого локального каталога установщик может исправить сам
            # корень. Содержимое и ACL подключённого хранилища не меняются.
            docker run --rm --network none --user root -v "$PDP_PATH:/target" \
                "$POSTGRES_HELPER_IMAGE" \
                sh -c 'chown 10001:10001 /target && chmod u+rwx /target' >/dev/null 2>&1 || true
            docker run --rm --network none --user 10001:10001 -v "$PDP_PATH:/target" \
                "$POSTGRES_HELPER_IMAGE" test -w /target >/dev/null 2>&1 ||
                die "контейнерный UID 10001 не получил право записи в $PDP_PATH"
        fi
        # Пустой каталог при переносе — почти всегда забытое хранилище: база
        # приехала и знает о копиях, а самих копий на новом узле нет. Служба
        # при этом поднимется и покажет их в списке, а «файл не найден» вылезет
        # позже, при первом восстановлении. Сказать сразу дешевле.
        if [ "$MIGRATION_ACTIVE" -eq 1 ] && [ "$PDP_KEY" = JHV_BACKUP_DIR ] &&
                [ -z "$(ls -A "$PDP_PATH" 2>/dev/null)" ]; then
            say "    внимание: $PDP_PATH пуст — копии на этот сервер ещё не подключены"
            say "    база будет знать о них, но восстановление не найдёт данных"
        fi
    done
}

prepare_docker_file_backup_source() {
    PDFB_WORK="$1"
    PDFB_VALUE="$(env_file_value "$PDFB_WORK/.env" JHV_FILE_BACKUP_DIR)"
    [ -n "$PDFB_VALUE" ] || return 1
    PDFB_PATH="$(docker_host_path "$PDFB_WORK" "$PDFB_VALUE")"
    PDFB_CREATED=0
    if [ ! -d "$PDFB_PATH" ]; then
        mkdir -p "$PDFB_PATH" || die "не удалось создать каталог $PDFB_PATH"
        PDFB_CREATED=1
    fi
    if ! docker run --rm --network none --user 10001:10001 -v "$PDFB_PATH:/source:ro" \
            "$POSTGRES_HELPER_IMAGE" sh -c 'test -r /source && test -x /source' >/dev/null 2>&1; then
        if [ "$PDFB_CREATED" -eq 1 ] || [ -z "$(ls -A "$PDFB_PATH" 2>/dev/null)" ]; then
            docker run --rm --network none --user root -v "$PDFB_PATH:/source" \
                "$POSTGRES_HELPER_IMAGE" \
                sh -c 'chown 10001:10001 /source && chmod 0750 /source' >/dev/null 2>&1 || true
        fi
        docker run --rm --network none --user 10001:10001 -v "$PDFB_PATH:/source:ro" \
            "$POSTGRES_HELPER_IMAGE" sh -c 'test -r /source && test -x /source' >/dev/null 2>&1 ||
            die "контейнерный UID 10001 не может читать $PDFB_PATH
Разрешите чтение каталога и файлов либо задайте другой JHV_FILE_BACKUP_DIR."
    fi
    say "    источник файловых бэкапов: $PDFB_PATH (в контейнере только чтение)"
}

prepare_docker_keycloak_truststore() {
    PDKT_WORK="$1"
    [ "$OIDC_MODE" = keycloak ] || return 0
    PDKT_VALUE="$(env_file_value "$PDKT_WORK/.env" JHV_KEYCLOAK_TRUSTSTORE_DIR)"
    [ -n "$PDKT_VALUE" ] || return 1
    PDKT_PATH="$(docker_host_path "$PDKT_WORK" "$PDKT_VALUE")"
    mkdir -p "$PDKT_PATH" || die "не удалось создать каталог $PDKT_PATH"
    if [ "$MIGRATION_ACTIVE" -eq 1 ] && [ -d "$MIGRATION_TMP/truststores" ]; then
        for PDKT_MIGRATED in "$MIGRATION_TMP/truststores"/*; do
            [ -f "$PDKT_MIGRATED" ] || continue
            install -o root -g root -m 0644 "$PDKT_MIGRATED" \
                "$PDKT_PATH/$(basename "$PDKT_MIGRATED")" ||
                die "не удалось восстановить truststore Keycloak"
        done
    fi
    if [ "$KEYCLOAK_AD_REQUESTED" -eq 1 ] && [ -n "$KEYCLOAK_AD_CA_FILE" ]; then
        PDKT_TMP="$PDKT_PATH/.active-directory-ca.pem.$$"
        install -o root -g root -m 0644 "$KEYCLOAK_AD_CA_FILE" "$PDKT_TMP" ||
            die "не удалось установить CA в $PDKT_PATH"
        mv -f "$PDKT_TMP" "$PDKT_PATH/active-directory-ca.pem" ||
            die "не удалось опубликовать CA в $PDKT_PATH"
        KEYCLOAK_AD_CA_TARGET="$PDKT_PATH/active-directory-ca.pem"
    fi
    if ! docker run --rm --network none --user 1000:0 -v "$PDKT_PATH:/trust:ro,z" \
            "$POSTGRES_HELPER_IMAGE" sh -c 'test -r /trust && test -x /trust' >/dev/null 2>&1; then
        if [ -z "$(ls -A "$PDKT_PATH" 2>/dev/null)" ]; then
            docker run --rm --network none --user root -v "$PDKT_PATH:/trust" \
                "$POSTGRES_HELPER_IMAGE" \
                sh -c 'chown 1000:0 /trust && chmod 0750 /trust' >/dev/null 2>&1 || true
        fi
        docker run --rm --network none --user 1000:0 -v "$PDKT_PATH:/trust:ro,z" \
            "$POSTGRES_HELPER_IMAGE" sh -c 'test -r /trust && test -x /trust' >/dev/null 2>&1 ||
            die "Keycloak UID 1000 не может читать $PDKT_PATH
Разрешите чтение каталога и CA-файлов либо задайте другой JHV_KEYCLOAK_TRUSTSTORE_DIR."
    fi
    if [ "$KEYCLOAK_AD_REQUESTED" -eq 1 ]; then
        PDKT_CERT_FOUND=0
        for PDKT_CERT in "$PDKT_PATH"/*; do
            [ -f "$PDKT_CERT" ] || continue
            if grep -q -- '-----BEGIN CERTIFICATE-----' "$PDKT_CERT" 2>/dev/null; then
                PDKT_CERT_FOUND=1
                break
            fi
        done
        [ "$PDKT_CERT_FOUND" -eq 1 ] || die "в $PDKT_PATH нет PEM-сертификата CA для LDAPS"
    fi
    say "    доверенные CA Keycloak: $PDKT_PATH (только чтение)"
    if [ -n "$KEYCLOAK_AD_CA_TARGET" ]; then
        say "    CA Active Directory: $KEYCLOAK_AD_CA_TARGET"
    fi
}

prepare_docker_keycloak_vault() {
    PDKV_WORK="$1"
    [ "$OIDC_MODE" = keycloak ] || return 0
    PDKV_VALUE="$(env_file_value "$PDKV_WORK/.env" JHV_KEYCLOAK_VAULT_DIR)"
    [ -n "$PDKV_VALUE" ] || return 1
    PDKV_PATH="$(docker_host_path "$PDKV_WORK" "$PDKV_VALUE")"
    PDKV_SECRET="$PDKV_PATH/${KEYCLOAK_REALM}_ad-bind"
    mkdir -p "$PDKV_PATH" || die "не удалось создать каталог $PDKV_PATH"
    chown root:root "$PDKV_PATH" 2>/dev/null || true
    chmod 0750 "$PDKV_PATH" || die "не удалось защитить каталог $PDKV_PATH"

    PDKV_SOURCE=""
    if [ "$KEYCLOAK_AD_REQUESTED" -eq 1 ]; then
        PDKV_SOURCE="$KEYCLOAK_AD_BIND_PASSWORD_FILE"
    elif [ "$MIGRATION_ACTIVE" -eq 1 ] && [ -s "$MIGRATION_TMP/data/keycloak-ad-bind" ]; then
        PDKV_SOURCE="$MIGRATION_TMP/data/keycloak-ad-bind"
    fi
    if [ -n "$PDKV_SOURCE" ]; then
        PDKV_TMP="$PDKV_PATH/.${KEYCLOAK_REALM}_ad-bind.$$"
        install -o root -g root -m 0440 "$PDKV_SOURCE" "$PDKV_TMP" ||
            die "не удалось установить bind-пароль в Keycloak vault"
        mv -f "$PDKV_TMP" "$PDKV_SECRET" ||
            die "не удалось опубликовать bind-пароль в Keycloak vault"
    elif [ "$KEYCLOAK_AD_REQUESTED" -eq 1 ] && [ -n "$KEYCLOAK_AD_BIND_PASSWORD" ]; then
        PDKV_TMP="$PDKV_PATH/.${KEYCLOAK_REALM}_ad-bind.$$"
        umask 077
        printf '%s\n' "$KEYCLOAK_AD_BIND_PASSWORD" > "$PDKV_TMP" ||
            die "не удалось записать bind-пароль в Keycloak vault"
        umask 022
        chown root:root "$PDKV_TMP" 2>/dev/null || true
        chmod 0440 "$PDKV_TMP" || die "не удалось защитить bind-пароль Keycloak"
        mv -f "$PDKV_TMP" "$PDKV_SECRET" ||
            die "не удалось опубликовать bind-пароль в Keycloak vault"
        KEYCLOAK_AD_BIND_PASSWORD=""
    fi

    if [ -f "$PDKV_SECRET" ]; then
        chown root:root "$PDKV_SECRET" 2>/dev/null || true
        chmod 0440 "$PDKV_SECRET" || die "не удалось защитить $PDKV_SECRET"
        docker run --rm --network none --user 1000:0 -v "$PDKV_PATH:/vault:ro,z" \
            "$POSTGRES_HELPER_IMAGE" sh -c \
            "test -s '/vault/${KEYCLOAK_REALM}_ad-bind' && test -r '/vault/${KEYCLOAK_REALM}_ad-bind'" \
            >/dev/null 2>&1 || die "Keycloak UID 1000:GID 0 не может прочитать $PDKV_SECRET"
        KEYCLOAK_AD_VAULT_TARGET="$PDKV_SECRET"
    elif [ "$KEYCLOAK_AD_REQUESTED" -eq 1 ] || \
            { [ "$MIGRATION_ACTIVE" -eq 1 ] && [ -s "$MIGRATION_TMP/data/keycloak-ad-bind" ]; }; then
        die "в Keycloak vault отсутствует ${KEYCLOAK_REALM}_ad-bind"
    fi
    say "    Keycloak file vault: $PDKV_PATH (read-only в контейнере)"
}

prepare_docker_dr_backup() {
    PDB_WORK="$1"
    PDB_VALUE="$DR_BACKUP_DIR_OVERRIDE"
    [ -n "$PDB_VALUE" ] || PDB_VALUE="$(env_file_value "$PDB_WORK/.env" JHV_DR_BACKUP_DIR)"
    if [ -z "$PDB_VALUE" ]; then
        if [ "$BUNDLE" -eq 1 ]; then PDB_VALUE="$PREFIX/dr-backups"; else PDB_VALUE="./dr-backups"; fi
    fi
    set_plain_env JHV_DR_BACKUP_DIR "$PDB_VALUE" "$PDB_WORK/.env"
    set_plain_env JHV_DR_ENABLED true "$PDB_WORK/.env"
    PDB_PATH="$(docker_host_path "$PDB_WORK" "$PDB_VALUE")"
    mkdir -p "$PDB_PATH/app" "$PDB_PATH/keycloak" ||
        die "не удалось создать каталог аварийных копий $PDB_PATH"

    # Разные backup-контейнеры работают под UID приложения и Keycloak. Они
    # получают только собственные подкаталоги и не могут менять копии соседа.
    docker run --rm --network none --user root -v "$PDB_PATH:/dr" \
        "$POSTGRES_HELPER_IMAGE" sh -c '
            mkdir -p /dr/app/postgres /dr/keycloak/postgres
            chown -R 10001:10001 /dr/app
            chown -R 1000:0 /dr/keycloak
            chmod 0700 /dr/app /dr/app/postgres /dr/keycloak /dr/keycloak/postgres' ||
        die "не удалось подготовить права $PDB_PATH"
    say "    аварийные копии: $PDB_PATH"
}

prepare_keycloak_runtime() {
    [ "$OIDC_MODE" = keycloak ] || return 0
    PKR_VOL="$(keycloak_data_volume)"
    PKR_APP_VOL="$(docker_metrics_volume)"
    ensure_labeled_volume "$PKR_VOL" keycloak-data

    KEYCLOAK_DB_PASSWORD="$(docker_volume_value "$PKR_VOL" ovirt-backup/database.password)"
    [ -n "$KEYCLOAK_DB_PASSWORD" ] || KEYCLOAK_DB_PASSWORD="$(gen_secret 24)"
    [ -n "$KEYCLOAK_DB_PASSWORD" ] || die "не удалось подготовить пароль базы Keycloak"
    printf '%s\n' "$KEYCLOAK_DB_PASSWORD" |
        docker_volume_write "$PKR_VOL" ovirt-backup/database.password 1000:0 0400

    if [ "$KEYCLOAK_DIRECT_TLS" -eq 1 ]; then
        docker run --rm --network none --user root \
            -v "$PKR_APP_VOL:/source:ro" -v "$PKR_VOL:/target" \
            "$POSTGRES_HELPER_IMAGE" sh -c '
                test -s /source/tls/server.crt && test -s /source/tls/server.key
                mkdir -p /target/ovirt-backup
                cp /source/tls/server.crt /target/ovirt-backup/server.crt
                cp /source/tls/server.key /target/ovirt-backup/server.key
                chown 1000:0 /target/ovirt-backup/server.crt /target/ovirt-backup/server.key
                chmod 0444 /target/ovirt-backup/server.crt
                chmod 0400 /target/ovirt-backup/server.key' ||
            die "не удалось передать TLS-сертификат встроенному Keycloak"
    fi

    {
        printf 'db=postgres\n'
        printf 'db-url=jdbc:postgresql://postgres:5432/%s\n' "${KEYCLOAK_DB:-keycloak}"
        printf 'db-username=%s\n' "$KEYCLOAK_DB_USER"
        printf 'db-password=%s\n' "$KEYCLOAK_DB_PASSWORD"
        printf 'hostname=%s\n' "$KEYCLOAK_URL"
        printf 'hostname-strict=true\n'
        printf 'http-enabled=true\nhttp-port=8080\n'
        printf 'health-enabled=true\ncache=local\n'
        if [ "$KEYCLOAK_DIRECT_TLS" -eq 1 ]; then
            printf 'https-port=8443\n'
            printf 'https-certificate-file=/opt/keycloak/data/ovirt-backup/server.crt\n'
            printf 'https-certificate-key-file=/opt/keycloak/data/ovirt-backup/server.key\n'
        else
            case "$KEYCLOAK_URL" in https://*) printf 'proxy-headers=xforwarded\n' ;; esac
        fi
        if [ -n "$KEYCLOAK_ADMIN_PASSWORD" ]; then
            printf 'bootstrap-admin-username=%s\n' "${KEYCLOAK_BOOTSTRAP_USER:-$KEYCLOAK_ADMIN_USER}"
            printf 'bootstrap-admin-password=%s\n' "$KEYCLOAK_ADMIN_PASSWORD"
        fi
    } | docker_volume_write "$PKR_VOL" ovirt-backup/keycloak.conf 1000:0 0400

    docker run --rm --network none --user root -v "$PKR_VOL:/data" \
        "$POSTGRES_HELPER_IMAGE" sh -c 'chown 1000:0 /data /data/ovirt-backup; chmod 0750 /data /data/ovirt-backup' ||
        die "не удалось выставить владельца тома Keycloak"
}

clear_keycloak_bootstrap_secret() {
    CKBS_VOL="$(keycloak_data_volume)"
    volume_exists "$CKBS_VOL" || return 0
    docker run --rm --network none --user root -v "$CKBS_VOL:/data" \
        "$POSTGRES_HELPER_IMAGE" sh -c '
            src=/data/ovirt-backup/keycloak.conf
            test -f "$src" || exit 0
            sed "/^bootstrap-admin-password=/d; /^bootstrap-admin-username=/d" \
                "$src" > "$src.tmp"
            chown 1000:0 "$src.tmp"
            chmod 0400 "$src.tmp"
            mv "$src.tmp" "$src"' || die "не удалось удалить bootstrap-пароль Keycloak"
}

valid_pg_identifier() {
    case "$1" in ""|*[!A-Za-z0-9_]*) return 1 ;; esac
}

ensure_docker_database_roles() {
    EDDR_WORK="$1"; EDDR_RUN="$2"
    EDDR_ADMIN="$(env_file_value "$EDDR_WORK/.env" POSTGRES_USER)"; [ -n "$EDDR_ADMIN" ] || EDDR_ADMIN=jhvirt
    EDDR_DB="$(env_file_value "$EDDR_WORK/.env" POSTGRES_DB)"; [ -n "$EDDR_DB" ] || EDDR_DB=jhvirt
    valid_pg_identifier "$EDDR_ADMIN" || die "небезопасное имя администратора PostgreSQL: $EDDR_ADMIN"
    valid_pg_identifier "$EDDR_DB" || die "небезопасное имя базы PostgreSQL: $EDDR_DB"

    # Старое приложение может держать соединения под административной ролью.
    # Сначала останавливаем клиентов, затем меняем владельцев и пароли.
    # shellcheck disable=SC2086
    (cd "$EDDR_WORK" && $EDDR_RUN stop "$COMPOSE_SERVICE" keycloak) >/dev/null 2>&1 || true
    # shellcheck disable=SC2086
    (cd "$EDDR_WORK" && $EDDR_RUN up -d postgres) >/dev/null 2>&1 || die "не удалось запустить PostgreSQL"
    docker_wait_postgres "$EDDR_WORK" "$EDDR_RUN" "$EDDR_ADMIN" || die "PostgreSQL не стала готова"

    step "разделение ролей PostgreSQL"
    # Пароли шестнадцатеричные и сгенерированы установщиком; имена проходят
    # строгую проверку выше. SQL идёт по stdin и не виден в списке процессов.
    # shellcheck disable=SC2086
    (cd "$EDDR_WORK" && $EDDR_RUN exec -T postgres psql -U "$EDDR_ADMIN" -d postgres -v ON_ERROR_STOP=1) <<SQL
SELECT 'CREATE ROLE "$APP_DB_USER" LOGIN' WHERE NOT EXISTS
  (SELECT 1 FROM pg_roles WHERE rolname='$APP_DB_USER') \gexec
ALTER ROLE "$APP_DB_USER" WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '$APP_DB_PASSWORD';
ALTER DATABASE "$EDDR_DB" OWNER TO "$APP_DB_USER";
REVOKE CONNECT ON DATABASE "$EDDR_DB" FROM PUBLIC;
GRANT CONNECT ON DATABASE "$EDDR_DB" TO "$APP_DB_USER", "$EDDR_ADMIN";
SQL

    # shellcheck disable=SC2086
    (cd "$EDDR_WORK" && $EDDR_RUN exec -T postgres psql -U "$EDDR_ADMIN" -d "$EDDR_DB" -v ON_ERROR_STOP=1) <<SQL
SELECT format(
  'ALTER %s %I.%I OWNER TO %I',
  CASE c.relkind
    WHEN 'r' THEN 'TABLE'
    WHEN 'p' THEN 'TABLE'
    WHEN 'S' THEN 'SEQUENCE'
    WHEN 'v' THEN 'VIEW'
    WHEN 'm' THEN 'MATERIALIZED VIEW'
    WHEN 'f' THEN 'FOREIGN TABLE'
  END,
  n.nspname,
  c.relname,
  '$APP_DB_USER'
)
FROM pg_class AS c
JOIN pg_namespace AS n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
  AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = '$EDDR_ADMIN')
  AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f')
  AND (c.relkind <> 'S' OR NOT EXISTS (
    SELECT 1
    FROM pg_depend AS d
    WHERE d.classid = 'pg_class'::regclass
      AND d.objid = c.oid
      AND d.refclassid = 'pg_class'::regclass
      AND d.deptype IN ('a', 'i')
  ))
\gexec
ALTER SCHEMA public OWNER TO "$APP_DB_USER";
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO "$APP_DB_USER";
SQL

    if [ "$OIDC_MODE" = keycloak ]; then
        # shellcheck disable=SC2086
        (cd "$EDDR_WORK" && $EDDR_RUN exec -T postgres psql -U "$EDDR_ADMIN" -d postgres -v ON_ERROR_STOP=1) <<SQL
SELECT 'CREATE ROLE "$KEYCLOAK_DB_USER" LOGIN' WHERE NOT EXISTS
  (SELECT 1 FROM pg_roles WHERE rolname='$KEYCLOAK_DB_USER') \gexec
ALTER ROLE "$KEYCLOAK_DB_USER" WITH LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION PASSWORD '$KEYCLOAK_DB_PASSWORD';
SELECT 'CREATE DATABASE keycloak OWNER "$KEYCLOAK_DB_USER"' WHERE NOT EXISTS
  (SELECT 1 FROM pg_database WHERE datname='keycloak') \gexec
ALTER DATABASE keycloak OWNER TO "$KEYCLOAK_DB_USER";
REVOKE CONNECT ON DATABASE keycloak FROM PUBLIC;
GRANT CONNECT ON DATABASE keycloak TO "$KEYCLOAK_DB_USER", "$EDDR_ADMIN";
SQL
        # shellcheck disable=SC2086
        (cd "$EDDR_WORK" && $EDDR_RUN exec -T postgres psql -U "$EDDR_ADMIN" -d keycloak -v ON_ERROR_STOP=1) <<SQL
SELECT format(
  'ALTER %s %I.%I OWNER TO %I',
  CASE c.relkind
    WHEN 'r' THEN 'TABLE'
    WHEN 'p' THEN 'TABLE'
    WHEN 'S' THEN 'SEQUENCE'
    WHEN 'v' THEN 'VIEW'
    WHEN 'm' THEN 'MATERIALIZED VIEW'
    WHEN 'f' THEN 'FOREIGN TABLE'
  END,
  n.nspname,
  c.relname,
  '$KEYCLOAK_DB_USER'
)
FROM pg_class AS c
JOIN pg_namespace AS n ON n.oid = c.relnamespace
WHERE n.nspname = 'public'
  AND c.relowner = (SELECT oid FROM pg_roles WHERE rolname = '$EDDR_ADMIN')
  AND c.relkind IN ('r', 'p', 'S', 'v', 'm', 'f')
  AND (c.relkind <> 'S' OR NOT EXISTS (
    SELECT 1
    FROM pg_depend AS d
    WHERE d.classid = 'pg_class'::regclass
      AND d.objid = c.oid
      AND d.refclassid = 'pg_class'::regclass
      AND d.deptype IN ('a', 'i')
  ))
\gexec
ALTER SCHEMA public OWNER TO "$KEYCLOAK_DB_USER";
REVOKE CREATE ON SCHEMA public FROM PUBLIC;
GRANT USAGE, CREATE ON SCHEMA public TO "$KEYCLOAK_DB_USER";
SQL
    fi
    say "    приложение и Keycloak используют разные непривилегированные роли"
}

install_keycloak_host_helper() {
    [ "$BUNDLE" -eq 1 ] || return 0
    [ -x "$PREFIX/bin/jhvirt-keycloak-helper" ] ||
        die "в комплекте нет jhvirt-keycloak-helper"
    [ -f "$HERE/systemd/jhvirt-keycloak-helper.service" ] ||
        die "в комплекте нет unit Keycloak helper"
    [ -f "$HERE/systemd/jhvirt-keycloak-helper.socket" ] ||
        die "в комплекте нет socket unit Keycloak helper"

    if ! has_systemd; then
        say "    предупреждение: systemd недоступен; запуск встроенного Keycloak из web отключён"
        set_plain_env JHV_HOST_HELPER_GID 65534 "$WORK/.env"
        return 0
    fi

    step "установка host helper для встроенного Keycloak"
    for HELPER_PATH in "$PREFIX/compose" "$PREFIX/keycloak-helper" \
            "$PREFIX/keycloak-truststores" "$PREFIX/keycloak-vault"; do
        [ ! -L "$HELPER_PATH" ] || die "путь host helper не должен быть symlink: $HELPER_PATH"
        [ ! -e "$HELPER_PATH" ] || [ -d "$HELPER_PATH" ] ||
            die "путь host helper должен быть каталогом: $HELPER_PATH"
    done
    if find "$PREFIX/compose" -type l -print | grep -q .; then
        die "каталог Compose содержит symlink; root helper отказывается его использовать"
    fi
    install -d -m 0700 -o root -g root "$PREFIX/keycloak-helper"
    chown root:root "$PREFIX/keycloak-helper" "$PREFIX/keycloak-truststores" "$PREFIX/keycloak-vault"
    chmod 0700 "$PREFIX/keycloak-helper"
    chmod 0750 "$PREFIX/keycloak-truststores" "$PREFIX/keycloak-vault"
    chown -R root:root "$PREFIX/compose"
    chmod 0750 "$PREFIX/compose"
    chmod 0600 "$PREFIX/compose/.env"
    if [ -e "$PREFIX/keycloak-helper/keycloak.json" ]; then
        [ -f "$PREFIX/keycloak-helper/keycloak.json" ] && [ ! -L "$PREFIX/keycloak-helper/keycloak.json" ] ||
            die "состояние host helper имеет недопустимый тип"
        chown root:root "$PREFIX/keycloak-helper/keycloak.json"
        chmod 0600 "$PREFIX/keycloak-helper/keycloak.json"
    fi
    chown root:root "$PREFIX/bin/jhvirt-keycloak-helper"
    chmod 0755 "$PREFIX/bin/jhvirt-keycloak-helper"
    sed -e "s|@PREFIX@|$PREFIX|g" -e "s|@USER_NAME@|$USER_NAME|g" \
        "$HERE/systemd/jhvirt-keycloak-helper.service" > "$KEYCLOAK_HELPER_UNIT.tmp"
    sed -e "s|@PREFIX@|$PREFIX|g" -e "s|@USER_NAME@|$USER_NAME|g" \
        "$HERE/systemd/jhvirt-keycloak-helper.socket" > "$KEYCLOAK_HELPER_SOCKET.tmp"
    install -m 0644 "$KEYCLOAK_HELPER_UNIT.tmp" "$KEYCLOAK_HELPER_UNIT"
    install -m 0644 "$KEYCLOAK_HELPER_SOCKET.tmp" "$KEYCLOAK_HELPER_SOCKET"
    rm -f "$KEYCLOAK_HELPER_UNIT.tmp" "$KEYCLOAK_HELPER_SOCKET.tmp"

    HELPER_GID="$(id -g "$USER_NAME")"
    case "$HELPER_GID" in ''|*[!0-9]*) die "не удалось определить GID группы $USER_NAME" ;; esac
    set_plain_env JHV_HOST_HELPER_GID "$HELPER_GID" "$WORK/.env"
    systemctl daemon-reload
}

start_keycloak_host_helper() {
    [ "$BUNDLE" -eq 1 ] || return 0
    has_systemd || return 0
    systemctl enable --now jhvirt-keycloak-helper.socket >/dev/null
    say "    Unix-сокет управления Keycloak доступен только root и группе $USER_NAME"
}

install_containers() {
    RUN="$(runner "$MODE")"
	# Старый helper не должен работать, пока установщик заменяет его бинарник и
	# временно меняет владельцев дерева bundle. При ошибке обновления сокет
	# останется остановлен и не запустит root-процесс с частичной конфигурацией.
	if [ "$BUNDLE" -eq 1 ] && has_systemd; then
		systemctl stop jhvirt-keycloak-helper.socket jhvirt-keycloak-helper.service >/dev/null 2>&1 || true
	fi

    if [ "$BUNDLE" -eq 1 ]; then
        WORK="$PREFIX/compose"
    else
        WORK="$COMPOSE_DIR"
    fi
	DOCKER_ENV_EXISTED=0
	[ -f "$WORK/.env" ] && DOCKER_ENV_EXISTED=1

    if [ "$START" -eq 1 ] && ! have curl && ! have wget; then
        die "для проверки готовности нужен curl или wget"
    fi

    # Спрашивается после адреса службы: из него складывается и адрес Keycloak,
    # и адрес возврата, который провайдер сверяет побуквенно.
    if [ "$MIGRATION_ACTIVE" -eq 0 ]; then
        select_oidc_config
        load_existing_oidc
        choose_oidc
        prepare_oidc
    else
        OIDC_MODE=none
    fi

    if [ "$BUNDLE" -eq 1 ]; then
        step "раскладка в $PREFIX"
        ensure_service_user
        mkdir -p "$PREFIX/compose" "$PREFIX/config" "$PREFIX/data" "$PREFIX/logs" \
                 "$PREFIX/docs" "$PREFIX/backups" "$PREFIX/restores" \
                 "$PREFIX/file-sources" "$PREFIX/file-restores" \
                 "$PREFIX/keycloak-truststores" "$PREFIX/keycloak-vault" "$PREFIX/keycloak-helper" \
                 "$PREFIX/proxmox"
        rm -rf "${PREFIX:?}/bin" "${PREFIX:?}/web"
        # Образ собирается из bin/ и web/dist рядом с Dockerfile, поэтому весь
        # комплект копируется целиком.
        cp -r "$HERE/bin" "$HERE/web" "$PREFIX/"
        cp "$HERE/Dockerfile" "$PREFIX/Dockerfile"
        install_bundle_config
        cp "$HERE/compose/docker-compose.yml" "$HERE/compose/.env.example" \
            "$HERE/compose/Dockerfile.keycloak" "$HERE/compose/Dockerfile.postgres" \
            "$HERE/compose/dr-backup.sh" "$PREFIX/compose/"
		install -m 0755 "$HERE/recover-admin.sh" "$PREFIX/bin/ovirt-backup-recover-admin"
		install -m 0755 "$HERE/recover-keycloak.sh" "$PREFIX/bin/ovirt-backup-recover-keycloak"
        chmod 644 "$PREFIX/Dockerfile" "$PREFIX/compose/docker-compose.yml" \
            "$PREFIX/compose/.env.example" "$PREFIX/compose/Dockerfile.keycloak" \
			"$PREFIX/compose/Dockerfile.postgres" \
            "$PREFIX/compose/dr-backup.sh"
        [ -d "$HERE/docs" ] && cp -r "$HERE/docs/." "$PREFIX/docs/"
        [ -f "$HERE/proxmox/jhvirt-pve-data-plane" ] && \
            install -o root -g root -m 0755 "$HERE/proxmox/jhvirt-pve-data-plane" "$PREFIX/proxmox/"
        [ -f "$HERE/VERSION" ] && cp "$HERE/VERSION" "$PREFIX/"
        WORK="$PREFIX/compose"
        BACKUPS="$PREFIX/backups"; RESTORES="$PREFIX/restores"
        FILE_SOURCES="$PREFIX/file-sources"; FILE_RESTORES="$PREFIX/file-restores"
        KEYCLOAK_TRUSTSTORES="$PREFIX/keycloak-truststores"
        KEYCLOAK_VAULT="$PREFIX/keycloak-vault"
    else
        WORK="$COMPOSE_DIR"
        mkdir -p "$WORK/backups" "$WORK/restores" "$WORK/file-sources" "$WORK/file-restores" \
            "$WORK/keycloak-truststores" "$WORK/keycloak-vault"
        BACKUPS="./backups"; RESTORES="./restores"
        FILE_SOURCES="./file-sources"; FILE_RESTORES="./file-restores"
        KEYCLOAK_TRUSTSTORES="./keycloak-truststores"
        KEYCLOAK_VAULT="./keycloak-vault"
    fi

    migration_apply_docker_files "$WORK"
    if [ "$MIGRATION_ACTIVE" -eq 1 ]; then
        # Способ входа известен только после распаковки перенесённого .env.
        # Загрузить его нужно до подготовки тома и file vault Keycloak.
        OIDC_MODE=""
        select_oidc_config
        load_existing_oidc
        [ -n "$OIDC_MODE" ] || OIDC_MODE=none
        prepare_oidc
    fi

	if [ -f "$WORK/.env" ]; then
        say "    $WORK/.env уже есть; пароль базы и пользовательские настройки сохранены"
        LOCAL_ADMIN_USER="$(env_file_value "$WORK/.env" JHV_AUTH_BOOTSTRAP_USER)"
        [ -n "$LOCAL_ADMIN_USER" ] || LOCAL_ADMIN_USER=admin
        set_plain_env JHV_EXTERNAL_URL "$URL" "$WORK/.env"
		set_plain_env JHV_PORT "$PORT" "$WORK/.env"
		set_plain_env JHV_BIND_ADDRESS "$BIND_ADDRESS" "$WORK/.env"
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
            printf 'JHV_BIND_ADDRESS=%s\n' "$BIND_ADDRESS"
            printf 'JHV_AUTH_BOOTSTRAP_USER=%s\n' "$LOCAL_ADMIN_USER"
            printf 'JHV_ADMIN_PASSWORD=%s\n' "$ADMPASS"
            printf 'JHV_BACKUP_DIR=%s\n' "$BACKUPS"
            printf 'JHV_RESTORE_DIR=%s\n' "$RESTORES"
            printf 'JHV_FILE_BACKUP_DIR=%s\n' "$FILE_SOURCES"
            printf 'JHV_FILE_RESTORE_DIR=%s\n' "$FILE_RESTORES"
            printf 'JHV_KEYCLOAK_TRUSTSTORE_DIR=%s\n' "$KEYCLOAK_TRUSTSTORES"
            printf 'JHV_KEYCLOAK_VAULT_DIR=%s\n' "$KEYCLOAK_VAULT"
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
		if [ -f "$PREFIX/proxmox/jhvirt-pve-data-plane" ]; then
			chown root:root "$PREFIX/proxmox" "$PREFIX/proxmox/jhvirt-pve-data-plane"
			chmod 0755 "$PREFIX/proxmox" "$PREFIX/proxmox/jhvirt-pve-data-plane"
		fi
		chmod 700 "$PREFIX/data"
		# YAML не содержит паролей, а контейнер читает bind mount под UID
		# 10001, который не обязан совпадать с системным пользователем хоста.
		chmod 644 "$PREFIX/config/$CONFIG_NAME"
	fi
	install_keycloak_host_helper

	if [ "$BUNDLE" -eq 1 ]; then
		RECOVERY_TOKEN_FILE="$PREFIX/config/recovery.token"
	else
		RECOVERY_TOKEN_FILE="$WORK/.recovery-token"
	fi
	step "host-only recovery-токен"
	ensure_recovery_token "$RECOVERY_TOKEN_FILE"
	set_plain_env JHV_RECOVERY_TOKEN_HASH "$RECOVERY_TOKEN_HASH" "$WORK/.env"
	if [ "$BUNDLE" -eq 1 ]; then
		RECOVERY_COMMAND="sudo $PREFIX/bin/ovirt-backup-recover-admin"
		KEYCLOAK_RECOVERY_COMMAND="sudo $PREFIX/bin/ovirt-backup-recover-keycloak"
	else
		RECOVERY_COMMAND="sudo sh $HERE/recover-admin.sh --mode docker --compose-dir $WORK"
		KEYCLOAK_RECOVERY_COMMAND="sudo sh $HERE/recover-keycloak.sh --compose-dir $WORK"
	fi

	step "token-файл Prometheus"
	ensure_docker_metrics_token
	migration_restore_docker_data
	prepare_docker_runtime_secrets "$WORK"
	install_tls_docker "$WORK/.env"
	prepare_keycloak_runtime
	apply_data_dir_overrides "$WORK"
	[ -n "$(env_file_value "$WORK/.env" JHV_FILE_BACKUP_DIR)" ] ||
		set_plain_env JHV_FILE_BACKUP_DIR "$FILE_SOURCES" "$WORK/.env"
	[ -n "$(env_file_value "$WORK/.env" JHV_FILE_RESTORE_DIR)" ] ||
		set_plain_env JHV_FILE_RESTORE_DIR "$FILE_RESTORES" "$WORK/.env"
	[ -n "$(env_file_value "$WORK/.env" JHV_KEYCLOAK_TRUSTSTORE_DIR)" ] ||
		set_plain_env JHV_KEYCLOAK_TRUSTSTORE_DIR "$KEYCLOAK_TRUSTSTORES" "$WORK/.env"
	[ -n "$(env_file_value "$WORK/.env" JHV_KEYCLOAK_VAULT_DIR)" ] ||
		set_plain_env JHV_KEYCLOAK_VAULT_DIR "$KEYCLOAK_VAULT" "$WORK/.env"
	prepare_docker_data_paths "$WORK"
	prepare_docker_file_backup_source "$WORK"
	prepare_docker_keycloak_truststore "$WORK"
	prepare_docker_keycloak_vault "$WORK"
	prepare_docker_dr_backup "$WORK"
	migration_restore_docker_database "$WORK" "$RUN"
	ensure_docker_database_roles "$WORK" "$RUN"

    if [ "$START" -eq 0 ]; then
        # shellcheck disable=SC2086
        (cd "$WORK" && $RUN stop postgres) >/dev/null 2>&1 || true
        say ""
        say "Подготовлено без запуска. Для безопасного первого старта повторите"
        say "эту установку без --no-start: установщик удалит bootstrap-файл и"
        say "пересоздаст первый контейнер. Не запускайте compose вручную до этого."
        return
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
        if ! keycloak_wait; then
            say ""
            # shellcheck disable=SC2086
            (cd "$WORK" && $RUN logs --tail 40 keycloak 2>/dev/null) || true
            die "Keycloak не ответил за три минуты по адресу $KEYCLOAK_API_URL.
Последние строки его журнала выше.

Если журнал выглядит нормально, а контейнер работает (docker ps), проверьте
проброс порта и firewall на этой машине:
  curl -sS -o /dev/null -w '%{http_code}' $KEYCLOAK_API_URL/realms/master
  ss -ltnp | grep $KEYCLOAK_PORT"
        fi
        keycloak_bootstrap
        keycloak_realm_exists || die "Keycloak ответил, но realm $KEYCLOAK_REALM после настройки недоступен"
        clear_keycloak_bootstrap_secret
        say "    realm $KEYCLOAK_REALM, клиент $OIDC_CLIENT_ID и группы созданы"
    fi

    if [ "$OIDC_MODE" != none ]; then
        step "проверка входа через провайдера"
        if ! oidc_app_check; then
            say ""
            # shellcheck disable=SC2086
            (cd "$WORK" && $RUN logs --tail 40 "$COMPOSE_SERVICE" 2>/dev/null) || true
            die "приложение не смогло прочитать настройки OIDC-провайдера.
Проверьте issuer и внутренний адрес:
  JHV_OIDC_ISSUER=$(env_file_value "$WORK/.env" JHV_OIDC_ISSUER)
  JHV_OIDC_BACKCHANNEL_URL=$(env_file_value "$WORK/.env" JHV_OIDC_BACKCHANNEL_URL)
Последние строки журнала приложения выведены выше."
        fi
        say "    discovery доступен из контейнера приложения"
    fi

    # Пароль администратора: если .env создавали мы, он известен точно. Если
    # .env был раньше — учётная запись уже существует, и показывать нечего.
    # После успешного старта bootstrap уже либо создал первую локальную
    # учётную запись, либо увидел существующих пользователей. В обоих случаях
    # одноразовый секрет больше не нужен, в том числе после миграции или
    # продолжения прерванной установки.
    BOOTSTRAP_RESTART_REQUIRED=0
    [ "$DOCKER_ENV_EXISTED" -eq 0 ] && BOOTSTRAP_RESTART_REQUIRED=1
    [ -n "$(env_file_value "$WORK/.env" JHV_ADMIN_PASSWORD_FILE)" ] && BOOTSTRAP_RESTART_REQUIRED=1
    docker_volume_remove "$(docker_metrics_volume)" bootstrap-admin.password
    set_plain_env JHV_ADMIN_PASSWORD_FILE "" "$WORK/.env"
	if [ "$BOOTSTRAP_RESTART_REQUIRED" -eq 1 ]; then
		step "удаление bootstrap-секрета из памяти"
		# Перезапуск старого контейнера сохранил бы прежнее environment. Нужен
		# именно новый контейнер, в котором пути к одноразовому файлу уже нет.
		# shellcheck disable=SC2086
		(cd "$WORK" && $RUN up -d --no-deps --force-recreate "$COMPOSE_SERVICE") ||
			die "не удалось пересоздать приложение без bootstrap-секрета"
		wait_ready "$READY_SCHEME://127.0.0.1:$PORT/readyz" ||
			die "приложение не стало готово после удаления bootstrap-секрета"
	fi
	start_keycloak_host_helper

    say ""
    say "════════════════════════════════════════════════════════════"
    say "  ГОТОВО"
    say ""
    say "  интерфейс:     $URL"
    if [ "$OIDC_MODE" != none ] && [ "$OIDC_ALLOW_LOCAL_LOGIN" = false ]; then
        say "  локальный вход: выключен"
        say "  local-admin создан с неизвестным случайным паролем."
        say "  Для аварийного доступа сначала включите --local-login enabled,"
        say "  затем выполните с хоста: $RECOVERY_COMMAND"
    elif [ -n "${ADMPASS:-}" ]; then
        say "  пользователь:  $LOCAL_ADMIN_USER"
        say "  пароль:        $ADMPASS"
        say ""
        say "  Запишите пароль — больше он нигде не хранится."
    else
        say "  учётная запись уже была создана прежде; пароль не менялся."
        say "  Забыли — задайте новый:"
        say "    $RECOVERY_COMMAND --user $LOCAL_ADMIN_USER"
    fi
    if [ "$OIDC_MODE" = keycloak ]; then
        say ""
        say "  Вход в oVirt Backup через Keycloak:"
        say "    realm:        $KEYCLOAK_REALM"
        if [ "$KEYCLOAK_APP_ADMIN_USER" = none ]; then
            say "    первый пользователь не создавался (--keycloak-app-admin-user none)"
        else
            say "    пользователь: $KEYCLOAK_APP_ADMIN_USER"
            say "    группа:       $GROUP_ADMIN"
            if [ "$KEYCLOAK_APP_ADMIN_CREATED" -eq 1 ]; then
                say "    пароль:       $KEYCLOAK_APP_ADMIN_PASSWORD"
                say "    Запишите пароль — он больше нигде не хранится."
            else
                say "    пароль существующего пользователя не менялся."
                say "    Забыли — откройте Admin Console → realm $KEYCLOAK_REALM → Users →"
                say "    $KEYCLOAK_APP_ADMIN_USER → Credentials → Reset password."
            fi
        fi
        say "    доверенные CA: $KEYCLOAK_TRUSTSTORES"
        if [ -n "$KEYCLOAK_AD_VAULT_TARGET" ]; then
            say "    LDAP bind secret: $KEYCLOAK_AD_VAULT_TARGET (вне БД и .env)"
        fi
        if [ "$KEYCLOAK_AD_REQUESTED" -eq 1 ]; then
            say "    Active Directory: provider $KEYCLOAK_AD_PROVIDER, группы из AD ($KEYCLOAK_AD_GROUP_MODE_API)"
            say "    роли: $GROUP_ADMIN → admin, $GROUP_OPERATOR → operator, $GROUP_VIEWER → viewer"
            if [ "$KEYCLOAK_AD_PASSWORD_WAS_INTERACTIVE" -eq 1 ]; then
                say "    введённый bind-пароль сохранён только в Keycloak vault"
            else
                say "    исходный $KEYCLOAK_AD_BIND_PASSWORD_FILE теперь можно безопасно удалить"
            fi
        fi
        say "    после добавления CA: cd $WORK && $RUN restart keycloak"
        say ""
        say "  Администрирование Keycloak (не вход в oVirt Backup):"
        say "    консоль:       $KEYCLOAK_URL/admin/master/console/"
        say "    realm:         master"
        say "    администратор: $KEYCLOAK_ADMIN_USER"
        if [ -n "$KEYCLOAK_ADMIN_PASSWORD" ]; then
            say "    пароль:        $KEYCLOAK_ADMIN_PASSWORD"
            say "    Запишите пароль — из файла он тоже стёрт."
        else
            say "    пароль не менялся и повторно не выводится."
            say "    Забыли — безопасно задайте новый с хоста:"
            say "      $KEYCLOAK_RECOVERY_COMMAND --user $KEYCLOAK_ADMIN_USER"
        fi
    elif [ "$OIDC_MODE" = external ]; then
        say ""
        say "  внешний вход:  $OIDC_ISSUER"
    fi
    say "════════════════════════════════════════════════════════════"
    say ""
    if [ "$OIDC_MODE" != none ]; then
        say "Внешний вход настроен. Проверьте пользователей и группы допуска:"
        say "  • пользователи должны входить в одну из групп —"
        say "    $GROUP_ADMIN, $GROUP_OPERATOR, $GROUP_VIEWER;"
        say "  • кто не попал ни в одну, в систему не допускается: так и задумано;"
        if [ "$OIDC_MODE" = keycloak ]; then
            say "  • домены подключаются в Keycloak: realm $KEYCLOAK_REALM → User Federation;"
            say "  • второй фактор — там же: Authentication → Required Actions → Configure OTP"
            say "    (FreeOTP и совместимые);"
        fi
        say "  • соответствие групп ролям правится в $WORK/$CONFIG_NAME без пересборки:"
        say "      cd $WORK && $RUN up -d"
        if [ "$OIDC_ALLOW_LOCAL_LOGIN" = true ]; then
            say "  • локальный вход разрешён как аварийный: он обходит провайдера и MFA;"
            say "    после проверки Keycloak выключите его повторной установкой с"
            say "    --local-login disabled."
        else
            say "  • локальный вход выключен; это не влияет на CLI-сброс пароля из ОС."
        fi
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
    say "  • Файловые бэкапы: источник $FILE_SOURCES (только чтение),"
    say "    восстановление: $FILE_RESTORES"
    say "  • Скопируйте ключ шифрования отдельно от базы и не туда, где копии:"
    say "      cd $WORK && $RUN cp $COMPOSE_SERVICE:/app/data/secret.key ./secret.key.backup"
    say "  • Чек-лист перед боем: docs/DEPLOY.md"
    say "  • Забыли пароль: $RECOVERY_COMMAND --user $LOCAL_ADMIN_USER"
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

    # sslmode=disable здесь правильно, а не «пока так»: host не задан, значит
    # соединение идёт через Unix socket и сеть не задействована вовсе.
    # Шифровать нечего. Служба это распознаёт и не возражает — возражает она
    # на disable до базы, до которой надо идти по сети.
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
    if [ -s "$MIGRATION_TMP/data/database.url" ]; then
        cp "$MIGRATION_TMP/data/database.url" "$PREFIX/config/database.url"
        set_env JHV_DATABASE_URL "" "$PREFIX/config/jhvirt.env"
        set_env JHV_DATABASE_URL_FILE "$PREFIX/config/database.url" "$PREFIX/config/jhvirt.env"
    fi
    if [ -s "$MIGRATION_TMP/data/oidc-client.secret" ]; then
        cp "$MIGRATION_TMP/data/oidc-client.secret" "$PREFIX/config/oidc-client.secret"
        set_env JHV_AUTH_OIDC_CLIENT_SECRET "" "$PREFIX/config/jhvirt.env"
        set_env JHV_AUTH_OIDC_CLIENT_SECRET_FILE "$PREFIX/config/oidc-client.secret" "$PREFIX/config/jhvirt.env"
    fi
    if [ -s "$MIGRATION_TMP/data/bootstrap-admin.password" ]; then
        cp "$MIGRATION_TMP/data/bootstrap-admin.password" "$PREFIX/config/bootstrap-admin.password"
        set_env JHV_AUTH_BOOTSTRAP_PASSWORD "" "$PREFIX/config/jhvirt.env"
        set_env JHV_AUTH_BOOTSTRAP_PASSWORD_FILE "$PREFIX/config/bootstrap-admin.password" "$PREFIX/config/jhvirt.env"
    fi
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
    case " $SWP_VALUE " in
        *" $PREFIX/file-restores "*) ;;
        *) SWP_VALUE="$SWP_VALUE $PREFIX/file-restores" ;;
    esac
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
    [ -f "$HERE/systemd/jhvirt-dr-backup.service" ] || die "в комплекте нет unit резервирования"
    [ -f "$HERE/systemd/jhvirt-dr-backup.timer" ] || die "в комплекте нет timer резервирования"
    [ -f "$HERE/systemd/dr-backup.sh" ] || die "в комплекте нет скрипта резервирования"
    [ -f "$HERE/recover-admin.sh" ] || die "в комплекте нет скрипта восстановления доступа"
    have systemd-run || die "не найдена команда systemd-run"

    detect_postgres_family
    [ "$START" -eq 0 ] || ensure_http_client
    select_oidc_config
    load_systemd_oidc
    choose_oidc
    prepare_oidc

    UPGRADE=0
    # Бинарь мог остаться от прерванной первой установки; наличие unit
    # подтверждает, что это обновление завершённой установки.
    INSTALLED_BINARY=""
    if [ -x "$PREFIX/bin/$SERVER_BINARY" ]; then
        INSTALLED_BINARY="$PREFIX/bin/$SERVER_BINARY"
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
    mkdir -p "$PREFIX/bin" "$PREFIX/web" "$PREFIX/config" "$PREFIX/data" "$PREFIX/logs" \
        "$PREFIX/docs" "$PREFIX/file-sources" "$PREFIX/file-restores" "$PREFIX/proxmox"

    WAS_ACTIVE=0
    if [ "$UPGRADE" -eq 1 ] && systemctl is-active --quiet jhvirt 2>/dev/null; then
        say "    останавливаю службу на время замены"
        systemctl stop jhvirt
        WAS_ACTIVE=1
    fi

    install -m 0755 "$HERE/bin/$SERVER_BINARY" "$PREFIX/bin/"
    [ -f "$HERE/bin/jvbackup" ] && install -m 0755 "$HERE/bin/jvbackup" "$PREFIX/bin/"
    install -m 0755 "$HERE/systemd/dr-backup.sh" "$PREFIX/bin/ovirt-backup-dr-backup"
    install -m 0755 "$HERE/recover-admin.sh" "$PREFIX/bin/ovirt-backup-recover-admin"
    rm -rf "$PREFIX/web/dist"
    cp -r "$HERE/web/dist" "$PREFIX/web/dist"
    [ -d "$HERE/docs" ] && cp -r "$HERE/docs/." "$PREFIX/docs/"
    [ -f "$HERE/proxmox/jhvirt-pve-data-plane" ] && \
        install -o root -g root -m 0755 "$HERE/proxmox/jhvirt-pve-data-plane" "$PREFIX/proxmox/"
    [ -f "$HERE/VERSION" ] && cp "$HERE/VERSION" "$PREFIX/"

    # Конфигурацию не трогаем: в ней уже могут быть правки оператора.
    install_bundle_config
	migration_apply_systemd_files

    chown -R "$USER_NAME:$USER_NAME" "$PREFIX"
	if [ -f "$PREFIX/proxmox/jhvirt-pve-data-plane" ]; then
		chown root:root "$PREFIX/proxmox" "$PREFIX/proxmox/jhvirt-pve-data-plane"
		chmod 0755 "$PREFIX/proxmox" "$PREFIX/proxmox/jhvirt-pve-data-plane"
	fi
    chmod 700 "$PREFIX/data"
    chmod 750 "$PREFIX/logs"
	chmod 750 "$PREFIX/config"
	chmod 640 "$PREFIX/config/$CONFIG_NAME"
	chmod 600 "$PREFIX/config/jhvirt.env" 2>/dev/null || true
	chmod 600 "$PREFIX/data/secret.key" "$PREFIX/config/database.url" \
        "$PREFIX/config/oidc-client.secret" "$PREFIX/config/bootstrap-admin.password" 2>/dev/null || true

    ENV_FILE="$PREFIX/config/jhvirt.env"
    ENV_EXISTED=0
    [ -f "$ENV_FILE" ] && ENV_EXISTED=1

    DATABASE_URL=""
    if [ -n "$DATABASE_URL_FILE" ]; then
        step "подключение внешней PostgreSQL"
        read_external_database_url
        install -o "$USER_NAME" -g "$USER_NAME" -m 0600 "$DATABASE_URL_FILE" "$PREFIX/config/database.url"
        set_env JHV_DATABASE_URL "" "$ENV_FILE"
        set_env JHV_DATABASE_URL_FILE "$PREFIX/config/database.url" "$ENV_FILE"
    elif [ "$MIGRATION_ACTIVE" -eq 1 ] && [ "$MIGRATION_DATABASE_KIND" = embedded ]; then
        if [ "$MIGRATION_RESUME" -eq 0 ] && have runuser && id postgres >/dev/null 2>&1 &&
                runuser -u postgres -- psql -d jhvirt -Atc \
                    "select to_regclass('public.users') is not null" 2>/dev/null | grep -q t; then
            die "локальная база jhvirt на новом сервере уже содержит данные; импорт остановлен"
        fi
        prepare_local_postgres
        set_env JHV_DATABASE_URL "$DATABASE_URL" "$ENV_FILE"
        set_env JHV_DATABASE_URL_FILE "" "$ENV_FILE"
    elif [ "$ENV_EXISTED" -eq 0 ]; then
        prepare_local_postgres
        set_env JHV_DATABASE_URL "$DATABASE_URL" "$ENV_FILE"
        set_env JHV_DATABASE_URL_FILE "" "$ENV_FILE"
    else
        EXISTING_DATABASE_URL_FILE="$(env_file_value "$ENV_FILE" JHV_DATABASE_URL_FILE)"
        if [ -n "$EXISTING_DATABASE_URL_FILE" ]; then
            [ -s "$EXISTING_DATABASE_URL_FILE" ] ||
                die "не найден сохранённый файл DSN: $EXISTING_DATABASE_URL_FILE"
            say "    подключение к базе сохранено в $EXISTING_DATABASE_URL_FILE"
        else
            EXISTING_DATABASE_URL="$(env_file_value "$ENV_FILE" JHV_DATABASE_URL)"
            case "$EXISTING_DATABASE_URL" in
                *host=*|*://*)
                    umask 077
                    printf '%s\n' "$EXISTING_DATABASE_URL" > "$PREFIX/config/database.url"
                    umask 022
                    chown "$USER_NAME:$USER_NAME" "$PREFIX/config/database.url"
                    chmod 600 "$PREFIX/config/database.url"
                    set_env JHV_DATABASE_URL "" "$ENV_FILE"
                    set_env JHV_DATABASE_URL_FILE "$PREFIX/config/database.url" "$ENV_FILE"
                    say "    DSN внешней базы перенесён из env в защищённый database.url"
                    ;;
                *) say "    локальное подключение к базе сохранено из $ENV_FILE" ;;
            esac
        fi
    fi

	set_env JHV_SERVER_EXTERNAL_URL "$URL" "$ENV_FILE"
	set_env JHV_SERVER_PORT "$PORT" "$ENV_FILE"
	set_env JHV_SERVER_ADDR "$BIND_ADDRESS" "$ENV_FILE"
	write_systemd_oidc
	RECOVERY_TOKEN_FILE="$PREFIX/config/recovery.token"
	step "host-only recovery-токен"
	ensure_recovery_token "$RECOVERY_TOKEN_FILE"
	set_env JHV_AUTH_RECOVERY_TOKEN_HASH "$RECOVERY_TOKEN_HASH" "$ENV_FILE"
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
	SYSTEMD_DR_LOCAL=1
	case "$(env_file_value "$ENV_FILE" JHV_DATABASE_URL)" in *host=*|*://*) SYSTEMD_DR_LOCAL=0 ;; esac
	[ -z "$(env_file_value "$ENV_FILE" JHV_DATABASE_URL_FILE)" ] || SYSTEMD_DR_LOCAL=0
	if [ "$SYSTEMD_DR_LOCAL" -eq 1 ]; then
		DR_BACKUP_DIR="$DR_BACKUP_DIR_OVERRIDE"
		[ -n "$DR_BACKUP_DIR" ] || DR_BACKUP_DIR="$(env_file_value "$ENV_FILE" JHV_DR_BACKUP_DIR)"
		[ -n "$DR_BACKUP_DIR" ] || DR_BACKUP_DIR="/var/backups/ovirt-backup"
		case "$DR_BACKUP_DIR" in /*) ;; *) die "--dr-backup-dir для systemd должен быть абсолютным путём" ;; esac
		mkdir -p "$DR_BACKUP_DIR/postgres"
		chown -R "$USER_NAME:$USER_NAME" "$DR_BACKUP_DIR"
		chmod 700 "$DR_BACKUP_DIR" "$DR_BACKUP_DIR/postgres"
		set_env JHV_DR_BACKUP_DIR "$DR_BACKUP_DIR" "$ENV_FILE"
		set_env JHV_DISASTER_RECOVERY_ENABLED true "$ENV_FILE"
		set_env JHV_DISASTER_RECOVERY_POSTGRES_DUMP_PATH "$DR_BACKUP_DIR/postgres" "$ENV_FILE"
		set_env JHV_DISASTER_RECOVERY_POSTGRES_DUMP_MAX_AGE 36h "$ENV_FILE"
		set_env JHV_DISASTER_RECOVERY_SECRET_KEY_BACKUP_PATH "$DR_BACKUP_DIR/secret.key" "$ENV_FILE"
	else
		set_env JHV_DISASTER_RECOVERY_ENABLED false "$ENV_FILE"
		say "    внешняя PostgreSQL: её резервирование остаётся на стороне СУБД"
	fi
	install_tls_systemd "$ENV_FILE"
	migration_restore_systemd_database
	if [ "$ENV_EXISTED" -eq 1 ]; then
		LOCAL_ADMIN_USER="$(env_file_value "$ENV_FILE" JHV_AUTH_BOOTSTRAP_USER)"
		[ -n "$LOCAL_ADMIN_USER" ] || LOCAL_ADMIN_USER=admin
	else
		set_env JHV_AUTH_BOOTSTRAP_USER "$LOCAL_ADMIN_USER" "$ENV_FILE"
	fi

    ADMPASS=""
    BOOTSTRAP_PASSWORD_FILE="$PREFIX/config/bootstrap-admin.password"
    if [ "$ENV_EXISTED" -eq 0 ] && [ -z "$DATABASE_URL_FILE" ] && local_database_needs_admin; then
        ADMPASS="$(gen_secret 18)"
        [ -n "$ADMPASS" ] || die "не удалось сгенерировать пароль администратора"
        umask 077
        printf '%s\n' "$ADMPASS" > "$BOOTSTRAP_PASSWORD_FILE"
        umask 022
        chown "$USER_NAME:$USER_NAME" "$BOOTSTRAP_PASSWORD_FILE"
        chmod 600 "$BOOTSTRAP_PASSWORD_FILE"
        set_env JHV_AUTH_BOOTSTRAP_PASSWORD "" "$ENV_FILE"
        set_env JHV_AUTH_BOOTSTRAP_PASSWORD_FILE "$BOOTSTRAP_PASSWORD_FILE" "$ENV_FILE"
    elif [ "$ENV_EXISTED" -eq 1 ]; then
        LEGACY_BOOTSTRAP_PASSWORD="$(env_file_value "$ENV_FILE" JHV_AUTH_BOOTSTRAP_PASSWORD)"
        if [ -n "$LEGACY_BOOTSTRAP_PASSWORD" ]; then
            umask 077
            printf '%s\n' "$LEGACY_BOOTSTRAP_PASSWORD" > "$BOOTSTRAP_PASSWORD_FILE"
            umask 022
            chown "$USER_NAME:$USER_NAME" "$BOOTSTRAP_PASSWORD_FILE"
            chmod 600 "$BOOTSTRAP_PASSWORD_FILE"
            set_env JHV_AUTH_BOOTSTRAP_PASSWORD "" "$ENV_FILE"
            set_env JHV_AUTH_BOOTSTRAP_PASSWORD_FILE "$BOOTSTRAP_PASSWORD_FILE" "$ENV_FILE"
        fi
    else
        # У внешней БД установщик не знает, пуста ли таблица users. Пароль
        # генерирует и печатает сама служба только если действительно создаёт
        # первую запись; показывать здесь пароль, который мог не примениться,
        # опаснее, чем отправить оператора в journalctl.
        set_env JHV_AUTH_BOOTSTRAP_PASSWORD "" "$ENV_FILE"
        set_env JHV_AUTH_BOOTSTRAP_PASSWORD_FILE "" "$ENV_FILE"
    fi

    SYSTEMD_WRITE_PATHS="$(systemd_write_paths)"
	prepare_systemd_write_paths "$SYSTEMD_WRITE_PATHS"
    sed -e "s|@PREFIX@|$PREFIX|g" -e "s|@USER_NAME@|$USER_NAME|g" \
        -e "s|@READ_WRITE_PATHS@|$SYSTEMD_WRITE_PATHS|g" \
        "$HERE/systemd/jhvirt.service" > "$UNIT.tmp"
    install -m 0644 "$UNIT.tmp" "$UNIT"
    rm -f "$UNIT.tmp"
	if [ "$SYSTEMD_DR_LOCAL" -eq 1 ]; then
		sed -e "s|@PREFIX@|$PREFIX|g" -e "s|@USER_NAME@|$USER_NAME|g" \
			-e "s|@DR_DIR@|$DR_BACKUP_DIR|g" \
			"$HERE/systemd/jhvirt-dr-backup.service" > "$DR_UNIT.tmp"
		install -m 0644 "$DR_UNIT.tmp" "$DR_UNIT"
		rm -f "$DR_UNIT.tmp"
		install -m 0644 "$HERE/systemd/jhvirt-dr-backup.timer" "$DR_TIMER"
	else
		rm -f "$DR_UNIT" "$DR_TIMER"
	fi
    systemctl daemon-reload

    step "проверка конфигурации"
    check_installed_config || die "установленная конфигурация не прошла проверку"

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
        if [ -s "$BOOTSTRAP_PASSWORD_FILE" ]; then
			BOOTSTRAP_RESTART_REQUIRED=1
            rm -f "$BOOTSTRAP_PASSWORD_FILE"
            set_env JHV_AUTH_BOOTSTRAP_PASSWORD_FILE "" "$ENV_FILE"
		else
			BOOTSTRAP_RESTART_REQUIRED=0
			[ "$ENV_EXISTED" -eq 0 ] && BOOTSTRAP_RESTART_REQUIRED=1
        fi
		if [ "$BOOTSTRAP_RESTART_REQUIRED" -eq 1 ]; then
			step "удаление bootstrap-секрета из памяти"
			systemctl restart jhvirt
			wait_ready "$READY_SCHEME://127.0.0.1:$PORT/readyz" ||
				die "служба не стала готова после удаления bootstrap-секрета"
		fi
		if [ "$SYSTEMD_DR_LOCAL" -eq 1 ]; then
			systemctl enable --now jhvirt-dr-backup.timer
			systemctl start jhvirt-dr-backup.service ||
				die "служба запущена, но первый dump PostgreSQL не создан"
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
        say "  пользователь:   $LOCAL_ADMIN_USER"
        say "  пароль:         $ADMPASS"
        say ""
        say "  Запишите пароль — после первого успешного старта его файл удаляется."
    elif [ "$ENV_EXISTED" -eq 0 ] && [ -n "$DATABASE_URL_FILE" ]; then
        say "  внешняя БД: если таблица users была пуста, пароль $LOCAL_ADMIN_USER"
        say "  один раз напечатан службой; смотрите journalctl -u jhvirt."
    else
        say "  учётные записи сохранены; пароль администратора не менялся."
    fi
    say "════════════════════════════════════════════════════════════"
    say ""
    if [ "$SHOULD_START" -eq 0 ]; then
        if [ "$UPGRADE" -eq 1 ]; then
            say "Служба до обновления была остановлена и осталась остановленной."
        else
            say "Для безопасного первого старта повторите установку без --no-start."
            say "Не запускайте jhvirt.service вручную: установщик должен удалить"
            say "bootstrap-файл и заменить первый процесс после создания учётной записи."
        fi
    fi
    say "Журнал: journalctl -u jhvirt -f"
    say "Сброс доступа: sudo $PREFIX/bin/ovirt-backup-recover-admin --user $LOCAL_ADMIN_USER"
    say "Файловые бэкапы: источник $PREFIX/file-sources, восстановление $PREFIX/file-restores."
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
