# RelayForge public API v1

Status: Implemented and verified (Phases 1–7 complete). Last updated: 2026-10-23.
Normative for implementation. The OpenAPI 3.1 document is embedded and served at
`/api/v1/openapi.yaml`; `TestOpenAPIMatchesRouter` and Redocly lint fail if its operations and the router diverge.

Base URL: `https://api.relayforge.example/api/v1` (deployment-specific).
Content type: `application/json; charset=utf-8`; errors use `application/problem+json`.
Timestamps: RFC 3339 with UTC offset, e.g. `2026-10-22T10:15:30.123Z`.

## 1. Authentication and authorisation

Every tenant endpoint requires:

```http
Authorization: Bearer rf_live_<key_id>_<secret>
```

- Keys are shown once at creation/rotation. The server stores only an HMAC-SHA256 digest and a public
  `key_id` used for lookup.
- Keys in query strings, cookies, or custom headers are not accepted.
- Missing or invalid key: `401` with `WWW-Authenticate: Bearer realm="relayforge"` and a generic body.
- Valid key without permission: `403 forbidden`.
- Suspended tenant: `403 tenant_suspended`; the key is not disclosed as valid or invalid beyond that.
- No CORS headers. This is a server-to-server API. Browser use is unsupported in v1.

### 1.1 Roles

| Role | Events write | Endpoints / secrets | Replay | Credentials | Audit read | Tenant settings |
| --- | --- | --- | --- | --- | --- | --- |
| `owner` | Yes | Yes | Yes | Yes | Yes | Yes |
| `admin` | Yes | Yes | Yes | No | Yes | No |
| `ingest` | Yes | No | No | No | No | No |
| `viewer` | No | Read only | No | No | Yes | No |

An owner cannot revoke the last active `owner` credential. A tenant always has at least one active owner.

## 2. Resource model

```text
Tenant
├── ApiCredential (hashed secret)
├── EventSchema (optional, per type + version)
├── Endpoint (HTTPS destination)
│   ├── EndpointSecret (encrypted, max 2 active)
│   └── Delivery (one per event × endpoint)
│       └── DeliveryAttempt (append-only, per cycle)
├── Event (immutable wire body + parsed payload)
│   └── Delivery (...)
├── IdempotencyRecord (tenant + route + key, 24 h)
└── AuditLog (append-only)
```

## 3. Endpoint summary

### 3.1 Public / tenant API

| Method | Path | Role | Purpose |
| --- | --- | --- | --- |
| `POST` | `/tenants` | public (signup policy) | Register tenant + first owner key |
| `GET` | `/tenant` | viewer+ | Current tenant |
| `PATCH` | `/tenant` | owner | Update tenant name |
| `GET` | `/credentials` | owner | List credential metadata |
| `POST` | `/credentials` | owner | Create credential |
| `POST` | `/credentials/{id}/rotate` | owner | Rotate with overlap |
| `DELETE` | `/credentials/{id}` | owner | Revoke immediately |
| `GET` | `/endpoints` | viewer+ | List endpoints |
| `POST` | `/endpoints` | admin+ | Create endpoint + first signing secret |
| `GET` | `/endpoints/{id}` | viewer+ | Endpoint detail (no secret) |
| `PATCH` | `/endpoints/{id}` | admin+ | Update URL, filters, policy, pause/resume |
| `DELETE` | `/endpoints/{id}` | admin+ | Soft delete |
| `GET` | `/endpoints/{id}/secrets` | admin+ | Secret metadata only |
| `POST` | `/endpoints/{id}/secrets` | admin+ | Create secret (returned once) |
| `DELETE` | `/endpoints/{id}/secrets/{secret_id}` | admin+ | Retire a secret |
| `POST` | `/events` | ingest+ | Ingest event (idempotency required) |
| `GET` | `/events` | viewer+ | List events |
| `GET` | `/events/{id}` | viewer+ | Event + delivery summary |
| `POST` | `/events/{id}/replay` | admin+ | Phase 4: replay failed/dead-lettered deliveries |
| `GET` | `/deliveries` | viewer+ | List deliveries |
| `GET` | `/deliveries/{id}` | viewer+ | Delivery detail |
| `GET` | `/deliveries/{id}/attempts` | viewer+ | Attempt history |
| `GET` | `/schemas` | viewer+ | List registered event schemas |
| `POST` | `/schemas` | owner | Register a JSON Schema |
| `DELETE` | `/schemas/{id}` | owner | Delete a JSON Schema |
| `GET` | `/audit-logs` | viewer+ | Audit query |
| `GET` | `/openapi.yaml` | public | OpenAPI 3.1 document |
| `GET` | `/healthz`, `/readyz` | public | Liveness / readiness |

