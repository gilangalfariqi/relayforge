# ADR-0002: PostgreSQL as the durable job queue

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

Event persistence and delivery-job creation must be atomic. A crash between a database commit and a
publish must not lose work. Concurrent workers must not process the same job. The brief forbids adding
Kafka, Redis, or another broker without a documented requirement or measured bottleneck.

PostgreSQL 18 documents `SELECT … FOR UPDATE SKIP LOCKED` as providing an inconsistent view and as
suitable for queue-like tables, not general-purpose queries
(https://www.postgresql.org/docs/18/sql-select.html).

## Decision

Use PostgreSQL as **both** the system of record and the durable queue. The Go API inserts the event and
its `deliveries` rows in one transaction. The commit **is** the publish. Workers claim with
`FOR UPDATE SKIP LOCKED`, a per-attempt `lease_token`, and `lease_expires_at`. HTTP never runs inside a
database transaction.

## Consequences

- Dual-write (commit then publish) is eliminated.
- Throughput, bloat, and lock contention are bounded by one primary. Autovacuum, retention, and a
  partial claim index are first-line mitigations.
- Failover can still produce duplicate deliveries (at-least-once). That is accepted.

## Revisit thresholds (measured, not guessed)

Evaluate a broker only after measuring, on documented hardware:

1. Claim p95 latency > 50 ms under sustained load, or
2. Queue depth that cannot drain within the retry window, or
3. Delivery throughput > 5,000/s on the reference hardware.

Any broker introduction requires an outbox (or equivalent) so event+job atomicity is preserved. See
[ADR-0012](0012-no-broker-until-measured.md).

## Alternatives considered

| Alternative | Why not now |
| --- | --- |
| Redis list / Streams | Extra durability domain; dual-write unless Redis is the SoR (it must not be) |
| Kafka + outbox | Correct at scale; unjustified operational cost before a measured bottleneck |
| LISTEN/NOTIFY | Wake-up hint only; not durable work; still need polling |
| SKIP LOCKED without leases | Abandoned in-progress rows after a crash |
