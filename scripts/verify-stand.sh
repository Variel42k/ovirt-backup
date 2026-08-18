#!/bin/sh
# Проверка копий на боевом стенде: прогоняет режимы проверки по каждой точке
# восстановления и печатает отчёт.
#
# Зачем отдельный скрипт, а не «нажать в интерфейсе»: приёмка должна быть
# повторяемой и оставлять след. Нажатия оставляют след только в памяти того,
# кто нажимал, а через месяц вопрос «а проверяли ли мы копии после того
# обновления» остаётся без ответа. Скрипт отвечает файлом.
#
# Сессию скрипт не создаёт и пароль не спрашивает: вход выполняет человек в
# браузере, а сюда передаётся значение куки. Так у проверки нет доступа
# большего, чем у того, кто её запустил, и отзывается он обычным выходом
# из системы.
#
# Использование:
#   JHV_SESSION=<кука jhvirt_session> ./scripts/verify-stand.sh [адрес]
#
#   JHV_MODES   — режимы через пробел; по умолчанию те, что не требуют
#                 гипервизора. Режим boot сюда не входит намеренно: ему нужен
#                 KVM-хост, и на стенде без вложенной виртуализации он всегда
#                 будет падать, приучая не смотреть на красный результат.
#   JHV_LIMIT   — сколько последних точек проверять (по умолчанию 5)
#   JHV_REPORT  — куда положить отчёт (по умолчанию verify-report-<дата>.txt)

set -eu

BASE="${1:-http://10.249.251.210:8080}"
MODES="${JHV_MODES:-quick chain manifest structure qemu}"
LIMIT="${JHV_LIMIT:-5}"
REPORT="${JHV_REPORT:-verify-report-$(date +%Y%m%d-%H%M%S).txt}"

[ -n "${JHV_SESSION:-}" ] || {
    echo "не задана JHV_SESSION — значение куки jhvirt_session из браузера" >&2
    exit 2
}

api() { # метод путь [тело]
    if [ $# -ge 3 ]; then
        curl -sS -m 900 -X "$1" -b "jhvirt_session=$JHV_SESSION" \
            -H 'Content-Type: application/json' -d "$3" "$BASE/api/v1$2"
    else
        curl -sS -m 900 -X "$1" -b "jhvirt_session=$JHV_SESSION" "$BASE/api/v1$2"
    fi
}

# Проверка сессии до начала работы: иначе первые же ответы «требуется вход»
# выглядят как отказ проверки, а не как истёкшая кука.
WHO="$(api GET /auth/me || true)"
case "$WHO" in
    *'"role"'*) : ;;
    *) echo "сессия не принята: $WHO" >&2; exit 3 ;;
esac

{
    echo "проверка копий: $BASE"
    echo "начата:         $(date '+%F %T')"
    echo "режимы:         $MODES"
    echo
} | tee "$REPORT"

RUNS="$(api GET "/backups?limit=$LIMIT" |
    tr ',' '\n' | grep -oE '"id":"[0-9a-f-]{36}"' | cut -d'"' -f4 || true)"

[ -n "$RUNS" ] || { echo "точек восстановления нет" | tee -a "$REPORT"; exit 0; }

FAILED=0
for RUN in $RUNS; do
    NAME="$(api GET "/backups/$RUN" | tr ',' '\n' | grep -m1 -oE '"vm_name":"[^"]*"' | cut -d'"' -f4 || true)"
    printf '%s (%s)\n' "${NAME:-без имени}" "$RUN" | tee -a "$REPORT"

    for MODE in $MODES; do
        OUT="$(api POST "/backups/$RUN/verify" "{\"mode\":\"$MODE\"}" || echo '{"status":"нет ответа"}')"
        # Быстрые режимы отвечают сразу, длинные — уходят в фон и дают статус
        # running; и то и другое печатается как есть, без попытки угадать.
        STATUS="$(printf '%s' "$OUT" | tr ',' '\n' | grep -m1 -oE '"status":"[^"]*"' | cut -d'"' -f4 || true)"
        ERROR="$(printf '%s' "$OUT" | tr ',' '\n' | grep -m1 -oE '"error":"[^"]*"' | cut -d'"' -f4 || true)"
        printf '  %-10s %-12s %s\n' "$MODE" "${STATUS:-?}" "${ERROR:-}" | tee -a "$REPORT"
        case "$STATUS" in
            succeeded|running|pending) : ;;
            *) FAILED=$((FAILED + 1)) ;;
        esac
    done
    echo | tee -a "$REPORT"
done

{
    echo "завершена: $(date '+%F %T')"
    if [ "$FAILED" -eq 0 ]; then
        echo "итог:      замечаний нет"
    else
        echo "итог:      неуспешных проверок: $FAILED"
    fi
    echo
    echo "Проверки, ушедшие в фон, досматривать в разделе «Бэкапы» или через"
    echo "GET /api/v1/verifications — здесь виден только их запуск."
} | tee -a "$REPORT"

echo "отчёт: $REPORT" >&2
[ "$FAILED" -eq 0 ]
