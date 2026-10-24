# Phase 3 report — delivery engine (Rust worker, demo receiver, E2E)

Date: 2026-10-23. Status: implemented and verified against PostgreSQL 18.6, Go 1.27.3 control plane, Rust 1.99.1 worker, and Go demo receiver.

## 1. Scope delivered

- **Rust delivery policy (`worker/src/policy.rs`)**:
  - Single source of truth for HTTP status classification (`contracts/delivery-contract-v1.md` §7.1): `2xx` -> `success`, `3xx` -> `permanent_failure` (`http_redirect`), specific `4xx` (`400`, `401`, `403`, `404`, `405`, `409`, `410`, `413`, `414`, `415`) -> `permanent_failure`, `408`/`425`/`429` -> `retryable_failure`, other `4xx` -> `permanent_failure` (`http_4xx`), `501`/`505`/`506`/`508`/`510`/`511` -> `permanent_failure` (`http_5xx_permanent`), other `5xx` -> `retryable_failure` (`http_5xx`).
  - `Retry-After` parser supporting both integer delta-seconds (`0..=86400`) and HTTP-date (`httpdate`), clamped to `[1, 3600]` seconds as a floor before `max_backoff_ms` cap.
  - Equal-jitter exponential backoff (`capped_ms / 2 + U[0, capped_ms / 2)`) and dead-letter rules (`attempt_count >= max_attempts` or `now + delay > cycle_started_at + retry_window_seconds`).
- **Rust secret decryption and Standard Webhooks `v1` signing (`worker/src/crypto.rs`)**:
  - AES-256-GCM decryption of `endpoint_secrets` rows using `RELAYFORGE_KEK` and canonical AAD `tenant_id:endpoint_id:secret_id:key_version`.
  - Standard Webhooks `v1` HMAC-SHA256 signing over `webhook_id + "." + timestamp + "." + raw_body` (`events.wire_body` unchanged), supporting multi-secret rotation joined by a single ASCII space.
- **Rust connect-time SSRF validator and pinned resolver (`worker/src/ssrf.rs`)**:
  - Full URL and IPv4/IPv6 CIDR validation matching `contracts/ssrf-policy-v1.md` and passing all 50 vectors in `contracts/ssrf-vectors.json` (including delivery-stage retryable `dns_error` vs `destination_blocked`).
  - `PinnedDnsResolver` implementing `reqwest::dns::Resolve` so outbound HTTP connects strictly to the pre-validated `SocketAddr` list without a second DNS lookup.
- **Rust PostgreSQL queue, rate limiter, reaper, and delivery engine (`worker/src/store.rs`, `worker/src/engine.rs`)**:
  - Normative batch claim query (`FOR UPDATE SKIP LOCKED`, `lease_token`, `attempt_count + 1`, `attempt_seq + 1`, atomic `delivery_attempts` insert).
  - Fixed-window destination rate limiter on `rate_limit_counters` (`scope_type = 'destination'`) with deferral path (`attempt_count - 1`, `outcome = 'rate_limited'`, no attempt consumed).
  - Hardened `reqwest 0.13.5` client (`default-features = false`, `system-proxy` disabled, `.no_proxy()`, `redirect::Policy::none()`, 3 s connect timeout, 10 s read timeout, per-delivery total timeout `1..=30` s, 8 KiB streamed response body cap).
  - Lease-fenced completion transaction (`WHERE id = $1 AND lease_token = $2 AND status = 'in_progress'`) and parent `events.status` projection (`SELECT ... FOR UPDATE`).
  - Expired-lease reaper (`deliveries_lease_idx`) with missing-attempt reconciliation (`ON CONFLICT (delivery_id, cycle, attempt_no) DO UPDATE`).
  - Graceful shutdown (`SIGTERM`/`SIGINT` marks `/readyz` `503`, stops claims, releases unstarted leases as `canceled` with `attempt_count - 1`).
- **Reference Demo Receiver (`api/internal/receiver`, `api/cmd/receiver`)**:
  - Raw-body Standard Webhooks `v1` HMAC-SHA256 verification (`hmac.Equal`), ±300 s timestamp check, durable deduplication in PostgreSQL (`receiver_processed_webhooks`), and programmable failure-injection control API (`/control/secrets`, `/control/behavior`, `/control/stats`, `/control/reset`).
  - Added `receiver` service to `deploy/compose.yaml` and built `/receiver` in `api/Dockerfile`.

## 2. Files added or updated

| Area | Files |
| --- | --- |
| Rust worker crate | `worker/Cargo.toml`, `worker/Cargo.lock`, `worker/Dockerfile`, `worker/src/lib.rs`, `worker/src/main.rs`, `worker/src/config.rs`, `worker/src/metrics.rs` |
| Rust delivery modules | `worker/src/policy.rs`, `worker/src/crypto.rs`, `worker/src/ssrf.rs`, `worker/src/store.rs`, `worker/src/engine.rs` |
| Rust integration tests | `worker/tests/delivery_integration.rs` |
| Go demo receiver | `api/internal/receiver/receiver.go`, `api/internal/receiver/receiver_test.go`, `api/cmd/receiver/main.go` |
| Go E2E suite & Compose | `api/internal/httpapi/e2e_phase3_test.go`, `api/Dockerfile`, `deploy/compose.yaml`, `api/internal/ssrf/validator.go`, `api/cmd/api/main.go` |

