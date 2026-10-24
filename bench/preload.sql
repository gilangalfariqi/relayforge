-- RelayForge Phase 6 Dataset Preload (1,000,000 events / 2,000,000 deliveries / 2,050,000 attempts)
-- Usage:
--   psql "$RELAYFORGE_DATABASE_URL" -v ON_ERROR_STOP=1 -f bench/preload.sql
--
-- Generates a deterministic, realistic 35-day dataset across 10 tenants and 20 endpoints
-- so index depth, buffer cache pressure, and planner statistics match docs/07-benchmark-plan.md §3.

\timing on

BEGIN;
SET LOCAL synchronous_commit = off;
SET LOCAL work_mem = '64MB';
SET LOCAL maintenance_work_mem = '256MB';

TRUNCATE TABLE
    rate_limit_counters,
    idempotency_records,
    event_schemas,
    delivery_attempts,
    deliveries,
    events,
    endpoint_secrets,
    endpoints,
    audit_logs,
    api_credentials,
    tenants
RESTART IDENTITY CASCADE;

-- 1. 10 Tenants (tenant 1 holds ~50% of traffic to simulate a large tenant; tenants 2..10 share ~50%)
INSERT INTO tenants (id, slug, name, status, created_at, updated_at)
SELECT
    ('019a0000-0000-7000-8000-' || lpad(to_hex(t), 12, '0'))::uuid,
    'bench-tenant-' || lpad(t::text, 2, '0'),
    'Benchmark Tenant ' || t,
    'active',
    now() - interval '40 days',
    now() - interval '40 days'
FROM generate_series(1, 10) AS t;

-- 2. API Credentials (2 per tenant = 20 credentials)
INSERT INTO api_credentials (id, tenant_id, name, key_id, secret_hmac, role, created_at)
SELECT
    ('019a0000-0002-7000-8000-' || lpad(to_hex((t - 1) * 2 + r), 12, '0'))::uuid,
    ('019a0000-0000-7000-8000-' || lpad(to_hex(t), 12, '0'))::uuid,
    CASE WHEN r = 1 THEN 'bench-owner-key' ELSE 'bench-ingest-key' END,
    'benchkey' || lpad(((t - 1) * 2 + r)::text, 4, '0'),
    decode(repeat(lpad(to_hex((t - 1) * 2 + r), 2, '0'), 32), 'hex'),
    CASE WHEN r = 1 THEN 'owner' ELSE 'ingest' END,
    now() - interval '40 days'
FROM generate_series(1, 10) AS t
CROSS JOIN generate_series(1, 2) AS r;

-- 3. Endpoints (2 active endpoints per tenant = 20 endpoints)
INSERT INTO endpoints (
    id, tenant_id, name, url, status, event_types,
    max_attempts, initial_backoff_ms, max_backoff_ms,
    retry_window_seconds, timeout_ms, rate_limit_per_sec,
    created_at, updated_at
)
SELECT
    ('019a0000-0001-7000-8000-' || lpad(to_hex((t - 1) * 2 + ep), 12, '0'))::uuid,
    ('019a0000-0000-7000-8000-' || lpad(to_hex(t), 12, '0'))::uuid,
    'bench-endpoint-' || t || '-' || ep,
    'https://receiver.bench.example.com/t/' || t || '/ep/' || ep,
    'active',
    CASE WHEN ep = 1 THEN '{}'::text[] ELSE ARRAY['order.created', 'payment.succeeded', 'invoice.paid'] END,
    8, 5000, 3600000, 86400, 15000, 100,
    now() - interval '40 days',
    now() - interval '40 days'
FROM generate_series(1, 10) AS t
CROSS JOIN generate_series(1, 2) AS ep;

-- 4. Endpoint Secrets (1 active + 1 retired secret per endpoint = 40 secrets)
INSERT INTO endpoint_secrets (
    id, tenant_id, endpoint_id, ciphertext, nonce, key_version,
    secret_hint, status, created_at, retired_at
)
SELECT
    ('019a0000-0003-7000-8000-' || lpad(to_hex((ep_idx - 1) * 2 + s), 12, '0'))::uuid,
    ('019a0000-0000-7000-8000-' || lpad(to_hex(((ep_idx - 1) / 2) + 1), 12, '0'))::uuid,
    ('019a0000-0001-7000-8000-' || lpad(to_hex(ep_idx), 12, '0'))::uuid,
    decode(repeat('ab', 32), 'hex'),
    decode(repeat('cd', 12), 'hex'),
    1,
    'b' || lpad(s::text, 3, '0'),
    CASE WHEN s = 1 THEN 'active' ELSE 'retired' END,
    now() - interval '40 days',
    CASE WHEN s = 2 THEN now() - interval '10 days' ELSE NULL END
