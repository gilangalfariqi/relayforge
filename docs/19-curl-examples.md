# RelayForge curl collection

These examples follow the OpenAPI contract and the exercised flows in
[docs/14-phase-3-report.md](14-phase-3-report.md) and [docs/18-phase-7-report.md](18-phase-7-report.md).
The E2E suite executes the same operations programmatically; this page has not been replayed end-to-end as
written, so treat individual responses as illustrative rather than recorded transcripts.

The examples use the local API at `http://127.0.0.1:8080`. The Compose stack runs with open signup and
deliberately low-entropy development secrets. **Do not use this configuration on an untrusted network.**

Set the base URL and a JSON pretty-printer once:

```bash
export RF_API=http://127.0.0.1:8080/api/v1
export RF_ADMIN=http://127.0.0.1:9090
alias rfjson='python -m json.tool'
```

## 1. Health and contract

```bash
curl -sS http://127.0.0.1:8080/healthz
curl -sS http://127.0.0.1:8080/readyz
curl -sS http://127.0.0.1:9091/readyz
curl -sS http://127.0.0.1:8080/api/v1/openapi.yaml | head -n 5
```

`/healthz` proves the process is alive. `/readyz` also checks PostgreSQL and schema version. The worker
readiness response includes `worker_loop`.

## 2. Create a tenant and capture the one-time API key

```bash
curl -sS -X POST "$RF_API/tenants" \
  -H 'Content-Type: application/json' \
  -d '{"name":"Curl Demo","slug":"curl-demo"}' | tee /tmp/rf-tenant.json

export RF_KEY="$(python -c 'import json; print(json.load(open("/tmp/rf-tenant.json"))["api_key"])')"
export RF_TENANT_ID="$(python -c 'import json; print(json.load(open("/tmp/rf-tenant.json"))["tenant"]["id"])')"
printf 'tenant=%s key_prefix=%s\n' "$RF_TENANT_ID" "${RF_KEY:0:20}"
```

The `api_key` value is returned once and is never recoverable from the API. Use a unique slug; a duplicate
slug returns `409`.

## 3. Create an endpoint and keep its signing secret

The URL is deliberately the Compose receiver. The worker's local-only mode rewrites
`https://receiver:8090/...` to plain HTTP inside the Docker network. In a real deployment, the endpoint must
be a public HTTPS URL on an allowed port.

```bash
curl -sS -X POST "$RF_API/endpoints" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "name": "orders receiver",
    "url": "https://receiver:8090/webhook",
    "event_types": ["order.created"],
    "max_attempts": 3,
    "initial_backoff_ms": 500,
    "max_backoff_ms": 2000,
    "retry_window_seconds": 300,
    "timeout_ms": 3000,
    "rate_limit_per_sec": 50
  }' | tee /tmp/rf-endpoint.json

export RF_ENDPOINT_ID="$(python -c 'import json; print(json.load(open("/tmp/rf-endpoint.json"))["id"])')"
export RF_SIGNING_SECRET="$(python -c 'import json; print(json.load(open("/tmp/rf-endpoint.json"))["signing_secret"])')"
```

Register the secret with the demo receiver so it can verify signatures:

```bash
curl -sS -X POST http://127.0.0.1:8090/control/secrets \
  -H 'Content-Type: application/json' \
  -d "{\"secrets\":[\"$RF_SIGNING_SECRET\"]}"
```

`GET /endpoints/{id}` and `GET /endpoints/{id}/secrets` never return a plaintext secret.

## 4. Ingest an event idempotently

```bash
export RF_IDEMPOTENCY_KEY="curl-order-$(date +%s)"
export RF_EVENT_BODY='{
  "type": "order.created",
  "occurred_at": "2026-10-24T00:00:00Z",
  "data": {"order_id": "ord_curl_1", "amount": 4200, "currency": "EUR"}
}'

curl -sS -D /tmp/rf-ingest.headers -X POST "$RF_API/events" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RF_IDEMPOTENCY_KEY" \
  -d "$RF_EVENT_BODY" | tee /tmp/rf-event.json

export RF_EVENT_ID="$(python -c 'import json; print(json.load(open("/tmp/rf-event.json"))["id"])')"
grep -i '^idempotency-replayed:' /tmp/rf-ingest.headers || true
```

Retry the same request with the same key and body. The second response has the same event ID and
`Idempotency-Replayed: true`. Reusing the key with a different body returns `409 idempotency_conflict`.

`Idempotency-Key` is mandatory. Different keys create different events even when the payloads match.

## 5. Read events, deliveries, attempts, and audit history

```bash
curl -sS "$RF_API/events?limit=25" \
  -H "Authorization: Bearer $RF_KEY" | rfjson

curl -sS "$RF_API/events/$RF_EVENT_ID" \
  -H "Authorization: Bearer $RF_KEY" | rfjson

curl -sS "$RF_API/deliveries?status=dead_lettered&limit=25" \
  -H "Authorization: Bearer $RF_KEY" | rfjson

curl -sS "$RF_API/audit-logs?limit=25" \
  -H "Authorization: Bearer $RF_KEY" | rfjson
```

List responses are keyset paginated. Pass the opaque `next_cursor` from one response as `cursor` on the
next request; do not construct cursors by hand. Unknown query parameters are rejected with `400`.

To inspect attempts, set `RF_DELIVERY_ID` from the event detail (`deliveries[0].id`):

