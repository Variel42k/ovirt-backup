#!/bin/sh
# Установка justhpc-virt-manager. Один скрипт на всё.
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
#   ./install.sh --uninstall           остановить и снять, данные оставить

set -eu

# Абсолютный путь до смены каталога: после cd относительный $0 не разрешается.
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
HERE="$(dirname "$SELF")"

PREFIX="${PREFIX:-/opt/jhvirt}"
USER_NAME="${USER_NAME:-jhvirt}"
UNIT="/etc/systemd/system/jhvirt.service"

MODE=""; URL=""; DATABASE_URL_FILE=""; START=1; PORT=8080

die() { printf '\nошибка: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --mode) [ $# -ge 2 ] || die "--mode требует значение"; MODE="$2"; shift 2 ;;
        --mode=*) MODE="${1#--mode=}"; shift ;;
        --url) [ $# -ge 2 ] || die "--url требует значение"; URL="$2"; shift 2 ;;
        --url=*) URL="${1#--url=}"; shift ;;
        --port) [ $# -ge 2 ] || die "--port требует значение"; PORT="$2"; shift 2 ;;
        --port=*) PORT="${1#--port=}"; shift ;;
        --database-url-file) [ $# -ge 2 ] || die "--database-url-file требует путь"; DATABASE_URL_FILE="$2"; shift 2 ;;
        --database-url-file=*) DATABASE_URL_FILE="${1#--database-url-file=}"; shift ;;
        --no-start) START=0; shift ;;
        --uninstall) MODE=uninstall; shift ;;
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
    if [ -f "$COMPOSE_DIR/.env" ]; then
        v="$(grep -m1 '^COMPOSE_PROJECT_NAME=' "$COMPOSE_DIR/.env" 2>/dev/null | cut -d= -f2-)"
        [ -n "$v" ] && { printf '%s' "$v"; return; }
    fi
    printf 'jhvirt'
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

set_plain_env() {
    KEY="$1"; VALUE="$2"; FILE="$3"; TMP="$FILE.tmp.$$"
    grep -v "^${KEY}=" "$FILE" > "$TMP" || true
    printf '%s=%s\n' "$KEY" "$VALUE" >> "$TMP"
    chmod 600 "$TMP"
    mv "$TMP" "$FILE"
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
    esac
}

validate_install_identity

# --- Удаление ---------------------------------------------------------------

uninstall() {
    [ "$(id -u)" -eq 0 ] || die "для удаления нужны права root: sudo $SELF --uninstall"
    for dir in "$PREFIX/compose" "$COMPOSE_DIR"; do
        [ -f "$dir/.env" ] || continue
        step "остановка контейнеров"
        for cmd in "docker compose" docker-compose; do
            # shellcheck disable=SC2086
            (cd "$dir" && $cmd down >/dev/null 2>&1) && break || true
        done
    done
    if has_systemd; then
        step "остановка службы"
        systemctl disable --now jhvirt >/dev/null 2>&1 || true
        rm -f "$UNIT"
        systemctl daemon-reload >/dev/null 2>&1 || true
    fi
    rm -rf "${PREFIX:?}/bin" "${PREFIX:?}/web" "${PREFIX:?}/docs"
    rm -f "$PREFIX/VERSION"
    say ""
    say "Снято. Данные намеренно оставлены:"
    if [ -d "$PREFIX" ]; then
        say "  $PREFIX/data   — ключ шифрования секретов"
        say "  $PREFIX/config — конфигурация"
    fi
    say "  тома контейнеров — ключ, база и копии внутри них"
    say ""
    say "Посмотреть тома:  docker volume ls | grep jhvirt"
    say "Без secret.key копии не расшифровать — снесите тома только тогда,"
    say "когда копии больше не нужны."
    [ -d "$PREFIX" ] && say "Удалить установку целиком: rm -rf $PREFIX && userdel $USER_NAME"
    exit 0
}

# --- Выбор ------------------------------------------------------------------

choose() {
    i=0; a=""; b=""; d=""; u=""
    has_docker   && { i=$((i+1)); a=$i; }
    has_dockerc  && { i=$((i+1)); b=$i; }
    has_systemd  && { i=$((i+1)); d=$i; }
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
    say "  $u) удалить          — снять приложение, сохранив конфигурацию и данные"
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
        if [ "$n" = "$u" ]; then
            printf 'Снять приложение, сохранив конфигурацию, ключи и базу? [y/N]: '
            read -r answer || answer=""
            case "$answer" in
                y|Y|yes|YES|да|Да|ДА) MODE=uninstall; return ;;
                *) say "Удаление отменено."; continue ;;
            esac
        fi
        say "Нет такого варианта."
    done
}

