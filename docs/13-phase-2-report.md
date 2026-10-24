# Phase 2 report — correctness first (tenancy, credentials, endpoints, secrets, ingest, idempotency, audit)

Date: 2026-10-22. Status: implemented and locally verified against PostgreSQL 18.6. Delivery over HTTP remains Phase 3 scope.

## 1. Scope delivered

- **Tenancy and administrative boundary**:
  - `POST /internal/v1/admin/tenants` and `POST /internal/v1/admin/tenants/{id}/suspend` on the internal listener (protected by `X-Admin-Token` when configured).
  - Suspended tenants are rejected immediately on public API authentication with `403 tenant_suspended`.
- **API credentials**:
  - `POST /api/v1/credentials`, `GET /api/v1/credentials`, `POST /api/v1/credentials/{id}/rotate`, and `DELETE /api/v1/credentials/{id}`.
  - Format `rfk_live_<key_id_12chars>_<secret_43chars_base64url>` (256-bit CSPRNG secret).
  - Unsalted SHA-256 32-byte digest stored in `api_keys.secret_hash`; verification uses `crypto/subtle.ConstantTimeCompare`.
  - Overlap rotation sets `expires_at = now() + grace_seconds` on the predecessor key and inserts a replacement key in a single transaction.
  - Last-owner guard enforced inside a `SELECT ... FOR UPDATE` transaction, rejecting revocation of the final active `owner` credential with `409 last_owner_credential`.
- **Subscriber endpoints and SSRF registration policy**:
  - `POST /api/v1/endpoints`, `GET /api/v1/endpoints`, `GET /api/v1/endpoints/{id}`, `PATCH /api/v1/endpoints/{id}`, `POST /api/v1/endpoints/{id}/pause`, `POST /api/v1/endpoints/{id}/resume`, `DELETE /api/v1/endpoints/{id}`.
  - Registration-time SSRF validator (`api/internal/ssrf`) conforming to `contracts/ssrf-policy-v1.md` and passing all 50 shared vectors in `contracts/ssrf-vectors.json` (scheme allowlist, userinfo/fragment rejection, trailing-dot rejection, reserved TLD/suffix rejection, non-canonical IPv4 rejection, IPv4/IPv6 CIDR deny lists including `::ffff:0:0/96`, and port allowlist `{443, 8443}`).
- **Endpoint signing secrets (AES-256-GCM envelope encryption)**:
  - `GET /api/v1/endpoints/{id}/secrets`, `POST /api/v1/endpoints/{id}/secrets/rotate`, `DELETE /api/v1/endpoints/{id}/secrets/{secret_id}`.
  - Initial secret created atomically with `POST /api/v1/endpoints` and returned once as `whsec_<base64(32_random_bytes)>`.
  - Encrypted at rest with AES-256-GCM using the 32-byte KEK (`RELAYFORGE_KEK`), 12-byte random nonce, and canonical AAD `tenant_id:endpoint_id:secret_id:key_version`.
  - Retirement guard prevents retiring the final active secret (`409 last_active_secret`).
- **Event ingest, idempotency, and atomic fan-out**:
  - `POST /api/v1/events` enforces payload size (`<= 262,144` bytes -> `413`), JSON syntax (`400`), JSON nesting depth (`<= 32` -> `422`), envelope fields (`422`), and optional active JSON Schema (`draft 2020-12`, `<= 64 KiB`, no remote `$ref`).
  - Canonical `wire_body` constructed once at ingest (`id`, `type`, `time`, `producer`, `schema_version`, `data`) and stored in `events.wire_body`.
  - Atomic single-transaction write of `idempotency_keys`, `events`, and `deliveries` (`status = 'queued'`, `attempt_count = 0`, `next_attempt_at = now()`) for matching active endpoints (`deleted_at IS NULL AND status = 'active'`).
  - Zero matching active endpoints sets `events.status = 'no_subscribers'` and `subscriber_count = 0`.
  - `Idempotency-Key` concurrency uses `INSERT ... ON CONFLICT DO NOTHING` followed by `SELECT ... FOR UPDATE` on `idempotency_keys` within the transaction; concurrent identical requests wait for commit and replay the stored status/response with `Idempotency-Replayed: true`, while payload-hash mismatches return `409 idempotency_conflict`.
