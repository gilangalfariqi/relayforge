# ADR-0004: UUIDv7 primary keys and text CHECK statuses

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

IDs must be unique, non-enumerable enough not to be the isolation control, and cheap to insert in
time order. PostgreSQL 18 provides native `uuidv7()`
(https://www.postgresql.org/docs/current/functions-uuid.html). Status columns need a closed set that
can grow without `ALTER TYPE` locking surprises.

## Decision

- Primary keys for tenant-owned entities are `uuid` defaulting to `uuidv7()` inside the database.
- Attempt and audit IDs are `bigint IDENTITY` because they are append-only and internal.
- Status columns are `text` with `CHECK (status IN (...))`, not PostgreSQL enums.

## Alternatives considered

| Alternative | Why not |
| --- | --- |
| UUIDv4 | Random inserts fragment B-tree indexes |
| `bigint` sequences for events | Guessable; still not the isolation control, but worse operationally for merge/replication |
| Application-generated ULIDs | Extra library; generation outside the transaction |
| Native ENUM | Adding a value can require awkward lock/migration sequences |
