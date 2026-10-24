use chrono::{DateTime, Duration, Utc};
use rand::Rng;

/// Attempt outcome values matching `delivery_attempts.outcome` CHECK constraint
/// in `contracts/delivery-contract-v1.md` §3.2.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum AttemptOutcome {
    Success,
    RetryableFailure,
    PermanentFailure,
    Timeout,
    Blocked,
    RateLimited,
    Abandoned,
    Canceled,
}

impl AttemptOutcome {
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Success => "success",
            Self::RetryableFailure => "retryable_failure",
            Self::PermanentFailure => "permanent_failure",
            Self::Timeout => "timeout",
            Self::Blocked => "blocked",
            Self::RateLimited => "rate_limited",
            Self::Abandoned => "abandoned",
            Self::Canceled => "canceled",
        }
    }
}

/// Classification of an HTTP response status code per `contracts/delivery-contract-v1.md` §7.1.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct StatusClassification {
    pub outcome: AttemptOutcome,
    pub retryable: bool,
    pub error_code: Option<&'static str>,
}

/// Classifies an HTTP status code according to the normative table in §7.1.
pub fn classify_http_status(status: u16) -> StatusClassification {
    match status {
        200..=299 => StatusClassification {
            outcome: AttemptOutcome::Success,
            retryable: false,
            error_code: None,
        },
        300..=399 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_redirect"),
        },
        400 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_400"),
        },
        401 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_401"),
        },
        403 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_403"),
        },
        404 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_404"),
        },
        405 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_405"),
        },
        408 => StatusClassification {
            outcome: AttemptOutcome::RetryableFailure,
            retryable: true,
            error_code: Some("http_408"),
        },
        409 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_409"),
        },
        410 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_410"),
        },
        413 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_413"),
        },
        414 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_414"),
        },
        415 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_415"),
        },
        425 => StatusClassification {
            outcome: AttemptOutcome::RetryableFailure,
            retryable: true,
            error_code: Some("http_425"),
        },
        429 => StatusClassification {
            outcome: AttemptOutcome::RetryableFailure,
            retryable: true,
            error_code: Some("http_429"),
        },
        402..=499 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_4xx"),
        },
        500 | 502 | 503 | 504 | 507 => StatusClassification {
            outcome: AttemptOutcome::RetryableFailure,
            retryable: true,
            error_code: Some("http_5xx"),
        },
        501 | 505 | 506 | 508 | 510 | 511 => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("http_5xx_permanent"),
        },
        509..=599 => StatusClassification {
            outcome: AttemptOutcome::RetryableFailure,
            retryable: true,
            error_code: Some("http_5xx"),
        },
        _ => StatusClassification {
            outcome: AttemptOutcome::PermanentFailure,
            retryable: false,
            error_code: Some("unexpected_status"),
        },
    }
}

/// Parses an HTTP `Retry-After` header value into milliseconds per §7.2:
/// - Integer delta-seconds accepted if `0 <= value <= 86400`.
/// - HTTP-date accepted if parseable; converted to a non-negative delta from `now`.
/// - Effective value clamped to `[1, 3600]` seconds (`1_000..=3_600_000` ms).
pub fn parse_retry_after_ms(raw: &str, now: DateTime<Utc>) -> Option<u64> {
    let trimmed = raw.trim();
    if trimmed.is_empty() {
        return None;
    }
    if let Ok(secs) = trimmed.parse::<i64>() {
        if !(0..=86_400).contains(&secs) {
            return None;
        }
        let clamped = secs.clamp(1, 3_600) as u64;
        return Some(clamped * 1_000);
    }

    let target = httpdate::parse_http_date(trimmed).ok()?;
    let target_dt: DateTime<Utc> = target.into();
    let delta_secs = (target_dt - now).num_seconds().max(0);
    let clamped = delta_secs.clamp(1, 3_600) as u64;
    Some(clamped * 1_000)
}

