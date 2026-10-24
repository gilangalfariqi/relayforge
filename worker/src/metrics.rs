use prometheus::{
    Encoder, HistogramOpts, HistogramVec, IntCounter, IntCounterVec, IntGauge, IntGaugeVec, Opts,
    Registry, TextEncoder,
};

/// Worker metrics matching `docs/01-architecture.md` §11.
#[derive(Clone)]
pub struct Metrics {
    registry: Registry,
    pub loop_ticks: IntCounter,
    pub db_up: IntGauge,
    pub schema_version: IntGauge,
    pub ready: IntGauge,
    pub deliveries_total: IntCounterVec,
    pub delivery_attempts_total: IntCounterVec,
    pub delivery_duration_seconds: HistogramVec,
    pub queue_due: IntGauge,
    pub queue_oldest_age_seconds: IntGauge,
    pub lease_recoveries_total: IntCounter,
    pub rate_limit_denied_total: IntCounterVec,
    pub ssrf_blocks_total: IntCounterVec,
}

impl Metrics {
    pub fn new(version: &str) -> Self {
        let registry = Registry::new();

        let build = IntGaugeVec::new(
            Opts::new(
                "relayforge_worker_build_info",
                "Worker build identity; value is always 1.",
            ),
            &["version"],
        )
        .expect("build info metric");
        build.with_label_values(&[version]).set(1);

        let loop_ticks = IntCounter::new(
            "relayforge_worker_loop_ticks_total",
            "Completed readiness-loop ticks.",
        )
        .expect("loop ticks metric");
        let db_up = IntGauge::new(
            "relayforge_worker_db_up",
            "1 when the last database check succeeded.",
        )
        .expect("db up metric");
        let schema_version = IntGauge::new(
            "relayforge_worker_schema_version",
            "Schema version observed in the database, or 0 when unreadable.",
        )
        .expect("schema version metric");
        let ready = IntGauge::new(
            "relayforge_worker_ready",
            "1 when the worker may claim work.",
        )
        .expect("ready metric");

        let deliveries_total = IntCounterVec::new(
            Opts::new(
                "relayforge_deliveries_total",
                "Terminal and scheduled delivery transitions by status.",
            ),
            &["outcome"],
        )
        .expect("deliveries_total metric");

        let delivery_attempts_total = IntCounterVec::new(
            Opts::new(
                "relayforge_delivery_attempts_total",
                "Delivery attempts by outcome and error_code.",
            ),
            &["outcome", "error_code"],
        )
        .expect("delivery_attempts_total metric");

        let delivery_duration_seconds = HistogramVec::new(
            HistogramOpts::new(
                "relayforge_delivery_duration_seconds",
                "Outbound HTTP attempt duration in seconds.",
            )
            .buckets(vec![
                0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0,
            ]),
            &["outcome"],
        )
        .expect("delivery_duration_seconds metric");

        let queue_due = IntGauge::new(
            "relayforge_queue_due",
            "Deliveries currently due for claiming.",
        )
        .expect("queue_due metric");

        let queue_oldest_age_seconds = IntGauge::new(
            "relayforge_queue_oldest_age_seconds",
            "Age in seconds of the oldest due delivery.",
        )
        .expect("queue_oldest_age_seconds metric");

        let lease_recoveries_total = IntCounter::new(
            "relayforge_lease_recoveries_total",
            "Expired delivery leases recovered by the reaper.",
        )
        .expect("lease_recoveries_total metric");

        let rate_limit_denied_total = IntCounterVec::new(
            Opts::new(
                "relayforge_rate_limit_denied_total",
                "Rate-limit denials by scope.",
            ),
            &["scope"],
        )
        .expect("rate_limit_denied_total metric");

        let ssrf_blocks_total = IntCounterVec::new(
            Opts::new(
                "relayforge_ssrf_blocks_total",
                "Connect-time SSRF policy blocks by reason.",
            ),
            &["reason"],
        )
        .expect("ssrf_blocks_total metric");

        registry.register(Box::new(build)).unwrap();
        registry.register(Box::new(loop_ticks.clone())).unwrap();
        registry.register(Box::new(db_up.clone())).unwrap();
        registry.register(Box::new(schema_version.clone())).unwrap();
        registry.register(Box::new(ready.clone())).unwrap();
        registry
            .register(Box::new(deliveries_total.clone()))
            .unwrap();
        registry
            .register(Box::new(delivery_attempts_total.clone()))
            .unwrap();
        registry
            .register(Box::new(delivery_duration_seconds.clone()))
            .unwrap();
        registry.register(Box::new(queue_due.clone())).unwrap();
        registry
            .register(Box::new(queue_oldest_age_seconds.clone()))
            .unwrap();
        registry
            .register(Box::new(lease_recoveries_total.clone()))
            .unwrap();
        registry
            .register(Box::new(rate_limit_denied_total.clone()))
            .unwrap();
        registry
            .register(Box::new(ssrf_blocks_total.clone()))
            .unwrap();

        Self {
            registry,
            loop_ticks,
            db_up,
            schema_version,
            ready,
            deliveries_total,
            delivery_attempts_total,
            delivery_duration_seconds,
            queue_due,
            queue_oldest_age_seconds,
            lease_recoveries_total,
            rate_limit_denied_total,
            ssrf_blocks_total,
        }
    }

    /// Returns the Prometheus text exposition and its content type.
    pub fn render(&self) -> (String, &'static str) {
        let encoder = TextEncoder::new();
        let mut buffer = Vec::new();
        if encoder
            .encode(&self.registry.gather(), &mut buffer)
            .is_err()
        {
            return (
                "# error encoding metrics\n".to_string(),
                "text/plain; version=0.0.4",
            );
        }
        (
            String::from_utf8_lossy(&buffer).into_owned(),
            "text/plain; version=0.0.4; charset=utf-8",
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn renders_build_and_counters() {
        let metrics = Metrics::new("test-version");
        metrics.loop_ticks.inc();
        metrics
            .delivery_attempts_total
            .with_label_values(&["success", "none"])
            .inc();
        let (body, content_type) = metrics.render();
        assert!(body.contains("relayforge_worker_build_info{version=\"test-version\"} 1"));
        assert!(body.contains("relayforge_worker_loop_ticks_total 1"));
        assert!(body.contains(
            "relayforge_delivery_attempts_total{error_code=\"none\",outcome=\"success\"} 1"
        ));
        assert!(content_type.starts_with("text/plain"));
    }
}