## 3. Commands and actual results

### Rust formatting, Clippy, unit/property tests, and PostgreSQL integration suite

Executed in `rust:1.99.1-bookworm` against PostgreSQL 18.6 (`relayforge_default`):

```text
$ ./scripts/install-tools.sh && cargo fmt --check && cargo clippy --all-targets --locked -- -D warnings && cargo test --all --locked
rustfmt 1.10.0-stable (58cf8f9c35 2026-10-13)
clippy 0.1.99 (58cf8f9c35 2026-10-13)
    Checking relayforge-worker v0.1.0 (/workspace/worker)
    Finished `dev` profile [unoptimized + debuginfo] target(s) in 3.33s
     Running unittests src/lib.rs

running 16 tests
test config::tests::accepts_valid_config ... ok
test config::tests::rejects_bad_database_url ... ok
test config::tests::rejects_invalid_lease_seconds ... ok
test config::tests::rejects_missing_port ... ok
test config::tests::rejects_short_poll_interval ... ok
test config::tests::rejects_unknown_log_level ... ok
test config::tests::rejects_zero_concurrency ... ok
test crypto::tests::aes_gcm_decrypt_verifies_aad ... ok
test crypto::tests::matches_published_signature_v1_vector ... ok
test crypto::tests::multi_secret_rotation_header_joins_with_space ... ok
test metrics::tests::renders_build_and_counters ... ok
test policy::tests::dead_letter_on_attempts_or_window_exhaustion ... ok
test policy::tests::retry_after_parsing_and_clamping ... ok
test policy::tests::status_matrix_matches_contract ... ok
test ssrf::tests::shared_ssrf_conformance_vectors ... ok
test policy::tests::backoff_respects_bounds_and_never_overflows ... ok

test result: ok. 16 passed; 0 failed; finished in 0.04s

     Running tests/delivery_integration.rs

running 1 test
test integration_suite ... ok

test result: ok. 1 passed; 0 failed; finished in 4.83s
```

### Go demo receiver unit tests (published HMAC vector, tamper rejection, stale timestamp, duplicate dedupe)

```text
$ go test -v -count=1 ./internal/receiver ./cmd/receiver
=== RUN   TestPublishedSignatureV1VectorAndTamperRejection
--- PASS: TestPublishedSignatureV1VectorAndTamperRejection (0.00s)
PASS
ok  	github.com/relayforge/relayforge/api/internal/receiver	1.173s
```

### Multi-service Docker Compose E2E verification (`api` + `postgres` + `worker` + `receiver`)

```text
$ docker compose -f deploy/compose.yaml ps
NAME                    STATUS                        PORTS
relayforge-api-1        Up About a minute (healthy)   0.0.0.0:8080->8080/tcp, 127.0.0.1:9090->9090/tcp
relayforge-postgres-1   Up 6 hours (healthy)          127.0.0.1:55432->5432/tcp
relayforge-receiver-1   Up About a minute (healthy)   127.0.0.1:8090->8090/tcp
relayforge-worker-1     Up About a minute (healthy)   127.0.0.1:9091->9090/tcp

$ go test -v -tags=e2e -count=1 -run TestEndToEndDeliveryLifecycle ./internal/httpapi
=== RUN   TestEndToEndDeliveryLifecycle
--- PASS: TestEndToEndDeliveryLifecycle (4.52s)
PASS
ok  	github.com/relayforge/relayforge/api/internal/httpapi	5.669s
```

Worker Prometheus metrics captured after `TestEndToEndDeliveryLifecycle` (`GET http://127.0.0.1:9091/metrics`):

```text
relayforge_deliveries_total{outcome="dead_lettered"} 1
relayforge_deliveries_total{outcome="delivered"} 2
relayforge_deliveries_total{outcome="failed"} 1
relayforge_deliveries_total{outcome="retry_scheduled"} 4
relayforge_delivery_attempts_total{error_code="http_404",outcome="permanent_failure"} 1
relayforge_delivery_attempts_total{error_code="http_5xx",outcome="retryable_failure"} 5
relayforge_delivery_attempts_total{error_code="none",outcome="success"} 2
relayforge_worker_db_up 1
relayforge_worker_ready 1
relayforge_worker_schema_version 5
```

## 4. Failures encountered and corrections during Phase 3

1. **`deliveries_lease_sanity` constraint in synthetic lease-expiry test**:
   - Setting `lease_expires_at = now() - interval '10 seconds'` on a newly inserted row violated `CHECK (lease_expires_at > created_at)`. Updated the test fixture to backdate both `created_at` and `lease_expires_at` when simulating an expired lease.
2. **Isolating `pg_stat_activity` check during slow-receiver HTTP test**:
   - When `/ok`, `/redirect`, `/rate-limit`, and `/slow` were claimed in the same batch, the fast tasks entered their short completion transactions while `/slow` was beginning its HTTP handler. Added a 200 ms delay before sampling `pg_stat_activity` in `/slow` to verify that the in-flight HTTP attempt holds zero open PostgreSQL transactions.

## 5. Acceptance status

All Phase 3 acceptance criteria are met and verified. Next phase: **Phase 4 — Security and Operations**.