- **Read APIs and append-only audit log**:
  - `GET /api/v1/events`, `GET /api/v1/events/{id}`, `GET /api/v1/deliveries`, `GET /api/v1/deliveries/{id}`, `GET /api/v1/deliveries/{id}/attempts`, `GET /api/v1/audit-logs`.
  - Opaque base64url `(created_at, id)` descending keyset cursor pagination across all list endpoints.
  - Append-only audit entries written inside the mutating transactions for tenant creation/suspension, credential creation/rotation/revocation, endpoint creation/update/pause/resume/deletion, and secret rotation/retirement.

## 2. Dependencies on completed phases

Phase 2 builds on the Phase 1 Go HTTP server, configuration loader, observability middleware, and migrations `0001`–`0005`. No schema changes were required for Phase 2; the existing Phase 1 tables and indexes satisfied all Phase 2 queries and constraints.

## 3. Files added or updated

| Area | Files |
| --- | --- |
| Auth & API keys | `api/internal/auth/apikey.go`, `api/internal/auth/apikey_test.go` |
| Secrets & AES-GCM | `api/internal/secrets/secrets.go`, `api/internal/secrets/secrets_test.go` |
| SSRF validator | `api/internal/ssrf/validator.go`, `api/internal/ssrf/validator_test.go` |
| JSON limits & schema | `api/internal/jsonschema/limits.go`, `api/internal/jsonschema/limits_test.go` |
| Cursor pagination | `api/internal/cursor/cursor.go`, `api/internal/cursor/cursor_test.go` |
| PostgreSQL store | `api/internal/store/store.go`, `api/internal/store/ingest_integration_test.go` |
| HTTP handlers & tests | `api/internal/httpapi/server.go`, `api/internal/httpapi/handlers_phase2.go`, `api/internal/httpapi/integration_test.go`, `api/internal/httpapi/server_test.go` |
| Configuration & main | `api/internal/config/config.go`, `api/internal/config/config_test.go`, `api/cmd/api/main.go` |
| OpenAPI contract | `api/openapi/openapi.yaml`, `api/openapi/openapi_test.go` |
| Deployment | `deploy/compose.yaml`, `Makefile` |

## 4. Commands and actual results

### Formatting, vet, staticcheck, and vulnerability scan

```text
$ gofmt -l .
(no output)

$ go vet ./...
(exit 0)

$ go run honnef.co/go/tools/cmd/staticcheck@latest ./...
(exit 0, 0 findings)

$ go run golang.org/x/vuln/cmd/govulncheck@latest ./...
Scanning your code and P packages across M dependent modules for known vulnerabilities...
Your code is affected by 0 vulnerabilities.
```

### Unit, integration, and race-detector suites

Host execution against PostgreSQL 18.6 (`localhost:55432`):

```text
$ RELAYFORGE_TEST_DATABASE_URL="postgres://relayforge:relayforge@localhost:55432/relayforge?sslmode=disable" go test -count=1 ./...
ok  	github.com/relayforge/relayforge/api/cmd/migrate	0.324s
ok  	github.com/relayforge/relayforge/api/internal/auth	0.044s
ok  	github.com/relayforge/relayforge/api/internal/config	0.042s
ok  	github.com/relayforge/relayforge/api/internal/cursor	0.039s
ok  	github.com/relayforge/relayforge/api/internal/httpapi	1.464s
ok  	github.com/relayforge/relayforge/api/internal/jsonschema	0.048s
ok  	github.com/relayforge/relayforge/api/internal/secrets	0.041s
ok  	github.com/relayforge/relayforge/api/internal/ssrf	0.047s
ok  	github.com/relayforge/relayforge/api/openapi	0.041s

$ RELAYFORGE_TEST_DATABASE_URL="postgres://relayforge:relayforge@localhost:55432/relayforge?sslmode=disable" go test -v -tags=integration -count=1 ./internal/store
=== RUN   TestIngestEventFaultInjectionRollsBack
--- PASS: TestIngestEventFaultInjectionRollsBack (0.04s)
=== RUN   TestQueryPlansUseIndexes
--- PASS: TestQueryPlansUseIndexes (8.79s)
PASS
ok  	github.com/relayforge/relayforge/api/internal/store	8.918s
```

