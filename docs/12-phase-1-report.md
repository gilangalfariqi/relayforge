# Phase 1 report — foundation

Date: 2026-10-22. Status: implemented and locally verified. No application delivery logic exists yet.

## 1. Scope delivered

- Git repository initialised on `main` (no commit created).
- Go 1.27.3 control-plane module with fail-fast configuration, JSON logging, request IDs, RFC 9457
  problems, body limits, graceful shutdown, liveness/readiness, metrics, optional OTLP tracing, and an
  embedded OpenAPI document.
- `cmd/migrate` using golang-migrate with the pgx/v5 driver and reversible migrations `0001`–`0005`.
- Rust 1.99.1 worker crate with validated configuration, SQLx pool, schema check, JSON tracing,
  health/readiness/metrics listener, readiness loop, and graceful shutdown.
- Multi-stage Dockerfiles for both services, running as UID/GID 65532.
- Compose stack: PostgreSQL 18.6, one-shot migrations, API, worker.
- Prometheus scrape configuration.
- GitHub Actions workflow for Go, Rust, migrations, image builds, and secret scanning, with actions pinned
  to commit SHAs.
- `Makefile`, `.gitignore`, `.gitattributes`, `.editorconfig`, and container ignore files.

## 2. Dependencies on completed phases

Phase 0 supplied the schema target, delivery contract, security decisions, and acceptance criteria. Phase 1
implements only the foundation; it does **not** implement tenancy, ingest, secrets, endpoint management,
delivery, SSRF enforcement, replay, or audit behaviour. Those remain Phase 2–4 work.

## 3. Files added

| Area | Files |
| --- | --- |
| Go entrypoints | `api/cmd/api/main.go`, `api/cmd/migrate/main.go` (+ test) |
| Go internals | `api/internal/config`, `db`, `httpapi`, `observability`, `version` |
| Go contract | `api/openapi/openapi.yaml`, `embed.go` (+ test) |
| Go module | `api/go.mod`, `api/go.sum` |
| Migrations | `api/migrations/0001`–`0005` (up and down each) |
| Rust | `worker/Cargo.toml`, `worker/Cargo.lock`, `worker/src/main.rs`, `config.rs`, `metrics.rs` |
| Rust tooling | `worker/scripts/check.sh`, `worker/scripts/install-tools.sh` |
| Containers | `api/Dockerfile`, `worker/Dockerfile`, `.dockerignore` files |
| Deployment | `deploy/compose.yaml`, `deploy/prometheus/prometheus.yml` |
| CI | `.github/workflows/ci.yml` |
| Repository | `Makefile`, `.gitignore`, `.gitattributes`, `.editorconfig` |
| Docs | this report; status updates in `README.md`, `docs/01-architecture.md`, `docs/08-implementation-plan.md` |

## 4. Commands and actual results

### Go

```
$ go version
go version go1.27.3 windows/amd64

$ gofmt -l .
(no output)

$ go build ./...
(exit 0)

$ go vet ./...
(exit 0)

$ go test -count=1 ./...
ok  .../cmd/migrate
ok  .../internal/config
ok  .../internal/httpapi
ok  .../openapi
```

Race detector, run in `golang:1.27.3` because the Windows host has no cgo compiler:

```
$ CGO_ENABLED=1 go test -race -count=1 ./...
ok  .../cmd/migrate
ok  .../internal/config
ok  .../internal/httpapi
ok  .../openapi
```

### Rust

Run in `rust:1.99.1-bookworm` after `rustup component add rustfmt clippy`:

```
cargo fmt --check           # pass
cargo clippy --all-targets --locked -- -D warnings   # pass, no warnings
cargo test --all --locked
running 7 tests
test result: ok. 7 passed; 0 failed
```

The release build succeeded inside the worker image build.

### Migrations (PostgreSQL 18.6)

```
$ go run ./cmd/migrate -path migrations up
schema version: 5 dirty=false

$ go run ./cmd/migrate -path migrations down
migrations rolled back

$ go run ./cmd/migrate -path migrations version
schema version: none

$ go run ./cmd/migrate -path migrations up
schema version: 5 dirty=false
```

`information_schema` reported 12 public tables (11 application tables plus `schema_migrations`);
`SELECT uuidv7()` returned a valid UUIDv7.

### Runtime checks

```
GET :8080/healthz  -> 200 {"status":"ok"}
GET :8080/readyz   -> 200 {"status":"ready"}
GET :9091/healthz  -> 200 {"status":"ok"}
GET :9091/readyz   -> 200 {"status":"ready"}
```

Negative readiness checks, using an expected schema version of 99:

```
API     /healthz -> 200, /readyz -> 503 not_ready
Worker  /healthz -> 200, /readyz -> 503
```

Admin token enforcement (API container with a token configured):

