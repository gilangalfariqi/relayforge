# Phase 6 report — Performance and benchmarking

Date: 2026-10-23. Status: **executed; scaled-down load, two findings, one optimization adopted, one rejected.**
Every number below comes from `bench/results/` (`baseline.json`, `optimized-worker.json`, `explain-baseline.txt`).

## 1. Environment (indicative, not SLO-grade)

| Item | Value |
| --- | --- |
| CPU / RAM | AMD Ryzen 3 5300U, 4 cores / 8 threads, 9.85 GiB |
| OS / Docker | Windows 11 (build 26300), Docker 29.5.2 (Linux engine) |
| PostgreSQL | `postgres:18.6-alpine`; `shared_buffers`=128 MB (16384×8 kB), `work_mem`=4 MB, `max_connections`=100, `synchronous_commit`=on, `fsync`=on, `max_wal_size`=1 GB |
| Services | API, worker (concurrency 8, claim batch 16), receiver, PostgreSQL all on one host and one Docker bridge |
| Toolchains | Go 1.27.3, Rust 1.99.1 |
| Commit | none — the repository has no commits (`git rev-parse HEAD` fails); tree is dirty by definition |

Because the commit SHA and image digests are missing, the benchmark plan (§4) requires results to be labelled
**indicative**. They cannot support an SLO claim.

## 2. Dataset

`bench/preload.sql` (SQL `generate_series`, 10 tenants, 20 endpoints) loaded 1,000,000 events, 2,000,000 deliveries
and 2,050,000 attempts, plus 50,000 idempotency rows, 10,000 rate-limit rows and 10,000 audit rows. Insert times:
events 122.6 s, deliveries 295.8 s, attempts 183.4 s. Sizes after `ANALYZE`: deliveries 1124 MB, events 882 MB,
delivery_attempts 670 MB.

**Confounder:** the live API retention loop (30-day policy, enabled by default) ran against the preload, because about
8.6% of events are older than 30 days. By the baseline run events were 990,000 and by the optimized run 971,334. The
dataset therefore shrank slightly during measurement; this was not disabled (the plan says retention should be off for
latency runs — **deviation**).

## 3. Scale actually run (deviation from plan)

The plan asks for 10-minute × 3 repetitions. On this 9.85 GiB laptop with all services co-located, runs were shortened
to 10–20 s per scenario and executed **once**. Percentiles are over those short windows. Load generator is a Go driver
(`api/cmd/bench`), not k6 (`bench/k6_ingest.js` is provided but was **not executed**). S4, S5 and S7 were not run in
Phase 6; S7 (SIGKILL) was executed in Phase 5. S6 and S8 were run.

## 4. Baseline results (`baseline.json`)

| Scenario | Load | p50 / p95 / p99 (ms) | Errors | Notes |
| --- | --- | --- | --- | --- |
| S1 ingest + fan-out (2 endpoints, 2 KiB / 64 KiB mix) | 300 req @ 30 ev/s | 47.4 / **163.9** / 277.3 | 0 | 3× reference rate |
| S2 steady state end-to-end | 200 ev @ 10 ev/s | 27.6 / 53.6 / 86.9 | 0 | queue wait p50/p95/p99 = 151 / 530 / 719 ms; 400/400 deliveries delivered (100%); 440 attempts (40 injected 503s retried); drain 0.32 s |
| S3 burst | 500 ev @ 50 ev/s (10 s) | 39.2 / 123.4 / 160.2 | 0 | **drain 21.4 s** for 1,000 deliveries |
| S6 permanent 4xx | 60 events | 25.4 / 41.6 / 65.1 | 0 | 60 failed, 60 attempts, **0 retries** |
| S8 replay storm | 200 replays, 16 concurrent (store layer) | 139.2 / 224.1 / 293.5 | 0 | 200/200 reached cycle 2 delivered; throughput 107/s |
| S9 read load | 500 req @ 50 rps | 13.1 / 38.5 / 53.5 | 0 | list events, by type, dead-lettered, attempts |
| S10 rate limiter upsert | 500 sequential calls | 4.89 / 8.84 / 21.64 | 0 | destination limit 2/s: 6 deferrals, all 6 delivered |
| S11 retention under 20 ev/s ingest | 3 passes | ingest 32.9 / 89.0 / 215.3 | 0 | pass p50 2.03 s, max 2.29 s; 15,000 events (30,000 cascaded deliveries) and 10,000 idempotency rows deleted |

