use std::env;
use std::time::Duration;

use anyhow::{Context, Result, bail};
use base64::Engine;
use base64::engine::general_purpose::STANDARD as BASE64_STD;

/// Validated worker configuration. All values come from the environment so the
/// same binary runs locally, in Compose, and in CI.
#[derive(Debug, Clone)]
pub struct Config {
    pub database_url: String,
    pub worker_id: String,
    pub health_addr: String,
    pub poll_interval: Duration,
    pub concurrency: usize,
    pub claim_batch: i64,
    pub lease_seconds: i64,
    pub reap_interval: Duration,
    pub reap_batch: i64,
    pub expected_schema_version: i64,
    pub shutdown_grace: Duration,
    pub log_level: String,
    pub kek: [u8; 32],
    pub allowed_ports: Vec<u16>,
    pub allow_insecure_local: bool,
}

impl Config {
    pub fn from_env() -> Result<Self> {
        let database_url =
            env::var("RELAYFORGE_DATABASE_URL").context("RELAYFORGE_DATABASE_URL is required")?;
        let worker_id = env::var("RELAYFORGE_WORKER_ID")
            .unwrap_or_else(|_| format!("worker-{}", &uuid::Uuid::new_v4().to_string()[..8]));
        let health_addr =
            env::var("RELAYFORGE_WORKER_HEALTH_ADDR").unwrap_or_else(|_| "0.0.0.0:9090".into());
        let poll_interval = parse_u64("RELAYFORGE_WORKER_POLL_INTERVAL_MS", 1000)?;
        let concurrency = parse_u64("RELAYFORGE_WORKER_CONCURRENCY", 8)?;
        let claim_batch = parse_u64("RELAYFORGE_WORKER_CLAIM_BATCH", 16)?;
        let lease_seconds = parse_u64("RELAYFORGE_WORKER_LEASE_SECONDS", 90)?;
        let reap_interval = parse_u64("RELAYFORGE_WORKER_REAP_INTERVAL_SECONDS", 15)?;
        let reap_batch = parse_u64("RELAYFORGE_WORKER_REAP_BATCH", 100)?;
        let expected_schema_version = parse_u64("RELAYFORGE_EXPECTED_SCHEMA_VERSION", 5)?;
        let shutdown_grace = parse_u64("RELAYFORGE_SHUTDOWN_TIMEOUT_SECONDS", 25)?;
        let log_level = env::var("RELAYFORGE_LOG_LEVEL").unwrap_or_else(|_| "info".into());

        let kek_raw = env::var("RELAYFORGE_KEK")
            .unwrap_or_else(|_| "QUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUE=".into());
        let kek_vec = BASE64_STD
            .decode(kek_raw.trim())
            .context("RELAYFORGE_KEK must be valid standard base64")?;
        let kek: [u8; 32] = kek_vec
            .try_into()
            .map_err(|_| anyhow::anyhow!("RELAYFORGE_KEK must decode to exactly 32 bytes"))?;

        let allowed_ports = match env::var("RELAYFORGE_ALLOWED_PORTS") {
            Ok(raw) if !raw.trim().is_empty() => {
                let mut ports = Vec::new();
                for part in raw.split(',') {
                    let p: u16 = part
                        .trim()
                        .parse()
                        .context("RELAYFORGE_ALLOWED_PORTS must be comma-separated u16 ports")?;
                    if p == 0 {
                        bail!("RELAYFORGE_ALLOWED_PORTS must not contain port 0");
                    }
                    ports.push(p);
                }
                ports
            }
            _ => vec![443],
        };

        let allow_insecure_local = matches!(
            env::var("RELAYFORGE_ALLOW_INSECURE_LOCAL_DELIVERY")
                .unwrap_or_default()
                .trim()
                .to_ascii_lowercase()
                .as_str(),
            "1" | "true" | "yes"
        );

        let cfg = Self {
            database_url,
            worker_id,
            health_addr,
            poll_interval: Duration::from_millis(poll_interval),
            concurrency: concurrency as usize,
            claim_batch: claim_batch as i64,
            lease_seconds: lease_seconds as i64,
            reap_interval: Duration::from_secs(reap_interval),
            reap_batch: reap_batch as i64,
            expected_schema_version: expected_schema_version as i64,
            shutdown_grace: Duration::from_secs(shutdown_grace),
            log_level,
            kek,
            allowed_ports,
            allow_insecure_local,
        };
        cfg.validate()?;
        Ok(cfg)
    }

