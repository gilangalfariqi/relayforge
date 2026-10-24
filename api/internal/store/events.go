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
	"github.com/jackc/pgx/v5/pgconn"
)

// IngestInput is a fully validated event ready to be committed.
type IngestInput struct {
	TenantID      uuid.UUID
	Type          string
	SchemaVersion int
	OccurredAt    time.Time
	WireBody      []byte
	Payload       json.RawMessage
	PayloadBytes  int
	PayloadSHA256 []byte
	TraceParent   string
	Scope         string
	Key           string
	Fingerprint   []byte
}

// IngestResult includes whether a stored idempotent response was replayed.
type IngestResult struct {
	StatusCode int
	Body       json.RawMessage
	Replayed   bool
}

type EventCursor struct {
	CreatedAt time.Time
	ID        uuid.UUID
}

type EventFilter struct {
	Statuses []string
	Type     string
	From     *time.Time
	To       *time.Time
	Order    string
	Limit    int
	Cursor   *EventCursor
}

// IngestEvent atomically stores the event, fans out deliveries, and records
// the idempotent response.
func (s *Store) IngestEvent(ctx context.Context, in IngestInput) (IngestResult, error) {
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SET LOCAL lock_timeout = '3s'`); err != nil {
		return IngestResult{}, err
	}
	lockKey := in.TenantID.String() + ":" + in.Scope + ":" + in.Key
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "55P03" {
			return IngestResult{}, ErrIdempotencyInFlight
		}
		return IngestResult{}, fmt.Errorf("acquire idempotency lock: %w", err)
	}

	var fingerprint []byte
	var statusCode int
	var storedBody []byte
	err = tx.QueryRow(ctx, `
		SELECT request_fingerprint, status_code, response_body
		FROM idempotency_records
		WHERE tenant_id = $1 AND scope = $2 AND key = $3`,
		in.TenantID, in.Scope, in.Key).Scan(&fingerprint, &statusCode, &storedBody)
	if err == nil {
		var expires time.Time
		if err := tx.QueryRow(ctx, `
			SELECT expires_at FROM idempotency_records
			WHERE tenant_id = $1 AND scope = $2 AND key = $3`,
			in.TenantID, in.Scope, in.Key).Scan(&expires); err != nil {
			return IngestResult{}, err
		}
		if expires.After(time.Now()) {
			if !equalBytes(fingerprint, in.Fingerprint) {
				return IngestResult{}, ErrIdempotencyReused
			}
			if err := tx.Commit(ctx); err != nil {
				return IngestResult{}, err
			}
			return IngestResult{StatusCode: statusCode, Body: storedBody, Replayed: true}, nil
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM idempotency_records
			WHERE tenant_id = $1 AND scope = $2 AND key = $3`,
			in.TenantID, in.Scope, in.Key); err != nil {
			return IngestResult{}, err
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return IngestResult{}, fmt.Errorf("read idempotency record: %w", err)
	}

	eventID := uuid.Must(uuid.NewV7())
	var receivedAt time.Time
	err = tx.QueryRow(ctx, `
		INSERT INTO events
			(id, tenant_id, type, schema_version, occurred_at, wire_body, payload,
			 payload_bytes, payload_sha256, trace_parent)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		RETURNING received_at`,
		eventID, in.TenantID, in.Type, in.SchemaVersion, in.OccurredAt, in.WireBody,
		in.Payload, in.PayloadBytes, in.PayloadSHA256, nullable(in.TraceParent)).
		Scan(&receivedAt)
	if err != nil {
		return IngestResult{}, fmt.Errorf("insert event: %w", err)
	}

	rows, err := tx.Query(ctx, `
		SELECT id, max_attempts, initial_backoff_ms, max_backoff_ms,
		       retry_window_seconds, timeout_ms
		FROM endpoints
		WHERE tenant_id = $1 AND status = 'active' AND deleted_at IS NULL
		  AND (cardinality(event_types) = 0 OR $2 = ANY(event_types))
		ORDER BY id
		LIMIT 51`, in.TenantID, in.Type)
	if err != nil {
		return IngestResult{}, fmt.Errorf("load fan-out endpoints: %w", err)
	}
	type policy struct {
		id                 uuid.UUID
		maxAttempts        int
		initialBackoffMS   int
		maxBackoffMS       int
		retryWindowSeconds int
		timeoutMS          int
	}
	var policies []policy
	for rows.Next() {
		var p policy
		if err := rows.Scan(&p.id, &p.maxAttempts, &p.initialBackoffMS, &p.maxBackoffMS,
			&p.retryWindowSeconds, &p.timeoutMS); err != nil {
			rows.Close()
			return IngestResult{}, err
		}
		policies = append(policies, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return IngestResult{}, err
	}
	if len(policies) > 50 {
		return IngestResult{}, fmt.Errorf("fan-out exceeds 50 endpoints")
	}
	for _, p := range policies {
		if _, err := tx.Exec(ctx, `
			INSERT INTO deliveries
				(tenant_id, event_id, endpoint_id, max_attempts, initial_backoff_ms,
				 max_backoff_ms, retry_window_seconds, timeout_ms)
			VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
			in.TenantID, eventID, p.id, p.maxAttempts, p.initialBackoffMS,
			p.maxBackoffMS, p.retryWindowSeconds, p.timeoutMS); err != nil {
			return IngestResult{}, fmt.Errorf("insert delivery: %w", err)
		}
	}

	status := "accepted"
	if len(policies) == 0 {
		status = "no_subscribers"
	}
	if _, err := tx.Exec(ctx, `
		UPDATE events SET status = $2, delivery_count = $3, updated_at = now()
		WHERE id = $1`, eventID, status, len(policies)); err != nil {
		return IngestResult{}, err
	}

	if s.ingestHook != nil {
		if err := s.ingestHook(); err != nil {
			return IngestResult{}, err
		}
	}

	body, err := json.Marshal(struct {
		ID            string    `json:"id"`
		Type          string    `json:"type"`
		SchemaVersion int       `json:"schema_version"`
		Status        string    `json:"status"`
		OccurredAt    time.Time `json:"occurred_at"`
		ReceivedAt    time.Time `json:"received_at"`
		DeliveryCount int       `json:"delivery_count"`
		CreatedAt     time.Time `json:"created_at"`
	}{
		ID: eventID.String(), Type: in.Type, SchemaVersion: in.SchemaVersion, Status: status,
		OccurredAt: in.OccurredAt.UTC(), ReceivedAt: receivedAt.UTC(),
		DeliveryCount: len(policies), CreatedAt: receivedAt.UTC(),
	})
	if err != nil {
		return IngestResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO idempotency_records
			(tenant_id, scope, key, request_fingerprint, status_code, response_body, expires_at)
		VALUES ($1,$2,$3,$4,201,$5,now() + $6::interval)`,
		in.TenantID, in.Scope, in.Key, in.Fingerprint, body,
		fmt.Sprintf("%d seconds", int(s.IdempotencyTTL.Seconds()))); err != nil {
		return IngestResult{}, err
	}
	var normalized []byte
	if err := tx.QueryRow(ctx, `
		SELECT response_body FROM idempotency_records
		WHERE tenant_id = $1 AND scope = $2 AND key = $3`,
		in.TenantID, in.Scope, in.Key).Scan(&normalized); err != nil {
		return IngestResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return IngestResult{}, err
	}
	return IngestResult{StatusCode: 201, Body: normalized}, nil
}

// ListEvents returns a keyset page plus whether another row exists.
func (s *Store) ListEvents(ctx context.Context, tenantID uuid.UUID, f EventFilter) ([]Event, bool, error) {
	limit := f.Limit
	if limit < 1 || limit > 100 {
		limit = 25
	}
	args := []any{tenantID}
	where := []string{"e.tenant_id = $1"}
	if len(f.Statuses) > 0 {
		args = append(args, f.Statuses)
		where = append(where, fmt.Sprintf("e.status = ANY($%d)", len(args)))
	}
	if f.Type != "" {
		args = append(args, f.Type)
		where = append(where, fmt.Sprintf("e.type = $%d", len(args)))
	}
	if f.From != nil {
		args = append(args, *f.From)
		where = append(where, fmt.Sprintf("e.created_at >= $%d", len(args)))
	}
	if f.To != nil {
		args = append(args, *f.To)
		where = append(where, fmt.Sprintf("e.created_at < $%d", len(args)))
	}
	op := "<"
	if f.Order == "asc" {
		op = ">"
	}
	if f.Cursor != nil {
		args = append(args, f.Cursor.CreatedAt, f.Cursor.ID)
		where = append(where, fmt.Sprintf("(e.created_at, e.id) %s ($%d, $%d)", op, len(args)-1, len(args)))
	}
	args = append(args, limit+1)
	query := fmt.Sprintf(`
		SELECT e.id, e.type, e.schema_version, e.status, e.occurred_at, e.received_at,
		       e.delivery_count, e.created_at,
		       (SELECT count(*) FROM deliveries d WHERE d.event_id = e.id AND d.status = 'delivered'),
		       (SELECT count(*) FROM deliveries d WHERE d.event_id = e.id AND d.status IN ('failed','dead_lettered'))
		FROM events e
		WHERE %s
		ORDER BY e.created_at %s, e.id %s
		LIMIT $%d`, strings.Join(where, " AND "), orderWord(f.Order), orderWord(f.Order), len(args))

	rows, err := s.Pool.Query(ctx, query, args...)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.Type, &e.SchemaVersion, &e.Status, &e.OccurredAt,
			&e.ReceivedAt, &e.DeliveryCount, &e.CreatedAt, &e.DeliveredCount, &e.FailedCount); err != nil {
			return nil, false, err
		}
		out = append(out, e)
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

// EventDetail loads one event with its parsed payload and delivery summary.
func (s *Store) EventDetail(ctx context.Context, tenantID, eventID uuid.UUID) (EventDetail, error) {
	var d EventDetail
	var payload []byte
	var hash []byte
	err := s.Pool.QueryRow(ctx, `
		SELECT e.id, e.type, e.schema_version, e.status, e.occurred_at, e.received_at,
		       e.delivery_count, e.created_at,
		       (SELECT count(*) FROM deliveries d WHERE d.event_id = e.id AND d.status = 'delivered'),
		       (SELECT count(*) FROM deliveries d WHERE d.event_id = e.id AND d.status IN ('failed','dead_lettered')),
		       e.payload, e.payload_sha256
		FROM events e
		WHERE e.tenant_id = $1 AND e.id = $2`, tenantID, eventID).
		Scan(&d.ID, &d.Type, &d.SchemaVersion, &d.Status, &d.OccurredAt, &d.ReceivedAt,
			&d.DeliveryCount, &d.CreatedAt, &d.DeliveredCount, &d.FailedCount, &payload, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return EventDetail{}, ErrNotFound
	}
	if err != nil {
		return EventDetail{}, err
	}
	if err := json.Unmarshal(payload, &d.Payload); err != nil {
		return EventDetail{}, err
	}
	d.PayloadSHA256 = fmt.Sprintf("%x", hash)

	rows, err := s.Pool.Query(ctx, `
		SELECT d.id, d.endpoint_id, e.name, e.url, d.status, d.attempt_count,
		       d.next_attempt_at, d.completed_at
		FROM deliveries d
		JOIN endpoints e ON e.id = d.endpoint_id AND e.tenant_id = d.tenant_id
		WHERE d.tenant_id = $1 AND d.event_id = $2
		ORDER BY d.created_at, d.id`, tenantID, eventID)
	if err != nil {
		return EventDetail{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var x DeliverySummary
		if err := rows.Scan(&x.ID, &x.EndpointID, &x.EndpointName, &x.EndpointURL,
			&x.Status, &x.AttemptCount, &x.NextAttemptAt, &x.CompletedAt); err != nil {
			return EventDetail{}, err
		}
		d.Deliveries = append(d.Deliveries, x)
	}
	return d, rows.Err()
}

func orderWord(order string) string {
	if order == "asc" {
		return "ASC"
	}
	return "DESC"
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}