FROM generate_series(1, 20) AS ep_idx
CROSS JOIN generate_series(1, 2) AS s;

-- 5. 1,000,000 Events spanning the last 35 days (~85,700 older than 30 days for retention S11)
WITH templates AS (
    SELECT
        0 AS mod_key,
        'order.created'::text AS ev_type,
        convert_to('{"type":"order.created","occurred_at":"2026-10-01T00:00:00Z","data":{"order_id":"ord_bench","amount_cents":14999,"currency":"USD","customer":{"tier":"enterprise","region":"ap-southeast-1"}}}', 'UTF8') AS wire_body,
        '{"order_id":"ord_bench","amount_cents":14999,"currency":"USD","customer":{"tier":"enterprise","region":"ap-southeast-1"}}'::jsonb AS payload,
        191 AS payload_bytes,
        decode('1111111111111111111111111111111111111111111111111111111111111111', 'hex') AS sha256
    UNION ALL
    SELECT
        1,
        'payment.succeeded',
        convert_to('{"type":"payment.succeeded","occurred_at":"2026-10-01T00:00:00Z","data":{"payment_id":"pay_bench","method":"card","amount_cents":14999,"captured":true}}', 'UTF8'),
        '{"payment_id":"pay_bench","method":"card","amount_cents":14999,"captured":true}'::jsonb,
        152,
        decode('2222222222222222222222222222222222222222222222222222222222222222', 'hex')
    UNION ALL
    SELECT
        2,
        'invoice.paid',
        convert_to('{"type":"invoice.paid","occurred_at":"2026-10-01T00:00:00Z","data":{"invoice_id":"inv_bench","subscription_id":"sub_bench","total_cents":49900}}', 'UTF8'),
        '{"invoice_id":"inv_bench","subscription_id":"sub_bench","total_cents":49900}'::jsonb,
        144,
        decode('3333333333333333333333333333333333333333333333333333333333333333', 'hex')
    UNION ALL
    SELECT
        3,
        'shipment.dispatched',
        convert_to('{"type":"shipment.dispatched","occurred_at":"2026-10-01T00:00:00Z","data":{"shipment_id":"shp_bench","carrier":"dhl","tracking":"DHL99887766"}}', 'UTF8'),
        '{"shipment_id":"shp_bench","carrier":"dhl","tracking":"DHL99887766"}'::jsonb,
        143,
        decode('4444444444444444444444444444444444444444444444444444444444444444', 'hex')
)
INSERT INTO events (
    id, tenant_id, type, schema_version, occurred_at, received_at,
    wire_body, content_type, payload, payload_bytes, payload_sha256,
    status, delivery_count, created_at, updated_at
)
SELECT
    ('019a0100-0000-7000-8000-' || lpad(to_hex(g), 12, '0'))::uuid,
    CASE
        WHEN g % 2 = 1 THEN '019a0000-0000-7000-8000-000000000001'::uuid
        ELSE ('019a0000-0000-7000-8000-' || lpad(to_hex(2 + (g % 9)), 12, '0'))::uuid
    END,
    t.ev_type,
    1,
    now() - interval '35 days' + (g * interval '3.024 seconds'),
    now() - interval '35 days' + (g * interval '3.024 seconds'),
    t.wire_body,
    'application/json',
    t.payload,
    t.payload_bytes,
    t.sha256,
    CASE
        WHEN g > 999650 THEN 'delivering'
        WHEN g % 20 = 0 THEN 'partially_delivered'
        WHEN g % 50 = 0 THEN 'failed'
        ELSE 'delivered'
    END,
    2,
    now() - interval '35 days' + (g * interval '3.024 seconds'),
    now() - interval '35 days' + (g * interval '3.024 seconds')
FROM generate_series(1, 1000000) AS g
JOIN templates t ON t.mod_key = (g % 4);

