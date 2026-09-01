#!/bin/sh
# Host-side recovery of the permanent administrator in the bundled Keycloak.
# The recovery credential exists only in a root-only temporary directory and
# in one disposable Keycloak container. It is never written to .env or a
# persistent volume and the temporary service account is removed before the
# new permanent password is printed.
set -eu

SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
SCRIPT_DIR="$(dirname "$SELF")"
case "$SCRIPT_DIR" in */bin) DEFAULT_PREFIX="$(dirname "$SCRIPT_DIR")" ;; *) DEFAULT_PREFIX="/opt/jhvirt" ;; esac

PREFIX="${PREFIX:-$DEFAULT_PREFIX}"
COMPOSE_DIR=""
ADMIN_USER=""
ASSUME_YES=0
SERVICE="keycloak"

die() { printf '\nошибка: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

usage() {
    cat <<'EOF'
Использование:
  sudo ovirt-backup-recover-keycloak [--user ИМЯ] [--yes]

Параметры:
  --prefix ПУТЬ       каталог установки, по умолчанию /opt/jhvirt
  --compose-dir ПУТЬ  каталог docker-compose.yml для установки из репозитория
  --user ИМЯ          постоянный администратор master realm; по умолчанию
                      KEYCLOAK_ADMIN_USER из .env
  --yes               не задавать вопрос подтверждения

Команда останавливает все контейнеры сервиса Keycloak, штатной offline-командой
создаёт одноразовый admin service account, сбрасывает или создаёт постоянного
администратора master realm, отзывает его сессии и удаляет временный account.
Новый пароль печатается один раз и нигде не сохраняется.
EOF
}

while [ $# -gt 0 ]; do
    case "$1" in
        --prefix) [ $# -ge 2 ] || die "--prefix требует путь"; PREFIX="$2"; shift 2 ;;
        --prefix=*) PREFIX="${1#--prefix=}"; shift ;;
        --compose-dir) [ $# -ge 2 ] || die "--compose-dir требует путь"; COMPOSE_DIR="$2"; shift 2 ;;
        --compose-dir=*) COMPOSE_DIR="${1#--compose-dir=}"; shift ;;
        --user) [ $# -ge 2 ] || die "--user требует имя"; ADMIN_USER="$2"; shift 2 ;;
        --user=*) ADMIN_USER="${1#--user=}"; shift ;;
        --yes) ASSUME_YES=1; shift ;;
        -h|--help) usage; exit 0 ;;
        *) die "неизвестный ключ: $1" ;;
    esac
done

[ "$(id -u)" -eq 0 ] || die "восстановление запускается с хоста от root: sudo $SELF"
have docker || die "docker не установлен"
have curl || die "для проверки Keycloak нужен curl"
have mktemp || die "не найдена команда mktemp"
have flock || die "для блокировки параллельного recovery нужна команда flock (util-linux)"
docker --version 2>&1 | grep -qi podman && die "podman не поддерживается"

[ -n "$COMPOSE_DIR" ] || COMPOSE_DIR="$PREFIX/compose"
ENV_FILE="$COMPOSE_DIR/.env"
[ -f "$COMPOSE_DIR/docker-compose.yml" ] || die "не найден $COMPOSE_DIR/docker-compose.yml"
[ -f "$ENV_FILE" ] || die "не найден $ENV_FILE"

env_value() {
    EV_VALUE="$(sed -n "s/^${1}=//p" "$ENV_FILE" | tail -n 1)"
    case "$EV_VALUE" in
        \"*\") EV_VALUE="${EV_VALUE#\"}"; EV_VALUE="${EV_VALUE%\"}" ;;
    esac
    printf '%s' "$EV_VALUE"
}

if docker compose version >/dev/null 2>&1; then
    compose() { (cd "$COMPOSE_DIR" && docker compose "$@"); }
elif have docker-compose; then
    compose() { (cd "$COMPOSE_DIR" && docker-compose "$@"); }
else
    die "не найден Docker Compose v1 или v2"
fi

PROFILES="$(env_value COMPOSE_PROFILES)"
case ",$PROFILES," in *,keycloak,*) ;; *) die "в этой установке профиль Keycloak не включён" ;; esac
compose config --services | grep -qx "$SERVICE" || die "в compose нет сервиса $SERVICE"

[ -n "$ADMIN_USER" ] || ADMIN_USER="$(env_value KEYCLOAK_ADMIN_USER)"
[ -n "$ADMIN_USER" ] || ADMIN_USER="kc-bootstrap-admin"
case "$ADMIN_USER" in
    *[!A-Za-z0-9._@-]*|"") die "недопустимое имя администратора: $ADMIN_USER" ;;
esac

