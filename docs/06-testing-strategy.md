# Testing strategy

Status: Implemented and verified through Phase 7. Last updated: 2026-10-24.
Executed evidence across all layers (unit, PostgreSQL integration, OpenAPI contract, Rust delivery engine,
SSRF/tenant-isolation/redaction/SQLi regression, fuzz/property testing, race detector, 10x concurrency
stability, SIGKILL lease recovery drill, and full E2E Compose suite) is recorded in
[12-phase-1-report.md](12-phase-1-report.md) through [18-phase-7-report.md](18-phase-7-report.md).

## 1. Principles

1. Test the failure windows, not only the happy path.
2. Use a real PostgreSQL 18.6 for locking, constraints, and transactions. Mocking `SKIP LOCKED` proves
   nothing.
3. Prefer deterministic coordination (barriers, channels, SQL state manipulation) over sleeps.
4. Every security claim in [the threat model](05-threat-model.md) has a corresponding test.
5. Tests that cannot run in an environment are reported as **not run**, never as passing.
6. Test data is synthetic. No real PII, keys, or secrets in fixtures.
7. A test that only asserts a status code is insufficient for stateful behaviour; assert database state.

## 2. Test layers

| Layer | Scope | Tooling | Runtime |
| --- | --- | --- | --- |
| Unit | Pure policy, validation, crypto helpers, pagination cursors, URL/IP classification | Go `testing`, Rust `#[test]`, `proptest` | Seconds |
| Component/DB integration | Repositories, transactions, constraints, claim/lease/reaper | Go tests + real PG, `#[sqlx::test]` | 1–3 min |
| API contract | Handler ↔ OpenAPI, error envelope, status codes, auth matrix | `httptest`, `kin-openapi`, golden JSON | < 1 min |
| Delivery contract | State transitions, classification, backoff, signature | Rust tests + DB + stub HTTP server | < 1 min |
| Security regression | SSRF vectors, tenant isolation, redaction, size limits, rate limits | Both suites | < 1 min |
| End-to-end | API + PG + worker + demo receiver | Docker Compose + Go test driver | 3–5 min |
| Load/performance | Throughput, latency, queue behaviour | k6 + Prometheus + `pg_stat_statements` | 20–40 min |
| Fuzz/property | Parser and policy edge cases | Go fuzz, `cargo-fuzz`, `proptest` | CI budgeted |

## 3. Environment and database isolation

### 3.1 Local and CI

- PostgreSQL image: `postgres:18.6-alpine`, pinned by tag in Compose and CI. The CI service container and
  the Compose service use the same major version.
- Go and Rust integration tests read `RELAYFORGE_TEST_DATABASE_URL` and share one PostgreSQL database;
  there is no per-package container and no per-test database. CI provides the URL from a service container.
- Both suites acquire PostgreSQL advisory lock `918273645` on a dedicated connection and truncate the domain
  tables before running, so they cannot run concurrently. Execute Go database tests with `-p 1`. Do not
  point them at a database containing data you need.
- Go tests that cannot run in a transaction (claim concurrency, lease expiry, `SKIP LOCKED`) still use the
  shared database, so they assert against rows they created and must leave them in a known state.
- Rust integration tests (`worker/tests/delivery_integration.rs`) take the same advisory lock and truncate
  before each case. They skip with a printed message when the database is unreachable, so `cargo test`
  remains usable without PostgreSQL.
- The migrations job runs `up`, `down`, `up` before either integration suite. Schema correctness is proven
  on an empty database by that sequence, not by a fresh database per test.
- Within a Go package, tests do not depend on execution order. Cross-package parallelism is disabled only
  for the database-backed packages that run in CI.

### 3.2 Time control

- The retry/backoff policy is a pure function of `(attempt, policy, retry_after, now)`. Unit tests inject
  `now` and the jitter source; no clock mocking of PostgreSQL is needed.
- DB integration tests manipulate `next_attempt_at`, `lease_expires_at`, and `cycle_started_at` directly
  with SQL to place rows in the desired state. This is deterministic and faster than sleeping.
- A single end-to-end test verifies that real `now()` scheduling moves work forward; it polls with a
  bounded deadline (e.g. 30 s) and a 100 ms interval, and fails with a diagnostic dump on timeout.

### 3.3 Receiver control

The test receiver exposes a control channel (an in-process channel in Go tests; a small admin endpoint in
E2E) that selects the next response:

