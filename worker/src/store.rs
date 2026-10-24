use anyhow::{Context, Result};
use chrono::{DateTime, Utc};
use sqlx::postgres::PgRow;
use sqlx::{FromRow, PgPool, Postgres, Row, Transaction};
use uuid::Uuid;

use crate::policy::{self, AttemptOutcome, RetryDecision};

/// Row returned by the normative claim query (`contracts/delivery-contract-v1.md` §4).
#[derive(Debug, Clone)]
pub struct ClaimedDelivery {
    pub id: Uuid,
    pub tenant_id: Uuid,
    pub event_id: Uuid,
    pub endpoint_id: Uuid,
    pub cycle: i32,
    pub cycle_started_at: DateTime<Utc>,
    pub attempt_count: i32,
    pub attempt_seq: i32,
    pub max_attempts: i32,
    pub initial_backoff_ms: i32,
    pub max_backoff_ms: i32,
    pub retry_window_seconds: i32,
    pub timeout_ms: i32,
    pub lease_token: Uuid,
    pub lease_expires_at: DateTime<Utc>,
}

impl<'r> FromRow<'r, PgRow> for ClaimedDelivery {
    fn from_row(row: &'r PgRow) -> std::result::Result<Self, sqlx::Error> {
        Ok(Self {
            id: row.try_get("id")?,
            tenant_id: row.try_get("tenant_id")?,
            event_id: row.try_get("event_id")?,
            endpoint_id: row.try_get("endpoint_id")?,
            cycle: row.try_get("cycle")?,
            cycle_started_at: row.try_get("cycle_started_at")?,
            attempt_count: row.try_get("attempt_count")?,
            attempt_seq: row.try_get("attempt_seq")?,
            max_attempts: row.try_get("max_attempts")?,
            initial_backoff_ms: row.try_get("initial_backoff_ms")?,
            max_backoff_ms: row.try_get("max_backoff_ms")?,
            retry_window_seconds: row.try_get("retry_window_seconds")?,
            timeout_ms: row.try_get("timeout_ms")?,
            lease_token: row.try_get("lease_token")?,
            lease_expires_at: row.try_get("lease_expires_at")?,
        })
    }
}

#[derive(Debug, Clone)]
pub struct EncryptedSecretRow {
    pub id: Uuid,
    pub tenant_id: Uuid,
    pub endpoint_id: Uuid,
    pub ciphertext: Vec<u8>,
    pub nonce: Vec<u8>,
    pub key_version: i32,
}

impl<'r> FromRow<'r, PgRow> for EncryptedSecretRow {
    fn from_row(row: &'r PgRow) -> std::result::Result<Self, sqlx::Error> {
        Ok(Self {
            id: row.try_get("id")?,
            tenant_id: row.try_get("tenant_id")?,
            endpoint_id: row.try_get("endpoint_id")?,
            ciphertext: row.try_get("ciphertext")?,
            nonce: row.try_get("nonce")?,
            key_version: row.try_get("key_version")?,
        })
    }
}

#[derive(Debug, Clone)]
pub struct DeliveryContext {
    pub event_type: String,
    pub wire_body: Vec<u8>,
    pub trace_parent: Option<String>,
    pub endpoint_url: String,
    pub rate_limit_per_sec: i32,
    pub endpoint_status: String,
    pub endpoint_deleted: bool,
    pub secrets: Vec<EncryptedSecretRow>,
}

#[derive(Debug, Clone)]
pub struct CompleteAttemptInput {
    pub delivery_id: Uuid,
    pub event_id: Uuid,
    pub endpoint_id: Uuid,
    pub lease_token: Uuid,
    pub cycle: i32,
    pub attempt_no: i32,
    pub next_status: &'static str,
    pub next_attempt_at: Option<DateTime<Utc>>,
    pub outcome: AttemptOutcome,
    pub http_status: Option<i32>,
    pub error_code: Option<&'static str>,
    pub error_detail: Option<String>,
    pub duration_ms: i32,
}

