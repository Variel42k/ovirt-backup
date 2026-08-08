SHELL := /bin/bash

BINARY      := justhpc-virt-server
CMD         := ./cmd/justhpc-virt-server
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION)
GOFLAGS     := -trimpath

.DEFAULT_GOAL := help

.PHONY: help
help: ## Показать список целей
	@grep -hE '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-22s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## Собрать бинарь под текущую платформу
	CGO_ENABLED=0 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

.PHONY: build-linux
build-linux: ## Собрать статический бинарь под linux/amd64
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" \
		-o bin/$(BINARY)-linux-amd64 $(CMD)

.PHONY: build-all
build-all: build-linux ## Собрать бинари под linux/amd64 и linux/arm64
	CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build $(GOFLAGS) -ldflags "$(LDFLAGS)" \
		-o bin/$(BINARY)-linux-arm64 $(CMD)

.PHONY: run
run: ## Запустить сервер с конфигурацией по умолчанию
	go run $(CMD) -config config/virt-manager.yaml

.PHONY: test
test: ## Прогнать тесты
	go test ./...

.PHONY: test-race
test-race: ## Прогнать тесты с детектором гонок
	go test -race ./...

.PHONY: cover
cover: ## Тесты с отчётом о покрытии
	go test -coverprofile=coverage.out ./...
	go tool cover -func=coverage.out | tail -n 20

.PHONY: vet
vet: ## Статический анализ
	go vet ./...

.PHONY: fmt
fmt: ## Форматирование
	gofmt -w -s $(shell find . -name '*.go' -not -path './web/*')

.PHONY: check
check: fmt vet test ## Формат, анализ и тесты

.PHONY: tidy
tidy: ## Привести go.mod в порядок
	go mod tidy

.PHONY: web-install
web-install: ## Установить зависимости веб-интерфейса
	cd web && npm install --no-audit --no-fund

.PHONY: web-build
web-build: ## Собрать веб-интерфейс в web/dist
	cd web && npm run build

.PHONY: web-dev
web-dev: ## Запустить веб-интерфейс в режиме разработки (порт 9000)
	cd web && npm run dev

.PHONY: dist
dist: web-build build-linux ## Полная сборка: интерфейс + бинарь под Linux
	@echo "Готово: bin/$(BINARY)-linux-amd64 и web/dist"

.PHONY: docker
docker: ## Собрать docker-образ
	docker build --build-arg VERSION=$(VERSION) -t justhpc-virt-manager:$(VERSION) .

.PHONY: compose-up
compose-up: ## Поднять окружение через docker compose
	cd deploy && docker compose up -d --build

.PHONY: compose-s3
compose-s3: ## Поднять окружение вместе с MinIO
	cd deploy && docker compose --profile s3 up -d --build

.PHONY: compose-down
compose-down: ## Остановить окружение
	cd deploy && docker compose down

.PHONY: clean
clean: ## Удалить артефакты сборки
	rm -rf bin coverage.out web/dist
