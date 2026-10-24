# Incident recovery procedures

Status: Phase 4 verified against PostgreSQL 18.6. Last updated: 2026-10-23.
All SQL diagnostic, break-glass reaper, and data-integrity queries in this document and
[data model §8](02-data-model.md) have been executed against the local Compose PostgreSQL 18.6
stack (`docs/15-phase-4-report.md`).

## 1. Principles

1. `deliveries` is authoritative; `events.status` is a projection that can be repaired.
2. `delivery_attempts` is append-only. Do not delete rows to “fix” history.
3. Prefer SQL that is lease-guarded or status-guarded. Never `UPDATE deliveries SET status='pending'`
   without a `WHERE` that excludes `in_progress`/`delivered`.
4. Replay through the API when possible so audit rows are written.
5. After any manual SQL, run the integrity queries in [data model §8](02-data-model.md).

## 2. Stuck queue (oldest due age growing)

Symptoms: `relayforge_queue_oldest_due_age_seconds` above alert threshold; ingest healthy.

Checks:

```sql
SELECT status, count(*), min(next_attempt_at), min(created_at)
FROM deliveries
GROUP BY status;

SELECT id, tenant_id, status, next_attempt_at, last_error_code, attempt_count
FROM deliveries
WHERE status IN ('pending','retry_scheduled')
  AND next_attempt_at <= now()
ORDER BY next_attempt_at
LIMIT 50;

SELECT id, worker_id, lease_expires_at, now() - lease_expires_at AS overdue
FROM deliveries
WHERE status = 'in_progress'
ORDER BY lease_expires_at
LIMIT 50;
```

Causes and actions:

| Cause | Action |
| --- | --- |
| Workers down / not ready | Restore workers; check `/readyz` (`schema_behind`, `schema_dirty`) |
| Destination outage | Pause the endpoint via API; let backoff work; do not reset `attempt_count` |
| Claim contention | Capture `EXPLAIN ANALYZE` of the claim query; add workers only if CPU is idle |
| Expired leases not reaped | Confirm reaper loop; if stuck, wait `WORKER_LEASE_SECONDS`; do not steal leases by SQL unless §4 |
| Autovacuum lag | Check `n_dead_tup` on `deliveries`; tune, do not `VACUUM FULL` on a hot primary without a window |

## 3. Failing destination

Symptoms: high `http_5xx` / `timeout` for one `endpoint_id`; other endpoints healthy.

1. `GET /api/v1/deliveries?endpoint_id=…&status=retry_scheduled,failed,dead_lettered`
2. Confirm receiver health independently (not through RelayForge).
3. Pause the endpoint (`PATCH` `status=paused`) if it is poisoning worker slots.
4. After the receiver is fixed, replay failed/dead-lettered deliveries with a required `reason`.
5. Do not replay `delivered` deliveries in v1.

## 4. Expired leases / zombie in_progress

If `lease_expires_at < now()` and `status = 'in_progress'`, the reaper should recover within one reap
interval. If it does not:

1. Confirm no live worker still holds that `lease_token` (worker logs, `worker_id`).
2. Wait a full lease duration after the last worker death.
3. Only then, as a break-glass (audited, operator-approved):

```sql
-- Break-glass: mark abandoned and reschedule. Do not use while a worker may still complete.
UPDATE delivery_attempts
   SET finished_at = now(),
       outcome = 'abandoned',
       error_code = 'lease_expired',
       error_detail = 'operator reaper'
 WHERE delivery_id = $1
   AND finished_at IS NULL;

UPDATE deliveries
   SET status = 'retry_scheduled',
       next_attempt_at = now(),
       lease_token = NULL,
       lease_expires_at = NULL,
       worker_id = NULL,
       last_error_code = 'lease_expired',
       updated_at = now()
 WHERE id = $1
   AND status = 'in_progress'
   AND lease_expires_at < now();
```

Expect possible duplicate delivery (failure windows F6–F9). Notify the tenant if the event is
financially sensitive.

## 5. Database outage

1. API `/readyz` fails; ingest returns `503`. Clients with idempotency keys may retry.
2. Do not start a second primary. Restore from backup/PITR per deployment procedure (not provided by
   the application).
3. After PostgreSQL is accepting writes: confirm `schema_migrations.dirty = false`.
4. Start API then workers (contract deploy order).
5. Expect a retry burst; destination rate limits exist to bound it. Watch queue age, not only QPS.
6. Idempotency: in-flight ingest that committed but lost the HTTP response will replay stored `201`
   (F2). Incomplete transactions produce no rows (F1).

## 6. Secret compromise

### API key

1. `DELETE /api/v1/credentials/{id}` (immediate revoke) or admin suspend of the tenant if blast radius
   is unclear.
2. Issue a replacement; rotate any integration that stored the old key.
3. Inspect `audit_logs` and `events` created after the suspected leak.

### Signing secret

1. Create a new secret (`POST /endpoints/{id}/secrets`).
2. **Revoke the compromised secret immediately** (`DELETE …/secrets/{id}`). Do not wait for overlap if
   compromise is suspected. Overlap is an availability feature.
3. Receivers must load the new secret. Deliveries already on the wire may still verify with the old
   secret; that window cannot be closed.
4. If the **KEK** is compromised, treat all signing secrets as compromised: rotate KEK, re-encrypt,
   rotate every endpoint secret, revoke old secrets. This is a platform incident.

## 7. Schema dirty / migration failure

`schema_migrations.dirty = true` fails worker and API readiness. Do not “fix” by flipping the flag
unless the DBA has verified the schema matches the intended version. Restore from backup or finish
the migration after inspection. Deploy order: migrate → API → worker.

## 8. Event status projection drift

If `events.status` disagrees with deliveries, recompute using the contract SQL in
`contracts/delivery-contract-v1.md` §5.1. Do not invent a status. Record the repair in the ops log
(there is no tenant-facing audit for this system action unless an admin endpoint is used).

## 9. Alert recommendations (actionable)

| Condition | Why it is actionable |
| --- | --- |
| Queue oldest age > 60 s at reference load | Work is not draining; page the on-call |
| `in_progress` with `lease_expires_at` overdue > 2× lease | Reaper failed |
| `schema_dirty` | Stop deploys; DBA |
| SSRF block spike | Probing or a bad client |
| Ingest p99 > 750 ms for 5 min | Saturation |
| Delivery success for previously healthy endpoint < 90% for 10 min | Destination or signing failure |
| Worker `/readyz` failing while `/healthz` ok | Schema or DB, not a crash loop yet |

Thresholds are starting points; Phase 6 revises them with measurements.

## 10. What this runbook cannot do

- It cannot prove Compose egress isolation.
- It cannot recover a lost KEK (ciphertext is unreadable).
- It cannot undo a receiver’s business effect after a duplicate delivery.
- It cannot erase payloads already delivered.
