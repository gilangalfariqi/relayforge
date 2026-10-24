use std::io::{Read, Write};
use std::net::TcpStream;
use std::sync::Arc;
use std::time::Duration;

use anyhow::{Context, Result};
use axum::extract::State;
use axum::http::{StatusCode, header};
use axum::response::IntoResponse;
use axum::routing::get;
use axum::{Json, Router};
use serde_json::{Value, json};
use sqlx::PgPool;
use sqlx::postgres::PgPoolOptions;
use tokio::sync::watch;
use tracing::{error, info, warn};
use tracing_subscriber::EnvFilter;

use relayforge_worker::config::Config;
use relayforge_worker::engine::{DeliveryEngine, run_worker_loops};
use relayforge_worker::metrics::Metrics;

const SERVICE_VERSION: &str = env!("CARGO_PKG_VERSION");
const GIT_COMMIT: &str = match option_env!("RELAYFORGE_GIT_COMMIT") {
    Some(commit) => commit,
    None => "unknown",
};

struct AppState {
    pool: PgPool,
    expected_schema_version: i64,
    metrics: Metrics,
    engine: DeliveryEngine,
}

#[tokio::main]
async fn main() -> Result<()> {
    let args: Vec<String> = std::env::args().collect();
    if args.get(1).map(String::as_str) == Some("healthcheck") {
        let addr = args.get(2).map(String::as_str).unwrap_or("127.0.0.1:9090");
        std::process::exit(healthcheck(addr));
    }

    let cfg = Config::from_env()?;
    init_tracing(&cfg.log_level);

    info!(
        service = "relayforge-worker",
        version = SERVICE_VERSION,
        commit = GIT_COMMIT,
        worker_id = %cfg.worker_id,
        concurrency = cfg.concurrency,
        claim_batch = cfg.claim_batch,
        lease_seconds = cfg.lease_seconds,
        expected_schema_version = cfg.expected_schema_version,
        "starting"
    );

    let pool = PgPoolOptions::new()
        .max_connections(cfg.concurrency as u32 + 4)
        .acquire_timeout(Duration::from_secs(5))
        .connect(&cfg.database_url)
        .await
        .context("connect to PostgreSQL")?;

    let metrics = Metrics::new(SERVICE_VERSION);
    match schema_status(&pool).await {
        Ok((version, dirty)) => {
            metrics.schema_version.set(version);
            if dirty || version != cfg.expected_schema_version {
                warn!(
                    version,
                    expected = cfg.expected_schema_version,
                    dirty,
                    "schema is not ready; readiness will fail"
                );
            } else {
                metrics.db_up.set(1);
                metrics.ready.set(1);
                info!(version, "schema version verified");
            }
        }
        Err(err) => {
            metrics.schema_version.set(0);
            warn!(error = %err, "could not read schema version; readiness will fail");
        }
    }

    let engine = DeliveryEngine::new(pool.clone(), cfg.clone(), metrics.clone())?;

    let state = Arc::new(AppState {
        pool,
        expected_schema_version: cfg.expected_schema_version,
        metrics,
        engine: engine.clone(),
    });

    let (shutdown_tx, shutdown_rx) = watch::channel(false);
    let loop_state = Arc::clone(&state);
    let poll_interval = cfg.poll_interval;
    let worker_loop = tokio::spawn(async move {
        run_loop(loop_state, poll_interval, shutdown_rx).await;
    });

    let app = Router::new()
        .route("/healthz", get(healthz))
        .route("/readyz", get(readyz))
        .route("/metrics", get(metrics_handler))
        .with_state(Arc::clone(&state));

    let listener = tokio::net::TcpListener::bind(&cfg.health_addr)
        .await
        .with_context(|| format!("bind health listener on {}", cfg.health_addr))?;
    info!(addr = %cfg.health_addr, "health listener started");

    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal(state.engine.clone(), shutdown_tx.clone()))
        .await
        .context("serve health listener")?;

    info!("shutdown requested");
    state.engine.mark_shutting_down();
    let _ = shutdown_tx.send(true);
    if tokio::time::timeout(cfg.shutdown_grace, worker_loop)
        .await
        .is_err()
    {
        warn!("worker loop did not stop within the shutdown grace period");
    }

    state.pool.close().await;
    info!("stopped");
    Ok(())
}

fn init_tracing(level: &str) {
    let filter = EnvFilter::try_new(level).unwrap_or_else(|_| EnvFilter::new("info"));
    tracing_subscriber::fmt()
        .json()
        .with_target(true)
        .with_env_filter(filter)
        .init();
}

async fn healthz() -> impl IntoResponse {
    (StatusCode::OK, Json(json!({"status": "ok"})))
}

