# Тонкая обёртка над ./run — единственной реализацией.
#
# Makefile оставлен для тех, у кого make в пальцах, но дублировать в нём логику
# значит завести вторую реализацию, которая разойдётся с первой. Все цели
# делегируют.

.DEFAULT_GOAL := help
.PHONY: help build dev web test check fmt vet docker version clean clean-all

help:      ; @./run help
build:     ; @./run build $(ARGS)
dev:       ; @./run dev
web:       ; @./run web
test:      ; @./run test $(ARGS)
check:     ; @./run check
fmt:       ; @./run fmt
vet:       ; @./run vet
docker:    ; @./run docker
version:   ; @./run version
clean:     ; @./run clean
clean-all: ; @./run clean-all
