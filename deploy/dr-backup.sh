#!/bin/sh
set -eu
umask 077

OUTPUT_DIR="${JHV_DR_OUTPUT_DIR:?не задан JHV_DR_OUTPUT_DIR}"
KEEP_DAYS="${JHV_DR_KEEP_DAYS:-7}"
INTERVAL_SECONDS="${JHV_DR_INTERVAL_SECONDS:-86400}"

case "$KEEP_DAYS" in ""|*[!0-9]*) echo "JHV_DR_KEEP_DAYS должен быть целым числом" >&2; exit 2 ;; esac
case "$INTERVAL_SECONDS" in ""|*[!0-9]*) echo "JHV_DR_INTERVAL_SECONDS должен быть целым числом" >&2; exit 2 ;; esac
[ "$KEEP_DAYS" -ge 1 ] || { echo "JHV_DR_KEEP_DAYS должен быть не меньше 1" >&2; exit 2; }
[ "$INTERVAL_SECONDS" -ge 60 ] || { echo "JHV_DR_INTERVAL_SECONDS должен быть не меньше 60" >&2; exit 2; }

read_password() {
    [ -n "${PGPASSWORD_FILE:-}" ] || return 0
    [ -f "$PGPASSWORD_FILE" ] || { echo "не найден PGPASSWORD_FILE: $PGPASSWORD_FILE" >&2; return 1; }
    PGPASSWORD="$(sed -n '1p' "$PGPASSWORD_FILE")"
    [ -n "$PGPASSWORD" ] || { echo "PGPASSWORD_FILE пуст: $PGPASSWORD_FILE" >&2; return 1; }
    export PGPASSWORD
}

backup_once() {
    mkdir -p "$OUTPUT_DIR" || {
        echo "не удалось создать каталог резервных копий: $OUTPUT_DIR" >&2
        return 1
    }
    LOCK="$OUTPUT_DIR/.backup.lock"
    if ! mkdir "$LOCK" 2>/dev/null; then
        echo "резервирование уже выполняется: $LOCK" >&2
        return 1
    fi

    STAMP="$(date -u '+%Y%m%dT%H%M%SZ')"
    TMP="$OUTPUT_DIR/.postgres-$STAMP.dump.tmp"
    FINAL="$OUTPUT_DIR/postgres-$STAMP.dump"
    KEY_TMP=""
    cleanup() { rm -f "$TMP" ${KEY_TMP:+"$KEY_TMP"}; rmdir "$LOCK" 2>/dev/null || true; }
    trap cleanup EXIT HUP INT TERM

    read_password || { cleanup; trap - EXIT HUP INT TERM; return 1; }
    pg_dump --format=custom --no-owner --no-privileges --file="$TMP" || {
        echo "pg_dump завершился ошибкой; финальный файл не опубликован" >&2
        cleanup; trap - EXIT HUP INT TERM; return 1
    }
    pg_restore --list "$TMP" >/dev/null || {
        echo "проверка dump завершилась ошибкой; финальный файл не опубликован" >&2
        cleanup; trap - EXIT HUP INT TERM; return 1
    }
    chmod 0600 "$TMP" && mv "$TMP" "$FINAL" || {
        echo "не удалось опубликовать dump" >&2
        cleanup; trap - EXIT HUP INT TERM; return 1
    }

    if [ -n "${JHV_SECRET_KEY_SOURCE:-}" ] || [ -n "${JHV_SECRET_KEY_BACKUP:-}" ]; then
        [ -n "${JHV_SECRET_KEY_SOURCE:-}" ] && [ -n "${JHV_SECRET_KEY_BACKUP:-}" ] || {
            echo "JHV_SECRET_KEY_SOURCE и JHV_SECRET_KEY_BACKUP задаются вместе" >&2
            cleanup; trap - EXIT HUP INT TERM; return 1
        }
        [ -s "$JHV_SECRET_KEY_SOURCE" ] || {
            echo "не найден secret.key: $JHV_SECRET_KEY_SOURCE" >&2
            cleanup; trap - EXIT HUP INT TERM; return 1
        }
        KEY_TMP="$JHV_SECRET_KEY_BACKUP.tmp"
        mkdir -p "$(dirname "$JHV_SECRET_KEY_BACKUP")" &&
            cp "$JHV_SECRET_KEY_SOURCE" "$KEY_TMP" && chmod 0600 "$KEY_TMP" || {
                echo "не удалось создать копию secret.key" >&2
                cleanup; trap - EXIT HUP INT TERM; return 1
            }
        KEY_SOURCE_SHA="$(sha256sum "$JHV_SECRET_KEY_SOURCE" | awk '{print $1}')"
        KEY_BACKUP_SHA="$(sha256sum "$KEY_TMP" | awk '{print $1}')"
        if [ -z "$KEY_SOURCE_SHA" ] || [ "$KEY_SOURCE_SHA" != "$KEY_BACKUP_SHA" ]; then
            echo "копия secret.key не совпала" >&2
            cleanup; trap - EXIT HUP INT TERM; return 1
        fi
        mv "$KEY_TMP" "$JHV_SECRET_KEY_BACKUP" || {
            echo "не удалось опубликовать копию secret.key" >&2
            cleanup; trap - EXIT HUP INT TERM; return 1
        }
        KEY_TMP=""
    fi

    find "$OUTPUT_DIR" -maxdepth 1 -type f -name 'postgres-*.dump' -mtime "+$KEEP_DAYS" -delete || {
        echo "предупреждение: не удалось удалить устаревшие dump" >&2
    }
    echo "создан $FINAL"
    trap - EXIT HUP INT TERM
    cleanup
}

case "${1:---once}" in
    --once) backup_once ;;
    --loop)
        while :; do
            backup_once || true
            sleep "$INTERVAL_SECONDS"
        done
        ;;
    *) echo "использование: $0 --once|--loop" >&2; exit 2 ;;
esac
