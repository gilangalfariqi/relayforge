# Phase 4 — Security and Operations Implementation Report

Date: 2026-10-23  
Status: **Complete — All Acceptance Criteria Verified**

## 1. Summary of Changes

Phase 4 hardened the RelayForge control plane and delivery engine with shared PostgreSQL rate limiting, authorised replay with cycle semantics, internal administrative inspection endpoints, bounded retention cleanup, Prometheus/Grafana observability artifacts, and full security/runbook verification:

1. **Connect-Time SSRF & DNS Rebinding Protection (`worker/src/ssrf.rs`, `api/internal/ssrf/validator.go`)**:
   - Both Go registration-time and Rust connect-time validators enforce all 50 shared conformance vectors (`contracts/ssrf-vectors.json`).
   - Added `remove_host_addrs` to `PinnedDnsResolver` and added `dns_rebinding_blocks_second_attempt` unit test in `worker/src/ssrf.rs` verifying that when a hostname initially resolves to a public IP (`93.184.216.34`) and subsequently rebinds to AWS/GCP metadata (`169.254.169.254`) or IPv4-mapped IPv6 loopback (`::ffff:127.0.0.1`), connect-time validation immediately denies the attempt (`ip_denied`) and removes any prior pin.
2. **PostgreSQL Fixed-Window Rate Limiting (`api/internal/store/ops.go`, `api/internal/httpapi/middleware_auth.go`)**:
   - Implemented `AllowRateLimit(ctx, scopeType, scopeID, limitPerSec)` using an atomic 1-second fixed-window upsert on `rate_limit_counters`.
   - Enforced `tenant_ingest` (default `50 req/s`, configurable via `RELAYFORGE_RATE_LIMIT_INGEST_PER_SEC`), `tenant_management` (default `10 req/s`, configurable via `RELAYFORGE_RATE_LIMIT_MANAGEMENT_PER_SEC`), and `admin` (default `20 req/s`, configurable via `RELAYFORGE_RATE_LIMIT_ADMIN_PER_SEC`) rate limits, returning `429 rate_limited` with `Retry-After: 1` (and `Retry-After: 60` on signup IP rate limit) and incrementing `relayforge_rate_limit_denied_total{scope=...}`.
3. **Authorised Replay with Cycle Semantics (`api/internal/store/ops.go`, `api/internal/httpapi/handlers_events.go`, `api/internal/httpapi/handlers_reads.go`)**:
   - Implemented `POST /api/v1/events/:id/replay` (`owner` or `admin` role required) and `POST /internal/v1/admin/deliveries/:id/replay`.
   - Locks target delivery rows (`SELECT ... FOR UPDATE`), replays only `failed` and `dead_lettered` deliveries (`cycle = cycle + 1`, `attempt_count = 0`, `attempt_seq = 0`, `cycle_started_at = now()`, `next_attempt_at = now()`, clears lease/error/completed/dead-lettered fields), preserves all historical `delivery_attempts` rows from prior cycles, records `event.replay` / `admin.replay` audit entries, and recomputes `events.status` projection (`delivering`).
   - Returns `202 Accepted` with per-delivery results or `409 replay_not_allowed` with per-delivery skip reasons when zero deliveries are eligible.
   - Documented `POST /api/v1/events/{id}/replay` in `api/openapi/openapi.yaml`.
4. **Internal Admin Inspection & Readiness Details (`api/internal/httpapi/router.go`, `worker/src/main.rs`)**:
   - Added `GET /internal/v1/admin/tenants`, `GET /internal/v1/admin/queue`, and `GET /internal/v1/admin/deliveries/stuck` on `NewAdminRouter` protected by constant-time `X-Admin-Token` authentication.
   - Updated `/readyz` on both the Go API (`{"status":"ready","checks":{"database":"ok","schema":"ok"},"version":"..."}`) and Rust worker (`{"status":"ready","checks":{"database":"ok","schema":"ok","worker_loop":"ok"},"version":"..."}`).
