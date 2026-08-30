SHELL := /bin/sh

GO ?= go
NPM ?= npm
COMPOSE ?= docker compose
DATABASE_URL ?= postgres://gateway:gateway@localhost:5432/gateway?sslmode=disable
GOCACHE ?= $(CURDIR)/.cache/go-build
GOMODCACHE ?= $(CURDIR)/.cache/go-mod
TOOLS_BIN := $(CURDIR)/.tools/bin
MIGRATE := $(TOOLS_BIN)/migrate
MIGRATE_VERSION := v4.19.1
SQLC := $(TOOLS_BIN)/sqlc
SQLC_VERSION := v1.31.1
WEB_DIR := web

export DATABASE_URL
export GOCACHE
export GOMODCACHE

ifneq (,$(wildcard .env))
include .env
export
endif

.PHONY: bootstrap web-install dev dev-backend dev-web test test-backend test-web test-dev-supervisor \
	typecheck typecheck-backend typecheck-web format lint lint-backend lint-web \
	build build-backend build-web integration race bench generate \
	postgres-up postgres-down migrate-version migrate-up migrate-down-one

bootstrap: $(MIGRATE) $(SQLC) web-install
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

web-install:
	@command -v $(NPM) >/dev/null
	cd $(WEB_DIR) && $(NPM) ci

$(MIGRATE):
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) $(GO) install -tags postgres github.com/golang-migrate/migrate/v4/cmd/migrate@$(MIGRATE_VERSION)

$(SQLC):
	@mkdir -p $(TOOLS_BIN)
	GOBIN=$(TOOLS_BIN) $(GO) install github.com/sqlc-dev/sqlc/cmd/sqlc@$(SQLC_VERSION)

dev: postgres-up
	@/bin/sh scripts/dev-supervisor.sh "$(GO)" "$(NPM)" "$(WEB_DIR)"

dev-backend:
	$(GO) run ./cmd/gateway

dev-web:
	cd $(WEB_DIR) && $(NPM) run dev

test: test-backend test-web test-dev-supervisor

test-backend:
	$(GO) test ./...

test-web:
	cd $(WEB_DIR) && $(NPM) test

test-dev-supervisor:
	/bin/sh scripts/dev-supervisor_test.sh

typecheck: typecheck-backend typecheck-web

typecheck-backend: $(SQLC)
	$(GO) vet ./...
	cd db && $(SQLC) vet

typecheck-web:
	cd $(WEB_DIR) && $(NPM) run typecheck

format:
	gofmt -w $$(find cmd internal -type f -name '*.go')

lint: lint-backend lint-web

lint-backend:
	@test -z "$$(gofmt -l $$(find cmd internal -type f -name '*.go'))" || (echo 'Go files are not formatted; run make format' >&2; exit 1)
	$(GO) vet ./...

lint-web:
	cd $(WEB_DIR) && $(NPM) run lint

generate: $(SQLC)
	cd db && $(SQLC) generate

build: build-backend build-web

build-backend:
	$(GO) build ./...

build-web:
	cd $(WEB_DIR) && $(NPM) run build

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
