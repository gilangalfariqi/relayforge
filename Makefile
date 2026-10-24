SHELL := /bin/sh
COMPOSE ?= docker compose -f deploy/compose.yaml
DATABASE_URL ?= postgres://relayforge:relayforge_local_only@localhost:55432/relayforge?sslmode=disable
GO ?= go
CARGO ?= cargo
VERSION ?= 1.0.0
COMMIT ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

.PHONY: help setup fmt fmt-check vet lint test test-go test-rust race integration build \
	up down logs ps migrate-up migrate-down migrate-version compose-config \
	check openapi-check demo bench-preload bench-explain bench-run clean

help:
	@echo "setup            download Go and Rust dependencies"
	@echo "fmt              format Go and Rust"
	@echo "check            fmt-check, vet, tests, clippy, compose validation"
	@echo "test             go test and cargo test"
	@echo "race             go test -race (requires cgo)"
	@echo "integration      PostgreSQL-backed Go integration tests (truncate local data)"
	@echo "build            build Go binaries and Rust worker"
	@echo "up / down        start or stop the local stack"
	@echo "demo             run end-to-end live walkthrough (ingest -> retry -> DLQ -> replay)"
	@echo "bench-preload    preload 1M events / 2M deliveries into PostgreSQL"
	@echo "bench-explain    capture EXPLAIN (ANALYZE, BUFFERS) for all named access paths"
	@echo "bench-run        run Phase 6 benchmark suite against the local stack"
	@echo "migrate-up/down  apply or roll back migrations against DATABASE_URL"
	@echo "logs / ps        inspect the local stack"

setup:
	cd api && $(GO) mod download
	cd worker && $(CARGO) fetch

fmt:
	cd api && gofmt -w .
	cd worker && $(CARGO) fmt

fmt-check:
	@test -z "$$(cd api && gofmt -l .)" || (cd api && gofmt -l . && exit 1)
	cd worker && $(CARGO) fmt --check

vet:
	cd api && $(GO) vet ./...

lint: vet
	cd worker && $(CARGO) clippy --all-targets -- -D warnings

test: test-go test-rust

test-go:
	cd api && $(GO) test ./...

test-rust:
	cd worker && $(CARGO) test --all

race:
	cd api && CGO_ENABLED=1 $(GO) test -race ./...

integration:
	cd api && $(GO) test -tags=integration -p 1 -count=1 ./internal/httpapi ./internal/store

build:
	cd api && $(GO) build -o ../bin/api ./cmd/api && $(GO) build -o ../bin/migrate ./cmd/migrate
	cd worker && $(CARGO) build --release --locked

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f --tail=100

ps:
	$(COMPOSE) ps

migrate-up:
	cd api && RELAYFORGE_DATABASE_URL="$(DATABASE_URL)" $(GO) run ./cmd/migrate -path migrations up

migrate-down:
	cd api && RELAYFORGE_DATABASE_URL="$(DATABASE_URL)" $(GO) run ./cmd/migrate -path migrations down

migrate-version:
	cd api && RELAYFORGE_DATABASE_URL="$(DATABASE_URL)" $(GO) run ./cmd/migrate -path migrations version

compose-config:
	$(COMPOSE) config --quiet

openapi-check:
	npx --yes @redocly/cli lint api/openapi/openapi.yaml

check: fmt-check vet test lint compose-config

demo:
	cd api && $(GO) run ./cmd/demo

bench-preload:
	docker exec -i relayforge-postgres-1 psql -U relayforge -d relayforge -v ON_ERROR_STOP=1 < bench/preload.sql

bench-explain:
	docker exec -i relayforge-postgres-1 psql -U relayforge -d relayforge -v ON_ERROR_STOP=1 < bench/explain.sql

bench-run:
	cd api && $(GO) run ./cmd/bench -db-url "$(DATABASE_URL)"

clean:
	rm -rf bin
	cd worker && $(CARGO) clean
