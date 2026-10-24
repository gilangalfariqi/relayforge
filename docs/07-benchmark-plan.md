# Benchmark and performance methodology

Status: plan, partially executed in Phase 6. Last updated: 2026-10-23.
This document is the methodology and contains no results. Measured (indicative, reduced-scale) results and
deviations from this plan are in [17-phase-6-report.md](17-phase-6-report.md).

The purpose is to make performance claims falsifiable. Every number published later must be reproducible
from the commands, versions, dataset, and hardware recorded here plus the run manifest.

## 1. Questions to answer

1. What is the maximum sustained ingest rate at p95 ≤ 150 ms with the reference payload mix?
2. Does the claim/lease queue keep first-attempt queue wait p95 ≤ 2 s at the reference load?
3. What is the end-to-end delivery success rate with 20% injected transient failures?
4. Where does the system saturate first: API CPU, PostgreSQL CPU/IO, worker concurrency, claim contention,
   or the rate limiter?
5. How does the system behave under a destination outage, a retry storm, and a worker crash?
6. What is the cost (latency, queries, CPU) of the PostgreSQL-backed rate limiter and idempotency record?
7. Do the chosen indexes produce the expected plans at realistic row counts?

## 2. Non-goals

- Comparing Rust and Go speed. They do different work on the delivery path; a fair comparison would require
  equivalent workloads, and no product decision depends on it.
- Proving production capacity from a laptop or a single CI runner.
- Benchmarking with mocked PostgreSQL. Locking and WAL behaviour are the point.
- Tuning during the baseline run. Baseline first, then one change at a time.

## 3. Reference workload

| Parameter | Value | Rationale |
| --- | --- | --- |
| Sustained ingest | 10 events/s | SLO reference in [requirements](00-requirements.md) |
| Burst | 50 events/s for 60 s | Headroom / burst SLO |
| Endpoints per event | 2 (average), max 50 | Typical fan-out; cap tested separately |
| Payload | 2 KiB mean, p99 64 KiB, max 256 KiB | JSON business events |
| Endpoint mix | 70% fast success, 20% transient 5xx/timeout, 5% permanent 4xx, 5% slow (2 s) | Exercises retries and timeouts |
| Retry policy | 8 attempts, 5 s base, 1 h cap, 24 h window | Defaults |
| Duration | 2 min warmup + 10 min measurement, 3 repetitions | Stable percentiles |
| Dataset preload | 1,000,000 events + 2,000,000 deliveries + attempts | Realistic index/plan sizes before measuring |
| Retention | 30 days configured; preload simulates 30 days at reference | Retention worker disabled during latency runs |

The preload must be generated through SQL `generate_series` with realistic UUIDs and timestamps, not by
running the API for days. Preload queries and timing are part of the manifest.

## 4. Environment capture (mandatory)

Before any run, record:

- CPU model, physical/logical cores, base clock, RAM, disk model and filesystem.
- OS and kernel; Docker Desktop/Engine version; WSL2 vs native Linux.
- PostgreSQL image digest, `shared_buffers`, `work_mem`, `max_connections`, `autovacuum` settings,
  `synchronous_commit`, `fsync`, `checkpoint_timeout`, `max_wal_size`.
- Go and Rust versions, build flags, `GOMAXPROCS`, worker concurrency, pool sizes.
- Compose/container CPU and memory limits.
- Network topology (everything on loopback or a single Docker bridge; no WAN in the critical path).
- Load generator version, host, and number of connections.
- Git commit SHA and whether the working tree is dirty.

If any of these is missing, results are labelled “indicative” and cannot be used to support an SLO claim.

Reference deployment shape (for the first baseline, not a claim):

| Component | Allocation |
| --- | --- |
| PostgreSQL | 4 vCPU, 8 GiB RAM, local SSD |
| API | 2 vCPU, 1 GiB, 1 instance |
| Worker | 1 vCPU, 512 MiB, 2 instances, concurrency 64 each |
| Receiver / load generator | Same host, separate containers, pinned CPUs if available |