/// Claims at most `batch_limit` due deliveries in a single short transaction using
/// `FOR UPDATE SKIP LOCKED` per `contracts/delivery-contract-v1.md` §4.
pub async fn claim_due_deliveries(
    pool: &PgPool,
    worker_id: &str,
    batch_limit: i64,
    lease_seconds: i64,
) -> Result<Vec<ClaimedDelivery>> {
    if batch_limit <= 0 {
        return Ok(Vec::new());
    }
    let rows = sqlx::query_as::<_, ClaimedDelivery>(
        r#"
        WITH due AS (
            SELECT id
            FROM deliveries
            WHERE status IN ('pending', 'retry_scheduled')
              AND next_attempt_at <= now()
            ORDER BY next_attempt_at, id
            FOR UPDATE SKIP LOCKED
            LIMIT $1
        ), leased AS (
            UPDATE deliveries d
               SET status           = 'in_progress',
                   lease_token      = gen_random_uuid(),
                   lease_expires_at = now() + make_interval(secs => $2::double precision),
                   worker_id        = $3,
                   attempt_count    = d.attempt_count + 1,
                   attempt_seq      = d.attempt_seq + 1,
                   updated_at       = now()
              FROM due
             WHERE d.id = due.id
            RETURNING d.*
        ), attempt AS (
            INSERT INTO delivery_attempts
                (tenant_id, delivery_id, cycle, attempt_no, started_at, worker_id)
            SELECT tenant_id, id, cycle, attempt_seq, now(), worker_id
            FROM leased
            RETURNING delivery_id
        )
        SELECT l.id, l.tenant_id, l.event_id, l.endpoint_id, l.cycle,
               l.cycle_started_at, l.attempt_count, l.attempt_seq,
               l.max_attempts, l.initial_backoff_ms, l.max_backoff_ms,
               l.retry_window_seconds, l.timeout_ms,
               l.lease_token, l.lease_expires_at
        FROM leased l
        ORDER BY l.next_attempt_at, l.id
        "#,
    )
    .bind(batch_limit)
    .bind(lease_seconds as f64)
    .bind(worker_id)
    .fetch_all(pool)
    .await
    .context("claim due deliveries")?;

    Ok(rows)
}

/// Loads the immutable event `wire_body`, endpoint destination, and active secrets
/// required to execute a claimed attempt.
pub async fn load_delivery_context(
    pool: &PgPool,
    event_id: Uuid,
    endpoint_id: Uuid,
) -> Result<DeliveryContext> {
    let (event_type, wire_body, trace_parent): (String, Vec<u8>, Option<String>) = sqlx::query_as(
        r#"
            SELECT type, wire_body, trace_parent
            FROM events
            WHERE id = $1
            "#,
    )
    .bind(event_id)
    .fetch_one(pool)
    .await
    .context("load event for delivery")?;

    let (endpoint_url, rate_limit_per_sec, endpoint_status, deleted_at): (
        String,
        i32,
        String,
        Option<DateTime<Utc>>,
    ) = sqlx::query_as(
        r#"
        SELECT url, rate_limit_per_sec, status, deleted_at
        FROM endpoints
        WHERE id = $1
        "#,
    )
    .bind(endpoint_id)
    .fetch_one(pool)
    .await
    .context("load endpoint for delivery")?;

    let secrets = sqlx::query_as::<_, EncryptedSecretRow>(
        r#"
        SELECT id, tenant_id, endpoint_id, ciphertext, nonce, key_version
        FROM endpoint_secrets
        WHERE endpoint_id = $1
          AND status = 'active'
          AND (expires_at IS NULL OR expires_at > now())
        ORDER BY created_at DESC, id DESC
        "#,
    )
    .bind(endpoint_id)
    .fetch_all(pool)
    .await
    .context("load active endpoint secrets")?;

    Ok(DeliveryContext {
        event_type,
        wire_body,
        trace_parent,
        endpoint_url,
        rate_limit_per_sec,
        endpoint_status,
        endpoint_deleted: deleted_at.is_some(),
        secrets,
    })
}

