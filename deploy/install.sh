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
#   ./install.sh --mode podman         podman-compose
#   ./install.sh --mode systemd        бинарь службой, PostgreSQL отдельно
#   ./install.sh --url https://host    внешний адрес без вопроса
#   ./install.sh --no-start            подготовить, но не запускать
#   ./install.sh --uninstall           остановить и снять, данные оставить

set -eu

# Абсолютный путь до смены каталога: после cd относительный $0 не разрешается.
SELF="$(cd "$(dirname "$0")" && pwd)/$(basename "$0")"
HERE="$(dirname "$SELF")"

PREFIX="${PREFIX:-/opt/jhvirt}"
USER_NAME="${USER_NAME:-jhvirt}"
UNIT="/etc/systemd/system/jhvirt.service"

MODE=""; URL=""; START=1

die() { printf '\nошибка: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }
step() { printf '\n==> %s\n' "$*"; }
have() { command -v "$1" >/dev/null 2>&1; }

while [ $# -gt 0 ]; do
    case "$1" in
        --mode) MODE="${2:-}"; shift 2 ;;
        --mode=*) MODE="${1#--mode=}"; shift ;;
        --url) URL="${2:-}"; shift 2 ;;
        --url=*) URL="${1#--url=}"; shift ;;
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
# Проверяется способность выполнить, а не наличие команды: docker на RHEL часто
# оказывается podman в режиме эмуляции, и compose у него может отсутствовать.

has_docker()    { have docker && docker compose version >/dev/null 2>&1; }
has_dockerc()   { have docker-compose; }
has_podmanc()   { have podman-compose; }
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

# Работа с томами идёт тем же движком, что и запуск: тома docker и podman —
# разные хранилища, и спрашивать не у того значит не найти существующий.
volume_engine() {
    case "$MODE" in
        podman) printf 'podman' ;;
        *)      printf 'docker' ;;
    esac
}

volume_exists() {
    "$(volume_engine)" volume inspect "$1" >/dev/null 2>&1
}

# Команда запуска для выбранного способа.
runner() {
    case "$1" in
        docker)         printf 'docker compose' ;;
        docker-compose) printf 'docker-compose' ;;
        podman)         printf 'podman-compose' ;;
    esac
}

# --- Удаление ---------------------------------------------------------------

if [ "$MODE" = uninstall ]; then
    for dir in "$PREFIX/compose" "$COMPOSE_DIR"; do
        [ -f "$dir/.env" ] || continue
        step "остановка контейнеров"
        for cmd in "docker compose" docker-compose podman-compose; do
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
fi

# --- Выбор ------------------------------------------------------------------

choose() {
    i=0; a=""; b=""; c=""; d=""
    has_docker   && { i=$((i+1)); a=$i; }
    has_dockerc  && { i=$((i+1)); b=$i; }
    has_podmanc  && { i=$((i+1)); c=$i; }
    has_systemd  && { i=$((i+1)); d=$i; }

    [ "$i" -gt 0 ] || die "нечем запускать. Поставьте одно из:
  dnf install -y podman-compose        (обычный путь на RHEL)
  плагин docker compose                (docker compose version)
  docker-compose                       (старый, через дефис)
  либо ставьте службой systemd — тогда нужен systemd и PostgreSQL"

    if [ ! -t 0 ]; then
        die "нет терминала — укажите способ ключом:
  ./install.sh --mode docker|docker-compose|podman|systemd"
    fi

    say ""
    say "Чем запускать?"
    say ""
    [ -n "$a" ] && say "  $a) docker compose   — сервис и PostgreSQL в контейнерах"
    [ -n "$b" ] && say "  $b) docker-compose   — то же, старой командой через дефис"
    [ -n "$c" ] && say "  $c) podman-compose   — то же на podman"
    [ -n "$d" ] && say "  $d) systemd          — бинарь службой, PostgreSQL отдельно"
    say ""
    say "Показано только то, что есть на этой машине."
    say ""
    while :; do
        printf 'Номер [1]: '
        read -r n || n=""
        [ -n "$n" ] || n=1
        [ -n "$a" ] && [ "$n" = "$a" ] && { MODE=docker; return; }
        [ -n "$b" ] && [ "$n" = "$b" ] && { MODE=docker-compose; return; }
        [ -n "$c" ] && [ "$n" = "$c" ] && { MODE=podman; return; }
        [ -n "$d" ] && [ "$n" = "$d" ] && { MODE=systemd; return; }
        say "Нет такого варианта."
    done
}

[ -n "$MODE" ] || choose

