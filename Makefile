SHELL := /bin/sh

GO ?= go
COMPOSE ?= docker compose
DATABASE_URL ?= postgres://gateway:gateway@localhost:5432/gateway?sslmode=disable
GOCACHE ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod
TOOLS_BIN := $(CURDIR)/.tools/bin
MIGRATE := $(TOOLS_BIN)/migrate
MIGRATE_VERSION := v4.19.1
SQLC := $(TOOLS_BIN)/sqlc
SQLC_VERSION := v1.31.1

export DATABASE_URL
export GOCACHE
export GOMODCACHE

ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: bootstrap dev test typecheck format lint build integration race bench generate \
	postgres-up postgres-down migrate-version migrate-up migrate-down-one

bootstrap: $(MIGRATE) $(SQLC)
	@command -v $(GO) >/dev/null
	@$(GO) version | grep -q 'go1\.26' || (echo 'Go 1.26.7 or a newer Go 1.26 patch is required' >&2; exit 1)
	@command -v docker >/dev/null
	@$(COMPOSE) version >/dev/null
	$(GO) mod download
	@$(MIGRATE) -help >/dev/null 2>&1
	@$(SQLC) version >/dev/null 2>&1
	@echo 'golang-migrate $(MIGRATE_VERSION) installed at $(MIGRATE)'
	@echo 'sqlc $(SQLC_VERSION) installed at $(SQLC)'
	@if [ ! -f .env ]; then echo 'Create .env from .env.example and set its required values before make dev.'; fi

$(MIGRATE):
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) $(GO) install -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)

$(SQLC):
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)

dev: postgres-up
	$(GO) run ./cmd/gateway

test:
	$(GO) test ./...

typecheck: $(SQLC)
	$(GO) vet ./...
	cd db && $(SQLC) vet

format:
	gofmt -w $$(find cmd internal -type f -name '*.go')

lint:
	@test -z "$$(gofmt -l $$(find cmd internal -type f -name '*.go'))" || (echo 'Go files are not formatted; run make format' >&2; exit 1)
	$(GO) vet ./...

generate: $(SQLC)
	cd db && $(SQLC) generate

build:
	$(GO) build ./...

integration: postgres-up
	$(GO) test -tags=integration ./...

race:
	$(GO) test -race ./...

bench:
	$(GO) test -run '^$$' -bench=. ./...

postgres-up:
	$(COMPOSE) up -d --wait postgres

postgres-down:
	$(COMPOSE) down

migrate-version:
	$(MIGRATE) -database '$(DATABASE_URL)' -path db/migrations version

migrate-up:
	$(MIGRATE) -database '$(DATABASE_URL)' -path db/migrations up

migrate-down-one:
	$(MIGRATE) -database '$(DATABASE_URL)' -path db/migrations down 1
