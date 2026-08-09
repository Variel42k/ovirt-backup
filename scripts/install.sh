#!/bin/sh
# Установка justhpc-virt-manager из собранного комплекта.
#
# Три способа, между которыми установщик предлагает выбрать: контейнеры через
# docker или podman и служба systemd с бинарём. Показываются только доступные
# на этой машине — предлагать docker там, где его нет, значит тратить время
# оператора на выяснение, почему выбор не работает.
#
# Скрипт не решает за человека то, что решает человек: не выпускает TLS, не
# подставляет внешний адрес молча и не запускает службу, не спросив. Пароль
# базы для контейнеров генерирует сам — это внутренний секрет, который человек
# всё равно придумал бы хуже.
#
# Повторный запуск обновляет установку на месте: бинари и интерфейс
# заменяются, конфигурация и данные остаются.
#
# Использование:
#   sudo ./install.sh                     выбор способа диалогом
#   sudo ./install.sh --mode systemd      без диалога: бинарь и systemd
#   sudo ./install.sh --mode docker       без диалога: docker compose
#   sudo ./install.sh --mode podman       без диалога: podman-compose
#   sudo PREFIX=/srv/jhvirt ./install.sh  другой каталог
#   sudo ./install.sh --uninstall         удалить, сохранив данные

set -eu

PREFIX="${PREFIX:-/opt/jhvirt}"
USER_NAME="${USER_NAME:-jhvirt}"
UNIT="/etc/systemd/system/jhvirt.service"
SRC="$(cd "$(dirname "$0")" && pwd)"
MODE=""

die() { printf 'ошибка: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --mode) MODE="${2:-}"; shift 2 ;;
        --mode=*) MODE="${1#--mode=}"; shift ;;
        --uninstall) MODE="uninstall"; shift ;;
        -h|--help) sed -n '2,24p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) die "неизвестный ключ: $1 (см. --help)" ;;
    esac
done

[ "$(id -u)" -eq 0 ] || die "нужны права root: sudo $0"

# --- Что доступно на этой машине --------------------------------------------
#
# Проверяется не наличие команды, а способность выполнить то, ради чего она
# нужна: docker на RHEL часто оказывается podman в режиме эмуляции, и compose
# у него может отсутствовать вовсе.

has_docker_compose() { have docker && docker compose version >/dev/null 2>&1; }
has_podman_compose() { have podman && have podman-compose; }
has_systemd()        { have systemctl && [ -d /run/systemd/system ]; }

compose_cmd() {
    case "$1" in
        docker) printf 'docker compose' ;;
        podman) printf 'podman-compose' ;;
    esac
}

# --- Удаление ---------------------------------------------------------------

if [ "$MODE" = "uninstall" ]; then
    if [ -f "$PREFIX/compose/docker-compose.yml" ]; then
        say "==> остановка контейнеров"
        if has_docker_compose; then
            (cd "$PREFIX/compose" && docker compose down 2>/dev/null) || true
        elif has_podman_compose; then
            (cd "$PREFIX/compose" && podman-compose down 2>/dev/null) || true
        fi
    fi
    if has_systemd; then
        say "==> остановка службы"
        systemctl disable --now jhvirt 2>/dev/null || true
        rm -f "$UNIT"
        systemctl daemon-reload 2>/dev/null || true
    fi

    rm -rf "$PREFIX/bin" "$PREFIX/web"
    say ""
    # Выравнивание держится на одинаковой длине $PREFIX во всех трёх строках,
    # а не на подсчёте пробелов: висячий отступ разъехался бы при другом PREFIX.
    say "Служба удалена. Намеренно оставлены:"
    say "  $PREFIX/data   — ключ шифрования секретов"
    say "  $PREFIX/config — конфигурация"
    say "  $PREFIX/logs   — журналы"
    say ""
    say "Без secret.key из data не расшифровать пароли подключений и"
    say "зашифрованные копии. Не удаляйте его, пока копии нужны."
    say ""
    say "База PostgreSQL живёт снаружи и этим скриптом не трогается."
    say "Тома контейнеров тоже: docker volume ls | grep jhvirt"
    say ""
    say "Удалить полностью: rm -rf $PREFIX && userdel $USER_NAME"
    exit 0
fi

# --- Проверки комплекта -----------------------------------------------------

[ -f "$SRC/bin/justhpc-virt-server" ] || die "в комплекте нет bin/justhpc-virt-server"
[ -f "$SRC/web/dist/index.html" ]     || die "в комплекте нет собранного интерфейса (web/dist)"

