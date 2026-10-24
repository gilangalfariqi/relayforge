# ADR-0007: Defer PostgreSQL row-level security

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

The brief allows RLS as an additional defence only if the session and role model can be implemented
safely. The API uses a connection pool as one role. The worker claims across tenants.

## Decision

**Do not enable RLS in v1.** Isolation is structural: `tenant_id` on every owned table, required on
every repository method, `404` on miss, integration tests for the full cross-tenant matrix.

If RLS is added later: `NOBYPASSRLS` app role, `SET LOCAL app.tenant_id` only via a connection wrapper,
policies on every table, worker uses a separate bypass role for claiming, tests fail if a query runs
without the GUC.

## Why not now

A missed `SET LOCAL` on a pooled connection either leaks data or fails closed unpredictably. A worker
bypass role expands the privilege model. Application tests plus mandatory tenant predicates are the
honest v1 control; there is **no database backstop yet**, and the threat model says so.

## Alternatives considered

| Alternative | Why not |
| --- | --- |
| RLS from day one on the pooled role | Easy to get wrong; false sense of safety |
| One connection per request with SET | Destroys pooling |