A single-host benchmark measures contention as well as the system; this limitation is reported with every
result.

## 5. Scenarios

| ID | Scenario | Method | Primary metrics |
| --- | --- | --- | --- |
| S1 | Ingest-only | k6 POST `/events`, 2 endpoints configured but worker paused | throughput, p50/p95/p99, error rate, DB CPU, WAL |
| S2 | Steady-state end-to-end | S1 + workers + receiver mix | ingest latency, delivery latency, queue wait, success rate, retries |
| S3 | Burst | 50 ev/s for 60 s, then 10 ev/s for 5 min | p99 during burst, backlog drain time, error rate |
| S4 | Destination outage | One endpoint returns 503 for 5 min, then recovers | retry volume, backoff distribution, queue age, recovery time |
| S5 | Slow receiver | 5% of endpoints delay 2 s; some exceed timeout | in-flight count, timeout rate, worker CPU, slot starvation |
| S6 | Permanent failure storm | 100% 4xx for one endpoint for 2 min | dead-letter/failed counts, DB write rate, no infinite retries |
| S7 | Worker crash | `SIGKILL` one worker mid-flight; restart after 10 s | lease recoveries, duplicate attempts, queue age, readiness |
| S8 | Replay storm | Replay 10,000 failed deliveries concurrently | replay latency, cycle increments, claim contention, duplicates |
| S9 | Pagination/read load | 50 rps list events/deliveries with filters | query latency, plan, index usage, buffer hits |
| S10 | Rate limiter cost | Ingest with tenant limit disabled vs enabled; destination limit 10/s | delta p95, DB writes/s, contention |
| S11 | Retention/vacuum | Run cleanup over 1M rows while serving S2 | latency impact, bloat, autovacuum activity |

## 6. Load generation

- k6 (pinned image) for HTTP ingest and read load. Scripts live in `bench/` and are versioned.
- A deterministic pseudo-random payload generator, seeded, so runs are comparable. Payloads are valid JSON
  of the configured size distribution.
- The receiver is a dedicated Go service with programmable behavior and Prometheus metrics. It records
  request latency and duplicate event IDs.
- Ingest uses unique idempotency keys per virtual user per iteration; a separate scenario intentionally
  repeats keys to measure idempotency overhead.
- Connection reuse is enabled; TLS is terminated locally (or disabled in a clearly labelled run) because
  TLS handshakes would measure the proxy, not the app. If TLS is disabled, the result is labelled.
- The load generator is not co-located on a saturated CPU; if it is, CPU steal is reported.

## 7. Metrics and instrumentation

| Metric | Source |
| --- | --- |
| Throughput, error rate | k6 summary + `relayforge_http_requests_total` |
| p50/p95/p99 ingest latency | k6 + Prometheus histogram |
| Delivery latency and queue wait | `delivery_attempts.started_at - deliveries.created_at`, Prometheus histograms |
| Retry volume and distribution | `relayforge_delivery_attempts_total`, attempt table grouped by outcome/attempt_no |
| Queue depth and oldest age | `relayforge_queue_due`, `relayforge_queue_oldest_age_seconds`, SQL |
| DB CPU/IO/latency | `pg_stat_statements`, `pg_stat_database`, `pg_stat_user_tables`, `EXPLAIN (ANALYZE, BUFFERS)` |
| Pool saturation | `relayforge_db_pool_acquired/max`, `pg_stat_activity` |
| Worker CPU/memory | `docker stats`, cgroup metrics |
| Duplicate deliveries | receiver dedupe table + `relayforge_lease_recoveries_total` |
| Disk growth/bloat | table/index sizes, `n_dead_tup`, WAL generation |

Percentiles are computed over the full measurement window, not a single 10-second slice. Histogram buckets
are chosen so p99 is not interpolated across a huge bucket.

## 8. Procedure

