use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, Instant};

use anyhow::{Context, Result};
use chrono::Utc;
use futures_util::StreamExt;
use reqwest::header::{CONTENT_TYPE, HeaderMap, HeaderName, HeaderValue, USER_AGENT};
use sqlx::PgPool;
use tokio::sync::{Semaphore, watch};
use tokio::task::JoinSet;
use tracing::{info, warn};

use crate::config::Config;
use crate::crypto;
use crate::metrics::Metrics;
use crate::policy::{self, AttemptOutcome, RetryDecision};
use crate::ssrf::{
    DnsResolver, PinnedDnsResolver, SystemDnsResolver, ValidationDecision, ValidationStage,
    validate_destination,
};
use crate::store::{self, ClaimedDelivery, CompleteAttemptInput};

const MAX_RESPONSE_BODY_BYTES: usize = 8 * 1024;

#[derive(Clone)]
pub struct DeliveryEngine {
    pub pool: PgPool,
    pub cfg: Config,
    pub metrics: Metrics,
    pub dns_resolver: Arc<dyn DnsResolver>,
    pub pinned_dns: PinnedDnsResolver,
    pub client: reqwest::Client,
    pub local_client: reqwest::Client,
    pub shutting_down: Arc<AtomicBool>,
}

impl DeliveryEngine {
    pub fn new(pool: PgPool, cfg: Config, metrics: Metrics) -> Result<Self> {
        Self::with_resolver(pool, cfg, metrics, Arc::new(SystemDnsResolver::new()))
    }

    pub fn with_resolver(
        pool: PgPool,
        cfg: Config,
        metrics: Metrics,
        dns_resolver: Arc<dyn DnsResolver>,
    ) -> Result<Self> {
        let pinned_dns = PinnedDnsResolver::new();
        let client = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .no_proxy()
            .connect_timeout(Duration::from_secs(3))
            .read_timeout(Duration::from_secs(10))
            .dns_resolver(Arc::new(pinned_dns.clone()))
            .build()
            .context("build SSRF-pinned reqwest client")?;

        let local_client = reqwest::Client::builder()
            .redirect(reqwest::redirect::Policy::none())
            .no_proxy()
            .connect_timeout(Duration::from_secs(3))
            .read_timeout(Duration::from_secs(10))
            .build()
            .context("build local reqwest client")?;

        Ok(Self {
            pool,
            cfg,
            metrics,
            dns_resolver,
            pinned_dns,
            client,
            local_client,
            shutting_down: Arc::new(AtomicBool::new(false)),
        })
    }

    pub fn is_shutting_down(&self) -> bool {
        self.shutting_down.load(Ordering::SeqCst)
    }

    pub fn mark_shutting_down(&self) {
        self.shutting_down.store(true, Ordering::SeqCst);
        self.metrics.ready.set(0);
    }

    /// Runs a single reaper pass and updates `relayforge_lease_recoveries_total`.
    pub async fn run_reaper_once(&self) -> Result<u64> {
        let recovered = store::reap_expired_leases(&self.pool, self.cfg.reap_batch).await?;
        if recovered > 0 {
            self.metrics.lease_recoveries_total.inc_by(recovered);
            info!(recovered, "reaped expired delivery leases");
        }
        Ok(recovered)
    }

    /// Claims a single batch of due deliveries and executes them concurrently.
    /// Useful both in the main worker loop and in deterministic integration tests.
    pub async fn claim_and_process_once(
        &self,
        max_claim: i64,
        shutdown_rx: &watch::Receiver<bool>,
    ) -> Result<usize> {
        if self.is_shutting_down() || *shutdown_rx.borrow() {
            return Ok(0);
        }
        let claimed = store::claim_due_deliveries(
            &self.pool,
            &self.cfg.worker_id,
            max_claim,
            self.cfg.lease_seconds,
        )
        .await?;
        if claimed.is_empty() {
            return Ok(0);
        }

        let count = claimed.len();
        let mut set = JoinSet::new();
        for item in claimed {
            let engine = self.clone();
            let rx = shutdown_rx.clone();
            set.spawn(async move {
                if let Err(err) = engine.execute_claimed_delivery(item, rx).await {
                    warn!(error = %err, "delivery execution error");
                }
            });
        }
        while set.join_next().await.is_some() {}
        Ok(count)
    }