    pub fn validate(&self) -> Result<()> {
        if !self.database_url.starts_with("postgres://")
            && !self.database_url.starts_with("postgresql://")
        {
            bail!("RELAYFORGE_DATABASE_URL must start with postgres:// or postgresql://");
        }
        if self.worker_id.is_empty() || self.worker_id.len() > 100 {
            bail!("RELAYFORGE_WORKER_ID must be 1-100 characters");
        }
        let (_, port) = self
            .health_addr
            .rsplit_once(':')
            .context("RELAYFORGE_WORKER_HEALTH_ADDR must be host:port")?;
        let port: u16 = port
            .parse()
            .context("RELAYFORGE_WORKER_HEALTH_ADDR has an invalid port")?;
        if port == 0 {
            bail!("RELAYFORGE_WORKER_HEALTH_ADDR port must not be 0");
        }
        if self.poll_interval < Duration::from_millis(50) {
            bail!("RELAYFORGE_WORKER_POLL_INTERVAL_MS must be >= 50");
        }
        if self.concurrency == 0 || self.concurrency > 256 {
            bail!("RELAYFORGE_WORKER_CONCURRENCY must be 1-256");
        }
        if self.claim_batch < 1 || self.claim_batch > 256 {
            bail!("RELAYFORGE_WORKER_CLAIM_BATCH must be 1-256");
        }
        if !(60..=300).contains(&self.lease_seconds) {
            bail!("RELAYFORGE_WORKER_LEASE_SECONDS must be between 60 and 300");
        }
        if self.reap_interval.is_zero() {
            bail!("RELAYFORGE_WORKER_REAP_INTERVAL_SECONDS must be positive");
        }
        if self.reap_batch < 1 || self.reap_batch > 1000 {
            bail!("RELAYFORGE_WORKER_REAP_BATCH must be 1-1000");
        }
        if self.expected_schema_version < 1 {
            bail!("RELAYFORGE_EXPECTED_SCHEMA_VERSION must be >= 1");
        }
        if self.shutdown_grace.is_zero() {
            bail!("RELAYFORGE_SHUTDOWN_TIMEOUT_SECONDS must be positive");
        }
        if self.allowed_ports.is_empty() {
            bail!("RELAYFORGE_ALLOWED_PORTS must not be empty");
        }
        match self.log_level.as_str() {
            "debug" | "info" | "warn" | "error" => {}
            other => bail!("RELAYFORGE_LOG_LEVEL {other:?} must be debug, info, warn, or error"),
        }
        Ok(())
    }
}

fn parse_u64(key: &str, fallback: u64) -> Result<u64> {
    match env::var(key) {
        Ok(raw) if !raw.trim().is_empty() => raw
            .trim()
            .parse::<u64>()
            .with_context(|| format!("{key} must be an integer")),
        _ => Ok(fallback),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn valid() -> Config {
        Config {
            database_url: "postgres://relayforge:relayforge@localhost:5432/relayforge".into(),
            worker_id: "worker-test".into(),
            health_addr: "0.0.0.0:9090".into(),
            poll_interval: Duration::from_millis(1000),
            concurrency: 8,
            claim_batch: 16,
            lease_seconds: 90,
            reap_interval: Duration::from_secs(15),
            reap_batch: 100,
            expected_schema_version: 5,
            shutdown_grace: Duration::from_secs(25),
            log_level: "info".into(),
            kek: [0x41u8; 32],
            allowed_ports: vec![443],
            allow_insecure_local: false,
        }
    }

    #[test]
    fn accepts_valid_config() {
        valid().validate().expect("valid config");
    }

    #[test]
    fn rejects_bad_database_url() {
        let mut cfg = valid();
        cfg.database_url = "mysql://localhost/db".into();
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn rejects_short_poll_interval() {
        let mut cfg = valid();
        cfg.poll_interval = Duration::from_millis(10);
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn rejects_zero_concurrency() {
        let mut cfg = valid();
        cfg.concurrency = 0;
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn rejects_invalid_lease_seconds() {
        let mut cfg = valid();
        cfg.lease_seconds = 30;
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn rejects_missing_port() {
        let mut cfg = valid();
        cfg.health_addr = "localhost".into();
        assert!(cfg.validate().is_err());
    }

    #[test]
    fn rejects_unknown_log_level() {
        let mut cfg = valid();
        cfg.log_level = "verbose".into();
        assert!(cfg.validate().is_err());
    }
}
