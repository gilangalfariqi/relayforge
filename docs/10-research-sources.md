# Phase 0 research sources

Retrieved and checked on **2026-10-22**. Prefer these URLs over blog posts. Patch versions for crates
and Go modules will be re-pinned in Phase 1 lockfiles; this table is the baseline that justified the
architecture, not a substitute for `go.sum` / `Cargo.lock`.

## Language and toolchain

| Topic | Source | Finding used |
| --- | --- | --- |
| Go current stable | https://go.dev/VERSION | `go1.27.3` (2026-10-19) |
| Go release notes | https://go.dev/doc/devel/release | 1.27 and 1.26 supported; 1.27.3 `crypto/x509` security fix (CVE-2026-111943) |
| Go security announce | https://groups.google.com/g/golang-announce/c/66pojueaQek/m/tmWX2Pq9AgAJ | Upgrade recommended |
| Rust stable | https://blog.rust-lang.org/2026/10/15/Rust-1.99.1/ | 1.99.1 (2026-10-15) |
| Rust releases | https://doc.rust-lang.org/stable/releases.html | Confirms 1.99.1 |

## Datastore

| Topic | Source | Finding used |
| --- | --- | --- |
| PostgreSQL 18.6 | https://www.postgresql.org/docs/release/18.6/ | Current 18 minor (2026-08-13); 18.5 skipped |
| `uuidv7()` | https://www.postgresql.org/docs/current/functions-uuid.html | Native time-ordered UUIDs |
| `SELECT` locking / `SKIP LOCKED` | https://www.postgresql.org/docs/18/sql-select.html | Inconsistent view; suitable for queue-like tables |
| Explicit locking | https://www.postgresql.org/docs/current/explicit-locking.html | `FOR UPDATE` semantics |
| PostgreSQL 19 | https://www.postgresql.org/about/news/postgresql-19-rc-1-released-3409/ | 19 RC 1 only; GA planned 2026-11-19; not for v1 |
| Beta policy | https://www.postgresql.org/developer/beta/ | RC not for production |

## Go / Rust libraries

| Topic | Source | Finding used |
| --- | --- | --- |
| Gin 1.12.0 | https://gin-gonic.com/en/blog/releases/release120/ | Released 2026-02-28; Go ≥ 1.25 |
| Gin docs | https://gin-gonic.com/en/docs/ | Confirmed min Go 1.25 |
| pgx v5.11.0 | https://github.com/jackc/pgx/blob/master/CHANGELOG.md | 2026-09-07; v5 is current major |
| pgx tags | https://github.com/jackc/pgx/tags | v5.11.0 latest v5 tag |
| Axum 0.8.9 | https://crates.io/crates/axum/0.8.9 | 2026-04-14; MSRV 1.80 |
| SQLx 0.9.0 | https://crates.io/crates/sqlx | 2026-05-21; MSRV 1.94 |
| SQLx changelog | https://github.com/transact-rs/sqlx/blob/main/CHANGELOG.md | Breaking 0.9; org move to transact-rs |
| reqwest 0.13.5 | https://crates.io/crates/reqwest | 2026-09-08; default features include `system-proxy` |
| reqwest 0.13 notes | https://github.com/seanmonstar/reqwest/releases/tag/v0.13.0 | rustls default; `dns_resolver` API retained |

## HTTP, crypto, webhooks

| Topic | Source | Finding used |
| --- | --- | --- |
| RFC 9457 | https://www.rfc-editor.org/rfc/rfc9457.html | Problem Details; obsoletes RFC 7807 |
| RFC 9110 | https://www.rfc-editor.org/rfc/rfc9110.html | HTTP semantics (status classes) |
| RFC 6585 | https://www.rfc-editor.org/rfc/rfc6585.html | 429 |
| RFC 2104 | https://www.rfc-editor.org/rfc/rfc2104.html | HMAC |
| Standard Webhooks | https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md | Headers, canonical string, `v1` HMAC, rotation |
| HMAC test vector | Independent PowerShell 7 / .NET `HMACSHA256` on 2026-10-22 | Matches `contracts/signature-v1.md` |

## Security guidance

| Topic | Source | Finding used |
| --- | --- | --- |
| OWASP SSRF | https://cheatsheetseries.owasp.org/cheatsheets/Server_Side_Request_Forgery_Prevention_Cheat_Sheet.html | Deny lists, DNS rebinding, redirects, webhooks as SSRF case |
| OWASP Webhook Security | https://cheatsheetseries.owasp.org/cheatsheets/Webhook_Security_Cheat_Sheet.html | HMAC, timestamp, idempotency, connect-time IP, disable redirects |
| IANA IPv4 special-purpose | https://www.iana.org/assignments/iana-ipv4-special-registry/iana-ipv4-special-registry.xhtml | Globally Reachable field |
| IANA IPv6 special-purpose | https://www.iana.org/assignments/iana-ipv6-special-registry/iana-ipv6-special-registry.xhtml | Same |

## Local environment (this workstation, 2026-10-22)

Commands actually run:

| Check | Result |
| --- | --- |
| `git status` | `fatal: not a git repository` |
| `go version` | `go1.24.2 windows/amd64` (EOL relative to 1.27; must upgrade before Phase 1) |
| `rustc` / `cargo` | Not installed |
| `docker version` | Client present; Linux engine pipe `dockerDesktopLinuxEngine` missing |
| `psql` | Not on `PATH` |
| HMAC vector (PowerShell/.NET) | `zWH/4qANrUYMGmZOIkwS0phhFBRxMelsoEkhyx2G71Q=` |

No application test, migration, or benchmark was run. None exists.

## Research limitations

- Crate and module **exact** checksums were not locked; Phase 1 will `go get` / `cargo add` and commit
  lockfiles.
- OWASP cheat sheets are living documents; the architecture snapshots their 2026-10-22 guidance.
- IANA registries can add ranges; the SSRF list is versioned in-repo and must be reviewed on contract
  change.
- Docker image digests for `postgres:18.6-alpine` were not pinned in Phase 0 because Compose files are
  not created until Phase 1.

## Phase 1 environment update (2026-10-22)

- Go 1.27.3 installed locally; `go version` returns `go1.27.3 windows/amd64`.
- Docker Desktop Linux engine running, server 29.5.2.
- Rust 1.99.1 is **not** installed on Windows. `cargo fmt`, clippy, tests, and the image build ran inside
  `rust:1.99.1-bookworm`. `worker/Cargo.lock` pins the resolved dependency graph.
- PostgreSQL 18.6 runs in Compose on host port 55432; host port 5432 was unavailable.
- `go test -race` ran in `golang:1.27.3` because the host lacks cgo support.
- See [12-phase-1-report.md](12-phase-1-report.md) for executed commands and outputs.