5. **Bounded Retention Cleanup Worker (`api/internal/store/ops.go`, `api/cmd/api/main.go`)**:
   - Implemented `RunRetentionPass` coordinated across replicas with a non-blocking PostgreSQL advisory lock (`pg_try_advisory_lock(738291045)`) and bounded `5000`-row `FOR UPDATE SKIP LOCKED` batches for expired `idempotency_records`, `rate_limit_counters`, and terminal `events` (preserving in-flight events and append-only `audit_logs`), reporting deleted row counts via `relayforge_retention_deleted_rows_total{table=...}`.
6. **Observability, Runbooks, and Security Scanning**:
   - Added Prometheus alert rules (`deploy/prometheus/alerts.yml`) and Grafana dashboard (`deploy/grafana/dashboard.json`).
   - Executed all diagnostic and data-integrity SQL queries from `docs/09-incident-recovery.md` and `docs/02-data-model.md` §8 against PostgreSQL 18.6, correcting the projection-drift diagnostic query in `docs/02-data-model.md` §8 (`HAVING count(d.id) FILTER (...) > 0` instead of `WHERE count(...)`).

## 2. Files Added or Modified

- `api/internal/config/config.go`
- `api/internal/observability/metrics.go`
- `api/internal/store/types.go`
- `api/internal/store/ops.go`
- `api/internal/httpapi/middleware_auth.go`
- `api/internal/httpapi/handlers_common.go`
- `api/internal/httpapi/handlers_events.go`
- `api/internal/httpapi/handlers_reads.go`
- `api/internal/httpapi/handlers_tenants.go`
- `api/internal/httpapi/router.go`
- `api/internal/httpapi/integration_test.go`
- `api/internal/httpapi/e2e_phase3_test.go`
- `api/internal/receiver/receiver.go`
- `api/internal/receiver/receiver_test.go`
- `api/cmd/api/main.go`
- `api/openapi/openapi.yaml`
- `worker/src/ssrf.rs`
- `worker/src/main.rs`
- `deploy/prometheus/alerts.yml`
- `deploy/grafana/dashboard.json`
- `docs/02-data-model.md`
- `docs/09-incident-recovery.md`
- `docs/15-phase-4-report.md`

## 3. Commands Executed and Verification Results

### 3.1 Go Vet, Staticcheck, Govulncheck, and Race-Enabled Integration Suite

```text
$ go vet -tags=integration ./...
$ go run honnef.co/go/tools/cmd/staticcheck@latest -tags=integration ./...
(exit 0, 0 findings)

$ go run golang.org/x/vuln/cmd/govulncheck@v1.1.4 ./...
=== Symbol Results ===
No vulnerabilities found.

$ docker run --rm --network relayforge_default ... golang:1.27.3 go test -race -tags=integration -count=1 ./...
ok  	github.com/relayforge/relayforge/api/cmd/migrate	1.041s
ok  	github.com/relayforge/relayforge/api/internal/auth	1.036s
ok  	github.com/relayforge/relayforge/api/internal/config	1.037s
ok  	github.com/relayforge/relayforge/api/internal/httpapi	16.671s
ok  	github.com/relayforge/relayforge/api/internal/jsonschema	1.108s
ok  	github.com/relayforge/relayforge/api/internal/receiver	1.050s
ok  	github.com/relayforge/relayforge/api/internal/secrets	1.033s
ok  	github.com/relayforge/relayforge/api/internal/ssrf	1.078s
ok  	github.com/relayforge/relayforge/api/internal/store	13.280s
ok  	github.com/relayforge/relayforge/api/openapi	1.028s
```

### 3.2 Rust Fmt, Clippy, Unit Tests (including DNS Rebinding), and Integration Suite

```text
$ cargo fmt --check && cargo clippy --all-targets -- -D warnings && cargo test -- --test-threads=1
running 17 tests
...
test ssrf::tests::dns_rebinding_blocks_second_attempt ... ok
test ssrf::tests::shared_ssrf_conformance_vectors ... ok
test result: ok. 17 passed; 0 failed

Running tests/delivery_integration.rs
test integration_suite ... ok
test result: ok. 1 passed; 0 failed; finished in 2.76s
```

