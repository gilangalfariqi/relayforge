use std::collections::HashMap;
use std::future::Future;
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr};
use std::pin::Pin;
use std::sync::{Arc, RwLock};
use std::time::Duration;

use reqwest::dns::{Addrs, Name, Resolve, Resolving};
use url::{Host, Url};

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ValidationStage {
    Registration,
    Delivery,
}

#[derive(Debug, Clone, PartialEq, Eq)]
pub enum ValidationDecision {
    Allow {
        url: Url,
        host: String,
        addrs: Vec<SocketAddr>,
    },
    Deny {
        reason: &'static str,
    },
    Retry {
        reason: &'static str,
    },
}

/// Abstraction over DNS resolution so both real Tokio DNS lookup and deterministic
/// test resolvers execute the exact same validation pipeline.
pub trait DnsResolver: Send + Sync {
    fn lookup<'a>(
        &'a self,
        host: &'a str,
    ) -> Pin<Box<dyn Future<Output = Result<Vec<IpAddr>, String>> + Send + 'a>>;
}

/// System DNS resolver using `tokio::net::lookup_host` with a 2-second timeout
/// per `contracts/ssrf-policy-v1.md` §4.1.
#[derive(Debug, Clone, Default)]
pub struct SystemDnsResolver {
    pub timeout: Duration,
}

impl SystemDnsResolver {
    pub fn new() -> Self {
        Self {
            timeout: Duration::from_secs(2),
        }
    }
}

impl DnsResolver for SystemDnsResolver {
    fn lookup<'a>(
        &'a self,
        host: &'a str,
    ) -> Pin<Box<dyn Future<Output = Result<Vec<IpAddr>, String>> + Send + 'a>> {
        let timeout = if self.timeout.is_zero() {
            Duration::from_secs(2)
        } else {
            self.timeout
        };
        Box::pin(async move {
            let target = format!("{host}:443");
            let resolved = tokio::time::timeout(timeout, tokio::net::lookup_host(target))
                .await
                .map_err(|_| "dns lookup timed out".to_string())?
                .map_err(|e| e.to_string())?;
            Ok(resolved.map(|sa| sa.ip()).collect())
        })
    }
}

