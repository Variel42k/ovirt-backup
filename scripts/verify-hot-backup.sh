#!/bin/sh
# Проверка горячего бэкапа на стенде: снимает разовые копии одной ВМ без
# заморозки и сверяет хронологию запусков с тем, что должна была сделать
# служба, — чтобы защита работающей ВМ проверялась файлом-отчётом, а не
# памятью того, кто смотрел в интерфейс.
#
# Что проверяется:
#   - гость не замораживался, уровень точки — crash;
#   - запуск успешен, а второй запуск — инкремент от первого;
#   - на KVM в отметке «Данные скопированы» есть пик scratch-файлов;
#   - на Proxmox — что применено из предела чтения и fleecing;
#   - не сработали сторожа места (scratch на KVM, домен хранения на oVirt) и
#     не было неудачной разморозки;
#   - прогноз места до старта (oVirt и KVM) и влияние каждого запуска на
#     ввод-вывод гостя — задержки во время бэкапа против обычных;
#   - ждал ли запуск очереди на хранилище (backup.max_runs_per_storage).
# Ротацию checkpoint-ов скрипт не видит — она на гипервизоре; в конце
# печатается команда, которой её проверить.
#
# Сессию скрипт не создаёт и пароль не спрашивает: вход выполняет человек в
# браузере, а сюда передаётся значение куки, как в verify-stand.sh.
#
# Использование:
#   JHV_SESSION=<кука jhvirt_session> ./scripts/verify-hot-backup.sh \
#       SERVER_ID VM_ID STORAGE_TARGET_ID [адрес]
#
#   JHV_TYPE     — тип копии (по умолчанию incremental: первый запуск станет
#                  полным, второй — инкрементом; для Proxmox — full)
#   JHV_RUNS     — сколько запусков подряд (по умолчанию 2)
#   JHV_TIMEOUT  — сколько ждать один запуск, с (по умолчанию 14400)
#   JHV_INSECURE — 1: не проверять сертификат службы (самоподписанный стенд)
#   JHV_REPORT   — куда положить отчёт (по умолчанию hot-backup-report-<дата>.txt)

set -eu

[ $# -ge 3 ] || { sed -n '20,29p' "$0"; exit 2; }
SERVER="$1"; VM="$2"; TARGET="$3"
BASE="${4:-https://10.249.251.36:8080}"
TYPE="${JHV_TYPE:-}"
RUNS="${JHV_RUNS:-2}"
TIMEOUT="${JHV_TIMEOUT:-14400}"
REPORT="${JHV_REPORT:-hot-backup-report-$(date +%Y%m%d-%H%M%S).txt}"
INSECURE=""
[ "${JHV_INSECURE:-0}" = 1 ] && INSECURE="-k"

[ -n "${JHV_SESSION:-}" ] || {
    echo "не задана JHV_SESSION — значение куки jhvirt_session из браузера" >&2
    exit 2
}
command -v jq >/dev/null 2>&1 || { echo "нужен jq" >&2; exit 2; }

api() { # метод путь [тело]
    if [ $# -ge 3 ]; then
        curl -sS $INSECURE -m 60 -X "$1" -b "jhvirt_session=$JHV_SESSION" \
            -H 'Content-Type: application/json' -d "$3" "$BASE/api/v1$2"
    else
        curl -sS $INSECURE -m 60 -X "$1" -b "jhvirt_session=$JHV_SESSION" "$BASE/api/v1$2"
    fi
}

say() { printf '%s\n' "$*" | tee -a "$REPORT"; }
ok() { say "  ОК        $*"; }
warn() { say "  ВНИМАНИЕ  $*"; WARNINGS=$((WARNINGS + 1)); }
bad() { say "  ОШИБКА    $*"; FAILED=$((FAILED + 1)); }

WHO="$(api GET /auth/me || true)"
printf '%s' "$WHO" | jq -e '.role' >/dev/null 2>&1 || { echo "сессия не принята: $WHO" >&2; exit 3; }

SERVER_JSON="$(api GET "/servers/$SERVER")"
KIND="$(printf '%s' "$SERVER_JSON" | jq -r '.kind // "?"')"
# Есть ли у движка oVirt Backup API: без него временный снапшот — законный путь.
BACKUP_API="$(printf '%s' "$SERVER_JSON" | jq -r '.supports_cbt // false')"
# Proxmox снимает только полный нативный vzdump.
if [ -z "$TYPE" ]; then
    if [ "$KIND" = proxmox ]; then TYPE=full; else TYPE=incremental; fi
fi
: >"$REPORT"
say "горячий бэкап: $BASE"
say "начат:         $(date '+%F %T')"
say "сервер:        $SERVER ($KIND)"
say "ВМ:            $VM"
say "хранилище:     $TARGET"
say "тип, запусков: $TYPE, $RUNS"
say ""

FAILED=0
WARNINGS=0

# Прогноз места: хватит ли места под записи гостя, пока бэкап открыт.
if [ "$KIND" != proxmox ]; then
    OPTIONS="$(api GET "/servers/$SERVER/vms/$VM/backup-options?storage_target_id=$TARGET" || echo '{}')"
    FORECAST="$(printf '%s' "$OPTIONS" | jq -c '.assessment.space_forecast // empty' 2>/dev/null || true)"
    if [ -n "$FORECAST" ]; then
        say "прогноз места (по прошлым бэкапам: $(printf '%s' "$FORECAST" | jq -r '.runs')):"
        printf '%s' "$FORECAST" | jq -r '.places[] |
            "    \(.name): \(.status), свободно \(if .free >= 0 then "\(.free / 1073741824 | floor) ГиБ" else "?" end), " +
            "пик записи \(if .need >= 0 then "\(.need / 1073741824 * 10 | floor / 10) ГиБ" else "?" end)"' | tee -a "$REPORT"
        case "$(printf '%s' "$FORECAST" | jq -r '[.places[].status] | join(" ")')" in
            *no_start*) warn "прогноз: на одном из мест бэкап не начнётся — свободно меньше порога" ;;
            *short*) warn "прогноз: места не хватит, сторож, скорее всего, закроет бэкап досрочно" ;;
            *) ok "прогноз места без угроз" ;;
        esac
    else
        warn "прогноза места нет в ответе backup-options — старая сборка службы?"
    fi
    say ""