### 3.2 Internal admin API

Served on `RELAYFORGE_ADMIN_ADDR`, not the public listener. Requires `X-Admin-Token` (constant-time compare).
Network access is restricted by deployment policy; the token is not the only control.

| Method | Path | Purpose |
| --- | --- | --- |
| `POST` | `/internal/v1/admin/tenants/{id}/suspend` | Suspend tenant, reason required (Phase 2) |
| `POST` | `/internal/v1/admin/tenants/{id}/resume` | Resume tenant (Phase 2) |
| `GET` | `/internal/v1/admin/tenants` | Phase 4: list tenants with status and counts |
| `GET` | `/internal/v1/admin/queue` | Phase 4: queue depth by status, oldest due age |
| `GET` | `/internal/v1/admin/deliveries/stuck` | Phase 4: in-progress with expired lease |
| `POST` | `/internal/v1/admin/deliveries/{id}/replay` | Phase 4: console replay, audited |

## 4. Consistent errors

Errors follow RFC 9457 with a stable machine-readable `code` extension:

```http
HTTP/1.1 409 Conflict
Content-Type: application/problem+json
```

```json
{
  "type": "https://relayforge.example/errors/idempotency-key-reused",
  "title": "Idempotency key reused with a different payload",
  "status": 409,
  "code": "idempotency_key_reused",
  "detail": "The key was first used with a different request body.",
  "instance": "/api/v1/events",
  "request_id": "req_01J9Z7...",
  "errors": []
}
```

Rules:

- `detail` never contains SQL, stack traces, secret material, or raw payloads.
- Validation errors add `errors: [{"pointer": "/data/amount", "code": "type", "detail": "..."}]`.
- Every response includes `X-Request-Id`; a client-supplied `X-Request-Id` is accepted only if it matches
  `^[A-Za-z0-9_.:-]{8,128}$`.
- `Cache-Control: no-store` on authenticated responses.
- `X-Content-Type-Options: nosniff`.

| HTTP | `code` | Meaning |
| --- | --- | --- |
| 400 | `invalid_request` | Malformed JSON, bad header, invalid cursor |
| 400 | `idempotency_key_required` | Missing `Idempotency-Key` on event ingest |
| 401 | `unauthorized` | Missing/invalid credential |
| 403 | `forbidden` | Role lacks permission |
| 403 | `tenant_suspended` | Tenant is suspended |
| 403 | `signup_disabled` | Tenant registration disabled |
| 404 | `not_found` | Resource absent **or owned by another tenant** |
| 409 | `idempotency_key_reused` | Same key, different body |
| 409 | `idempotency_in_progress` | Concurrent request still committing; retry |
| 409 | `replay_not_allowed` | No delivery in a replayable state |
| 413 | `payload_too_large` | Body exceeds 256 KiB |
| 415 | `unsupported_media_type` | Not `application/json` |
| 422 | `validation_failed` | Envelope/schema validation failed |
| 422 | `destination_rejected` | URL/SSRF policy rejected at registration |
| 429 | `rate_limited` | Tenant/IP/destination limit |
| 500 | `internal_error` | Unexpected failure; request ID is the correlation handle |
| 503 | `service_unavailable` | DB/readiness failure; safe to retry with backoff |

## 5. Tenant registration

