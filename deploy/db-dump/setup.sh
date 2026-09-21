#!/bin/sh
# Установка хелпера jhvirt-db-dump на хост СУБД.
#
#   sudo sh setup.sh --pubkey jhvirt.pub --postgresql [--mysql] [--allow-restore] [--user jhvirt_dump]
#
# Что делает:
#   1. создаёт системного пользователя (по умолчанию jhvirt_dump) без пароля;
#   2. ставит хелпер в /usr/local/sbin/jhvirt-db-dump (root, 0755);
#   3. кладёт публичный ключ службы в ~/.ssh/authorized_keys пользователя
#      с restrict,command="/usr/local/sbin/jhvirt-db-dump" — shell по этому
#      ключу недоступен;
#   4. создаёт роль в СУБД с правом только на чтение и входом через сокет;
#   5. создаёт /etc/jhvirt/db-dump.conf (root, 0644), если его ещё нет.
#
# Пароли СУБД не создаются и не запрашиваются.
set -eu

HERE=$(cd -- "$(dirname -- "$0")" && pwd)
USER_NAME=jhvirt_dump
PUBKEY=
WANT_PG=0
WANT_MY=0
ALLOW_RESTORE=0

die() {
    printf 'ошибка: %s\n' "$*" >&2
    exit 1
}
say() { printf '%s\n' "$*"; }

while [ $# -gt 0 ]; do
    case "$1" in
    --pubkey) PUBKEY=${2:-}; shift 2 ;;
    --user) USER_NAME=${2:-}; shift 2 ;;
    --postgresql) WANT_PG=1; shift ;;
    --mysql) WANT_MY=1; shift ;;
    --allow-restore) ALLOW_RESTORE=1; shift ;;
    -h | --help) sed -n '2,17p' "$0"; exit 0 ;;
    *) die "неизвестный параметр: $1" ;;
    esac
done

[ "$(id -u)" -eq 0 ] || die "запустите от root"
[ -n "$PUBKEY" ] && [ -r "$PUBKEY" ] || die "укажите --pubkey с публичным ключом службы (его показывает интерфейс)"
[ "$WANT_PG" -eq 1 ] || [ "$WANT_MY" -eq 1 ] || die "укажите --postgresql и/или --mysql"
printf '%s' "$USER_NAME" | grep -Eq '^[a-z_][a-z0-9_]{0,31}$' || die "имя пользователя: строчная латиница, цифры и «_»"
KEY_LINE=$(grep -Ev '^[[:space:]]*(#|$)' "$PUBKEY" | head -n 1)
KEY_TYPE=$(printf '%s\n' "$KEY_LINE" | awk '{print $1}')
KEY_DATA=$(printf '%s\n' "$KEY_LINE" | awk '{print $2}')
KEY="$KEY_TYPE $KEY_DATA"
printf '%s' "$KEY" | grep -Eq '^(ssh-ed25519|ssh-rsa|ecdsa-sha2-[a-z0-9-]+) [A-Za-z0-9+/=]+$' ||
    die "в $PUBKEY нет публичного ключа OpenSSH"

# 1. Пользователь. Shell нужен: sshd выполняет forced command через него.
if ! id "$USER_NAME" >/dev/null 2>&1; then
    useradd --system --create-home --shell /bin/sh --comment "jhvirt database dumps" "$USER_NAME"
    say "создан пользователь $USER_NAME"
fi
passwd -l "$USER_NAME" >/dev/null 2>&1 || true
HOME_DIR=$(getent passwd "$USER_NAME" | cut -d: -f6)
[ -n "$HOME_DIR" ] && [ -d "$HOME_DIR" ] || die "нет домашнего каталога у $USER_NAME"

# 2. Хелпер.
install -m 0755 -o root -g root "$HERE/jhvirt-db-dump" /usr/local/sbin/jhvirt-db-dump
say "установлен /usr/local/sbin/jhvirt-db-dump"

# 3. Ключ службы — только с forced command. Каталог и файл
# остаются у root: даже при компрометации непривилегированной учётки
# она не сможет убрать restrict или заменить forced command.
install -d -m 0755 -o root -g root "$HOME_DIR/.ssh"
AUTH="$HOME_DIR/.ssh/authorized_keys"
[ ! -L "$AUTH" ] || die "$AUTH не должен быть символической ссылкой"
LINE="restrict,command=\"/usr/local/sbin/jhvirt-db-dump\" $KEY"
# Rebuild the file on every run. An older/manual installation may contain the
# same key without restrictions; merely noticing that line would leave shell
# access enabled. Remove every occurrence of this key and add one canonical,
# forced-command-only entry.
AUTH_TMP=$(mktemp "${AUTH}.XXXXXX")
trap 'rm -f "$AUTH_TMP"' EXIT HUP INT TERM
if [ -f "$AUTH" ]; then
    awk -v key="$KEY" 'index($0, key) == 0' "$AUTH" >"$AUTH_TMP"
