# Requirements, scope, and service objectives

Status: requirements approved in Phase 0; implemented through Phase 7 and locally verified. Last updated: 2026-10-24.

## 1. Problem statement

Companies integrate independent systems (e-commerce, ERP, CRM, internal services) with webhooks.
The receiving system is often slow, misconfigured, or briefly unavailable. Engineering teams that build
their own delivery layer repeatedly hit the same failures:

1. Events are lost between a database commit and the publish step.
2. Requests are duplicated, causing duplicate orders, invoices, or notifications.
3. Two workers process the same event.
4. Permanent failures are retried forever; transient failures are abandoned.
5. Failed deliveries cannot be inspected or safely replayed.
6. API keys and signing secrets leak through logs, source control, or chat.
7. One tenant reads another tenant's events, endpoints, or secrets.
8. A customer-supplied URL is used to reach the platform's internal network or cloud metadata service.
9. There is no evidence of queue age, failure rate, or retry volume.
10. Operators cannot recover from a partial failure without destroying delivery history.

RelayForge addresses these with a durable, observable, multi-tenant delivery platform and an explicit
failure model.

## 2. Target users and jobs to be done

| User | Job to be done | Success looks like |
| --- | --- | --- |
| Integration engineer at a SaaS company | Publish business events to customer endpoints without building retry infrastructure | Events are accepted once, delivered at least once, and failures are explainable |
| Platform operator | Detect and recover from backlogs and destination outages | Queue age, failure codes, and attempts are visible; recovery steps are documented |
| Security engineer | Bound the blast radius of credentials and customer URLs | Keys are hashed, secrets encrypted and rotatable, SSRF blocked at connect time |
| Tenant administrator | Investigate a failed delivery and replay it | Delivery history is immutable and replay is auditable and race-safe |
| Finance/ops engineer | Trust that a duplicate webhook will not double-book an order | Receiver pattern with durable idempotency is documented and demonstrated |

## 3. Assumptions

These are engineering assumptions, not confirmed business requirements. They are flagged so they can be
challenged before implementation.

| ID | Assumption | Impact if wrong |
| --- | --- | --- |
| A1 | One deployment serves multiple tenants; tenants are logically isolated in one database | Schema, auth, and tests change substantially |
| A2 | Event payloads are JSON objects, typically < 20 KiB, with a hard 256 KiB limit | Larger payloads require blob storage and a different contract |
| A3 | A single event fans out to a bounded number of endpoints (default cap 50, reference average 2) | Fan-out transaction and queue sizing change |
| A4 | Endpoints are public HTTPS services; private/on-prem destinations are out of scope for v1 | Would require a private-link or agent model |
| A5 | Sustained reference load is 10 events/s with 50 events/s bursts; targets are revised after measurement | Queue partitioning and retention policy change |
| A6 | At-least-once delivery is acceptable if receivers are idempotent | Exactly-once would require receiver cooperation beyond our control |
| A7 | Signing secrets must be recoverable by the delivery workers to compute HMAC | A KMS/signing-service design would be required otherwise |
| A8 | A single PostgreSQL primary is the durability boundary in v1; HA is deployment-specific | Multi-region or active-active is out of scope |
| A9 | System clocks are NTP-synchronised; a 300 s signature tolerance is acceptable | Replay protection and receiver verification change |
| A10 | Tenants authenticate with API keys only in v1; human SSO is out of scope | Requires an identity provider and session model |

## 4. Non-goals (v1)

- Exactly-once delivery or exactly-once business side effects.
- Kafka, Redis, or another broker. A documented threshold must be measured before adding one.
- A web UI. The API, OpenAPI document, and curl examples are the interface.
- Arbitrary event transformation, filtering DSLs, or workflow orchestration.
- Inbound webhooks from third parties (we are the publisher, not the consumer).
- Private network destinations, VPC peering, or an on-premises agent.
- Multi-region replication, active-active writers, or cross-region failover.
- Human user accounts, SSO, RBAC administration UI, or billing.
- Email/SMS notification channels. Delivery is HTTP webhook only.
- A general-purpose schema registry. Optional JSON Schema validation is per tenant and per event type.
- Compliance certification claims (SOC 2, PCI). Controls are described, not certified.