1. Freeze the commit; record SHA and dirty state.
2. Build images with the recorded toolchains; record image digests.
3. Start a fresh PostgreSQL volume; apply migrations; preload the dataset; run `ANALYZE`.
4. Warm up for 2 minutes at the target rate.
5. Run the scenario for 10 minutes; collect metrics every 5 s.
6. Stop load; allow the queue to drain to zero; record drain time.
7. Snapshot `EXPLAIN (ANALYZE, BUFFERS)` for the named access paths; export `pg_stat_statements`.
8. Repeat 3 times from a fresh database (or document why reuse is safe).
9. Report median and min/max of the three runs; do not cherry-pick.

Failure injection is controlled by the receiver and by SQL state changes, not by timing sleeps. For S7, the
worker is killed with `SIGKILL` and restarted by Compose; the driver waits for readiness.

## 9. Query-plan analysis

Run before and after each optimization:

```sql
EXPLAIN (ANALYZE, BUFFERS)
SELECT id FROM deliveries
WHERE status IN ('pending','retry_scheduled')
  AND next_attempt_at <= now()
ORDER BY next_attempt_at, id
FOR UPDATE SKIP LOCKED
LIMIT 20;

EXPLAIN (ANALYZE, BUFFERS)
SELECT id, type, status, created_at
FROM events
WHERE tenant_id = $1
  AND (created_at, id) < ($2, $3)
ORDER BY created_at DESC, id DESC
LIMIT 25;

EXPLAIN (ANALYZE, BUFFERS)
SELECT id, status, attempt_count, next_attempt_at
FROM deliveries
WHERE tenant_id = $1 AND status = 'dead_lettered'
ORDER BY created_at DESC, id DESC
LIMIT 25;
```

Record: plan node, rows, actual time, buffers, index used, whether a sort or sequential scan appeared.
A plan change without a measured regression is not automatically a defect, but it must be explained.

## 10. Optimization rules

- Optimize only a measured bottleneck, with a hypothesis and a before/after run.
- One change at a time; rerun the same scenario and environment.
- Reject optimizations that weaken correctness (for example, dropping lease fencing for speed).
- Report negative results. If a change does not help, revert it and say so.
- If a target is not met, report the shortfall and the limiting resource. Do not lower the target without a
  stated product reason.

## 11. Reporting template

```markdown
### Run <id> — <scenario>
- Commit / image digests:
- Date, operator, environment:
- Workload: rate, duration, payload mix, endpoints, failure mix
- Result: throughput, error rate, p50/p95/p99 ingest, delivery success, queue wait, drain time
- Resources: API CPU, worker CPU, PG CPU, IOPS, memory, pool wait
- Plans: claim / list events / list deliveries (attach)
- Observations and limiting resource:
- Comparison to previous run (if any):
- Limitations:
- Artifacts: bench/results/<id>/
```

## 12. Acceptance and decision points

Initial targets (from requirements, not measurements):

- S2 at 10 ev/s: ingest p95 ≤ 150 ms, p99 ≤ 400 ms, error rate < 0.1%.
- First-attempt queue wait p95 ≤ 2 s.
- Delivery success for healthy endpoints ≥ 99.5% after injected transient failures.
- S3 burst: p99 ≤ 750 ms, error rate < 0.1%, backlog drains within 5 minutes.
- S7: lease recovery within 2 × lease (180 s); no lost deliveries; duplicates bounded and counted.
- No named query degrades to a sequential scan on a table > 1M rows.

If a target fails, the Phase 6 report states the measured value, the bottleneck, and the proposed change.
The SLO document is revised only with evidence and a product decision.

## 13. Known benchmark limitations

- Single-host Docker adds contention and is not a production capacity model.
- Docker Desktop on Windows/WSL2 has filesystem and networking overhead; results may differ from Linux.
- The receiver and load generator share hardware and can distort tail latency.
- Synthetic payloads may not match real JSON shapes or database access patterns.
- A 1M-row preload is not a 100M-row production table; index depth and vacuum behaviour differ.
- Retry-storm scenarios compress time; real outages last hours.
- No multi-AZ, replication, or failover latency is measured.

All published numbers will carry these caveats. A number without its manifest is not evidence.