`POST /tenants` (no auth). Controlled by `RELAYFORGE_SIGNUP_MODE`:

- `open`: allowed, rate-limited per IP (5/min fixed window).
- `token`: requires `X-Signup-Token` equal to `RELAYFORGE_SIGNUP_TOKEN`.
- `disabled`: `403 signup_disabled`.

```json
{
  "name": "Acme Integration",
  "slug": "acme-integration"
}
```

`slug` is optional; if omitted it is derived and suffixed to avoid collisions. It is immutable.

Response `201`:

```json
{
  "tenant": {
    "id": "0199a1c4-6f2a-7d31-9c0e-2b7f4a1d8e55",
    "slug": "acme-integration",
    "name": "Acme Integration",
    "status": "active",
    "created_at": "2026-10-22T10:00:00Z"
  },
  "api_key": "rf_live_<key_id>_<secret>",
  "api_key_id": "0199a1c4-...",
  "warning": "Store this key now. It cannot be retrieved again."
}
```

The tenant, owner credential, and audit row commit atomically. The plaintext key is never logged.

## 6. Credentials

### Create

`POST /credentials` (owner):

```json
{"name": "CI ingest", "role": "ingest", "expires_in_days": 90}
```

`201` returns `id`, `key_id`, `name`, `role`, `created_at`, `expires_at`, and `api_key` once.

### Rotate

`POST /credentials/{id}/rotate` (owner):

```json
{"overlap_seconds": 3600}
```

`overlap_seconds` is 0–86400. The old key remains valid until `now + overlap_seconds`, then is revoked.
Response returns the new `api_key` once and the old key's `revoked_at`/expiry. Rotating a revoked key is
`409`.

### Revoke

`DELETE /credentials/{id}` → `204`. Immediate; idempotent (revoking an already revoked key is `204`).
Refusing to revoke the last owner key is `409 last_owner_credential`.

### List

`GET /credentials` returns metadata only: `id`, `key_id`, `name`, `role`, `created_at`, `last_used_at`,
`expires_at`, `revoked_at`, `rotated_from`. No secret, no digest.

## 7. Endpoints and signing secrets

### Create

`POST /endpoints` (admin+):

```json
{
  "name": "Acme orders receiver",
  "url": "https://example.com/relayforge/orders",
  "event_types": ["order.created", "order.updated"],
  "max_attempts": 8,
  "initial_backoff_ms": 5000,
  "max_backoff_ms": 3600000,
  "retry_window_seconds": 86400,
  "timeout_ms": 15000,
  "rate_limit_per_sec": 10
}
```

Validation:

- `url`: full SSRF policy at registration. Non-HTTPS, userinfo, non-allowlisted port, private/reserved
  destinations, single-label hosts, and ambiguous authorities → `422 destination_rejected` with a
  machine-readable `reason` (e.g. `ip_denied`, `scheme_not_https`).
- `event_types`: empty/omitted means all events; max 20, each matching the event-type regex.
- Policy values are bounded as in the data model. A policy change affects **future** events only; queued
  deliveries keep their snapshot.
- Maximum endpoints per tenant: 50. Exceeding → `422 endpoint_limit_reached`.

Response `201` includes endpoint fields plus:

```json
{
  "signing_secret": "whsec_<base64-encoded-32-byte-secret>",
  "signing_secret_id": "0199a1c5-..."
}
```

The secret is returned once. `GET /endpoints/{id}` never includes it.

### Update / pause

`PATCH /endpoints/{id}` (admin+):

```json
{"status": "paused"}
```

- `active` and `paused` are tenant-settable. `disabled` is system-only (auto-pause).
- Pausing stops future fan-out; it does not cancel deliveries already committed. In-flight attempts may
  still complete. This is documented and tested.
- Changing `url` re-runs registration SSRF validation and affects future attempts; queued deliveries to
  the endpoint use the new URL. There is no URL history in v1; audit records the old and new URL (without
  query secrets).
