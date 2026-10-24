-- 0001: tenants. No extensions are required: PostgreSQL 18 provides
-- uuidv7() natively.

CREATE TABLE tenants (
    id           uuid PRIMARY KEY DEFAULT uuidv7(),
    slug         text NOT NULL UNIQUE
                 CHECK (slug ~ '^[a-z0-9][a-z0-9-]{2,62}$'),
    name         text NOT NULL
                 CHECK (char_length(name) BETWEEN 1 AND 200),
    status       text NOT NULL DEFAULT 'active'
                 CHECK (status IN ('active', 'suspended')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    suspended_at timestamptz
);
