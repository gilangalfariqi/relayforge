// Package db owns the PostgreSQL connection pool and schema readiness checks.
package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ErrSchemaNotReady indicates the database is reachable but its migration
// version is absent, dirty, or different from the version this binary expects.
var ErrSchemaNotReady = errors.New("database schema is not ready")

// Store wraps a PostgreSQL pool and the schema version expected by this binary.
type Store struct {
	Pool                  *pgxpool.Pool
	ExpectedSchemaVersion int
}

// Open creates and verifies a connection pool.
func Open(ctx context.Context, databaseURL string, maxConns int32) (*pgxpool.Pool, error) {
	cfg, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	cfg.MaxConns = maxConns
	cfg.MinConns = 0
	cfg.MaxConnLifetime = 30 * time.Minute
	cfg.MaxConnIdleTime = 5 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return pool, nil
}

// SchemaStatus reports the migration version and dirty flag.
func SchemaStatus(ctx context.Context, pool *pgxpool.Pool) (version int64, dirty bool, err error) {
	err = pool.QueryRow(ctx, `SELECT version, dirty FROM schema_migrations LIMIT 1`).Scan(&version, &dirty)
	if err != nil {
		return 0, false, fmt.Errorf("read schema_migrations: %w", err)
	}
	return version, dirty, nil
}

// Check implements the readiness contract used by the HTTP layer.
func (s *Store) Check(ctx context.Context) error {
	if err := s.Pool.Ping(ctx); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	version, dirty, err := SchemaStatus(ctx, s.Pool)
	if err != nil {
		return err
	}
	if dirty {
		return fmt.Errorf("%w: migration version %d is dirty", ErrSchemaNotReady, version)
	}
	if version != int64(s.ExpectedSchemaVersion) {
		return fmt.Errorf("%w: database version %d, binary expects %d",
			ErrSchemaNotReady, version, s.ExpectedSchemaVersion)
	}
	return nil
}
