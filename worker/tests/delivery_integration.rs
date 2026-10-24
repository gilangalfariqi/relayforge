use std::collections::HashSet;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::Duration;

use aes_gcm::aead::{Aead, KeyInit, Payload};
use aes_gcm::{Aes256Gcm, Nonce};
use axum::Router;
use axum::body::Bytes;
use axum::extract::State;
use axum::http::{HeaderMap, StatusCode, header};
use axum::response::IntoResponse;
use axum::routing::post;
use base64::Engine;
use base64::engine::general_purpose::STANDARD as BASE64_STD;
use chrono::Utc;
use sqlx::PgPool;
use sqlx::postgres::PgPoolOptions;
use tokio::sync::watch;
use uuid::Uuid;

use relayforge_worker::config::Config;
use relayforge_worker::crypto;
use relayforge_worker::engine::DeliveryEngine;
use relayforge_worker::metrics::Metrics;
use relayforge_worker::policy::AttemptOutcome;
use relayforge_worker::store::{self, CompleteAttemptInput};

const TEST_KEK: [u8; 32] = [0x41u8; 32];
const TEST_SECRET_RAW: [u8; 32] = [0x22u8; 32];

async fn open_test_pool() -> Option<PgPool> {
    let url = std::env::var("RELAYFORGE_TEST_DATABASE_URL").unwrap_or_else(|_| {
        "postgres://relayforge:relayforge_local_only@postgres:5432/relayforge?sslmode=disable"
            .to_string()
    });
    PgPoolOptions::new()
        .max_connections(16)
        .acquire_timeout(Duration::from_secs(5))
        .connect(&url)
        .await
        .ok()
}

async fn reset_db(pool: &PgPool) {
    sqlx::query(
        r#"
        TRUNCATE audit_logs, delivery_attempts, deliveries,
                 idempotency_records, event_schemas, endpoint_secrets, endpoints,
                 api_credentials, rate_limit_counters, events, tenants
        RESTART IDENTITY CASCADE
        "#,
    )
    .execute(pool)
    .await
    .expect("truncate test tables");
}

async fn seed_fixture(
    pool: &PgPool,
    endpoint_url: &str,
    max_attempts: i32,
    timeout_ms: i32,
    rate_limit_per_sec: i32,
    delivery_count: usize,
) -> (Uuid, Uuid, Vec<Uuid>) {
    let slug = format!("t-{}", &Uuid::new_v4().to_string()[..8]);
    let tenant_id: Uuid = sqlx::query_scalar(
        "INSERT INTO tenants (slug, name) VALUES ($1, 'Worker Test Tenant') RETURNING id",
    )
    .bind(slug)
    .fetch_one(pool)
    .await
    .expect("insert tenant");

    // Note: endpoints table has CHECK (url ~ '^https://'). For local HTTP stub tests,
    // we temporarily drop or satisfy the check, or use https:// with a custom resolver.
    // Wait: let's check if endpoints_url_check requires https://.
    // In 0003_endpoints_and_secrets.up.sql: CHECK (char_length(url) <= 2048 AND url ~ '^https://')
    let endpoint_id: Uuid = sqlx::query_scalar(
        r#"
        INSERT INTO endpoints
            (tenant_id, name, url, max_attempts, initial_backoff_ms, max_backoff_ms,
             retry_window_seconds, timeout_ms, rate_limit_per_sec)
        VALUES ($1, 'ep', $2, $3, 1000, 60000, 3600, $4, $5)
        RETURNING id
        "#,
    )
    .bind(tenant_id)
    .bind(endpoint_url)
    .bind(max_attempts)
    .bind(timeout_ms)
    .bind(rate_limit_per_sec)
    .fetch_one(pool)
    .await
    .expect("insert endpoint");

    let secret_id = Uuid::now_v7();
    let whsec = format!("whsec_{}", BASE64_STD.encode(TEST_SECRET_RAW));
    let aad = crypto::canonical_aad(tenant_id, endpoint_id, secret_id, 1);
    let cipher = <Aes256Gcm as KeyInit>::new_from_slice(&TEST_KEK).unwrap();
    let nonce = [0x09u8; 12];
    let ciphertext = cipher
        .encrypt(
            &Nonce::from(nonce),
            Payload {
                msg: whsec.as_bytes(),
                aad: aad.as_bytes(),
            },
        )
        .unwrap();

    sqlx::query(
        r#"
        INSERT INTO endpoint_secrets
            (id, tenant_id, endpoint_id, ciphertext, nonce, key_version, secret_hint)
        VALUES ($1, $2, $3, $4, $5, 1, 'test')
        "#,
    )
    .bind(secret_id)
    .bind(tenant_id)
    .bind(endpoint_id)
    .bind(ciphertext)
    .bind(&nonce[..])
    .execute(pool)
    .await
    .expect("insert endpoint secret");

    let mut delivery_ids = Vec::with_capacity(delivery_count);
    let wire_body = br#"{"id":"evt_1","type":"order.created","data":{"amount":4200}}"#;
    let payload_sha256 = [0x11u8; 32];

    for _ in 0..delivery_count {
        let event_id: Uuid = sqlx::query_scalar(
            r#"
            INSERT INTO events
                (tenant_id, type, schema_version, occurred_at, wire_body, payload,
                 payload_bytes, payload_sha256, status, delivery_count)
            VALUES ($1, 'order.created', 1, now(), $2, '{"amount":4200}'::jsonb,
                    $3, $4, 'accepted', 1)
            RETURNING id
            "#,
        )
        .bind(tenant_id)
        .bind(&wire_body[..])
        .bind(wire_body.len() as i32)
        .bind(&payload_sha256[..])
        .fetch_one(pool)
        .await
        .expect("insert event");

        let deliv_id: Uuid = sqlx::query_scalar(
            r#"
            INSERT INTO deliveries
                (tenant_id, event_id, endpoint_id, max_attempts, initial_backoff_ms,
                 max_backoff_ms, retry_window_seconds, timeout_ms)
            VALUES ($1, $2, $3, $4, 1000, 60000, 3600, $5)
            RETURNING id
            "#,
        )
        .bind(tenant_id)
        .bind(event_id)
        .bind(endpoint_id)
        .bind(max_attempts)
        .bind(timeout_ms)
        .fetch_one(pool)
        .await
        .expect("insert delivery");

        delivery_ids.push(deliv_id);
    }

    (tenant_id, endpoint_id, delivery_ids)
}

