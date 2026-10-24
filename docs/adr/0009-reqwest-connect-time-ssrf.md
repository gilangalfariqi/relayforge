# ADR-0009: Connect-time SSRF enforcement

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

Customer-controlled webhook URLs are the primary SSRF vector. OWASP SSRF and Webhook cheat sheets
require HTTPS-only schemes, deny lists of non-global addresses, connect-time validation of the
addresses actually used, disabled redirects, and network isolation. Hostname checks at registration
are stale by delivery time (DNS rebinding).

reqwest 0.13.5 supports `ClientBuilder::dns_resolver`. Default features include `system-proxy`, which
would bypass connect-time checks if left enabled.

## Decision

- Registration validation in Go is advisory UX.
- Every delivery attempt in Rust resolves A+AAAA, rejects if **any** address is denied, and connects
  only to the validated `SocketAddr` list while preserving hostname for `Host`/SNI/TLS.
- Redirects off. `system-proxy` crate feature off. `no_proxy()` on the client.
- IPv6 only inside `2000::/3` minus documented special ranges.
- Production egress policy is an independent layer; Compose tests do not prove it.

## Alternatives considered

| Alternative | Why not |
| --- | --- |
| Registration-only hostname allowlist | Rebinding TOCTOU |
| HTTP allowed | Cleartext, extra schemes to police |
| Follow redirects if target is public | Redirect chains are a classic bypass; Stripe-style fail on 3xx |
| Allow private destinations behind a flag | Needs a network architecture (non-goal EP-7) |
