package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/relayforge/relayforge/api/internal/auth"
)

// Actor describes who caused an audited action.
type Actor struct {
	Type      string
	ID        string
	RequestID string
	IP        string
	UserAgent string
}

func writeAudit(ctx context.Context, tx pgx.Tx, tenantID *uuid.UUID, actor Actor, action, resourceType string, resourceID *uuid.UUID, metadata map[string]any) error {
	if actor.Type == "" {
		actor.Type = "system"
	}
	if actor.ID == "" {
		actor.ID = "relayforge"
	}
	if metadata == nil {
		metadata = map[string]any{}
	}
	var ip any
	if actor.IP != "" {
		ip = actor.IP
	}
	var reqID any
	if actor.RequestID != "" {
		reqID = actor.RequestID
	}
	var ua any
	if actor.UserAgent != "" {
		ua = truncate(actor.UserAgent, 512)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO audit_logs
			(tenant_id, actor_type, actor_id, action, resource_type, resource_id,
			 request_id, ip, user_agent, metadata)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8::inet, $9, $10)`,
		tenantID, actor.Type, truncate(actor.ID, 100), action, resourceType, resourceID,
		reqID, ip, ua, metadata)
	if err != nil {
		return fmt.Errorf("insert audit log: %w", err)
	}
	return nil
}

// CreateTenant atomically creates a tenant, its first owner credential, and
// an audit row.
func (s *Store) CreateTenant(ctx context.Context, slug, name string, key auth.Key, digest []byte, actor Actor) (Tenant, NewCredential, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Tenant{}, NewCredential{}, err
	}
	defer tx.Rollback(ctx)

	var t Tenant
	err = tx.QueryRow(ctx, `
		INSERT INTO tenants (slug, name)
		VALUES ($1, $2)
		RETURNING id, slug, name, status, created_at, updated_at, suspended_at`,
		slug, name,
	).Scan(&t.ID, &t.Slug, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.SuspendedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return Tenant{}, NewCredential{}, ErrSlugTaken
		}
		return Tenant{}, NewCredential{}, fmt.Errorf("insert tenant: %w", err)
	}

	var c Credential
	err = tx.QueryRow(ctx, `
		INSERT INTO api_credentials (tenant_id, name, key_id, secret_hmac, role)
		VALUES ($1, 'owner', $2, $3, 'owner')
		RETURNING id, key_id, name, role, created_at, last_used_at, expires_at, revoked_at, rotated_from`,
		t.ID, key.KeyID, digest,
	).Scan(&c.ID, &c.KeyID, &c.Name, &c.Role, &c.CreatedAt, &c.LastUsedAt, &c.ExpiresAt, &c.RevokedAt, &c.RotatedFrom)
	if err != nil {
		return Tenant{}, NewCredential{}, fmt.Errorf("insert owner credential: %w", err)
	}

	if err := writeAudit(ctx, tx, &t.ID, actor, "tenant.create", "tenant", &t.ID, map[string]any{
		"slug":   t.Slug,
		"key_id": c.KeyID,
	}); err != nil {
		return Tenant{}, NewCredential{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Tenant{}, NewCredential{}, err
	}
	return t, NewCredential{Credential: c, APIKey: key.Plaintext}, nil
}

// Tenant returns the caller's tenant.
func (s *Store) Tenant(ctx context.Context, tenantID uuid.UUID) (Tenant, error) {
	var t Tenant
	err := s.Pool.QueryRow(ctx, `
		SELECT id, slug, name, status, created_at, updated_at, suspended_at
		FROM tenants WHERE id = $1`, tenantID).
		Scan(&t.ID, &t.Slug, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.SuspendedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	return t, err
}

// UpdateTenantName changes the display name.
func (s *Store) UpdateTenantName(ctx context.Context, tenantID uuid.UUID, name string, actor Actor) (Tenant, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Tenant{}, err
	}
	defer tx.Rollback(ctx)

	var t Tenant
	err = tx.QueryRow(ctx, `
		UPDATE tenants SET name = $2, updated_at = now()
		WHERE id = $1
		RETURNING id, slug, name, status, created_at, updated_at, suspended_at`,
		tenantID, name).
		Scan(&t.ID, &t.Slug, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.SuspendedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, err
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, "tenant.update", "tenant", &tenantID, map[string]any{
		"name": name,
	}); err != nil {
		return Tenant{}, err
	}
	return t, tx.Commit(ctx)
}

// SetTenantStatus suspends or resumes a tenant. It is an admin operation.
func (s *Store) SetTenantStatus(ctx context.Context, tenantID uuid.UUID, status, reason string, actor Actor) (Tenant, error) {
	if status != "active" && status != "suspended" {
		return Tenant{}, fmt.Errorf("invalid tenant status %q", status)
	}
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return Tenant{}, err
	}
	defer tx.Rollback(ctx)

	var t Tenant
	err = tx.QueryRow(ctx, `
		UPDATE tenants
		SET status = $2,
		    suspended_at = CASE WHEN $2 = 'suspended' THEN now() ELSE NULL END,
		    updated_at = now()
		WHERE id = $1
		RETURNING id, slug, name, status, created_at, updated_at, suspended_at`,
		tenantID, status).
		Scan(&t.ID, &t.Slug, &t.Name, &t.Status, &t.CreatedAt, &t.UpdatedAt, &t.SuspendedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Tenant{}, ErrNotFound
	}
	if err != nil {
		return Tenant{}, err
	}
	action := "tenant.suspend"
	if status == "active" {
		action = "tenant.resume"
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, action, "tenant", &tenantID, map[string]any{
		"reason": reason,
	}); err != nil {
		return Tenant{}, err
	}
	return t, tx.Commit(ctx)
}

// CredentialByKeyID performs the unauthenticated key lookup. Callers must
// verify the HMAC before trusting the result.
func (s *Store) CredentialByKeyID(ctx context.Context, keyID string) (AuthCredential, error) {
	var c AuthCredential
	err := s.Pool.QueryRow(ctx, `
		SELECT c.id, c.tenant_id, c.name, c.key_id, c.secret_hmac, c.role,
		       c.created_at, c.last_used_at, c.expires_at, c.revoked_at, c.rotated_from,
		       t.status
		FROM api_credentials c
		JOIN tenants t ON t.id = c.tenant_id
		WHERE c.key_id = $1`, keyID).
		Scan(&c.ID, &c.TenantID, &c.Name, &c.KeyID, &c.SecretHMAC, &c.Role,
			&c.CreatedAt, &c.LastUsedAt, &c.ExpiresAt, &c.RevokedAt, &c.RotatedFrom,
			&c.TenantStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return AuthCredential{}, ErrNotFound
	}
	return c, err
}

// TouchCredential records use without blocking the request path on failure.
func (s *Store) TouchCredential(ctx context.Context, credentialID uuid.UUID) {
	_, _ = s.Pool.Exec(ctx, `
		UPDATE api_credentials SET last_used_at = now()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < now() - interval '60 seconds')`,
		credentialID)
}

// CreateCredential issues a new key for the tenant.
func (s *Store) CreateCredential(ctx context.Context, tenantID uuid.UUID, name, role string, expiresAt *time.Time, key auth.Key, digest []byte, actor Actor) (NewCredential, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return NewCredential{}, err
	}
	defer tx.Rollback(ctx)

	var c Credential
	err = tx.QueryRow(ctx, `
		INSERT INTO api_credentials (tenant_id, name, key_id, secret_hmac, role, expires_at)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, key_id, name, role, created_at, last_used_at, expires_at, revoked_at, rotated_from`,
		tenantID, name, key.KeyID, digest, role, expiresAt).
		Scan(&c.ID, &c.KeyID, &c.Name, &c.Role, &c.CreatedAt, &c.LastUsedAt, &c.ExpiresAt, &c.RevokedAt, &c.RotatedFrom)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return NewCredential{}, fmt.Errorf("key id collision")
		}
		return NewCredential{}, err
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, "credential.create", "credential", &c.ID, map[string]any{
		"role":   role,
		"key_id": c.KeyID,
	}); err != nil {
		return NewCredential{}, err
	}
	return NewCredential{Credential: c, APIKey: key.Plaintext}, tx.Commit(ctx)
}

// ListCredentials returns tenant credential metadata.
func (s *Store) ListCredentials(ctx context.Context, tenantID uuid.UUID) ([]Credential, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, key_id, name, role, created_at, last_used_at, expires_at, revoked_at, rotated_from
		FROM api_credentials
		WHERE tenant_id = $1
		ORDER BY created_at DESC, id DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		var c Credential
		if err := rows.Scan(&c.ID, &c.KeyID, &c.Name, &c.Role, &c.CreatedAt, &c.LastUsedAt,
			&c.ExpiresAt, &c.RevokedAt, &c.RotatedFrom); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RotateCredential issues a replacement key and schedules the old one for
// revocation after the overlap window.
func (s *Store) RotateCredential(ctx context.Context, tenantID, credentialID uuid.UUID, overlap time.Duration,
	key auth.Key, digest []byte, actor Actor) (NewCredential, time.Time, error) {

	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return NewCredential{}, time.Time{}, err
	}
	defer tx.Rollback(ctx)

	var role, name string
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT role, name, revoked_at FROM api_credentials
		WHERE id = $1 AND tenant_id = $2
		FOR UPDATE`, credentialID, tenantID).Scan(&role, &name, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return NewCredential{}, time.Time{}, ErrNotFound
	}
	if err != nil {
		return NewCredential{}, time.Time{}, err
	}
	if revokedAt != nil && !revokedAt.After(time.Now()) {
		return NewCredential{}, time.Time{}, ErrRotationRevoked
	}
	oldExpiry := time.Now().UTC().Add(overlap)
	if _, err := tx.Exec(ctx, `
		UPDATE api_credentials SET revoked_at = $3
		WHERE id = $1 AND tenant_id = $2`, credentialID, tenantID, oldExpiry); err != nil {
		return NewCredential{}, time.Time{}, err
	}

	var c Credential
	err = tx.QueryRow(ctx, `
		INSERT INTO api_credentials (tenant_id, name, key_id, secret_hmac, role, rotated_from)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, key_id, name, role, created_at, last_used_at, expires_at, revoked_at, rotated_from`,
		tenantID, name, key.KeyID, digest, role, credentialID).
		Scan(&c.ID, &c.KeyID, &c.Name, &c.Role, &c.CreatedAt, &c.LastUsedAt, &c.ExpiresAt, &c.RevokedAt, &c.RotatedFrom)
	if err != nil {
		return NewCredential{}, time.Time{}, err
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, "credential.rotate", "credential", &c.ID, map[string]any{
		"rotated_from":  credentialID.String(),
		"overlap_until": oldExpiry.Format(time.RFC3339),
	}); err != nil {
		return NewCredential{}, time.Time{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return NewCredential{}, time.Time{}, err
	}
	return NewCredential{Credential: c, APIKey: key.Plaintext}, oldExpiry, nil
}

// RevokeCredential immediately revokes a credential, refusing to remove the
// tenant's last usable owner.
func (s *Store) RevokeCredential(ctx context.Context, tenantID, credentialID uuid.UUID, actor Actor) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var role string
	var revokedAt *time.Time
	err = tx.QueryRow(ctx, `
		SELECT role, revoked_at FROM api_credentials
		WHERE id = $1 AND tenant_id = $2
		FOR UPDATE`, credentialID, tenantID).Scan(&role, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	if revokedAt != nil && !revokedAt.After(time.Now()) {
		return nil // idempotent
	}
	if role == "owner" {
		var owners int
		err = tx.QueryRow(ctx, `
			SELECT count(*) FROM api_credentials
			WHERE tenant_id = $1 AND role = 'owner' AND id <> $2
			  AND (revoked_at IS NULL OR revoked_at > now())
			  AND (expires_at IS NULL OR expires_at > now())`, tenantID, credentialID).Scan(&owners)
		if err != nil {
			return err
		}
		if owners == 0 {
			return ErrLastOwnerCredential
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE api_credentials SET revoked_at = now()
		WHERE id = $1 AND tenant_id = $2`, credentialID, tenantID); err != nil {
		return err
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, "credential.revoke", "credential", &credentialID, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
