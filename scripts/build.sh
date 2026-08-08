#!/bin/sh
# Локальная сборка justhpc-virt-manager.
#
# Собирает веб-интерфейс и оба бинаря и складывает всё в комплект, готовый к
# переносу на сервер: dist/jhvirt-<версия>-<ос>-<арх>.tar.gz со скриптом
# установки внутри.
#
# Скрипт на POSIX sh и без make: make есть не везде (Windows, минимальные
# образы CI), а собирать проект должно быть можно везде, где есть Go и Node.
# Cgo не используется, поэтому кросс-компиляция работает с любой платформы:
# собрать Linux-бинарь из Windows — обычный сценарий, а не исключение.
#
# Использование:
#   scripts/build.sh                      сборка под текущую платформу
#   scripts/build.sh --target linux/amd64 кросс-сборка
#   scripts/build.sh --version 1.2.0      явная версия
#   scripts/build.sh --skip-web           только бинари (web/dist уже собран)
#   scripts/build.sh --no-archive         разложить в dist/, не паковать

set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

TARGET=""
VERSION=""
SKIP_WEB=0
NO_ARCHIVE=0

die() { printf 'ошибка: %s\n' "$*" >&2; exit 1; }
say() { printf '%s\n' "$*"; }

while [ $# -gt 0 ]; do
    case "$1" in
        --target)     TARGET="${2:-}"; shift 2 ;;
        --version)    VERSION="${2:-}"; shift 2 ;;
        --skip-web)   SKIP_WEB=1; shift ;;
        --no-archive) NO_ARCHIVE=1; shift ;;
        -h|--help)    sed -n '2,26p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *)            die "неизвестный аргумент: $1 (см. --help)" ;;
    esac
done

# --- Проверки окружения -----------------------------------------------------
#
# Проверяем до начала работы: узнать об отсутствии Node после четырёх минут
# компиляции Go — потерянные четыре минуты.

command -v go >/dev/null 2>&1 || die "не найден go — нужен Go 1.26+ (https://go.dev/dl/)"

GO_VERSION="$(go env GOVERSION 2>/dev/null || echo unknown)"
say "Go:      $GO_VERSION"

if [ "$SKIP_WEB" -eq 0 ]; then
    command -v npm >/dev/null 2>&1 || \
        die "не найден npm — нужен Node 20+, либо соберите с --skip-web, если web/dist уже готов"
    say "Node:    $(node --version 2>/dev/null || echo unknown)"
fi

# --- Версия -----------------------------------------------------------------
#
# Версия попадает в бинарь и в имя архива. Из git, если он есть; иначе dev.
# Суффикс -dirty у git describe означает несохранённые правки — оставляем его
# видимым, чтобы «та самая сборка» не путалась с похожей.

if [ -z "$VERSION" ]; then
    if command -v git >/dev/null 2>&1 && git -C "$ROOT" rev-parse --git-dir >/dev/null 2>&1; then
        VERSION="$(git -C "$ROOT" describe --tags --always --dirty 2>/dev/null || echo dev)"
    else
        VERSION="dev"
    fi
fi

# --- Платформа --------------------------------------------------------------

if [ -n "$TARGET" ]; then
    GOOS="${TARGET%/*}"
    GOARCH="${TARGET#*/}"
    [ "$GOOS" != "$TARGET" ] || die "цель задаётся как ос/арх, например linux/amd64"
else
    GOOS="$(go env GOOS)"
    GOARCH="$(go env GOARCH)"
fi

say "версия:  $VERSION"
say "цель:    $GOOS/$GOARCH"
say ""

SUFFIX=""
[ "$GOOS" = "windows" ] && SUFFIX=".exe"

NAME="jhvirt-$VERSION-$GOOS-$GOARCH"
OUT="$ROOT/dist/$NAME"
rm -rf "$OUT"
mkdir -p "$OUT/bin" "$OUT/config" "$OUT/web" "$OUT/systemd" "$OUT/docs"

# --- Веб-интерфейс ----------------------------------------------------------

if [ "$SKIP_WEB" -eq 0 ]; then
    say "==> сборка веб-интерфейса"
    if [ ! -d "$ROOT/web/node_modules" ]; then
        say "    зависимости не установлены, ставим"
        (cd "$ROOT/web" && npm ci --no-audit --no-fund) || \
            (cd "$ROOT/web" && npm install --no-audit --no-fund)
    fi
    (cd "$ROOT/web" && npm run build)
else
    say "==> веб-интерфейс пропущен (--skip-web)"
fi

[ -f "$ROOT/web/dist/index.html" ] || \
    die "нет web/dist/index.html — интерфейс не собран; уберите --skip-web"

# --- Бинари -----------------------------------------------------------------
#
# CGO_ENABLED=0 не оптимизация, а требование: драйвер SQLite здесь на чистом Go,
# и без cgo бинарь получается статическим — его можно положить в любой
# дистрибутив, не думая о версии libc.

say "==> сборка бинарей"
LDFLAGS="-s -w -X main.version=$VERSION"

CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -trimpath -ldflags "$LDFLAGS" \
    -o "$OUT/bin/justhpc-virt-server$SUFFIX" ./cmd/justhpc-virt-server
say "    justhpc-virt-server$SUFFIX"

CGO_ENABLED=0 GOOS="$GOOS" GOARCH="$GOARCH" \
    go build -trimpath -ldflags "$LDFLAGS" \
    -o "$OUT/bin/jvbackup$SUFFIX" ./cmd/jvbackup
say "    jvbackup$SUFFIX"

# --- Комплект ---------------------------------------------------------------

cp -r "$ROOT/web/dist" "$OUT/web/dist"
cp "$ROOT/config/virt-manager.yaml" "$OUT/config/"
cp "$ROOT/scripts/install.sh" "$OUT/install.sh"
cp "$ROOT/deploy/systemd/jhvirt.service" "$OUT/systemd/"
for doc in DEPLOY OPERATIONS BUILD; do
    [ -f "$ROOT/docs/$doc.md" ] && cp "$ROOT/docs/$doc.md" "$OUT/docs/"
done
cp "$ROOT/README.md" "$OUT/docs/"
chmod +x "$OUT/install.sh" "$OUT/bin/"* 2>/dev/null || true

printf '%s\n' "$VERSION" > "$OUT/VERSION"

# --- Архив ------------------------------------------------------------------

say ""
if [ "$NO_ARCHIVE" -eq 1 ]; then
    say "готово: dist/$NAME/"
else
    # Бинари упаковываются отдельным проходом с явным режимом 0755.
    #
    # Бит исполнения не хранится на NTFS, поэтому chmod при сборке из Windows
    # ничего не даёт и в архив бинарь попадает как 0644 — распакованный на
    # сервере он не запустится. Права в комплекте не должны зависеть от того,
    # на какой системе его собирали.
    (
        cd "$ROOT/dist"
        tar cf "$NAME.tar" --exclude="$NAME/bin" --exclude="$NAME/install.sh" "$NAME"
        tar rf "$NAME.tar" --mode=0755 "$NAME/bin" "$NAME/install.sh"
        rm -f "$NAME.tar.gz"
        gzip -9 "$NAME.tar"
    )
    SIZE="$(du -h "$ROOT/dist/$NAME.tar.gz" 2>/dev/null | cut -f1 || echo '?')"
    say "готово: dist/$NAME.tar.gz ($SIZE)"
    say ""
    say "Установка на сервере:"
    say "  scp dist/$NAME.tar.gz server:/tmp/"
    say "  ssh server 'tar xzf /tmp/$NAME.tar.gz -C /tmp && sudo /tmp/$NAME/install.sh'"
fi
