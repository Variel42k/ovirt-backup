# Полные имена базовых образов делают источник зависимостей явным и одинаковым
# для локальной и серверной сборки Docker.

# Сборка веб-интерфейса.
FROM docker.io/library/node:22-alpine AS web
WORKDIR /build/web
COPY web/package.json web/package-lock.json* ./
RUN npm ci --no-audit --no-fund || npm install --no-audit --no-fund
COPY web/ ./
RUN npm run build:fast

# Сборка бинаря. CGO не нужен ни для чего, поэтому образ получается
# статическим и не тянет за собой libc сборочного дистрибутива.
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /build
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X main.version=${VERSION}" \
        -o /out/justhpc-virt-server ./cmd/justhpc-virt-server

FROM docker.io/library/alpine:3.21
# qemu-img нужен только для экспорта восстановленных образов в qcow2 и для
# режима проверки «qemu-img check»; всё остальное работает без него.
RUN apk add --no-cache ca-certificates tzdata qemu-img && \
    addgroup -g 10001 jhvirt && \
    adduser -u 10001 -G jhvirt -h /app -D jhvirt

WORKDIR /app
COPY --from=build /out/justhpc-virt-server /app/justhpc-virt-server
COPY --from=web /build/web/dist /app/web/dist
COPY config/virt-manager.yaml /app/config/virt-manager.yaml

# Ключ шифрования секретов и временные файлы восстановления; при локальном
# хранилище — сами копии. База живёт снаружи, в PostgreSQL.
RUN mkdir -p /app/data /app/logs /backups && chown -R jhvirt:jhvirt /app /backups
VOLUME ["/app/data", "/backups"]

USER jhvirt
EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -qO- http://127.0.0.1:8080/healthz || exit 1

ENTRYPOINT ["/app/justhpc-virt-server"]
CMD ["-config", "/app/config/virt-manager.yaml"]