- `DELETE /endpoints/{id}` is a soft delete: `deleted_at` set, status `paused`, no future fan-out.
  Historical deliveries and attempts remain. Deleting does not revoke secrets immediately; a separate
  secret cleanup runs at retention. Documented limitation.

### Secrets

`POST /endpoints/{id}/secrets`:

```json
{"overlap_seconds": 86400}
```

- Creates a new 32-byte secret, stores ciphertext, returns plaintext once.
- Maximum two active secrets; a third returns `409 secret_overlap_limit`.
- Old secret stays active until revoked or `expires_at`.
- Worker signs with every active secret during overlap.

`DELETE /endpoints/{id}/secrets/{secret_id}` → `204`; retirement is immediate. Retiring the last active
secret is `409 last_active_secret`; create a replacement first. This prevents an accidental permanently
undeliverable endpoint.

`GET /endpoints/{id}/secrets` returns metadata: `id`, `secret_hint` (last 4), `status`, `created_at`,
`expires_at`, `retired_at`.

## 8. Event ingestion

### Request

`POST /events` (ingest+), required headers:

```http
Authorization: Bearer rf_live_...
Content-Type: application/json
Idempotency-Key: order-123-created-v1
```

```json
{
  "type": "order.created",
  "schema_version": 1,
  "occurred_at": "2026-10-22T10:14:00Z",
  "data": {
    "order_id": "ord_123",
    "amount": 4200,
    "currency": "EUR"
  }
}
```

Rules:

- Body ≤ 256 KiB. Enforced by `http.MaxBytesReader` and by a second length check after decoding.
- Exactly one JSON object; trailing data is rejected. Unknown **envelope** fields are rejected. `data` may
  contain arbitrary JSON but must be an object.
- `type`: 3–100 chars, `^[a-z][a-z0-9_]*(\.[a-z0-9_]+){1,3}$`.
- `occurred_at`: optional, defaults to now. Must not be more than 5 minutes in the future or more than 30
  days in the past (configurable). This prevents accidental backfills from becoming an unbounded load
  source; a dedicated backfill mode is future work.
- If a schema is registered for `(type, schema_version)`, `data` is validated against it. Failure → `422
  schema_validation_failed` with JSON pointers.
- `Idempotency-Key` is mandatory. 8–255 chars, `^[A-Za-z0-9_.:-]+$`.
- The exact bytes received are stored as `wire_body` and signed on every attempt. Whitespace and key order
  are preserved; there is no re-serialisation.

### Response

`201 Created` (`200` if replaying a stored response? No: the original status is replayed, which is `201`):

```json
{
  "id": "0199a1c6-3a4b-7c2d-8e1f-5a6b7c8d9e0f",
  "type": "order.created",
  "schema_version": 1,
  "status": "accepted",
  "occurred_at": "2026-10-22T10:14:00Z",
  "received_at": "2026-10-22T10:14:00.412Z",
  "delivery_count": 2,
  "created_at": "2026-10-22T10:14:00.412Z"
}
```

Headers on a replay: `Idempotency-Replayed: true`. The response is read back from PostgreSQL JSONB before
the transaction commits, so the first response and every replay have identical bytes. JSONB normalises
whitespace and object key order; clients must treat the fields, not the original request whitespace, as the
contract.

### Idempotency semantics

| Situation | Result |
| --- | --- |
| New key | Process; commit event + deliveries + response record atomically |
| Same key, same raw body | Return stored status/body; no new event |
| Same key, different raw body | `409 idempotency_key_reused` |
| Same key, concurrent, first still committing | Block on a transaction-scoped advisory lock up to 3 s, then `409 idempotency_in_progress` + `Retry-After: 1` |
| Key older than 24 h and cleaned up | Treated as new; documented TTL |
| Missing key | `400 idempotency_key_required` |

Fingerprint = SHA-256 of the **raw request bytes**. Semantically equal JSON with different whitespace is a
different fingerprint. This is deliberate; canonical JSON is a large source of subtle bugs.