```bash
export RF_DELIVERY_ID="$(curl -sS "$RF_API/events/$RF_EVENT_ID" \
  -H "Authorization: Bearer $RF_KEY" \
  | python -c 'import json,sys; print(json.load(sys.stdin)["deliveries"][0]["id"])')"

curl -sS "$RF_API/deliveries/$RF_DELIVERY_ID/attempts" \
  -H "Authorization: Bearer $RF_KEY" | rfjson
```

## 6. Force a failure, inspect it, and replay

The receiver control API is local-only:

```bash
# Fail every request with 503.
curl -sS -X POST http://127.0.0.1:8090/control/behavior \
  -H 'Content-Type: application/json' \
  -d '{"status":503}'

# Ingest a second event and keep its id; it enters retry_scheduled, then dead_lettered.
export RF_FAILED_IDEMPOTENCY_KEY="curl-failure-$(date +%s)"
export RF_FAILED_EVENT_ID="$(curl -sS -X POST "$RF_API/events" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -H "Idempotency-Key: $RF_FAILED_IDEMPOTENCY_KEY" \
  -d '{"type":"order.created","data":{"order_id":"ord_curl_failure"}}' \
  | python -c 'import json,sys; print(json.load(sys.stdin)["id"])')"

# Restore success before replay.
curl -sS -X POST http://127.0.0.1:8090/control/behavior \
  -H 'Content-Type: application/json' \
  -d '{"status":200}'
```

Wait until the delivery is terminal (`GET /events/$RF_FAILED_EVENT_ID` shows `failed`), then replay only
eligible `failed` or `dead_lettered` rows:

```bash
curl -sS -X POST "$RF_API/events/$RF_FAILED_EVENT_ID/replay" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"reason":"receiver recovered after 503 outage"}' | rfjson
```

Replay returns `202` with per-delivery results, or `409 replay_not_allowed` when no delivery is eligible.
Replay increments `cycle`, preserves all old attempts, and is audited as `event.replay`.

## 7. Credentials, rotation, and schemas

```bash
# List credentials (never contains a plaintext key).
curl -sS "$RF_API/credentials" \
  -H "Authorization: Bearer $RF_KEY" | rfjson

# Create a second owner credential, then rotate it with a 24-hour overlap.
curl -sS -X POST "$RF_API/credentials" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"name":"curl second owner","role":"owner","expires_in_days":30}' | tee /tmp/rf-credential.json

export RF_CREDENTIAL_ID="$(python -c 'import json; print(json.load(open("/tmp/rf-credential.json"))["id"])')"

curl -sS -X POST "$RF_API/credentials/$RF_CREDENTIAL_ID/rotate" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"overlap_seconds":86400}' | rfjson

# Create a JSON Schema for order.created v1.
curl -sS -X POST "$RF_API/schemas" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -d '{
    "event_type":"order.created",
    "version":1,
    "schema":{
      "$schema":"https://json-schema.org/draft/2020-12/schema",
      "type":"object",
      "required":["order_id"],
      "properties":{"order_id":{"type":"string"}},
      "additionalProperties":true
    }
  }' | rfjson
```

Creating an endpoint secret and retiring it are owner/admin operations:

```bash
curl -sS -X POST "$RF_API/endpoints/$RF_ENDPOINT_ID/secrets" \
  -H "Authorization: Bearer $RF_KEY" \
  -H 'Content-Type: application/json' \
  -d '{"overlap_seconds":86400}' | tee /tmp/rf-secret.json

export RF_SECRET_ID="$(python -c 'import json; print(json.load(open("/tmp/rf-secret.json"))["id"])')"

curl -sS -X DELETE "$RF_API/endpoints/$RF_ENDPOINT_ID/secrets/$RF_SECRET_ID" \
  -H "Authorization: Bearer $RF_KEY" | rfjson
```

The last active secret cannot be retired (`409 last_active_secret`).

## 8. Internal admin listener

The Compose stack leaves `RELAYFORGE_ADMIN_TOKEN` unset, so local requests work without the header. In
production the token is mandatory. Metrics are not exposed on the public API listener.

```bash
curl -sS "$RF_ADMIN/readyz" | rfjson
curl -sS "$RF_ADMIN/metrics" | head -n 20
curl -sS "$RF_ADMIN/internal/v1/admin/queue" | rfjson
curl -sS "$RF_ADMIN/internal/v1/admin/deliveries/stuck?limit=25" | rfjson
curl -sS "$RF_ADMIN/internal/v1/admin/tenants" | rfjson

# With a configured token:
# curl -sS -H "X-Admin-Token: $RF_ADMIN_TOKEN" "$RF_ADMIN/internal/v1/admin/queue"
```

Suspending a tenant immediately blocks authenticated public requests with `403 tenant_suspended`:

```bash
curl -sS -X POST "$RF_ADMIN/internal/v1/admin/tenants/$RF_TENANT_ID/suspend" \
  -H 'Content-Type: application/json' \
  -d '{"reason":"curl demo"}' | rfjson

curl -sS -X POST "$RF_ADMIN/internal/v1/admin/tenants/$RF_TENANT_ID/resume" | rfjson
```

## 9. Error shape

Errors are RFC 9457-style `application/problem+json`:

```json
{
  "type": "https://relayforge.dev/problems/validation_failed",
  "title": "validation_failed",
  "status": 422,
  "detail": "field 'type' is required",
  "code": "validation_failed",
  "request_id": "0199...",
  "instance": "/api/v1/events"
}
```

Always check the HTTP status before parsing a success body. The `request_id` is safe to share when
reporting a problem; it is also present in structured logs.
