# Implementation plan and acceptance criteria

Status: Phases 0–7 implemented and verified locally. Last updated: 2026-10-23.
All phases (Phase 0 through Phase 7) are complete with executed acceptance checks and recorded evidence in
[12-phase-1-report.md](12-phase-1-report.md), [13-phase-2-report.md](13-phase-2-report.md),
[14-phase-3-report.md](14-phase-3-report.md), [15-phase-4-report.md](15-phase-4-report.md),
[16-phase-5-report.md](16-phase-5-report.md), [17-phase-6-report.md](17-phase-6-report.md), and
[18-phase-7-report.md](18-phase-7-report.md).

## Phase 0 — Discovery and architecture (this deliverable)

### Deliverables

- Repository inspection (this session): design documents and contracts only; no
  application code, migrations, tests, Dockerfiles, or `.git`. Earlier drafts
  already existed in `docs/` and `contracts/`; they were incomplete relative to
  the README (missing ADRs, runbook, research sources, event envelope).
- Toolchain and environment inspection.
- Current-version research from primary sources.
- Requirements, assumptions, non-goals, priorities, SLO targets.
- Architecture, module boundaries, transaction/failure analysis.
- Data model, constraints, indexes, retention, RLS assessment.
- Versioned delivery contract and signature specification.
- SSRF policy and shared conformance vectors.
- API draft, threat model, testing strategy, benchmark plan, incident runbook.
- ADRs and known limitations.

### Acceptance criteria

- [x] All documents listed in the README exist and are internally consistent.
- [x] No application code, migration, or test is claimed as implemented.
- [x] Every performance and security statement is either a design decision, a target, or explicitly
      marked unverified.
- [x] Environment limitations are disclosed.
- [x] **Approval to proceed to Phase 1.** (Approved 2026-10-22 with the proposed defaults.)

### Open decisions

See [requirements §8](00-requirements.md). Defaults are proposed; approval resolves them.

## Phase 1 — Foundation (implemented 2026-10-22)

Status: acceptance checks executed locally; see [12-phase-1-report.md](12-phase-1-report.md). The GitHub
Actions workflow exists and its YAML parses, but it has **not** run on GitHub.

### Prerequisites (blocking)

1. Install Go 1.27.3 (current 1.24.2 is end-of-life). Verify `go version`.
2. Install Rust 1.99.1 via rustup with the MSVC toolchain on Windows, or use a Linux container for builds.
   Verify `rustc --version`, `cargo --version`.
3. Start Docker Desktop and verify `docker info` succeeds. If the Linux engine cannot run, use WSL2 Ubuntu
   for PostgreSQL and worker builds, and document the deviation.
4. Decide the Phase 0 open questions (signup mode, port allowlist, schema validation, replay scope,
   retention, shared KEK).

No phase-1 acceptance will be reported if these are missing; a workaround must be labelled as such.

### Work

- `git init`, `.gitignore`, `.gitattributes` (LF for source, CRLF policy explicit), `.editorconfig`.
- `Makefile` with `setup`, `fmt`, `lint`, `test`, `integration`, `e2e`, `migrate-up`, `migrate-down`,
  `bench`, `check`.
- Go module `api/`, config loader with fail-fast validation, structured logging, request IDs, error
  envelope, body limits, health/readiness, metrics registry, migration command.
- Rust workspace `worker/`, config loader, SQLx pool, tracing JSON, health/metrics listener, schema-version
  check, graceful shutdown skeleton.
- SQL migrations `0001`–`0005` matching the data model, with up/down.
- Dockerfiles (multi-stage, non-root, pinned digests where practical), Compose with `postgres`, `api`,
  `worker`, `receiver` placeholder, networks, healthchecks.
- CI skeleton: Go fmt/vet/test, Rust fmt/clippy/test, migrations up/down/up, Docker build, secret scan.
- OpenAPI 3.1 skeleton and spec-serving route.
- `docs/` links validated; no secrets in the repository.

### Acceptance criteria

| Check | Command / evidence |
| --- | --- |
| Go builds | `cd api && go build ./...` |
| Go formatting | `gofmt -l .` returns no files |
| Go vet | `go vet ./...` |
| Go tests | `go test -race ./...` |
| Rust formatting | `cargo fmt --all -- --check` |
| Rust clippy | `cargo clippy --all-targets -- -D warnings` |
| Rust tests | `cargo test --all` |
| Migrations | `migrate up`, `down 0`, `up` on empty PostgreSQL 18.6 |
| API health | `GET /healthz` 200; `/readyz` 200 with DB and schema |
| Worker readiness | `/readyz` 200; wrong schema version → 503 |
| Compose | `docker compose up -d` reaches healthy; no service restarts |
| CI | Equivalent local commands pass; GitHub Actions workflow valid |
| Secrets | `gitleaks detect` finds none in tracked files |
| Images | Build succeeds; containers run as non-root |

