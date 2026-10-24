# SSRF destination policy v1

Status: proposed, Phase 0. Last updated: 2026-10-22.
Applies to: Go endpoint registration (early feedback) and Rust delivery worker (authoritative enforcement).

Customer-controlled webhook URLs are the primary SSRF vector in this product. A single hostname check is
not sufficient. This policy is enforced at **connect time** on the exact addresses the HTTP client uses,
with redirects and proxies disabled, and with network egress isolation as an independent layer.

## 1. Enforcement points

| Point | Purpose | Authority |
| --- | --- | --- |
| Endpoint create/update (Go) | Reject obviously invalid destinations; good UX; reduce bad rows | Advisory |
| Every delivery attempt (Rust) | Resolve, validate, and connect only to validated addresses | **Authoritative** |
| Network egress (deployment) | Prevent access to private networks/metadata even if application checks fail | Independent layer |

A registration check can become stale: DNS can change between registration and delivery, or an attacker
can register a hostname that later resolves privately. Therefore the worker repeats the full check on
every attempt and never relies on a stored “validated” flag.

## 2. URL policy

A URL is acceptable only if all of the following hold:

1. It parses under a WHATWG-compatible URL parser. Parser disagreement is treated as rejection.
2. Scheme is exactly `https` (case-insensitive, normalised). `http`, `file`, `gopher`, `ftp`, `data`,
   `ws`, `wss`, and everything else are rejected.
3. No userinfo (`user:pass@`). Reject before any host comparison.
4. No fragment (`#...`). Fragments have no delivery meaning and create parser ambiguity.
5. Host is present, ASCII or punycode, length ≤ 253, and contains at least one dot. Single-label hosts
   (`localhost`, `metadata`, `intranet`) are rejected.
6. Host does not end with a dot. Trailing-dot normalisation differs between resolvers; reject.
7. Host does not end with a denied suffix: `.localhost`, `.local`, `.internal`, `.home.arpa`, `.lan`,
   `.corp`, `.intranet`, `.test`, `.example`, `.invalid`.
8. Port is in the configured allowlist. Default: `443` only. No port means 443.
9. Path and query are allowed; total URL length ≤ 2048 bytes.
10. The authority contains no backslash, percent-encoded host delimiter, or `@` ambiguity. The parser's
    host reading must be unambiguous.

If the host is an IP literal, it is normalised and checked directly. Decimal, octal, hexadecimal,
shorthand (`127.1`), and IPv4-mapped IPv6 forms are normalised by the parser and then rejected by the IP
policy where applicable. The implementation must use the parser's normalised host, never the raw string.

## 3. IP policy

An address is permitted only if it is **globally reachable** in the sense of the IANA special-purpose
address registries, and is not in the deny list below. When a hostname resolves to multiple addresses,
**every** address must be permitted; if any address is denied, the whole destination is rejected. This
prevents a “split answer” attack where one public and one private address are returned.

### 3.1 IPv4 deny list

| CIDR | Reason |
| --- | --- |
| `0.0.0.0/8` | “This network”, unspecified |
| `10.0.0.0/8` | Private (RFC 1918) |
| `100.64.0.0/10` | Carrier-grade NAT |
| `127.0.0.0/8` | Loopback |
| `169.254.0.0/16` | Link-local, cloud metadata (`169.254.169.254`) |
| `172.16.0.0/12` | Private |
| `192.0.0.0/24` | IETF protocol assignments |
| `192.0.2.0/24` | Documentation (TEST-NET-1) |
| `192.88.99.0/24` | 6to4 relay anycast (deprecated) |
| `192.168.0.0/16` | Private |
| `198.18.0.0/15` | Benchmarking |
| `198.51.100.0/24` | Documentation (TEST-NET-2) |
| `203.0.113.0/24` | Documentation (TEST-NET-3) |
| `224.0.0.0/4` | Multicast |
| `240.0.0.0/4` | Reserved, including broadcast |

### 3.2 IPv6 policy

