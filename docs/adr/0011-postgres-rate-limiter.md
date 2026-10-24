# ADR-0011: Fixed-window rate limits in PostgreSQL

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

The brief requires per-tenant and per-destination rate limits without adding Redis unless measured
need exists. Limits must be shared across API instances and workers.

## Decision

Store fixed-window counters in `rate_limit_counters` with primary key
`(scope_type, scope_id, window_start)`. Increment with a single atomic upsert. Windows are 1 second
for ingest/management/destination and 60 seconds for signup-per-IP.

Known trade-off: a client can burst up to **2×** the limit across a window boundary. That is
documented in the API and is acceptable for v1.

Destination limiting happens in the worker **before** HTTP. Denial releases the lease, does not
consume an attempt, and reschedules to the next window.

## Alternatives considered

| Alternative | Why not now |
| --- | --- |
| Redis token bucket | Extra infrastructure; revisit if counter upserts show up in claim p95 |
| Sliding window / GCRA in SQL | More writes and math; not justified before measurement |
| Process-local limiter | Incorrect with multiple API/worker instances |