### Result (executed 2026-10-22)

| Check | Result |
| --- | --- |
| Go build / vet / test | Pass (`go build ./...`, `go vet ./...`, `go test -count=1 ./...`) |
| Go race | Pass in `golang:1.27.3` with `go test -race -count=1 ./...` |
| Rust fmt / clippy / test | Pass in `rust:1.99.1-bookworm`; 7 tests |
| Migrations | `up` → 5, `down` → none, `up` → 5 on PostgreSQL 18.6 |
| API health/readiness | `200` ready with schema 5; `503 not_ready` with expected schema 99 |
| Worker health/readiness | `200` ready with schema 5; `503` with expected schema 99 |
| Compose | API, worker, PostgreSQL healthy; 0 restarts; migration exit 0 |
| Images | API and worker build; both run as UID/GID 65532 |
| OpenAPI | Redocly lint valid; 2 warnings (documented) |
| CI YAML | Parsed with `yq`; not executed on GitHub |
| Secret scan | `gitleaks --no-git` finds no leaks after replacing example credentials |
| Graceful shutdown | Worker `SIGTERM` stops loop and exits 0 |

### Phase report

Files added, commands run with results, failing checks, deviations, and the next phase.

## Phase 2 - Correctness first

Status: implemented 2026-10-22; executed evidence is in [13-phase-2-report.md](13-phase-2-report.md). The
delivery worker still only checks readiness, so no event is delivered over HTTP in this phase.

### Work

- Tenant registration and suspension.
- API credential creation, hashing, rotation, revocation, last-owner guard.
- Endpoint CRUD, registration-time SSRF validation, pause/resume/soft delete.
- Endpoint secret generation, AES-256-GCM encryption, metadata listing, retirement guard.
- Event ingest: envelope validation, size/depth limits, optional JSON Schema, idempotency, `wire_body`
  storage, atomic fan-out, `no_subscribers`.
- Read APIs: events, deliveries, attempts, audit; cursor pagination.
- Append-only audit for all security-sensitive actions.
- Integration tests for transactions, idempotency concurrency, tenant isolation, rollback.

### Acceptance criteria

- Duplicate ingest (100 concurrent, same key/body) → exactly one event, one set of deliveries, identical
  responses; different body → `409`.
- Fault injection after event insert → zero event/delivery/idempotency rows.
- Cross-tenant matrix returns `404` for every resource and every verb.
- API key plaintext never appears in DB or logs; digest is 32 bytes; constant-time compare.
- Secret plaintext appears only in create/rotate responses; DB contains ciphertext with correct AAD.
- Oversized/deep/invalid payloads produce `413`/`422` with no rows.
- Event/delivery/attempt list queries use the intended indexes (`EXPLAIN` captured).
- `go test -race`, integration tests, and OpenAPI contract tests pass.
- `go vet`, `staticcheck`, `govulncheck` pass (or findings triaged and documented).

## Phase 3 — Delivery engine

### Work

- Rust claim loop with batch limit, lease token, attempt row, concurrency caps.
- PostgreSQL destination rate limiter and deferral path.
- reqwest client: no proxy, no redirects, TLS verification, connect/read/total timeouts, 8 KiB response cap.
- HMAC-SHA256 signing with rotation (all active secrets), canonical headers.
- Failure classification and retry policy (single implementation), backoff with jitter, retry window.
- Completion transaction with lease fencing and event projection.
- Expired-lease reaper and abandoned-attempt handling.
- Graceful shutdown and lease release.
- Demo receiver with raw-body HMAC verification, timestamp check, durable dedupe, duplicate-safe effect.
- E2E: API + PostgreSQL + worker + receiver.

### Acceptance criteria

- Concurrent claim test: no duplicate claims, one attempt per claim.
- Status matrix test: every documented status class behaves as specified.
- Backoff property test: bounds and cap hold; no overflow.
- Signature test vector passes in Rust and the independent receiver.
- Slow receiver test records timeouts; no transaction is held during HTTP.
- SIGKILL test: lease recovered within 2× lease; attempt `abandoned`; no lost delivery.
- Stale completion affects zero rows and writes no outcome.
- Rate-limit deferral does not consume an attempt and does not lose work.
- Graceful shutdown exits within the configured grace and releases unstarted work.
- E2E success, retry, permanent failure, dead-letter, and duplicate handling pass.

## Phase 4 — Security and operations

### Work

