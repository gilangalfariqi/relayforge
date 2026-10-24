# ADR-0010: Separate internal listener for admin and metrics

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

Prometheus metrics, readiness internals, and administrative actions (suspend tenant, inspect stuck
queue, console replay) must not be on the public internet. A single listener with path-based
“security” fails when a reverse proxy is misconfigured.

## Decision

Bind `HTTP_ADDR` (public API) and `INTERNAL_ADDR` (health, metrics, admin) separately. Production
must not route the public proxy to `INTERNAL_ADDR`. Admin routes additionally require
`X-Admin-Token` compared in constant time. Network policy is the primary control; the token is
defence in depth.

`/healthz` and `/readyz` may be on both listeners so orchestrators can probe without the admin token.
`/metrics` is internal only.

## Alternatives considered

| Alternative | Why not |
| --- | --- |
| Path prefix `/internal` on the public port | One proxy mistake exposes metrics and admin |
| mTLS for admin | Correct for later; heavier for v1 |
| No admin API | Operators would use raw SQL without audit |