# --- Выбор способа ----------------------------------------------------------

choose_mode() {
    n=0
    d=""; p=""; y=""
    has_docker_compose && { n=$((n + 1)); d=$n; }
    has_podman_compose && { n=$((n + 1)); p=$n; }
    has_systemd        && { n=$((n + 1)); y=$n; }

    if [ "$n" -eq 0 ]; then
        die "не найдено ни одного способа установки.
Нужно что-то одно:
  docker с плагином compose  — проверка: docker compose version
  podman и podman-compose    — dnf install -y podman-compose
  systemd                    — для установки бинарём (плюс PostgreSQL)"
    fi

    # Без терминала диалог невозможен, а зависшее приглашение внутри скрипта
    # выглядит как повисшая установка. Лучше отказать и назвать ключ.
    if [ ! -t 0 ]; then
        die "установщик запущен без терминала — укажите способ явно:
  ./install.sh --mode docker|podman|systemd"
    fi

    say ""
    say "Как устанавливать?"
    say ""
    [ -n "$d" ] && say "  $d) docker compose  — сервис и PostgreSQL в контейнерах"
    [ -n "$p" ] && say "  $p) podman-compose  — то же на podman (обычный путь на RHEL)"
    [ -n "$y" ] && say "  $y) systemd         — бинарь службой, PostgreSQL отдельно"
    say ""
    say "Показаны только доступные на этой машине."
    say ""

    while :; do
        printf 'Номер [1]: '
        read -r answer || answer=""
        [ -n "$answer" ] || answer=1
        if [ -n "$d" ] && [ "$answer" = "$d" ]; then MODE=docker; return; fi
        if [ -n "$p" ] && [ "$answer" = "$p" ]; then MODE=podman; return; fi
        if [ -n "$y" ] && [ "$answer" = "$y" ]; then MODE=systemd; return; fi
        say "Нет такого варианта."
    done
}

[ -n "$MODE" ] || choose_mode

case "$MODE" in
    docker)  has_docker_compose || die "docker compose недоступен на этой машине" ;;
    podman)  has_podman_compose || die "podman-compose недоступен: dnf install -y podman-compose" ;;
    systemd) has_systemd || die "systemd не найден; запускайте бинарь своим способом, см. docs/DEPLOY.md" ;;
    *) die "неизвестный способ: $MODE (docker, podman или systemd)" ;;
esac

# --- Общее ------------------------------------------------------------------

if ! id "$USER_NAME" >/dev/null 2>&1; then
    useradd --system --home-dir "$PREFIX" --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null ||
        useradd --system --home-dir "$PREFIX" --shell /sbin/nologin "$USER_NAME"
    say "    создан системный пользователь $USER_NAME"
fi

mkdir -p "$PREFIX/config" "$PREFIX/data" "$PREFIX/logs" "$PREFIX/docs"
[ -d "$SRC/docs" ] && cp -r "$SRC/docs/." "$PREFIX/docs/"
[ -f "$SRC/VERSION" ] && cp "$SRC/VERSION" "$PREFIX/VERSION"

# --- Контейнеры -------------------------------------------------------------