- `200` with body
- `204`
- `500` N times
- `429` with `Retry-After`
- `401`
- `302` to a private address
- delay of D ms before headers/body
- close connection mid-response
- oversized response body

The receiver records every request (event ID, timestamp, signature, body hash, headers) in memory and in a
SQLite/PostgreSQL table for E2E assertions. It never performs real outbound network calls in unit tests.

## 4. Required test matrix

### 4.1 Tenancy, auth, credentials

| Test | Type | Assertion |
| --- | --- | --- |
| Key format and uniqueness | unit | 100k keys unique, correct prefix, 256-bit secret |
| Digest-only storage | integration | DB contains no plaintext key; `secret_hmac` is 32 bytes |
| Constant-time compare helper | unit | Equal/unequal cases; implementation uses `hmac.Equal` |
| Missing/invalid/expired/revoked key | API | `401`, generic body, no existence leak |
| Role matrix | API | Every role × every endpoint; default deny |
| Cross-tenant read/write matrix | integration | Every resource type returns `404`, never data |
| Suspend tenant | integration | All requests `403 tenant_suspended`; no writes |
| Last owner revocation | integration | `409 last_owner_credential` |
| Rotation overlap | integration | Old and new keys valid during overlap; old invalid after expiry |
| Audit records | integration | Actor, action, resource, request ID present; no secrets |

### 4.2 Ingestion and idempotency

| Test | Type | Assertion |
| --- | --- | --- |
| Valid event | API | `201`, event + N deliveries in one tx |
| Missing idempotency key | API | `400 idempotency_key_required` |
| Same key + same body × 100 concurrent | concurrency | Exactly one event; all responses identical; `Idempotency-Replayed` on losers |
| Same key + different body | API | `409 idempotency_key_reused`; first event unchanged |
| Concurrent key contention | concurrency | Unique index serialises; no duplicate rows; timeout path returns `409 idempotency_in_progress` |
| Oversized body | API | `413`; connection not left half-read; no partial event |
| Invalid JSON / trailing data / unknown field | API | `400`/`422`; no rows |
| Deeply nested `data` | API | Rejected at depth limit; no stack overflow |
| Payload size boundary | API | 256 KiB accepted, 256 KiB + 1 rejected |
| Missing matching endpoints | integration | Event `no_subscribers`, zero deliveries, no error |
| Fan-out 50 endpoints | integration | Exactly 50 deliveries; 51st subscription prevented |
| Transaction rollback | fault injection | Failure after event insert leaves zero events/deliveries/idempotency rows |
| Schema validation valid/invalid | integration | `201` / `422` with JSON pointer; no event on failure |
| `wire_body` stability | integration | Stored bytes equal the request bytes; retries send identical bytes |

The 100-concurrent test uses a start barrier and a shared key. It does not sleep; it synchronises
goroutines with `sync.WaitGroup` and asserts after all return.

### 4.3 Delivery engine

| Test | Type | Assertion |
| --- | --- | --- |
| Claim concurrency | DB concurrency | 10 workers × 100 rows: no duplicate delivery IDs, one attempt row each |
| Claim respects `next_attempt_at` | integration | Future rows are not claimed |
| Lease expiry | integration | Forced expiry: attempt `abandoned`, retry scheduled or dead-lettered |
| Stale completion | integration | Old token updates 0 rows; no attempt outcome overwrite |
| Success path | E2E | 2xx → `delivered`; event `delivered`; metrics increment |
| 3xx | E2E | Not followed; `failed` with `http_redirect` |
| 4xx permanent | E2E | 400/401/403/404/409/413/422 → `failed` |
| 408/425/429 | E2E | `retry_scheduled`; 429 honours `Retry-After` |
| 5xx transient | E2E | 500/502/503/504/507 retried; 501/505/508 permanent |
| Timeout | E2E | Slow receiver → `timeout`; retry scheduled |
| DNS failure | integration | `dns_error`, retryable |
| Connection refused | E2E | `connect_error`, retryable |
| TLS verification failure | E2E | `tls_error`, permanent |
| Backoff bounds | property | Delay within documented range for attempts 1..20; cap enforced |
| Retry window | integration | Old `cycle_started_at` → dead-letter, not infinite retry |
| Max attempts | integration | Attempt 8 failure → `dead_lettered`; no 9th claim |
| Rate-limit deferral | integration | `attempt_count` unchanged, `attempt_seq` advanced, attempt `rate_limited` |
| Per-destination concurrency | load | Never exceeds configured cap |
| Graceful shutdown | E2E | SIGTERM stops claims, releases unstarted work, exits within grace |
| SIGKILL recovery | E2E | Lease expires, reaper retries, attempt `abandoned` |
| No HTTP inside transaction | static + integration | `pg_stat_activity` shows no long idle-in-tx during slow receiver |
| Event projection | integration | Mixed outcomes produce correct event status |
| Projection race | concurrency | Two deliveries finishing simultaneously serialise on event row; final status correct |
| Duplicate acceptance | E2E | Receiver processes same event twice; dedupe table has one row; second gets 2xx |

