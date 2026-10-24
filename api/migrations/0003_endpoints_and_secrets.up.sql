-- 0003: tenant endpoints and their encrypted signing secrets.

CREATE TABLE endpoints (
    id                     uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id              uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    name                   text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    url                    text NOT NULL
                           CHECK (char_length(url) <= 2048 AND url ~ '^https://'),
    status                 text NOT NULL DEFAULT 'active'
                           CHECK (status IN ('active', 'paused', 'disabled')),
    event_types            text[] NOT NULL DEFAULT '{}'
                           CHECK (cardinality(event_types) <= 20),
    max_attempts           int NOT NULL DEFAULT 8 CHECK (max_attempts BETWEEN 1 AND 20),
    initial_backoff_ms     int NOT NULL DEFAULT 5000
                           CHECK (initial_backoff_ms BETWEEN 100 AND 3600000),
    max_backoff_ms         int NOT NULL DEFAULT 3600000
                           CHECK (max_backoff_ms >= initial_backoff_ms),
    retry_window_seconds   int NOT NULL DEFAULT 86400
                           CHECK (retry_window_seconds BETWEEN 60 AND 604800),
    timeout_ms             int NOT NULL DEFAULT 15000
                           CHECK (timeout_ms BETWEEN 1000 AND 30000),
    rate_limit_per_sec     int NOT NULL DEFAULT 10
                           CHECK (rate_limit_per_sec BETWEEN 1 AND 100),
    consecutive_failures   int NOT NULL DEFAULT 0 CHECK (consecutive_failures >= 0),
    disabled_at            timestamptz,
    disabled_reason        text CHECK (disabled_reason IS NULL OR char_length(disabled_reason) <= 64),
    created_at             timestamptz NOT NULL DEFAULT now(),
    updated_at             timestamptz NOT NULL DEFAULT now(),
    deleted_at             timestamptz
);

CREATE UNIQUE INDEX endpoints_tenant_url_active_key
    ON endpoints (tenant_id, lower(url))
    WHERE deleted_at IS NULL;

CREATE INDEX endpoints_tenant_created_idx
    ON endpoints (tenant_id, created_at DESC)
    WHERE deleted_at IS NULL;

CREATE INDEX endpoints_fanout_idx
    ON endpoints (tenant_id)
    INCLUDE (id, event_types, max_attempts, initial_backoff_ms, max_backoff_ms,
             retry_window_seconds, timeout_ms, rate_limit_per_sec)
    WHERE status = 'active' AND deleted_at IS NULL;

CREATE TABLE endpoint_secrets (
    id          uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    endpoint_id uuid NOT NULL REFERENCES endpoints(id) ON DELETE CASCADE,
    ciphertext  bytea NOT NULL CHECK (octet_length(ciphertext) >= 16),
    nonce       bytea NOT NULL CHECK (octet_length(nonce) = 12),
    key_version int NOT NULL CHECK (key_version >= 1),
    secret_hint text NOT NULL CHECK (char_length(secret_hint) BETWEEN 1 AND 8),
    status      text NOT NULL DEFAULT 'active'
                CHECK (status IN ('active', 'retired')),
    created_at  timestamptz NOT NULL DEFAULT now(),
    expires_at  timestamptz,
    retired_at  timestamptz
);

CREATE INDEX endpoint_secrets_active_idx
    ON endpoint_secrets (endpoint_id)
    WHERE status = 'active';

CREATE INDEX endpoint_secrets_tenant_idx
    ON endpoint_secrets (tenant_id, endpoint_id);