-- 6. 2,000,000 Deliveries (2 deliveries per event)
--    - 1,909,300 delivered
--    -    50,000 dead_lettered
--    -    40,000 failed
--    -       500 due ('pending' / 'retry_scheduled') for claim plan verification
--    -       200 'in_progress' (100 expired lease for reaper plan verification, 100 active lease)
INSERT INTO deliveries (
    id, tenant_id, event_id, endpoint_id, cycle, cycle_started_at,
    status, attempt_count, attempt_seq,
    max_attempts, initial_backoff_ms, max_backoff_ms, retry_window_seconds, timeout_ms,
    next_attempt_at, lease_token, lease_expires_at, worker_id,
    last_error_code, last_error_detail, last_http_status,
    created_at, updated_at, completed_at, dead_lettered_at
)
SELECT
    ('019a0200-0000-7000-8000-' || lpad(to_hex((g - 1) * 2 + ep), 12, '0'))::uuid,
    CASE
        WHEN g % 2 = 1 THEN '019a0000-0000-7000-8000-000000000001'::uuid
        ELSE ('019a0000-0000-7000-8000-' || lpad(to_hex(2 + (g % 9)), 12, '0'))::uuid
    END,
    ('019a0100-0000-7000-8000-' || lpad(to_hex(g), 12, '0'))::uuid,
    CASE
        WHEN g % 2 = 1 THEN ('019a0000-0001-7000-8000-' || lpad(to_hex(ep), 12, '0'))::uuid
        ELSE ('019a0000-0001-7000-8000-' || lpad(to_hex((1 + (g % 9)) * 2 + ep), 12, '0'))::uuid
    END,
    1,
    now() - interval '35 days' + (g * interval '3.024 seconds'),
    CASE
        WHEN g > 999900 THEN 'in_progress'
        WHEN g > 999650 THEN CASE WHEN ep = 1 THEN 'pending' ELSE 'retry_scheduled' END
        WHEN ep = 2 AND g % 20 = 0 THEN 'dead_lettered'
        WHEN g % 50 = 0 THEN 'failed'
        ELSE 'delivered'
    END,
    CASE
        WHEN g > 999900 THEN 1
        WHEN g > 999650 THEN CASE WHEN ep = 1 THEN 0 ELSE 2 END
        WHEN ep = 2 AND g % 20 = 0 THEN 8
        WHEN g % 50 = 0 THEN 1
        ELSE 1
    END,
    CASE
        WHEN g > 999900 THEN 1
        WHEN g > 999650 THEN CASE WHEN ep = 1 THEN 0 ELSE 2 END
        WHEN ep = 2 AND g % 20 = 0 THEN 8
        WHEN g % 50 = 0 THEN 1
        ELSE 1
    END,
    8, 5000, 3600000, 86400, 15000,
    CASE
        WHEN g > 999650 AND g <= 999900 THEN now() - interval '5 seconds'
        ELSE now() - interval '35 days' + (g * interval '3.024 seconds')
    END,
    CASE
        WHEN g > 999900 THEN ('019a0300-0000-7000-8000-' || lpad(to_hex((g - 1) * 2 + ep), 12, '0'))::uuid
        ELSE NULL
    END,
    CASE
        WHEN g > 999950 THEN now() + interval '60 seconds'
        WHEN g > 999900 THEN now() - interval '30 seconds'
        ELSE NULL
    END,
    CASE
        WHEN g > 999900 THEN 'worker-preload-1'
        ELSE NULL
    END,
    CASE
        WHEN ep = 2 AND g <= 999650 AND g % 20 = 0 THEN 'http_5xx'
        WHEN g <= 999650 AND g % 50 = 0 THEN 'http_4xx'
        ELSE NULL
    END,
    CASE
        WHEN ep = 2 AND g <= 999650 AND g % 20 = 0 THEN 'HTTP 503 Service Unavailable'
        WHEN g <= 999650 AND g % 50 = 0 THEN 'HTTP 422 Unprocessable Entity'
        ELSE NULL
    END,
    CASE
        WHEN g > 999650 THEN NULL
        WHEN ep = 2 AND g % 20 = 0 THEN 503
        WHEN g % 50 = 0 THEN 422
        ELSE 200
    END,
    now() - interval '35 days' + (g * interval '3.024 seconds'),
    now() - interval '35 days' + (g * interval '3.024 seconds') + interval '1 second',
    CASE
        WHEN g <= 999650 THEN now() - interval '35 days' + (g * interval '3.024 seconds') + interval '1 second'
        ELSE NULL
    END,
    CASE
        WHEN g <= 999650 AND ep = 2 AND g % 20 = 0 THEN now() - interval '35 days' + (g * interval '3.024 seconds') + interval '1 second'
        ELSE NULL
    END
FROM generate_series(1, 1000000) AS g
CROSS JOIN generate_series(1, 2) AS ep;

