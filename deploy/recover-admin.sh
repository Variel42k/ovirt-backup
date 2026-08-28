#!/bin/sh
# Host-side recovery of the local administrator after a lost password or an
# application-container compromise. The normal service never receives the raw
# recovery token.
set -eu

SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
SCRIPT_DIR="$(dirname "$SELF")"
case "$SCRIPT_DIR" in */bin) DEFAULT_PREFIX="$(dirname "$SCRIPT_DIR")" ;; *) DEFAULT_PREFIX="/opt/jhvirt" ;; esac

PREFIX="${PREFIX:-$DEFAULT_PREFIX}"
MODE=""
ADMIN_USER="local-admin"
COMPOSE_DIR=""
TOKEN_FILE=""
SERVICE="ovirt-backup"

die() { printf '\nошибка: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

usage() {
    cat <<'EOF'
Использование:
  sudo ovirt-backup-recover-admin [--mode docker|systemd] [--user ИМЯ]

Параметры:
  --prefix ПУТЬ       каталог установки, по умолчанию /opt/jhvirt
  --compose-dir ПУТЬ  каталог docker-compose.yml для установки из репозитория
  --token-file ПУТЬ   нестандартный host-only recovery.token

Скрипт останавливает рабочее приложение, запускает доверенный одноразовый
процесс с recovery-токеном, генерирует новый пароль, отзывает все сессии,
API-токены из PostgreSQL и делегирования, затем пересоздаёт/запускает службу.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --mode) [ $# -ge 2 ] || die "--mode требует значение"; MODE="$2"; shift 2 ;;
        --mode=*) MODE="${1#--mode=}"; shift ;;
        --user) [ $# -ge 2 ] || die "--user требует имя"; ADMIN_USER="$2"; shift 2 ;;
        --user=*) ADMIN_USER="${1#--user=}"; shift ;;
        --prefix) [ $# -ge 2 ] || die "--prefix требует путь"; PREFIX="$2"; shift 2 ;;
        --prefix=*) PREFIX="${1#--prefix=}"; shift ;;
        --compose-dir) [ $# -ge 2 ] || die "--compose-dir требует путь"; COMPOSE_DIR="$2"; shift 2 ;;
        --compose-dir=*) COMPOSE_DIR="${1#--compose-dir=}"; shift ;;
        --token-file) [ $# -ge 2 ] || die "--token-file требует путь"; TOKEN_FILE="$2"; shift 2 ;;
        --token-file=*) TOKEN_FILE="${1#--token-file=}"; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "неизвестный ключ: $1" ;;
    esac
done

HOST_KERNEL="$(uname -s 2>/dev/null || true)"
case "$HOST_KERNEL" in
    MINGW*|MSYS*|CYGWIN*) export MSYS_NO_PATHCONV=1 ;;
esac
if [ "$(id -u)" -ne 0 ]; then
    # У Docker Desktop на Windows нет Unix-root и владельца root:root. Доступ
    # к daemon и ACL token-файла остаются границей; systemd здесь недоступен.
    case "$HOST_KERNEL" in
        MINGW*|MSYS*|CYGWIN*) ;;
        *) die "восстановление запускается с хоста от root: sudo $SELF" ;;
    esac
fi
[ -n "$ADMIN_USER" ] || die "имя пользователя пусто"

[ -n "$COMPOSE_DIR" ] || COMPOSE_DIR="$PREFIX/compose"
SYSTEMD_PRESENT=0; DOCKER_PRESENT=0
have systemctl && systemctl cat jhvirt.service >/dev/null 2>&1 && SYSTEMD_PRESENT=1
[ -f "$COMPOSE_DIR/docker-compose.yml" ] && DOCKER_PRESENT=1

if [ -z "$MODE" ]; then
    SYSTEMD_ACTIVE=0; DOCKER_ACTIVE=0
    [ "$SYSTEMD_PRESENT" -eq 1 ] && systemctl is-active --quiet jhvirt.service 2>/dev/null && SYSTEMD_ACTIVE=1
    if [ "$DOCKER_PRESENT" -eq 1 ] && have docker; then
        CID="$(cd "$COMPOSE_DIR" && docker compose ps -q "$SERVICE" 2>/dev/null || true)"
        [ -n "$CID" ] && DOCKER_ACTIVE=1
    fi
    if [ "$SYSTEMD_ACTIVE" -eq 1 ] && [ "$DOCKER_ACTIVE" -eq 0 ]; then
        MODE=systemd
    elif [ "$DOCKER_ACTIVE" -eq 1 ] && [ "$SYSTEMD_ACTIVE" -eq 0 ]; then
        MODE=docker
    elif [ "$SYSTEMD_PRESENT" -eq 1 ] && [ "$DOCKER_PRESENT" -eq 0 ]; then
        MODE=systemd
    elif [ "$DOCKER_PRESENT" -eq 1 ] && [ "$SYSTEMD_PRESENT" -eq 0 ]; then
        MODE=docker
    else
        die "не удалось однозначно выбрать установку; укажите --mode docker или --mode systemd"
    fi
fi
case "$MODE" in docker|systemd) ;; *) die "--mode: допустимы docker и systemd" ;; esac

if [ -z "$TOKEN_FILE" ]; then
    if [ -s "$PREFIX/config/recovery.token" ]; then
        TOKEN_FILE="$PREFIX/config/recovery.token"
    else
        TOKEN_FILE="$COMPOSE_DIR/.recovery-token"
    fi