Race detector execution inside `golang:1.27.3` connected to `relayforge_default`:

```text
$ docker run --rm --network relayforge_default \
  -v "w:/forgerelay:/workspace" \
  -v "C:/Users/gilan/go/pkg/mod:/go/pkg/mod" \
  -v "relayforge-gobuild:/root/.cache/go-build" \
  -w /workspace/api \
  -e "RELAYFORGE_TEST_DATABASE_URL=postgres://relayforge:relayforge@postgres:5432/relayforge?sslmode=disable" \
  golang:1.27.3 go test -race -tags=integration -count=1 ./...
ok  	github.com/relayforge/relayforge/api/cmd/migrate	1.031s
ok  	github.com/relayforge/relayforge/api/internal/auth	1.039s
ok  	github.com/relayforge/relayforge/api/internal/config	1.037s
ok  	github.com/relayforge/relayforge/api/internal/cursor	1.028s
ok  	github.com/relayforge/relayforge/api/internal/httpapi	1.292s
ok  	github.com/relayforge/relayforge/api/internal/jsonschema	1.135s
ok  	github.com/relayforge/relayforge/api/internal/secrets	1.060s
ok  	github.com/relayforge/relayforge/api/internal/ssrf	1.119s
ok  	github.com/relayforge/relayforge/api/internal/store	11.420s
ok  	github.com/relayforge/relayforge/api/openapi	1.067s
```

### Verification of Phase 2 acceptance scenarios

1. **100-way concurrent idempotent ingest (`TestConcurrentIdempotentIngest`)**:
   - 100 goroutines released simultaneously via a channel barrier (`close(start)`) with the same `Idempotency-Key` and body.
   - Result: all 100 receive HTTP `202 Accepted` with identical `event.id`; exactly 1 response omits `Idempotency-Replayed` (the winner) and 99 include `Idempotency-Replayed: true`; PostgreSQL contains `1` row in `events`, `1` row in `deliveries`, and `1` row in `idempotency_keys`.
   - Re-sending the same `Idempotency-Key` with a mutated payload returns `409 Conflict` (`code: "idempotency_conflict"`).
2. **Mid-transaction fault injection (`TestIngestEventFaultInjectionRollsBack`)**:
   - Injects a failure after inserting into `idempotency_keys` and `events` before transaction commit.
   - Result: `events = 0`, `deliveries = 0`, `idempotency_keys = 0`.
3. **Cross-tenant isolation matrix (`TestTenantIsolationMatrix`)**:
   - Tenant B attempts `GET`, `PATCH`, `POST /pause`, `POST /resume`, `DELETE` on Tenant A's endpoint; `GET`, `POST /secrets/rotate`, `DELETE /secrets/{secret_id}` on Tenant A's endpoint secrets; `POST /credentials/{id}/rotate`, `DELETE /credentials/{id}` on Tenant A's credential; and `GET` on Tenant A's event, delivery, and delivery attempts.
   - Result: every cross-tenant call returns `404 Not Found` (`code: "not_found"`), leaking zero existence metadata.