```
GET /metrics without X-Admin-Token -> 401
GET /metrics with the token        -> 200
GET /healthz without a token       -> 200 (probe remains open)
```

Compose:

```
relayforge-api-1       Up (healthy)   restarts=0
relayforge-worker-1    Up (healthy)   restarts=0
relayforge-postgres-1  Up (healthy)
relayforge-migrate-1   Exited (0)
```

Both application containers report `user=65532:65532`.

Graceful shutdown of the worker (`docker stop -t 20`) logged `shutdown requested`, `worker loop stopping`,
and `stopped`, then exited 0.

### Static / contract checks

```
redocly lint api/openapi/openapi.yaml
Woohoo! Your API description is valid. (2 warnings)

yq eval '.' .github/workflows/ci.yml        # exit 0
yq eval '.' deploy/compose.yaml             # exit 0
yq eval '.' deploy/prometheus/prometheus.yml # exit 0

gitleaks detect --source . --no-git
no leaks found
```

The two Redocly warnings are non-blocking: the dev server URL and an example-server rule. No GitHub
Actions run was executed.

## 5. Failures encountered and corrections

1. **Partial Go extraction.** `Expand-Archive` was interrupted by a command timeout, leaving a `go.exe`
   with incomplete standard-library sources (`net/url is not in std`). Re-extracted the same verified ZIP
   with `tar.exe`; `go version` and builds then succeeded.
2. **Host port 5432 unavailable.** Windows refused the bind. Compose now publishes PostgreSQL on
   `127.0.0.1:55432`; the Makefile default matches.
3. **PostgreSQL 18 volume path.** The 18+ image expects the mount at `/var/lib/postgresql`, not
   `/var/lib/postgresql/data`. The first container restart-looped; the mount and local volume were
   recreated.
4. **Listener validation rejected `:8080`.** The first validator required a non-empty host. It was
   corrected to accept an empty host (all interfaces) while still validating the port.
5. **Race detector needs cgo.** Windows has no C compiler, so `-race` runs in the pinned Go container.
6. **Rust image lacked rustfmt/clippy.** Components are installed by `worker/scripts/install-tools.sh`
   before checks; the check script sets the cargo PATH explicitly.
7. **Secret scan findings.** Gitleaks flagged example credentials in `docs/04-api-v1.md`. They were replaced
   with unmistakable placeholders. The published HMAC test vector remains in `contracts/signature-v1.md`.
8. **OpenAPI lint errors.** The spec lacked an explicit `security: []` and 4xx responses. Both were added.
9. **Configuration drift.** `docs/01-architecture.md` named `HTTP_ADDR`/`INTERNAL_ADDR`/`DATABASE_URL` and
   claimed sslmode validation that did not exist. The document now lists the implemented
   `RELAYFORGE_*` variables, and `config.Validate` now rejects non-TLS database connections in
   staging/production.

## 6. Security, consistency, and compatibility notes

- Tenant isolation, API keys, secrets, SSRF, replay, and audit are **not implemented**; their tables exist
  but no application code uses them. Database constraints are not a substitute for Phase 2 handlers.
- Database TLS is mandatory in staging/production. Local Compose uses `sslmode=disable` with development
  credentials bound to localhost. The development credentials are not production secrets.
- The public listener never exposes `/metrics`. Metrics are on the admin listener, bound to loopback in
  Compose; `X-Admin-Token` is enforced when configured.
- API keys and payloads are not logged. Startup logs contain no database URL or password (checked).
- Request IDs accept a bounded safe charset; invalid values are replaced.
- The API image is `scratch` with no shell; operational debugging requires a separate tool container.
- Migration `down` is destructive by design and intended for tests/local recovery only. Production is
  forward-only.
- The OpenAPI document describes only implemented endpoints, avoiding handler/spec drift.

## 7. Remaining limitations

- GitHub Actions has not run. CI syntax is valid, but a green badge would be an unverified claim.
- No native Rust toolchain on the Windows host. All Rust evidence comes from the pinned container.
- No integration or concurrency tests yet. Phase 1 tests are unit tests for config, routing, problems,
  admin auth, metrics, migration URL handling, and the embedded spec.
- OpenTelemetry is initialised in Go but no spans are created yet; the worker has JSON tracing only.
- The worker readiness loop does not claim or deliver work. This is intentional Phase 1 scope.
- Redocly reports two warnings; no rule was disabled.
- `gitleaks --no-git` scanned the working tree, not commit history, because nothing has been committed.
- No coverage percentage or benchmark is reported.

## 8. Acceptance status

Phase 1 acceptance criteria are met for local execution, with the explicit exception that the CI workflow
has not run on GitHub. The next phase is Phase 2 — tenancy, credentials, endpoints, ingest, idempotency,
and audit.