/// Validates a destination URL and its resolved IP addresses according to
/// `contracts/ssrf-policy-v1.md`.
pub async fn validate_destination(
    raw: &str,
    stage: ValidationStage,
    allowed_ports: &[u16],
    resolver: &dyn DnsResolver,
) -> ValidationDecision {
    if raw.is_empty() || raw.len() > 2048 {
        return ValidationDecision::Deny {
            reason: "url_invalid",
        };
    }
    if has_ambiguous_authority(raw) {
        return ValidationDecision::Deny {
            reason: "ambiguous_authority",
        };
    }
    let parsed = match Url::parse(raw) {
        Ok(u) => u,
        Err(_) => {
            return ValidationDecision::Deny {
                reason: "url_invalid",
            };
        }
    };
    if parsed.scheme() != "https" {
        return ValidationDecision::Deny {
            reason: "scheme_not_https",
        };
    }
    if !parsed.username().is_empty() || parsed.password().is_some() || authority_has_at(raw) {
        return ValidationDecision::Deny {
            reason: "userinfo_present",
        };
    }
    if parsed.fragment().is_some() || raw.contains('#') {
        return ValidationDecision::Deny {
            reason: "fragment_present",
        };
    }
    let port = parsed.port_or_known_default().unwrap_or(443);
    if !allowed_ports.contains(&port) {
        return ValidationDecision::Deny {
            reason: "port_not_allowed",
        };
    }

    let host_str = match parsed.host_str() {
        Some(h) if !h.is_empty() => h.to_ascii_lowercase(),
        _ => {
            return ValidationDecision::Deny {
                reason: "host_missing",
            };
        }
    };
    if host_str.len() > 253 {
        return ValidationDecision::Deny {
            reason: "url_invalid",
        };
    }
    if host_str.ends_with('.') {
        return ValidationDecision::Deny {
            reason: "trailing_dot",
        };
    }
    if !host_str.is_ascii() {
        return ValidationDecision::Deny {
            reason: "host_not_allowed",
        };
    }

    match parsed.host() {
        Some(Host::Ipv4(v4)) => {
            let ip = IpAddr::V4(v4);
            if !is_global_ip(ip) {
                return ValidationDecision::Deny {
                    reason: "ip_denied",
                };
            }
            ValidationDecision::Allow {
                url: parsed,
                host: host_str,
                addrs: vec![SocketAddr::new(ip, port)],
            }
        }
        Some(Host::Ipv6(v6)) => {
            let ip = IpAddr::V6(v6);
            if !is_global_ip(ip) {
                return ValidationDecision::Deny {
                    reason: "ip_denied",
                };
            }
            ValidationDecision::Allow {
                url: parsed,
                host: host_str,
                addrs: vec![SocketAddr::new(ip, port)],
            }
        }
        Some(Host::Domain(domain)) => {
            let lower = domain.to_ascii_lowercase();
            if has_denied_suffix(&lower) {
                return ValidationDecision::Deny {
                    reason: "denied_suffix",
                };
            }
            if is_non_canonical_ipv4(&lower) {
                return ValidationDecision::Deny {
                    reason: "ip_denied",
                };
            }
            if !lower.contains('.') {
                return ValidationDecision::Deny {
                    reason: "host_not_fqdn",
                };
            }
            let ips = match resolver.lookup(&lower).await {
                Ok(list) => list,
                Err(_) => {
                    return match stage {
                        ValidationStage::Registration => ValidationDecision::Deny {
                            reason: "dns_error",
                        },
                        ValidationStage::Delivery => ValidationDecision::Retry {
                            reason: "dns_error_retryable",
                        },
                    };
                }
            };
            if ips.is_empty() {
                return ValidationDecision::Deny {
                    reason: "no_addresses",
                };
            }
            let mut socket_addrs = Vec::with_capacity(ips.len());
            for ip in ips {
                if !is_global_ip(ip) {
                    return ValidationDecision::Deny {
                        reason: "ip_denied",
                    };
                }
                socket_addrs.push(SocketAddr::new(ip, port));
            }
            ValidationDecision::Allow {
                url: parsed,
                host: lower,
                addrs: socket_addrs,
            }
        }
        None => ValidationDecision::Deny {
            reason: "host_missing",
        },
    }
}

fn authority_slice(raw: &str) -> &str {
    let after_scheme = match raw.find("://") {
        Some(idx) => &raw[idx + 3..],
        None => raw,
    };
    match after_scheme.find(['/', '?', '#']) {
        Some(idx) => &after_scheme[..idx],
        None => after_scheme,
    }
}

fn has_ambiguous_authority(raw: &str) -> bool {
    let auth = authority_slice(raw);
    auth.contains('\\') || auth.contains('%')
}

fn authority_has_at(raw: &str) -> bool {
    authority_slice(raw).contains('@')
}

const DENIED_SUFFIXES: &[&str] = &[
    ".localhost",
    ".local",
    ".internal",
    ".home.arpa",
    ".lan",
    ".corp",
    ".intranet",
    ".test",
    ".example",
    ".invalid",
];

fn has_denied_suffix(host: &str) -> bool {
    if host == "localhost" {
        return true;
    }
    DENIED_SUFFIXES.iter().any(|s| host.ends_with(s))
}

fn is_non_canonical_ipv4(host: &str) -> bool {
    let parts: Vec<&str> = host.split('.').collect();
    if parts.is_empty() {
        return false;
    }
    for p in parts {
        if p.is_empty() {
            return false;
        }
        if let Some(hex) = p.strip_prefix("0x") {
            if hex.is_empty() || !hex.chars().all(|c| c.is_ascii_hexdigit()) {
                return false;
            }
            continue;
        }
        if !p.chars().all(|c| c.is_ascii_digit()) {
            return false;
        }
    }
    true
}

