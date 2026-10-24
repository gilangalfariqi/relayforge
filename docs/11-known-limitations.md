# Known limitations (v1)

Status: Phases 1–7 complete. Last updated: 2026-10-23.
These are product and engineering limits, not bugs to be silently "fixed" in implementation.

## Delivery semantics

- At-least-once only. Duplicate windows F6–F9, shutdown-abort, and operator replay are real.
- No ordering across deliveries or endpoints.
- No exactly-once business side effects; receivers must persist `webhook-id` durably.

## Security

- No RLS. Isolation is application-tested, not database-enforced.
- Shared KEK between API and worker. There is no KEK version migration or re-encryption job.
- Static admin token; no per-operator identity.
- Audit rows are insert-only by application convention. The application database role owns the table, so a
  compromised process could update or delete rows; this is not WORM or enforced by grants.
- Tenant deletion / GDPR erasure is not implemented.
- TLS is terminated at a reverse proxy in the reference design; the Go process does not terminate TLS.
- The signup limiter is in process memory, so its 5/min/IP limit is per API replica and resets on restart.
- Production egress policy is deployment-specific and was not deployed or tested; Compose allows
  `RELAYFORGE_ALLOW_INSECURE_LOCAL_DELIVERY=true` for the local demo only.

## Data and operations

- Single PostgreSQL primary is the durability boundary.
- Retention runs in the API (30 days default; 7 days documented for local Compose is **not** what Compose
  configures — the default is 720 h). It deletes history by policy.
- Fixed-window rate limits can burst 2× at window edges.
- Destination URL changes apply to queued deliveries; there is no URL history table.
- Soft-deleted endpoints do not immediately shred signing secrets.
- JSON Schema validation is optional and can add ingest latency. External `$ref` fetching is disabled.
- Event responses are JSONB-normalised; original request bytes are preserved for signing.
- The last-owner guard has a window during credential rotation overlap.
- The OpenAPI contract test compares operations and paths, not every schema.
- The receiver's behaviour (`/control/behavior`) is global, not per endpoint; the demo and benchmarks rely on it.
- Extended statistics on `events (tenant_id, type)` are an operator recommendation
  (`bench/optimizations.sql`), not a migration; without them the planner may pick a slow plan for sparse
  tenant/type filters ([17-phase-6-report.md](17-phase-6-report.md)).

## Performance (see [17-phase-6-report.md](17-phase-6-report.md))

- Benchmarks ran for 10–20 s windows, once per scenario, on a 4-core laptop with every component co-located.
  They are **indicative** and cannot support an SLO claim. No commit SHA or image digest was recorded for
  that run.
- S4, S5 were not run; the k6 script was not executed; retention was not disabled during latency runs.
- Higher worker concurrency (32) cut burst drain from 21.4 s to 2.3 s but raised burst ingest p99 to 1.33 s on
  the test host; it was rejected, not tuned further.
- S1 ingest p95 at 30 ev/s was 163.9 ms (above 150 ms).

## What has not been proven

- The GitHub Actions workflow covers the jobs listed in [06-testing-strategy.md](06-testing-strategy.md) §8.
  Checks outside that list (standalone OpenAPI schema validation, image scanning, `cargo audit`, the
  `SIGKILL` drill, and the k6 load run) were executed locally only; their evidence is in the phase reports.
- Rust was built and tested in a pinned container, not with a native Windows toolchain.
- No coverage number is claimed.
- No multi-AZ, replication, or failover behaviour was measured.

Do not describe the system as production-ready. Security and reliability behaviours listed in the phase reports
were executed locally; production capacity and egress controls were not.