Scope = `(tenant_id, 'events.create', key)`. Two tenants may use the same key without collision.

### Optional JSON Schema registration

Schemas are tenant-scoped and keyed by `(event_type, schema_version)`.

`POST /schemas` (owner):

```json
{
  "event_type": "order.created",
  "version": 1,
  "schema": {
    "$schema": "https://json-schema.org/draft/2020-12/schema",
    "type": "object",
    "required": ["order_id"],
    "properties": {"order_id": {"type": "string"}}
  }
}
```

- The schema must compile as JSON Schema draft 2020-12, be at most 64 KiB, and must not use external
  `http(s)` or `file` references.
- Duplicate `(event_type, version)` → `409 schema_exists`.
- When present, `data` (not the whole envelope) is validated. Failures return `422
  schema_validation_failed` with pointers prefixed `/data` (at most 20 errors).
- `GET /schemas` (viewer+) lists schemas. `DELETE /schemas/{id}` (owner) removes a schema; existing events
  are unaffected and becoming unvalidated is intentional and audited.
- Ingesting without a registered schema performs envelope validation only.

## 9. Event queries

`GET /events?limit=25&cursor=...&status=failed&type=order.created&from=...&to=...&order=desc`

- `limit` 1–100 (default 25).
- `status`: comma-separated subset of event statuses.
- `type`: exact event type.
- `from`/`to`: RFC 3339; range ≤ 90 days.
- `order`: `asc|desc` (default `desc`); sort is always `(created_at, id)`.
- `cursor`: opaque; bound to the filter set. Invalid or filter-mismatched cursor → `400 invalid_cursor`.

Response:

```json
{
  "data": [
    {
      "id": "0199a1c6-...",
      "type": "order.created",
      "status": "delivered",
      "occurred_at": "2026-10-22T10:14:00Z",
      "received_at": "2026-10-22T10:14:00.412Z",
      "delivery_count": 2,
      "delivered_count": 2,
      "failed_count": 0,
      "created_at": "2026-10-22T10:14:00.412Z"
    }
  ],
  "next_cursor": "eyJ0IjoiMjAyNi0xMC0yMlQxMDoxNDowMC40MTJaIiwiaWQiOiIwMTk5...",
  "has_more": false
}
```

`GET /events/{id}` adds `payload` (parsed), `payload_sha256`, and a delivery summary per endpoint.
`wire_body` is not returned by default; a debug endpoint is out of scope (payload may contain PII).

## 10. Delivery queries

`GET /deliveries?status=dead_lettered&endpoint_id=...&event_id=...&from=...&to=...`

Response item:

```json
{
  "id": "0199a1c7-...",
  "event_id": "0199a1c6-...",
  "endpoint_id": "0199a1c5-...",
  "cycle": 1,
  "status": "retry_scheduled",
  "attempt_count": 3,
  "max_attempts": 8,
  "next_attempt_at": "2026-10-22T10:20:00Z",
  "last_error_code": "http_503",
  "last_error_detail": "upstream unavailable",
  "last_http_status": 503,
  "created_at": "2026-10-22T10:14:00.412Z",
  "completed_at": null,
  "dead_lettered_at": null
}
```

`GET /deliveries/{id}/attempts?limit=50&cursor=...`:

```json
{
  "data": [
    {
      "attempt_no": 1,
      "cycle": 1,
      "started_at": "2026-10-22T10:14:01Z",
      "finished_at": "2026-10-22T10:14:01.238Z",
      "outcome": "retryable_failure",
      "http_status": 503,
      "error_code": "http_503",
      "error_detail": "upstream unavailable",
      "duration_ms": 238
    }
  ],
  "next_cursor": null,
  "has_more": false
}
```

Attempt history is append-only. `error_detail` is bounded and redacted; response bodies are never stored.

## 11. Replay

`POST /events/{event_id}/replay` (admin+):

```json
{
  "endpoint_ids": ["0199a1c5-..."],
  "reason": "Receiver fixed its 500 after deploy 2026-10-22"
}
```