Against the §12 targets (indicative):

- S2 ingest p95 ≤ 150 ms, p99 ≤ 400 ms: **met** (53.6 / 86.9).
- First-attempt queue wait p95 ≤ 2 s: **met** (0.53 s).
- Delivery success ≥ 99.5%: **met** (100%).
- S3 p99 ≤ 750 ms and error < 0.1%: **met** (160 ms, 0%); drain ≤ 5 min: **met** (21 s).
- S1 (not a stated SLO scenario) at 30 ev/s: p95 163.9 ms **exceeds the 150 ms figure**. This is 3× the reference rate with a 64 KiB payload in 1% of requests and on a single shared host; the shortfall is recorded, not hidden.
- Not measured: S4, S5, S7 duplicates under load, CPU/IOPS/pool wait per component, `pg_stat_statements`.

## 5. Query plans at scale (`bench/explain.sql`)

All named paths were captured with `EXPLAIN (ANALYZE, BUFFERS)` on the 1M/2M dataset (`explain-baseline.txt`).
Execution times, post-warm-up capture:

| Path | Plan | Execution |
| --- | --- | --- |
| Claim due deliveries | Index scan `deliveries_claim_idx` → LockRows → Limit, no sort | 14.3 ms |
| Reap expired leases | Index scan `deliveries_lease_idx` | 6.9 ms |
| List tenant events (cursor) | Index scan backward on `events_retention_idx` + incremental sort (**not** `events_tenant_created_idx`) | 19.6 ms |
| List by type, populated type | same retention-index plan | 2.6 ms |
| List by type, sparse type (Q4b) | retention-index plan, 863,928 rows filtered | **26,389 ms** |
| Dead-lettered deliveries | `deliveries_tenant_status_created_idx` | 3.3 ms |
| Event detail deliveries / projection | `deliveries_event_idx` | 3.1 / 1.0 ms |
| Attempt history | `delivery_attempts_delivery_idx` | 2.2 ms |
| Idempotency lookup | primary key | 2.8 ms |
| Retention delete batch (5,000) | `events_retention_idx` + anti join `deliveries_event_idx` | 124 ms |
| Auth lookup, secrets, fan-out | sequential scan — tables have 20–40 rows, so the planner correctly prefers it; the indexes were not exercised at this cardinality | 13.6 / 0.8 / 1.2 ms |

Honest caveats: the first capture (before cache warm-up) showed the same plans with larger times (for example Q1 52.9 ms),
so single-run timings are noisy. The tiny-table paths (auth, secrets, fan-out) are **not** evidence that the indexes
work at scale.

### Finding 1 — planner picks the wrong index for sparse filters (fixed)

For a tenant/type combination that matches no rows, the planner estimated ~107,000 matching rows (it assumes
`tenant_id` and `type` are independent), chose a backward scan of `events_retention_idx` expecting an early stop, and
read 53,173 buffers (26.4 s). In my preload, tenant 1 only receives types `payment.succeeded` and `shipment.dispatched`
(an artifact of `g % 2` and `g % 4`), which is exactly the correlation the planner cannot see.

Fix, applied and measured: `CREATE STATISTICS events_tenant_type_stats (dependencies, ndistinct, mcv) ON tenant_id, type`
(`bench/optimizations.sql`), then `ANALYZE events`.