## 5. Prioritised requirements

Priority: **P0** = required for a credible v1; **P1** = required for the portfolio release; **P2** =
explicitly deferred.

### 5.1 Tenancy and identity

| ID | Pri | Requirement | Acceptance signal |
| --- | --- | --- | --- |
| TEN-1 | P0 | Register a tenant and issue a first API credential | Tenant row + credential row committed atomically; plaintext key returned once |
| TEN-2 | P0 | Every tenant-scoped query includes the authenticated tenant ID | Code review + cross-tenant integration tests return `404` |
| TEN-3 | P0 | Suspend a tenant; all authenticated requests then fail closed | Suspended tenant receives `403 tenant_suspended`; no writes |
| TEN-4 | P1 | Self-service signup abuse control | Per-IP rate limit; optional `RELAYFORGE_SIGNUP_TOKEN`; audit record |
| TEN-5 | P2 | SSO and human users | Deferred |

### 5.2 Credentials and secrets

| ID | Pri | Requirement | Acceptance signal |
| --- | --- | --- | --- |
| CRED-1 | P0 | Generate 256-bit API keys with an identifiable prefix | Key format documented; 100k generated keys unique in test |
| CRED-2 | P0 | Store only an HMAC-SHA256 digest of the key secret, never plaintext | DB inspection test; digest compared in constant time |
| CRED-3 | P0 | Revoke and rotate credentials, with bounded overlap | Old key rejected after revocation; audit row written |
| CRED-4 | P0 | Generate per-endpoint 256-bit signing secrets, returned once | Secret shown only in create/rotate response; never in GET |
| CRED-5 | P0 | Encrypt signing secrets at rest with AES-256-GCM and a versioned KEK | DB contains ciphertext only; key version recorded |
| CRED-6 | P1 | Rotate signing secrets with overlap; sign with all active secrets | Receiver accepts either signature during overlap |
| CRED-7 | P1 | Redact secrets, signatures, Authorization headers, and payload bodies from logs | Log-capture tests assert absence of canary values |

### 5.3 Ingestion and idempotency

| ID | Pri | Requirement | Acceptance signal |
| --- | --- | --- | --- |
| ING-1 | P0 | `POST /api/v1/events` with an authenticated tenant, event type, and JSON object payload | Valid request returns `201` with stable event ID |
| ING-2 | P0 | Enforce a 256 KiB request body limit before decoding | Oversized body returns `413`, connection handled safely |
| ING-3 | P0 | Require an `Idempotency-Key`; same key + same body returns the original response | Replay returns `201` + `Idempotency-Replayed: true`, exactly one event row |
| ING-4 | P0 | Same key + different body returns `409 idempotency_key_reused` | Documented and tested |
| ING-5 | P0 | Idempotency scope is tenant + route; TTL 24 h | Expired key may be reused; cleanup test |
| ING-6 | P0 | Persist event and all initial deliveries in one transaction | Fault-injection test leaves zero partial rows |
| ING-7 | P0 | Bounded fan-out: at most 50 endpoints per event | Excess subscriptions rejected at endpoint creation or event ingest with `422` |
| ING-8 | P1 | Optional tenant-scoped JSON Schema per event type/version | Invalid payload returns `422`; valid payload passes |
| ING-9 | P1 | Capture W3C `traceparent` for cross-service tracing | Worker span links to ingest trace |
| ING-10 | P2 | Batch ingestion endpoint | Deferred |

### 5.4 Endpoints and SSRF