KC_PORT="$(env_value KEYCLOAK_PORT)"; [ -n "$KC_PORT" ] || KC_PORT=8081
case "$KC_PORT" in *[!0-9]*|"") die "недопустимый KEYCLOAK_PORT: $KC_PORT" ;; esac
KC_BIND="$(env_value KEYCLOAK_BIND_ADDRESS)"; [ -n "$KC_BIND" ] || KC_BIND=127.0.0.1
case "$KC_BIND" in
    0.0.0.0) KC_API_HOST=127.0.0.1 ;;
    ::|"[::]") KC_API_HOST="[::1]" ;;
    *:*) KC_API_HOST="[$KC_BIND]" ;;
    *) KC_API_HOST="$KC_BIND" ;;
esac
KC_SCHEME=http
case "$(env_value KEYCLOAK_DIRECT_TLS)" in
    1|true|yes|on) KC_SCHEME=https ;;
esac
KC_API_URL="$KC_SCHEME://$KC_API_HOST:$KC_PORT"
KC_PUBLIC_URL="$(env_value JHV_KEYCLOAK_URL)"; [ -n "$KC_PUBLIC_URL" ] || KC_PUBLIC_URL="$KC_API_URL"

if [ "$ASSUME_YES" -eq 0 ]; then
    [ -t 0 ] || die "без терминала требуется --yes"
    say "Будет изменён пароль администратора Keycloak '$ADMIN_USER'."
    say "Realm, пользователи, federation, MFA и клиент приложения сохранятся."
    printf 'Продолжить? [y/N]: '
    read -r ANSWER
    case "$ANSWER" in y|Y|yes|YES|да|ДА) ;; *) say "Операция отменена."; exit 0 ;; esac
fi

gen_secret() {
    if have openssl; then
        openssl rand -hex "$1"
    else
        head -c "$1" /dev/urandom | od -An -tx1 | tr -d '[:space:]'
    fi
}

RUNTIME_BASE=/run
[ -d "$RUNTIME_BASE" ] && [ -w "$RUNTIME_BASE" ] || RUNTIME_BASE="${TMPDIR:-/tmp}"
umask 077
LOCK_FILE="$RUNTIME_BASE/ovirt-backup-keycloak-recovery.lock"
exec 9>"$LOCK_FILE"
chmod 0600 "$LOCK_FILE"
flock -n 9 || die "другая операция восстановления Keycloak уже выполняется"
RECOVERY_DIR="$(mktemp -d "$RUNTIME_BASE/ovirt-backup-keycloak-recovery.XXXXXX")" ||
    die "не удалось создать временный каталог"
chmod 0700 "$RECOVERY_DIR"
TEMP_CLIENT="ovirt-backup-recovery-$(gen_secret 6)"
TEMP_SECRET="$(gen_secret 24)"
FINAL_PASSWORD="$(gen_secret 18)"
[ -n "$TEMP_SECRET" ] && [ -n "$FINAL_PASSWORD" ] || die "не удалось сгенерировать пароли"
printf '%s' "$TEMP_SECRET" > "$RECOVERY_DIR/service.secret"
printf '%s' "$FINAL_PASSWORD" > "$RECOVERY_DIR/admin.password"
chmod 0600 "$RECOVERY_DIR/service.secret" "$RECOVERY_DIR/admin.password"
TEMP_SECRET=""; FINAL_PASSWORD=""

TOKEN=""
TEMP_CREATED=0
KEYCLOAK_STOPPED=0

wait_keycloak() {
    WK_TRY=0
    while [ "$WK_TRY" -lt 90 ]; do
        if curl -k -sS --fail -m 5 -o /dev/null "$KC_API_URL/realms/master" 2>/dev/null; then
            return 0
        fi
        WK_TRY=$((WK_TRY+1))
        sleep 2
    done
    return 1
}

service_token() {
    ST_BODY="$RECOVERY_DIR/token.body"
    printf 'grant_type=client_credentials&client_id=%s&client_secret=%s' \
        "$TEMP_CLIENT" "$(cat "$RECOVERY_DIR/service.secret")" > "$ST_BODY"
    ST_RESPONSE="$(curl -k -sS --fail -m 30 \
        -H 'Content-Type: application/x-www-form-urlencoded' \
        --data-binary "@$ST_BODY" \
        "$KC_API_URL/realms/master/protocol/openid-connect/token" 2>/dev/null)" || return 1
    printf '%s' "$ST_RESPONSE" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'
}