/// Computes equal-jitter exponential backoff in milliseconds per §7.3:
/// `raw_ms = initial_backoff_ms * 2^(attempts_made - 1)`
/// `capped_ms = min(raw_ms, max_backoff_ms)`
/// `jittered_ms = capped_ms / 2 + U[0, capped_ms / 2)`
/// `delay_ms = min(max(jittered_ms, retry_after_ms), max_backoff_ms)`
pub fn compute_backoff_with_jitter_ms(
    attempts_made: i32,
    initial_backoff_ms: i32,
    max_backoff_ms: i32,
    retry_after_ms: Option<u64>,
    jitter_unit: f64,
) -> u64 {
    let init = (initial_backoff_ms.max(1)) as u64;
    let max_cap = (max_backoff_ms.max(initial_backoff_ms).max(1)) as u64;
    let exp = (attempts_made.max(1) as u32 - 1).min(30);
    let raw_ms = init.saturating_mul(1u64 << exp);
    let capped_ms = raw_ms.min(max_cap);

    let lower = capped_ms / 2;
    let span = capped_ms - lower;
    let unit = jitter_unit.clamp(0.0, 1.0 - f64::EPSILON);
    let jitter = ((span as f64) * unit).floor() as u64;
    let jittered_ms = lower + jitter.min(span.saturating_sub(1));

    match retry_after_ms {
        Some(ra) => jittered_ms.max(ra).min(max_cap),
        None => jittered_ms,
    }
}

/// Samples a random equal-jitter backoff using thread_rng.
pub fn sample_backoff_ms(
    attempts_made: i32,
    initial_backoff_ms: i32,
    max_backoff_ms: i32,
    retry_after_ms: Option<u64>,
) -> u64 {
    let unit: f64 = rand::thread_rng().r#gen::<f64>();
    compute_backoff_with_jitter_ms(
        attempts_made,
        initial_backoff_ms,
        max_backoff_ms,
        retry_after_ms,
        unit,
    )
}

/// Next delivery state after a retryable failure or expired lease per §7.4.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum RetryDecision {
    RetryScheduled { next_attempt_at: DateTime<Utc> },
    DeadLettered,
}

/// Applies the dead-letter rules in §7.4:
/// 1. `attempt_count >= max_attempts`
/// 2. `now + delay > cycle_started_at + retry_window_seconds`
pub fn decide_retry(
    now: DateTime<Utc>,
    cycle_started_at: DateTime<Utc>,
    attempt_count: i32,
    max_attempts: i32,
    retry_window_seconds: i32,
    delay_ms: u64,
) -> RetryDecision {
    if attempt_count >= max_attempts {
        return RetryDecision::DeadLettered;
    }
    let next_attempt_at = now + Duration::milliseconds(delay_ms as i64);
    let deadline = cycle_started_at + Duration::seconds(retry_window_seconds.max(1) as i64);
    if next_attempt_at > deadline {
        return RetryDecision::DeadLettered;
    }
    RetryDecision::RetryScheduled { next_attempt_at }
}