- `endpoint_ids` optional; omitted means all failed/dead-lettered deliveries for the event.
- Only `failed` and `dead_lettered` are eligible. `delivered`, `pending`, `retry_scheduled`, and
  `in_progress` are skipped (`in_progress` may be retried after it terminates).
- `reason` required, 1–500 chars, stored in the audit log.
- Replay increments `cycle`, resets attempts, preserves all previous attempt rows, and re-runs SSRF and
  destination rate limiting.

`202 Accepted`:

```json
{
  "event_id": "0199a1c6-...",
  "replayed": 1,
  "skipped": 1,
  "results": [
    {"delivery_id": "0199a1c7-...", "endpoint_id": "...", "previous_cycle": 1, "cycle": 2, "status": "replayed"},
    {"delivery_id": "0199a1c8-...", "endpoint_id": "...", "previous_cycle": 1, "status": "skipped", "reason": "already_delivered"}
  ]
}
```

If nothing is replayable → `409 replay_not_allowed` with per-delivery reasons. Replay of another tenant's
event → `404` (not `403`).

## 12. Audit logs

`GET /audit-logs?action=endpoint.secret.rotate&resource_type=endpoint&resource_id=...&from=...&to=...`

Returns append-only records: `id`, `actor_type`, `actor_id`, `action`, `resource_type`, `resource_id`,
`request_id`, `ip`, `created_at`, `metadata`. Metadata is an explicit allowlist per action and never
contains secrets, digests, signatures, or payloads.

Actions include: `tenant.create`, `tenant.update`, `tenant.suspend`, `tenant.resume`,
`credential.create`, `credential.rotate`, `credential.revoke`, `endpoint.create`, `endpoint.update`,
`endpoint.pause`, `endpoint.resume`, `endpoint.delete`, `endpoint.secret.create`,
`endpoint.secret.retire`, `schema.create`, `schema.delete`, `event.replay`, `admin.replay`.
Records are inserted only by the application; there is no update or delete path.

## 13. Pagination, filtering, sorting

- Cursor pagination only; `OFFSET` is not supported.
- Cursor encoding: base64url of `{"t":"<created_at>","id":"<uuid>","f":"<filter tag>"}`. The server validates
  types and the filter hash; it is not encrypted and contains no secrets. Attempt and audit cursors use the
  numeric row id for the tie-breaker.
- Default 25, maximum 100. A larger `limit` → `400 invalid_request`.
- Sort fields are allowlisted (`created_at`). Arbitrary sort columns are rejected.
- Time filters are inclusive `from`, exclusive `to`; max range 90 days.
- Unknown query parameters are rejected (strict validation) to catch typos.

## 14. Rate limits

Phase 4 implements shared PostgreSQL fixed-window limits for tenant ingest, tenant management, admin,
and per-destination delivery. The signup limiter remains in process memory: 5 requests per minute per
client IP, per API replica. The `Idempotency-Key` in-progress path also returns `Retry-After: 1`.

| Scope | Default | Window | Response |
| --- | --- | --- | --- |
| Tenant ingest | 50 req/s | 1 s fixed | `429 rate_limited` |
| Tenant management | 10 req/s | 1 s fixed | `429 rate_limited` |
| Destination delivery | 10 req/s per endpoint | 1 s fixed | Deferred, not a 429 to the tenant |
| Admin | 20 req/s | 1 s fixed | `429 rate_limited` |

The ingest, management, and admin limits are configurable with
`RELAYFORGE_RATE_LIMIT_INGEST_PER_SEC`, `RELAYFORGE_RATE_LIMIT_MANAGEMENT_PER_SEC`, and
`RELAYFORGE_RATE_LIMIT_ADMIN_PER_SEC`. Fixed windows can allow up to 2× the limit across a boundary.
This is a documented trade-off ([ADR-0011](adr/0011-postgres-rate-limiter.md)). Destination limiting is
enforced by the worker before the HTTP request; a deferred delivery does not consume an attempt and is
retried at the next window.

## 15. Health and metrics