- Connect-time SSRF enforcement with custom resolver; shared vectors green in both services.
- Redirect/proxy regression tests; IPv4/IPv6 transition cases.
- Tenant ingest and management rate limits; destination limits under concurrency.
- Secret rotation with overlap and revocation; KEK versioning and re-encryption job (if in scope).
- Audit coverage for every sensitive action; redaction tests.
- Dead-letter management and authorised replay with cycle semantics.
- Dashboards (Prometheus/Grafana JSON or documented PromQL), alerts, runbooks.
- Admin API on the internal listener; readiness details.
- Dependency and image scanning; CI hardened (pinned actions, least privilege).

### Acceptance criteria

- All SSRF vectors pass at both stages; DNS-rebinding test blocks the second attempt.
- `HTTP_PROXY` is ignored; a redirect to metadata is never followed.
- Rate limits return `429` with `Retry-After`; destination deferral preserves work.
- Rotation: both secrets valid during overlap; retired secret rejected; no plaintext in logs.
- Replay: cycle increment, history preserved, concurrent replay single winner, audit present.
- Metrics and alerts documented with thresholds; `/metrics` not publicly exposed.
- Runbook commands executed in a staging-like environment and corrected where wrong.
- Trivy/gitleaks/govulncheck/cargo-audit results recorded.

## Phase 5 — Validation

### Work

- Full unit, integration, contract, concurrency, failure-recovery, security, and E2E suites.
- Go fuzz targets; Rust `proptest`/`cargo-fuzz`.
- Race detector; repeated concurrency runs (e.g. 20 iterations) to expose flakiness.
- Crash/failover drills: worker SIGKILL, DB restart, expired leases, duplicate storms.
- OpenAPI validation against handlers.
- Fix defects before scope expansion.

### Acceptance criteria

- All blocking suites pass; failures listed with root cause.
- No flaky test retries hidden; quarantined tests have an issue and a reason.
- Fuzz targets run for a defined budget without panics (budget recorded).
- A full E2E run passes from a clean checkout and clean database.
- Every threat-model mitigation marked “tested” has an executed test.

## Phase 6 — Performance

### Work

- Preload 1M events / 2M deliveries; run scenarios S1–S11 from the benchmark plan.
- Capture p50/p95/p99, throughput, error rate, queue age, resource usage, DB plans.
- Identify the limiting resource; optimize one change at a time.
- Publish a benchmark report with environment, dataset, commands, raw artifacts, limitations.

### Acceptance criteria

- Baseline and at least one optimization (or a documented decision not to optimize) are reported.
- Every named query has an `EXPLAIN (ANALYZE, BUFFERS)` at realistic scale.
- Claims match measured numbers; no invented results.
- If an SLO is missed, the shortfall and bottleneck are stated explicitly.
- No correctness test is weakened for performance.

## Phase 7 — Portfolio release

### Work

- Final README with architecture diagram, quick start, demo walkthrough, screenshots/ASCII where useful.
- Complete OpenAPI document with examples; curl collection.
- Threat model, ADRs, benchmark report, known limitations, runbooks.
- Demo receiver documentation and a one-command demo.
- Reproducible setup on a clean machine; pinned versions; CI green.
- Security review checklist and disclosure policy.

### Acceptance criteria

- A new engineer can run `make demo` and observe ingest → retry → delivery → replay.
- Documentation matches the code and the API spec.
- All tests pass; CI is green; image scans are reported.
- Known limitations are explicit; no security or performance claim lacks evidence.

## Cross-phase rules

- Report after each phase: changes, files, commands and results, failing tests, decisions, acceptance
  status, next phase.
- No placeholder security or reliability feature may be marked complete.
- No secret in git; no destructive command without explicit approval.
- If an environment blocks a check, state the limitation and provide a verifiable alternative; never claim
  a skipped check passed.
- Keep changes small and reviewable; do not rewrite unrelated files.

## Dependency graph

```text
Phase 1 (foundation)
   └── Phase 2 (tenancy, ingest, idempotency, schema)
          ├── Phase 3 (delivery engine)
          │      └── Phase 4 (security/ops, replay, SSRF)
          │             └── Phase 5 (validation)
          │                    └── Phase 6 (performance)
          │                           └── Phase 7 (release)
          └── (Phase 4 read APIs can start after Phase 2)
```

Phase 3 depends on the Phase 1 worker skeleton and the Phase 2 schema. Phase 4 SSRF depends on the worker
HTTP client. Phase 6 depends on a stable feature set; benchmarking before Phase 5 would measure a moving
target.

## Schedule realism

This is a portfolio-grade build, not a weekend CRUD app. The critical path is Phase 3 correctness
(leases, fencing, retries) and Phase 5 failure injection. If time is constrained, reduce scope explicitly
(for example defer JSON Schema validation) rather than weakening concurrency or security controls.