### 4.4 Signatures and security

| Test | Type | Assertion |
| --- | --- | --- |
| Published vector | unit (Go + Rust) | Exact signature matches `signature-v1.md` |
| Tampered body | unit | Verification fails |
| Tampered timestamp | unit | Verification fails |
| Wrong secret | unit | Verification fails |
| Stale/future timestamp | unit | ±300 s boundary: 300 accepted, 301 rejected |
| Multiple signatures | unit | Any valid `v1` accepted; unknown scheme ignored |
| Rotation overlap | E2E | Both secrets produce valid signatures; retired secret rejected |
| Missing secret | integration | `no_active_secret`, permanent, replayable |
| SSRF conformance vectors | unit (both) | Every vector's decision matches; delivery DNS error is `retry`, not `blocked` |
| DNS rebinding | integration | Controlled resolver returns public then private; second attempt blocked |
| Redirect to metadata | E2E | 3xx is a failure; no request to the redirect target |
| Proxy env ignored | unit/E2E | `HTTP_PROXY` set but no proxy connection is made |
| Oversized response | E2E | Only 8 KiB read; no memory growth; status used |
| Log redaction | integration | Canary key/secret/signature/body never appears in logs |
| Error redaction | API | No SQL/stack/secret in `detail` |
| SQL injection corpus | API | Filters/cursor/type with payloads return `400`, no data leak |
| Rate limit burst | API | 429 with `Retry-After`; headers consistent |

### 4.5 Replay

| Test | Type | Assertion |
| --- | --- | --- |
| Replay failed delivery | E2E | `202`, cycle 2, new attempts start at 1, old attempts retained |
| Replay delivered delivery | API | `409 replay_not_allowed` |
| Replay in-progress | API | Skipped; retry allowed after terminal |
| Concurrent replay × 20 | concurrency | Exactly one cycle increment; others `409`/skipped |
| Replay other tenant | API | `404` |
| Replay authorisation | API | `viewer`/`ingest` forbidden |
| Replay re-runs SSRF/rate limit | integration | Blocked destination fails again; limiter applies |
| Audit | API | Reason, actor, previous/new cycle recorded |

### 4.6 API quality

| Test | Type | Assertion |
| --- | --- | --- |
| OpenAPI validation | contract | Every handler request/response validates against the spec |
| Error envelope | contract | RFC 9457 fields present; stable `code` |
| Pagination | DB | No duplicates/skips across pages under concurrent inserts (documented snapshot behaviour) |
| Limit boundaries | API | 0, 101 rejected; 1/100 accepted |
| Filter validation | API | Unknown params/types/ranges rejected |
| Cursor tampering | API | Invalid cursor/filter hash → `400` |
| Request ID | API | Generated/echoed, bounded charset |

## 5. End-to-end scenario

Compose stack: `postgres`, `api`, `worker`, `receiver`. The Go E2E driver:

1. Creates a tenant, endpoint, and secret.
2. Configures the receiver to fail the first two attempts with `503`, then succeed.
3. Ingests an event with an idempotency key and repeats the request; asserts one event and identical IDs.
4. Waits (bounded, polling) for attempt 1/2 `retryable_failure`, attempt 3 `success`, delivery `delivered`.
5. Verifies the receiver's signature checks and dedupe table; asserts one business effect.
6. Forces a dead letter (receiver `500` always), replays with an authorised key, asserts cycle 2 and history.
7. Registers `https://169.254.169.254/` and asserts `422`; registers a controlled hostname that resolves
   to a private address (test resolver) and asserts delivery `destination_blocked`.
8. Kills the worker with `SIGKILL` during a slow delivery; restarts it; asserts lease recovery and an
   `abandoned` attempt.
9. Asserts Prometheus metrics and `/readyz` after each stage.

The driver must produce a diagnostic dump (DB rows for the tenant, worker logs) on failure.

## 6. Fuzzing and property-based tests