    /// Executes one claimed delivery attempt from rate-limit check through HTTP send
    /// and lease-fenced completion (`contracts/delivery-contract-v1.md` §4–§8).
    pub async fn execute_claimed_delivery(
        &self,
        claimed: ClaimedDelivery,
        shutdown_rx: watch::Receiver<bool>,
    ) -> Result<()> {
        if self.is_shutting_down() || *shutdown_rx.borrow() {
            store::release_unstarted_claim(
                &self.pool,
                claimed.id,
                claimed.lease_token,
                claimed.cycle,
                claimed.attempt_seq,
            )
            .await?;
            return Ok(());
        }

        let ctx =
            store::load_delivery_context(&self.pool, claimed.event_id, claimed.endpoint_id).await?;

        if ctx.endpoint_deleted || ctx.endpoint_status != "active" {
            self.finish_attempt(
                &claimed,
                "canceled",
                None,
                AttemptOutcome::Canceled,
                None,
                Some("endpoint_inactive"),
                Some("endpoint is paused, disabled, or deleted".to_string()),
                0,
            )
            .await?;
            return Ok(());
        }

        // Destination rate-limit check (`rate_limit_counters`).
        let admitted = store::acquire_destination_rate_limit(
            &self.pool,
            claimed.endpoint_id,
            ctx.rate_limit_per_sec,
        )
        .await?;
        if !admitted {
            self.metrics
                .rate_limit_denied_total
                .with_label_values(&["destination"])
                .inc();
            store::defer_rate_limited(
                &self.pool,
                claimed.id,
                claimed.lease_token,
                claimed.cycle,
                claimed.attempt_seq,
            )
            .await?;
            return Ok(());
        }

        if self.is_shutting_down() || *shutdown_rx.borrow() {
            store::release_unstarted_claim(
                &self.pool,
                claimed.id,
                claimed.lease_token,
                claimed.cycle,
                claimed.attempt_seq,
            )
            .await?;
            return Ok(());
        }

        // Active signing secrets check.
        if ctx.secrets.is_empty() {
            self.finish_attempt(
                &claimed,
                "failed",
                None,
                AttemptOutcome::PermanentFailure,
                None,
                Some("no_active_secret"),
                Some("endpoint has no active signing secret".to_string()),
                0,
            )
            .await?;
            return Ok(());
        }

        let mut secret_keys = Vec::with_capacity(ctx.secrets.len());
        for s in &ctx.secrets {
            match crypto::decrypt_endpoint_secret(
                &self.cfg.kek,
                &s.ciphertext,
                &s.nonce,
                s.tenant_id,
                s.endpoint_id,
                s.id,
                s.key_version,
            ) {
                Ok(k) => secret_keys.push(k),
                Err(err) => {
                    warn!(
                        delivery_id = %claimed.id,
                        secret_id = %s.id,
                        error = %err,
                        "failed to decrypt endpoint signing secret"
                    );
                    self.finish_retryable(
                        &claimed,
                        AttemptOutcome::RetryableFailure,
                        None,
                        "internal_error",
                        "failed to decrypt endpoint signing secret".to_string(),
                        None,
                        0,
                    )
                    .await?;
                    return Ok(());
                }
            }
        }

        let timestamp_secs = Utc::now().timestamp();
        let webhook_id = claimed.event_id.to_string();
        let signature_header = match crypto::sign_webhook_header(
            &secret_keys,
            &webhook_id,
            timestamp_secs,
            &ctx.wire_body,
        ) {
            Ok(h) => h,
            Err(err) => {
                warn!(delivery_id = %claimed.id, error = %err, "failed to construct HMAC signature");
                self.finish_retryable(
                    &claimed,
                    AttemptOutcome::RetryableFailure,
                    None,
                    "internal_error",
                    "failed to construct webhook signature".to_string(),
                    None,
                    0,
                )
                .await?;
                return Ok(());
            }
        };

        // SSRF connect-time enforcement (`contracts/ssrf-policy-v1.md`).
        let use_local_client = self.cfg.allow_insecure_local
            && (ctx.endpoint_url.starts_with("http://127.0.0.1:")
                || ctx.endpoint_url.starts_with("http://localhost:")
                || ctx.endpoint_url.starts_with("http://receiver:")
                || ctx.endpoint_url.starts_with("https://127.0.0.1:")
                || ctx.endpoint_url.starts_with("https://localhost:")
                || ctx.endpoint_url.starts_with("https://receiver:"));

        if !use_local_client {
            match validate_destination(
                &ctx.endpoint_url,
                ValidationStage::Delivery,
                &self.cfg.allowed_ports,
                self.dns_resolver.as_ref(),
            )
            .await
            {
                ValidationDecision::Allow { host, addrs, .. } => {
                    self.pinned_dns.set_host_addrs(&host, addrs);
                }
                ValidationDecision::Deny { reason } => {
                    self.metrics
                        .ssrf_blocks_total
                        .with_label_values(&[reason])
                        .inc();
                    let code = if reason == "url_invalid" || reason == "host_missing" {
                        "invalid_destination"
                    } else {
                        "destination_blocked"
                    };
                    self.finish_attempt(
                        &claimed,
                        "failed",
                        None,
                        AttemptOutcome::Blocked,
                        None,
                        Some(code),
                        Some(format!("destination rejected by SSRF policy: {reason}")),
                        0,
                    )
                    .await?;
                    return Ok(());
                }
                ValidationDecision::Retry { reason } => {
                    self.finish_retryable(
                        &claimed,
                        AttemptOutcome::RetryableFailure,
                        None,
                        "dns_error",
                        format!("DNS resolution failed: {reason}"),
                        None,
                        0,
                    )
                    .await?;
                    return Ok(());
                }
            }
        }

        // Last check before starting outbound HTTP.
        if self.is_shutting_down() || *shutdown_rx.borrow() {
            store::release_unstarted_claim(
                &self.pool,
                claimed.id,
                claimed.lease_token,
                claimed.cycle,
                claimed.attempt_seq,
            )
            .await?;
            return Ok(());
        }

        let mut headers = HeaderMap::new();
        headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
        headers.insert(USER_AGENT, HeaderValue::from_static("RelayForge/1"));
        if let Ok(v) = HeaderValue::from_str(&webhook_id) {
            headers.insert(HeaderName::from_static("webhook-id"), v);
        }
        if let Ok(v) = HeaderValue::from_str(&timestamp_secs.to_string()) {
            headers.insert(HeaderName::from_static("webhook-timestamp"), v);
        }
        if let Ok(v) = HeaderValue::from_str(&signature_header) {
            headers.insert(HeaderName::from_static("webhook-signature"), v);
        }
        if let Ok(v) = HeaderValue::from_str(&claimed.id.to_string()) {
            headers.insert(HeaderName::from_static("relayforge-delivery"), v);
        }
        if let Ok(v) = HeaderValue::from_str(&claimed.attempt_count.to_string()) {
            headers.insert(HeaderName::from_static("relayforge-attempt"), v);
        }
        if let Ok(v) = HeaderValue::from_str(&ctx.event_type) {
            headers.insert(HeaderName::from_static("relayforge-event-type"), v);
        }
        if let Some(tp) = ctx.trace_parent.as_deref()
            && !tp.is_empty()
            && let Ok(v) = HeaderValue::from_str(tp)
        {
            headers.insert(HeaderName::from_static("traceparent"), v);
        }

        let total_timeout = Duration::from_millis(claimed.timeout_ms.clamp(1_000, 30_000) as u64);
        let (client, target_url) = if use_local_client {
            let rewritten = if let Some(rest) = ctx.endpoint_url.strip_prefix("https://") {
                format!("http://{rest}")
            } else {
                ctx.endpoint_url.clone()
            };
            (&self.local_client, rewritten)
        } else {
            (&self.client, ctx.endpoint_url.clone())
        };

        let started = Instant::now();
        let http_res = tokio::time::timeout(total_timeout, async {
            let resp = client
                .post(&target_url)
                .headers(headers)
                .body(ctx.wire_body)
                .send()
                .await?;

            let status = resp.status().as_u16();
            let retry_after = resp
                .headers()
                .get("retry-after")
                .and_then(|v| v.to_str().ok())
                .map(str::to_string);

            // Stream and discard up to 8 KiB (`contracts/delivery-contract-v1.md` §8).
            let mut stream = resp.bytes_stream();
            let mut read_bytes = 0usize;
            while let Some(chunk_res) = stream.next().await {
                let chunk = chunk_res?;
                read_bytes = read_bytes.saturating_add(chunk.len());
                if read_bytes >= MAX_RESPONSE_BODY_BYTES {
                    break;
                }
            }
            Ok::<(u16, Option<String>), reqwest::Error>((status, retry_after))
        })
        .await;

        let duration_ms = started.elapsed().as_millis().min(i32::MAX as u128) as i32;

        match http_res {
            Ok(Ok((status_u16, retry_after_raw))) => {
                let classification = policy::classify_http_status(status_u16);
                let http_status = Some(status_u16 as i32);

                match classification.outcome {
                    AttemptOutcome::Success => {
                        self.finish_attempt(
                            &claimed,
                            "delivered",
                            None,
                            AttemptOutcome::Success,
                            http_status,
                            None,
                            None,
                            duration_ms,
                        )
                        .await?;
                    }
                    AttemptOutcome::PermanentFailure => {
                        let code = classification.error_code.unwrap_or("http_4xx");
                        self.finish_attempt(
                            &claimed,
                            "failed",
                            None,
                            AttemptOutcome::PermanentFailure,
                            http_status,
                            Some(code),
                            Some(format!("HTTP {status_u16}")),
                            duration_ms,
                        )
                        .await?;
                    }
                    _ => {
                        let code = classification.error_code.unwrap_or("http_5xx");
                        let retry_after_ms = if status_u16 == 429 {
                            retry_after_raw
                                .as_deref()
                                .and_then(|raw| policy::parse_retry_after_ms(raw, Utc::now()))
                        } else {
                            None
                        };
                        self.finish_retryable(
                            &claimed,
                            AttemptOutcome::RetryableFailure,
                            http_status,
                            code,
                            format!("HTTP {status_u16}"),
                            retry_after_ms,
                            duration_ms,
                        )
                        .await?;
                    }
                }
            }
            Ok(Err(req_err)) => {
                let (outcome, code, retryable) = classify_reqwest_error(&req_err);
                if retryable {
                    self.finish_retryable(
                        &claimed,
                        outcome,
                        None,
                        code,
                        req_err.to_string(),
                        None,
                        duration_ms,
                    )
                    .await?;
                } else {
                    self.finish_attempt(
                        &claimed,
                        "failed",
                        None,
                        outcome,
                        None,
                        Some(code),
                        Some(req_err.to_string()),
                        duration_ms,
                    )
                    .await?;
                }
            }
            Err(_) => {
                self.finish_retryable(
                    &claimed,
                    AttemptOutcome::Timeout,
                    None,
                    "total_timeout",
                    format!(
                        "attempt exceeded total timeout of {} ms",
                        claimed.timeout_ms
                    ),
                    None,
                    duration_ms,
                )
                .await?;
            }
        }

        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    async fn finish_retryable(
        &self,
        claimed: &ClaimedDelivery,
        outcome: AttemptOutcome,
        http_status: Option<i32>,
        error_code: &'static str,
        error_detail: String,
        retry_after_ms: Option<u64>,
        duration_ms: i32,
    ) -> Result<()> {
        let now = Utc::now();
        let delay_ms = policy::sample_backoff_ms(
            claimed.attempt_count,
            claimed.initial_backoff_ms,
            claimed.max_backoff_ms,
            retry_after_ms,
        );
        let decision = policy::decide_retry(
            now,
            claimed.cycle_started_at,
            claimed.attempt_count,
            claimed.max_attempts,
            claimed.retry_window_seconds,
            delay_ms,
        );

        let (next_status, next_attempt_at) = match decision {
            RetryDecision::RetryScheduled { next_attempt_at } => {
                ("retry_scheduled", Some(next_attempt_at))
            }
            RetryDecision::DeadLettered => ("dead_lettered", None),
        };

        self.finish_attempt(
            claimed,
            next_status,
            next_attempt_at,
            outcome,
            http_status,
            Some(error_code),
            Some(error_detail),
            duration_ms,
        )
        .await
    }

    #[allow(clippy::too_many_arguments)]
    async fn finish_attempt(
        &self,
        claimed: &ClaimedDelivery,
        next_status: &'static str,
        next_attempt_at: Option<chrono::DateTime<Utc>>,
        outcome: AttemptOutcome,
        http_status: Option<i32>,
        error_code: Option<&'static str>,
        error_detail: Option<String>,
        duration_ms: i32,
    ) -> Result<()> {
        let committed = store::complete_attempt(
            &self.pool,
            CompleteAttemptInput {
                delivery_id: claimed.id,
                event_id: claimed.event_id,
                endpoint_id: claimed.endpoint_id,
                lease_token: claimed.lease_token,
                cycle: claimed.cycle,
                attempt_no: claimed.attempt_seq,
                next_status,
                next_attempt_at,
                outcome,
                http_status,
                error_code,
                error_detail,
                duration_ms,
            },
        )
        .await?;

        if !committed {
            warn!(
                delivery_id = %claimed.id,
                event_id = %claimed.event_id,
                cycle = claimed.cycle,
                attempt_no = claimed.attempt_seq,
                "lease_lost"
            );
            return Ok(());
        }

        let outcome_str = outcome.as_str();
        let code_str = error_code.unwrap_or("none");
        self.metrics
            .delivery_attempts_total
            .with_label_values(&[outcome_str, code_str])
            .inc();
        self.metrics
            .deliveries_total
            .with_label_values(&[next_status])
            .inc();
        self.metrics
            .delivery_duration_seconds
            .with_label_values(&[outcome_str])
            .observe((duration_ms.max(0) as f64) / 1000.0);

        info!(
            tenant_id = %claimed.tenant_id,
            event_id = %claimed.event_id,
            delivery_id = %claimed.id,
            cycle = claimed.cycle,
            attempt_no = claimed.attempt_seq,
            outcome = outcome_str,
            next_status,
            error_code = code_str,
            duration_ms,
            "delivery attempt completed"
        );
        Ok(())
    }
}

fn classify_reqwest_error(err: &reqwest::Error) -> (AttemptOutcome, &'static str, bool) {
    let msg = err.to_string().to_ascii_lowercase();
    if msg.contains("certificate")
        || msg.contains("tls")
        || msg.contains("handshake")
        || msg.contains("webpki")
        || msg.contains("invalid peer certificate")
    {
        return (AttemptOutcome::PermanentFailure, "tls_error", false);
    }
    if err.is_timeout() {
        if err.is_connect() {
            return (AttemptOutcome::Timeout, "connect_timeout", true);
        }
        return (AttemptOutcome::Timeout, "read_timeout", true);
    }
    if err.is_connect() {
        return (AttemptOutcome::RetryableFailure, "connect_error", true);
    }
    (AttemptOutcome::RetryableFailure, "connect_error", true)
}

/// Runs the continuous worker claim loop, reaper loop, and queue gauge sampler
/// until `shutdown` signals termination.
pub async fn run_worker_loops(engine: DeliveryEngine, mut shutdown: watch::Receiver<bool>) {
    let semaphore = Arc::new(Semaphore::new(engine.cfg.concurrency));
    let mut in_flight = JoinSet::new();

    let mut poll_ticker = tokio::time::interval(engine.cfg.poll_interval);
    poll_ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

    let mut reap_ticker = tokio::time::interval(engine.cfg.reap_interval);
    reap_ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

    loop {
        tokio::select! {
            _ = poll_ticker.tick() => {
                engine.metrics.loop_ticks.inc();

                // Reap finished task handles without blocking.
                while in_flight.try_join_next().is_some() {}

                if let Ok((due, oldest)) = store::sample_queue_stats(&engine.pool).await {
                    engine.metrics.queue_due.set(due);
                    engine.metrics.queue_oldest_age_seconds.set(oldest);
                }

                let available_slots = semaphore.available_permits() as i64;
                let batch = available_slots.min(engine.cfg.claim_batch);
                if batch <= 0 {
                    continue;
                }

                match store::claim_due_deliveries(
                    &engine.pool,
                    &engine.cfg.worker_id,
                    batch,
                    engine.cfg.lease_seconds,
                ).await {
                    Ok(claimed_list) => {
                        for item in claimed_list {
                            let Ok(permit) = Arc::clone(&semaphore).acquire_owned().await else {
                                break;
                            };
                            let eng = engine.clone();
                            let rx = shutdown.clone();
                            in_flight.spawn(async move {
                                let _permit = permit;
                                if let Err(err) = eng.execute_claimed_delivery(item, rx).await {
                                    warn!(error = %err, "delivery task failed");
                                }
                            });
                        }
                    }
                    Err(err) => {
                        warn!(error = %err, "failed to claim due deliveries");
                    }
                }
            }
            _ = reap_ticker.tick() => {
                if let Err(err) = engine.run_reaper_once().await {
                    warn!(error = %err, "expired lease reaper failed");
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    engine.mark_shutting_down();
                    info!("worker loop stopping; draining in-flight deliveries");
                    break;
                }
            }
        }
    }

    while in_flight.join_next().await.is_some() {}
}