# Права root нужны там, где скрипт трогает систему: раскладывает комплект в
# /opt, заводит пользователя, ставит юнит. Запуск контейнеров из каталога
# репозитория обходится без них, и требовать sudo там значит заставлять
# работать под root без причины.
if [ "$BUNDLE" -eq 1 ] || [ "$MODE" = systemd ]; then
    [ "$(id -u)" -eq 0 ] || die "нужны права root: sudo $SELF"
fi

case "$MODE" in
    docker)         has_docker  || die "docker compose недоступен" ;;
    docker-compose) has_dockerc || die "docker-compose не найден" ;;
    podman)         has_podmanc || die "podman-compose не найден: dnf install -y podman-compose" ;;
    systemd)        has_systemd || die "systemd не найден" ;;
    *) die "неизвестный способ: $MODE" ;;
esac

# --- Внешний адрес ----------------------------------------------------------
#
# Спрашивается, а не подставляется молча: из него выводится флаг Secure у куки
# сессии, и localhost по умолчанию означал бы куку без Secure за прокси.

ask_url() {
    [ -n "$URL" ] && return
    guess="https://$(hostname -f 2>/dev/null || hostname):8080"
    if [ -t 0 ]; then
        say ""
        say "Адрес, по которому интерфейс открывают в браузере."
        say "https включает флаг Secure у куки сессии — даже если TLS"
        say "терминирует обратный прокси, а сервис слушает http."
        printf 'Адрес [%s]: ' "$guess"
        read -r URL || URL=""
    fi
    [ -n "$URL" ] || URL="$guess"
}

# --- Контейнеры -------------------------------------------------------------

install_containers() {
    RUN="$(runner "$MODE")"

    if [ "$BUNDLE" -eq 1 ]; then
        step "раскладка в $PREFIX"
        id "$USER_NAME" >/dev/null 2>&1 || \
            useradd --system --home-dir "$PREFIX" --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null || \
            useradd --system --home-dir "$PREFIX" --shell /sbin/nologin "$USER_NAME"
        mkdir -p "$PREFIX/compose" "$PREFIX/config" "$PREFIX/data" "$PREFIX/logs" \
                 "$PREFIX/docs" "$PREFIX/backups" "$PREFIX/restores"
        rm -rf "$PREFIX/bin" "$PREFIX/web"
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
        say "    $WORK/.env уже есть, оставлен как был"
    else
        # Пароль базы задаётся один раз — при создании тома. Если том с прошлой
        # установки уцелел, а .env исчез, сгенерированный пароль базе не
        # подойдёт: она примет только тот, с которым была создана. Служба тогда
        # уходит в цикл перезапуска с «password authentication failed», и связь
        # с пропавшим .env совсем не очевидна.
        VOL="$(project_name)_postgres-data"
        VOLRM="$(volume_engine) volume rm"
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

        ask_url
        # Пароль базы генерируется: внутренний секрет, человеком был бы придуман
        # хуже. Шестнадцатеричный — годится и в форме URL, где / и + пришлось бы
        # кодировать.
        if have openssl; then
            PGPASS="$(openssl rand -hex 24)"
        else
            PGPASS="$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')"
        fi
        [ -n "$PGPASS" ] || die "не удалось сгенерировать пароль базы"

        umask 077
        {
            printf 'COMPOSE_PROJECT_NAME=jhvirt\n'
            printf 'POSTGRES_USER=jhvirt\n'
            printf 'POSTGRES_PASSWORD=%s\n' "$PGPASS"
            printf 'POSTGRES_DB=jhvirt\n'
            printf 'JHV_EXTERNAL_URL=%s\n' "$URL"
            printf 'JHV_PORT=8080\n'
            printf 'JHV_ADMIN_PASSWORD=\n'
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

    # Ждём именно строку готовности, а не факт запуска процесса: между «служба
    # стартовала» и «интерфейс отвечает» проходят миграции и создание учётной
    # записи. Ожидание по первой строке уже приводило к «ГОТОВО» с недоступным
    # интерфейсом и ненайденным паролем.
    step "жду готовности"
    READY=0
    i=0
    while [ "$i" -lt 90 ]; do
        if (cd "$WORK" && $RUN logs justhpc-virt-manager 2>/dev/null |
                grep -q "веб-интерфейс и API доступны"); then
            READY=1
            break
        fi
        # Если контейнер успел упасть, ждать дальше бессмысленно.
        if (cd "$WORK" && $RUN logs justhpc-virt-manager 2>/dev/null |
                grep -q "критическая ошибка"); then
            say ""
            (cd "$WORK" && $RUN logs justhpc-virt-manager 2>/dev/null | tail -5)
            die "служба не поднялась — причина выше"
        fi
        i=$((i+1)); sleep 2
    done
    [ "$READY" -eq 1 ] || say "    за 3 минуты строка готовности не появилась — смотрите журнал"

    # Пароль администратора печатается службой один раз. Достать его из журнала
    # здесь же — иначе оператору пришлось бы вспоминать команду grep, а второго
    # шанса увидеть пароль не будет.
    PW="$(cd "$WORK" && $RUN logs justhpc-virt-manager 2>/dev/null |
          grep -m1 -E 'пароль:' | sed 's/.*пароль:[[:space:]]*//' | tr -d '\r ')"

    say ""
    say "════════════════════════════════════════════════════════════"
    say "  ГОТОВО"
    say ""
    say "  интерфейс:     $URL"
    if [ -n "$PW" ]; then
        say "  пользователь:  admin"
        say "  пароль:        $PW"
        say ""
        say "  Пароль показан один раз — запишите его."
    else
        say "  пароль администратора:"
        say "    cd $WORK && $RUN logs justhpc-virt-manager | grep -A6 'УЧЁТНАЯ ЗАПИСЬ'"
    fi
    say "════════════════════════════════════════════════════════════"
    say ""
    say "Дальше:"
    say "  • TLS не настроен — поставьте обратный прокси перед портом 8080."
    say "  • Скопируйте ключ шифрования отдельно от базы и не туда, где копии:"
    say "      cd $WORK && $RUN cp justhpc-virt-manager:/app/data/secret.key ./secret.key.backup"
    say "  • Чек-лист перед боем: docs/DEPLOY.md"
    say "  • Забыли пароль: cd $WORK && $RUN run --rm justhpc-virt-manager -reset-password admin"
}

