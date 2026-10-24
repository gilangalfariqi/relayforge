# Phase 5 — Validation, Fuzzing, and Crash/Failover Drills Report

Date: 2026-10-23  
Status: **Complete — All Acceptance Criteria Verified**

## 1. Summary of Validation Work

Phase 5 validated the full RelayForge system across unit, property, fuzz, database integration, OpenAPI contract, concurrency, security regression, and live crash/failover E2E layers:

1. **Go Fuzz Targets & Budgeted Property Suite (`api/internal/httpapi/fuzz_test.go`)**:
   - Implemented `FuzzEventEnvelopeValidation`, `FuzzIdempotencyKeyValidation`, `FuzzCursorDecoder`, and `TestFuzzBudgetedPropertySuite` (executing `30,000` mutated/randomized inputs across the 3 targets with 0 panics).
   - Rust `proptest` property tests in `worker/src/policy.rs` verify backoff bounds, jitter, overflow freedom across attempts `1..=1000`, and retry-window dead-lettering.
2. **Lock-Order Hardening & Concurrency Flake Elimination (`api/internal/store/ops.go`)**:
   - During repeated concurrency runs (`-count=5`) while a background worker was active, we identified an AB-BA lock-order inversion between `ReplayEvent` (which locked `events FOR UPDATE` before `deliveries FOR UPDATE`) and `worker::store::complete_delivery` (which locks `deliveries` before `events FOR UPDATE`).
   - Fixed `ReplayEvent` in `api/internal/store/ops.go` to lock `deliveries` in `ORDER BY id ASC FOR UPDATE` first and lock `events FOR UPDATE` inside `recomputeEventProjectionTx` second, matching the worker's canonical lock order.
   - Re-ran 10 consecutive iterations (`-count=10`) of `TestConcurrentIdempotentIngest`, `TestEventReplayAdminReplayAndConcurrency`, and `TestLogRedactionAndSQLInjectionCorpus` with 0 failures (`21.682s`).
3. **Security Regression & Log Redaction (`api/internal/httpapi/integration_test.go`)**:
   - Added `TestLogRedactionAndSQLInjectionCorpus` verifying that canary API keys, signing secrets (`whsec_...`), and sensitive event payload strings never appear in structured JSON logs, and that SQL injection payloads across `type`, `cursor`, and `action` filters return `400 invalid_request` without leaking SQL error details.
4. **Live Crash & Failover Drill (`TestWorkerSIGKILLAndLeaseRecovery` in `api/internal/httpapi/e2e_phase3_test.go`)**:
   - Ingested an event against a slow receiver (`delay_ms = 8000`), waited until `relayforge-worker-1` claimed the delivery (`status = 'in_progress'`), killed the live worker container with `docker kill --signal=SIGKILL relayforge-worker-1`, verified the leased state in PostgreSQL, expired the lease and restarted `relayforge-worker-1`, and verified that the reaper marked attempt 1 `abandoned` (`error_code = 'lease_expired'`) and attempt 2 succeeded (`status = 'delivered'`).

## 2. Executed Commands and Recorded Outputs

### 2.1 Go Fuzz Seed Corpus & 30,000-Iteration Budgeted Fuzz Suite

```text
$ go test -v -run "Fuzz|TestFuzzBudgetedPropertySuite" ./internal/httpapi
=== RUN   TestFuzzBudgetedPropertySuite
    fuzz_test.go:181: executed 30000 budgeted fuzz iterations across 3 targets with 0 panics
--- PASS: TestFuzzBudgetedPropertySuite (0.10s)
=== RUN   FuzzEventEnvelopeValidation
--- PASS: FuzzEventEnvelopeValidation (0.00s)
=== RUN   FuzzIdempotencyKeyValidation
--- PASS: FuzzIdempotencyKeyValidation (0.00s)
=== RUN   FuzzCursorDecoder
--- PASS: FuzzCursorDecoder (0.00s)
PASS
ok  	github.com/relayforge/relayforge/api/internal/httpapi	1.175s
```

### 2.2 Repeated Concurrency & Security Regression Suite (`-count=10`)

