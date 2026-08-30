# Полные имена базовых образов делают источник зависимостей явным и одинаковым
# для локальной и серверной сборки Docker.

# Сборка веб-интерфейса.
FROM docker.io/library/node:24-alpine@sha256:d32cdf619f63fe0471182d08996dd516c6275bb5fd31ae06e55a570bd9e1ad43 AS web
WORKDIR /build/web
COPY web/package.json web/package-lock.json* ./
RUN npm ci --no-audit --no-fund || npm install --no-audit --no-fund
COPY web/ ./
RUN npm run build:fast

# Сборка бинаря. CGO не нужен ни для чего, поэтому образ получается
# статическим и не тянет за собой libc сборочного дистрибутива.
FROM docker.io/library/golang:1.27-alpine@sha256:4c9fe60190a2a3350ddc51de80d0224b8a6698d12bdfc999fee45ea9d6c46dbc AS build
WORKDIR /build
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/ovirt-backup-server ./cmd/ovirt-backup-server

FROM docker.io/library/alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
# qemu-img нужен только для экспорта восстановленных образов в qcow2 и для
# режима проверки «qemu-img check»; всё остальное работает без него.
RUN apk upgrade --no-cache && \
    apk add --no-cache ca-certificates tzdata qemu-img && \
    addgroup -g 10001 jhvirt && \
    adduser -u 10001 -G jhvirt -h /app -D jhvirt

WORKDIR /app
COPY --from=build /out/ovirt-backup-server /app/ovirt-backup-server
COPY --from=web /build/web/dist /app/web/dist
COPY config/ovirt-backup.yaml /app/config/ovirt-backup.yaml

# Ключ шифрования секретов и временные файлы восстановления; при локальном
# хранилище — сами копии. База живёт снаружи, в PostgreSQL.
# /app/logs здесь не создаётся намеренно: каталог лежал бы в слое контейнера и
# исчезал при каждом пересоздании вместе со всей историей, а при read_only
# оказался бы ещё и недоступен для записи. Журнал пишется в /app/data/logs.
RUN mkdir -p /app/data /backups && chown -R jhvirt:jhvirt /app /backups
VOLUME ["/app/data", "/backups"]

USER jhvirt
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/ovirt-backup-server"]
CMD ["-config", "/app/config/ovirt-backup.yaml"]
