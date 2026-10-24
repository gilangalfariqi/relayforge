package store

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/relayforge/relayforge/api/internal/secrets"
	"github.com/relayforge/relayforge/api/internal/ssrf"
)

// CountEndpoints returns the number of non-deleted endpoints.
func (s *Store) CountEndpoints(ctx context.Context, tenantID uuid.UUID) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM endpoints WHERE tenant_id = $1 AND deleted_at IS NULL`, tenantID).Scan(&n)
	return n, err
}

// CreateEndpoint inserts an endpoint and its first signing secret atomically.
func (s *Store) CreateEndpoint(ctx context.Context, tenantID uuid.UUID, ep Endpoint, actor Actor) (NewEndpoint, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return NewEndpoint{}, err
	}
	defer tx.Rollback(ctx)

	var count int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM endpoints WHERE tenant_id = $1 AND deleted_at IS NULL`,
		tenantID).Scan(&count); err != nil {
		return NewEndpoint{}, err
	}
	if count >= s.EndpointLimit {
		return NewEndpoint{}, ErrEndpointLimit
	}

	var out Endpoint
	err = tx.QueryRow(ctx, `
		INSERT INTO endpoints
			(tenant_id, name, url, status, event_types, max_attempts, initial_backoff_ms,
			 max_backoff_ms, retry_window_seconds, timeout_ms, rate_limit_per_sec)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		RETURNING id, name, url, status, event_types, max_attempts, initial_backoff_ms,
		          max_backoff_ms, retry_window_seconds, timeout_ms, rate_limit_per_sec,
		          consecutive_failures, disabled_at, disabled_reason, created_at, updated_at, deleted_at`,
		tenantID, ep.Name, ep.URL, ep.Status, ep.EventTypes, ep.MaxAttempts,
		ep.InitialBackoffMS, ep.MaxBackoffMS, ep.RetryWindowSeconds, ep.TimeoutMS, ep.RateLimitPerSec).
		Scan(&out.ID, &out.Name, &out.URL, &out.Status, &out.EventTypes, &out.MaxAttempts,
			&out.InitialBackoffMS, &out.MaxBackoffMS, &out.RetryWindowSeconds, &out.TimeoutMS,
			&out.RateLimitPerSec, &out.ConsecutiveFailure, &out.DisabledAt, &out.DisabledReason,
			&out.CreatedAt, &out.UpdatedAt, &out.DeletedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return NewEndpoint{}, ErrDuplicateEndpoint
		}
		return NewEndpoint{}, err
	}

	secret, err := s.createSecretTx(ctx, tx, tenantID, out.ID, 0)
	if err != nil {
		return NewEndpoint{}, err
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, "endpoint.create", "endpoint", &out.ID, map[string]any{
		"url":         ssrf.AuditURL(out.URL),
		"secret_id":   secret.SecretMeta.ID.String(),
		"event_types": out.EventTypes,
	}); err != nil {
		return NewEndpoint{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return NewEndpoint{}, err
	}
	return NewEndpoint{Endpoint: out, SigningSecret: secret.SigningSecret, SigningSecretID: secret.SecretMeta.ID}, nil
}

// ListEndpoints returns non-deleted endpoints for a tenant.
func (s *Store) ListEndpoints(ctx context.Context, tenantID uuid.UUID) ([]Endpoint, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, name, url, status, event_types, max_attempts, initial_backoff_ms,
		       max_backoff_ms, retry_window_seconds, timeout_ms, rate_limit_per_sec,
		       consecutive_failures, disabled_at, disabled_reason, created_at, updated_at, deleted_at
		FROM endpoints
		WHERE tenant_id = $1 AND deleted_at IS NULL
		ORDER BY created_at DESC, id DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		ep, err := scanEndpoint(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, ep)
	}
	return out, rows.Err()
}