async fn readyz(State(state): State<Arc<AppState>>) -> (StatusCode, Json<Value>) {
    if state.engine.is_shutting_down() {
        state.metrics.ready.set(0);
        return (
            StatusCode::SERVICE_UNAVAILABLE,
            Json(json!({
                "type": "https://relayforge.example/errors/not-ready",
                "title": "Service not ready",
                "status": 503,
                "code": "shutting_down",
                "detail": "Worker is shutting down."
            })),
        );
    }

    let checked = tokio::time::timeout(
        Duration::from_secs(2),
        check_ready(&state.pool, state.expected_schema_version),
    )
    .await;

    match checked {
        Ok(Ok(version)) => {
            state.metrics.db_up.set(1);
            state.metrics.schema_version.set(version);
            state.metrics.ready.set(1);
            (
                StatusCode::OK,
                Json(json!({
                    "status": "ready",
                    "checks": {
                        "database": "ok",
                        "schema": "ok",
                        "worker_loop": "ok"
                    },
                    "version": env!("CARGO_PKG_VERSION")
                })),
            )
        }
        Ok(Err(err)) => {
            state.metrics.db_up.set(0);
            state.metrics.ready.set(0);
            warn!(error = %err, "readiness failed");
            (
                StatusCode::SERVICE_UNAVAILABLE,
                Json(json!({
                    "type": "https://relayforge.example/errors/not-ready",
                    "title": "Service not ready",
                    "status": 503,
                    "code": "not_ready",
                    "detail": "A required dependency is unavailable."
                })),
            )
        }
        Err(_) => {
            state.metrics.db_up.set(0);
            state.metrics.ready.set(0);
            warn!("readiness check timed out");
            (
                StatusCode::SERVICE_UNAVAILABLE,
                Json(json!({
                    "type": "https://relayforge.example/errors/not-ready",
                    "title": "Service not ready",
                    "status": 503,
                    "code": "not_ready",
                    "detail": "A required dependency is unavailable."
                })),
            )
        }
    }
}

async fn metrics_handler(State(state): State<Arc<AppState>>) -> impl IntoResponse {
    let (body, content_type) = state.metrics.render();
    ([(header::CONTENT_TYPE, content_type)], body)
}

async fn schema_status(pool: &PgPool) -> Result<(i64, bool), sqlx::Error> {
    sqlx::query_as::<_, (i64, bool)>(
        "SELECT version, dirty FROM schema_migrations ORDER BY version DESC LIMIT 1",
    )
    .fetch_one(pool)
    .await
}

async fn check_ready(pool: &PgPool, expected: i64) -> Result<i64, String> {
    sqlx::query_scalar::<_, i32>("SELECT 1")
        .fetch_one(pool)
        .await
        .map_err(|err| format!("ping failed: {err}"))?;
    let (version, dirty) = schema_status(pool)
        .await
        .map_err(|err| format!("schema version unreadable: {err}"))?;
    if dirty {
        return Err(format!("migration version {version} is dirty"));
    }
    if version != expected {
        return Err(format!(
            "database version {version}, worker expects {expected}"
        ));
    }
    Ok(version)
}

async fn run_loop(state: Arc<AppState>, poll: Duration, mut shutdown: watch::Receiver<bool>) {
    // Wait until schema is ready before starting claim/reaper loops, while continuing
    // to check readiness on each poll tick.
    let mut ticker = tokio::time::interval(poll);
    ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);

    loop {
        tokio::select! {
            _ = ticker.tick() => {
                match tokio::time::timeout(
                    Duration::from_secs(2),
                    check_ready(&state.pool, state.expected_schema_version),
                ).await {
                    Ok(Ok(version)) => {
                        state.metrics.db_up.set(1);
                        state.metrics.schema_version.set(version);
                        state.metrics.ready.set(1);
                        break;
                    }
                    Ok(Err(err)) => {
                        state.metrics.loop_ticks.inc();
                        state.metrics.db_up.set(0);
                        state.metrics.ready.set(0);
                        warn!(error = %err, "worker loop dependency check failed");
                    }
                    Err(_) => {
                        state.metrics.loop_ticks.inc();
                        state.metrics.db_up.set(0);
                        state.metrics.ready.set(0);
                        warn!("worker loop dependency check timed out");
                    }
                }
            }
            changed = shutdown.changed() => {
                if changed.is_err() || *shutdown.borrow() {
                    info!("worker loop stopping");
                    return;
                }
            }
        }
    }

    run_worker_loops(state.engine.clone(), shutdown).await;
    info!("worker loop stopping");
}

async fn shutdown_signal(engine: DeliveryEngine, shutdown_tx: watch::Sender<bool>) {
    let ctrl_c = async {
        if let Err(err) = tokio::signal::ctrl_c().await {
            error!(error = %err, "failed to install ctrl-c handler");
        }
    };

    #[cfg(unix)]
    let terminate = async {
        match tokio::signal::unix::signal(tokio::signal::unix::SignalKind::terminate()) {
            Ok(mut signal) => {
                signal.recv().await;
            }
            Err(err) => error!(error = %err, "failed to install SIGTERM handler"),
        }
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {},
        _ = terminate => {},
    }

    engine.mark_shutting_down();
    let _ = shutdown_tx.send(true);
}

/// Container healthcheck. The runtime image has no shell or curl.
fn healthcheck(addr: &str) -> i32 {
    let mut stream = match TcpStream::connect(addr) {
        Ok(stream) => stream,
        Err(err) => {
            eprintln!("healthcheck: connect {addr}: {err}");
            return 1;
        }
    };
    let _ = stream.set_read_timeout(Some(Duration::from_secs(3)));
    let request = b"GET /healthz HTTP/1.1\r\nHost: localhost\r\nConnection: close\r\n\r\n";
    if let Err(err) = stream.write_all(request) {
        eprintln!("healthcheck: write: {err}");
        return 1;
    }
    let mut response = String::new();
    if let Err(err) = stream.read_to_string(&mut response) {
        eprintln!("healthcheck: read: {err}");
        return 1;
    }
    if response.starts_with("HTTP/1.1 200") {
        0
    } else {
        eprintln!(
            "healthcheck: unexpected response: {}",
            response.lines().next().unwrap_or("<empty>")
        );
        1
    }
}