-- 7. 2,050,000 Delivery Attempts
--    - 2,000,000 first attempts (1 per delivery)
--    -    50,000 second attempts for dead-lettered deliveries so multi-attempt history plans are exercised
INSERT INTO delivery_attempts (
    tenant_id, delivery_id, cycle, attempt_no,
    started_at, finished_at, outcome, http_status,
    error_code, error_detail, duration_ms, worker_id
)
SELECT
    d.tenant_id,
    d.id,
    1,
    1,
    d.created_at + interval '100 milliseconds',
    CASE WHEN d.status = 'in_progress' THEN NULL ELSE d.created_at + interval '250 milliseconds' END,
    CASE
        WHEN d.status = 'in_progress' THEN NULL
        WHEN d.status = 'delivered' THEN 'success'
        WHEN d.status = 'failed' THEN 'permanent_failure'
        ELSE 'retryable_failure'
    END,
    d.last_http_status,
    d.last_error_code,
    d.last_error_detail,
    CASE WHEN d.status = 'in_progress' THEN NULL ELSE 15 END,
    'worker-preload-1'
FROM deliveries d;

INSERT INTO delivery_attempts (
    tenant_id, delivery_id, cycle, attempt_no,
    started_at, finished_at, outcome, http_status,
    error_code, error_detail, duration_ms, worker_id
)
SELECT
    d.tenant_id,
    d.id,
    1,
    2,
    d.created_at + interval '5 seconds',
    d.created_at + interval '5 seconds 180 milliseconds',
    'retryable_failure',
    503,
    'http_5xx',
    'HTTP 503 Service Unavailable',
    180,
    'worker-preload-1'
FROM deliveries d
WHERE d.status = 'dead_lettered';

-- 8. 50,000 Idempotency Records (25,000 expired for S11 retention cleanup, 25,000 active)
INSERT INTO idempotency_records (
    tenant_id, scope, key, request_fingerprint, status_code, response_body, created_at, expires_at
)
SELECT
    '019a0000-0000-7000-8000-000000000001'::uuid,
    'POST:/api/v1/events',
    'idem-preload-' || lpad(i::text, 8, '0'),
    decode(repeat('aa', 32), 'hex'),
    202,
    '{"id":"019a0100-0000-7000-8000-000000000001","status":"accepted","delivery_count":2}'::jsonb,
    CASE WHEN i <= 25000 THEN now() - interval '48 hours' ELSE now() - interval '1 hour' END,
    CASE WHEN i <= 25000 THEN now() - interval '24 hours' ELSE now() + interval '23 hours' END
FROM generate_series(1, 50000) AS i;

-- 9. 10,000 Rate Limit Counters (expired windows older than 2 hours for S11 retention cleanup)
INSERT INTO rate_limit_counters (scope_type, scope_id, window_start, count)
SELECT
    'tenant_ingest',
    '019a0000-0000-7000-8000-000000000001'::uuid,
    date_trunc('second', now() - interval '3 hours') - (i * interval '1 second'),
    25
FROM generate_series(1, 10000) AS i;

-- 10. 10,000 Audit Logs
INSERT INTO audit_logs (
    tenant_id, actor_type, actor_id, action, resource_type, resource_id, request_id, ip, metadata, created_at
)
SELECT
    '019a0000-0000-7000-8000-000000000001'::uuid,
    'api_key',
    'benchkey0001',
    CASE WHEN i % 3 = 0 THEN 'event.replay' WHEN i % 3 = 1 THEN 'endpoint.create' ELSE 'endpoint.secret.rotate' END,
    'delivery',
    ('019a0200-0000-7000-8000-' || lpad(to_hex(i), 12, '0'))::uuid,
    'req-preload-' || i,
    '127.0.0.1'::inet,
    '{"reason":"benchmark preload"}'::jsonb,
    now() - interval '30 days' + (i * interval '4 minutes')
FROM generate_series(1, 10000) AS i;

COMMIT;

ANALYZE VERBOSE tenants;
ANALYZE VERBOSE api_credentials;
ANALYZE VERBOSE endpoints;
ANALYZE VERBOSE endpoint_secrets;
ANALYZE VERBOSE events;
ANALYZE VERBOSE deliveries;
ANALYZE VERBOSE delivery_attempts;
ANALYZE VERBOSE idempotency_records;
ANALYZE VERBOSE rate_limit_counters;
ANALYZE VERBOSE audit_logs;

SELECT
    relname AS table_name,
    n_live_tup AS live_rows,
    pg_size_pretty(pg_total_relation_size(relid)) AS total_size,
    pg_size_pretty(pg_relation_size(relid)) AS heap_size,
    pg_size_pretty(pg_indexes_size(relid)) AS index_size
FROM pg_stat_user_tables
ORDER BY pg_total_relation_size(relid) DESC;