| Endpoint | Listener | Auth | Semantics |
| --- | --- | --- | --- |
| `/healthz` | public + admin | none | Process alive; no DB dependency |
| `/readyz` | public + admin | none | API: DB reachable and schema current. Worker: DB, schema, and loop |
| `/metrics` | admin only | optional `X-Admin-Token`; required in production | Prometheus text format |
| `/internal/v1/admin/*` | admin only | `X-Admin-Token` | Tenant list/suspend/resume, queue summary, stuck deliveries, delivery replay |

Readiness body (API):

```json
{"status":"ready","checks":{"database":"ok","schema":"ok"},"version":"0.2.0"}
```

The worker readiness body additionally reports `worker_loop`. On failure the response is `503` with
`{"status":"not_ready","checks":{"database":"failed"},"reason":"db_ping_timeout"}` and no internal
details beyond the check name.

## 16. curl examples

Set variables (never commit real keys):

```bash
export RF_API=https://api.relayforge.example/api/v1
export RF_KEY=rf_live_xxx_yyy
```

Create a tenant (dev signup open):

```bash
curl -sS -X POST "$RF_API/tenants" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Acme Integration","slug":"acme"}'
```

Create an endpoint:

```bash
curl -sS -X POST "$RF_API/endpoints" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "Orders receiver",
    "url": "https://example.com/relayforge/orders",
    "event_types": ["order.created"],
    "max_attempts": 8
  }'
```

Ingest an event:

```bash
curl -sS -X POST "$RF_API/events" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -H 'Idempotency-Key: order-123-created-v1' \
  -d '{
    "type": "order.created",
    "occurred_at": "2026-10-22T10:14:00Z",
    "data": {"order_id":"ord_123","amount":4200,"currency":"EUR"}
  }'
```

Retry the exact same command: expect `201` plus `Idempotency-Replayed: true`, with the same event ID.

List dead-lettered deliveries:

```bash
curl -sS "$RF_API/deliveries?status=dead_lettered&limit=25" \
  -H "Authorization: Bearer $RF_KEY"
```

Replay an event to one endpoint:

```bash
curl -sS -X POST "$RF_API/events/0199a1c6-3a4b-7c2d-8e1f-5a6b7c8d9e0f/replay" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "endpoint_ids": ["0199a1c5-6b7c-7d8e-9f00-1a2b3c4d5e6f"],
    "reason": "receiver fixed its 500 after deploy"
  }'
```

## 17. Signature headers (summary)

Full specification: [contracts/signature-v1.md](../contracts/signature-v1.md).

```http
webhook-id: 0199a1c6-3a4b-7c2d-8e1f-5a6b7c8d9e0f
webhook-timestamp: 1761128040
webhook-signature: v1,zWH/4qANrUYMGmZOIkwS0phhFBRxMelsoEkhyx2G71Q=
RelayForge-Delivery: 0199a1c7-...
RelayForge-Attempt: 1
RelayForge-Event-Type: order.created
```

Canonical string: `webhook-id + "." + webhook-timestamp + "." + raw_body`. HMAC key is the decoded
`whsec_` secret. Receiver tolerance is 300 s; durable deduplication on `webhook-id` is mandatory because a
captured delivery can be replayed within the window.

## 18. API versioning policy

- URL path version (`/api/v1`) is the compatibility boundary.
- Additive fields may appear in responses at any time; clients must ignore unknown fields.
- Removing fields, changing meanings, or changing status codes requires `/api/v2`.
- The OpenAPI document is the executable contract; CI fails if handlers and spec diverge.
- Deprecation: announce in the OpenAPI document and return `Deprecation`/`Sunset` headers for at least one
  release cycle. No deprecations exist in v1.

## 19. Deliberate omissions

- No batch ingest, no WebSocket/SSE status stream, no custom headers on delivery, no per-endpoint HTTP
  method selection (always `POST`), no payload transformation, no ordering guarantee, no delivery to
  private networks, no tenant self-service deletion, no human users.