IPv6 is accepted only if it is inside `2000::/3` (global unicast) and not in any deny range below.
Everything outside `2000::/3` is denied, including IPv4-mapped, IPv4-compatible, NAT64, 6to4, and Teredo.
This is intentionally stricter than “not private”: transition mechanisms can embed private IPv4 addresses
and bypass a naive check.

| CIDR | Reason |
| --- | --- |
| `::/128` | Unspecified |
| `::1/128` | Loopback |
| `::ffff:0:0/96` | IPv4-mapped (rejected entirely; no embedded-IPv4 reinterpretation) |
| `64:ff9b::/96` | NAT64 well-known prefix |
| `64:ff9b:1::/48` | NAT64 local-use prefix |
| `100::/64` | Discard-only |
| `2001::/32` | Teredo |
| `2001:1::1/128`, `2001:1::2/128` | Protocol assignments |
| `2001:2::/48` | Benchmarking |
| `2001:3::/32` | AMT |
| `2001:4:112::/48` | AS112 |
| `2001:10::/28` | ORCHID (deprecated) |
| `2001:20::/28` | ORCHIDv2 |
| `2001:db8::/32` | Documentation |
| `2002::/16` | 6to4 (embeds IPv4) |
| `3fff::/20` | Documentation |
| `5f00::/16` | Segment routing (SRv6) |
| `fc00::/7` | Unique local |
| `fe80::/10` | Link-local |
| `ff00::/8` | Multicast |

The list is versioned. New IANA assignments are reviewed at each contract revision; the conformance
vectors must be updated with the list.

## 4. DNS and connect-time enforcement

1. Resolve `A` and `AAAA` records for the hostname using the system resolver with a bounded timeout
   (2 s, inside the 3 s connect budget).
2. If resolution fails: registration rejects; delivery records `dns_error` (retryable, **not** `blocked`,
   because it is not evidence of a policy violation).
3. If zero addresses: reject (`no_addresses`).
4. Apply §3 to every returned address. If any is denied: reject (`destination_blocked`).
5. Connect only to the validated addresses. The HTTP client must not perform a second, unchecked
   resolution. Implement this with a custom resolver that returns the validated `SocketAddr` list to the
   client. The URL hostname is preserved for the HTTP `Host` header, TLS SNI, and certificate verification.
6. On retry, resolve and validate again. A hostname that changes from public to private is blocked on the
   next attempt.
7. Do not cache DNS answers across attempts in v1. Caching would create a rebinding window unless the
   cache is the same object used for the connection; simplicity wins.

`reqwest` 0.13.5 exposes `ClientBuilder::dns_resolver` with a `Resolve` implementation, which satisfies
step 5. This must be verified by a test with a controlled resolver, not assumed from documentation.

**Proxy feature trap (verified 2026-10-22):** reqwest 0.13 default features include `system-proxy`.
Calling `no_proxy()` is required and is **not** sufficient by itself if a future code path constructs a
second client. Phase 3 must disable the `system-proxy` crate feature (`default-features = false`, enable
`rustls`, `http2`, `charset` explicitly) **and** call `ClientBuilder::no_proxy()`. Tests must set
`HTTP_PROXY`/`HTTPS_PROXY` and prove no CONNECT/proxy hop occurs.

## 5. Redirects and proxies

- Redirects are **disabled** (`redirect::Policy::none()`). Any `3xx` is a failed delivery with
  `http_redirect`. RelayForge does not follow a `Location` header, even to an allowlisted host.
- Proxy auto-detection is **disabled** (`ClientBuilder::no_proxy()`). `HTTP_PROXY`, `HTTPS_PROXY`,
  `ALL_PROXY`, and OS proxy settings are ignored. A proxy would perform its own DNS resolution and
  bypass the connect-time check. If an operator requires an egress proxy, it must be a deliberate
  deployment-level proxy with its own destination policy, documented in a future ADR.
- No `Host` header override from tenant input. The host comes from the validated URL.

## 6. TLS and response handling

