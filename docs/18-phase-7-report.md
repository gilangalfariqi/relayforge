# Phase 7 report — Portfolio release

Date: 2026-10-23. Status: demo and documentation complete. The CI, commit, security-policy, and curl gaps
recorded below were closed on 2026-10-24; see the addendum at the end.

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


## Addendum 2026-10-24 - repository, CI, and release state

The gaps listed above were closed after this report was written.

- **First commit and remote.** `a95cf077fae8d3e95b360bdc7ac78df55cdcebf7` on `main`, pushed to
  <https://github.com/gilangalfariqi/relayforge> (public, no history rewritten).
- **CI green.** [ci run 40077250780](https://github.com/gilangalfariqi/relayforge/actions/runs/40077250780)
  passed every job: Go control plane (gofmt, vet, build, staticcheck v0.8.2, `go test -race`, govulncheck
  v1.9.0), Rust worker (fmt, clippy, tests), PostgreSQL and integration (migrations up/down/up, Go
  integration, Rust `delivery_integration`), Container images (Compose config plus api/receiver/worker
  builds), End-to-end delivery (live Compose lifecycle), and Secret scan (gitleaks over full history).
- **Publish green.** [publish run 40077250807](https://github.com/gilangalfariqi/relayforge/actions/runs/40077250807)
  pushed `linux/amd64` images for `api`, `worker`, and `receiver` to
  `ghcr.io/gilangalfariqi/relayforge/`, with SBOM and provenance. The workflow now runs from
  `workflow_run` after a successful `ci` on `main`, so a failed CI run cannot publish; `v*` tags still
  publish directly and must be cut from a green `main` commit.
- **Pinning.** Third-party actions are pinned by commit SHA; `govulncheck` and `staticcheck` are pinned by
  version. The Dockerfiles expose explicit `api`, `receiver`, and `worker` targets so each published image
  has the correct entrypoint.
- **Policy and examples.** [SECURITY.md](../SECURITY.md) adds private vulnerability reporting and scope.
  [19-curl-examples.md](19-curl-examples.md) collects the public and internal operations; it is derived
  from the contract and the E2E flows, not a recorded transcript.
- **Dependency updates.** Dependabot watches Go, Cargo, Actions, Dockerfiles, and Compose on a weekly
  grouped schedule.

Remaining gaps are unchanged: full-scale benchmarks (10-minute runs, k6, S4/S5), branch protection rules,
and replaying the curl collection end to end. GHCR package visibility follows the repository settings and
was not verified from an unauthenticated client.