# --- Служба systemd ---------------------------------------------------------

install_systemd() {
    [ "$BUNDLE" -eq 1 ] || die "установка службой возможна только из комплекта .run
Из репозитория соберите его: ./run build --target linux/amd64"

    [ -f "$HERE/bin/justhpc-virt-server" ] || die "в комплекте нет bin/justhpc-virt-server"

    UPGRADE=0
    [ -x "$PREFIX/bin/justhpc-virt-server" ] && UPGRADE=1

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

    # Службу останавливаем перед заменой бинаря: подменить файл под работающим
    # процессом Linux позволяет, но применилось бы это только после перезапуска.
    RESTART=0
    if [ "$UPGRADE" -eq 1 ] && systemctl is-active --quiet jhvirt 2>/dev/null; then
        say "    останавливаю службу на время замены"
        systemctl stop jhvirt
        RESTART=1
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

    install -m 0644 "$HERE/systemd/jhvirt.service" "$UNIT"
    systemctl daemon-reload

    chown -R "$USER_NAME:$USER_NAME" "$PREFIX"
    chmod 700 "$PREFIX/data"
    chmod 750 "$PREFIX/logs"

    if [ "$RESTART" -eq 1 ]; then
        systemctl start jhvirt
        say ""
        say "==> обновлено и запущено. Проверьте: systemctl status jhvirt"
        return
    fi
    if [ "$UPGRADE" -eq 1 ]; then
        say ""
        say "==> обновлено. Запустите: systemctl start jhvirt"
        return
    fi

    ask_url
    say ""
    say "════════════════════════════════════════════════════════════"
    say "  УСТАНОВЛЕНО в $PREFIX"
    say ""
    say "  Осталось три шага — служба без базы не стартует."
    say "════════════════════════════════════════════════════════════"
    say ""
    say "1. PostgreSQL:"
    say "     dnf install -y postgresql-server && postgresql-setup --initdb"
    say "     systemctl enable --now postgresql"
    say "     sudo -u postgres psql -c \"CREATE USER jhvirt WITH PASSWORD 'пароль';\" \\"
    say "                         -c \"CREATE DATABASE jhvirt OWNER jhvirt;\""
    say ""
    say "2. $PREFIX/config/jhvirt.env (права 0600 — юнит читают все):"
    say "     JHV_DATABASE_URL=host=localhost port=5432 user=jhvirt password=пароль dbname=jhvirt sslmode=disable"
    say "     JHV_SERVER_EXTERNAL_URL=$URL"
    say ""
    say "3. Запуск и пароль администратора:"
    say "     systemctl enable --now jhvirt"
    say "     journalctl -u jhvirt -n 50 | grep -A6 'УЧЁТНАЯ ЗАПИСЬ'"
}

case "$MODE" in
    docker|docker-compose|podman) install_containers ;;
    systemd)                      install_systemd ;;
esac
