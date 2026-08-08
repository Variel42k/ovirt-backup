#!/bin/sh
# Установка justhpc-virt-manager из собранного комплекта.
#
# Скрипт намеренно ничего не додумывает за оператора: не перезаписывает
# существующую конфигурацию, не создаёт TLS, не прописывает пароль СУБД и не
# запускает службу. Это решения, которые принимает человек, и молча принять их
# за него — худшее, что может сделать установщик.
#
# Повторный запуск обновляет установку на месте: бинари и интерфейс заменяются,
# конфигурация и данные остаются.
#
# Использование:
#   sudo ./install.sh                    установить в /opt/jhvirt
#   sudo PREFIX=/srv/jhvirt ./install.sh другой каталог
#   sudo ./install.sh --uninstall        удалить, сохранив данные

set -eu

PREFIX="${PREFIX:-/opt/jhvirt}"
USER_NAME="${USER_NAME:-jhvirt}"
UNIT="/etc/systemd/system/jhvirt.service"
SRC="$(cd "$(dirname "$0")" && pwd)"

die() { printf 'ошибка: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

[ "$(id -u)" -eq 0 ] || die "нужны права root: sudo $0"

# --- Удаление ---------------------------------------------------------------

if [ "${1:-}" = "--uninstall" ]; then
    say "==> остановка службы"
    systemctl disable --now jhvirt 2>/dev/null || true
    rm -f "$UNIT"
    systemctl daemon-reload 2>/dev/null || true

    rm -rf "$PREFIX/bin" "$PREFIX/web"
    say ""
    say "Служба удалена. Намеренно оставлены:"
    say "  $PREFIX/data   — ключ шифрования и база; без ключа не расшифровать"
    say "                   пароли подключений и зашифрованные копии"
    say "  $PREFIX/config — конфигурация"
    say "  $PREFIX/logs   — журналы"
    say ""
    say "Удалить полностью: rm -rf $PREFIX && userdel $USER_NAME"
    exit 0
fi

# --- Проверки ---------------------------------------------------------------

[ -f "$SRC/bin/justhpc-virt-server" ] || die "в комплекте нет bin/justhpc-virt-server"
[ -f "$SRC/web/dist/index.html" ]     || die "в комплекте нет собранного интерфейса (web/dist)"

command -v systemctl >/dev/null 2>&1 || \
    die "systemd не найден; запускайте бинарь своим способом, см. docs/DEPLOY.md"

UPGRADE=0
[ -x "$PREFIX/bin/justhpc-virt-server" ] && UPGRADE=1

if [ "$UPGRADE" -eq 1 ]; then
    OLD="$("$PREFIX/bin/justhpc-virt-server" -version 2>/dev/null || echo '?')"
    NEW="$("$SRC/bin/justhpc-virt-server" -version 2>/dev/null || echo '?')"
    say "==> обновление: $OLD -> $NEW"
else
    say "==> установка в $PREFIX"
fi

# --- Пользователь и каталоги ------------------------------------------------

if ! id "$USER_NAME" >/dev/null 2>&1; then
    useradd --system --home-dir "$PREFIX" --shell /usr/sbin/nologin "$USER_NAME" 2>/dev/null ||
        useradd --system --home-dir "$PREFIX" --shell /sbin/nologin "$USER_NAME"
    say "    создан системный пользователь $USER_NAME"
fi

mkdir -p "$PREFIX/bin" "$PREFIX/config" "$PREFIX/data" "$PREFIX/logs" "$PREFIX/web" "$PREFIX/docs"

# --- Файлы ------------------------------------------------------------------
#
# Службу останавливаем перед заменой бинаря: подменить исполняемый файл под
# работающим процессом на Linux можно, но обновление тогда применится только
# после перезапуска, и «обновил, а версия старая» станет загадкой.

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

[ -d "$SRC/docs" ] && cp -r "$SRC/docs/." "$PREFIX/docs/"
[ -f "$SRC/VERSION" ] && cp "$SRC/VERSION" "$PREFIX/VERSION"

# Конфигурацию не трогаем: в ней уже могут быть правки оператора.
if [ -f "$PREFIX/config/virt-manager.yaml" ]; then
    cp "$SRC/config/virt-manager.yaml" "$PREFIX/config/virt-manager.yaml.new"
    say "    конфигурация сохранена; новая версия рядом: virt-manager.yaml.new"
else
    install -m 0640 "$SRC/config/virt-manager.yaml" "$PREFIX/config/"
fi

install -m 0644 "$SRC/systemd/jhvirt.service" "$UNIT"
systemctl daemon-reload

# --- Права ------------------------------------------------------------------
#
# data содержит ключ шифрования: 0700, доступ только владельцу. Всё остальное
# читаемо, чтобы администратор мог заглянуть без sudo.

chown -R "$USER_NAME:$USER_NAME" "$PREFIX"
chmod 700 "$PREFIX/data"
chmod 750 "$PREFIX/logs"
[ -f "$PREFIX/config/jhvirt.env" ] && chmod 600 "$PREFIX/config/jhvirt.env"

# --- Итог -------------------------------------------------------------------

say ""
if [ "$RESTART" -eq 1 ]; then
    systemctl start jhvirt
    say "==> служба обновлена и запущена"
    say ""
    say "Проверьте: systemctl status jhvirt"
    exit 0
fi

if [ "$UPGRADE" -eq 1 ]; then
    say "==> обновлено. Служба была остановлена — запустите: systemctl start jhvirt"
    exit 0
fi

say "==> установлено в $PREFIX"
say ""
say "Дальше — вручную, потому что это ваши решения:"
say ""
say "  1. Конфигурация:   $PREFIX/config/virt-manager.yaml"
say "     Секреты (пароль СУБД) — в $PREFIX/config/jhvirt.env, права 0600:"
say "       JHV_DATABASE_DRIVER=postgres"
say "       JHV_DATABASE_POSTGRES_PASSWORD=..."
say "       JHV_SERVER_EXTERNAL_URL=https://virt.example.org"
say ""
say "  2. Запуск:         systemctl enable --now jhvirt"
say ""
say "  3. Пароль администратора печатается ОДИН раз:"
say "       journalctl -u jhvirt -n 50 | grep -A6 'УЧЁТНАЯ ЗАПИСЬ'"
say "     Потеряли — не страшно:"
say "       sudo -u $USER_NAME $PREFIX/bin/justhpc-virt-server \\"
say "            -config $PREFIX/config/virt-manager.yaml -reset-password admin"
say ""
say "  4. Чек-лист перед боем: $PREFIX/docs/DEPLOY.md"