| Target | Technique | Property |
| --- | --- | --- |
| Event envelope decoder | Go `Fuzz` | No panic, bounded allocation, either valid DTO or typed error |
| Idempotency key | Go `Fuzz` | Accepted keys match regex; no SQL leakage; bounded length |
| Cursor decoder | Go `Fuzz` | No panic; tampered input rejected |
| URL/IP policy | Rust `cargo-fuzz` or `proptest` | No panic; every accepted URL has all-global IPs; denied classes never accepted |
| Backoff | `proptest` | Monotonic cap, jitter bounds, no overflow for attempt ≤ 1000 |
| Status classification | `proptest` | Total function for 100–599; only documented statuses retry |
| Signature verification | `proptest` | Any one-byte body/timestamp change fails; valid vector passes |

Fuzz corpora and crash artifacts are committed (small) or uploaded as CI artifacts. A fuzz target that
finds a panic is a release blocker.

## 7. Concurrency and failure-injection techniques

- **Start barriers** for simultaneous claims/replays/ingests.
- **SQL state injection** to expire leases, age retries, and force window exhaustion.
- **Lease token capture** to simulate a stale worker: read the token, let the reaper expire the row, then
  attempt completion with the old token.
- **Fault injection at the DB boundary** (Go interface wrapper) to fail after the event insert and before
  job insert, and after HTTP completion before commit.
- **Real `SIGKILL`** for at least one E2E worker crash; `SIGTERM` for graceful shutdown.
- **No sleeps for correctness.** Bounded polling is allowed only to observe asynchronous progress, with a
  clear failure message and a timeout.

## 8. CI jobs and evidence

`.github/workflows/ci.yml` defines these jobs. Every job is blocking; a job that is skipped is shown as
skipped in the run summary and is never reported as passed.

| Job | Commands | Notes |
| --- | --- | --- |
| Go control plane | `gofmt -l`, `go vet ./...`, `go build ./...`, `staticcheck` v0.8.2, `go test -race -count=1 ./...`, `govulncheck` v1.9.0 | No database |
| Rust worker | `cargo fmt --all -- --check`, `cargo clippy --all-targets --locked -- -D warnings`, `cargo test --all --locked` | No database; integration tests print a skip |
| PostgreSQL and integration | `migrate up`, `migrate version`, `migrate down`, `migrate up`, `go test -tags=integration -p 1 ./internal/httpapi ./internal/store`, `cargo test --test delivery_integration` | `postgres:18.6-alpine` service container |
| Container images | `docker compose config --quiet`, `docker build ./api`, `docker build ./worker` | Images are built, not pushed |
| End-to-end delivery | Compose `up -d --build`, readiness probe loop, `go test -tags=e2e -run '^TestEndToEndDeliveryLifecycle$'` | Logs dumped on failure; stack removed afterwards |
| Secret scan | `gitleaks/gitleaks-action` on full history | Requires no stored token |

Publish (`publish.yml`) is separate from CI: on `main` and `v*` tags it builds `linux/amd64` API, worker,
and receiver images and pushes them to GHCR with an SBOM and provenance.

First execution: commit `a95cf07` on 2026-10-24 passed all six CI jobs in
[run 40077250780](https://github.com/gilangalfariqi/relayforge/actions/runs/40077250780), and the three
publish jobs in [run 40077250807](https://github.com/gilangalfariqi/relayforge/actions/runs/40077250807).

Not in CI, and therefore **not run** there: a standalone OpenAPI schema validator (the route/operation
anti-drift test `TestOpenAPIMatchesRouter` does run in the Go job), Trivy image scanning, `cargo audit`,
fuzzing beyond the unit budget, `SIGKILL` crash drill, and the k6 load run. Those remain local/manual
checks with results in the phase reports. There is no JUnit XML or coverage artifact; the job log is the
evidence.

## 9. Reporting rules

For every phase and release, report:

- The exact command, working directory, tool versions, and date.
- Pass/fail counts and the names of failing tests.
- Tests that were not run and why.
- Any test that was weakened or skipped, with justification.
- Flaky tests: quarantine with an issue, never silently retry in a loop.

A green suite is evidence about the tested properties, not proof of correctness or security. The threat
model's residual risks remain.

## 10. Definition of done for testing

A feature is not done until:

1. Its normal and failure behaviour is covered by at least one automated test.
2. Concurrency/transaction claims have a real-DB test.
3. Security-sensitive behaviour has a negative test.
4. The test has actually run in this environment or CI, with output recorded.
5. Any unexecuted or environment-blocked test is explicitly listed in the phase report.