fi
printf '%s\n' "$LINE" >>"$AUTH_TMP"
install -m 0600 -o root -g root "$AUTH_TMP" "$AUTH"
rm -f "$AUTH_TMP"
trap - EXIT HUP INT TERM
say "ключ службы установлен в $AUTH с restrict,command"
if command -v restorecon >/dev/null 2>&1; then
    restorecon -R "$HOME_DIR/.ssh" /usr/local/sbin/jhvirt-db-dump 2>/dev/null || true
fi

# 4. Роли в СУБД.
if [ "$WANT_PG" -eq 1 ]; then
    command -v psql >/dev/null 2>&1 || die "psql не найден"
    runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 -d postgres <<SQL
DO \$\$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = '$USER_NAME') THEN
        CREATE ROLE $USER_NAME LOGIN;
    END IF;
END
\$\$;
GRANT pg_read_all_data TO $USER_NAME;
GRANT pg_read_all_stats TO $USER_NAME;
SQL
    if [ "$ALLOW_RESTORE" -eq 1 ]; then
        runuser -u postgres -- psql -X -q -v ON_ERROR_STOP=1 -d postgres -c "ALTER ROLE $USER_NAME CREATEDB"
    fi
    say "роль PostgreSQL $USER_NAME: вход через сокет (peer), pg_read_all_data, pg_read_all_stats$([ "$ALLOW_RESTORE" -eq 1 ] && echo ', CREATEDB')"
    say "  pg_read_all_data есть с PostgreSQL 14; большие объекты (lo) требуют отдельного права"
fi

if [ "$WANT_MY" -eq 1 ]; then
    CLIENT=$(command -v mariadb 2>/dev/null || command -v mysql 2>/dev/null || true)
    [ -n "$CLIENT" ] || die "клиент mysql/mariadb не найден"
    VERSION=$("$CLIENT" -NBe 'SELECT VERSION()')
    case "$VERSION" in
    *MariaDB*) AUTH_SQL="IDENTIFIED VIA unix_socket" ;;
    *) AUTH_SQL="IDENTIFIED WITH auth_socket" ;;
    esac
    GRANTS="SELECT, SHOW VIEW, TRIGGER, EVENT, LOCK TABLES"
    if [ "$ALLOW_RESTORE" -eq 1 ]; then
        GRANTS="$GRANTS, CREATE, DROP, INSERT, ALTER, INDEX, REFERENCES, CREATE VIEW, CREATE ROUTINE, ALTER ROUTINE"
    fi
    "$CLIENT" -e "CREATE USER IF NOT EXISTS '$USER_NAME'@'localhost' $AUTH_SQL; GRANT $GRANTS ON *.* TO '$USER_NAME'@'localhost';"
    say "учётная запись MySQL/MariaDB $USER_NAME@localhost: вход через сокет, права: $GRANTS"
fi

# 5. Настройки. Каталог должен быть проходим для пользователя хелпера: иначе
# хелпер не увидит db-dump.conf. Ранние версии guest-hooks/install.sh
# закрывали его до 0700.
install -d -m 0755 -o root -g root /etc/jhvirt
if [ ! -e /etc/jhvirt/db-dump.conf ]; then
    install -m 0644 -o root -g root "$HERE/db-dump.conf.example" /etc/jhvirt/db-dump.conf
    if [ "$ALLOW_RESTORE" -eq 1 ]; then
        sed -i 's/^#\{0,1\}JHVIRT_DB_ALLOW_RESTORE=.*/JHVIRT_DB_ALLOW_RESTORE=1/' /etc/jhvirt/db-dump.conf
    fi
    say "настройки: /etc/jhvirt/db-dump.conf"
fi

say ""
say "Проверка от имени хелпера (без SSH):"
say "  runuser -u $USER_NAME -- /usr/local/sbin/jhvirt-db-dump probe"
say "Затем в интерфейсе службы: Дампы СУБД → Хост СУБД → «Проверить»."