- TLS verification is always on. There is no `danger_accept_invalid_certs` option exposed by configuration.
- TLS 1.2 minimum; TLS 1.3 preferred. The exact cipher policy is delegated to rustls defaults.
- Response headers and bodies are untrusted. Only the status code and a parsed `Retry-After` are used.
- The response body is streamed and discarded after 8 KiB. It is never logged or stored.
- HTTP/2 is supported; the SSRF check is at the TCP address layer and is independent of protocol version.
- The client sends no cookies and keeps no cookie jar across tenants.

## 7. Network-layer defense (required in production, not provable in Compose)

Application checks can have bugs. Production deployment must also enforce:

- Worker egress allowed only to the public internet; no route to the database subnet except PostgreSQL, no
  route to the control plane, no route to cloud metadata (`169.254.169.254`, `fd00:ec2::254`).
- A default-deny egress policy or firewall rule; an allowlist for the PostgreSQL endpoint and DNS.
- Optional egress proxy (for example Smokescreen) with the same destination policy, as a second
  independent implementation.
- No Docker socket, no host network, no privileged containers.
- DNS resolver restricted to a trusted resolver; DNS over the platform's normal resolver, not tenant input.

Local Docker Compose cannot prove these properties. The README and threat model say so explicitly. A
green SSRF test suite is evidence about the application layer only.

## 8. Test vectors

`contracts/ssrf-vectors.json` is the shared conformance suite. Each case has:

```json
{
  "id": "ipv4-loopback",
  "input": "https://127.0.0.1/hook",
  "stage": "registration",
  "resolved_ips": [],
  "expected": "deny",
  "reason": "ip_denied"
}
```

- `stage`: `registration` (Go) or `delivery` (Rust). Cases without DNS use `resolved_ips: []` or a literal.
- `resolved_ips` lets both suites test DNS-dependent decisions without real network access. The test
  resolver returns exactly these addresses. A `resolution: "error"` field simulates a resolver failure.
- `expected`: `allow`, `deny`, or `retry`. `retry` is valid only for the delivery stage and means the
  destination is not proven hostile; it maps to a retryable `dns_error`, not `destination_blocked`.
- `reason`: stable machine code, not a user-facing message. Tests assert the decision for every vector and
  assert the exact reason where the parser result is deterministic. Parser-normalisation cases may return
  either `invalid_url` or `ip_denied`; both are denials. This exception is explicit so a test cannot hide a
  decision regression.

Both implementations must pass every vector. A vector change requires both test suites to change in the
same pull request. Phase 5 also runs real end-to-end cases against a controlled receiver and a DNS name
that resolves to a private address (using a test resolver or `/etc/hosts` in a container; never by
contacting a real private service).

## 9. Known limitations

| Limitation | Consequence | Mitigation |
| --- | --- | --- |
| A public IP can proxy to internal services | RelayForge cannot detect an open proxy on a public host | Egress policy, rate limits, abuse monitoring; inherent to webhooks |
| DNS resolver compromise | Attacker could return a public IP they control, not an internal one (private IPs are blocked) | TLS verification; trusted resolver; connect-time check |
| IANA ranges change | New special-purpose range could be temporarily allowed | Versioned list, review, alerts on policy updates |
| IPv6 transition mechanisms | Complex bypasses if parsed naively | Deny everything outside `2000::/3`; deny known transition prefixes |
| URL parser disagreement | Host confusion between services | One parser per service, reject ambiguous authority, shared vectors |
| Compose egress | Dev environment can reach host/private networks | Documented as not production-equivalent; production network policy required |
| Port allowlist too narrow | Some legitimate receivers use non-443 ports | Configurable allowlist; default remains 443 |
| Registration check and delivery check drift | Bad row accepted, blocked later | Rust is authoritative; shared vectors; delivery error is visible and alertable |

No claim is made that this policy makes SSRF impossible. It closes the direct destination classes and
forces an attacker to control a public service, while the network layer provides the final boundary.
