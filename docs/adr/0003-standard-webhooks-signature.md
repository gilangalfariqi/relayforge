# ADR-0003: Standard Webhooks HMAC scheme

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

Receivers need a precise canonical representation, timestamped signatures, and rotation. Inventing a
bespoke header set would force every customer to write a custom verifier.

The [Standard Webhooks specification](https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md)
defines `webhook-id`, `webhook-timestamp`, `webhook-signature` and the string
`id + "." + timestamp + "." + raw_body` with HMAC-SHA256 (`v1`). OWASP Webhook Security Cheat Sheet
recommends HMAC-SHA256, timestamp tolerance, and event-ID deduplication.

## Decision

Adopt Standard Webhooks at the wire level. Add RelayForge-specific headers (`RelayForge-Delivery`,
`RelayForge-Attempt`, `RelayForge-Event-Type`) as additive metadata, unsigned except insofar as they
travel with a signed body. Event IDs in production are UUIDv7; the published test vector uses a
`msg_…` ID to stay compatible with the Standard Webhooks examples.

Timestamp tolerance is 300 seconds. Durable idempotency on `webhook-id` is mandatory for receivers;
the timestamp window is not a substitute.

## Alternatives considered

| Alternative | Why not |
| --- | --- |
| Stripe-style `t=,v1=` concatenated signed payload | Equivalent security; less library reuse |
| Asymmetric `v1a` (Ed25519) | Extra key management; deferred |
| Sign only the body | Forged timestamps and swapped IDs |
| Canonical JSON before signing | Fragile; we store and send `wire_body` instead ([ADR-0005](0005-wire-body-stability.md)) |