/// Sanitizes and truncates `error_detail` to at most 512 characters without control characters.
pub fn sanitize_error_detail(detail: &str) -> String {
    let cleaned: String = detail
        .chars()
        .map(|c| if c.is_control() { ' ' } else { c })
        .collect();
    let trimmed = cleaned.trim();
    if trimmed.chars().count() <= 512 {
        trimmed.to_string()
    } else {
        trimmed.chars().take(512).collect()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use proptest::prelude::*;

    #[test]
    fn status_matrix_matches_contract() {
        for code in [200u16, 201, 202, 204, 205, 299] {
            let c = classify_http_status(code);
            assert_eq!(c.outcome, AttemptOutcome::Success);
            assert!(!c.retryable);
            assert_eq!(c.error_code, None);
        }

        for code in [300u16, 301, 302, 303, 307, 308, 399] {
            let c = classify_http_status(code);
            assert_eq!(c.outcome, AttemptOutcome::PermanentFailure);
            assert!(!c.retryable);
            assert_eq!(c.error_code, Some("http_redirect"));
        }

        let permanent_specific = [
            (400u16, "http_400"),
            (401, "http_401"),
            (403, "http_403"),
            (404, "http_404"),
            (405, "http_405"),
            (409, "http_409"),
            (410, "http_410"),
            (413, "http_413"),
            (414, "http_414"),
            (415, "http_415"),
        ];
        for (code, expected) in permanent_specific {
            let c = classify_http_status(code);
            assert_eq!(c.outcome, AttemptOutcome::PermanentFailure);
            assert!(!c.retryable);
            assert_eq!(c.error_code, Some(expected));
        }

        for code in [418u16, 421, 422, 423, 424, 426, 428, 431, 451, 499] {
            let c = classify_http_status(code);
            assert_eq!(c.outcome, AttemptOutcome::PermanentFailure);
            assert!(!c.retryable);
            assert_eq!(c.error_code, Some("http_4xx"));
        }

        for (code, expected) in [(408u16, "http_408"), (425, "http_425"), (429, "http_429")] {
            let c = classify_http_status(code);
            assert_eq!(c.outcome, AttemptOutcome::RetryableFailure);
            assert!(c.retryable);
            assert_eq!(c.error_code, Some(expected));
        }

        for code in [500u16, 502, 503, 504, 507, 599] {
            let c = classify_http_status(code);
            assert_eq!(c.outcome, AttemptOutcome::RetryableFailure);
            assert!(c.retryable);
            assert_eq!(c.error_code, Some("http_5xx"));
        }

        for code in [501u16, 505, 506, 508, 510, 511] {
            let c = classify_http_status(code);
            assert_eq!(c.outcome, AttemptOutcome::PermanentFailure);
            assert!(!c.retryable);
            assert_eq!(c.error_code, Some("http_5xx_permanent"));
        }

        let c100 = classify_http_status(100);
        assert_eq!(c100.outcome, AttemptOutcome::PermanentFailure);
        assert_eq!(c100.error_code, Some("unexpected_status"));
    }

    #[test]
    fn retry_after_parsing_and_clamping() {
        let now = Utc::now();
        assert_eq!(parse_retry_after_ms("0", now), Some(1_000));
        assert_eq!(parse_retry_after_ms("15", now), Some(15_000));
        assert_eq!(parse_retry_after_ms("7200", now), Some(3_600_000));
        assert_eq!(parse_retry_after_ms("86401", now), None);
        assert_eq!(parse_retry_after_ms("-5", now), None);
        assert_eq!(parse_retry_after_ms("not-a-date", now), None);

        let future = now + Duration::seconds(120);
        let http_date = httpdate::fmt_http_date(future.into());
        let parsed = parse_retry_after_ms(&http_date, now).expect("valid http-date");
        assert!((119_000..=121_000).contains(&parsed));
    }

    #[test]
    fn dead_letter_on_attempts_or_window_exhaustion() {
        let now = Utc::now();
        let start = now - Duration::seconds(86_398);
        // Attempt count exhausted
        assert_eq!(
            decide_retry(now, now, 8, 8, 86_400, 5_000),
            RetryDecision::DeadLettered
        );
        // Window exhausted (only 2 seconds left, delay is 5 seconds)
        assert_eq!(
            decide_retry(now, start, 2, 8, 86_400, 5_000),
            RetryDecision::DeadLettered
        );
        // Within bounds
        assert!(matches!(
            decide_retry(now, now, 2, 8, 86_400, 5_000),
            RetryDecision::RetryScheduled { .. }
        ));
    }

    proptest! {
        #[test]
        fn backoff_respects_bounds_and_never_overflows(
            attempt in 1i32..=100,
            initial_ms in 100i32..=60_000,
            extra_cap_ms in 0i32..=3_600_000,
            jitter in 0.0f64..1.0f64,
        ) {
            let max_ms = initial_ms.saturating_add(extra_cap_ms);
            let delay = compute_backoff_with_jitter_ms(attempt, initial_ms, max_ms, None, jitter);

            let exp = (attempt as u32 - 1).min(30);
            let raw = (initial_ms as u64).saturating_mul(1u64 << exp);
            let capped = raw.min(max_ms as u64);
            let min_bound = capped / 2;

            prop_assert!(delay >= min_bound, "delay {} < min_bound {}", delay, min_bound);
            prop_assert!(delay <= capped, "delay {} > capped {}", delay, capped);
        }
    }
}
