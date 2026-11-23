# Полные имена базовых образов делают источник зависимостей явным и одинаковым
# для локальной и серверной сборки Docker.

# Сборка веб-интерфейса.
FROM docker.io/library/node:24-alpine@sha256:d32cdf619f63fe0471182d08996dd516c6275bb5fd31ae06e55a570bd9e1ad43 AS web
WORKDIR /build/web
COPY web/package.json web/package-lock.json* ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build:fast

# Сборка бинаря. CGO не нужен ни для чего, поэтому образ получается
# статическим и не тянет за собой libc сборочного дистрибутива.
FROM docker.io/library/golang:1.27.3-alpine@sha256:d200d2dd76b03a99b52651fd1325aff7dcd35422c35c01bac56571b0507f3deb AS go-base

FROM go-base AS frontend-build
WORKDIR /build
COPY go.mod ./
COPY cmd/jhvirt-frontend/ ./cmd/jhvirt-frontend/
COPY internal/webui/ ./internal/webui/
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags "-s -w" \
        -o /out/jhvirt-frontend ./cmd/jhvirt-frontend

FROM docker.io/library/alpine:3.24@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b AS alpine-base

FROM alpine-base AS frontend
RUN apk upgrade --no-cache && apk add --no-cache ca-certificates
WORKDIR /app
COPY --from=frontend-build /out/jhvirt-frontend /app/jhvirt-frontend
COPY --from=web /build/web/dist /app/web/dist
USER 10001:10001
EXPOSE 8080
ENTRYPOINT ["/app/jhvirt-frontend"]

FROM go-base AS build
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

FROM alpine-base AS runtime
# qemu-img нужен для совместимой QCOW2-цепочки oVirt 4.3, экспорта
# восстановленных образов в qcow2 и режима проверки «qemu-img check».
RUN apk upgrade --no-cache && \
    apk add --no-cache ca-certificates tzdata qemu-img \
        'libblkid>=2.42.3-r1' 'libmount>=2.42.3-r1' && \
    addgroup -g 10001 jhvirt && \
    adduser -u 10001 -G jhvirt -h /app -D jhvirt

WORKDIR /app
COPY --from=build /out/ovirt-backup-server /app/ovirt-backup-server
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
    CMD /app/ovirt-backup-server -healthcheck -config /app/config/ovirt-backup.yaml

ENTRYPOINT ["/app/ovirt-backup-server"]
CMD ["-supervise", "-config", "/app/config/ovirt-backup.yaml"]

FROM runtime AS backend
ENV JHV_SERVER_SERVE_SPA=false JHV_LOGGING_FILE=/app/data/logs/jhvirt.log

FROM runtime AS combined
COPY --from=web /build/web/dist /app/web/dist
