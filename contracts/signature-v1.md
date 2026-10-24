# RelayForge webhook signature v1

Status: proposed, Phase 0. Last updated: 2026-10-22.
Part of [delivery contract v1](delivery-contract-v1.md).

This scheme is deliberately compatible with the [Standard Webhooks specification](https://github.com/standard-webhooks/standard-webhooks/blob/main/spec/standard-webhooks.md)
so that receivers can use existing verification libraries and our demo receiver is not a bespoke format.
Compatibility is at the wire level (headers, canonical string, HMAC); RelayForge-specific headers are
additive.

## 1. Goals and non-goals

Goals:

- Prove authenticity and integrity of the exact bytes sent.
- Authenticate the event ID and timestamp, not just the body.
- Allow secret rotation without dropping deliveries.
- Be implementable in any language with HMAC-SHA256 and base64.
- Fail closed on missing, malformed, stale, or mismatched signatures.

Non-goals:

- Confidentiality. TLS provides that; HMAC does not encrypt.
- Non-repudiation against the platform operator. HMAC is symmetric.
- Exactly-once processing. Replay protection is bounded by the timestamp window; durable event-ID
  deduplication is the receiver's responsibility.
- Asymmetric signatures. `v1a` is out of scope for v1.

## 2. Secret format

- 32 random bytes from the operating system CSPRNG.
- Serialised as `whsec_` followed by standard base64 (with padding) of the bytes.
- Example: `whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=`
- The HMAC key is the **decoded 32 bytes**, not the ASCII string and not the prefix.
- Each endpoint has its own secret. Secrets are never shared across endpoints or tenants.
- The plaintext secret is returned exactly once, in the create or rotate response. It is never returned by
  a GET, never logged, and never included in an audit metadata field.
- At rest: AES-256-GCM ciphertext with AAD binding tenant, endpoint, secret ID, and KEK version. See
  [ADR-0006](../docs/adr/0006-secret-storage.md).

## 3. Canonical string

```text
signed_payload = webhook_id + "." + webhook_timestamp + "." + raw_body
```

| Component | Source | Constraint |
| --- | --- | --- |
| `webhook_id` | `webhook-id` header, equal to `events.id` | UUID string; no `.` |
| `webhook_timestamp` | `webhook-timestamp` header, Unix seconds | Base-10 integer; no `.`; fresh per attempt |
| `raw_body` | Exact request body bytes | Must be the bytes sent; never re-serialised JSON |

The dots are literal delimiters. No trailing newline is added. The body is hashed as raw bytes, so
whitespace, key order, and character encoding matter exactly as transmitted.

`webhook-id` and `webhook-timestamp` are server-generated and contain no user-controlled dots. The event
ID is a server-generated UUID; the timestamp is an integer. A receiver must reject a header containing a
`.` in either field before constructing the string.

## 4. Signature computation

```text
signature_bytes = HMAC-SHA256(key = decoded_secret, message = UTF8(signed_payload))
signature_b64   = base64_standard(signature_bytes)        # with padding
header_value    = "v1," + signature_b64
```

- Algorithm: HMAC per RFC 2104 with SHA-256 per FIPS 180-4.
- Base64: RFC 4648 standard alphabet, with `=` padding.
- Lowercase `v1` scheme identifier, comma, no spaces inside one signature.
- Multiple active secrets: compute one `v1,<sig>` per secret, join with a single ASCII space, newest
  secret first. Order is not semantically meaningful; receivers must accept any matching entry.

## 5. Request headers

| Header | Required | Example | Notes |
| --- | --- | --- | --- |
| `Content-Type` | Yes | `application/json` | v1 only |
| `webhook-id` | Yes | `0199a1c4-...` | Stable event ID across retries/replays |
| `webhook-timestamp` | Yes | `1700000000` | Unix seconds, regenerated per attempt |
| `webhook-signature` | Yes | `v1,zWH/4q...` | Space-separated list during rotation |
| `RelayForge-Delivery` | Yes | `0199a1c5-...` | Stable per (event, endpoint) across cycles |
| `RelayForge-Attempt` | Yes | `1` | `attempt_count` for this cycle |
| `RelayForge-Event-Type` | Yes | `order.created` | Same as envelope `type` |
| `User-Agent` | Yes | `RelayForge/1` | No version disclosure beyond major |
| `Content-Length` | Yes | byte length | Client sets it; no chunked signing ambiguity |

No `Authorization` header is sent. No cookies. No redirects are followed by the sender.

## 6. Receiver verification algorithm (normative)

1. Read the **raw** request body before any JSON parser, framework middleware, or re-encoding. If the
   framework has already parsed and re-serialised it, verification must fail rather than “repair” the
   body.
2. Enforce a maximum body size (receiver-side; recommended 256 KiB, Absolute 1 MiB) and `POST` only.
3. Read `webhook-id`, `webhook-timestamp`, and `webhook-signature`. Missing → `401`, no detail.
4. Reject if `webhook-id` or `webhook-timestamp` contains a `.`; reject if the timestamp is not a
   base-10 integer.
5. Reject if `abs(now_unix - timestamp) > 300` seconds. Use an NTP-synchronised clock. Five minutes is
   the documented tolerance.
6. Parse the signature header into entries. Accept only entries whose scheme is exactly `v1`; ignore
   `v0`, `v1a`, or unknown schemes to prevent downgrade confusion.
7. For each active receiver secret, recompute HMAC-SHA256 over
   `webhook-id + "." + webhook-timestamp + "." + raw_body`, base64-encode, and compare with a
   constant-time function (`hmac.Equal`, `crypto.timingSafeEqual`, `hmac.compare_digest`, or equivalent).
   Do not use `==`.
8. Accept if any `v1` entry matches any active secret. If none match → `401`, generic body.
9. **After** signature and freshness validation, consult a durable idempotency table keyed by
   `webhook-id`. In one transaction:
   - `INSERT ... ON CONFLICT DO NOTHING` the event ID;
   - if inserted, apply the business effect;
   - if not inserted, do nothing.
   Commit, then return `2xx`. For an authentic duplicate, return `2xx` without repeating the effect.
10. Return `5xx` or `429` only if the effect was not durably applied and a retry is desired.

The 300-second window bounds replay of a captured request but does **not** prevent replay within the
window. Step 9 is mandatory; the timestamp check is not a substitute for durable deduplication.

## 7. Rotation procedure

Planned rotation, with overlap:

1. `POST /api/v1/endpoints/{id}/secrets` with `overlap_seconds` (default 86400, max 604800).
2. The API creates a new active secret and returns the plaintext once. The old secret remains active until
   its `expires_at`.
3. The worker signs every delivery with **all** active secrets (maximum 2).
4. The receiver loads the new secret; it already accepts any matching `v1` entry, so either side can switch
   first inside the window.
5. After successful verification with the new secret, the tenant revokes the old one:
   `DELETE /api/v1/endpoints/{id}/secrets/{secret_id}`. Revocation retires it immediately; no new
   signature is produced with it.
6. If compromise is suspected, revoke first and rotate immediately. Overlap is a availability feature, not
   a security feature. A compromised secret can forge deliveries during overlap; this is documented.

Delivery behaviour during rotation:

- Active secrets are fetched at attempt time from `endpoint_secrets WHERE status='active' AND
  (expires_at IS NULL OR expires_at > now())`.
- No in-process long-lived cache of plaintext secrets in v1. The extra small query is acceptable and avoids
  a stale-secret window. A short TTL cache is a measured optimization only if benchmarks justify it.
- If zero active secrets exist, the attempt fails permanently with `no_active_secret`; the delivery is
  replayable after a secret is created.

## 8. Test vector (normative)

Secret (base64 of bytes `0x00..0x1f`):

```text
whsec_AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=
```

Event ID:

```text
msg_01J9Z6Q3V8T7R2M4N5P6Q7R8S9
```

Timestamp:

```text
1700000000
```

Raw body (exact bytes, no trailing newline):

```json
{"type":"order.created","data":{"id":"ord_123","amount":4200}}
```

Canonical string:

```text
msg_01J9Z6Q3V8T7R2M4N5P6Q7R8S9.1700000000.{"type":"order.created","data":{"id":"ord_123","amount":4200}}
```

Expected header:

```text
webhook-signature: v1,zWH/4qANrUYMGmZOIkwS0phhFBRxMelsoEkhyx2G71Q=
```

This vector was recomputed independently in Phase 0 with PowerShell 7 /
.NET `HMACSHA256` on 2026-10-22; the digest matched
`zWH/4qANrUYMGmZOIkwS0phhFBRxMelsoEkhyx2G71Q=`. It must be asserted in the Rust
unit tests and the Go demo receiver tests. Changing one byte of the body must
produce a different signature.

## 9. Failure handling and observability

- Missing/invalid signature at the receiver: `401`, generic body, log the event ID and source IP, never the
  signature value.
- Stale timestamp: `401`, log `signature_stale` with the skew, not the body.
- Duplicate event: `2xx`, log `duplicate_ignored` with the event ID.
- Sender failures: `no_active_secret` is permanent; `internal_error` is retryable and alerts.
- Metrics: `relayforge_signature_failures_total` on the receiver is a receiver concern; the sender exposes
  `relayforge_delivery_attempts_total{outcome,error_code}`.
- Never log the secret, the signature header, or the raw body. Log `payload_sha256` and byte length.

## 10. Residual risks

| Risk | Residual exposure | Mitigation |
| --- | --- | --- |
| Receiver replays a captured request within 300 s | Duplicate processing if receiver lacks durable dedupe | Mandatory receiver dedupe; demo demonstrates it |
| Secret leak from a tenant's own systems | Forged deliveries until revocation | Rotation, per-endpoint secrets, audit, revocation |
| Clock skew > 300 s | Valid deliveries rejected | NTP requirement; tolerance documented; receiver can widen with risk |
| Shared KEK compromise | All signing secrets decryptable | KEK isolation, rotation procedure, KMS future work |
| Base64/parser disagreement | Verification bypass in a buggy receiver | Normative algorithm, independent test vector, demo implementation |
| JSON re-serialisation by receiver framework | False rejection, not forgery | Raw-body requirement documented and tested |
