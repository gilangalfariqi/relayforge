package store

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// CreateEventSchema stores a validated JSON Schema for a tenant event type.
func (s *Store) CreateEventSchema(ctx context.Context, tenantID uuid.UUID, eventType string, version int, schema json.RawMessage, actor Actor) (EventSchema, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return EventSchema{}, err
	}
	defer tx.Rollback(ctx)

	var out EventSchema
	var raw []byte
	err = tx.QueryRow(ctx, `
		INSERT INTO event_schemas (tenant_id, event_type, version, schema)
		VALUES ($1,$2,$3,$4)
		RETURNING id, event_type, version, schema, created_at`,
		tenantID, eventType, version, schema).
		Scan(&out.ID, &out.EventType, &out.Version, &raw, &out.CreatedAt)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return EventSchema{}, ErrSchemaExists
		}
		return EventSchema{}, err
	}
	_ = json.Unmarshal(raw, &out.Schema)
	if err := writeAudit(ctx, tx, &tenantID, actor, "schema.create", "event_schema", &out.ID, map[string]any{
		"event_type": eventType,
		"version":    version,
	}); err != nil {
		return EventSchema{}, err
	}
	return out, tx.Commit(ctx)
}

// ListEventSchemas lists schema metadata and documents for a tenant.
func (s *Store) ListEventSchemas(ctx context.Context, tenantID uuid.UUID) ([]EventSchema, error) {
	rows, err := s.Pool.Query(ctx, `
		SELECT id, event_type, version, schema, created_at
		FROM event_schemas WHERE tenant_id = $1
		ORDER BY event_type, version DESC, id DESC`, tenantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventSchema
	for rows.Next() {
		var x EventSchema
		var raw []byte
		if err := rows.Scan(&x.ID, &x.EventType, &x.Version, &raw, &x.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(raw, &x.Schema)
		out = append(out, x)
	}
	return out, rows.Err()
}

// EventSchemaJSON returns the raw schema document for validation.
func (s *Store) EventSchemaJSON(ctx context.Context, tenantID uuid.UUID, eventType string, version int) ([]byte, error) {
	var raw []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT schema FROM event_schemas
		WHERE tenant_id = $1 AND event_type = $2 AND version = $3`,
		tenantID, eventType, version).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return raw, err
}

// DeleteEventSchema removes a schema. Existing events are unaffected.
func (s *Store) DeleteEventSchema(ctx context.Context, tenantID, id uuid.UUID, actor Actor) error {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `DELETE FROM event_schemas WHERE tenant_id = $1 AND id = $2`, tenantID, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	if err := writeAudit(ctx, tx, &tenantID, actor, "schema.delete", "event_schema", &id, nil); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
