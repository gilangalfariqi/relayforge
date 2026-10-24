# Phase 7 report — Portfolio release

Date: 2026-10-23. Status: **demo and documentation complete; CI/commit gaps remain and are stated.**

## Changes

- `make demo` (`api/cmd/demo/main.go`): live 7-step walkthrough against the Compose stack.
- `Makefile`: `demo`, `bench-preload`, `bench-explain`, `bench-run` targets and help text.
- `.github/workflows/ci.yml`: added `govulncheck` step and PostgreSQL-backed integration tests in the
  migrations job. **Neither has run on GitHub.** `govulncheck@latest` is unpinned — a deviation from the
  pinned-actions policy; pin it before relying on CI.
- Documentation synced: README (status, demo, layout, doc map, environment facts), architecture, API,
  threat model, testing strategy, benchmark plan, implementation plan, known limitations.

## Executed evidence

`make demo` equivalent (`go run ./cmd/demo`) against the live stack, recorded in
`bench/results/demo-output.txt`:

- Idempotent ingest: second POST with the same key returned the same event ID with
  `Idempotency-Replayed: true`.
- Transient failure: attempt 1 `retryable_failure` HTTP 503, attempt 2 `success`; delivery `delivered`,
  event `delivered`.
- Outage: 3 attempts all 503, delivery `dead_lettered`, event `failed`.
- Replay (`POST /events/:id/replay` → 202): cycle 2 attempt 1 `success`; cycle 1 history preserved
  (4 attempt rows); event `delivered`.
- Audit entries `event.replay`, `endpoint.create`, `tenant.create`; receiver `effects_applied=2`,
  `duplicates_ignored=0`, `auth_failures=0`, `total_attempts=6`.

Final checks on this tree: `gofmt -l` clean, `go vet ./...` exit 0, `go test ./...` all packages `ok`,
`docker compose config` valid. Phase 4/5 reports hold the integration, race, fuzz, crash-drill and scan
results (govulncheck 0, gitleaks 0, Trivy 0 HIGH/CRITICAL); they were **not re-run** in Phase 7.

## Defects found and fixed while building the demo

- Demo/bench code assumed plain-HTTP destinations; the API correctly rejects non-HTTPS endpoint URLs.
  Fixed to use the HTTPS receiver URL from the E2E suite and register the endpoint signing secret with the receiver.
- Replay returns 202 (per OpenAPI), not 200.
- Signup response shape (`api_key` is a string) differs from what the first draft assumed.

## Acceptance

| Criterion | Status |
| --- | --- |
| `make demo` shows ingest → retry → delivery → replay | Met (recorded output) |
| Documentation matches code and API spec | Updated; OpenAPI contract test passes in `go test` |
| All tests pass | Unit tests pass; integration/e2e suites last ran in Phase 5 |
| CI green | **Not demonstrated** — workflow has never run on GitHub |
| Image scans reported | Phase 4 results; not re-run |
| Security review checklist / disclosure policy | **Not written** |
| Curl collection | **Not written**; README and API docs contain curl examples |
| No claim without evidence | Performance claims limited to indicative Phase 6 numbers |

## Remaining gaps

1. No git commit exists; create one so benchmarks can carry a SHA.
2. Run CI on GitHub and pin `govulncheck`.
3. SECURITY.md and a curl collection.
4. Full-scale benchmark repetitions (10 min × 3, retention disabled, k6, S4/S5).