| ID | Pri | Requirement | Acceptance signal |
| --- | --- | --- | --- |
| EP-1 | P0 | Register, list, update, pause, and soft-delete HTTPS endpoints | `201/200/404` semantics tested; deleted endpoint stops fan-out |
| EP-2 | P0 | Reject non-HTTPS schemes, userinfo, non-allowlisted ports, and non-global destinations at registration | Conformance vectors pass |
| EP-3 | P0 | Enforce destination policy at connect time, not only at registration | DNS-rebinding test: registration succeeds with a global IP, delivery to a private IP is blocked |
| EP-4 | P0 | Disable redirects and proxy environment variables in the delivery client | Redirect target is never contacted; `HTTP_PROXY` ignored |
| EP-5 | P1 | Rate-limit deliveries per destination (default 10 req/s, configurable 1–100) | Excess work is deferred, not lost or counted as a failed attempt |
| EP-6 | P1 | Auto-pause an endpoint after repeated terminal failures | Threshold configurable; audit record; resumable by tenant |
| EP-7 | P2 | Allowlisted private destinations for enterprise tenants | Deferred; needs a network architecture |

### 5.5 Delivery engine

| ID | Pri | Requirement | Acceptance signal |
| --- | --- | --- | --- |
| DEL-1 | P0 | Claim due deliveries with `FOR UPDATE SKIP LOCKED` and a lease token | Concurrent claim test: no row claimed twice |
| DEL-2 | P0 | Never hold a database transaction open during an outbound HTTP request | Code review + test that HTTP latency does not hold locks |
| DEL-3 | P0 | Bounded global and per-destination concurrency | Load test shows in-flight never exceeds configured caps |
| DEL-4 | P0 | Per-attempt connect, read, and total timeouts | Slow-receiver test records `read_timeout`/`total_timeout` |
| DEL-5 | P0 | HMAC-SHA256 signature over a precise canonical string | Independent verifier accepts; tampered body/timestamp rejected |
| DEL-6 | P0 | Classify transient vs permanent failures; retry only transient | Status-code matrix test |
| DEL-7 | P0 | Exponential backoff with jitter, capped attempts and retry window | Deterministic bounds test; no unbounded retries |
| DEL-8 | P0 | Record every attempt with outcome, status, duration, and redacted error | Attempt history endpoint returns ordered attempts |
| DEL-9 | P0 | Recover expired leases without stale-worker overwrite | Crash test: lease expiry schedules retry; stale completion affects 0 rows |
| DEL-10 | P0 | Graceful shutdown stops claiming and releases unstarted work | SIGTERM test with in-flight request |
| DEL-11 | P1 | Distinguish dead-lettered from permanently failed deliveries | Both replayable, different codes and metrics |
| DEL-12 | P2 | Delivery ordering guarantees | Not promised; documented |

### 5.6 Replay and operations

| ID | Pri | Requirement | Acceptance signal |
| --- | --- | --- | --- |
| OPS-1 | P0 | Authorised replay of failed/dead-lettered deliveries only | Delivered deliveries are not replayed; `409` |
| OPS-2 | P0 | Replay preserves attempt history and increments a cycle counter | Old attempts remain; new attempts carry `cycle = n+1` |
| OPS-3 | P0 | Concurrent replay requests cannot double-schedule | One `202`, one `409`; exactly one cycle increment |
| OPS-4 | P0 | Audit all security-sensitive actions | Append-only audit rows for keys, secrets, endpoints, replay, tenant suspend |
| OPS-5 | P0 | Health and readiness have distinct meanings | Liveness independent of DB; readiness requires DB + schema + loop |
| OPS-6 | P0 | Prometheus metrics for requests, deliveries, retries, queue age, failures | `/metrics` scraped in E2E; alert rules documented |
| OPS-7 | P0 | Dashboard queries and alerts tied to actionable conditions | Documented PromQL + thresholds |
| OPS-8 | P0 | Documented incident recovery procedures | Runbook with verified commands (verified in Phase 5) |
| OPS-9 | P1 | Admin API for tenant suspension and queue inspection | Separate internal listener + admin token |
| OPS-10 | P2 | Automated secret-manager integration (Vault/KMS) | Deferred; KEK from environment in v1 |