install_containers() {
    CC="$(compose_cmd "$MODE")"
    UPGRADE=0
    [ -f "$PREFIX/compose/.env" ] && UPGRADE=1

    if [ "$UPGRADE" -eq 1 ]; then
        say "==> обновление установки в контейнерах ($MODE)"
    else
        say "==> установка в контейнерах ($MODE), каталог $PREFIX"
    fi

    mkdir -p "$PREFIX/compose" "$PREFIX/backups" "$PREFIX/restores"
    # Комплект копируется целиком: образ собирается из bin/ и web/dist рядом с
    # Dockerfile, поэтому они должны лежать в каталоге сборки.
    rm -rf "$PREFIX/bin" "$PREFIX/web"
    cp -r "$SRC/bin" "$SRC/web" "$PREFIX/"
    cp "$SRC/Dockerfile" "$PREFIX/Dockerfile"
    cp "$SRC/config/virt-manager.yaml" "$PREFIX/config/virt-manager.yaml"
    cp "$SRC/compose/docker-compose.yml" "$PREFIX/compose/"
    cp "$SRC/compose/.env.example" "$PREFIX/compose/"

    if [ "$UPGRADE" -eq 0 ]; then
        # Внешний адрес спрашиваем: из него выводится флаг Secure у куки
        # сессии, и молча подставленный localhost означал бы куку без Secure
        # в боевой установке за обратным прокси.
        HOSTNAME_FQDN="$(hostname -f 2>/dev/null || hostname)"
        EXTERNAL=""
        if [ -t 0 ]; then
            say ""
            say "Внешний адрес, по которому интерфейс открывают в браузере."
            say "https здесь включает флаг Secure у куки сессии — даже если TLS"
            say "терминирует обратный прокси, а сервис слушает http."
            printf 'Адрес [https://%s:8080]: ' "$HOSTNAME_FQDN"
            read -r EXTERNAL || EXTERNAL=""
        fi
        [ -n "$EXTERNAL" ] || EXTERNAL="https://$HOSTNAME_FQDN:8080"

        # Пароль базы генерируется, а не спрашивается: он внутренний, наружу
        # не публикуется и человеком был бы придуман хуже. Шестнадцатеричный —
        # чтобы годился и в форме URL, если строку потом перепишут руками.
        PGPASS="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"

        umask 077
        {
            printf 'COMPOSE_PROJECT_NAME=jhvirt\n'
            printf 'POSTGRES_USER=jhvirt\n'
            printf 'POSTGRES_PASSWORD=%s\n' "$PGPASS"
            printf 'POSTGRES_DB=jhvirt\n'
            printf 'JHV_EXTERNAL_URL=%s\n' "$EXTERNAL"
            printf 'JHV_PORT=8080\n'
            printf 'JHV_ADMIN_PASSWORD=\n'
            printf 'JHV_BACKUP_DIR=%s/backups\n' "$PREFIX"
            printf 'JHV_RESTORE_DIR=%s/restores\n' "$PREFIX"
            printf 'JHV_LOG_FILE=/app/logs/jhvirt.log\n'
            printf 'TZ=%s\n' "$(cat /etc/timezone 2>/dev/null || echo Europe/Moscow)"
        } > "$PREFIX/compose/.env"
        umask 022
        chmod 600 "$PREFIX/compose/.env"
        say "    создан $PREFIX/compose/.env, пароль базы сгенерирован"
    else
        say "    $PREFIX/compose/.env сохранён"
    fi

    chown -R "$USER_NAME:$USER_NAME" "$PREFIX"
    chmod 700 "$PREFIX/data"

    START="y"
    if [ -t 0 ]; then
        say ""
        printf 'Собрать образ и запустить сейчас? [Y/n]: '
        read -r START || START="y"
        [ -n "$START" ] || START="y"
    fi

    case "$START" in
        [Nn]*)
            say ""
            say "==> подготовлено. Запуск:"
            say "      cd $PREFIX/compose && $CC up -d --build"
            ;;
        *)
            say ""
            say "==> сборка образа и запуск (в первый раз это несколько минут)"
            (cd "$PREFIX/compose" && $CC up -d --build)
            say ""
            say "==> запущено"
            ;;
    esac

    say ""
    say "Дальше:"
    say ""
    say "  1. Пароль администратора печатается ОДИН раз:"
    say "       cd $PREFIX/compose && $CC logs justhpc-virt-manager | grep -A6 'УЧЁТНАЯ ЗАПИСЬ'"
    say "     Потеряли — задайте новый, база не пострадает:"
    say "       cd $PREFIX/compose && $CC run --rm justhpc-virt-manager -reset-password admin"
    say ""
    say "  2. Интерфейс на порту 8080. TLS не настроен — поставьте обратный"
    say "     прокси, см. $PREFIX/docs/DEPLOY.md."
    say ""
    say "  3. Скопируйте ключ шифрования отдельно от базы и не в то хранилище,"
    say "     где лежат копии. Без него копии не расшифровать:"
    say "       cd $PREFIX/compose && $CC cp justhpc-virt-manager:/app/data/secret.key ./secret.key.backup"
    say ""
    say "  4. Чек-лист перед боем: $PREFIX/docs/DEPLOY.md"
}

# --- Служба systemd ---------------------------------------------------------