```text
$ go test -tags=integration -run "TestConcurrentIdempotentIngest|TestEventReplayAdminReplayAndConcurrency|TestLogRedactionAndSQLInjectionCorpus" -count=10 ./internal/httpapi
ok  	github.com/relayforge/relayforge/api/internal/httpapi	21.682s
```

### 2.3 Live E2E Delivery, Replay, and Worker `SIGKILL` Crash Recovery Suite

```text
$ go test -v -tags=e2e -run "TestEndToEndDeliveryLifecycle|TestWorkerSIGKILLAndLeaseRecovery" ./internal/httpapi
=== RUN   TestEndToEndDeliveryLifecycle
--- PASS: TestEndToEndDeliveryLifecycle (5.91s)
=== RUN   TestWorkerSIGKILLAndLeaseRecovery
--- PASS: TestWorkerSIGKILLAndLeaseRecovery (3.48s)
PASS
```

## 3. Threat-Model Traceability Matrix (`docs/05-threat-model.md` §11)

| Requirement | Executed Verification Test | Result |
| --- | --- | --- |
| Secure API key generation & digest-only storage | `TestGenerateAndParseKey`, `TestSignupAuthAndKeyStorage` | **PASS** |
| Constant-time HMAC comparison | `TestVerifyDigestConstantTime`, `TestPublishedSignatureV1VectorAndTamperRejection` | **PASS** |
| Standard Webhooks `v1` HMAC signing | `matches_published_signature_v1_vector` (Rust), `TestPublishedSignatureV1VectorAndTamperRejection` (Go) | **PASS** |
| Timestamp replay protection (±300 s + durable dedupe) | `TestPublishedSignatureV1VectorAndTamperRejection`, `TestEndToEndDeliveryLifecycle` | **PASS** |
| Secret rotation overlap & retirement | `TestSecretRotationOverlapAndRetirementWithReceiver`, `multi_secret_rotation_header_joins_with_space` | **PASS** |
| Tenant isolation (`404` on all cross-tenant verbs) | `TestTenantIsolationMatrix` | **PASS** |
| Request validation, size, depth, and fuzzing | `TestEventValidationWritesNoRows`, `TestFuzzBudgetedPropertySuite` (30,000 iterations) | **PASS** |
| SQL injection resistance | `TestLogRedactionAndSQLInjectionCorpus` | **PASS** |
| Rate limiting (`429` + `Retry-After` & worker deferral) | `TestTenantAndAdminRateLimits`, `delivery_integration.rs` | **PASS** |
| Structured log redaction (canary verification) | `TestLogRedactionAndSQLInjectionCorpus` | **PASS** |
| Safe RFC 9457 errors (no panic/SQL/dependency leak) | `TestRecoveryHidesPanicDetail`, `TestReadinessReflectsDependency` | **PASS** |
| Registration & connect-time SSRF + DNS rebinding + redirect | `TestSharedConformanceVectors`, `shared_ssrf_conformance_vectors`, `dns_rebinding_blocks_second_attempt`, `delivery_integration.rs` | **PASS** |
| Dependency, secret, and image/config scanning | `govulncheck`, `gitleaks`, `trivy fs --scanners vuln,misconfig` | **PASS** |
| Internal admin listener & token protection | `TestAdminMetricsRequiresToken`, `TestAuditAndAdminSuspension`, `TestEndToEndDeliveryLifecycle` | **PASS** |

## 4. Phase 5 Acceptance Criteria Status

| Criterion | Status | Evidence |
| --- | --- | --- |
| All blocking suites pass; failures listed with root cause | **PASS** | All Go + Rust suites pass; AB-BA lock order root cause between `ReplayEvent` and `complete_delivery` identified and fixed |
| No flaky test retries hidden; quarantined tests have an issue and a reason | **PASS** | 0 quarantined tests; 10/10 consecutive concurrency runs passed |
| Fuzz targets run for a defined budget without panics (budget recorded) | **PASS** | 30,000 iterations (`10,000` per target) + seed corpora in `fuzz_test.go`, 0 panics |
| A full E2E run passes from a clean checkout and clean database | **PASS** | `TestEndToEndDeliveryLifecycle` + `TestWorkerSIGKILLAndLeaseRecovery` pass against Compose stack |
| Every threat-model mitigation marked "tested" has an executed test | **PASS** | Traceability table in §3 above |