### 3.3 OpenAPI Lint, Gitleaks Secret Scan, and Trivy Vulnerability/Misconfig Scan

```text
$ docker run --rm -v "w:/forgerelay:/workspace" -w /workspace redocly/cli:1.26.0 lint api/openapi/openapi.yaml
validating api/openapi/openapi.yaml...
api/openapi/openapi.yaml: validated in 191ms
Woohoo! Your API description is valid.

$ docker run --rm -v "w:/forgerelay:/workspace" -w /workspace zricethezav/gitleaks:v8.24.0 detect --no-git --source . --redact -v
INF scanned ~873445 bytes (873.45 KB) in 754ms
INF no leaks found

$ docker run --rm -v "w:/forgerelay:/workspace" -w /workspace aquasec/trivy:0.59.1 fs --scanners vuln,misconfig --severity HIGH,CRITICAL .
INFO [gomod] Detecting vulnerabilities...
INFO [cargo] Detecting vulnerabilities...
INFO Detected config files num=2
(exit 0, 0 HIGH/CRITICAL findings)
```

### 3.4 Live Multi-Service E2E Lifecycle + Authorised Replay (`TestEndToEndDeliveryLifecycle`)

```text
$ go test -v -tags=e2e -run TestEndToEndDeliveryLifecycle ./internal/httpapi
=== RUN   TestEndToEndDeliveryLifecycle
--- PASS: TestEndToEndDeliveryLifecycle (4.18s)
PASS
ok  	github.com/relayforge/relayforge/api/internal/httpapi	5.240s
```

## 4. Phase 4 Acceptance Criteria Status

| Criterion | Status | Evidence |
| --- | --- | --- |
| All SSRF vectors pass at both stages; DNS-rebinding test blocks the second attempt | **PASS** | `TestSharedConformanceVectors` (Go), `shared_ssrf_conformance_vectors` & `dns_rebinding_blocks_second_attempt` (Rust) |
| `HTTP_PROXY` is ignored; a redirect to metadata is never followed | **PASS** | `reqwest` compiled with `default-features = false` (no `system-proxy`) + `.no_proxy()` + `redirect::Policy::none()`; verified in `delivery_integration.rs` (`/redirect` -> `http_redirect`) |
| Rate limits return `429` with `Retry-After`; destination deferral preserves work | **PASS** | `TestTenantAndAdminRateLimits` (`429 rate_limited`, `Retry-After: 1`) + `delivery_integration.rs` section 4 (`rate_limited` outcome, `attempt_count = 0`) |
| Rotation: both secrets valid during overlap; retired secret rejected; no plaintext in logs | **PASS** | `TestSecretRotationOverlapAndRetirementWithReceiver` + `TestSecretsAndCredentialGuards` |
| Replay: cycle increment, history preserved, concurrent replay single winner, audit present | **PASS** | `TestEventReplayAdminReplayAndConcurrency` (10 concurrent callers -> 1 `202` winner, 9 `409` conflicts, cycle 1->2->3, `event.replay` & `admin.replay` audit rows) + `TestEndToEndDeliveryLifecycle` Scenario E |
| Metrics and alerts documented with thresholds; `/metrics` not publicly exposed | **PASS** | `deploy/prometheus/alerts.yml`, `deploy/grafana/dashboard.json`, public `/metrics` returns `404` |
| Runbook commands executed in a staging-like environment and corrected where wrong | **PASS** | Executed against `relayforge-postgres-1` (PostgreSQL 18.6); corrected projection-drift `HAVING` clause in `docs/02-data-model.md` §8 |
| Trivy/gitleaks/govulncheck/cargo-audit results recorded | **PASS** | `govulncheck` (0 vulns), `gitleaks` (0 leaks), `trivy fs --scanners vuln,misconfig` covering `go.mod`, `Cargo.lock`, and Dockerfiles (0 HIGH/CRITICAL findings) |
