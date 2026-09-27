#!/bin/sh
# Матрица проверки горячего бэкапа на oVirt: прогоняет verify-hot-backup.sh по
# набору тестовых ВМ разных типов и сводит итог в одну таблицу.
#
# Зачем: «горячий бэкап любой ВМ» — утверждение про матрицу, а не про одну
# машину. qcow2 с инкрементами, raw на файловом и блочном домене, смешанная
# ВМ, выключенная, без агента, Windows — у каждой свой путь в службе и в
# движке, и проверять надо каждый.
#
# Файл матрицы — строки «метка идентификатор_ВМ», # — комментарий:
#
#   qcow2-incremental   0f2c…   # все диски qcow2, режим incremental включён
#   raw-file            4b1e…   # raw на NFS
#   raw-block           a93d…   # raw preallocated на iSCSI/FC
#   mixed               7c55…   # системный qcow2 + данные raw
#   down                e012…   # выключенная ВМ
#   no-agent            51aa…   # без qemu-guest-agent
#   windows             9d07…
#
# Использование:
#   JHV_SESSION=<кука jhvirt_session> sh ./scripts/verify-ovirt-matrix.sh \
#       SERVER_ID STORAGE_TARGET_ID MATRIX_FILE [адрес]
#
# Остальные переменные (JHV_RUNS, JHV_TIMEOUT, JHV_INSECURE) передаются в
# verify-hot-backup.sh как есть. Отчёты по каждой ВМ — рядом, сводка —
# в ovirt-matrix-<дата>.txt.

set -eu

[ $# -ge 3 ] || { sed -n '10,26p' "$0"; exit 2; }
SERVER="$1"; TARGET="$2"; MATRIX="$3"
BASE="${4:-https://10.249.251.36:8080}"
[ -r "$MATRIX" ] || { echo "нет файла матрицы: $MATRIX" >&2; exit 2; }

HERE="$(cd "$(dirname "$0")" && pwd)"
STAMP="$(date +%Y%m%d-%H%M%S)"
SUMMARY="ovirt-matrix-$STAMP.txt"
FAILED=0

printf '%-20s %-8s %-40s %s\n' "метка" "итог" "путь" "заметки" | tee "$SUMMARY"

while read -r LABEL VM _REST; do
    case "$LABEL" in ''|'#'*) continue ;; esac
    REPORT="ovirt-matrix-$STAMP-$LABEL.txt"
    if JHV_REPORT="$REPORT" sh "$HERE/verify-hot-backup.sh" "$SERVER" "$VM" "$TARGET" "$BASE" >/dev/null 2>&1; then
        RESULT="ОК"
    else
        RESULT="ОШИБКИ"
        FAILED=$((FAILED + 1))
    fi
    PATH_LINE="$(grep -m1 'путь:' "$REPORT" 2>/dev/null | sed 's/.*путь: //' || true)"
    NOTES=""
    grep -q 'смешанный бэкап' "$REPORT" 2>/dev/null && NOTES="$NOTES смешанный;"
    grep -q 'инкремент от checkpoint' "$REPORT" 2>/dev/null && NOTES="$NOTES инкремент;"
    grep -q 'второй запуск не инкремент' "$REPORT" 2>/dev/null && NOTES="$NOTES без инкремента;"
    grep -q 'инкремент снят полным' "$REPORT" 2>/dev/null && NOTES="$NOTES откат к полному;"
    grep -q 'гость замораживался' "$REPORT" 2>/dev/null && NOTES="$NOTES заморозка!;"
    grep -q 'влияние на ВМ: сильно' "$REPORT" 2>/dev/null && NOTES="$NOTES сильное влияние на ВМ;"
    grep -q 'влияние на ВМ: заметно' "$REPORT" 2>/dev/null && NOTES="$NOTES заметное влияние на ВМ;"
    grep -q 'прогноз: места не хватит\|прогноз: на одном из мест' "$REPORT" 2>/dev/null && NOTES="$NOTES мало места;"
    grep -q 'ждал очереди на хранилище' "$REPORT" 2>/dev/null && NOTES="$NOTES очередь;"
    TOTAL="$(grep -m1 '^итог:' "$REPORT" 2>/dev/null | sed 's/^итог: //' || true)"
    printf '%-20s %-8s %-40s %s\n' "$LABEL" "$RESULT" "${PATH_LINE:-?}" "${NOTES# } ${TOTAL:+($TOTAL)}" |
        tee -a "$SUMMARY"
done <"$MATRIX"

echo | tee -a "$SUMMARY"
echo "ВМ с ошибками: $FAILED; сводка — $SUMMARY, подробности — ovirt-matrix-$STAMP-<метка>.txt" | tee -a "$SUMMARY"
[ "$FAILED" -eq 0 ]
