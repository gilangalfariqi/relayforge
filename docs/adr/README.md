# Architectural decision records

Status: decisions accepted and implemented through Phase 7. Last updated: 2026-10-24.

Each ADR records a decision that is expensive to reverse. Rejected alternatives are listed so a later
reviewer can see what was considered, not only what shipped.

Format: Context → Decision → Consequences → Alternatives considered → Status.

| ID | Title | Status |
| --- | --- | --- |
| [0001](0001-modular-monolith-and-worker.md) | Go modular monolith + separately deployable Rust worker | Accepted |
| [0002](0002-postgres-as-queue.md) | PostgreSQL as durable queue | Accepted |
| [0003](0003-standard-webhooks-signature.md) | Standard Webhooks HMAC scheme | Accepted |
| [0004](0004-uuidv7-and-text-statuses.md) | UUIDv7 PKs and text CHECK statuses | Accepted |
| [0005](0005-wire-body-stability.md) | Store exact ingest bytes for signing | Accepted |
| [0006](0006-secret-storage.md) | Hashed API keys, AES-GCM signing secrets, shared KEK | Accepted |
| [0007](0007-no-rls-in-v1.md) | Defer PostgreSQL RLS | Accepted |
| [0008](0008-one-delivery-row-per-pair.md) | One delivery row per (event, endpoint), replay via cycle | Accepted |
| [0009](0009-reqwest-connect-time-ssrf.md) | Connect-time SSRF via custom resolver | Accepted |
| [0010](0010-internal-admin-listener.md) | Separate internal listener for admin/metrics | Accepted |
| [0011](0011-postgres-rate-limiter.md) | Fixed-window counters in PostgreSQL | Accepted |
| [0012](0012-no-broker-until-measured.md) | No Kafka/Redis until a measured threshold | Accepted |