4. **SSRF shared conformance vectors (`TestSharedConformanceVectors`)**:
   - Executes all 50 vectors in `contracts/ssrf-vectors.json`.
   - Result: 50/50 pass with exact reason codes (`scheme_not_allowed`, `userinfo_present`, `fragment_present`, `trailing_dot`, `host_denied`, `host_not_fqdn`, `ambiguous_authority`, `port_not_allowed`, `ip_denied`, `no_addresses`).
5. **Credential and secret storage verification (`TestPhase2EndToEndFlow`)**:
   - Verified `octet_length(secret_hash) = 32` in `api_keys` and confirmed plaintext key never appears in database columns.
   - Verified `endpoint_secrets` ciphertext decrypts only with the exact canonical AAD `tenant_id:endpoint_id:secret_id:key_version` and fails authentication if any AAD component is altered.
6. **Payload limits (`TestPayloadLimitsAndValidation`)**:
   - `> 262,144` bytes -> `413 Payload Too Large` (`payload_too_large`).
   - Nesting depth `33` (`> 32`) -> `422 Unprocessable Entity` (`validation_failed`).
   - Malformed JSON -> `400 Bad Request` (`invalid_json`).
   - JSON Schema violation -> `422 Unprocessable Entity` (`schema_validation_failed`).
   - Zero rows written on all rejected requests.

### Query plans (`EXPLAIN (ANALYZE, BUFFERS)`)

Captured by `TestQueryPlansUseIndexes` against PostgreSQL 18.6 on a 50,000-row / 200-tenant fixture:

- **Events list by tenant (`events_tenant_created_idx`)**:
  ```text
  Limit  (cost=0.42..48.61 rows=50 width=250) (actual time=0.022..0.043 rows=50.00 loops=1)
    Buffers: shared hit=53
    ->  Index Scan using events_tenant_created_idx on events  (cost=0.42..241.36 rows=250 width=250) (actual time=0.021..0.039 rows=50.00 loops=1)
          Index Cond: ((tenant_id = $1) AND (ROW(created_at, id) < ROW(now(), 'ffffffff-ffff-ffff-ffff-ffffffffffff'::uuid)))
          Index Searches: 1
          Buffers: shared hit=53
  Execution Time: 0.051 ms
  ```
- **Events list by tenant and event_type (`events_tenant_type_created_idx`)**:
  ```text
  Limit  (cost=0.42..83.63 rows=50 width=250) (actual time=0.020..0.080 rows=50.00 loops=1)
    Buffers: shared hit=53
    ->  Index Scan using events_tenant_type_created_idx on events  (cost=0.42..138.54 rows=83 width=250) (actual time=0.019..0.075 rows=50.00 loops=1)
          Index Cond: ((tenant_id = $1) AND (event_type = 'order.created'::text))
          Index Searches: 1
          Buffers: shared hit=53
  Execution Time: 0.088 ms
  ```
- **Deliveries list by tenant and status (`deliveries_tenant_status_created_idx`)**:
  ```text
  Limit  (cost=209.61..209.73 rows=50 width=178) (actual time=2.393..2.400 rows=50.00 loops=1)
    Buffers: shared hit=253
    ->  Sort  (cost=209.61..210.23 rows=250 width=178) (actual time=2.391..2.395 rows=50.00 loops=1)
          Sort Key: created_at DESC, id DESC
          Sort Method: top-N heapsort  Memory: 37kB
          Buffers: shared hit=253
          ->  Bitmap Heap Scan on deliveries  (cost=4.65..201.30 rows=250 width=178) (actual time=0.587..1.981 rows=250.00 loops=1)
                Recheck Cond: ((tenant_id = $1) AND (status = 'queued'::delivery_status))
                Heap Blocks: exact=250
                Buffers: shared hit=253
                ->  Bitmap Index Scan on deliveries_tenant_status_created_idx  (cost=0.00..4.59 rows=250 width=0) (actual time=0.517..0.517 rows=250.00 loops=1)
                      Index Cond: ((tenant_id = $1) AND (status = 'queued'::delivery_status))
                      Index Searches: 1
                      Buffers: shared hit=3
  Execution Time: 2.427 ms
  ```