// Endpoint returns one tenant-owned endpoint or ErrNotFound.
func (s *Store) Endpoint(ctx context.Context, tenantID, endpointID uuid.UUID) (Endpoint, error) {
	row := s.Pool.QueryRow(ctx, `
		SELECT id, name, url, status, event_types, max_attempts, initial_backoff_ms,
		       max_backoff_ms, retry_window_seconds, timeout_ms, rate_limit_per_sec,
		       consecutive_failures, disabled_at, disabled_reason, created_at, updated_at, deleted_at
		FROM endpoints WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, endpointID)
	ep, err := scanEndpoint(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	return ep, err
}

// UpdateEndpoint applies a full pre-validated endpoint state.
func (s *Store) UpdateEndpoint(ctx context.Context, tenantID, endpointID uuid.UUID, ep Endpoint, actor Actor) (Endpoint, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Endpoint{}, err
	}
	defer tx.Rollback(ctx)

	var oldURL, oldStatus string
	err = tx.QueryRow(ctx, `
		SELECT url, status FROM endpoints
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		FOR UPDATE`, tenantID, endpointID).Scan(&oldURL, &oldStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return Endpoint{}, ErrNotFound
	}
	if err != nil {
		return Endpoint{}, err
	}

	var out Endpoint
	err = tx.QueryRow(ctx, `
		UPDATE endpoints SET
			name = $3, url = $4, status = $5, event_types = $6, max_attempts = $7,
			initial_backoff_ms = $8, max_backoff_ms = $9, retry_window_seconds = $10,
			timeout_ms = $11, rate_limit_per_sec = $12, updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		RETURNING id, name, url, status, event_types, max_attempts, initial_backoff_ms,
		          max_backoff_ms, retry_window_seconds, timeout_ms, rate_limit_per_sec,
		          consecutive_failures, disabled_at, disabled_reason, created_at, updated_at, deleted_at`,
		tenantID, endpointID, ep.Name, ep.URL, ep.Status, ep.EventTypes, ep.MaxAttempts,
		ep.InitialBackoffMS, ep.MaxBackoffMS, ep.RetryWindowSeconds, ep.TimeoutMS, ep.RateLimitPerSec).
		Scan(&out.ID, &out.Name, &out.URL, &out.Status, &out.EventTypes, &out.MaxAttempts,
			&out.InitialBackoffMS, &out.MaxBackoffMS, &out.RetryWindowSeconds, &out.TimeoutMS,
			&out.RateLimitPerSec, &out.ConsecutiveFailure, &out.DisabledAt, &out.DisabledReason,
			&out.CreatedAt, &out.UpdatedAt, &out.DeletedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Endpoint{}, ErrDuplicateEndpoint
		}
		return Endpoint{}, err
	}
	action := "endpoint.update"
	switch {
	case oldStatus != "paused" && out.Status == "paused":
		action = "endpoint.pause"
	case oldStatus == "paused" && out.Status == "active":
		action = "endpoint.resume"
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, action, "endpoint", &endpointID, map[string]any{
		"old_url":    ssrf.AuditURL(oldURL),
		"new_url":    ssrf.AuditURL(out.URL),
		"old_status": oldStatus,
		"new_status": out.Status,
	}); err != nil {
		return Endpoint{}, err
	}
	return out, tx.Commit(ctx)
}

// DeleteEndpoint soft-deletes an endpoint and pauses fan-out.
func (s *Store) DeleteEndpoint(ctx context.Context, tenantID, endpointID uuid.UUID, actor Actor) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE endpoints SET status = 'paused', deleted_at = now(), updated_at = now()
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`, tenantID, endpointID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, "endpoint.delete", "endpoint", &endpointID, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ListSecrets returns secret metadata only.
func (s *Store) ListSecrets(ctx context.Context, tenantID, endpointID uuid.UUID) ([]SecretMeta, error) {
	var exists bool
	if err := s.Pool.QueryRow(ctx, `
		SELECT true FROM endpoints
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL`,
		tenantID, endpointID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `
		SELECT s.id, s.secret_hint, s.status, s.created_at, s.expires_at, s.retired_at
		FROM endpoint_secrets s
		JOIN endpoints e ON e.id = s.endpoint_id AND e.tenant_id = s.tenant_id
		WHERE s.tenant_id = $1 AND s.endpoint_id = $2 AND e.deleted_at IS NULL
		ORDER BY s.created_at DESC, s.id DESC`, tenantID, endpointID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SecretMeta
	for rows.Next() {
		var m SecretMeta
		if err := rows.Scan(&m.ID, &m.SecretHint, &m.Status, &m.CreatedAt, &m.ExpiresAt, &m.RetiredAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// CreateEndpointSecret creates a new active secret and starts the overlap
// window for existing secrets.
func (s *Store) CreateEndpointSecret(ctx context.Context, tenantID, endpointID uuid.UUID, overlap time.Duration, actor Actor) (NewSecret, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return NewSecret{}, err
	}
	defer tx.Rollback(ctx)

	var exists bool
	err = tx.QueryRow(ctx, `
		SELECT true FROM endpoints
		WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL
		FOR UPDATE`, tenantID, endpointID).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return NewSecret{}, ErrNotFound
	}
	if err != nil {
		return NewSecret{}, err
	}
	secret, err := s.createSecretTx(ctx, tx, tenantID, endpointID, overlap)
	if err != nil {
		return NewSecret{}, err
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, "endpoint.secret.create", "endpoint", &endpointID, map[string]any{
		"secret_id": secret.SecretMeta.ID.String(),
	}); err != nil {
		return NewSecret{}, err
	}
	return secret, tx.Commit(ctx)
}

func (s *Store) createSecretTx(ctx context.Context, tx pgx.Tx, tenantID, endpointID uuid.UUID, overlap time.Duration) (NewSecret, error) {
	var active int
	err := tx.QueryRow(ctx, `
		SELECT count(*) FROM endpoint_secrets
		WHERE tenant_id = $1 AND endpoint_id = $2 AND status = 'active'
		  AND (expires_at IS NULL OR expires_at > now())`, tenantID, endpointID).Scan(&active)
	if err != nil {
		return NewSecret{}, err
	}
	if active >= 2 {
		return NewSecret{}, ErrSecretOverlapLimit
	}
	if active > 0 && overlap > 0 {
		if _, err := tx.Exec(ctx, `
			UPDATE endpoint_secrets SET expires_at = now() + $3::interval
			WHERE tenant_id = $1 AND endpoint_id = $2 AND status = 'active'
			  AND (expires_at IS NULL OR expires_at > now())`,
			tenantID, endpointID, fmt.Sprintf("%d seconds", int(overlap.Seconds()))); err != nil {
			return NewSecret{}, err
		}
	}

	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return NewSecret{}, fmt.Errorf("generate signing secret: %w", err)
	}
	plaintext := "whsec_" + base64.StdEncoding.EncodeToString(raw)
	id := uuid.Must(uuid.NewV7())
	aad := secrets.AAD(tenantID.String(), endpointID.String(), id.String(), s.Box.Version())
	ciphertext, nonce, err := s.Box.Seal([]byte(plaintext), aad)
	if err != nil {
		return NewSecret{}, err
	}
	hint := plaintext
	if len(hint) > 4 {
		hint = hint[len(hint)-4:]
	}
	var m SecretMeta
	err = tx.QueryRow(ctx, `
		INSERT INTO endpoint_secrets
			(id, tenant_id, endpoint_id, ciphertext, nonce, key_version, secret_hint)
		VALUES ($1,$2,$3,$4,$5,$6,$7)
		RETURNING id, secret_hint, status, created_at, expires_at, retired_at`,
		id, tenantID, endpointID, ciphertext, nonce, s.Box.Version(), hint).
		Scan(&m.ID, &m.SecretHint, &m.Status, &m.CreatedAt, &m.ExpiresAt, &m.RetiredAt)
	if err != nil {
		return NewSecret{}, err
	}
	return NewSecret{SecretMeta: m, SigningSecret: plaintext}, nil
}

// RetireEndpointSecret retires a secret, refusing to retire the last active
// one.
func (s *Store) RetireEndpointSecret(ctx context.Context, tenantID, endpointID, secretID uuid.UUID, actor Actor) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var status string
	err = tx.QueryRow(ctx, `
		SELECT status FROM endpoint_secrets
		WHERE tenant_id = $1 AND endpoint_id = $2 AND id = $3
		FOR UPDATE`, tenantID, endpointID, secretID).Scan(&status)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if status == "retired" {
		return nil
	}
	var others int
	if err := tx.QueryRow(ctx, `
		SELECT count(*) FROM endpoint_secrets
		WHERE tenant_id = $1 AND endpoint_id = $2 AND id <> $3 AND status = 'active'
		  AND (expires_at IS NULL OR expires_at > now())`,
		tenantID, endpointID, secretID).Scan(&others); err != nil {
		return err
	}
	if others == 0 {
		return ErrLastActiveSecret
	}
	if _, err := tx.Exec(ctx, `
		UPDATE endpoint_secrets SET status = 'retired', retired_at = now()
		WHERE tenant_id = $1 AND endpoint_id = $2 AND id = $3`,
		tenantID, endpointID, secretID); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, "endpoint.secret.retire", "endpoint", &endpointID, map[string]any{
		"secret_id": secretID.String(),
	}); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanEndpoint(row rowScanner) (Endpoint, error) {
	var ep Endpoint
	err := row.Scan(&ep.ID, &ep.Name, &ep.URL, &ep.Status, &ep.EventTypes, &ep.MaxAttempts,
		&ep.InitialBackoffMS, &ep.MaxBackoffMS, &ep.RetryWindowSeconds, &ep.TimeoutMS,
		&ep.RateLimitPerSec, &ep.ConsecutiveFailure, &ep.DisabledAt, &ep.DisabledReason,
		&ep.CreatedAt, &ep.UpdatedAt, &ep.DeletedAt)
	return ep, err
}