fn test_config(allow_insecure_local: bool) -> Config {
    Config {
        database_url: "postgres://relayforge:relayforge@localhost:5432/relayforge".into(),
        worker_id: "worker-it".into(),
        health_addr: "127.0.0.1:0".into(),
        poll_interval: Duration::from_millis(100),
        concurrency: 8,
        claim_batch: 16,
        lease_seconds: 90,
        reap_interval: Duration::from_secs(15),
        reap_batch: 100,
        expected_schema_version: 5,
        shutdown_grace: Duration::from_secs(10),
        log_level: "info".into(),
        kek: TEST_KEK,
        allowed_ports: vec![443],
        allow_insecure_local,
    }
}

#[tokio::test]
async fn integration_suite() {
    let Some(pool) = open_test_pool().await else {
        eprintln!("skipping delivery_integration: database not reachable");
        return;
    };

    // Acquire advisory lock on a dedicated connection so parallel test processes don't collide.
    let mut lock_conn = pool.acquire().await.expect("acquire lock conn");
    sqlx::query("SELECT pg_advisory_lock(918273645)")
        .execute(&mut *lock_conn)
        .await
        .expect("acquire advisory lock");

    // 1. Concurrent claim safety: 50 due rows claimed across 8 concurrent tasks -> 0 duplicates.
    reset_db(&pool).await;
    let (_, _, seeded) = seed_fixture(&pool, "https://example.com/hook", 8, 5000, 100, 50).await;
    assert_eq!(seeded.len(), 50);

    let mut handles = Vec::new();
    for idx in 0..8 {
        let p = pool.clone();
        handles.push(tokio::spawn(async move {
            let mut my_ids = Vec::new();
            let wid = format!("w-{idx}");
            loop {
                let batch = store::claim_due_deliveries(&p, &wid, 7, 90)
                    .await
                    .expect("claim batch");
                if batch.is_empty() {
                    break;
                }
                my_ids.extend(batch.into_iter().map(|c| c.id));
            }
            my_ids
        }));
    }

    let mut all_claimed = Vec::new();
    for h in handles {
        all_claimed.extend(h.await.expect("join claimer"));
    }
    assert_eq!(all_claimed.len(), 50, "must claim all 50 deliveries");
    let unique: HashSet<Uuid> = all_claimed.iter().copied().collect();
    assert_eq!(unique.len(), 50, "concurrent claims must not overlap");

    let attempt_rows: i64 = sqlx::query_scalar("SELECT count(*) FROM delivery_attempts")
        .fetch_one(&pool)
        .await
        .unwrap();
    assert_eq!(
        attempt_rows, 50,
        "each claim must insert exactly 1 attempt row"
    );

    // 2. Stale completion fencing: completing with an old lease_token affects 0 rows and writes no outcome.
    reset_db(&pool).await;
    let (_, endpoint_id, deliv_ids) =
        seed_fixture(&pool, "https://example.com/hook", 8, 5000, 100, 1).await;
    let claimed = store::claim_due_deliveries(&pool, "w-1", 1, 90)
        .await
        .unwrap()
        .pop()
        .unwrap();
    let stale_token = Uuid::new_v4();
    let committed = store::complete_attempt(
        &pool,
        CompleteAttemptInput {
            delivery_id: claimed.id,
            event_id: claimed.event_id,
            endpoint_id,
            lease_token: stale_token,
            cycle: claimed.cycle,
            attempt_no: claimed.attempt_seq,
            next_status: "delivered",
            next_attempt_at: None,
            outcome: AttemptOutcome::Success,
            http_status: Some(200),
            error_code: None,
            error_detail: None,
            duration_ms: 15,
        },
    )
    .await
    .expect("stale completion query");
    assert!(!committed, "stale lease token must be rejected");

    let (status, outcome): (String, Option<String>) = sqlx::query_as(
        r#"
        SELECT d.status, a.outcome
        FROM deliveries d
        JOIN delivery_attempts a ON a.delivery_id = d.id
        WHERE d.id = $1
        "#,
    )
    .bind(deliv_ids[0])
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(status, "in_progress");
    assert!(
        outcome.is_none(),
        "stale completion must not write attempt outcome"
    );

    // 3. Expired-lease reaper + missing-attempt reconciliation.
    reset_db(&pool).await;
    let (_, _, reaper_ids) = seed_fixture(&pool, "https://example.com/hook", 2, 5000, 100, 2).await;
    let claimed_reap = store::claim_due_deliveries(&pool, "w-dead", 2, 90)
        .await
        .unwrap();
    assert_eq!(claimed_reap.len(), 2);

    // Exhaust attempts on the second row and delete its attempt row to test reconciliation.
    sqlx::query(
        "UPDATE deliveries SET created_at = now() - interval '100 seconds', lease_expires_at = now() - interval '10 seconds' WHERE id = $1",
    )
    .bind(claimed_reap[0].id)
    .execute(&pool)
    .await
    .unwrap();

    sqlx::query(
        "UPDATE deliveries SET created_at = now() - interval '100 seconds', attempt_count = 2, attempt_seq = 2, lease_expires_at = now() - interval '10 seconds' WHERE id = $1",
    )
    .bind(claimed_reap[1].id)
    .execute(&pool)
    .await
    .unwrap();
    sqlx::query("DELETE FROM delivery_attempts WHERE delivery_id = $1")
        .bind(claimed_reap[1].id)
        .execute(&pool)
        .await
        .unwrap();

    let recovered = store::reap_expired_leases(&pool, 100)
        .await
        .expect("reap expired leases");
    assert_eq!(recovered, 2);

    let (st0, code0): (String, Option<String>) =
        sqlx::query_as("SELECT status, last_error_code FROM deliveries WHERE id = $1")
            .bind(reaper_ids[0])
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(st0, "retry_scheduled");
    assert_eq!(code0.as_deref(), Some("lease_expired"));

    let (st1, ev_st1): (String, String) = sqlx::query_as(
        "SELECT d.status, e.status FROM deliveries d JOIN events e ON e.id = d.event_id WHERE d.id = $1",
    )
    .bind(reaper_ids[1])
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(st1, "dead_lettered");
    assert_eq!(ev_st1, "failed");

    let reconciled_outcome: String = sqlx::query_scalar(
        "SELECT outcome FROM delivery_attempts WHERE delivery_id = $1 AND attempt_no = 2",
    )
    .bind(reaper_ids[1])
    .fetch_one(&pool)
    .await
    .expect("reconciled missing attempt row");
    assert_eq!(reconciled_outcome, "abandoned");

    // 4. Rate-limit deferral: does not consume attempt_count, advances attempt_seq, records rate_limited.
    reset_db(&pool).await;
    let (_, ep_rl, rl_ids) = seed_fixture(&pool, "https://example.com/hook", 3, 5000, 1, 2).await;
    let rl_claimed = store::claim_due_deliveries(&pool, "w-rl", 2, 90)
        .await
        .unwrap();
    assert_eq!(rl_claimed.len(), 2);

    assert!(
        store::acquire_destination_rate_limit(&pool, ep_rl, 1)
            .await
            .unwrap()
    );
    assert!(
        !store::acquire_destination_rate_limit(&pool, ep_rl, 1)
            .await
            .unwrap()
    );

    let deferred = store::defer_rate_limited(
        &pool,
        rl_claimed[1].id,
        rl_claimed[1].lease_token,
        rl_claimed[1].cycle,
        rl_claimed[1].attempt_seq,
    )
    .await
    .unwrap();
    assert!(deferred);

    let (rl_status, rl_attempts, rl_seq): (String, i32, i32) =
        sqlx::query_as("SELECT status, attempt_count, attempt_seq FROM deliveries WHERE id = $1")
            .bind(rl_ids[1])
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(rl_status, "retry_scheduled");
    assert_eq!(
        rl_attempts, 0,
        "rate-limit deferral must not consume an attempt"
    );
    assert_eq!(rl_seq, 1, "attempt_seq must remain monotonic");

    let rl_attempt_outcome: String =
        sqlx::query_scalar("SELECT outcome FROM delivery_attempts WHERE delivery_id = $1")
            .bind(rl_ids[1])
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(rl_attempt_outcome, "rate_limited");

    // 5. Graceful shutdown before HTTP send releases lease and refunds attempt_count.
    reset_db(&pool).await;
    let (_, _, sd_ids) = seed_fixture(&pool, "https://example.com/hook", 3, 5000, 10, 1).await;
    let sd_claimed = store::claim_due_deliveries(&pool, "w-sd", 1, 90)
        .await
        .unwrap()
        .pop()
        .unwrap();
    let metrics = Metrics::new("test");
    let engine = DeliveryEngine::new(pool.clone(), test_config(false), metrics).unwrap();
    engine.mark_shutting_down();
    let (_tx, rx) = watch::channel(true);
    engine
        .execute_claimed_delivery(sd_claimed, rx)
        .await
        .expect("execute under shutdown");

    let (sd_status, sd_attempts, sd_seq): (String, i32, i32) =
        sqlx::query_as("SELECT status, attempt_count, attempt_seq FROM deliveries WHERE id = $1")
            .bind(sd_ids[0])
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(sd_status, "retry_scheduled");
    assert_eq!(
        sd_attempts, 0,
        "shutdown before send must refund attempt_count"
    );
    assert_eq!(sd_seq, 1);

    // 6. End-to-end HTTP delivery against local stub receiver (HMAC verification, 302 redirect
    //    not followed, 429 Retry-After honoured, slow receiver timeout with 0 open DB tx during HTTP).
    #[derive(Clone)]
    struct StubState {
        pool: PgPool,
        saw_open_tx_during_slow: Arc<AtomicBool>,
    }

    let saw_open_tx = Arc::new(AtomicBool::new(false));
    let stub_state = StubState {
        pool: pool.clone(),
        saw_open_tx_during_slow: Arc::clone(&saw_open_tx),
    };

    let stub_app = Router::new()
        .route(
            "/ok",
            post(|headers: HeaderMap, body: Bytes| async move {
                let msg_id = headers
                    .get("webhook-id")
                    .and_then(|v| v.to_str().ok())
                    .unwrap_or("");
                let ts: i64 = headers
                    .get("webhook-timestamp")
                    .and_then(|v| v.to_str().ok())
                    .and_then(|v| v.parse().ok())
                    .unwrap_or(0);
                let sig = headers
                    .get("webhook-signature")
                    .and_then(|v| v.to_str().ok())
                    .unwrap_or("");
                let expected = crypto::sign_v1(&TEST_SECRET_RAW, msg_id, ts, &body).unwrap();
                if sig != expected || (Utc::now().timestamp() - ts).abs() > 300 {
                    return StatusCode::UNAUTHORIZED.into_response();
                }
                StatusCode::NO_CONTENT.into_response()
            }),
        )
        .route(
            "/redirect",
            post(|| async {
                (
                    StatusCode::FOUND,
                    [(header::LOCATION, "http://169.254.169.254/latest/meta-data/")],
                    "redirecting",
                )
            }),
        )
        .route(
            "/rate-limit",
            post(|| async {
                (
                    StatusCode::TOO_MANY_REQUESTS,
                    [(header::RETRY_AFTER, "5")],
                    "slow down",
                )
            }),
        )
        .route(
            "/slow",
            post(|State(st): State<StubState>| async move {
                // Allow fast sibling tasks (/ok, /redirect, /rate-limit) to finish their
                // completion transactions so we isolate the in-flight /slow HTTP request.
                tokio::time::sleep(Duration::from_millis(200)).await;
                let active_tx: i64 = sqlx::query_scalar(
                    r#"
                    SELECT count(*)
                    FROM pg_stat_activity
                    WHERE datname = current_database()
                      AND pid <> pg_backend_pid()
                      AND state IN ('idle in transaction', 'idle in transaction (aborted)')
                    "#,
                )
                .fetch_one(&st.pool)
                .await
                .unwrap_or(0);
                if active_tx > 0 {
                    st.saw_open_tx_during_slow.store(true, Ordering::SeqCst);
                }
                tokio::time::sleep(Duration::from_millis(1200)).await;
                StatusCode::OK
            }),
        )
        .with_state(stub_state);

    let stub_listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let stub_port = stub_listener.local_addr().unwrap().port();
    tokio::spawn(async move {
        axum::serve(stub_listener, stub_app).await.ok();
    });

    reset_db(&pool).await;
    let (_, _, ok_ids) = seed_fixture(
        &pool,
        &format!("https://127.0.0.1:{stub_port}/ok"),
        3,
        5000,
        50,
        1,
    )
    .await;
    let (_, _, redir_ids) = seed_fixture(
        &pool,
        &format!("https://127.0.0.1:{stub_port}/redirect"),
        3,
        5000,
        50,
        1,
    )
    .await;
    let (_, _, rl429_ids) = seed_fixture(
        &pool,
        &format!("https://127.0.0.1:{stub_port}/rate-limit"),
        3,
        5000,
        50,
        1,
    )
    .await;
    let (_, _, slow_ids) = seed_fixture(
        &pool,
        &format!("https://127.0.0.1:{stub_port}/slow"),
        3,
        1000,
        50,
        1,
    )
    .await;

    let local_engine =
        DeliveryEngine::new(pool.clone(), test_config(true), Metrics::new("test")).unwrap();
    let (_tx2, rx2) = watch::channel(false);
    let processed = local_engine.claim_and_process_once(10, &rx2).await.unwrap();
    assert_eq!(processed, 4);

    let (ok_st, ok_ev_st): (String, String) = sqlx::query_as(
        "SELECT d.status, e.status FROM deliveries d JOIN events e ON e.id = d.event_id WHERE d.id = $1",
    )
    .bind(ok_ids[0])
    .fetch_one(&pool)
    .await
    .unwrap();
    assert_eq!(ok_st, "delivered");
    assert_eq!(ok_ev_st, "delivered");

    let (redir_st, redir_code): (String, Option<String>) =
        sqlx::query_as("SELECT status, last_error_code FROM deliveries WHERE id = $1")
            .bind(redir_ids[0])
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(redir_st, "failed");
    assert_eq!(redir_code.as_deref(), Some("http_redirect"));

    let (rl429_st, rl429_code): (String, Option<String>) =
        sqlx::query_as("SELECT status, last_error_code FROM deliveries WHERE id = $1")
            .bind(rl429_ids[0])
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(rl429_st, "retry_scheduled");
    assert_eq!(rl429_code.as_deref(), Some("http_429"));

    let (slow_st, slow_code): (String, Option<String>) =
        sqlx::query_as("SELECT status, last_error_code FROM deliveries WHERE id = $1")
            .bind(slow_ids[0])
            .fetch_one(&pool)
            .await
            .unwrap();
    assert_eq!(slow_st, "retry_scheduled");
    assert_eq!(slow_code.as_deref(), Some("total_timeout"));
    assert!(
        !saw_open_tx.load(Ordering::SeqCst),
        "worker must never hold an open PostgreSQL transaction during outbound HTTP"
    );

    reset_db(&pool).await;
}