/// Checks whether an IPv4 or IPv6 address is globally routable under
/// `contracts/ssrf-policy-v1.md` §3.1 and §3.2.
pub fn is_global_ip(ip: IpAddr) -> bool {
    match ip {
        IpAddr::V4(v4) => is_global_ipv4(v4),
        IpAddr::V6(v6) => is_global_ipv6(v6),
    }
}

fn is_global_ipv4(ip: Ipv4Addr) -> bool {
    let bits = u32::from(ip);
    const DENIED_V4: &[(u32, u32)] = &[
        (0x0000_0000, 8),  // 0.0.0.0/8
        (0x0a00_0000, 8),  // 10.0.0.0/8
        (0x6440_0000, 10), // 100.64.0.0/10
        (0x7f00_0000, 8),  // 127.0.0.0/8
        (0xa9fe_0000, 16), // 169.254.0.0/16
        (0xac10_0000, 12), // 172.16.0.0/12
        (0xc000_0000, 24), // 192.0.0.0/24
        (0xc000_0200, 24), // 192.0.2.0/24
        (0xc058_6300, 24), // 192.88.99.0/24
        (0xc0a8_0000, 16), // 192.168.0.0/16
        (0xc612_0000, 15), // 198.18.0.0/15
        (0xc633_6400, 24), // 198.51.100.0/24
        (0xcb00_7100, 24), // 203.0.113.0/24
        (0xe000_0000, 4),  // 224.0.0.0/4
        (0xf000_0000, 4),  // 240.0.0.0/4
    ];
    for &(net, prefix) in DENIED_V4 {
        let mask = if prefix == 0 {
            0
        } else {
            !0u32 << (32 - prefix)
        };
        if (bits & mask) == (net & mask) {
            return false;
        }
    }
    true
}

fn is_global_ipv6(ip: Ipv6Addr) -> bool {
    let bits = u128::from(ip);
    // Must be inside 2000::/3
    let global_mask = !0u128 << (128 - 3);
    let global_net = 0x2000_0000_0000_0000_0000_0000_0000_0000u128;
    if (bits & global_mask) != global_net {
        return false;
    }

    const DENIED_V6: &[(u128, u32)] = &[
        (0x0000_0000_0000_0000_0000_0000_0000_0000, 128), // ::/128
        (0x0000_0000_0000_0000_0000_0000_0000_0001, 128), // ::1/128
        (0x0000_0000_0000_0000_0000_ffff_0000_0000, 96),  // ::ffff:0:0/96
        (0x0064_ff9b_0000_0000_0000_0000_0000_0000, 96),  // 64:ff9b::/96
        (0x0064_ff9b_0001_0000_0000_0000_0000_0000, 48),  // 64:ff9b:1::/48
        (0x0100_0000_0000_0000_0000_0000_0000_0000, 64),  // 100::/64
        (0x2001_0000_0000_0000_0000_0000_0000_0000, 32),  // 2001::/32 (Teredo)
        (0x2001_0001_0000_0000_0000_0000_0000_0001, 128), // 2001:1::1/128
        (0x2001_0001_0000_0000_0000_0000_0000_0002, 128), // 2001:1::2/128
        (0x2001_0002_0000_0000_0000_0000_0000_0000, 48),  // 2001:2::/48
        (0x2001_0003_0000_0000_0000_0000_0000_0000, 32),  // 2001:3::/32
        (0x2001_0004_0112_0000_0000_0000_0000_0000, 48),  // 2001:4:112::/48
        (0x2001_0010_0000_0000_0000_0000_0000_0000, 28),  // 2001:10::/28
        (0x2001_0020_0000_0000_0000_0000_0000_0000, 28),  // 2001:20::/28
        (0x2001_0db8_0000_0000_0000_0000_0000_0000, 32),  // 2001:db8::/32
        (0x2002_0000_0000_0000_0000_0000_0000_0000, 16),  // 2002::/16 (6to4)
        (0x3fff_0000_0000_0000_0000_0000_0000_0000, 20),  // 3fff::/20
        (0x5f00_0000_0000_0000_0000_0000_0000_0000, 16),  // 5f00::/16
        (0xfc00_0000_0000_0000_0000_0000_0000_0000, 7),   // fc00::/7
        (0xfe80_0000_0000_0000_0000_0000_0000_0000, 10),  // fe80::/10
        (0xff00_0000_0000_0000_0000_0000_0000_0000, 8),   // ff00::/8
    ];
    for &(net, prefix) in DENIED_V6 {
        let mask = if prefix == 128 {
            !0u128
        } else {
            !0u128 << (128 - prefix)
        };
        if (bits & mask) == (net & mask) {
            return false;
        }
    }
    true
}

