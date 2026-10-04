#!/bin/sh
# Установка хелпера jhvirt-gitlab-clean на ВМ с GitLab.
#
#   sudo sh setup.sh --pubkey jhvirt.pub [--user jhvirt_gitclean] [--allow-clean]
#
# Что делает:
#   1. создаёт системного пользователя (по умолчанию jhvirt_gitclean) без пароля;
#   2. ставит хелпер в /usr/local/sbin/jhvirt-gitlab-clean (root, 0755);
#   3. разрешает этому пользователю запускать хелпер от имени git через sudo —
#      и только его: репозитории Gitaly принадлежат git;
#   4. кладёт публичный ключ службы в ~/.ssh/authorized_keys с
#      restrict,command="sudo -n -u git /usr/local/sbin/jhvirt-gitlab-clean" —
#      shell по этому ключу недоступен.
#
# Без --allow-clean хелпер умеет только анализ: он читает историю и ничего не
# меняет, поэтому так его можно ставить и на рабочий GitLab.
#
# --allow-clean создаёт /etc/jhvirt/gitlab-clean.allow и разрешает переписывать
# историю репозиториев НА ЭТОЙ МАШИНЕ. Указывайте его только на копии ВМ,
# поднятой из бэкапа. Установщик переспросит имя машины.
set -eu

HERE=$(cd -- "$(dirname -- "$0")" && pwd)
USER_NAME=jhvirt_gitclean
PUBKEY=
ALLOW_CLEAN=0
ALLOW=/etc/jhvirt/gitlab-clean.allow

die() {
    printf 'ошибка: %s\n' "$*" >&2
    exit 1
}
say() { printf '%s\n' "$*"; }

while [ $# -gt 0 ]; do
    case "$1" in
    --pubkey) PUBKEY=${2:-}; shift 2 ;;
    --user) USER_NAME=${2:-}; shift 2 ;;
    --allow-clean) ALLOW_CLEAN=1; shift ;;
    -h | --help) sed -n '2,21p' "$0"; exit 0 ;;
    *) die "неизвестный параметр: $1" ;;
    esac
done

[ "$(id -u)" -eq 0 ] || die "запустите от root"
[ -n "$PUBKEY" ] && [ -r "$PUBKEY" ] || die "укажите --pubkey с публичным ключом службы"
id git >/dev/null 2>&1 || die "пользователь git не найден: GitLab Omnibus на этой машине не установлен"
command -v sudo >/dev/null 2>&1 || die "sudo не найден"
printf '%s' "$USER_NAME" | grep -Eq '^[a-z_][a-z0-9_]{0,31}$' || die "имя пользователя: строчная латиница, цифры и «_»"
KEY_LINE=$(grep -Ev '^[[:space:]]*(#|$)' "$PUBKEY" | head -n 1)
KEY_TYPE=$(printf '%s\n' "$KEY_LINE" | awk '{print $1}')
KEY_DATA=$(printf '%s\n' "$KEY_LINE" | awk '{print $2}')
KEY="$KEY_TYPE $KEY_DATA"
printf '%s' "$KEY" | grep -Eq '^(ssh-ed25519|ssh-rsa|ecdsa-sha2-[a-z0-9-]+) [A-Za-z0-9+/=]+$' ||
    die "в $PUBKEY нет публичного ключа OpenSSH"

# 1. Пользователь. Shell нужен: sshd выполняет forced command через него.
if ! id "$USER_NAME" >/dev/null 2>&1; then
    useradd --system --create-home --shell /bin/sh --comment "jhvirt gitlab repository cleanup" "$USER_NAME"
    say "создан пользователь $USER_NAME"
fi
passwd -l "$USER_NAME" >/dev/null 2>&1 || true
HOME_DIR=$(getent passwd "$USER_NAME" | cut -d: -f6)
[ -n "$HOME_DIR" ] && [ -d "$HOME_DIR" ] || die "нет домашнего каталога у $USER_NAME"

# 2. Хелпер.
install -m 0755 -o root -g root "$HERE/jhvirt-gitlab-clean" /usr/local/sbin/jhvirt-gitlab-clean
say "установлен /usr/local/sbin/jhvirt-gitlab-clean"

# 3. sudo: только хелпер и только от имени git. SSH_ORIGINAL_COMMAND
# сохраняется — в нём sshd передаёт запрос службы.
SUDOERS=/etc/sudoers.d/jhvirt-gitlab-clean
TMP=$(mktemp)
cat >"$TMP" <<EOF
Defaults:$USER_NAME env_keep += "SSH_ORIGINAL_COMMAND"
Defaults:$USER_NAME !requiretty
$USER_NAME ALL=(git) NOPASSWD: /usr/local/sbin/jhvirt-gitlab-clean
EOF
visudo -cf "$TMP" >/dev/null || { rm -f "$TMP"; die "не удалось составить правило sudo"; }
install -m 0440 -o root -g root "$TMP" "$SUDOERS"
rm -f "$TMP"
say "добавлено правило sudo: $SUDOERS"

# 4. Ключ службы: только forced command.
install -d -m 0700 -o "$USER_NAME" -g "$(id -gn "$USER_NAME")" "$HOME_DIR/.ssh"
AUTH="$HOME_DIR/.ssh/authorized_keys"
LINE="restrict,command=\"sudo -n -u git /usr/local/sbin/jhvirt-gitlab-clean\" $KEY jhvirt"
touch "$AUTH"
if ! grep -qF "$KEY_DATA" "$AUTH"; then
    printf '%s\n' "$LINE" >>"$AUTH"
    say "ключ службы добавлен в $AUTH"
fi
chown "$USER_NAME:$(id -gn "$USER_NAME")" "$AUTH"
chmod 0600 "$AUTH"

if ! sudo -u git sh -c 'command -v git-filter-repo >/dev/null 2>&1 || PATH=/opt/gitlab/embedded/bin:$PATH git filter-repo --version >/dev/null 2>&1'; then
    say "внимание: git filter-repo не найден. Для анализа он не нужен, для очистки установите пакет git-filter-repo."
fi

# 5. Разрешение на очистку — только по явному флагу и с подтверждением имени.
if [ "$ALLOW_CLEAN" -eq 1 ]; then
    HOST=$(hostname -f 2>/dev/null || hostname)
    say ""
    say "Вы разрешаете ПЕРЕПИСЫВАТЬ ИСТОРИЮ репозиториев GitLab на машине $HOST."
    say "Это необратимо. Делайте это только на копии ВМ, поднятой из бэкапа, а не на рабочем GitLab."
    printf 'Введите слово КОПИЯ, чтобы продолжить: '
    read -r ANSWER
    [ "$ANSWER" = "КОПИЯ" ] || die "разрешение не выдано"
    install -d -m 0755 -o root -g root /etc/jhvirt
    printf 'Очистка истории репозиториев разрешена %s на %s\n' "$(date -u +%Y-%m-%dT%H:%M:%SZ)" "$HOST" >"$ALLOW"
    chown root:root "$ALLOW"
    chmod 0644 "$ALLOW"
    say "создан $ALLOW: очистка на этой машине разрешена"
    say "перед очисткой остановите запись в репозитории: gitlab-ctl stop puma; gitlab-ctl stop sidekiq"
elif [ -e "$ALLOW" ]; then
    say "внимание: $ALLOW уже существует — очистка на этой машине разрешена. Если это рабочий GitLab, удалите файл."
fi

say ""
say "Готово. Проверка: sudo -u $USER_NAME sudo -n -u git /usr/local/sbin/jhvirt-gitlab-clean probe"
