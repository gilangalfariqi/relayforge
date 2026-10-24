-- 0002: API credentials and append-only audit log.

CREATE TABLE api_credentials (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    tenant_id    uuid NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    name         text NOT NULL CHECK (char_length(name) BETWEEN 1 AND 100),
    key_id       text NOT NULL UNIQUE CHECK (key_id ~ '^[a-z0-9]{12}$'),
    secret_hmac  bytea NOT NULL CHECK (octet_length(secret_hmac) = 32),
    role         text NOT NULL CHECK (role IN ('owner', 'admin', 'ingest', 'viewer')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    last_used_at timestamptz,
    expires_at   timestamptz,
    revoked_at   timestamptz,
    rotated_from uuid REFERENCES api_credentials(id) ON DELETE SET NULL
);

CREATE INDEX api_credentials_tenant_id_created_at_idx
    ON api_credentials (tenant_id, created_at DESC);

CREATE TABLE audit_logs (
    id            bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    tenant_id     uuid REFERENCES tenants(id) ON DELETE RESTRICT,
    actor_type    text NOT NULL CHECK (actor_type IN ('api_key', 'admin_token', 'system')),
    actor_id      text NOT NULL CHECK (char_length(actor_id) BETWEEN 1 AND 100),
    action        text NOT NULL CHECK (char_length(action) BETWEEN 1 AND 64),
    resource_type text NOT NULL CHECK (char_length(resource_type) BETWEEN 1 AND 32),
    resource_id   uuid,
    request_id    text CHECK (request_id IS NULL OR char_length(request_id) <= 128),
    ip            inet,
    user_agent    text CHECK (user_agent IS NULL OR char_length(user_agent) <= 512),
    metadata      jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX audit_logs_tenant_created_idx
    ON audit_logs (tenant_id, created_at DESC, id DESC);
CREATE INDEX audit_logs_resource_idx
    ON audit_logs (resource_type, resource_id, created_at DESC);
CREATE INDEX audit_logs_created_idx
    ON audit_logs (created_at);