/// Custom connect-time `reqwest::dns::Resolve` implementation that returns only
/// the pre-validated `SocketAddr` list for a hostname, preventing DNS rebinding
/// between validation and socket connect (`contracts/ssrf-policy-v1.md` §4.5).
#[derive(Clone, Default)]
pub struct PinnedDnsResolver {
    pins: Arc<RwLock<HashMap<String, Vec<SocketAddr>>>>,
}

impl PinnedDnsResolver {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn set_host_addrs(&self, host: &str, addrs: Vec<SocketAddr>) {
        if let Ok(mut guard) = self.pins.write() {
            guard.insert(host.to_ascii_lowercase(), addrs);
        }
    }

    pub fn remove_host_addrs(&self, host: &str) {
        if let Ok(mut guard) = self.pins.write() {
            guard.remove(&host.to_ascii_lowercase());
        }
    }
}

impl Resolve for PinnedDnsResolver {
    fn resolve(&self, name: Name) -> Resolving {
        let host = name.as_str().to_ascii_lowercase();
        let maybe_addrs = self
            .pins
            .read()
            .ok()
            .and_then(|guard| guard.get(&host).cloned());

        Box::pin(async move {
            match maybe_addrs {
                Some(addrs) if !addrs.is_empty() => {
                    let iter: Addrs = Box::new(addrs.into_iter());
                    Ok(iter)
                }
                _ => Err(Box::new(std::io::Error::new(
                    std::io::ErrorKind::AddrNotAvailable,
                    format!("no validated SSRF pin for host {host}"),
                ))
                    as Box<dyn std::error::Error + Send + Sync>),
            }
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde::Deserialize;

    #[derive(Debug, Deserialize)]
    struct VectorFile {
        vectors: Vec<TestVector>,
    }

    #[derive(Debug, Deserialize)]
    struct TestVector {
        id: String,
        input: String,
        stage: String,
        resolved_ips: Vec<String>,
        #[serde(default)]
        resolution: Option<String>,
        expected: String,
        reason: String,
    }

    struct StaticTestResolver {
        ips: Vec<IpAddr>,
        fail: bool,
    }

    impl DnsResolver for StaticTestResolver {
        fn lookup<'a>(
            &'a self,
            _host: &'a str,
        ) -> Pin<Box<dyn Future<Output = Result<Vec<IpAddr>, String>> + Send + 'a>> {
            let res = if self.fail {
                Err("simulated dns error".to_string())
            } else {
                Ok(self.ips.clone())
            };
            Box::pin(async move { res })
        }
    }

    #[tokio::test]
    async fn shared_ssrf_conformance_vectors() {
        let raw = std::fs::read_to_string("../contracts/ssrf-vectors.json")
            .expect("read ../contracts/ssrf-vectors.json");
        let file: VectorFile = serde_json::from_str(&raw).expect("parse ssrf-vectors.json");
        assert!(!file.vectors.is_empty());

        for v in file.vectors {
            let ips: Vec<IpAddr> = v
                .resolved_ips
                .iter()
                .map(|s| s.parse::<IpAddr>().expect("valid vector IP"))
                .collect();
            let resolver = StaticTestResolver {
                ips,
                fail: v.resolution.as_deref() == Some("error"),
            };
            let stage = match v.stage.as_str() {
                "delivery" => ValidationStage::Delivery,
                _ => ValidationStage::Registration,
            };

            let decision = validate_destination(&v.input, stage, &[443], &resolver).await;
            match (v.expected.as_str(), decision) {
                ("allow", ValidationDecision::Allow { .. }) => {}
                ("deny", ValidationDecision::Deny { reason }) => {
                    assert_eq!(
                        reason, v.reason,
                        "vector {} deny reason mismatch for {}",
                        v.id, v.input
                    );
                }
                ("retry", ValidationDecision::Retry { reason }) => {
                    assert_eq!(
                        reason, v.reason,
                        "vector {} retry reason mismatch for {}",
                        v.id, v.input
                    );
                }
                (exp, got) => {
                    panic!(
                        "vector {} ({}) expected {}, got {:?}",
                        v.id, v.input, exp, got
                    );
                }
            }
        }
    }

    #[tokio::test]
    async fn dns_rebinding_blocks_second_attempt() {
        use std::sync::atomic::{AtomicUsize, Ordering};

        struct RebindingResolver {
            calls: AtomicUsize,
        }

        impl DnsResolver for RebindingResolver {
            fn lookup<'a>(
                &'a self,
                _host: &'a str,
            ) -> Pin<Box<dyn Future<Output = Result<Vec<IpAddr>, String>> + Send + 'a>>
            {
                let n = self.calls.fetch_add(1, Ordering::SeqCst);
                let ips = if n == 0 {
                    vec!["93.184.216.34".parse::<IpAddr>().unwrap()]
                } else if n == 1 {
                    vec!["169.254.169.254".parse::<IpAddr>().unwrap()]
                } else {
                    vec!["::ffff:127.0.0.1".parse::<IpAddr>().unwrap()]
                };
                Box::pin(async move { Ok(ips) })
            }
        }

        let resolver = RebindingResolver {
            calls: AtomicUsize::new(0),
        };
        let pinned = PinnedDnsResolver::new();
        let url = "https://hooks.customer.test.org/webhook";

        // Attempt 1 (registration or first delivery): resolves to public IP 93.184.216.34 -> allowed
        match validate_destination(url, ValidationStage::Delivery, &[443], &resolver).await {
            ValidationDecision::Allow { host, addrs, .. } => {
                assert_eq!(addrs.len(), 1);
                assert_eq!(addrs[0].ip().to_string(), "93.184.216.34");
                pinned.set_host_addrs(&host, addrs);
            }
            other => panic!("expected first lookup to be allowed, got {other:?}"),
        }

        // Attempt 2 (DNS rebinds to 169.254.169.254 metadata IP): blocked with ip_denied
        match validate_destination(url, ValidationStage::Delivery, &[443], &resolver).await {
            ValidationDecision::Deny { reason } => {
                assert_eq!(reason, "ip_denied");
                pinned.remove_host_addrs("hooks.customer.test.org");
            }
            other => panic!("expected rebind to metadata IP to be denied, got {other:?}"),
        }

        // Attempt 3 (DNS rebinds to IPv4-mapped IPv6 loopback ::ffff:127.0.0.1): blocked with ip_denied
        match validate_destination(url, ValidationStage::Delivery, &[443], &resolver).await {
            ValidationDecision::Deny { reason } => {
                assert_eq!(reason, "ip_denied");
            }
            other => panic!("expected rebind to IPv4-mapped IPv6 to be denied, got {other:?}"),
        }
    }
}