install_systemd() {
    UPGRADE=0
    [ -x "$PREFIX/bin/justhpc-virt-server" ] && UPGRADE=1

    if [ "$UPGRADE" -eq 1 ]; then
        # -version печатает «justhpc-virt-server 1.0.0»; в строке обновления
        # нужна только сама версия, иначе имя программы дублируется дважды.
        OLD="$("$PREFIX/bin/justhpc-virt-server" -version 2>/dev/null | awk '{print $NF}' || true)"
        NEW="$("$SRC/bin/justhpc-virt-server" -version 2>/dev/null | awk '{print $NF}' || true)"
        [ -n "$OLD" ] || OLD='?'
        [ -n "$NEW" ] || NEW='?'
        say "==> обновление: $OLD -> $NEW"
    else
        say "==> установка службой systemd, каталог $PREFIX"
    fi

    mkdir -p "$PREFIX/bin" "$PREFIX/web"

    # Службу останавливаем перед заменой бинаря: подменить исполняемый файл под
    # работающим процессом на Linux можно, но обновление тогда применится
    # только после перезапуска, и «обновил, а версия старая» станет загадкой.
    RESTART=0
    if [ "$UPGRADE" -eq 1 ] && systemctl is-active --quiet jhvirt 2>/dev/null; then
        say "    останавливаю службу на время замены"
        systemctl stop jhvirt
        RESTART=1
    fi

    install -m 0755 "$SRC/bin/justhpc-virt-server" "$PREFIX/bin/"
    [ -f "$SRC/bin/jvbackup" ] && install -m 0755 "$SRC/bin/jvbackup" "$PREFIX/bin/"

    rm -rf "$PREFIX/web/dist"
    cp -r "$SRC/web/dist" "$PREFIX/web/dist"

    # Конфигурацию не трогаем: в ней уже могут быть правки оператора.
    if [ -f "$PREFIX/config/virt-manager.yaml" ]; then
        cp "$SRC/config/virt-manager.yaml" "$PREFIX/config/virt-manager.yaml.new"
        say "    конфигурация сохранена; новая версия рядом: virt-manager.yaml.new"
    else
        install -m 0640 "$SRC/config/virt-manager.yaml" "$PREFIX/config/"
    fi

    install -m 0644 "$SRC/systemd/jhvirt.service" "$UNIT"
    systemctl daemon-reload

    # data содержит ключ шифрования: 0700, доступ только владельцу. Всё
    # остальное читаемо, чтобы администратор мог заглянуть без sudo.
    chown -R "$USER_NAME:$USER_NAME" "$PREFIX"
    chmod 700 "$PREFIX/data"
    chmod 750 "$PREFIX/logs"
    [ -f "$PREFIX/config/jhvirt.env" ] && chmod 600 "$PREFIX/config/jhvirt.env"

    say ""
    if [ "$RESTART" -eq 1 ]; then
        systemctl start jhvirt
        say "==> служба обновлена и запущена"
        say ""
        say "Проверьте: systemctl status jhvirt"
        return
    fi
    if [ "$UPGRADE" -eq 1 ]; then
        say "==> обновлено. Служба была остановлена — запустите: systemctl start jhvirt"
        return
    fi

    say "==> установлено в $PREFIX"
    say ""
    say "Дальше — вручную, потому что это ваши решения:"
    say ""
    say "  1. База данных. Сервис работает только с PostgreSQL и без неё не"
    say "     стартует. Если её ещё нет:"
    say "       dnf install -y postgresql-server && postgresql-setup --initdb"
    say "       systemctl enable --now postgresql"
    say "       sudo -u postgres psql -c \"CREATE USER jhvirt WITH PASSWORD 'пароль';\" \\"
    say "                           -c \"CREATE DATABASE jhvirt OWNER jhvirt;\""
    say "     Схема создастся сама при первом запуске."
    say ""
    say "  2. Конфигурация:   $PREFIX/config/virt-manager.yaml"
    say "     Пароль СУБД — в $PREFIX/config/jhvirt.env, права 0600 (юнит читают все):"
    say "       JHV_DATABASE_URL=host=localhost port=5432 user=jhvirt password=пароль dbname=jhvirt sslmode=disable"
    say "       JHV_SERVER_EXTERNAL_URL=https://virt.example.org"
    say "     Форма host=… а не postgres://…: в URL пароль пришлось бы"
    say "     percent-кодировать, а openssl rand -base64 выдаёт / и +."
    say ""
    say "  3. Запуск:         systemctl enable --now jhvirt"
    say ""
    say "  4. Пароль администратора печатается ОДИН раз:"
    say "       journalctl -u jhvirt -n 50 | grep -A6 'УЧЁТНАЯ ЗАПИСЬ'"
    say "     Потеряли — не страшно:"
    say "       sudo -u $USER_NAME $PREFIX/bin/justhpc-virt-server \\"
    say "            -config $PREFIX/config/virt-manager.yaml -reset-password admin"
    say ""
    say "  5. Чек-лист перед боем: $PREFIX/docs/DEPLOY.md"
}

case "$MODE" in
    docker|podman) install_containers ;;
    systemd)       install_systemd ;;
esac