fi

i=0
while [ "$i" -lt "$RUNS" ]; do
    i=$((i + 1))
    # Запуски ВМ до этого: новый ищется как отсутствующий в этом списке, а не
    # по времени — часы стенда и машины со скриптом могут расходиться.
    BEFORE="$(api GET "/backups?server_id=$SERVER&vm_id=$VM&limit=50" | jq -c '[.items[].id]' || echo '[]')"
    BODY="$(jq -nc --arg s "$SERVER" --arg v "$VM" --arg t "$TARGET" --arg type "$TYPE" \
        '{server_id:$s, vm_id:$v, storage_target_id:$t, type:$type, consistency:"crash"}')"
    QUEUED="$(api POST /backups "$BODY")"
    printf '%s' "$QUEUED" | jq -e '.status == "queued"' >/dev/null 2>&1 || {
        bad "запуск $i не поставлен в очередь: $QUEUED"
        break
    }

    # Разовый бэкап отвечает только «в очереди»: запуск ищется среди запусков ВМ.
    RUN=""
    waited=0
    while [ -z "$RUN" ] && [ "$waited" -lt 600 ]; do
        sleep 5
        waited=$((waited + 5))
        RUN="$(api GET "/backups?server_id=$SERVER&vm_id=$VM&limit=5" |
            jq -r --argjson before "$BEFORE" '[.items[] | select(.id as $id | ($before | any(.[]; . == $id)) | not)] | sort_by(.created_at) | .[0].id // empty' ||
            true)"
    done
    [ -n "$RUN" ] || { bad "запуск $i не появился за 10 минут"; break; }

    STATUS=""
    waited=0
    while [ "$waited" -lt "$TIMEOUT" ]; do
        DETAIL="$(api GET "/backups/$RUN" || echo '{}')"
        STATUS="$(printf '%s' "$DETAIL" | jq -r '.status // ""' || true)"
        case "$STATUS" in succeeded|failed|partial|canceled) break ;; esac
        sleep 15
        waited=$((waited + 15))
    done

    say "запуск $i: $RUN — $STATUS, тип $(printf '%s' "$DETAIL" | jq -r '.type'), уровень $(printf '%s' "$DETAIL" | jq -r '.consistency // "?"')"
    EVENTS="$(api GET "/backups/$RUN/events")"
    printf '%s' "$EVENTS" | jq -r '.items[] |
        "    \(.at[11:19])  \(.title)\(if .duration_ms > 0 then " (\(.duration_ms / 1000 | floor) с)" else "" end)\(if .detail then " — \(.detail)" else "" end)"' |
        tee -a "$REPORT"

    case "$STATUS" in
        succeeded) ok "запуск успешен" ;;
        *) bad "запуск завершился со статусом $STATUS: $(printf '%s' "$DETAIL" | jq -r '.error // ""')" ;;
    esac

    if printf '%s' "$EVENTS" | jq -e '[.items[].kind] | any(. == "freeze_requested" or . == "frozen" or . == "engine_frozen")' >/dev/null; then
        bad "гость замораживался, хотя уровень crash"
    else
        ok "гость не замораживался"
    fi
    [ "$(printf '%s' "$DETAIL" | jq -r '.consistency // ""')" = crash ] && ok "уровень точки crash" ||
        warn "уровень точки не crash: $(printf '%s' "$DETAIL" | jq -r '.consistency_note // ""')"

    if printf '%s' "$EVENTS" | jq -e '[.items[].kind] | any(. == "thaw_failed")' >/dev/null; then
        bad "в хронологии «Не удалось разморозить гостя» — проверьте ВМ вручную"
    fi
    if printf '%s' "$EVENTS" | jq -e '[.items[].kind] | any(. == "storage_queue")' >/dev/null; then
        ok "запуск ждал очереди на хранилище (backup.max_runs_per_storage)"
    fi

    # Влияние на ВМ: задержки гостя во время бэкапа против обычных за сутки.
    IMPACT="$(api GET "/backups/$RUN/telemetry" | jq -c '.impact // empty' 2>/dev/null || true)"
    if [ -n "$IMPACT" ]; then
        NOTE="$(printf '%s' "$IMPACT" | jq -r '.note')"
        case "$(printf '%s' "$IMPACT" | jq -r '.level')" in
            none) ok "влияние на ВМ: не мешал — $NOTE" ;;
            noticeable) warn "влияние на ВМ: заметно — $NOTE" ;;
            strong) warn "влияние на ВМ: сильно — $NOTE" ;;
            *) say "  СВЕДЕНИЕ  влияние на ВМ неизвестно — $NOTE" ;;
        esac
    else
        say "  СВЕДЕНИЕ  замеров ввода-вывода за время запуска нет — оценить влияние на ВМ нельзя"
    fi

    TRANSFER="$(printf '%s' "$EVENTS" | jq -r '[.items[] | select(.kind == "transfer_finished")] | last | .detail // ""')"
    case "$KIND" in
        kvm)
            case "$TRANSFER" in
                *"пик"*) ok "пик scratch записан: ${TRANSFER##*пик }" ;;
                *) warn "пика scratch в «Данные скопированы» нет — libvirt не отдал статистику задания" ;;
            esac ;;
        proxmox)
            case "$TRANSFER" in *"не применён"*) warn "часть параметров vzdump не применена: $TRANSFER" ;; esac
            case "$TRANSFER" in *"fleecing в хранилище"*) ok "fleecing применён" ;; esac ;;
        ovirt|redvirt|olvm|rhv)
            # Каким путём снята копия. Служебный снапшот рядом с бэкапом — это
            # hybrid backup движка 4.5, а не временный снапшот службы.
            ENGINE_BACKUP="$(printf '%s' "$DETAIL" | jq -r '.engine_backup_id // ""')"
            if [ -n "$ENGINE_BACKUP" ]; then
                ok "путь: Backup API"
            elif [ "$(printf '%s' "$DETAIL" | jq -r '.type')" = snapshot ]; then
                if [ "$BACKUP_API" = true ]; then
                    warn "путь: временный снапшот, хотя у движка есть Backup API"
                else
                    ok "путь: временный снапшот — у движка нет Backup API"
                fi
            fi
            case "$TRANSFER" in
                *"целиком, остальные инкрементом:"*) ok "смешанный бэкап — ${TRANSFER##*целиком, остальные инкрементом: }" ;;
            esac
            if printf '%s' "$EVENTS" | jq -e '[.items[].kind] | any(. == "downgraded_full")' >/dev/null; then
                warn "инкремент снят полным: движок не принял смешанный бэкап (нужен oVirt 4.4.5+)"
            fi ;;
    esac
    case "$(printf '%s' "$DETAIL" | jq -r '.error // ""')" in
        *"место под временные файлы"*) bad "сработал сторож scratch на KVM" ;;
        *"домене хранения мало места"*) bad "сработала защита домена хранения oVirt" ;;
    esac

    if [ "$i" -ge 2 ] && [ "$TYPE" = incremental ] && [ "$STATUS" = succeeded ]; then
        FROM="$(printf '%s' "$DETAIL" | jq -r '.from_checkpoint_id // ""')"
        if [ "$(printf '%s' "$DETAIL" | jq -r '.type')" = incremental ] && [ -n "$FROM" ]; then
            ok "инкремент от checkpoint $FROM"
        else
            warn "второй запуск не инкремент: $(printf '%s' "$EVENTS" | jq -r '[.items[] | select(.kind == "run_started")] | first | .detail // ""')"
        fi
    fi
    say ""
done

say "итог: ошибок $FAILED, предупреждений $WARNINGS"
case "$KIND" in
    kvm) say "ротация checkpoint-ов: на хосте virsh checkpoint-list <имя ВМ> — служебных jhv-* должно быть не больше двух на хранилище" ;;
    ovirt|redvirt|olvm|rhv) say "ротация checkpoint-ов: GET /ovirt-engine/api/vms/<id>/checkpoints — лишние корневые checkpoint-ы службы должны исчезать" ;;
esac
say "отчёт: $REPORT"
[ "$FAILED" -eq 0 ]