fi
[ -f "$TOKEN_FILE" ] || die "не найден recovery-токен $TOKEN_FILE; повторно запустите доверенный установщик"
[ -s "$TOKEN_FILE" ] || die "recovery-токен $TOKEN_FILE пуст"
case "$HOST_KERNEL" in MINGW*|MSYS*|CYGWIN*) CHECK_UNIX_MODE=0 ;; *) CHECK_UNIX_MODE=1 ;; esac
if [ "$CHECK_UNIX_MODE" -eq 1 ] && have stat; then
    MODE_BITS="$(stat -c '%a' "$TOKEN_FILE" 2>/dev/null || true)"
    case "$MODE_BITS" in *00) ;; *) die "$TOKEN_FILE доступен группе или остальным; требуются права 0600" ;; esac
fi

wait_docker() {
    i=0
    while [ "$i" -lt 180 ]; do
        CID="$(cd "$COMPOSE_DIR" && compose ps -q "$SERVICE" 2>/dev/null || true)"
        if [ -n "$CID" ]; then
            STATUS="$(docker inspect --format '{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}' "$CID" 2>/dev/null || true)"
            case "$STATUS" in healthy|running) return 0 ;; esac
        fi
        i=$((i+1)); sleep 1
    done
    return 1
}

if [ "$MODE" = docker ]; then
    [ "$DOCKER_PRESENT" -eq 1 ] || die "не найден $COMPOSE_DIR/docker-compose.yml"
    have docker || die "docker не установлен"
    docker --version 2>&1 | grep -qi podman && die "podman не поддерживается"
    if docker compose version >/dev/null 2>&1; then
        compose() { docker compose "$@"; }
    elif have docker-compose; then
        compose() { docker-compose "$@"; }
    else
        die "не найден Docker Compose v1 или v2"
    fi

    say "Останавливаю рабочий контейнер приложения..."
    (cd "$COMPOSE_DIR" && compose stop -t 30 "$SERVICE")
    STOPPED=1
    restart_docker() {
        if [ "${STOPPED:-0}" -eq 1 ]; then
            (cd "$COMPOSE_DIR" && compose up -d --no-deps --force-recreate "$SERVICE") >/dev/null 2>&1 || true
        fi
    }
    trap restart_docker 0 HUP INT TERM

    say "Сбрасываю пароль и отзываю действующие доступы..."
    cat "$TOKEN_FILE" | (cd "$COMPOSE_DIR" && compose run -T --rm --no-deps \
        "$SERVICE" -config /app/config/ovirt-backup.yaml \
        -reset-password "$ADMIN_USER" \
        -recovery-token-file - \
        -revoke-all-access)

    say "Пересоздаю приложение из установленного образа..."
    (cd "$COMPOSE_DIR" && compose up -d --no-deps --force-recreate "$SERVICE")
    STOPPED=0
    trap - 0 HUP INT TERM
    wait_docker || die "контейнер не стал готов за 3 минуты; проверьте docker compose logs $SERVICE"
else
    [ "$SYSTEMD_PRESENT" -eq 1 ] || die "jhvirt.service не установлен"
    have systemd-run || die "не найдена команда systemd-run"
    BINARY="$PREFIX/bin/ovirt-backup-server"
    CONFIG="$PREFIX/config/ovirt-backup.yaml"
    ENV_FILE="$PREFIX/config/jhvirt.env"
    [ -x "$BINARY" ] || die "не найден $BINARY"
    [ -f "$ENV_FILE" ] || die "не найден $ENV_FILE"
    SERVICE_USER="$(systemctl show jhvirt.service -p User --value)"
    SERVICE_GROUP="$(systemctl show jhvirt.service -p Group --value)"
    [ -n "$SERVICE_USER" ] || die "в jhvirt.service не задан User"
    [ -n "$SERVICE_GROUP" ] || SERVICE_GROUP="$SERVICE_USER"

    say "Останавливаю jhvirt.service..."
    systemctl stop jhvirt.service
    STOPPED=1
    restart_systemd() {
        if [ "${STOPPED:-0}" -eq 1 ]; then
            systemctl start jhvirt.service >/dev/null 2>&1 || true
        fi
    }
    trap restart_systemd 0 HUP INT TERM

    say "Сбрасываю пароль и отзываю действующие доступы..."
    cat "$TOKEN_FILE" | systemd-run --quiet --wait --pipe --collect \
        --unit="ovirt-backup-recovery-$$" \
        --property="User=$SERVICE_USER" --property="Group=$SERVICE_GROUP" \
        --property="WorkingDirectory=$PREFIX" --property="EnvironmentFile=$ENV_FILE" \
        "$BINARY" -config "$CONFIG" -reset-password "$ADMIN_USER" \
        -recovery-token-file - -revoke-all-access

    systemctl start jhvirt.service
    STOPPED=0
    trap - 0 HUP INT TERM
    i=0
    while [ "$i" -lt 180 ]; do
        systemctl is-active --quiet jhvirt.service && break
        i=$((i+1)); sleep 1
    done
    systemctl is-active --quiet jhvirt.service || die "jhvirt.service не запустилась; смотрите journalctl -u jhvirt"
fi

say ""
say "Доступ восстановлен. Новый пароль напечатан выше и нигде не сохранён."
say "Все сессии и выдаваемые из БД токены отозваны. Проверьте пользователей и аудит."
say "Если вход через OIDC включён, отдельно завершите сессии у провайдера и смените его client secret."
