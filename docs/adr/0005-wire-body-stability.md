# ADR-0005: Store exact ingest bytes for signing and delivery

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

HMAC is computed over raw body bytes. Re-serialising JSONB does not preserve key order, whitespace, or
number formatting. Receivers that verify the signature against the bytes on the wire will fail if the
sender re-serialises on retry.

## Decision

Store `events.wire_body bytea` as the exact request bytes accepted at ingest, cap 256 KiB. Also store
parsed `payload jsonb` for inspection. Workers send and sign `wire_body` unchanged on every attempt and
replay. Timestamp and signature headers change; body bytes do not.

Duplication is bounded by the size cap. Signature stability is more important than storage elegance.

## Alternatives considered

| Alternative | Why not |
| --- | --- |
| Re-serialise JSONB with sorted keys | Still diverges on numbers/unicode; receivers using raw bytes fail |
| Canonical JCS (RFC 8785) | Extra complexity; not Standard Webhooks |
| Store only JSONB | Classic production signature failure |