/// Atomically acquires a 1-second fixed-window token for a destination endpoint
/// from `rate_limit_counters`. Returns `true` if admitted, `false` if rate-limited.
pub async fn acquire_destination_rate_limit(
    pool: &PgPool,
    endpoint_id: Uuid,
    rate_limit_per_sec: i32,
) -> Result<bool> {
    let limit = rate_limit_per_sec.max(1);
    let acquired: Option<i32> = sqlx::query_scalar(
        r#"
        INSERT INTO rate_limit_counters (scope_type, scope_id, window_start, count)
        VALUES ('destination', $1, date_trunc('second', now()), 1)
        ON CONFLICT (scope_type, scope_id, window_start)
        DO UPDATE SET count = rate_limit_counters.count + 1
        WHERE rate_limit_counters.count < $2
        RETURNING count
        "#,
    )
    .bind(endpoint_id)
    .bind(limit)
    .fetch_optional(pool)
    .await
    .context("acquire destination rate limit")?;

    Ok(acquired.is_some())
}

/// Defers a claimed delivery when the destination rate limiter denies the attempt
/// (`contracts/delivery-contract-v1.md` §3.1): marks the attempt `rate_limited`,
/// decrements `attempt_count` so no attempt is consumed, clears the lease, and
/// schedules retry at the end of the 1-second rate-limit window.
pub async fn defer_rate_limited(
    pool: &PgPool,
    delivery_id: Uuid,
    lease_token: Uuid,
    cycle: i32,
    attempt_no: i32,
) -> Result<bool> {
    let mut tx = pool.begin().await.context("begin rate-limit deferral tx")?;

    let updated: Option<Uuid> = sqlx::query_scalar(
        r#"
        UPDATE deliveries
           SET status           = 'retry_scheduled',
               attempt_count    = GREATEST(attempt_count - 1, 0),
               lease_token      = NULL,
               lease_expires_at = NULL,
               worker_id        = NULL,
               next_attempt_at  = date_trunc('second', now()) + interval '1 second',
               updated_at       = now()
         WHERE id = $1
           AND lease_token = $2
           AND status = 'in_progress'
        RETURNING id
        "#,
    )
    .bind(delivery_id)
    .bind(lease_token)
    .fetch_optional(&mut *tx)
    .await
    .context("defer delivery on rate limit")?;

    let Some(_) = updated else {
        tx.rollback().await.ok();
        return Ok(false);
    };

    sqlx::query(
        r#"
        UPDATE delivery_attempts
           SET finished_at  = now(),
               outcome      = 'rate_limited',
               error_code   = 'rate_limited',
               error_detail = 'deferred by destination rate limiter',
               duration_ms  = 0
         WHERE delivery_id = $1
           AND cycle = $2
           AND attempt_no = $3
           AND finished_at IS NULL
        "#,
    )
    .bind(delivery_id)
    .bind(cycle)
    .bind(attempt_no)
    .execute(&mut *tx)
    .await
    .context("mark attempt rate_limited")?;

    tx.commit().await.context("commit rate-limit deferral")?;
    Ok(true)
}

/// Releases a claimed delivery during graceful shutdown when HTTP has not started
/// (`contracts/delivery-contract-v1.md` §10 step 4): marks the attempt `canceled`,
/// refunds `attempt_count = attempt_count - 1`, clears the lease, and schedules
/// `next_attempt_at = now() + 1s`.
pub async fn release_unstarted_claim(
    pool: &PgPool,
    delivery_id: Uuid,
    lease_token: Uuid,
    cycle: i32,
    attempt_no: i32,
) -> Result<bool> {
    let mut tx = pool.begin().await.context("begin release unstarted tx")?;

    let updated: Option<Uuid> = sqlx::query_scalar(
        r#"
        UPDATE deliveries
           SET status           = 'retry_scheduled',
               attempt_count    = GREATEST(attempt_count - 1, 0),
               lease_token      = NULL,
               lease_expires_at = NULL,
               worker_id        = NULL,
               next_attempt_at  = now() + interval '1 second',
               updated_at       = now()
         WHERE id = $1
           AND lease_token = $2
           AND status = 'in_progress'
        RETURNING id
        "#,
    )
    .bind(delivery_id)
    .bind(lease_token)
    .fetch_optional(&mut *tx)
    .await
    .context("release unstarted delivery lease")?;

    let Some(_) = updated else {
        tx.rollback().await.ok();
        return Ok(false);
    };

    sqlx::query(
        r#"
        UPDATE delivery_attempts
           SET finished_at  = now(),
               outcome      = 'canceled',
               error_code   = 'shutdown_before_send',
               error_detail = 'worker shutting down before HTTP started',
               duration_ms  = 0
         WHERE delivery_id = $1
           AND cycle = $2
           AND attempt_no = $3
           AND finished_at IS NULL
        "#,
    )
    .bind(delivery_id)
    .bind(cycle)
    .bind(attempt_no)
    .execute(&mut *tx)
    .await
    .context("mark unstarted attempt canceled")?;

    tx.commit().await.context("commit release unstarted")?;
    Ok(true)
}