### 5.7 API quality

| ID | Pri | Requirement | Acceptance signal |
| --- | --- | --- | --- |
| API-1 | P0 | Versioned `/api/v1`, consistent RFC 9457 errors with a stable `code` | Contract tests |
| API-2 | P0 | Cursor pagination, default 25, max 100; allowlisted sort fields | Bounded query test; invalid cursor `400` |
| API-3 | P0 | Filtering with validated types and bounded time ranges (max 90 days) | Filters tested; no SQL injection |
| API-4 | P0 | OpenAPI 3.1 document matching handlers | Spec lint + contract tests in CI |
| API-5 | P0 | `429` with `Retry-After` and `RateLimit-*` headers | Rate-limit tests |
| API-6 | P0 | Request IDs accepted or generated and echoed | Middleware test |

## 6. Initial service-level objectives (targets, not measurements)

No benchmark has been run. These are proposed targets for a reference workload, to be revised in Phase 6
using measured results. They are deliberately moderate for a single-primary PostgreSQL deployment.

Reference workload: 10 events/s sustained (864,000 events/day), 50 events/s for 60 s bursts, 2 endpoints
per event, mean payload 2 KiB, 20% of deliveries experience a transient failure (5xx or timeout) on the
first attempt.

| SLO | Target | Measurement |
| --- | --- | --- |
| Ingest availability | 99.9% monthly | Successful `2xx` / valid authenticated requests, excluding client errors |
| Ingest latency at 10 ev/s | p95 ≤ 150 ms, p99 ≤ 400 ms | Server-side histogram, excluding network |
| Burst behaviour at 50 ev/s for 60 s | error rate < 0.1%, p99 ≤ 750 ms | Load test |
| Acknowledged event durability | 0 acknowledged events lost | Commit-before-ack audit + crash tests |
| First-attempt queue wait | p95 ≤ 2 s at reference load | `deliveries.created_at` → attempt `started_at` |
| Delivery success for healthy endpoints | ≥ 99.5% of deliveries | Benchmark receiver returning `2xx` after injected transient failures |
| Lease recovery | ≤ 2 × lease duration (180 s) after worker death | Crash test |
| Replay correctness | 100% of replay requests produce exactly one new cycle | Concurrency test |

These SLOs are **not** performance claims. Phase 6 will either confirm them, revise them with evidence, or
report that they were not met.

## 7. Constraints

- Go control plane, Rust delivery worker, PostgreSQL, SQL migrations, OpenAPI, Prometheus, OpenTelemetry.
- No broker in v1. Redis, Kafka, or SQS require a documented measured justification.
- Secrets only through environment/secret manager; never committed.
- Customer-controlled egress must be treated as hostile.
- Windows development host with Docker; CI runs on Linux. Path and line-ending issues must be handled
  explicitly (`.gitattributes` in Phase 1).
- The public API is HTTP/JSON. TLS is terminated by a reverse proxy in the reference deployment; the Go
  service itself does not terminate TLS in v1. This must be documented operationally, not implied.

## 8. Open questions requiring approval

| ID | Question | Proposed default |
| --- | --- | --- |
| Q1 | Is self-service tenant signup open, invite-only, or admin-only? | Open in dev; optional signup token required when configured |
| Q2 | Are non-443 destination ports required? | Allowlist 443 only; configurable |
| Q3 | Is optional JSON Schema validation required in v1? | Include in Phase 2 as P1 |
| Q4 | Should delivered events ever be replayable? | No in v1; failed and dead-lettered only |
| Q5 | Is a 30-day event retention acceptable, given storage math? | 30 days configurable; 7 days in local Compose |
| Q6 | Is a shared KEK between control plane and worker acceptable? | Yes for v1; KMS/signing service is documented future work |
