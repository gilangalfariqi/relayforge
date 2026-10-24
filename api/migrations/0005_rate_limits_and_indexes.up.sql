-- 0005: fixed-window rate-limit counters.
--
-- Deliberately simple: one atomic upsert per request or delivery. The
-- boundary-burst trade-off is documented in docs/adr/0011-postgres-rate-limiter.md.

CREATE TABLE rate_limit_counters (
    scope_type   text NOT NULL
                 CHECK (scope_type IN ('tenant_ingest', 'tenant_management', 'destination')),
    scope_id     uuid NOT NULL,
    window_start timestamptz NOT NULL,
    count        int NOT NULL CHECK (count >= 0),

    PRIMARY KEY (scope_type, scope_id, window_start)
);

CREATE INDEX rate_limit_counters_window_idx
    ON rate_limit_counters (window_start);
