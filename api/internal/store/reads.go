package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type DeliveryFilter struct {
	Statuses   []string
	EndpointID *uuid.UUID
	EventID    *uuid.UUID
	From       *time.Time
	To         *time.Time
	Limit      int
	Cursor     *EventCursor
}

type AttemptCursor struct {
	StartedAt time.Time
	ID        int64
}

type AuditCursor struct {
	CreatedAt time.Time
	ID        int64
}

type AuditFilter struct {
	Action       string
	ResourceType string
	ResourceID   *uuid.UUID
	From         *time.Time
	To           *time.Time
	Limit        int
	Cursor       *AuditCursor
}

// ListDeliveries returns tenant-scoped deliveries.
func (s *Store) ListDeliveries(ctx context.Context, tenantID uuid.UUID, f DeliveryFilter) ([]Delivery, bool, error) {
	limit := f.Limit
	if limit < 1 || limit > 100 {
		limit = 25
	}
	args := []any{tenantID}
	where := []string{"tenant_id = $1"}
	if len(f.Statuses) > 0 {
		args = append(args, f.Statuses)
		where = append(where, fmt.Sprintf("status = ANY($%d)", len(args)))
	}
	if f.EndpointID != nil {
		args = append(args, *f.EndpointID)
		where = append(where, fmt.Sprintf("endpoint_id = $%d", len(args)))
	}
	if f.EventID != nil {
		args = append(args, *f.EventID)
		where = append(where, fmt.Sprintf("event_id = $%d", len(args)))
	}
	if f.From != nil {
		args = append(args, *f.From)
		where = append(where, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if f.To != nil {
		args = append(args, *f.To)
		where = append(where, fmt.Sprintf("created_at < $%d", len(args)))
	}
	if f.Cursor != nil {
		args = append(args, f.Cursor.CreatedAt, f.Cursor.ID)
		where = append(where, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	query := fmt.Sprintf(`
		SELECT id, event_id, endpoint_id, cycle, status, attempt_count, max_attempts,
		       next_attempt_at, last_error_code, last_error_detail, last_http_status,
		       created_at, completed_at, dead_lettered_at
		FROM deliveries
		WHERE %s
		ORDER BY created_at DESC, id DESC
		LIMIT $%d`, strings.Join(where, " AND "), len(args))
	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Delivery
	for rows.Next() {
		var d Delivery
		if err := rows.Scan(&d.ID, &d.EventID, &d.EndpointID, &d.Cycle, &d.Status,
			&d.AttemptCount, &d.MaxAttempts, &d.NextAttemptAt, &d.LastErrorCode,
			&d.LastErrorDetail, &d.LastHTTPStatus, &d.CreatedAt, &d.CompletedAt,
			&d.DeadLetteredAt); err != nil {
			return nil, false, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// Delivery returns one tenant-owned delivery.
func (s *Store) Delivery(ctx context.Context, tenantID, id uuid.UUID) (Delivery, error) {
	var d Delivery
	err := s.Pool.QueryRow(ctx, `
		SELECT id, event_id, endpoint_id, cycle, status, attempt_count, max_attempts,
		       next_attempt_at, last_error_code, last_error_detail, last_http_status,
		       created_at, completed_at, dead_lettered_at
		FROM deliveries WHERE tenant_id = $1 AND id = $2`, tenantID, id).
		Scan(&d.ID, &d.EventID, &d.EndpointID, &d.Cycle, &d.Status, &d.AttemptCount,
			&d.MaxAttempts, &d.NextAttemptAt, &d.LastErrorCode, &d.LastErrorDetail,
			&d.LastHTTPStatus, &d.CreatedAt, &d.CompletedAt, &d.DeadLetteredAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return Delivery{}, ErrNotFound
	}
	return d, err
}

// ListAttempts returns append-only attempt history for a delivery.
func (s *Store) ListAttempts(ctx context.Context, tenantID, deliveryID uuid.UUID, limit int, cursor *AttemptCursor) ([]Attempt, bool, error) {
	if limit < 1 || limit > 100 {
		limit = 50
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT true FROM deliveries WHERE tenant_id = $1 AND id = $2`,
		tenantID, deliveryID).Scan(&exists); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, false, ErrNotFound
		}
		return nil, false, err
	}
	args := []any{tenantID, deliveryID}
	where := "tenant_id = $1 AND delivery_id = $2"
	if cursor != nil {
		args = append(args, cursor.StartedAt, cursor.ID)
		where += fmt.Sprintf(" AND (started_at, id) < ($%d, $%d)", len(args)-1, len(args))
	}
	args = append(args, limit+1)
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(`
		SELECT id, attempt_no, cycle, started_at, finished_at, outcome, http_status,
		       error_code, error_detail, duration_ms
		FROM delivery_attempts
		WHERE %s
		ORDER BY started_at DESC, id DESC
		LIMIT $%d`, where, len(args)), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Attempt
	var lastID int64
	for rows.Next() {
		var a Attempt
		if err := rows.Scan(&lastID, &a.AttemptNo, &a.Cycle, &a.StartedAt, &a.FinishedAt,
			&a.Outcome, &a.HTTPStatus, &a.ErrorCode, &a.ErrorDetail, &a.DurationMS); err != nil {
			return nil, false, err
		}
		a.CursorID = lastID
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// AttemptCursorFor builds a cursor from the last attempt row.
func AttemptCursorFor(startedAt time.Time, id int64) AttemptCursor {
	return AttemptCursor{StartedAt: startedAt, ID: id}
}

// ListAudit returns filtered audit entries for the tenant.
func (s *Store) ListAudit(ctx context.Context, tenantID uuid.UUID, f AuditFilter) ([]AuditEntry, bool, error) {
	limit := f.Limit
	if limit < 1 || limit > 100 {
		limit = 25
	}
	args := []any{tenantID}
	where := []string{"tenant_id = $1"}
	if f.Action != "" {
		args = append(args, f.Action)
		where = append(where, fmt.Sprintf("action = $%d", len(args)))
	}
	if f.ResourceType != "" {
		args = append(args, f.ResourceType)
		where = append(where, fmt.Sprintf("resource_type = $%d", len(args)))
	}
	if f.ResourceID != nil {
		args = append(args, *f.ResourceID)
		where = append(where, fmt.Sprintf("resource_id = $%d", len(args)))
	}
	if f.From != nil {
		args = append(args, *f.From)
		where = append(where, fmt.Sprintf("created_at >= $%d", len(args)))
	}
	if f.To != nil {
		args = append(args, *f.To)
		where = append(where, fmt.Sprintf("created_at < $%d", len(args)))
	}
	if f.Cursor != nil {
		args = append(args, f.Cursor.CreatedAt, f.Cursor.ID)
		where = append(where, fmt.Sprintf("(created_at, id) < ($%d, $%d)", len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	rows, err := s.Pool.Query(ctx, fmt.Sprintf(`
		SELECT id, actor_type, actor_id, action, resource_type, resource_id,
		       request_id, host(ip), metadata, created_at
		FROM audit_logs
		WHERE %s
		ORDER BY created_at DESC, id DESC
		LIMIT $%d`, strings.Join(where, " AND "), len(args)), args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var a AuditEntry
		var metadata []byte
		if err := rows.Scan(&a.ID, &a.ActorType, &a.ActorID, &a.Action, &a.ResourceType,
			&a.ResourceID, &a.RequestID, &a.IP, &metadata, &a.CreatedAt); err != nil {
			return nil, false, err
		}
		_ = json.Unmarshal(metadata, &a.Metadata)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}