admin_user_token() {
    AUT_BODY="$RECOVERY_DIR/admin-token.body"
    printf 'grant_type=password&client_id=admin-cli&username=%s&password=%s' \
        "$ADMIN_USER" "$(cat "$RECOVERY_DIR/admin.password")" > "$AUT_BODY"
    AUT_RESPONSE="$(curl -k -sS --fail -m 30 \
        -H 'Content-Type: application/x-www-form-urlencoded' \
        --data-binary "@$AUT_BODY" \
        "$KC_API_URL/realms/master/protocol/openid-connect/token" 2>/dev/null)" || return 1
    printf '%s' "$AUT_RESPONSE" | sed -n 's/.*"access_token":"\([^"]*\)".*/\1/p'
}

write_auth_config() {
    [ -n "$TOKEN" ] || return 1
    printf 'header = "Authorization: Bearer %s"\nheader = "Content-Type: application/json"\n' \
        "$TOKEN" > "$RECOVERY_DIR/curl.conf"
    chmod 0600 "$RECOVERY_DIR/curl.conf"
}

api_get() {
    curl -k -sS --fail -m 30 --config "$RECOVERY_DIR/curl.conf" \
        "$KC_API_URL/admin/realms/master$1" 2>/dev/null
}

api_status() {
    AS_METHOD="$1"; AS_PATH="$2"; AS_BODY="${3:-}"
    if [ -n "$AS_BODY" ]; then
        curl -k -sS -m 30 -o /dev/null -w '%{http_code}' \
            --config "$RECOVERY_DIR/curl.conf" -X "$AS_METHOD" \
            --data-binary "@$AS_BODY" "$KC_API_URL/admin/realms/master$AS_PATH" 2>/dev/null
    else
        curl -k -sS -m 30 -o /dev/null -w '%{http_code}' \
            --config "$RECOVERY_DIR/curl.conf" -X "$AS_METHOD" \
            "$KC_API_URL/admin/realms/master$AS_PATH" 2>/dev/null
    fi
}

lookup_temp_client() {
    LTC_JSON="$(curl -k -sS --fail -m 30 --config "$RECOVERY_DIR/curl.conf" \
        --get --data-urlencode "clientId=$TEMP_CLIENT" \
        "$KC_API_URL/admin/realms/master/clients" 2>/dev/null)" || return 1
    printf '%s' "$LTC_JSON" | sed -n 's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1
}

remove_temp_client() {
    [ -n "$TOKEN" ] || TOKEN="$(service_token)"
    [ -n "$TOKEN" ] || return 1
    write_auth_config || return 1
    RTC_ID="$(lookup_temp_client)"
    [ -n "$RTC_ID" ] || return 1
    [ "$(api_status DELETE "/clients/$RTC_ID")" = 204 ] || return 1
    TEMP_CREATED=0
}

cleanup() {
    trap - 0 HUP INT TERM
    set +e
    if [ "$KEYCLOAK_STOPPED" -eq 1 ]; then
        compose up -d --no-deps "$SERVICE" >/dev/null 2>&1
        KEYCLOAK_STOPPED=0
    fi
    if [ "$TEMP_CREATED" -eq 1 ]; then
        wait_keycloak >/dev/null 2>&1
        if ! remove_temp_client >/dev/null 2>&1; then
            printf '\nКРИТИЧНО: не удалось удалить временный admin service account %s.\n' \
                "$TEMP_CLIENT" >&2
            printf 'После восстановления доступа удалите его в master realm → Clients.\n' >&2
        fi
    fi
    [ -z "$RECOVERY_DIR" ] || rm -rf "$RECOVERY_DIR"
}
trap cleanup 0
trap 'exit 1' HUP INT TERM

say "Останавливаю Keycloak для штатной offline recovery-команды..."
compose stop -t 30 "$SERVICE" >/dev/null
KEYCLOAK_STOPPED=1

say "Создаю одноразовый admin service account..."
(
    export KC_RECOVERY_SECRET="$(cat "$RECOVERY_DIR/service.secret")"
    compose run -T --rm --no-deps -e KC_RECOVERY_SECRET "$SERVICE" \
        --config-file=/opt/keycloak/data/ovirt-backup/keycloak.conf \
        bootstrap-admin service --optimized --client-id "$TEMP_CLIENT" \
        --client-secret:env=KC_RECOVERY_SECRET --no-prompt
) >/dev/null || die "Keycloak не создал временный recovery account"
TEMP_CREATED=1

compose up -d --no-deps "$SERVICE" >/dev/null
KEYCLOAK_STOPPED=0
wait_keycloak || die "Keycloak не стал готов за 3 минуты; проверьте docker compose logs keycloak"
TOKEN="$(service_token)"
[ -n "$TOKEN" ] || die "Keycloak не выдал токен временному recovery account"
write_auth_config || die "не удалось подготовить авторизованный запрос"

