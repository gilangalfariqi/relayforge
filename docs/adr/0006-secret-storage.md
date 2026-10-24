# ADR-0006: Credential hashing, envelope encryption, shared KEK

Status: Accepted (implemented; last reviewed 2026-10-24). Date: 2026-10-22.

## Context

API keys must be verifiable without storing plaintext. Signing secrets must be recoverable by the
worker to compute HMAC (assumption A7). A KMS/signing service would avoid sharing a KEK but adds a
hard dependency and latency on the delivery path.

## Decision

- API keys: 256-bit CSPRNG secret, public `key_id`, store `HMAC-SHA256(pepper, secret)` (32 bytes).
  Compare with `hmac.Equal`. Plaintext shown once.
- Signing secrets: 32 random bytes, `whsec_` + standard base64. At rest: AES-256-GCM with a 96-bit
  random nonce, AAD = `tenant_id || endpoint_id || secret_id || key_version`.
- KEK: 32-byte AES key from the environment (`RELAYFORGE_KEK`), versioned. **Shared by API and worker
  in v1.** Documented residual risk: worker compromise decrypts all signing secrets.
- No custom crypto. Standard-library / well-known crates only.

KEK rotation re-encrypts rows in a batched job (Phase 4). A KMS or remote HMAC service is future work,
not a hidden half-implementation.

## Alternatives considered

| Alternative | Why not now |
| --- | --- |
| bcrypt/argon2 for API keys | Unnecessary for 256-bit random secrets; slower auth path |
| Store signing secrets hashed | Worker cannot HMAC |
| Per-tenant KEK | Better isolation; more operational complexity; deferred |
| Cloud KMS / Vault Transit | Correct blast-radius reduction; extra SPOF; deferred with this ADR as the trigger |