[ -n "$MODE" ] || choose
[ "$MODE" != uninstall ] || uninstall

# Права root нужны там, где скрипт трогает систему: раскладывает комплект в
# /opt, заводит пользователя, ставит юнит. Запуск контейнеров из каталога
# репозитория обходится без них, и требовать sudo там значит заставлять
# работать под root без причины.
if [ "$BUNDLE" -eq 1 ] || [ "$MODE" = systemd ]; then
    [ "$(id -u)" -eq 0 ] || die "нужны права root: sudo $SELF"
fi

# Проверяется здесь, а не при разборе ключей: ошибку в номере порта compose
# сообщает уже на запуске контейнера, когда образ собран и время потрачено.
case "$PORT" in
    ''|*[!0-9]*) die "порт должен быть числом: --port 18080" ;;
esac
[ "$PORT" -ge 1 ] 2>/dev/null && [ "$PORT" -le 65535 ] 2>/dev/null ||
    die "порт должен быть в диапазоне 1..65535: --port 18080"

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

ask_url

http_ok() {
    if have curl; then
        curl -fsS --max-time 5 "$1" >/dev/null 2>&1
    elif have wget; then
        wget -qO- -T 5 "$1" >/dev/null 2>&1
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

# --- Контейнеры -------------------------------------------------------------

install_containers() {
    RUN="$(runner "$MODE")"

    if [ "$START" -eq 1 ] && ! have curl && ! have wget; then
        die "для проверки готовности нужен curl или wget"
    fi

    if [ "$BUNDLE" -eq 1 ]; then
        step "раскладка в $PREFIX"
        id "$USER_NAME" >/dev/null 2>&1 || \
            useradd --system --home-dir "$PREFIX" --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null || \
            useradd --system --home-dir "$PREFIX" --shell /sbin/nologin "$USER_NAME"
        mkdir -p "$PREFIX/compose" "$PREFIX/config" "$PREFIX/data" "$PREFIX/logs" \
                 "$PREFIX/docs" "$PREFIX/backups" "$PREFIX/restores"
        rm -rf "${PREFIX:?}/bin" "${PREFIX:?}/web"
        # Образ собирается из bin/ и web/dist рядом с Dockerfile, поэтому весь
        # комплект копируется целиком.
        cp -r "$HERE/bin" "$HERE/web" "$PREFIX/"
        cp "$HERE/Dockerfile" "$PREFIX/Dockerfile"
        cp "$HERE/config/virt-manager.yaml" "$PREFIX/config/"
        cp "$HERE/compose/docker-compose.yml" "$HERE/compose/.env.example" "$PREFIX/compose/"
        [ -d "$HERE/docs" ] && cp -r "$HERE/docs/." "$PREFIX/docs/"
        [ -f "$HERE/VERSION" ] && cp "$HERE/VERSION" "$PREFIX/"
        WORK="$PREFIX/compose"
        BACKUPS="$PREFIX/backups"; RESTORES="$PREFIX/restores"
    else
        WORK="$COMPOSE_DIR"
        mkdir -p "$WORK/backups" "$WORK/restores"
        BACKUPS="./backups"; RESTORES="./restores"
    fi

    if [ -f "$WORK/.env" ]; then
        say "    $WORK/.env уже есть; пароль базы и пользовательские настройки сохранены"
        set_plain_env JHV_EXTERNAL_URL "$URL" "$WORK/.env"
        set_plain_env JHV_PORT "$PORT" "$WORK/.env"
    else
        # Пароль базы задаётся один раз — при создании тома. Если том с прошлой
        # установки уцелел, а .env исчез, сгенерированный пароль базе не
        # подойдёт: она примет только тот, с которым была создана. Служба тогда
        # уходит в цикл перезапуска с «password authentication failed», и связь
        # с пропавшим .env совсем не очевидна.
        VOL="$(project_name)_postgres-data"
        VOLRM="docker volume rm"
        if volume_exists "$VOL"; then
            die "том базы $VOL остался с прошлой установки, а $WORK/.env — нет.

PostgreSQL хранит пароль внутри тома и новый не примет: служба будет
перезапускаться с «password authentication failed».

Одно из двух:
  • верните прежний .env — в нём пароль, который база ждёт;
  • либо удалите том вместе с данными и поставьте заново:
      $VOLRM $VOL

Во втором случае теряются подключения, задания и история. Сами копии лежат в
хранилище и не пострадают, но сервис о них забудет."
        fi

        # Пароль базы генерируется: внутренний секрет, человеком был бы придуман
        # хуже. Шестнадцатеричный — годится и в форме URL, где / и + пришлось бы
        # кодировать.
        PGPASS="$(gen_secret 24)"
        [ -n "$PGPASS" ] || die "не удалось сгенерировать пароль базы"

        # Пароль администратора задаём сами, а не вылавливаем потом из журнала:
        # формат вывода у docker compose и docker-compose разный, поэтому пароль
        # задаётся до старта, а не извлекается из журнала.
        #
        # Из .env он стирается сразу после запуска: учётная запись уже создана,
        # и держать пароль в файле дольше незачем.
        ADMPASS="$(gen_secret 18)"
        [ -n "$ADMPASS" ] || die "не удалось сгенерировать пароль администратора"

        umask 077
        {
            printf 'COMPOSE_PROJECT_NAME=jhvirt\n'
            printf 'POSTGRES_USER=jhvirt\n'
            printf 'POSTGRES_PASSWORD=%s\n' "$PGPASS"
            printf 'POSTGRES_DB=jhvirt\n'
            printf 'JHV_EXTERNAL_URL=%s\n' "$URL"
            printf 'JHV_PORT=%s\n' "$PORT"
            printf 'JHV_ADMIN_PASSWORD=%s\n' "$ADMPASS"
            printf 'JHV_BACKUP_DIR=%s\n' "$BACKUPS"
            printf 'JHV_RESTORE_DIR=%s\n' "$RESTORES"
            printf 'JHV_LOG_FILE=/app/logs/jhvirt.log\n'
            printf 'TZ=%s\n' "$(cat /etc/timezone 2>/dev/null || echo Europe/Moscow)"
        } > "$WORK/.env"
        umask 022
        chmod 600 "$WORK/.env"
        say "    создан $WORK/.env, пароль базы сгенерирован"
    fi

    [ "$BUNDLE" -eq 1 ] && { chown -R "$USER_NAME:$USER_NAME" "$PREFIX"; chmod 700 "$PREFIX/data"; }

    if [ "$START" -eq 0 ]; then
        say ""
        say "Подготовлено. Запуск:"
        say "  cd $WORK && $RUN up -d --build"
        return
    fi

    step "сборка образа и запуск (в первый раз это несколько минут)"
    # shellcheck disable=SC2086
    (cd "$WORK" && $RUN up -d --build) || die "запуск не удался; смотрите вывод выше"

    step "жду готовности"
    if ! wait_ready "http://127.0.0.1:$PORT/readyz"; then
        say ""
        # shellcheck disable=SC2086
        (cd "$WORK" && $RUN logs --tail 30 justhpc-virt-manager 2>/dev/null) || true
        die "за 3 минуты сервис не стал готов — последние строки журнала выше"
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
        say "    cd $WORK && $RUN run --rm justhpc-virt-manager -reset-password admin"
    fi
    say "════════════════════════════════════════════════════════════"
    say ""
    say "Дальше:"
    say "  • TLS не настроен — при необходимости поставьте обратный прокси перед портом $PORT."
    say "  • Скопируйте ключ шифрования отдельно от базы и не туда, где копии:"
    say "      cd $WORK && $RUN cp justhpc-virt-manager:/app/data/secret.key ./secret.key.backup"
    say "  • Чек-лист перед боем: docs/DEPLOY.md"
    say "  • Забыли пароль: cd $WORK && $RUN run --rm justhpc-virt-manager -reset-password admin"
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
        "$PREFIX/bin/justhpc-virt-server" \
        -config "$PREFIX/config/virt-manager.yaml" -check-config
}

install_systemd() {
    [ "$BUNDLE" -eq 1 ] || die "установка службой возможна только из комплекта .run
Из репозитория соберите его: ./run build --target linux/amd64"

    [ -f "$HERE/bin/justhpc-virt-server" ] || die "в комплекте нет bin/justhpc-virt-server"
    [ -f "$HERE/web/dist/index.html" ] || die "в комплекте нет web/dist/index.html"
    [ -f "$HERE/config/virt-manager.yaml" ] || die "в комплекте нет конфигурации"
    [ -f "$HERE/systemd/jhvirt.service" ] || die "в комплекте нет unit systemd"
    have systemd-run || die "не найдена команда systemd-run"

    detect_postgres_family
    [ "$START" -eq 0 ] || ensure_http_client

    UPGRADE=0
    # Бинарь мог остаться от прерванной первой установки; наличие unit
    # подтверждает, что это обновление завершённой установки.
    [ -x "$PREFIX/bin/justhpc-virt-server" ] && [ -f "$UNIT" ] && UPGRADE=1

    if [ "$UPGRADE" -eq 1 ]; then
        # -version печатает «justhpc-virt-server 1.0.0»; нужна только версия.
        OLD="$("$PREFIX/bin/justhpc-virt-server" -version 2>/dev/null | awk '{print $NF}' || true)"
        NEW="$("$HERE/bin/justhpc-virt-server" -version 2>/dev/null | awk '{print $NF}' || true)"
        step "обновление: ${OLD:-?} -> ${NEW:-?}"
    else
        step "установка службой systemd в $PREFIX"
    fi

    id "$USER_NAME" >/dev/null 2>&1 || \
        useradd --system --home-dir "$PREFIX" --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null || \
        useradd --system --home-dir "$PREFIX" --shell /sbin/nologin "$USER_NAME"
    mkdir -p "$PREFIX/bin" "$PREFIX/web" "$PREFIX/config" "$PREFIX/data" "$PREFIX/logs" "$PREFIX/docs"

    WAS_ACTIVE=0
    if [ "$UPGRADE" -eq 1 ] && systemctl is-active --quiet jhvirt 2>/dev/null; then
        say "    останавливаю службу на время замены"
        systemctl stop jhvirt
        WAS_ACTIVE=1
    fi

    install -m 0755 "$HERE/bin/justhpc-virt-server" "$PREFIX/bin/"
    [ -f "$HERE/bin/jvbackup" ] && install -m 0755 "$HERE/bin/jvbackup" "$PREFIX/bin/"
    rm -rf "$PREFIX/web/dist"
    cp -r "$HERE/web/dist" "$PREFIX/web/dist"
    [ -d "$HERE/docs" ] && cp -r "$HERE/docs/." "$PREFIX/docs/"
    [ -f "$HERE/VERSION" ] && cp "$HERE/VERSION" "$PREFIX/"

    # Конфигурацию не трогаем: в ней уже могут быть правки оператора.
    if [ -f "$PREFIX/config/virt-manager.yaml" ]; then
        cp "$HERE/config/virt-manager.yaml" "$PREFIX/config/virt-manager.yaml.new"
        say "    конфигурация сохранена; новая версия рядом: virt-manager.yaml.new"
    else
        install -m 0640 "$HERE/config/virt-manager.yaml" "$PREFIX/config/"
    fi

    chown -R "$USER_NAME:$USER_NAME" "$PREFIX"
    chmod 700 "$PREFIX/data"
    chmod 750 "$PREFIX/logs"

    ENV_FILE="$PREFIX/config/jhvirt.env"
    ENV_EXISTED=0
    [ -f "$ENV_FILE" ] && ENV_EXISTED=1

    DATABASE_URL=""
    if [ -n "$DATABASE_URL_FILE" ]; then
        step "подключение внешней PostgreSQL"
        read_external_database_url
        set_env JHV_DATABASE_URL "$DATABASE_URL" "$ENV_FILE"
    elif [ "$ENV_EXISTED" -eq 0 ]; then
        prepare_local_postgres
        set_env JHV_DATABASE_URL "$DATABASE_URL" "$ENV_FILE"
    else
        say "    подключение к базе сохранено из $ENV_FILE"
    fi

    set_env JHV_SERVER_EXTERNAL_URL "$URL" "$ENV_FILE"
    set_env JHV_SERVER_PORT "$PORT" "$ENV_FILE"

    ADMPASS=""
    if [ "$START" -eq 1 ] && [ "$ENV_EXISTED" -eq 0 ] &&
            [ -z "$DATABASE_URL_FILE" ] && local_database_needs_admin; then
        ADMPASS="$(gen_secret 18)"
        [ -n "$ADMPASS" ] || die "не удалось сгенерировать пароль администратора"
        set_env JHV_AUTH_BOOTSTRAP_PASSWORD "$ADMPASS" "$ENV_FILE"
    elif [ "$ENV_EXISTED" -eq 0 ]; then
        set_env JHV_AUTH_BOOTSTRAP_PASSWORD "" "$ENV_FILE"
    fi

    sed -e "s|@PREFIX@|$PREFIX|g" -e "s|@USER_NAME@|$USER_NAME|g" \
        "$HERE/systemd/jhvirt.service" > "$UNIT.tmp"
    install -m 0644 "$UNIT.tmp" "$UNIT"
    rm -f "$UNIT.tmp"
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
        if ! wait_ready "http://127.0.0.1:$PORT/readyz"; then
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
}

case "$MODE" in
    docker|docker-compose) install_containers ;;
    systemd)               install_systemd ;;
esac