USERS_JSON="$(curl -k -sS --fail -m 30 --config "$RECOVERY_DIR/curl.conf" \
    --get --data-urlencode "username=$ADMIN_USER" --data-urlencode 'exact=true' \
    "$KC_API_URL/admin/realms/master/users" 2>/dev/null)" || die "не удалось найти администратора"
ADMIN_ID="$(printf '%s' "$USERS_JSON" | sed -n \
    's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"

if [ -z "$ADMIN_ID" ]; then
    printf '{"username":"%s","enabled":true,"emailVerified":true}' "$ADMIN_USER" \
        > "$RECOVERY_DIR/user.json"
    [ "$(api_status POST /users "$RECOVERY_DIR/user.json")" = 201 ] ||
        die "не удалось создать постоянного администратора $ADMIN_USER"
    USERS_JSON="$(curl -k -sS --fail -m 30 --config "$RECOVERY_DIR/curl.conf" \
        --get --data-urlencode "username=$ADMIN_USER" --data-urlencode 'exact=true' \
        "$KC_API_URL/admin/realms/master/users" 2>/dev/null)" || die "не удалось прочитать созданного администратора"
    ADMIN_ID="$(printf '%s' "$USERS_JSON" | sed -n \
        's/.*"id"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n 1)"
    [ -n "$ADMIN_ID" ] || die "Keycloak создал пользователя без доступного ID"
else
    printf '{"enabled":true}' > "$RECOVERY_DIR/user.json"
    [ "$(api_status PUT "/users/$ADMIN_ID" "$RECOVERY_DIR/user.json")" = 204 ] ||
        die "не удалось включить администратора $ADMIN_USER"
fi

printf '{"type":"password","value":"%s","temporary":false}' \
    "$(cat "$RECOVERY_DIR/admin.password")" > "$RECOVERY_DIR/password.json"
[ "$(api_status PUT "/users/$ADMIN_ID/reset-password" "$RECOVERY_DIR/password.json")" = 204 ] ||
    die "не удалось сменить пароль администратора $ADMIN_USER"

api_get /roles/admin > "$RECOVERY_DIR/admin-role.json" || die "не найдена роль admin master realm"
printf '[' > "$RECOVERY_DIR/role-map.json"
cat "$RECOVERY_DIR/admin-role.json" >> "$RECOVERY_DIR/role-map.json"
printf ']' >> "$RECOVERY_DIR/role-map.json"
[ "$(api_status POST "/users/$ADMIN_ID/role-mappings/realm" "$RECOVERY_DIR/role-map.json")" = 204 ] ||
    die "не удалось назначить роль admin пользователю $ADMIN_USER"

# Парольный reset не должен оставлять уже выданные административные сессии.
[ "$(api_status POST "/users/$ADMIN_ID/logout")" = 204 ] ||
    die "пароль изменён, но не удалось завершить прежние сессии $ADMIN_USER"
api_status DELETE "/attack-detection/brute-force/users/$ADMIN_ID" >/dev/null 2>&1 || true

PERMANENT_TOKEN="$(admin_user_token)"
[ -n "$PERMANENT_TOKEN" ] ||
    die "пароль изменён, но постоянный администратор $ADMIN_USER не смог войти"
printf 'header = "Authorization: Bearer %s"\n' "$PERMANENT_TOKEN" \
    > "$RECOVERY_DIR/permanent-curl.conf"
chmod 0600 "$RECOVERY_DIR/permanent-curl.conf"
PERMANENT_ADMIN_CODE="$(curl -k -sS -m 30 -o /dev/null -w '%{http_code}' \
    --config "$RECOVERY_DIR/permanent-curl.conf" \
    "$KC_API_URL/admin/realms/master/users" 2>/dev/null)"
[ "$PERMANENT_ADMIN_CODE" = 200 ] ||
    die "пароль принят, но у $ADMIN_USER нет административного доступа к master realm"
PERMANENT_TOKEN=""

remove_temp_client || die "не удалось удалить временный admin service account $TEMP_CLIENT"
FINAL_PASSWORD="$(cat "$RECOVERY_DIR/admin.password")"
rm -rf "$RECOVERY_DIR"
RECOVERY_DIR=""
TOKEN=""
trap - 0 HUP INT TERM

have logger && logger -t ovirt-backup "Keycloak administrator $ADMIN_USER recovered from host; sessions revoked" || true
say ""
say "Доступ к Keycloak восстановлен."
say "  адрес:         $KC_PUBLIC_URL/admin/master/console/"
say "  администратор: $ADMIN_USER"
say "  новый пароль:  $FINAL_PASSWORD"
say ""
say "Пароль напечатан один раз и нигде не сохранён. Временный recovery account удалён."
say "После входа настройте MFA и сохраните пароль в принятом менеджере секретов."
FINAL_PASSWORD=""