/// Completes a delivery attempt in a single lease-fenced transaction and updates
/// the parent event's status projection (`contracts/delivery-contract-v1.md` §5).
/// Returns `Ok(false)` if the worker lost its lease (`lease_token` mismatch).
pub async fn complete_attempt(pool: &PgPool, input: CompleteAttemptInput) -> Result<bool> {
    let mut tx = pool.begin().await.context("begin completion tx")?;

    let detail = input
        .error_detail
        .as_deref()
        .map(policy::sanitize_error_detail);

    // 1) Fence on the lease. Zero rows means the worker lost the lease.
    let fenced: Option<(Uuid, Uuid, i32, i32, i32)> = sqlx::query_as(
        r#"
        UPDATE deliveries
           SET status            = $3,
               lease_token       = NULL,
               lease_expires_at  = NULL,
               worker_id         = NULL,
               next_attempt_at   = COALESCE($4, next_attempt_at),
               last_error_code   = $5,
               last_error_detail = $6,
               last_http_status  = $7,
               completed_at      = CASE WHEN $3 IN ('delivered','failed','dead_lettered','canceled')
                                        THEN now() ELSE NULL END,
               dead_lettered_at  = CASE WHEN $3 = 'dead_lettered' THEN now() ELSE NULL END,
               updated_at        = now()
         WHERE id = $1
           AND lease_token = $2
           AND status = 'in_progress'
        RETURNING id, event_id, attempt_count, attempt_seq, cycle
        "#,
    )
    .bind(input.delivery_id)
    .bind(input.lease_token)
    .bind(input.next_status)
    .bind(input.next_attempt_at)
    .bind(input.error_code)
    .bind(detail.as_deref())
    .bind(input.http_status)
    .fetch_optional(&mut *tx)
    .await
    .context("update delivery on completion")?;

    let Some((_, event_id, _, _, _)) = fenced else {
        tx.rollback().await.ok();
        return Ok(false);
    };

    // 2) Only if step 1 returned one row, record the attempt outcome.
    sqlx::query(
        r#"
        UPDATE delivery_attempts
           SET finished_at  = now(),
               outcome      = $4,
               http_status  = $5,
               error_code   = $6,
               error_detail = $7,
               duration_ms  = $8
         WHERE delivery_id = $1
           AND cycle = $2
           AND attempt_no = $3
           AND finished_at IS NULL
        "#,
    )
    .bind(input.delivery_id)
    .bind(input.cycle)
    .bind(input.attempt_no)
    .bind(input.outcome.as_str())
    .bind(input.http_status)
    .bind(input.error_code)
    .bind(detail.as_deref())
    .bind(input.duration_ms.max(0))
    .execute(&mut *tx)
    .await
    .context("update delivery_attempts on completion")?;

    // Track consecutive_failures on the endpoint.
    if input.next_status == "delivered" {
        sqlx::query(
            "UPDATE endpoints SET consecutive_failures = 0, updated_at = now() WHERE id = $1 AND consecutive_failures <> 0",
        )
        .bind(input.endpoint_id)
        .execute(&mut *tx)
        .await
        .context("reset endpoint consecutive_failures")?;
    } else if input.next_status == "failed" || input.next_status == "dead_lettered" {
        sqlx::query(
            "UPDATE endpoints SET consecutive_failures = consecutive_failures + 1, updated_at = now() WHERE id = $1",
        )
        .bind(input.endpoint_id)
        .execute(&mut *tx)
        .await
        .context("increment endpoint consecutive_failures")?;
    }

    // 3) Project parent event status (always lock delivery -> event).
    project_event_status_tx(&mut tx, event_id).await?;

    tx.commit().await.context("commit completion tx")?;
    Ok(true)
}

