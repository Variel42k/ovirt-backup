#!/bin/sh
# Установка сценариев заморозки jhvirt в гостевую ВМ Linux.
#
#   sudo ./install.sh --postgresql [--mysql]   установить диспетчер и сценарии
#   sudo ./install.sh --check                  прогнать freeze/thaw сценариев без заморозки ФС
#   sudo ./install.sh --uninstall              убрать сценарии и вернуть штатный диспетчер
#
# Установщик не меняет настройки qemu-guest-agent сам: он проверяет, включён
# ли вызов хука, и печатает, что поправить, если нет.
set -eu

HERE=$(cd -- "$(dirname -- "$0")" && pwd)
MARK='комплект jhvirt'

die() {
    printf 'ошибка: %s\n' "$*" >&2
    exit 1
}
say() { printf '%s\n' "$*"; }

[ "$(id -u)" -eq 0 ] || die "запустите от root"

want_pg=0 want_my=0 mode=install
for arg in "$@"; do
    case "$arg" in
    --postgresql) want_pg=1 ;;
    --mysql) want_my=1 ;;
    --check) mode=check ;;
    --uninstall) mode=uninstall ;;
    -h | --help)
        sed -n '2,10p' "$0"
        exit 0
        ;;
    *) die "неизвестный параметр: $arg" ;;
    esac
done

# Каталог настроек агента: /etc/qemu-ga в семействе RHEL (РЕД ОС, Alma, Rocky),
# /etc/qemu в Debian, Ubuntu и Astra Linux.
detect_dir() {
    for dir in /etc/qemu-ga /etc/qemu; do
        if [ -f "$dir/fsfreeze-hook" ]; then
            printf '%s\n' "$dir"
            return 0
        fi
    done
    for dir in /etc/qemu-ga /etc/qemu; do
        if [ -d "$dir" ]; then
            printf '%s\n' "$dir"
            return 0
        fi
    done
    return 1
}

DIR=$(detect_dir) || die "не найден каталог /etc/qemu-ga или /etc/qemu — установите qemu-guest-agent"
HOOK="$DIR/fsfreeze-hook"
HOOK_D="$DIR/fsfreeze-hook.d"

relabel() {
    if command -v selinuxenabled >/dev/null 2>&1 && selinuxenabled && command -v restorecon >/dev/null 2>&1; then
        restorecon -R "$DIR" || say "предупреждение: restorecon $DIR завершился с ошибкой"
    fi
}

# hook_enabled сообщает, вызывает ли агент хук. В RHEL это переменная
# FSFREEZE_HOOK_PATHNAME в /etc/sysconfig/qemu-ga, в Debian — ключ
# fsfreeze-hook в /etc/qemu/qemu-ga.conf или параметр -F в командной строке.
hook_enabled() {
    if ps -o args= -C qemu-ga 2>/dev/null | grep -Eq -- '(^| )(-F|--fsfreeze-hook)'; then
        ps -o args= -C qemu-ga | grep -Eq -- '-F ?/dev/null|--fsfreeze-hook=/dev/null' && return 1
        return 0
    fi
    if [ -f /etc/qemu/qemu-ga.conf ] && grep -Eq '^[[:space:]]*fsfreeze-hook[[:space:]]*=' /etc/qemu/qemu-ga.conf; then
        return 0
    fi
    return 1
}

enable_hint() {
    say ""
    say "Агент сейчас не вызывает fsfreeze-hook. Включите его и перезапустите агент:"
    if [ -f /etc/sysconfig/qemu-ga ]; then
        say "  в /etc/sysconfig/qemu-ga:  FSFREEZE_HOOK_PATHNAME=$HOOK"
    else
        say "  в /etc/qemu/qemu-ga.conf:"
        say "    [general]"
        say "    fsfreeze-hook=$HOOK"
    fi
    say "  systemctl restart qemu-guest-agent"
}

case "$mode" in
check)
    [ -x "$HOOK" ] || die "$HOOK не установлен"
    grep -q "$MARK" "$HOOK" || say "предупреждение: $HOOK — не строгий диспетчер jhvirt, ошибки сценариев не отменят заморозку"
    say "freeze (файловые системы не замораживаются, только сценарии)..."
    started=$(date +%s)
    if "$HOOK" freeze; then
        rc=0
    else
        rc=$?
    fi
    held=$(($(date +%s) - started))
    "$HOOK" thaw || true
    tail -n 20 /var/log/qga-fsfreeze-hook.log 2>/dev/null || true
    if [ "$rc" -ne 0 ]; then
        die "freeze завершился с кодом $rc — в таком состоянии агент отменил бы заморозку"
    fi
    say "готово: сценарии подготовили приложения за $held с"
    hook_enabled || enable_hint
    exit 0
    ;;
uninstall)
    rm -f "$HOOK_D/50-jhvirt-postgresql" "$HOOK_D/50-jhvirt-mysql"
    if [ -f "$HOOK.jhvirt-orig" ]; then
        mv -f "$HOOK.jhvirt-orig" "$HOOK"
        say "штатный диспетчер восстановлен: $HOOK"
    elif [ -f "$HOOK" ] && grep -q "$MARK" "$HOOK"; then
        rm -f "$HOOK"
        say "диспетчер jhvirt удалён"
    fi
    relabel
    say "сценарии jhvirt удалены; /etc/jhvirt оставлен как есть"
    exit 0
    ;;
esac

[ "$want_pg" -eq 1 ] || [ "$want_my" -eq 1 ] || die "укажите --postgresql и/или --mysql"

if [ -f "$HOOK" ] && ! grep -q "$MARK" "$HOOK" && [ ! -e "$HOOK.jhvirt-orig" ]; then
    cp -p "$HOOK" "$HOOK.jhvirt-orig"
    say "штатный диспетчер сохранён как $HOOK.jhvirt-orig"
fi
install -m 0755 -o root -g root "$HERE/fsfreeze-hook" "$HOOK"
install -d -m 0755 -o root -g root "$HOOK_D"
if [ "$want_pg" -eq 1 ]; then
    install -m 0700 -o root -g root "$HERE/fsfreeze-hook.d/50-jhvirt-postgresql" "$HOOK_D/"
    say "установлен $HOOK_D/50-jhvirt-postgresql"
fi
if [ "$want_my" -eq 1 ]; then
    install -m 0700 -o root -g root "$HERE/fsfreeze-hook.d/50-jhvirt-mysql" "$HOOK_D/"
    say "установлен $HOOK_D/50-jhvirt-mysql"
fi
install -d -m 0700 -o root -g root /etc/jhvirt
if [ ! -e /etc/jhvirt/guest-hooks.conf ]; then
    install -m 0600 -o root -g root "$HERE/guest-hooks.conf.example" /etc/jhvirt/guest-hooks.conf
    say "настройки: /etc/jhvirt/guest-hooks.conf (всё закомментировано — значения по умолчанию)"
fi
relabel

say "строгий диспетчер: $HOOK"
hook_enabled || enable_hint
say ""
say "Проверьте сценарии без заморозки ФС:  $0 --check"