| | Before | After |
| --- | --- | --- |
| Q4b sparse type | 26,389 ms, retention index, 53,173 buffers | **4.1 ms**, `events_tenant_type_created_idx`, 4 buffers |
| Q4a populated type | 2.6 ms | 4.1 ms (retention index still chosen; still fast) |

The unfiltered tenant list (Q3) still uses `events_retention_idx` because the tenant owns ~50% of rows and the first
26 rows hit the filter quickly; its execution was 19.6 ms. This is documented, not "fixed". The statistics object is
an operator recommendation, **not a migration** (expected schema version stays 5); real tenants may or may not show
this correlation.

### Finding 2 — worker throughput vs. ingest latency (optimization rejected)

S3 drained 1,000 deliveries in 21.4 s (about 47 deliveries/s) with worker concurrency 8 and claim batch 16.
Hypothesis: worker concurrency is the limiting resource. Change tried (one change): concurrency 8 → 32, claim batch
16 → 32 (`deploy/compose.yaml`, `optimized-worker.json`).

| | Baseline | Concurrency 32 / batch 32 |
| --- | --- | --- |
| S3 drain | 21.39 s | **2.32 s** |
| S3 ingest p95 / p99 | 123.4 / 160.2 ms | **1231.2 / 1325.4 ms** |
| S2 ingest p95 / p99 | 53.6 / 86.9 ms | 80.6 / 121.4 ms |
| S2 queue wait p95 / p99 | 530 / 719 ms | 262 / 266 ms |

Drain and queue wait improved by an order of magnitude, but burst-time ingest p99 became 1.33 s, breaching the 750 ms
target, on a 4-core machine where the API, worker, PostgreSQL and the load generator compete. Part of the regression
may come from concurrent retention deletes (dataset shrank by ~19k events between runs), which I could not separate
because there was only one run per configuration. **Decision: revert to 8 / 16** (the compose file is back at the
baseline values) and report the trade-off. A deployment with dedicated CPU for the API should re-evaluate; that needs
the 3-repetition runs this report does not have.

## 6. Other limitations of these results

- Single run per scenario, 10–20 s windows, one shared host, Windows/WSL2 Docker overhead.
- S10 measured the limiter's database call in isolation; the plan's enabled-vs-disabled ingest comparison was not run.
- Preload tenants and endpoints are synthetic (`receiver.bench.example.com`); only runs after preload used the live receiver.
- Benchmark tenants (and their rows) from S1–S11 remain in the database.
- No commit SHA/digests; results are indicative only.

## 7. Acceptance criteria

| Criterion | Status |
| --- | --- |
| Baseline and at least one optimization (or documented decision) reported | Met: Q4b statistics (adopted, 26.4 s → 4.1 ms); worker concurrency (rejected, measured) |
| Every named query has `EXPLAIN (ANALYZE, BUFFERS)` at realistic scale | Met for the 1M/2M dataset (small-table paths flagged) |
| Claims match measured numbers | Yes; all figures from `bench/results/` |
| SLO miss stated | S1 p95 163.9 ms at 30 ev/s; S3 p99 under concurrency 32 |
| No correctness test weakened | Yes; correctness tests untouched, unit tests still green |
| Full plan scale (10 min × 3, 1M rows held constant, k6, S4/S5/S7) | **Not met — documented deviation** |

## 8. Artifacts and commands

- `bench/preload.sql`, `bench/explain.sql`, `bench/optimizations.sql`, `bench/k6_ingest.js` (not executed)
- `api/cmd/bench/main.go`; `make bench-preload bench-explain bench-run`
- `bench/results/baseline.json`, `bench/results/optimized-worker.json`, `bench/results/explain-baseline.txt`

## Addendum 2026-10-24 - commit provenance

These benchmark runs executed before the repository's first commit, so no commit SHA or image digest can be
attached to them retroactively. The first commit is
`a95cf077fae8d3e95b360bdc7ac78df55cdcebf7` (2026-10-24); it contains the same benchmark harness and the
recorded results under `bench/results/`. The numbers remain indicative and were not re-run against the
published images.