/// Updates `events.status` inside an existing transaction per `contracts/delivery-contract-v1.md` §5.1.
pub async fn project_event_status_tx(
    tx: &mut Transaction<'_, Postgres>,
    event_id: Uuid,
) -> Result<()> {
    sqlx::query("SELECT id FROM events WHERE id = $1 FOR UPDATE")
        .bind(event_id)
        .fetch_optional(&mut **tx)
        .await
        .context("lock event row for projection")?;

    sqlx::query(
        r#"
        UPDATE events e
           SET status = CASE
                 WHEN s.pending > 0        THEN 'delivering'
                 WHEN s.delivered = s.total THEN 'delivered'
                 WHEN s.delivered > 0       THEN 'partially_delivered'
                 WHEN s.failed = s.total    THEN 'failed'
                 ELSE e.status
               END,
               updated_at = now()
          FROM (
              SELECT count(*) AS total,
                     count(*) FILTER (WHERE status = 'delivered') AS delivered,
                     count(*) FILTER (WHERE status IN ('failed','dead_lettered','canceled')) AS failed,
                     count(*) FILTER (WHERE status IN ('pending','in_progress','retry_scheduled')) AS pending
              FROM deliveries
              WHERE event_id = $1
          ) s
         WHERE e.id = $1
           AND s.total > 0
        "#,
    )
    .bind(event_id)
    .execute(&mut **tx)
    .await
    .context("update event status projection")?;

    Ok(())
}

#[derive(Debug)]
struct ExpiredLeaseRow {
    id: Uuid,
    tenant_id: Uuid,
    event_id: Uuid,
    cycle: i32,
    attempt_count: i32,
    attempt_seq: i32,
    cycle_started_at: DateTime<Utc>,
    max_attempts: i32,
    initial_backoff_ms: i32,
    max_backoff_ms: i32,
    retry_window_seconds: i32,
    worker_id: Option<String>,
}

impl<'r> FromRow<'r, PgRow> for ExpiredLeaseRow {
    fn from_row(row: &'r PgRow) -> std::result::Result<Self, sqlx::Error> {
        Ok(Self {
            id: row.try_get("id")?,
            tenant_id: row.try_get("tenant_id")?,
            event_id: row.try_get("event_id")?,
            cycle: row.try_get("cycle")?,
            attempt_count: row.try_get("attempt_count")?,
            attempt_seq: row.try_get("attempt_seq")?,
            cycle_started_at: row.try_get("cycle_started_at")?,
            max_attempts: row.try_get("max_attempts")?,
            initial_backoff_ms: row.try_get("initial_backoff_ms")?,
            max_backoff_ms: row.try_get("max_backoff_ms")?,
            retry_window_seconds: row.try_get("retry_window_seconds")?,
            worker_id: row.try_get("worker_id")?,
        })
    }
}