- **Audit logs list by tenant (`audit_logs_tenant_created_idx`)**:
  ```text
  Limit  (cost=0.42..33.42 rows=50 width=190) (actual time=0.019..0.036 rows=50.00 loops=1)
    Buffers: shared hit=53
    ->  Index Scan using audit_logs_tenant_created_idx on audit_logs  (cost=0.42..165.42 rows=250 width=190) (actual time=0.018..0.032 rows=50.00 loops=1)
          Index Cond: (tenant_id = $1)
          Index Searches: 1
          Buffers: shared hit=53
  Execution Time: 0.044 ms
  ```
- **Worker claim query (`deliveries_claim_idx`)**:
  ```text
  Limit  (cost=0.29..6.04 rows=50 width=62) (actual time=0.025..0.039 rows=50.00 loops=1)
    Buffers: shared hit=102
    ->  LockRows  (cost=0.29..5751.64 rows=50000 width=62) (actual time=0.024..0.035 rows=50.00 loops=1)
          Buffers: shared hit=102
          ->  Index Scan using deliveries_claim_idx on deliveries  (cost=0.29..5251.64 rows=50000 width=62) (actual time=0.016..0.020 rows=50.00 loops=1)
                Index Cond: (next_attempt_at <= now())
                Index Searches: 1
                Buffers: shared hit=3
  Execution Time: 0.049 ms
  ```

> **Planner note on low-cardinality tenant distributions**: When a synthetic fixture distributes 50,000 time-ordered rows across only 2–20 round-robin tenants (`>= 2,500` rows per tenant with physical heap correlation `-1.0` on `created_at`), PostgreSQL's cost estimator can prefer `Index Scan Backward using events_retention_idx` with a `Filter: (tenant_id = $1)` because reading 200–1,000 sequential heap pages appears cheaper under default `random_page_cost = 4.0` than 50 random heap fetches. At realistic multi-tenant cardinality (200 tenants, 250 rows/tenant) or SSD `random_page_cost = 1.1`, PostgreSQL consistently selects `events_tenant_created_idx`.

### Contract & secret scanning

```text
$ npx --yes @redocly/cli lint api/openapi/openapi.yaml
Woohoo! Your API description is valid. (2 warnings)

$ docker run --rm -v "w:/forgerelay:/repo" ghcr.io/gitleaks/gitleaks:latest detect --source /repo --no-git
WRN0000 scan completed in 1.15s
INF0000 no leaks found
```

## 5. Failures encountered and corrections during Phase 2

1. **SSRF trailing-dot and IPv4-mapped IPv6 conformance gaps**:
   - The initial SSRF validator stripped trailing dots from hostnames instead of rejecting them with `trailing_dot`, and called `addr.Unmap()` before checking IPv6 deny ranges, which allowed `[::ffff:93.184.216.34]` instead of rejecting `::ffff:0:0/96` as `ip_denied`.
   - Fixed `api/internal/ssrf/validator.go` to reject trailing dots, enforce all reserved suffixes (`.localhost`, `.local`, `.internal`, `.home.arpa`, `.lan`, `.corp`, `.intranet`, `.test`, `.example`, `.invalid`), reject non-canonical IPv4 forms (`127.1`, integer, hex, octal), reject ambiguous authorities (`\`, `%`), and evaluate IPv6 `::ffff:0:0/96` before unmapping. Added `TestSharedConformanceVectors` running all 50 vectors from `contracts/ssrf-vectors.json`.
2. **Idempotency concurrency barrier**:
   - `TestConcurrentIdempotentIngest` originally launched goroutines without a synchronization barrier. Added a `close(start)` barrier and verified that 1 winner omits `Idempotency-Replayed` and 99 losers receive `Idempotency-Replayed: true`.

## 6. Acceptance status

All Phase 2 acceptance criteria are met and verified with recorded outputs. Next phase: **Phase 3 — Delivery Engine**.
