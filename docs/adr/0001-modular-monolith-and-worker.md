# ADR-0001: Go modular monolith and separately deployable Rust worker

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

The brief requires a Go control plane (Gin, public API) and a Rust delivery data plane (Tokio, outbound
HTTP). The split must have an operational reason, not a stylistic one. The control plane must keep
handlers free of business logic and keep HTTP types out of domain modules.

## Decision

Ship a **modular Go monolith** for all tenant-facing and administrative HTTP, plus a **separately
deployable Rust worker** that claims work from PostgreSQL and performs outbound delivery.

The Go process does not call the Rust process. They share a schema and a versioned contract
(`contracts/delivery-contract-v1.md`). The worker exposes only an internal health/metrics listener.

## Why the split is operational

- Outbound delivery is long-tailed I/O against untrusted hosts. Isolating it from the ingest latency path
  lets operators scale, restart, and rate-limit workers without taking ingestion down.
- Privilege profiles differ: the worker needs decrypt access to signing secrets and public egress; the
  API needs inbound HTTPS and must not make customer-controlled connections.
- A worker crash, memory growth, or slow-receiver pile-up must not fail `/readyz` for ingest.

## Language choice

Rust vs Go on the delivery path is **mandated by the project brief**. This ADR does **not** claim Rust is
faster than Go for this workload. Both sides are I/O-bound. Memory-safety for parsing untrusted HTTP
responses is a supporting argument, not a measured performance claim.

## Alternatives considered

| Alternative | Why not |
| --- | --- |
| Single Go binary doing ingest and delivery | Simpler ops locally; couples ingest availability to hostile egress and long-tail I/O |
| Microservices for tenants, credentials, events | Extra network hops and dual-write risk without a measured scaling need |
| Worker as a library in-process | Cannot isolate crashes, privileges, or scaling |
| gRPC between API and worker | Reintroduces a publish step after commit; dual-write failure window |

## Consequences

- Two deployables, two toolchains, one schema. Compatibility is an explicit contract and a readiness check.
- Local Compose must run both; CI must build both.
- Operators can run N workers against one API and one primary.