/// Reaps expired `in_progress` leases per `contracts/delivery-contract-v1.md` §6,
/// including missing-attempt reconciliation, and returns the number of recovered rows.
pub async fn reap_expired_leases(pool: &PgPool, batch_limit: i64) -> Result<u64> {
    if batch_limit <= 0 {
        return Ok(0);
    }
    let mut tx = pool.begin().await.context("begin reaper tx")?;

    let expired = sqlx::query_as::<_, ExpiredLeaseRow>(
        r#"
        SELECT id, tenant_id, event_id, cycle, attempt_count, attempt_seq,
               cycle_started_at, max_attempts, initial_backoff_ms,
               max_backoff_ms, retry_window_seconds, worker_id
        FROM deliveries
        WHERE status = 'in_progress'
          AND lease_expires_at < now()
        ORDER BY lease_expires_at
        FOR UPDATE SKIP LOCKED
        LIMIT $1
        "#,
    )
    .bind(batch_limit)
    .fetch_all(&mut *tx)
    .await
    .context("select expired leases")?;

    if expired.is_empty() {
        tx.rollback().await.ok();
        return Ok(0);
    }

    let now = Utc::now();
    let mut recovered = 0u64;

    for row in expired {
        let attempt_no = row.attempt_seq.max(1);

        // Reconcile missing attempt row if absent, then mark abandoned (§6).
        sqlx::query(
            r#"
            INSERT INTO delivery_attempts
                (tenant_id, delivery_id, cycle, attempt_no, started_at, finished_at,
                 outcome, error_code, error_detail, worker_id)
            VALUES ($1, $2, $3, $4, now(), now(),
                    'abandoned', 'lease_expired', 'lease expired before completion', $5)
            ON CONFLICT (delivery_id, cycle, attempt_no)
            DO UPDATE
               SET finished_at  = now(),
                   outcome      = 'abandoned',
                   error_code   = 'lease_expired',
                   error_detail = 'lease expired before completion'
             WHERE delivery_attempts.finished_at IS NULL
            "#,
        )
        .bind(row.tenant_id)
        .bind(row.id)
        .bind(row.cycle)
        .bind(attempt_no)
        .bind(row.worker_id.as_deref())
        .execute(&mut *tx)
        .await
        .context("mark expired attempt abandoned")?;

        let delay_ms = policy::sample_backoff_ms(
            row.attempt_count,
            row.initial_backoff_ms,
            row.max_backoff_ms,
            None,
        );
        let decision = policy::decide_retry(
            now,
            row.cycle_started_at,
            row.attempt_count,
            row.max_attempts,
            row.retry_window_seconds,
            delay_ms,
        );

        let (next_status, next_attempt_at) = match decision {
            RetryDecision::RetryScheduled { next_attempt_at } => {
                ("retry_scheduled", next_attempt_at)
            }
            RetryDecision::DeadLettered => ("dead_lettered", now),
        };

        let res = sqlx::query(
            r#"
            UPDATE deliveries
               SET status            = $2,
                   next_attempt_at   = $3,
                   lease_token       = NULL,
                   lease_expires_at  = NULL,
                   worker_id         = NULL,
                   last_error_code   = 'lease_expired',
                   last_error_detail = 'lease expired before completion',
                   completed_at      = CASE WHEN $2 = 'dead_lettered' THEN now() ELSE NULL END,
                   dead_lettered_at  = CASE WHEN $2 = 'dead_lettered' THEN now() ELSE NULL END,
                   updated_at        = now()
             WHERE id = $1
               AND status = 'in_progress'
               AND lease_expires_at < now()
            "#,
        )
        .bind(row.id)
        .bind(next_status)
        .bind(next_attempt_at)
        .execute(&mut *tx)
        .await
        .context("update reaped delivery")?;

        if res.rows_affected() > 0 {
            recovered += 1;
            project_event_status_tx(&mut tx, row.event_id).await?;
        }
    }

    tx.commit().await.context("commit reaper tx")?;
    Ok(recovered)
}

/// Samples queue backlog depth and oldest due age in seconds for Prometheus gauges.
pub async fn sample_queue_stats(pool: &PgPool) -> Result<(i64, i64)> {
    let (due_count, oldest_age_secs): (i64, i64) = sqlx::query_as(
        r#"
        SELECT count(*)::bigint,
               COALESCE(MAX(EXTRACT(EPOCH FROM (now() - next_attempt_at)))::bigint, 0)
        FROM deliveries
        WHERE status IN ('pending', 'retry_scheduled')
          AND next_attempt_at <= now()
        "#,
    )
    .fetch_one(pool)
    .await
    .context("sample queue stats")?;

    Ok((due_count, oldest_age_secs.max(0)))
}
