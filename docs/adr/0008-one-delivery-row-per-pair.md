# ADR-0008: One delivery row per (event, endpoint); replay via cycle

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

The brief suggested a `delivery_jobs` table. A “job” is one claim; a delivery is the lifecycle of an
(event, endpoint) pair across retries and replay. Conflating them either loses history or creates a
row per attempt.

## Decision

Rename to `deliveries`. Unique `(event_id, endpoint_id)`. Replay increments `cycle` and resets
attempt counters; `delivery_attempts` is unique on `(delivery_id, cycle, attempt_no)`. History is
append-only.

`attempt_seq` is a monotonic claim counter that is never decremented; `attempt_count` is consumed
attempts and is decremented only on rate-limit deferral and shutdown-before-send.

## Alternatives considered

| Alternative | Why not |
| --- | --- |
| New job row per retry | Harder uniqueness; history split |
| New job row per replay | Loses a simple “one delivery per pair” invariant |
| Mutate the same attempt row | Destroys forensic history |
