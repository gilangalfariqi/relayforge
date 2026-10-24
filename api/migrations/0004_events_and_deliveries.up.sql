-- 0004: events, deliveries, attempts, per-tenant schemas, idempotency.

CREATE TABLE events (
    id             uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id      uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    type           text NOT NULL
                   CHECK (char_length(type) <= 100
                          AND type ~ '^[a-z][a-z0-9_]*(\.[a-z0-9_]+){1,3}$'),
    schema_version int NOT NULL DEFAULT 1 CHECK (schema_version > 0),
    occurred_at    timestamptz NOT NULL,
    received_at    timestamptz NOT NULL DEFAULT now(),
    wire_body      bytea NOT NULL
                   CHECK (octet_length(wire_body) BETWEEN 1 AND 262144),
    content_type   text NOT NULL DEFAULT 'application/json'
                   CHECK (content_type = 'application/json'),
    payload        jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
    payload_bytes  int NOT NULL CHECK (payload_bytes BETWEEN 1 AND 262144),
    payload_sha256 bytea NOT NULL CHECK (octet_length(payload_sha256) = 32),
    trace_parent   text CHECK (trace_parent IS NULL OR char_length(trace_parent) <= 128),
    status         text NOT NULL DEFAULT 'accepted'
                   CHECK (status IN ('accepted', 'delivering', 'delivered',
                                     'partially_delivered', 'failed', 'no_subscribers')),
    delivery_count int NOT NULL DEFAULT 0 CHECK (delivery_count BETWEEN 0 AND 50),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX events_tenant_created_idx
    ON events (tenant_id, created_at DESC, id DESC);
CREATE INDEX events_tenant_type_created_idx
    ON events (tenant_id, type, created_at DESC, id DESC);
CREATE INDEX events_tenant_status_created_idx
    ON events (tenant_id, status, created_at DESC, id DESC);
CREATE INDEX events_retention_idx
    ON events (created_at);

CREATE TABLE deliveries (
    id                   uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id            uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    event_id             uuid NOT NULL REFERENCES events(id) ON DELETE CASCADE,
    endpoint_id          uuid NOT NULL REFERENCES endpoints(id) ON DELETE RESTRICT,
    cycle                int NOT NULL DEFAULT 1 CHECK (cycle >= 1),
    cycle_started_at     timestamptz NOT NULL DEFAULT now(),
    status               text NOT NULL DEFAULT 'pending'
                         CHECK (status IN ('pending', 'in_progress', 'retry_scheduled',
                                           'delivered', 'failed', 'dead_lettered', 'canceled')),
    attempt_count        int NOT NULL DEFAULT 0 CHECK (attempt_count >= 0),
    attempt_seq          int NOT NULL DEFAULT 0 CHECK (attempt_seq >= attempt_count),
    max_attempts         int NOT NULL CHECK (max_attempts BETWEEN 1 AND 20),
    initial_backoff_ms   int NOT NULL CHECK (initial_backoff_ms > 0),
    max_backoff_ms       int NOT NULL CHECK (max_backoff_ms >= initial_backoff_ms),
    retry_window_seconds int NOT NULL CHECK (retry_window_seconds > 0),
    timeout_ms           int NOT NULL CHECK (timeout_ms > 0),
    next_attempt_at      timestamptz NOT NULL DEFAULT now(),
    lease_token          uuid,
    lease_expires_at     timestamptz,
    worker_id            text CHECK (worker_id IS NULL OR char_length(worker_id) <= 100),
    last_error_code      text CHECK (last_error_code IS NULL OR char_length(last_error_code) <= 64),
    last_error_detail    text CHECK (last_error_detail IS NULL OR char_length(last_error_detail) <= 512),
    last_http_status     int CHECK (last_http_status IS NULL
                                   OR last_http_status BETWEEN 100 AND 599),
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    completed_at         timestamptz,
    dead_lettered_at     timestamptz,

    CONSTRAINT deliveries_event_endpoint_key UNIQUE (event_id, endpoint_id),
    CONSTRAINT deliveries_attempt_bound CHECK (attempt_count <= max_attempts),
    CONSTRAINT deliveries_in_progress_has_lease
        CHECK (status <> 'in_progress'
               OR (lease_token IS NOT NULL AND lease_expires_at IS NOT NULL)),
    CONSTRAINT deliveries_lease_sanity
        CHECK (lease_expires_at IS NULL OR lease_expires_at > created_at)
);

CREATE INDEX deliveries_claim_idx
    ON deliveries (next_attempt_at, id)
    WHERE status IN ('pending', 'retry_scheduled');
CREATE INDEX deliveries_lease_idx
    ON deliveries (lease_expires_at)
    WHERE status = 'in_progress';
CREATE INDEX deliveries_tenant_created_idx
    ON deliveries (tenant_id, created_at DESC, id DESC);
CREATE INDEX deliveries_tenant_status_created_idx
    ON deliveries (tenant_id, status, created_at DESC, id DESC);
CREATE INDEX deliveries_event_idx
    ON deliveries (event_id);
CREATE INDEX deliveries_endpoint_status_idx
    ON deliveries (endpoint_id, status)
    WHERE status IN ('pending', 'retry_scheduled', 'in_progress');
CREATE INDEX deliveries_retention_idx
    ON deliveries (created_at);

CREATE TABLE delivery_attempts (
    id          bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id   uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    delivery_id uuid NOT NULL REFERENCES deliveries(id) ON DELETE CASCADE,
    cycle       int NOT NULL CHECK (cycle >= 1),
    attempt_no  int NOT NULL CHECK (attempt_no BETWEEN 1 AND 1000),
    started_at  timestamptz NOT NULL DEFAULT now(),
    finished_at timestamptz,
    outcome     text CHECK (outcome IN ('success', 'retryable_failure', 'permanent_failure',
                                       'timeout', 'blocked', 'rate_limited', 'abandoned',
                                       'canceled')),
    http_status int CHECK (http_status IS NULL OR http_status BETWEEN 100 AND 599),
    error_code  text CHECK (error_code IS NULL OR char_length(error_code) <= 64),
    error_detail text CHECK (error_detail IS NULL OR char_length(error_detail) <= 512),
    duration_ms int CHECK (duration_ms IS NULL OR duration_ms >= 0),
    worker_id   text CHECK (worker_id IS NULL OR char_length(worker_id) <= 100),

    CONSTRAINT delivery_attempts_sequence_key UNIQUE (delivery_id, cycle, attempt_no),
    CONSTRAINT delivery_attempts_time_order
        CHECK (finished_at IS NULL OR finished_at >= started_at)
);

CREATE INDEX delivery_attempts_delivery_idx
    ON delivery_attempts (delivery_id, started_at DESC);
CREATE INDEX delivery_attempts_tenant_started_idx
    ON delivery_attempts (tenant_id, started_at DESC);
CREATE INDEX delivery_attempts_retention_idx
    ON delivery_attempts (started_at);

CREATE TABLE event_schemas (
    id         uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id  uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    event_type text NOT NULL
               CHECK (event_type ~ '^[a-z][a-z0-9_]*(\.[a-z0-9_]+){1,3}$'),
    version    int NOT NULL CHECK (version > 0),
    schema     jsonb NOT NULL CHECK (pg_column_size(schema) <= 65536),
    created_at timestamptz NOT NULL DEFAULT now(),

    CONSTRAINT event_schemas_tenant_type_version_key UNIQUE (tenant_id, event_type, version)
);

CREATE INDEX event_schemas_latest_idx
    ON event_schemas (tenant_id, event_type, version DESC);

CREATE TABLE idempotency_records (
    tenant_id           uuid NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    scope               text NOT NULL CHECK (char_length(scope) BETWEEN 1 AND 64),
    key                 text NOT NULL
                        CHECK (char_length(key) BETWEEN 8 AND 255
                               AND key ~ '^[A-Za-z0-9_.:-]+$'),
    request_fingerprint bytea NOT NULL CHECK (octet_length(request_fingerprint) = 32),
    status_code         int NOT NULL CHECK (status_code BETWEEN 100 AND 599),
    response_body       jsonb NOT NULL,
    created_at          timestamptz NOT NULL DEFAULT now(),
    expires_at          timestamptz NOT NULL,

    PRIMARY KEY (tenant_id, scope, key),
    CONSTRAINT idempotency_expiry_after_creation CHECK (expires_at > created_at)
);

CREATE INDEX idempotency_records_expires_idx
    ON idempotency_records (expires_at);
