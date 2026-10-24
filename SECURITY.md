# Security policy

RelayForge is a portfolio-grade reference implementation, not a certified or production-hardened service.
Security reports are still welcome and are handled as described below.

## Supported versions

| Version | Supported |
| --- | --- |
| `main` (latest commit) | Yes |
| Tagged `v1.x` releases | Yes, for the latest minor |
| Older commits and forks | No |

## Reporting a vulnerability

**Do not open a public issue for a security problem.**

Use GitHub's private vulnerability reporting:

<https://github.com/gilangalfariqi/relayforge/security/advisories/new>

Include:

- affected commit or image tag;
- a minimal reproduction or proof of concept;
- the security impact you believe is present;
- whether the issue requires a hostile destination URL, a valid tenant credential, database access, or
  operator access;
- any suggested fix, if you have one.

Please do not include real customer data, production credentials, or live signing secrets. Use synthetic
tenants and local Compose data.

## What to expect

- Acknowledgement within 7 days.
- An initial triage decision within 14 days.
- A fix or a documented mitigation for confirmed high-severity issues, coordinated with the reporter.
- Public disclosure after a fix is available, unless the reporter and maintainer agree otherwise.

This is a single-maintainer project. These are targets, not a contractual SLA.

## Scope

In scope:

- authentication and tenant isolation bypasses;
- API-key or signing-secret disclosure;
- SSRF or DNS-rebinding bypasses at registration or connect time;
- signature forgery, timestamp replay, or secret-rotation flaws;
- delivery-state corruption, lost acknowledged events, or lease-fencing bypasses;
- SQL injection, unsafe deserialization, or dependency vulnerabilities reachable from this code;
- privilege escalation through the internal admin listener;
- CI/CD supply-chain weaknesses in the workflows in this repository.

Out of scope:

- findings that require a compromised host, database superuser, Docker daemon, or CI runner;
- missing production controls that are explicitly listed in
  [docs/11-known-limitations.md](docs/11-known-limitations.md), unless the report shows a concrete bypass
  of a documented mitigation;
- volumetric denial of service and capacity claims;
- the deliberately open local signup and HTTPS-downgrade path enabled only by
  `RELAYFORGE_ALLOW_INSECURE_LOCAL_DELIVERY=true` in `deploy/compose.yaml`;
- reports about the example receiver being plain HTTP inside the local Compose network.

## Local and deployment safety

- `deploy/compose.yaml` uses deliberately low-entropy placeholder secrets and open signup. It is for
  local development only. Do not expose it to an untrusted network.
- Production deployments must inject `RELAYFORGE_API_KEY_PEPPER`, `RELAYFORGE_KEK`, and
  `RELAYFORGE_ADMIN_TOKEN` from a secret manager.
- The API terminates no TLS itself; a reverse proxy must terminate TLS and must not expose the internal
  admin/metrics listener.
- RelayForge provides durable **at-least-once** delivery. Receivers must deduplicate on the
  `webhook-id` header in the same transaction as their business effect.

## Safe harbour

Good-faith research that follows this policy, avoids privacy violations and service disruption, and gives
the maintainer reasonable time to respond will not be pursued or reported by the maintainer. This
statement does not bind third parties or override applicable law.
