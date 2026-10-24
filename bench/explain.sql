-- RelayForge Phase 6 Query Plan Analysis (1M events / 2M deliveries / 2.05M attempts)
-- Usage:
--   psql "$RELAYFORGE_DATABASE_URL" -v ON_ERROR_STOP=1 -f bench/explain.sql
--
-- Exercises every named access path in docs/02-data-model.md §4 and docs/07-benchmark-plan.md §9.

\pset pager off

\echo '=== Q1: Claim due deliveries (deliveries_claim_idx) [docs/07-benchmark-plan.md §9] ==='
BEGIN;
EXPLAIN (ANALYZE, BUFFERS)
SELECT id FROM deliveries
WHERE status IN ('pending','retry_scheduled')
  AND next_attempt_at <= now()
ORDER BY next_attempt_at, id
FOR UPDATE SKIP LOCKED
LIMIT 20;
ROLLBACK;

\echo '=== Q2: Reap expired leases (deliveries_lease_idx) [docs/02-data-model.md §4] ==='
BEGIN;
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, tenant_id, event_id, cycle, attempt_count, attempt_seq,
       cycle_started_at, max_attempts, initial_backoff_ms,
       max_backoff_ms, retry_window_seconds, worker_id
FROM deliveries
WHERE status = 'in_progress'
  AND lease_expires_at < now()
ORDER BY lease_expires_at
FOR UPDATE SKIP LOCKED
LIMIT 100;
ROLLBACK;

\echo '=== Q3: List tenant events with cursor (events_tenant_created_idx) [docs/07-benchmark-plan.md §9] ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, type, status, created_at
FROM events
WHERE tenant_id = '019a0000-0000-7000-8000-000000000001'::uuid
  AND (created_at, id) < (now() - interval '5 days', '019a0100-0000-7000-8000-0000000d0000'::uuid)
ORDER BY created_at DESC, id DESC
LIMIT 25;

\echo '=== Q4a: List tenant events by type (populated type: payment.succeeded, 250k rows) [docs/02-data-model.md §4] ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, type, status, created_at
FROM events
WHERE tenant_id = '019a0000-0000-7000-8000-000000000001'::uuid
  AND type = 'payment.succeeded'
  AND (created_at, id) < (now() - interval '5 days', '019a0100-0000-7000-8000-0000000d0000'::uuid)
ORDER BY created_at DESC, id DESC
LIMIT 25;

\echo '=== Q4b: List tenant events by type (sparse/zero-row type for tenant 1: order.created) ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, type, status, created_at
FROM events
WHERE tenant_id = '019a0000-0000-7000-8000-000000000001'::uuid
  AND type = 'order.created'
  AND (created_at, id) < (now() - interval '5 days', '019a0100-0000-7000-8000-0000000d0000'::uuid)
ORDER BY created_at DESC, id DESC
LIMIT 25;

\echo '=== Q5: List dead-lettered deliveries for tenant (deliveries_tenant_status_created_idx) [docs/07-benchmark-plan.md §9] ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, status, attempt_count, next_attempt_at
FROM deliveries
WHERE tenant_id = '019a0000-0000-7000-8000-000000000001'::uuid
  AND status = 'dead_lettered'
ORDER BY created_at DESC, id DESC
LIMIT 25;

\echo '=== Q6a: Event detail + deliveries (deliveries_event_idx) [docs/02-data-model.md §4] ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, endpoint_id, cycle, status, attempt_count, max_attempts, next_attempt_at
FROM deliveries
WHERE tenant_id = '019a0000-0000-7000-8000-000000000001'::uuid
  AND event_id = '019a0100-0000-7000-8000-00000007a121'::uuid
ORDER BY created_at DESC, id DESC;

\echo '=== Q6b: Event status projection count (deliveries_event_idx) [worker/src/store.rs project_event_status_tx] ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT count(*) AS total,
       count(*) FILTER (WHERE status = 'delivered') AS delivered,
       count(*) FILTER (WHERE status IN ('failed','dead_lettered','canceled')) AS failed,
       count(*) FILTER (WHERE status IN ('pending','in_progress','retry_scheduled')) AS pending
FROM deliveries
WHERE event_id = '019a0100-0000-7000-8000-00000007a121'::uuid;

\echo '=== Q7: Delivery attempt history (delivery_attempts_delivery_idx) [docs/02-data-model.md §4] ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, attempt_no, cycle, started_at, finished_at, outcome, http_status,
       error_code, error_detail, duration_ms
FROM delivery_attempts
WHERE tenant_id = '019a0000-0000-7000-8000-000000000001'::uuid
  AND delivery_id = '019a0200-0000-7000-8000-000000000029'::uuid
ORDER BY started_at DESC, id DESC
LIMIT 50;

\echo '=== Q8: Auth lookup by key_id (api_credentials_key_id_key) [docs/02-data-model.md §4] ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT c.id, c.tenant_id, c.key_id, c.secret_hmac, c.role, c.expires_at, c.revoked_at, t.status
FROM api_credentials c
JOIN tenants t ON t.id = c.tenant_id
WHERE c.key_id = 'benchkey0001';

\echo '=== Q9: Active secrets for delivery (endpoint_secrets_active_idx) [docs/02-data-model.md §4] ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, tenant_id, endpoint_id, ciphertext, nonce, key_version
FROM endpoint_secrets
WHERE endpoint_id = '019a0000-0001-7000-8000-000000000001'::uuid
  AND status = 'active'
  AND (expires_at IS NULL OR expires_at > now())
ORDER BY created_at DESC, id DESC;

\echo '=== Q10: Fan-out endpoint lookup (endpoints_fanout_idx) [docs/02-data-model.md §4] ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT id, event_types, max_attempts, initial_backoff_ms, max_backoff_ms,
       retry_window_seconds, timeout_ms, rate_limit_per_sec
FROM endpoints
WHERE tenant_id = '019a0000-0000-7000-8000-000000000001'::uuid
  AND status = 'active'
  AND deleted_at IS NULL;

\echo '=== Q11: Idempotency replay lookup (idempotency_records_pkey) [docs/02-data-model.md §4] ==='
EXPLAIN (ANALYZE, BUFFERS)
SELECT request_fingerprint, status_code, response_body, expires_at
FROM idempotency_records
WHERE tenant_id = '019a0000-0000-7000-8000-000000000001'::uuid
  AND scope = 'POST:/api/v1/events'
  AND key = 'idem-preload-00030000';

\echo '=== Q12: Retention delete batch on events (events_retention_idx) [docs/02-data-model.md §4] ==='
BEGIN;
EXPLAIN (ANALYZE, BUFFERS)
SELECT e.id FROM events e
WHERE e.created_at <= now() - interval '30 days'
  AND NOT EXISTS (
      SELECT 1 FROM deliveries d
      WHERE d.event_id = e.id
        AND d.status IN ('pending', 'in_progress', 'retry_scheduled')
  )
ORDER BY e.created_at
LIMIT 5000
FOR UPDATE SKIP LOCKED;
ROLLBACK;
